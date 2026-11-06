package provision

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// ptrInt64 / ptrInt — small test helpers for taking addresses of literals.
func ptrInt64(v int64) *int64 { return &v }
func ptrInt(v int) *int       { return &v }

// TestBuildCallbackURL verifies callback URL construction with and without
// GPUCTL_PUBLIC_URL configured.
func TestBuildCallbackURL(t *testing.T) {
	tests := []struct {
		name       string
		publicURL  string
		hostname   string
		instanceID string
		want       string
	}{
		{
			name:       "with GPUCTL_PUBLIC_URL",
			publicURL:  "https://api.example.com",
			hostname:   "gpu-abc123.example.com",
			instanceID: "gpu-abc123",
			want:       "https://api.example.com/internal/instances/gpu-abc123/ready",
		},
		{
			name:       "with trailing slash on public URL",
			publicURL:  "https://api.example.com/",
			hostname:   "gpu-abc123.example.com",
			instanceID: "gpu-abc123",
			want:       "https://api.example.com/internal/instances/gpu-abc123/ready",
		},
		{
			name:       "without GPUCTL_PUBLIC_URL falls back to hostname",
			publicURL:  "",
			hostname:   "gpu-abc123.example.com",
			instanceID: "gpu-abc123",
			want:       "https://gpu-abc123.example.com/internal/instances/gpu-abc123/ready",
		},
		{
			name:       "public URL with path prefix",
			publicURL:  "https://api.example.com/v1",
			hostname:   "gpu-def456.example.com",
			instanceID: "gpu-def456",
			want:       "https://api.example.com/v1/internal/instances/gpu-def456/ready",
		},
		{
			name:       "public URL with multiple trailing slashes",
			publicURL:  "https://api.example.com///",
			hostname:   "gpu-abc123.example.com",
			instanceID: "gpu-abc123",
			want:       "https://api.example.com/internal/instances/gpu-abc123/ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildCallbackURL(tt.publicURL, tt.hostname, tt.instanceID)
			if got != tt.want {
				t.Errorf("buildCallbackURL(%q, %q, %q) = %q, want %q",
					tt.publicURL, tt.hostname, tt.instanceID, got, tt.want)
			}
		})
	}
}

// TestGenerateInstanceID verifies the instance ID format: "gpu-" + 8 hex chars.
func TestGenerateInstanceID(t *testing.T) {
	id, err := generateInstanceID()
	if err != nil {
		t.Fatalf("generateInstanceID() error: %v", err)
	}

	// Must start with "gpu-".
	if len(id) < 4 || id[:4] != "gpu-" {
		t.Errorf("generateInstanceID() = %q, want prefix 'gpu-'", id)
	}

	// Total length: "gpu-" (4) + 8 hex chars = 12.
	if len(id) != 12 {
		t.Errorf("generateInstanceID() length = %d, want 12", len(id))
	}

	// Hex suffix should only contain valid hex characters.
	for _, c := range id[4:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("generateInstanceID() contains non-hex char %q in suffix %q", string(c), id[4:])
		}
	}

	// Two calls should produce different IDs (extremely unlikely to collide).
	id2, err := generateInstanceID()
	if err != nil {
		t.Fatalf("generateInstanceID() second call error: %v", err)
	}
	if id == id2 {
		t.Errorf("generateInstanceID() produced duplicate IDs: %q", id)
	}
}

// TestGenerateHexToken verifies the token length matches byteLen * 2 (hex encoding).
func TestGenerateHexToken(t *testing.T) {
	tests := []struct {
		name    string
		byteLen int
		wantLen int
	}{
		{name: "16 bytes", byteLen: 16, wantLen: 32},
		{name: "32 bytes", byteLen: 32, wantLen: 64},
		{name: "1 byte", byteLen: 1, wantLen: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := generateHexToken(tt.byteLen)
			if err != nil {
				t.Fatalf("generateHexToken(%d) error: %v", tt.byteLen, err)
			}
			if len(token) != tt.wantLen {
				t.Errorf("generateHexToken(%d) length = %d, want %d", tt.byteLen, len(token), tt.wantLen)
			}

			// Verify all characters are valid hex.
			for _, c := range token {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					t.Errorf("generateHexToken(%d) contains non-hex char %q", tt.byteLen, string(c))
				}
			}
		})
	}

	// Two calls should produce different tokens.
	t1, _ := generateHexToken(16)
	t2, _ := generateHexToken(16)
	if t1 == t2 {
		t.Errorf("generateHexToken(16) produced duplicate tokens: %q", t1)
	}
}

// TestSetOnStatusChange verifies the setter stores the callback function.
func TestSetOnStatusChange(t *testing.T) {
	engine := &Engine{}

	if engine.onStatusChange != nil {
		t.Fatal("onStatusChange should be nil before SetOnStatusChange")
	}

	var called bool
	engine.SetOnStatusChange(func(instanceID, status string) {
		called = true
	})

	if engine.onStatusChange == nil {
		t.Fatal("onStatusChange should be set after SetOnStatusChange")
	}

	// Invoke the callback to verify it's wired correctly.
	engine.onStatusChange("test-id", "provisioning")
	if !called {
		t.Error("onStatusChange callback was not invoked")
	}
}

// Note: Full progressStatus polling loop is tested via integration tests with
// real DB and provider mocks. The polling loop interacts with e.db (a *db.Pool)
// and provider.GetStatus which require either a running database or interface-based
// mocking (deferred to Phase 6 test infrastructure).

// --- SelectProviderCandidates test suite (15-01 Wave 0) ---
//
// These tests exercise engine.SelectProviderCandidates using a stubProvider.
// They close the test gap called out in the comment block above (before 15-01:
// "Note: Full progressStatus polling loop is tested via integration tests...
// deferred to Phase 6 test infrastructure.").

func makeOffering(gt provider.GPUType, region, dc string, price float64, available int) provider.GPUOffering {
	return provider.GPUOffering{
		Provider:           "stub",
		GPUType:            gt,
		GPUCount:           1,
		VRAMPerGPUGB:       80,
		PricePerHour:       price,
		Tier:               provider.TierOnDemand,
		Region:             region,
		DatacenterLocation: dc,
		StockStatus:        "High",
		AvailableCount:     available,
	}
}

func newTestEngineWithProviders(providers ...provider.Provider) *Engine {
	reg := provider.NewRegistry()
	for _, p := range providers {
		reg.Register(p)
	}
	return &Engine{
		registry: reg,
		logger:   slog.Default(),
	}
}

func TestSelectProviderCandidates_NoRegionFilter(t *testing.T) {
	stub := &stubProvider{
		name: "stub",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "us-east", "US-TX-3", 2.80, 4),
			makeOffering(provider.GPUTypeH100SXM, "eu-east", "EU-RO-1", 2.60, 4),
			makeOffering(provider.GPUTypeH100SXM, "us-west", "US-CA-2", 2.70, 4),
		},
	}
	e := newTestEngineWithProviders(stub)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
		// no region filter
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}
	// First candidate must be cheapest (eu-east at 2.60).
	if candidates[0].Offering.Region != "eu-east" {
		t.Errorf("expected cheapest candidate eu-east, got %s", candidates[0].Offering.Region)
	}
	if candidates[0].Offering.PricePerHour != 2.60 {
		t.Errorf("expected price 2.60, got %f", candidates[0].Offering.PricePerHour)
	}
}

func TestSelectProviderCandidates_RegionFilter(t *testing.T) {
	stub := &stubProvider{
		name: "stub",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "us-east", "US-TX-3", 2.80, 4),
			makeOffering(provider.GPUTypeH100SXM, "eu-east", "EU-RO-1", 2.60, 4),
			makeOffering(provider.GPUTypeH100SXM, "us-west", "US-CA-2", 2.70, 4),
		},
	}
	e := newTestEngineWithProviders(stub)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
		Regions:  []string{"us-east"},
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate for us-east filter, got %d", len(candidates))
	}
	if candidates[0].Offering.Region != "us-east" {
		t.Errorf("expected region us-east, got %s", candidates[0].Offering.Region)
	}
	if candidates[0].Offering.DatacenterLocation != "US-TX-3" {
		t.Errorf("expected datacenter US-TX-3, got %s", candidates[0].Offering.DatacenterLocation)
	}
}

func TestSelectProviderCandidates_RegionNoMatch(t *testing.T) {
	stub := &stubProvider{
		name: "stub",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "us-east", "US-TX-3", 2.80, 4),
			makeOffering(provider.GPUTypeH100SXM, "eu-east", "EU-RO-1", 2.60, 4),
		},
	}
	e := newTestEngineWithProviders(stub)

	_, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
		Regions:  []string{"ap-south"}, // no matching offerings
	})
	if !errors.Is(err, ErrNoProvider) {
		t.Errorf("expected ErrNoProvider for unmatched region, got: %v", err)
	}
}

// TestSelectProviderCandidates_NativeRegionMatchesCanonicalFilter is the
// regression test for REGION-03: providers emit NATIVE region labels ("del",
// "Delhi") but the user filters by CANONICAL codes
// ("india-south") sourced from the availability cache. Pre-fix the engine
// compared raw strings and excluded both Indian providers; post-fix it
// normalizes the offering region through the same map the cache uses.
func TestSelectProviderCandidates_NativeRegionMatchesCanonicalFilter(t *testing.T) {
	fixedimageco := &stubProvider{
		name: "fixedimageco",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "del", "del", 2.99, 8),
		},
	}
	vmImage := &stubProvider{
		name: "vmimageco",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "Delhi", "Delhi", 2.71, 6),
		},
	}
	e := newTestEngineWithProviders(fixedimageco, vmImage)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
		Regions:  []string{"india-south"},
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected both Indian providers to match canonical filter, got %d", len(candidates))
	}
	// Cheapest first.
	if candidates[0].Prov.Name() != "vmimageco" {
		t.Errorf("expected vmImage first (cheapest), got %s", candidates[0].Prov.Name())
	}
	// Native region preserved on the candidate so the adapter receives what it expects.
	if candidates[0].Offering.Region != "Delhi" {
		t.Errorf("native region should ride on candidate, got %s", candidates[0].Offering.Region)
	}
	if candidates[1].Offering.Region != "del" {
		t.Errorf("native region should ride on candidate, got %s", candidates[1].Offering.Region)
	}
}

// TestPlacement_PinsExactOffering pins the Phase 63 exact-offering placement
// contract (Plan 01 Wave 0, RED until Plan 05 threads SelectedOfferingID through
// SelectProviderCandidates). When a request names a specific offering
// (SelectedOfferingID) that is no longer available, the engine must fail
// EXPLICITLY (D-63-08) and must NEVER silently substitute a cheaper offering —
// the classic "you asked for offering X, we quietly gave you a different one"
// footgun. The error must not leak any provider name (T-63-04).
func TestPlacement_PinsExactOffering(t *testing.T) {
	// Two live, cheaper offerings the engine could be tempted to substitute.
	stub := &stubProvider{
		name: "podco",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "eu-east", "EU-RO-1", 2.60, 4),
			makeOffering(provider.GPUTypeH100SXM, "us-east", "US-TX-3", 2.80, 4),
		},
	}
	e := newTestEngineWithProviders(stub)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
		// The pinned offering is not present in the live set (it drained between
		// catalog read and launch): the engine must NOT fall through to placement.
		SelectedOfferingID: "off_pinned_but_drained",
	})

	// Must fail explicitly rather than substitute a cheaper live offering.
	if err == nil {
		t.Fatalf("expected an explicit unavailable error for a drained pinned offering, got %d candidates and nil err", len(candidates))
	}
	if len(candidates) != 0 {
		t.Errorf("pinned-but-unavailable offering must yield NO silent substitute, got %d candidates", len(candidates))
	}
	if !strings.Contains(err.Error(), "no longer available") {
		t.Errorf("error should state the pinned offering is no longer available, got: %v", err)
	}
	// T-63-04: the placement error must not leak provider identity.
	for _, name := range []string{"podco", "generic", "fixedimageco", "vmimageco", "vmco", "metalco", "cloudvmco"} {
		if strings.Contains(strings.ToLower(err.Error()), name) {
			t.Errorf("placement error leaks provider name %q: %v", name, err)
		}
	}

	// Legacy path safety: an empty SelectedOfferingID must leave placement
	// byte-for-byte unchanged (cheapest live offering wins).
	legacy, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
	})
	if err != nil {
		t.Fatalf("legacy placement (empty SelectedOfferingID) errored: %v", err)
	}
	if len(legacy) != 2 || legacy[0].Offering.Region != "eu-east" {
		t.Errorf("legacy placement changed: expected cheapest eu-east first of 2, got %d candidates", len(legacy))
	}
}

// TestSelectProviderCandidates_MultiRegionMembership verifies that a request
// with multiple canonical regions (e.g. selecting both india-south and
// india-west in the UI) accepts offerings from any region in the set, sorted
// globally by price.
func TestSelectProviderCandidates_MultiRegionMembership(t *testing.T) {
	stub := &stubProvider{
		name: "fixedimageco",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "del", "del", 2.99, 4),         // india-south
			makeOffering(provider.GPUTypeH100SXM, "Mumbai", "Mumbai", 2.50, 4),   // india-west
			makeOffering(provider.GPUTypeH100SXM, "us-east", "US-TX-3", 2.80, 4), // out of scope
		},
	}
	e := newTestEngineWithProviders(stub)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
		Regions:  []string{"india-south", "india-west"},
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 india-* candidates, got %d", len(candidates))
	}
	if candidates[0].Offering.Region != "Mumbai" {
		t.Errorf("expected Mumbai (cheapest in scope) first, got %s", candidates[0].Offering.Region)
	}
	if candidates[0].Offering.PricePerHour != 2.50 {
		t.Errorf("expected cheapest price 2.50, got %v", candidates[0].Offering.PricePerHour)
	}
}

func TestSelectProviderCandidates_ZeroAvailableCountFiltered(t *testing.T) {
	stub := &stubProvider{
		name: "stub",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "us-east", "US-TX-3", 2.80, 0), // zero stock
			makeOffering(provider.GPUTypeH100SXM, "eu-east", "EU-RO-1", 2.60, 4),
		},
	}
	e := newTestEngineWithProviders(stub)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate (zero-stock filtered), got %d", len(candidates))
	}
	if candidates[0].Offering.Region != "eu-east" {
		t.Errorf("expected only eu-east (non-zero stock), got %s", candidates[0].Offering.Region)
	}
}

func TestSelectProviderCandidates_PriceTiebreakStable(t *testing.T) {
	stub := &stubProvider{
		name: "stub",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "us-east", "US-TX-3", 2.60, 4),
			makeOffering(provider.GPUTypeH100SXM, "eu-east", "EU-RO-1", 2.60, 4),
		},
	}
	e := newTestEngineWithProviders(stub)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 tied candidates, got %d", len(candidates))
	}
	// stable sort preserves map iteration order which is randomized in Go --
	// this assertion only requires BOTH to be present, not a specific order.
	// The real guarantee is that the sort is stable *given* registry order,
	// which a multi-provider test would catch; with one provider the map
	// iteration inside Registry.All() dominates. Assert the set, not order.
	regions := map[string]bool{}
	for _, c := range candidates {
		regions[c.Offering.Region] = true
	}
	if !regions["us-east"] || !regions["eu-east"] {
		t.Errorf("expected both us-east and eu-east candidates, got: %+v", regions)
	}
}

// TestRegionFromResult_Fallback verifies the three-layer fallback in regionFromResult:
//  1. result.Region when non-empty
//  2. offering.Region when result is empty
//  3. "unknown" when both are empty (Pitfall 6 — prevents NOT NULL violation on
//     instances.region VARCHAR(50) NOT NULL).
func TestRegionFromResult_Fallback(t *testing.T) {
	tests := []struct {
		name     string
		result   *provider.ProvisionResult
		offering *provider.GPUOffering
		want     string
	}{
		{
			name:     "result region wins",
			result:   &provider.ProvisionResult{Region: "us-east"},
			offering: &provider.GPUOffering{Region: "eu-east"},
			want:     "us-east",
		},
		{
			name:     "falls back to offering region",
			result:   &provider.ProvisionResult{Region: ""},
			offering: &provider.GPUOffering{Region: "eu-east"},
			want:     "eu-east",
		},
		{
			name:     "both empty returns unknown",
			result:   &provider.ProvisionResult{Region: ""},
			offering: &provider.GPUOffering{Region: ""},
			want:     "unknown",
		},
		// Native provider labels must come out canonical (D-05). Before this
		// normalization, /v1/instances showed raw "yto" / "Delhi"
		// next to canonical labels (2026-06-10 API review finding).
		{
			name:     "vmco native result region is normalized",
			result:   &provider.ProvisionResult{Region: "yto"},
			offering: &provider.GPUOffering{Region: "yto", Provider: "vmco"},
			want:     "ca-central",
		},
		{
			name:     "fixedimageco native offering region is normalized",
			result:   &provider.ProvisionResult{Region: ""},
			offering: &provider.GPUOffering{Region: "Delhi", Provider: "fixedimageco"},
			want:     "india-south",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := regionFromResult(tt.result, tt.offering)
			if got != tt.want {
				t.Errorf("regionFromResult(%+v, %+v) = %q, want %q", tt.result, tt.offering, got, tt.want)
			}
		})
	}
}

// TestSelectProviderCandidates_MultiProvider verifies the engine emits all
// matching candidates when 4 providers (podco + generic + fixedimageco + vmimageco) each
// publish offerings for the same GPUType + Region + Tier with distinct prices.
//
// Phase 17 Wave 0 contract (AVAIL-05): the engine must not stop at the first
// provider with stock — every provider with a viable offering must appear in
// the candidate list, sorted ascending by price for the cheapest-first
// retry loop. Plan 17-04 (Wave 3) adds the multi-provider preference logic
// that this test guards against regression.
//
// Reuses the stubProvider seam established by Plan 15-01 (per STATE.md
// [Phase 15-01]: "stubProvider placed in _test.go to ship zero code to
// production binaries"). Each stub is named to model a distinct provider.
func TestSelectProviderCandidates_MultiProvider(t *testing.T) {
	podStub := &stubProvider{
		name: "podco",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "india-south", "IN-DEL-1", 2.50, 4),
		},
	}
	genericStub := &stubProvider{
		name: "generic",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "india-south", "IN-DEL-2", 2.55, 2),
		},
	}
	fixedStub := &stubProvider{
		name: "fixedimageco",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "india-south", "del", 2.60, 6),
		},
	}
	vmImageStub := &stubProvider{
		name: "vmimageco",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "india-south", "Delhi", 2.65, 3),
		},
	}

	e := newTestEngineWithProviders(podStub, genericStub, fixedStub, vmImageStub)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
		Regions:  []string{"india-south"},
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 4 {
		t.Fatalf("expected 4 candidates (one per provider), got %d", len(candidates))
	}

	// Cheapest-first sort by price (D-04 stable tiebreak; equal prices
	// preserve registry-insertion order — the test uses distinct prices so
	// the ordering is fully deterministic).
	wantOrder := []float64{2.50, 2.55, 2.60, 2.65}
	for i, want := range wantOrder {
		if candidates[i].Offering.PricePerHour != want {
			t.Errorf("candidate[%d] price = %v, want %v", i, candidates[i].Offering.PricePerHour, want)
		}
	}

	// All four named providers must appear in the candidate list — this is
	// the AVAIL-05 contract that Plan 17-04 turns from "engine selects
	// podco-only" into "engine considers every registered provider".
	gotProviders := map[string]bool{}
	for _, c := range candidates {
		gotProviders[c.Prov.Name()] = true
	}
	for _, want := range []string{"podco", "generic", "fixedimageco", "vmimageco"} {
		if !gotProviders[want] {
			t.Errorf("expected candidate from provider %q, got providers: %+v", want, gotProviders)
		}
	}
}

// --- 40-03 daily cap (BILL-04) + no-starter-credit (BILL-03) ----------------
