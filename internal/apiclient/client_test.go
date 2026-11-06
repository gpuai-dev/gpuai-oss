package apiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_ListInstances_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test_key" {
			t.Errorf("auth header: got %q, want Bearer test_key", got)
		}
		if r.URL.Path != "/instances" {
			t.Errorf("path: got %q, want /instances", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id":             "gpu-abc",
				"status":         "running",
				"gpu_type":       "h100_sxm",
				"gpu_count":      1,
				"tier":           "on_demand",
				"region":         "us-east",
				"price_per_hour": 2.5,
				"created_at":     "2026-05-14T00:00:00Z",
			}},
			"next_cursor": nil,
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "test_key", nil)
	page, err := c.ListInstances(context.Background(), ListInstancesOpts{Limit: 10})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].ID != "gpu-abc" {
		t.Errorf("unexpected page: %+v", page)
	}
}

func TestClient_GetInstance_ProblemError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("X-Request-Id", "req-xyz")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":   "https://errors.example.com/not_found",
			"title":  "Not Found",
			"detail": "instance not found",
			"code":   "not_found",
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "test_key", nil)
	_, err := c.GetInstance(context.Background(), "gpu-missing")
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*ProblemError)
	if !ok {
		t.Fatalf("expected *ProblemError, got %T: %v", err, err)
	}
	if pe.Status != 404 {
		t.Errorf("status: got %d, want 404", pe.Status)
	}
	if pe.Code != "not_found" {
		t.Errorf("code: got %q, want not_found", pe.Code)
	}
	if pe.RequestID != "req-xyz" {
		t.Errorf("request_id: got %q, want req-xyz", pe.RequestID)
	}
}

func TestClient_CreateInstance_ReturnsOperationID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Operation-Id", "op-12345")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"operation_id": "op-12345",
			"kind":         "instance.create",
			"state":        "pending",
			"created_at":   "2026-05-14T00:00:00Z",
			"updated_at":   "2026-05-14T00:00:00Z",
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "test_key", nil)
	op, opID, err := c.CreateInstance(context.Background(), CreateInstanceRequest{
		GPUType: "h100_sxm", Tier: "on_demand",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if opID != "op-12345" {
		t.Errorf("op-id header: got %q, want op-12345", opID)
	}
	if op.OperationID != "op-12345" {
		t.Errorf("body operation_id: got %q", op.OperationID)
	}
}

func TestClient_NetworkError(t *testing.T) {
	// Use a known-invalid port (zero) to force connection refused.
	c := New("http://127.0.0.1:1", "test_key", nil)
	_, err := c.GetInstance(context.Background(), "gpu-x")
	if err == nil {
		t.Fatal("expected network error")
	}
	ne, ok := err.(*NetworkError)
	if !ok {
		// Some envs return wrapped ProblemError instead — fallback: check
		// Error() text for networky words.
		if !strings.Contains(err.Error(), "network") &&
			!strings.Contains(err.Error(), "refused") &&
			!strings.Contains(err.Error(), "connection") {
			t.Errorf("expected NetworkError or networky message, got %T: %v", err, err)
		}
		_ = ne
	}
}

func TestClient_ListGPUTypes_UnwrapsEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gpu-types" {
			t.Errorf("path: %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"gpu_type": "h100_sxm", "vram_gb": 80},
			},
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "", nil) // no token for catalog
	types, err := c.ListGPUTypes(context.Background())
	if err != nil {
		t.Fatalf("ListGPUTypes: %v", err)
	}
	if len(types) != 1 || types[0].GPUType != "h100_sxm" {
		t.Errorf("unexpected types: %+v", types)
	}
}

// TestClient_ListPricing_FollowsCursor verifies ListPricing walks next_cursor
// to exhaustion — the pre-fix client fetched only the first page, so `gpu
// pricing` silently truncated the catalog at the server's default page size
// (2026-06-10 API review finding).
func TestClient_ListPricing_FollowsCursor(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.URL.Query().Get("limit"); got != "200" {
			t.Errorf("limit: got %q, want 200", got)
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{
					"gpu_type": "a100_80gb", "gpu_count": 1, "region": "us-east",
					"tier": "on_demand", "price_per_hour": 1.58, "available": 4,
				}},
				"next_cursor": "page2",
			})
		case "page2":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{
					"gpu_type": "h100_sxm", "gpu_count": 8, "region": "us-west",
					"tier": "on_demand", "price_per_hour": 21.5, "available": 2,
				}},
				"next_cursor": nil,
			})
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "test_key", nil)
	rows, err := c.ListPricing(context.Background(), ListPricingOpts{})
	if err != nil {
		t.Fatalf("ListPricing: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls: got %d, want 2 (cursor followed)", calls)
	}
	if len(rows) != 2 || rows[0].GPUType != "a100_80gb" || rows[1].GPUCount != 8 {
		t.Errorf("rows: got %+v, want both pages merged", rows)
	}
}

// TestClient_UserAgentHeader guards the `gpu` CLI's request-time identity
// . The CLI is the ONE first-party client that already
// stamped a distinct, versioned User-Agent before this phase — which is
// precisely why it needed a test: everything downstream (the auth-layer
// surface classifier, the daily rollup, the developer-metrics page) reads
// `gpu/<version> (<os>/<arch>)` off the wire and silently reclassifies CLI
// traffic as 'unknown' if the header ever changes shape or disappears.
// Nothing else in the repo asserts it, so a refactor could drop it and every
// test would still pass while the chart quietly went wrong.
func TestClient_UserAgentHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}, "next_cursor": nil})
	}))
	defer srv.Close()

	// The shape cmd/gpu/main.go injects once the ldflag version is known.
	const want = "gpu/1.2.3 (darwin/arm64)"
	c := New(srv.URL, "test_key", nil).WithUserAgent(want)
	if _, err := c.ListInstances(context.Background(), ListInstancesOpts{Limit: 1}); err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if got != want {
		t.Errorf("User-Agent: got %q, want %q", got, want)
	}

	// A client built without WithUserAgent must still identify itself as the
	// CLI — an unstamped default would land in the 'unknown' bucket.
	got = ""
	if _, err := New(srv.URL, "test_key", nil).ListInstances(context.Background(), ListInstancesOpts{Limit: 1}); err != nil {
		t.Fatalf("ListInstances (default UA): %v", err)
	}
	if !strings.HasPrefix(got, "gpu/") {
		t.Errorf("default User-Agent: got %q, want a gpu/ prefix", got)
	}
}
