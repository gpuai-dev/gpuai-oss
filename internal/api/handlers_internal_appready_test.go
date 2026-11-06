// Wave 2 production test (Plan 31-08, TMPL-10 app_ready gate). The Wave 0
// build tag was dropped by Plan 08, which landed the app_ready gate on POST
// /internal/instances/{id}/ready and its in-memory DB seam (readyCallbackStore
// + newReadyHandlerForTest).
//
// The gates: for a kind=service (template) instance, the ready callback only
// flips booting→running when the bootstrap reports {"app_ready":"true"}. A
// "false" body is an idempotent retry (200, no transition). ALL instances
// additionally defer on an explicit {"tunnel_up":"false"} (RAWREADY — running
// must imply the FRP tunnel is registered); an absent tunnel_up stays
// permissive for legacy bootstraps.
//
// References (Wave 2 production symbols this file pins, by the names Plan
// 06/08 commit to):
//   - db.Instance.TemplateID *string                      — set for template deploys (Plan 06)
//   - api.readyCallbackStore interface                     — the ready-handler DB seam (Plan 08)
//   - api.newReadyHandlerForTest(readyCallbackStore) *Server — test constructor (Plan 08)

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

// fakeReadyStore satisfies the ready-handler DB seam Plan 08 declares. It
// records SetInstanceRunning invocations so the gate can be asserted without a
// live Postgres.
type fakeReadyStore struct {
	mu             sync.Mutex
	inst           *db.Instance
	setRunningCall int
	// envReport captures the Phase 63 bootstrap environment report
	// (driver_version, cuda_version, certified_image_digest, docker_up) that
	// the ready callback is expected to persist. RecordReadyReport is the seam
	// Wave 2 wires the handler to; until then this stays nil so
	// TestReadyCallback_EnvReport runs RED (values never land).
	envReport map[string]string
}

// RecordReadyReport is the Phase 63 env-report seam. The production ready
// handler will call it to persist the certified-image / driver / cuda / docker
// telemetry the bootstrap posts alongside the tunnel_up/app_ready signals. It
// is intentionally NOT part of the readyCallbackStore interface yet — Wave 2
// widens the seam; for now the method exists only so the RED test can pin the
// expected shape.
func (f *fakeReadyStore) RecordReadyReport(_ context.Context, _ string, report map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.envReport == nil {
		f.envReport = map[string]string{}
	}
	for k, v := range report {
		f.envReport[k] = v
	}
}

func (f *fakeReadyStore) GetInstance(_ context.Context, _ string) (*db.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *f.inst
	return &cp, nil
}

func (f *fakeReadyStore) SetInstanceRunning(_ context.Context, _ string, _ string) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setRunningCall++
	return true, "booting", nil
}

func ptr(s string) *string { return &s }

// fireReady invokes the ready handler with the given instance + JSON body and
// returns the recorder. newReadyHandlerForTest builds a minimal *Server whose
// ready handler reads from the fake store (Plan 08 entrypoint).
func fireReady(t *testing.T, store *fakeReadyStore, instID, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := newReadyHandlerForTest(store)
	req := httptest.NewRequest(http.MethodPost, "/internal/instances/"+instID+"/ready", strings.NewReader(body))
	req.SetPathValue("id", instID)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.handleInstanceReady(w, req)
	return w
}

// TestInstanceReadyAppGate_ServiceFalse asserts a service instance with
// app_ready=false does NOT transition but still returns 200 (idempotent retry).
func TestInstanceReadyAppGate_ServiceFalse(t *testing.T) {
	store := &fakeReadyStore{inst: &db.Instance{
		InstanceID: "gpu-svc1",
		Status:     "booting",
		TemplateID: ptr("comfyui"),
	}}
	w := fireReady(t, store, "gpu-svc1", `{"app_ready":"false"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (idempotent); body=%s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setRunningCall != 0 {
		t.Errorf("SetInstanceRunning called %d times for app_ready=false; want 0", store.setRunningCall)
	}
}

// TestInstanceReadyAppGate_ServiceTrue asserts a service instance with
// app_ready=true transitions to running.
func TestInstanceReadyAppGate_ServiceTrue(t *testing.T) {
	store := &fakeReadyStore{inst: &db.Instance{
		InstanceID: "gpu-svc2",
		Status:     "booting",
		TemplateID: ptr("comfyui"),
	}}
	w := fireReady(t, store, "gpu-svc2", `{"app_ready":"true"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setRunningCall != 1 {
		t.Errorf("SetInstanceRunning called %d times for app_ready=true; want 1", store.setRunningCall)
	}
}

// TestInstanceReadyAppGate_RawInstance asserts a raw instance (template_id
// NULL) with an EMPTY body still transitions — a legacy bootstrap that omits
// tunnel_up/app_ready entirely must keep flipping on callback rather than
// stranding the instance in booting (the tunnel gate defers only on an
// explicit "false").
func TestInstanceReadyAppGate_RawInstance(t *testing.T) {
	store := &fakeReadyStore{inst: &db.Instance{
		InstanceID: "gpu-raw1",
		Status:     "booting",
		TemplateID: nil,
	}}
	w := fireReady(t, store, "gpu-raw1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setRunningCall != 1 {
		t.Errorf("SetInstanceRunning called %d times for raw instance with empty body; want 1 (legacy-permissive)", store.setRunningCall)
	}
}

// TestInstanceReadyTunnelGate_False asserts the RAWREADY gate: a raw instance
// whose bootstrap reports tunnel_up=false (the gateway SSH port probe failed)
// does NOT transition but returns 200, so the bootstrap's background re-probe
// can re-POST once the tunnel registers.
func TestInstanceReadyTunnelGate_False(t *testing.T) {
	store := &fakeReadyStore{inst: &db.Instance{
		InstanceID: "gpu-raw2",
		Status:     "booting",
		TemplateID: nil,
	}}
	w := fireReady(t, store, "gpu-raw2", `{"tunnel_up":"false","app_ready":"true"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (idempotent); body=%s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setRunningCall != 0 {
		t.Errorf("SetInstanceRunning called %d times for tunnel_up=false; want 0 (running must imply SSH reachability)", store.setRunningCall)
	}
}

// TestInstanceReadyTunnelGate_True asserts the verified-tunnel path: a raw
// instance with tunnel_up=true transitions to running.
func TestInstanceReadyTunnelGate_True(t *testing.T) {
	store := &fakeReadyStore{inst: &db.Instance{
		InstanceID: "gpu-raw3",
		Status:     "booting",
		TemplateID: nil,
	}}
	w := fireReady(t, store, "gpu-raw3", `{"tunnel_up":"true","app_ready":"true"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setRunningCall != 1 {
		t.Errorf("SetInstanceRunning called %d times for tunnel_up=true; want 1", store.setRunningCall)
	}
}

// TestReadyCallback_EnvReport (Phase 63, D-63-07) asserts the ready callback
// accepts and persists the bootstrap's environment report: the certified-image
// bootstrap posts driver_version, cuda_version, certified_image_digest and
// docker_up alongside the existing tunnel_up/app_ready signals, and those values
// must land on the instance ready record (asserted here via the readyCallbackStore
// double). RED until Wave 2 wires handleInstanceReady to record the report — the
// current handler ignores the extra fields, so store.envReport stays empty.
func TestReadyCallback_EnvReport(t *testing.T) {
	store := &fakeReadyStore{inst: &db.Instance{
		InstanceID: "gpu-env1",
		Status:     "booting",
		TemplateID: nil,
	}}
	body := `{"tunnel_up":"true","app_ready":"true",` +
		`"driver_version":"570.86.10","cuda_version":"12.8",` +
		`"certified_image_digest":"sha256:0123456789abcdef","docker_up":"true"}`
	w := fireReady(t, store, "gpu-env1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (env report must be accepted); body=%s", w.Code, w.Body.String())
	}
	want := map[string]string{
		"driver_version":         "570.86.10",
		"cuda_version":           "12.8",
		"certified_image_digest": "sha256:0123456789abcdef",
		"docker_up":              "true",
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for k, v := range want {
		if store.envReport[k] != v {
			t.Errorf("env report %q: got %q, want %q (handler must record the bootstrap env report)", k, store.envReport[k], v)
		}
	}
}

// TestInstanceReadyTunnelGate_TemplateAlsoGated asserts the tunnel gate is
// universal: a service template with app_ready=true but tunnel_up=false stays
// booting — its deploy URL AND SSH both ride the tunnel, so neither signal
// alone may flip it.
func TestInstanceReadyTunnelGate_TemplateAlsoGated(t *testing.T) {
	store := &fakeReadyStore{inst: &db.Instance{
		InstanceID: "gpu-svc3",
		Status:     "booting",
		TemplateID: ptr("comfyui"),
	}}
	w := fireReady(t, store, "gpu-svc3", `{"tunnel_up":"false","app_ready":"true"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (idempotent); body=%s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setRunningCall != 0 {
		t.Errorf("SetInstanceRunning called %d times for template with tunnel_up=false; want 0", store.setRunningCall)
	}
}
