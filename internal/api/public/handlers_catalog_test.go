package public

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/redis/go-redis/v9"
)

// catalogTestRedis returns a connected Redis client gated by REDIS_TEST_URL,
// matching the rate-limit test pattern (newTestRedis in
// middleware_ratelimit_test.go). Tests skip cleanly in environments without
// Redis (CI service container is configured by Plan 18-03).
func catalogTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL not set — skipping Redis-gated catalog test")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_TEST_URL: %v", err)
	}
	c := redis.NewClient(opt)
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	return c
}

// catalogTestCache writes the supplied offerings into a per-test Redis key
// and returns a *availability.Cache wired to that key. Each test gets a
// unique key so parallel tests do not stomp each other; the key is deleted
// when the test ends.
func catalogTestCache(t *testing.T, offerings []availability.AvailableOffering) *availability.Cache {
	t.Helper()
	rdb := catalogTestRedis(t)
	key := fmt.Sprintf("test:gpu:offerings:%d", time.Now().UnixNano())
	cache := availability.NewCacheWithKey(rdb, time.Minute, key)
	if offerings != nil {
		if err := cache.SetOfferings(context.Background(), offerings); err != nil {
			t.Fatalf("seed offerings: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), key).Err()
		_ = rdb.Close()
	})
	return cache
}

// catalogTestCacheBroken returns a Cache pointed at a Redis client that
// dials a closed port — every Read returns a network error. Used to
// exercise the 503 path without depending on a live Redis service.
func catalogTestCacheBroken(t *testing.T) *availability.Cache {
	t.Helper()
	// 127.0.0.1:1 is RFC-reserved tcpmux; nothing listens there in CI sandboxes,
	// so dial fails fast with a connection-refused error.
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1, // disable retries so the test fails fast
	})
	t.Cleanup(func() { _ = rdb.Close() })
	return availability.NewCacheWithKey(rdb, time.Minute, "test:catalog:broken")
}

// sampleOfferings returns a fixture set with two distinct gpu_models across
// three (model, region, tier) tuples — enough to exercise dedupe + sort +
// pagination on both endpoints.
func sampleOfferings() []availability.AvailableOffering {
	return []availability.AvailableOffering{
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.5, Region: "us-west", Tier: "on_demand", AvailableCount: 8},
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.7, Region: "us-east", Tier: "on_demand", AvailableCount: 4},
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 1.9, Region: "us-west", Tier: "spot", AvailableCount: 2},
		{GPUModel: "a100_pcie", GPUCount: 1, VRAMGB: 40, PricePerHour: 1.2, Region: "us-east", Tier: "on_demand", AvailableCount: 16},
		{GPUModel: "a100_pcie", GPUCount: 1, VRAMGB: 40, PricePerHour: 0.9, Region: "us-east", Tier: "spot", AvailableCount: 5},
	}
}

// TestKnownGPUModels — issue #99: the set used to validate gpu_type at create
// time is exactly the gpu_models currently in the cache (the same source as
// GET /v1/gpu-types). Listed models are present (deduped); a never-offered
// value is absent.
func TestKnownGPUModels(t *testing.T) {
	cache := catalogTestCache(t, sampleOfferings())
	known, err := cache.KnownGPUModels(context.Background())
	if err != nil {
		t.Fatalf("KnownGPUModels: %v", err)
	}
	if len(known) != 2 {
		t.Errorf("known set size = %d want 2 (deduped models)", len(known))
	}
	for _, want := range []string{"h100_sxm", "a100_pcie"} {
		if _, ok := known[want]; !ok {
			t.Errorf("%q should be a known gpu_type", want)
		}
	}
	if _, ok := known["not_a_real_gpu"]; ok {
		t.Error("not_a_real_gpu must not be a known gpu_type")
	}
}

func TestHandleListGPUTypes_NoAuth(t *testing.T) {
	cache := catalogTestCache(t, sampleOfferings())
	deps := PublicDeps{AvailCache: cache}

	mux := http.NewServeMux()
	RegisterCatalogRoutes(mux, deps)

	req := httptest.NewRequest(http.MethodGet, "/v1/gpu-types", nil)
	// NOTE: deliberately no Authorization header — endpoint must serve 200.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var page Page[GPUType]
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Data) != 2 {
		t.Fatalf("dedupe: got %d rows, want 2 (a100_pcie + h100_sxm)", len(page.Data))
	}
	// Sort assertion: a100_pcie < h100_sxm lexicographically.
	if page.Data[0].GPUType != "a100_pcie" || page.Data[1].GPUType != "h100_sxm" {
		t.Errorf("sort: got %+v, want [a100_pcie, h100_sxm]", page.Data)
	}
	// X-Request-Id always set (RequestIDMiddleware wraps).
	if w.Header().Get("X-Request-Id") == "" {
		t.Errorf("X-Request-Id header missing — RequestIDMiddleware not wired")
	}
}

func TestHandleListGPUTypes_DedupesByType(t *testing.T) {
	// 5 offerings, 2 distinct gpu_models → expect 2 rows.
	cache := catalogTestCache(t, sampleOfferings())
	deps := PublicDeps{AvailCache: cache}

	mux := http.NewServeMux()
	RegisterCatalogRoutes(mux, deps)

	req := httptest.NewRequest(http.MethodGet, "/v1/gpu-types", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var page Page[GPUType]
	_ = json.NewDecoder(w.Body).Decode(&page)
	if len(page.Data) != 2 {
		t.Fatalf("dedupe: got %d, want 2", len(page.Data))
	}
	for _, row := range page.Data {
		if row.GPUType == "" {
			t.Errorf("empty gpu_type: %+v", row)
		}
		if row.VRAMGB == 0 {
			t.Errorf("vram_gb zero (cache fixture has 40/80): %+v", row)
		}
	}
}

// TestHandleListGPUTypes_VRAMIsMinAcrossSightings — a model whose sightings
// disagree on VRAM (v100: 16GB silicon from one feed vs 32GB from another)
// must advertise the minimum, regardless of snapshot order. First-sighting
// flapped 16↔32 with iteration order and over-promised against 16GB launches
// (2026-08-12 support ticket).
func TestHandleListGPUTypes_VRAMIsMinAcrossSightings(t *testing.T) {
	for name, off := range map[string][]availability.AvailableOffering{
		"high first": {
			{GPUModel: "v100", VRAMGB: 32, PricePerHour: 2.4, Region: "us-east", Tier: "on_demand", AvailableCount: 4},
			{GPUModel: "v100", VRAMGB: 16, PricePerHour: 0.14, Region: "us-east", Tier: "on_demand", AvailableCount: 1},
		},
		"low first": {
			{GPUModel: "v100", VRAMGB: 16, PricePerHour: 0.14, Region: "us-east", Tier: "on_demand", AvailableCount: 1},
			{GPUModel: "v100", VRAMGB: 32, PricePerHour: 2.4, Region: "us-east", Tier: "on_demand", AvailableCount: 4},
		},
		// A zero sighting (unknown model through a supplier feed dodges the
		// availability safety net) must neither pin the row at 0 nor win the min.
		"zero first": {
			{GPUModel: "v100", VRAMGB: 0, PricePerHour: 2.4, Region: "us-east", Tier: "on_demand", AvailableCount: 4},
			{GPUModel: "v100", VRAMGB: 16, PricePerHour: 0.14, Region: "us-east", Tier: "on_demand", AvailableCount: 1},
		},
		"zero last": {
			{GPUModel: "v100", VRAMGB: 16, PricePerHour: 0.14, Region: "us-east", Tier: "on_demand", AvailableCount: 1},
			{GPUModel: "v100", VRAMGB: 0, PricePerHour: 2.4, Region: "us-east", Tier: "on_demand", AvailableCount: 4},
		},
	} {
		t.Run(name, func(t *testing.T) {
			cache := catalogTestCache(t, off)
			deps := PublicDeps{AvailCache: cache}

			mux := http.NewServeMux()
			RegisterCatalogRoutes(mux, deps)

			req := httptest.NewRequest(http.MethodGet, "/v1/gpu-types", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			var page Page[GPUType]
			_ = json.NewDecoder(w.Body).Decode(&page)
			if len(page.Data) != 1 {
				t.Fatalf("dedupe: got %d rows, want 1", len(page.Data))
			}
			if got := page.Data[0].VRAMGB; got != 16 {
				t.Errorf("vram_gb = %d, want 16 (minimum across sightings)", got)
			}
		})
	}
}

func TestHandleListGPUTypes_CursorPagination(t *testing.T) {
	// 5 distinct gpu_models, limit=2 → walk all pages.
	off := []availability.AvailableOffering{
		{GPUModel: "a100_pcie", VRAMGB: 40, PricePerHour: 1.0, Region: "us-east", Tier: "on_demand", AvailableCount: 1},
		{GPUModel: "b200", VRAMGB: 192, PricePerHour: 5.0, Region: "us-east", Tier: "on_demand", AvailableCount: 1},
		{GPUModel: "h100_sxm", VRAMGB: 80, PricePerHour: 2.5, Region: "us-west", Tier: "on_demand", AvailableCount: 1},
		{GPUModel: "l4", VRAMGB: 24, PricePerHour: 0.5, Region: "us-east", Tier: "on_demand", AvailableCount: 1},
		{GPUModel: "rtx4090", VRAMGB: 24, PricePerHour: 0.4, Region: "us-east", Tier: "on_demand", AvailableCount: 1},
	}
	cache := catalogTestCache(t, off)
	deps := PublicDeps{AvailCache: cache}

	mux := http.NewServeMux()
	RegisterCatalogRoutes(mux, deps)

	collected := []string{}
	cursor := ""
	for pageNum := 0; pageNum < 5; pageNum++ {
		url := "/v1/gpu-types?limit=2"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("page %d status: got %d, want 200; body=%s", pageNum, w.Code, w.Body.String())
		}
		var page Page[GPUType]
		if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
			t.Fatalf("page %d decode: %v", pageNum, err)
		}
		for _, row := range page.Data {
			collected = append(collected, row.GPUType)
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	want := []string{"a100_pcie", "b200", "h100_sxm", "l4", "rtx4090"}
	if len(collected) != len(want) {
		t.Fatalf("collected=%v, want=%v", collected, want)
	}
	for i := range want {
		if collected[i] != want[i] {
			t.Errorf("position %d: got %q, want %q", i, collected[i], want[i])
		}
	}
}

func TestHandleListGPUTypes_CacheError(t *testing.T) {
	deps := PublicDeps{AvailCache: catalogTestCacheBroken(t)}
	mux := http.NewServeMux()
	RegisterCatalogRoutes(mux, deps)

	req := httptest.NewRequest(http.MethodGet, "/v1/gpu-types", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var p Problem
	if err := json.NewDecoder(w.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Code != "internal_error" {
		t.Errorf("code: got %q, want internal_error", p.Code)
	}
}

func TestHandleListPricing_NoAuth(t *testing.T) {
	cache := catalogTestCache(t, sampleOfferings())
	deps := PublicDeps{AvailCache: cache}

	mux := http.NewServeMux()
	RegisterCatalogRoutes(mux, deps)

	req := httptest.NewRequest(http.MethodGet, "/v1/pricing", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var page Page[Pricing]
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	// Full breakdown — same number of rows as input fixture (5).
	if len(page.Data) != 5 {
		t.Fatalf("breakdown: got %d, want 5", len(page.Data))
	}
	// Sort: gpu_type asc → a100_pcie rows precede h100_sxm rows.
	if page.Data[0].GPUType != "a100_pcie" {
		t.Errorf("first row gpu_type: got %q, want a100_pcie", page.Data[0].GPUType)
	}
}

func TestHandleListPricing_CacheError(t *testing.T) {
	deps := PublicDeps{AvailCache: catalogTestCacheBroken(t)}
	mux := http.NewServeMux()
	RegisterCatalogRoutes(mux, deps)

	req := httptest.NewRequest(http.MethodGet, "/v1/pricing", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var p Problem
	if err := json.NewDecoder(w.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Code != "internal_error" {
		t.Errorf("code: got %q, want internal_error", p.Code)
	}
}

// TestHandleListPricing_InstantBootField verifies /v1/pricing surfaces the
// per-offering boot class so SDK/CLI consumers can skip slow-start capacity.
// instant_boot is part of the aggregation key, so it maps 1:1 from the cached
// offering here (the two rows differ by gpu_type anyway).
func TestHandleListPricing_InstantBootField(t *testing.T) {
	cache := catalogTestCache(t, []availability.AvailableOffering{
		{GPUModel: "b200", VRAMGB: 180, PricePerHour: 6.24, Region: "us-west", Tier: "on_demand", AvailableCount: 2, InstantBoot: false},
		{GPUModel: "h100_sxm", VRAMGB: 80, PricePerHour: 2.85, Region: "us-east", Tier: "on_demand", AvailableCount: 5, InstantBoot: true},
	})
	deps := PublicDeps{AvailCache: cache}

	mux := http.NewServeMux()
	RegisterCatalogRoutes(mux, deps)

	req := httptest.NewRequest(http.MethodGet, "/v1/pricing", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var page Page[Pricing]
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Data) != 2 {
		t.Fatalf("rows: got %d, want 2", len(page.Data))
	}
	// Sorted gpu_type asc: b200 (slow) precedes h100_sxm (instant).
	if page.Data[0].GPUType != "b200" || page.Data[0].InstantBoot {
		t.Errorf("row 0: want b200 instant_boot=false, got %+v", page.Data[0])
	}
	if page.Data[1].GPUType != "h100_sxm" || !page.Data[1].InstantBoot {
		t.Errorf("row 1: want h100_sxm instant_boot=true, got %+v", page.Data[1])
	}
}

// listPricing is a small helper for the merge/filter/pagination tests below:
// GETs the given /v1/pricing URL against a fresh mux and decodes the page.
func listPricing(t *testing.T, deps PublicDeps, target string) Page[Pricing] {
	t.Helper()
	mux := http.NewServeMux()
	RegisterCatalogRoutes(mux, deps)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200; body=%s", target, w.Code, w.Body.String())
	}
	var page Page[Pricing]
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	return page
}

// TestHandleListPricing_MergesIdenticalOffers reproduces the duplicate-row
// bug from the 2026-06-10 API review: three upstream offerings identical in
// every customer-visible field (a100_80gb / us-east / on_demand / 1.58)
// rendered as three identical rows. They must merge into ONE row with
// availability summed.
func TestHandleListPricing_MergesIdenticalOffers(t *testing.T) {
	cache := catalogTestCache(t, []availability.AvailableOffering{
		{GPUModel: "a100_80gb", GPUCount: 1, VRAMGB: 80, PricePerHour: 1.58, Region: "us-east", Tier: "on_demand", AvailableCount: 4},
		{GPUModel: "a100_80gb", GPUCount: 1, VRAMGB: 80, PricePerHour: 1.58, Region: "us-east", Tier: "on_demand", AvailableCount: 4},
		{GPUModel: "a100_80gb", GPUCount: 1, VRAMGB: 80, PricePerHour: 1.58, Region: "us-east", Tier: "on_demand", AvailableCount: 2},
	})
	page := listPricing(t, PublicDeps{AvailCache: cache}, "/v1/pricing")
	if len(page.Data) != 1 {
		t.Fatalf("merge: got %d rows, want 1; rows=%+v", len(page.Data), page.Data)
	}
	if page.Data[0].Available != 10 {
		t.Errorf("available: got %d, want summed 10", page.Data[0].Available)
	}
}

// TestHandleListPricing_AggregatesByCheapestPrice — issue #102: offers with the
// same (gpu_type, gpu_count, region, tier, boot class) but DIFFERENT prices
// collapse to one row (cheapest wins, availability summed) instead of surfacing
// as indistinguishable duplicate rows. A $0 dedicated-style sibling must not win
// as "cheapest"; a different boot class stays a separate, distinguishable row.
func TestHandleListPricing_AggregatesByCheapestPrice(t *testing.T) {
	cache := catalogTestCache(t, []availability.AvailableOffering{
		{GPUModel: "a100_80gb", GPUCount: 8, VRAMGB: 80, PricePerHour: 11.75, Region: "eu-west", Tier: "on_demand", AvailableCount: 1},
		{GPUModel: "a100_80gb", GPUCount: 8, VRAMGB: 80, PricePerHour: 8.48, Region: "eu-west", Tier: "on_demand", AvailableCount: 14},
		{GPUModel: "a100_80gb", GPUCount: 8, VRAMGB: 80, PricePerHour: 0, Region: "eu-west", Tier: "on_demand", AvailableCount: 2},
		{GPUModel: "a100_80gb", GPUCount: 8, VRAMGB: 80, PricePerHour: 9.0, Region: "eu-west", Tier: "on_demand", AvailableCount: 3, InstantBoot: true},
	})
	page := listPricing(t, PublicDeps{AvailCache: cache}, "/v1/pricing")

	// #102 invariant: no two rows share (gpu_type, gpu_count, region, tier,
	// instant_boot).
	type k struct {
		typ, region, tier string
		count             int
		boot              bool
	}
	rowByKey := map[k]Pricing{}
	for _, row := range page.Data {
		key := k{row.GPUType, row.Region, row.Tier, row.GPUCount, row.InstantBoot}
		if _, dup := rowByKey[key]; dup {
			t.Errorf("duplicate row for key %+v — aggregation invariant violated", key)
		}
		rowByKey[key] = row
	}
	if len(page.Data) != 2 {
		t.Fatalf("rows: got %d, want 2 (merged on_demand + separate instant-boot); rows=%+v", len(page.Data), page.Data)
	}

	merged := rowByKey[k{"a100_80gb", "eu-west", "on_demand", 8, false}]
	if merged.PricePerHour != 8.48 {
		t.Errorf("merged price: got %v, want 8.48 (cheapest positive — $0 sibling must not win)", merged.PricePerHour)
	}
	if merged.Available != 17 { // 1 + 14 + 2
		t.Errorf("merged available: got %d, want 17 (summed across all merged offers)", merged.Available)
	}
	instant := rowByKey[k{"a100_80gb", "eu-west", "on_demand", 8, true}]
	if instant.PricePerHour != 9.0 || instant.Available != 3 {
		t.Errorf("instant-boot row: got %+v, want price 9.0 avail 3 (distinct by instant_boot)", instant)
	}
}

// TestHandleListPricing_GPUCountSeparatesNodeConfigs verifies multi-GPU node
// configs stay distinct rows and surface gpu_count — the "wild price spread"
// from the review ($11.94 next to $1.47 for a100_80gb) was an 8-GPU node
// price rendered without its count.
func TestHandleListPricing_GPUCountSeparatesNodeConfigs(t *testing.T) {
	cache := catalogTestCache(t, []availability.AvailableOffering{
		{GPUModel: "a100_80gb", GPUCount: 1, VRAMGB: 80, PricePerHour: 1.47, Region: "us-east", Tier: "on_demand", AvailableCount: 3},
		{GPUModel: "a100_80gb", GPUCount: 8, VRAMGB: 80, PricePerHour: 11.94, Region: "us-east", Tier: "on_demand", AvailableCount: 1},
	})
	page := listPricing(t, PublicDeps{AvailCache: cache}, "/v1/pricing")
	if len(page.Data) != 2 {
		t.Fatalf("rows: got %d, want 2 (1x and 8x configs)", len(page.Data))
	}
	// Sort key orders gpu_count asc within the same (type, region, tier).
	if page.Data[0].GPUCount != 1 || page.Data[1].GPUCount != 8 {
		t.Errorf("gpu_count: got [%d, %d], want [1, 8]", page.Data[0].GPUCount, page.Data[1].GPUCount)
	}
	if page.Data[1].PricePerHour != 11.94 {
		t.Errorf("8x node price: got %v, want 11.94", page.Data[1].PricePerHour)
	}
}

// TestHandleListPricing_DropsZeroAvailabilityByDefault: out-of-stock offers
// are noise for "what can I provision right now" — they are omitted unless
// the caller opts in with ?include_unavailable=true.
func TestHandleListPricing_DropsZeroAvailabilityByDefault(t *testing.T) {
	cache := catalogTestCache(t, []availability.AvailableOffering{
		{GPUModel: "a40", GPUCount: 1, VRAMGB: 48, PricePerHour: 0.47, Region: "us-east", Tier: "on_demand", AvailableCount: 0},
		{GPUModel: "a40", GPUCount: 1, VRAMGB: 48, PricePerHour: 0.47, Region: "eu-west", Tier: "on_demand", AvailableCount: 4},
	})
	deps := PublicDeps{AvailCache: cache}

	page := listPricing(t, deps, "/v1/pricing")
	if len(page.Data) != 1 || page.Data[0].Region != "eu-west" {
		t.Fatalf("default: got %+v, want only the eu-west in-stock row", page.Data)
	}

	page = listPricing(t, deps, "/v1/pricing?include_unavailable=true")
	if len(page.Data) != 2 {
		t.Fatalf("include_unavailable: got %d rows, want 2", len(page.Data))
	}
}

// TestHandleListPricing_QueryFilters: the gpu CLI has sent ?gpu_type/?region/
// ?tier since v1.0 but the server silently ignored them — `gpu pricing
// --gpu-type X` returned the whole grid. They are now honored server-side.
func TestHandleListPricing_QueryFilters(t *testing.T) {
	cache := catalogTestCache(t, sampleOfferings())
	deps := PublicDeps{AvailCache: cache}

	page := listPricing(t, deps, "/v1/pricing?gpu_type=a100_pcie")
	if len(page.Data) != 2 {
		t.Fatalf("gpu_type filter: got %d rows, want 2", len(page.Data))
	}
	for _, row := range page.Data {
		if row.GPUType != "a100_pcie" {
			t.Errorf("gpu_type filter leaked row: %+v", row)
		}
	}

	page = listPricing(t, deps, "/v1/pricing?gpu_type=h100_sxm&region=us-west&tier=spot")
	if len(page.Data) != 1 || page.Data[0].PricePerHour != 1.9 {
		t.Fatalf("combined filters: got %+v, want the single us-west spot row", page.Data)
	}
}

// deploymentFixture is a mixed vm/container/unclassified catalog: the CLI
// documents `gpu pricing --type vm` as the escape hatch for workloads that run
// their own Docker (container offerings can't do Docker-in-Docker), so the
// filter must actually narrow server-side — it used to be silently ignored.
func deploymentFixture() []availability.AvailableOffering {
	return []availability.AvailableOffering{
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.5, Region: "us-east", Tier: "on_demand", AvailableCount: 4, DeploymentType: "vm", CertifiedImage: true, OfferingID: "vm-certified"},
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.4, Region: "us-west", Tier: "on_demand", AvailableCount: 3, DeploymentType: "vm", CertifiedImage: false, OfferingID: "vm-raw"},
		{GPUModel: "a100_pcie", GPUCount: 1, VRAMGB: 40, PricePerHour: 1.2, Region: "us-east", Tier: "on_demand", AvailableCount: 6, DeploymentType: "container", CertifiedImage: true, OfferingID: "ct-certified"},
		// Pre-field cache row: deployment_type "" is UNKNOWN, not "vm".
		{GPUModel: "a40", GPUCount: 1, VRAMGB: 48, PricePerHour: 0.5, Region: "eu-west", Tier: "on_demand", AvailableCount: 2, OfferingID: "unclassified"},
	}
}

// TestHandleListPricing_DeploymentTypeFilter: ?deployment_type= narrows to one
// deployment class and drops unclassified ("") rows — answering "which capacity
// can run my Docker workload" must never include capacity we can't classify.
func TestHandleListPricing_DeploymentTypeFilter(t *testing.T) {
	cache := catalogTestCache(t, deploymentFixture())
	deps := PublicDeps{AvailCache: cache}

	page := listPricing(t, deps, "/v1/pricing?deployment_type=vm")
	if len(page.Data) != 2 {
		t.Fatalf("deployment_type=vm: got %d rows, want 2; rows=%+v", len(page.Data), page.Data)
	}
	for _, row := range page.Data {
		if row.DeploymentType != "vm" {
			t.Errorf("deployment_type=vm leaked a %q row: %+v", row.DeploymentType, row)
		}
	}

	page = listPricing(t, deps, "/v1/pricing?deployment_type=container")
	if len(page.Data) != 1 || page.Data[0].DeploymentType != "container" {
		t.Fatalf("deployment_type=container: got %+v, want the single container row", page.Data)
	}
}

// TestHandleListPricing_CertifiedImageFilter: ?certified_image=true|false is
// the second filter the CLI has always sent and the server used to ignore.
func TestHandleListPricing_CertifiedImageFilter(t *testing.T) {
	cache := catalogTestCache(t, deploymentFixture())
	deps := PublicDeps{AvailCache: cache}

	page := listPricing(t, deps, "/v1/pricing?certified_image=true")
	if len(page.Data) != 2 {
		t.Fatalf("certified_image=true: got %d rows, want 2; rows=%+v", len(page.Data), page.Data)
	}
	for _, row := range page.Data {
		if !row.CertifiedImage {
			t.Errorf("certified_image=true leaked a non-certified row: %+v", row)
		}
	}

	page = listPricing(t, deps, "/v1/pricing?certified_image=false")
	if len(page.Data) != 2 {
		t.Fatalf("certified_image=false: got %d rows, want 2; rows=%+v", len(page.Data), page.Data)
	}
	for _, row := range page.Data {
		if row.CertifiedImage {
			t.Errorf("certified_image=false leaked a certified row: %+v", row)
		}
	}

	// Combined with deployment_type: the two filters intersect, not override.
	page = listPricing(t, deps, "/v1/pricing?deployment_type=vm&certified_image=true")
	if len(page.Data) != 1 || page.Data[0].Region != "us-east" {
		t.Fatalf("combined filters: got %+v, want the single certified vm row", page.Data)
	}
}

// TestHandleListPricing_InvalidFilterValues: both new filters are strict enums
// (T-q82-01). A typo must be a loud 400, never a silently-empty page that reads
// as "no such capacity exists". The broken cache doubles as the assertion that
// validation runs BEFORE the cache read — a 503 here would mean it doesn't.
func TestHandleListPricing_InvalidFilterValues(t *testing.T) {
	deps := PublicDeps{AvailCache: catalogTestCacheBroken(t)}
	mux := http.NewServeMux()
	RegisterCatalogRoutes(mux, deps)

	for _, target := range []string{
		"/v1/pricing?deployment_type=bogus",
		"/v1/pricing?deployment_type=VM",
		"/v1/pricing?certified_image=yes",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s: got %d, want 400; body=%s", target, w.Code, w.Body.String())
		}
	}
}

// TestHandleListPricing_CursorPaginationLossless is the regression test for
// the tie-drop bug: the old cursor key (gpu_type|region|tier) was shared by
// every price point of the same offer family, and the strict `key > after`
// comparison silently dropped all rows tied with the page boundary. With the
// full-identity key, walking the cursor must yield every row exactly once.
func TestHandleListPricing_CursorPaginationLossless(t *testing.T) {
	// 5 rows that ALL share (gpu_type, region, tier) — worst case for the old
	// key. Post-#102 price no longer distinguishes rows (same-SKU price variants
	// merge), so the two gpu_count=1 rows are split by instant_boot instead; the
	// rest are distinct by gpu_count. Five distinct rows under the aggregation key
	// keep this a real pagination-losslessness test.
	cache := catalogTestCache(t, []availability.AvailableOffering{
		{GPUModel: "a100_40gb", GPUCount: 1, VRAMGB: 40, PricePerHour: 0.94, Region: "india-south", Tier: "on_demand", AvailableCount: 4, InstantBoot: false},
		{GPUModel: "a100_40gb", GPUCount: 1, VRAMGB: 40, PricePerHour: 2.10, Region: "india-south", Tier: "on_demand", AvailableCount: 1, InstantBoot: true},
		{GPUModel: "a100_40gb", GPUCount: 2, VRAMGB: 40, PricePerHour: 4.21, Region: "india-south", Tier: "on_demand", AvailableCount: 2},
		{GPUModel: "a100_40gb", GPUCount: 4, VRAMGB: 40, PricePerHour: 8.42, Region: "india-south", Tier: "on_demand", AvailableCount: 3},
		{GPUModel: "a100_40gb", GPUCount: 8, VRAMGB: 40, PricePerHour: 16.84, Region: "india-south", Tier: "on_demand", AvailableCount: 1},
	})
	deps := PublicDeps{AvailCache: cache}

	seen := map[string]bool{}
	target := "/v1/pricing?limit=2"
	for hops := 0; hops < 10; hops++ {
		page := listPricing(t, deps, target)
		for _, row := range page.Data {
			key := fmt.Sprintf("%s|%d|%.2f", row.GPUType, row.GPUCount, row.PricePerHour)
			if seen[key] {
				t.Fatalf("row repeated across pages: %s", key)
			}
			seen[key] = true
		}
		if page.NextCursor == nil {
			break
		}
		target = "/v1/pricing?limit=2&cursor=" + *page.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("pagination lost rows: got %d unique rows, want 5 (seen=%v)", len(seen), seen)
	}
}

// TestHandleListPricing_CommunityBadge is the public-API half of the Pitfall-1
// guard: a community and a datacenter offering identical in every other
// aggregation-key field must surface as TWO distinct rows (community joins
// offerKey), and the community row marshals "community":true. If community were
// absent from offerKey the pair would merge and mislabel datacenter capacity.
func TestHandleListPricing_CommunityBadge(t *testing.T) {
	cache := catalogTestCache(t, []availability.AvailableOffering{
		{GPUModel: "rtx_4090", GPUCount: 1, VRAMGB: 24, PricePerHour: 0.20, Region: "us-west", Tier: "on_demand", AvailableCount: 1, Community: true},
		{GPUModel: "rtx_4090", GPUCount: 1, VRAMGB: 24, PricePerHour: 0.30, Region: "us-west", Tier: "on_demand", AvailableCount: 5, Community: false},
	})
	deps := PublicDeps{AvailCache: cache}

	page := listPricing(t, deps, "/v1/pricing")
	if len(page.Data) != 2 {
		t.Fatalf("mixed community/datacenter same-SKU: got %d rows, want 2; rows=%+v", len(page.Data), page.Data)
	}

	var community, datacenter *Pricing
	for i := range page.Data {
		if page.Data[i].Community {
			community = &page.Data[i]
		} else {
			datacenter = &page.Data[i]
		}
	}
	if community == nil || datacenter == nil {
		t.Fatalf("expected one community + one datacenter row, got %+v", page.Data)
	}
	if community.Available != 1 || community.PricePerHour != 0.20 {
		t.Errorf("community row: got avail=%d price=%v, want avail=1 price=0.20 (never merged)", community.Available, community.PricePerHour)
	}
	if datacenter.Available != 5 || datacenter.PricePerHour != 0.30 {
		t.Errorf("datacenter row: got avail=%d price=%v, want avail=5 price=0.30 (badge never transferred)", datacenter.Available, datacenter.PricePerHour)
	}

	// The community row must marshal "community":true on the wire.
	b, err := json.Marshal(*community)
	if err != nil {
		t.Fatalf("marshal community row: %v", err)
	}
	if !strings.Contains(string(b), `"community":true`) {
		t.Errorf("community row missing \"community\":true, got: %s", string(b))
	}
}

// certifiedPricingRow mirrors the Phase 63 /v1/pricing row surface Plan 03 will
// add to public.Pricing. Decoding the live response into THIS local struct lets
// the Wave 0 contract test compile against today's field-less Pricing while
// still asserting the new wire fields — it stays RED until Plan 03 emits them.
type certifiedPricingRow struct {
	GPUType        string `json:"gpu_type"`
	CertifiedImage bool   `json:"certified_image"`
	DeploymentType string `json:"deployment_type"`
	OfferingID     string `json:"offering_id"`
	Community      bool   `json:"community"`
	Environment    *struct {
		Image       string `json:"image"`
		ImageDigest string `json:"image_digest"`
		DriverMin   string `json:"driver_min"`
		CUDA        string `json:"cuda"`
	} `json:"environment,omitempty"`
}

// TestCatalogCertifiedFields is the Wave 0 (Plan 01) RED contract for the
// Phase 63 catalog surface. Two invariants, both failing until Plan 03:
//
//  1. certified_image + deployment_type join the aggregation key — two
//     offerings identical in every legacy field but differing in those two
//     must surface as TWO rows (today's offerKey collapses them to one).
//  2. Each row carries certified_image, deployment_type, and a non-empty
//     offering_id (D-63-08); certified-capable rows publish the digest-pinned
//     environment contract (D-63-07). All server-derived — never customer input.
func TestCatalogCertifiedFields(t *testing.T) {
	cache := catalogTestCache(t, []availability.AvailableOffering{
		// certified-capable VM offering (normalized to the GPU.ai Certified Image).
		// OfferingID is populated in production by the normalization path
		// (availability.offeringID); seed it here since we construct the offering
		// directly and bypass Normalize.
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.5, Region: "us-east", Tier: "on_demand", AvailableCount: 4, CertifiedImage: true, DeploymentType: "vm", OfferingID: "a1b2c3d4e5f6"},
		// same SKU, container-native, NOT certified — must stay a distinct row.
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.5, Region: "us-east", Tier: "on_demand", AvailableCount: 6, CertifiedImage: false, DeploymentType: "container", OfferingID: "f6e5d4c3b2a1"},
	})
	deps := PublicDeps{AvailCache: cache}

	mux := http.NewServeMux()
	RegisterCatalogRoutes(mux, deps)
	req := httptest.NewRequest(http.MethodGet, "/v1/pricing", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var page struct {
		Data []certifiedPricingRow `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}

	// Invariant 1: the certified/deployment split keeps the two offerings apart.
	if len(page.Data) != 2 {
		t.Fatalf("certified split: got %d rows, want 2 (certified_image+deployment_type join the aggregation key); rows=%+v", len(page.Data), page.Data)
	}

	var certified, raw *certifiedPricingRow
	for i := range page.Data {
		if page.Data[i].CertifiedImage {
			certified = &page.Data[i]
		} else {
			raw = &page.Data[i]
		}
	}
	if certified == nil || raw == nil {
		t.Fatalf("expected one certified + one non-certified row, got %+v", page.Data)
	}

	// Invariant 2: every row carries the new descriptors.
	for _, row := range page.Data {
		if row.OfferingID == "" {
			t.Errorf("row %+v missing offering_id (D-63-08 pin target)", row)
		}
		if row.DeploymentType != "vm" && row.DeploymentType != "container" {
			t.Errorf("row %+v deployment_type = %q, want vm|container", row, row.DeploymentType)
		}
	}
	if certified.DeploymentType != "vm" {
		t.Errorf("certified row deployment_type = %q, want vm", certified.DeploymentType)
	}
	if raw.DeploymentType != "container" {
		t.Errorf("non-certified row deployment_type = %q, want container", raw.DeploymentType)
	}

	// Certified-capable row publishes the digest-pinned environment contract.
	if certified.Environment == nil {
		t.Fatalf("certified row missing environment contract (D-63-07)")
	}
	if !strings.Contains(certified.Environment.ImageDigest, "@sha256:") {
		t.Errorf("environment image_digest = %q, want a digest-pinned ref containing @sha256:", certified.Environment.ImageDigest)
	}
	if certified.Environment.DriverMin == "" || certified.Environment.CUDA == "" {
		t.Errorf("environment contract missing driver/CUDA floors: %+v", certified.Environment)
	}
}

// TestHandleListPricing_CommunityCursorStable verifies pricingCursorKey's
// community segment makes a community row and its otherwise-identical
// datacenter twin distinct cursor identities, so paginating across a page
// boundary that falls between them yields every row exactly once (no loss, no
// repeat).
func TestHandleListPricing_CommunityCursorStable(t *testing.T) {
	cache := catalogTestCache(t, []availability.AvailableOffering{
		{GPUModel: "rtx_4090", GPUCount: 1, VRAMGB: 24, PricePerHour: 0.20, Region: "us-west", Tier: "on_demand", AvailableCount: 1, Community: true},
		{GPUModel: "rtx_4090", GPUCount: 1, VRAMGB: 24, PricePerHour: 0.30, Region: "us-west", Tier: "on_demand", AvailableCount: 5, Community: false},
		{GPUModel: "rtx_4090", GPUCount: 8, VRAMGB: 24, PricePerHour: 2.40, Region: "us-west", Tier: "on_demand", AvailableCount: 2, Community: false},
	})
	deps := PublicDeps{AvailCache: cache}

	seen := map[string]int{}
	target := "/v1/pricing?limit=1"
	for hops := 0; hops < 10; hops++ {
		page := listPricing(t, deps, target)
		for _, row := range page.Data {
			key := fmt.Sprintf("%s|%d|%.2f|%t", row.GPUType, row.GPUCount, row.PricePerHour, row.Community)
			seen[key]++
			if seen[key] > 1 {
				t.Fatalf("row repeated across pages: %s", key)
			}
		}
		if page.NextCursor == nil {
			break
		}
		target = "/v1/pricing?limit=1&cursor=" + *page.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("pagination across community rows lost rows: got %d unique, want 3 (seen=%v)", len(seen), seen)
	}
}

// TestHandleListPricing_HardwareSpecs verifies /v1/pricing surfaces the exact
// per-offering cpu_cores/ram_gb/storage_gb, and that when offers merge the
// surfaced specs track the offer whose price is shown (cheapest-wins) — so the
// spec and the price always describe the same machine.
func TestHandleListPricing_HardwareSpecs(t *testing.T) {
	cache := catalogTestCache(t, []availability.AvailableOffering{
		// Two offers that merge (identical aggregation key) but differ in price
		// AND host specs. The cheaper offer's specs must be the ones surfaced.
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.90, Region: "us-east", Tier: "on_demand", AvailableCount: 2, CPUCores: 32, RAMGB: 256, StorageGB: 1024},
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.50, Region: "us-east", Tier: "on_demand", AvailableCount: 3, CPUCores: 26, RAMGB: 200, StorageGB: 512},
	})
	page := listPricing(t, PublicDeps{AvailCache: cache}, "/v1/pricing")
	if len(page.Data) != 1 {
		t.Fatalf("merge: got %d rows, want 1; rows=%+v", len(page.Data), page.Data)
	}
	row := page.Data[0]
	if row.PricePerHour != 2.50 {
		t.Fatalf("cheapest price should win: got %v, want 2.50", row.PricePerHour)
	}
	if row.CPUCores != 26 || row.RAMGB != 200 || row.StorageGB != 512 {
		t.Errorf("specs must track the cheapest offer: got cpu=%d ram=%d storage=%d, want 26/200/512",
			row.CPUCores, row.RAMGB, row.StorageGB)
	}
}

// TestHandleListGPUTypes_HardwareSpecs verifies /v1/gpu-types surfaces the
// representative cpu_cores/ram_gb/storage_gb for a model (first-sighting).
func TestHandleListGPUTypes_HardwareSpecs(t *testing.T) {
	cache := catalogTestCache(t, []availability.AvailableOffering{
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.5, Region: "us-east", Tier: "on_demand", AvailableCount: 4, CPUCores: 26, RAMGB: 200, StorageGB: 512},
	})
	deps := PublicDeps{AvailCache: cache}
	mux := http.NewServeMux()
	RegisterCatalogRoutes(mux, deps)
	req := httptest.NewRequest(http.MethodGet, "/v1/gpu-types", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var page Page[GPUType]
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Data) != 1 {
		t.Fatalf("rows: got %d, want 1", len(page.Data))
	}
	row := page.Data[0]
	if row.CPUCores != 26 || row.RAMGB != 200 || row.StorageGB != 512 {
		t.Errorf("gpu-types specs: got cpu=%d ram=%d storage=%d, want 26/200/512", row.CPUCores, row.RAMGB, row.StorageGB)
	}
}

// TestHandleListGPUTypes_NormalizationComposesWithMin is the 2026-08-12 support
// ticket end to end, through BOTH layers: raw upstream figures go through the
// availability seam's nameplate normalization, then the type-level minimum.
// A fleet mixing 16GB and 32GB V100 silicon must card at 16 — the figure every
// launch can honor — while an L4 whose only sighting measures 22 must card at
// its 24GB nameplate.
func TestHandleListGPUTypes_NormalizationComposesWithMin(t *testing.T) {
	raw := []provider.GPUOffering{
		{Provider: "alpha", GPUType: "v100", GPUCount: 1, VRAMPerGPUGB: 16, PricePerHour: 0.14, Region: "us-east", Tier: provider.TierOnDemand},
		{Provider: "generic", GPUType: "v100", GPUCount: 1, VRAMPerGPUGB: 32, PricePerHour: 2.40, Region: "us-east", Tier: provider.TierOnDemand},
		{Provider: "alpha", GPUType: "l4", GPUCount: 1, VRAMPerGPUGB: 22, PricePerHour: 0.30, Region: "us-east", Tier: provider.TierOnDemand},
	}
	offerings := make([]availability.AvailableOffering, 0, len(raw))
	for _, o := range raw {
		offerings = append(offerings, availability.ToAvailableOffering(
			o, availability.DefaultMarkupPct, availability.DefaultMinRetailMarkupPct, 0, 0, 0))
	}

	cache := catalogTestCache(t, offerings)
	deps := PublicDeps{AvailCache: cache}
	mux := http.NewServeMux()
	RegisterCatalogRoutes(mux, deps)

	req := httptest.NewRequest(http.MethodGet, "/v1/gpu-types", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var page Page[GPUType]
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]int{}
	for _, row := range page.Data {
		got[row.GPUType] = row.VRAMGB
	}
	if got["v100"] != 16 {
		t.Errorf("v100 vram_gb = %d, want 16 — a mixed 16/32 fleet must card at what every launch delivers", got["v100"])
	}
	if got["l4"] != 24 {
		t.Errorf("l4 vram_gb = %d, want 24 — a lone measured sighting must card at nameplate", got["l4"])
	}
}
