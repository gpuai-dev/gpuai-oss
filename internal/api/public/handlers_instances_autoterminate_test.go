// Tests for auto_terminate_hours on POST /v1/instances (GPU-60):
//
//   - bounds    → 422 validation_failed for 0, negative and over-ceiling values
//   - threading → the value reaches the engine as ProvisionRequest.AutoTerminateHours
//   - read      → the instance response echoes the bound and its resolved deadline
//
// The load-bearing cases are the OMITTED one (no bound must reach the engine as
// nil, so every existing launch is unchanged) and the EXPLICIT ZERO one (a
// client that computed its way to 0 must be refused, not silently granted an
// unbounded instance — which is the exact failure this feature exists to stop).
package public_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
)

func TestLaunch_AutoTerminateHoursBounds(t *testing.T) {
	// Mirrors the handler's ceiling (30 days). Duplicated as a literal on
	// purpose: if someone widens the constant, this test should make them say
	// so out loud rather than silently agreeing.
	const wantMax = 24 * 30

	tests := []struct {
		name     string
		hours    int
		wantCode int
		wantIn   string
	}{
		// Absent is the only way to say "no limit"; 0 is a broken request.
		{name: "explicit_zero_rejected", hours: 0, wantCode: http.StatusUnprocessableEntity, wantIn: "at least 1"},
		{name: "negative_rejected", hours: -4, wantCode: http.StatusUnprocessableEntity, wantIn: "at least 1"},
		{name: "one_hour_accepted", hours: 1, wantCode: http.StatusAccepted},
		{name: "at_ceiling_accepted", hours: wantMax, wantCode: http.StatusAccepted},
		{name: "above_ceiling_rejected", hours: wantMax + 1, wantCode: http.StatusUnprocessableEntity, wantIn: "at most"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-at01", Hostname: "gpu-at01.example.com"}}
			deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)

			body := map[string]any{
				"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand",
				"auto_terminate_hours": tc.hours,
			}
			w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
			if w.Code != tc.wantCode {
				t.Fatalf("auto_terminate_hours=%d: got %d, want %d; body=%s", tc.hours, w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantCode == http.StatusAccepted {
				return
			}
			if code := problemCode(t, w.Body.String()); code != "validation_failed" {
				t.Errorf("auto_terminate_hours=%d: problem code = %q, want validation_failed", tc.hours, code)
			}
			if detail := problemDetail(t, w.Body.String()); !strings.Contains(detail, tc.wantIn) {
				t.Errorf("auto_terminate_hours=%d: detail %q must say %q so the caller learns the bound",
					tc.hours, detail, tc.wantIn)
			}
		})
	}
}

// TestLaunch_AutoTerminateThreadedToEngine proves the field survives the trip to
// the engine, and — the half that matters — that omitting it leaves the engine
// request nil. A defaulted value here would put every launch on the platform
// under a runtime bound nobody asked for.
func TestLaunch_AutoTerminateThreadedToEngine(t *testing.T) {
	t.Run("explicit_value_threaded", func(t *testing.T) {
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-at02", Hostname: "gpu-at02.example.com"}}
		deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)

		body := map[string]any{
			"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand",
			"auto_terminate_hours": 8,
		}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusAccepted {
			t.Fatalf("valid auto_terminate_hours must be accepted: got %d; body=%s", w.Code, w.Body.String())
		}
		req := waitForProvisionReq(t, engine)
		if req.AutoTerminateHours == nil {
			t.Fatal("AutoTerminateHours = nil, want 8 — the bound was dropped between the handler and the engine")
		}
		if *req.AutoTerminateHours != 8 {
			t.Errorf("AutoTerminateHours = %d, want 8", *req.AutoTerminateHours)
		}
	})

	t.Run("omitted_stays_nil", func(t *testing.T) {
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-at03", Hostname: "gpu-at03.example.com"}}
		deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)

		body := map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand"}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusAccepted {
			t.Fatalf("omitted auto_terminate_hours must be accepted: got %d; body=%s", w.Code, w.Body.String())
		}
		req := waitForProvisionReq(t, engine)
		if req.AutoTerminateHours != nil {
			t.Errorf("AutoTerminateHours = %d, want nil — an omitted field must never become a runtime bound",
				*req.AutoTerminateHours)
		}
	})
}

// TestGetInstance_SurfacesAutoTerminate proves a customer can see the bound they
// set and, more usefully, WHEN it fires — a limit you cannot read back is a
// limit you cannot trust. The unbounded instance must omit both fields rather
// than render nulls.
func TestGetInstance_SurfacesAutoTerminate(t *testing.T) {
	hours := 6
	deadline := time.Now().UTC().Add(6 * time.Hour).Truncate(time.Second)

	store := newStubInstanceStore()
	store.instances["gpu-bound01"] = &db.Instance{
		InstanceID: "gpu-bound01", OrgID: "org-A", Status: "running",
		GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand", Region: "us-west",
		PricePerHour:       2.5,
		AutoTerminateHours: &hours,
		AutoTerminateAt:    &deadline,
	}
	store.instances["gpu-free001"] = &db.Instance{
		InstanceID: "gpu-free001", OrgID: "org-A", Status: "running",
		GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand", Region: "us-west",
		PricePerHour: 2.5,
	}
	deps := instanceTestDeps(newStubOperationStore(), store, &stubEngine{})

	t.Run("bounded_instance_reports_limit_and_deadline", func(t *testing.T) {
		w := invokeInstance(t, deps, http.MethodGet, "/v1/instances/gpu-bound01", "org-A", "user-A", "ak-1", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		var got struct {
			AutoTerminateHours *int       `json:"auto_terminate_hours"`
			AutoTerminateAt    *time.Time `json:"auto_terminate_at"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.AutoTerminateHours == nil || *got.AutoTerminateHours != hours {
			t.Errorf("auto_terminate_hours = %v, want %d", got.AutoTerminateHours, hours)
		}
		if got.AutoTerminateAt == nil || !got.AutoTerminateAt.Equal(deadline) {
			t.Errorf("auto_terminate_at = %v, want %v", got.AutoTerminateAt, deadline)
		}
	})

	t.Run("unbounded_instance_omits_both", func(t *testing.T) {
		w := invokeInstance(t, deps, http.MethodGet, "/v1/instances/gpu-free001", "org-A", "user-A", "ak-1", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		for _, field := range []string{"auto_terminate_hours", "auto_terminate_at"} {
			if strings.Contains(body, field) {
				t.Errorf("unbounded instance response carries %q; omitempty must drop it: %s", field, body)
			}
		}
	})
}
