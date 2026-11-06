package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// The ready callback's startup GPU compute check gate. Running is the billing
// boundary, so an enforce-mode failure must never reach SetInstanceRunning; it
// is recorded for the provisioning poll loop instead. Shadow mode reports and
// still flips, and bootstraps without the check behave exactly as before.

type gpuCheckReadyStore struct {
	*fakeReadyStore
	failures []string
}

func (g *gpuCheckReadyStore) SetInstanceReadinessFailure(_ context.Context, _ string, reason string) (bool, error) {
	g.failures = append(g.failures, reason)
	return true, nil
}

func newGPUCheckStore() *gpuCheckReadyStore {
	return &gpuCheckReadyStore{fakeReadyStore: &fakeReadyStore{inst: &db.Instance{InstanceID: "gpu-1", Status: "booting"}}}
}

func fireGPUCheckReady(t *testing.T, store *gpuCheckReadyStore, body string) int {
	t.Helper()
	s := newReadyHandlerForTest(store)
	req := httptest.NewRequest(http.MethodPost, "/internal/instances/gpu-1/ready", strings.NewReader(body))
	req.SetPathValue("id", "gpu-1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleInstanceReady(rec, req)
	return rec.Code
}

func TestReadyCallback_GPUCheckEnforceFailureNeverFlipsRunning(t *testing.T) {
	store := newGPUCheckStore()
	code := fireGPUCheckReady(t, store, `{"tunnel_up":"false","gpu_check":"fail","gpu_check_mode":"enforce",`+
		`"gpu_check_detail":"host: cuInit returned 999, nvidia-uvm open: Input/output error\"; DROP TABLE","gpus_seen":"1"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (the bootstrap does not retry a refusal)", code)
	}
	if store.setRunningCall != 0 {
		t.Fatalf("SetInstanceRunning called %d times; a failed host must never start billing", store.setRunningCall)
	}
	if len(store.failures) != 1 || !strings.HasPrefix(store.failures[0], "host: cuInit returned 999, nvidia-uvm open: Input/output error") {
		t.Fatalf("readiness failure = %v", store.failures)
	}
	if strings.ContainsAny(store.failures[0], "\"'") {
		t.Errorf("reason was not sanitized: %q", store.failures[0])
	}
	if store.envReport["gpu_check"] != "fail" || store.envReport["gpu_check_mode"] != "enforce" {
		t.Errorf("env report = %v; want the check recorded", store.envReport)
	}
}

// A refusal must win even if the body also claims the tunnel is up.
func TestReadyCallback_GPUCheckEnforceFailureBeatsTunnelUp(t *testing.T) {
	store := newGPUCheckStore()
	fireGPUCheckReady(t, store, `{"tunnel_up":"true","gpu_check":"fail","gpu_check_mode":"enforce","gpu_check_detail":"host: 1 of 8 GPUs visible"}`)
	if store.setRunningCall != 0 || len(store.failures) != 1 {
		t.Fatalf("setRunning=%d failures=%v", store.setRunningCall, store.failures)
	}
}

func TestReadyCallback_GPUCheckShadowFailureStillFlips(t *testing.T) {
	store := newGPUCheckStore()
	fireGPUCheckReady(t, store, `{"tunnel_up":"true","gpu_check":"fail","gpu_check_mode":"shadow","gpu_check_detail":"environment: pytorch is not installed"}`)
	if store.setRunningCall != 1 {
		t.Fatalf("shadow mode must not block: SetInstanceRunning called %d times", store.setRunningCall)
	}
	if len(store.failures) != 0 {
		t.Errorf("shadow mode recorded a readiness failure: %v", store.failures)
	}
	if store.envReport["gpu_check"] != "fail" || store.envReport["gpu_check_detail"] != "environment: pytorch is not installed" {
		t.Errorf("shadow failure not reported: %v", store.envReport)
	}
}

func TestReadyCallback_GPUCheckPassFlips(t *testing.T) {
	store := newGPUCheckStore()
	fireGPUCheckReady(t, store, `{"tunnel_up":"true","gpu_check":"ok","gpu_check_mode":"enforce","gpus_seen":"8"}`)
	if store.setRunningCall != 1 || len(store.failures) != 0 {
		t.Fatalf("setRunning=%d failures=%v", store.setRunningCall, store.failures)
	}
	if store.envReport["gpus_seen"] != "8" {
		t.Errorf("gpus_seen not reported: %v", store.envReport)
	}
}

func TestReadyCallback_NoGPUCheckFieldsUnchanged(t *testing.T) {
	store := newGPUCheckStore()
	fireGPUCheckReady(t, store, `{"tunnel_up":"true"}`)
	if store.setRunningCall != 1 || len(store.failures) != 0 {
		t.Fatalf("legacy bootstrap: setRunning=%d failures=%v", store.setRunningCall, store.failures)
	}
	if _, ok := store.envReport["gpu_check"]; ok {
		t.Errorf("legacy bootstrap grew a gpu_check report: %v", store.envReport)
	}
}

// The shadow-mode check runs after the ready callback (so it adds no latency to
// readiness) and posts its result report-only. That POST is recorded in the
// ready report and must never change status or record a verdict, even if it
// claims the tunnel is up or carries an enforce-mode failure.
func TestReadyCallback_ReportOnlyRecordsWithoutTouchingStatus(t *testing.T) {
	for _, body := range []string{
		`{"tunnel_up":"false","app_ready":"false","driver_version":"580.126.09","gpu_check":"fail","gpu_check_mode":"shadow",` +
			`"gpu_check_detail":"host: cuInit returned 999","gpus_seen":"0","report_only":"true"}`,
		`{"tunnel_up":"true","gpu_check":"fail","gpu_check_mode":"enforce","gpu_check_detail":"host: cuInit returned 999","report_only":"true"}`,
	} {
		store := newGPUCheckStore()
		if code := fireGPUCheckReady(t, store, body); code != http.StatusOK {
			t.Fatalf("status = %d; want 200", code)
		}
		if store.setRunningCall != 0 || len(store.failures) != 0 {
			t.Fatalf("report-only POST changed state: setRunning=%d failures=%v (%s)", store.setRunningCall, store.failures, body)
		}
		if store.envReport["gpu_check"] != "fail" || store.envReport["gpu_check_detail"] != "host: cuInit returned 999" {
			t.Errorf("report-only result not recorded: %v", store.envReport)
		}
	}
}
