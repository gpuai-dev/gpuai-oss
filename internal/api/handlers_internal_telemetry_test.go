// TELEM-01 (Plan 49-01) telemetry ingest test. Mirrors
// handlers_internal_appready_test.go: a fake telemetryStore lets the ingest be
// asserted without a live Postgres (DB tests skip locally). The per-instance
// token auth (InstanceTokenAuth) is exercised in the middleware tests; this
// file asserts the handler stores exactly one parsed sample on the happy path
// and 400s on a missing id.

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// fakeTelemetryStore satisfies the telemetry ingest seam, capturing every
// inserted sample so the handler's parse+store path can be asserted.
type fakeTelemetryStore struct {
	mu      sync.Mutex
	inserts []db.GPUTelemetrySample
	instIDs []string
}

func (f *fakeTelemetryStore) InsertGPUTelemetry(_ context.Context, instanceID string, s db.GPUTelemetrySample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instIDs = append(f.instIDs, instanceID)
	f.inserts = append(f.inserts, s)
	return nil
}

// fireTelemetry invokes the telemetry handler with the given instance id +
// JSON body and returns the recorder.
func fireTelemetry(t *testing.T, store *fakeTelemetryStore, instID, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := newTelemetryHandlerForTest(store)
	req := httptest.NewRequest(http.MethodPost, "/internal/instances/"+instID+"/telemetry", strings.NewReader(body))
	req.SetPathValue("id", instID)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.handleInstanceTelemetry(w, req)
	return w
}

// TestInstanceTelemetry_HappyPath asserts a valid sample returns 204 and stores
// exactly one parsed row keyed on the path instance id.
func TestInstanceTelemetry_HappyPath(t *testing.T) {
	store := &fakeTelemetryStore{}
	w := fireTelemetry(t, store, "gpu-tel1", `{"sample":"98, 1024, 81920, 250"}`)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204; body=%s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.inserts) != 1 {
		t.Fatalf("InsertGPUTelemetry called %d times; want 1", len(store.inserts))
	}
	if store.instIDs[0] != "gpu-tel1" {
		t.Errorf("instance id: got %q, want gpu-tel1", store.instIDs[0])
	}
	s := store.inserts[0]
	if s.SMUtil == nil || *s.SMUtil != 98 {
		t.Errorf("SMUtil: got %v, want 98", s.SMUtil)
	}
	if s.VRAMUsedMB == nil || *s.VRAMUsedMB != 1024 {
		t.Errorf("VRAMUsedMB: got %v, want 1024", s.VRAMUsedMB)
	}
	if s.VRAMTotalMB == nil || *s.VRAMTotalMB != 81920 {
		t.Errorf("VRAMTotalMB: got %v, want 81920", s.VRAMTotalMB)
	}
	if s.PowerW == nil || *s.PowerW != 250 {
		t.Errorf("PowerW: got %v, want 250", s.PowerW)
	}
}

// TestInstanceTelemetry_MissingID asserts an empty path id returns 400 and
// stores nothing.
func TestInstanceTelemetry_MissingID(t *testing.T) {
	store := &fakeTelemetryStore{}
	w := fireTelemetry(t, store, "", `{"sample":"98, 1024, 81920, 250"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.inserts) != 0 {
		t.Errorf("InsertGPUTelemetry called %d times for missing id; want 0", len(store.inserts))
	}
}

// TestInstanceTelemetry_MalformedBody asserts a malformed JSON body does not
// 500 — the tolerant decode stores an all-NULL row (mirrors readyCallback).
func TestInstanceTelemetry_MalformedBody(t *testing.T) {
	store := &fakeTelemetryStore{}
	w := fireTelemetry(t, store, "gpu-tel2", `{not json`)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204 (tolerant); body=%s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.inserts) != 1 {
		t.Fatalf("InsertGPUTelemetry called %d times; want 1", len(store.inserts))
	}
	s := store.inserts[0]
	if s.SMUtil != nil || s.VRAMUsedMB != nil || s.VRAMTotalMB != nil || s.PowerW != nil {
		t.Errorf("malformed body should store all-NULL sample, got %+v", s)
	}
}
