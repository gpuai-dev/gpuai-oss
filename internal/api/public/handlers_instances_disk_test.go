// Tests for the disk_gb field on POST /v1/instances:
//
//   - bounds        → 422 validation_failed (floor in Validate, ceiling in the
//     handler where config is reachable)
//   - capability    → 422 disk_unavailable, synchronously, rather than a 202
//     that dies later in the async provision path
//   - threading     → the value reaches the engine as RequestedDiskGB
//
// The load-bearing test in this file is the OMITTED case: a request without
// disk_gb must reach the engine with RequestedDiskGB == nil and must never trip
// the capability gate, even against capacity that cannot configure disk at all.
// That is what keeps every launch on the platform behaving exactly as it did
// before this field existed.
package public_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
	tmplcatalog "github.com/gpuai-dev/gpuai-oss/internal/template/catalog"
)

// waitForProvisionReq polls the stub engine until the handler's async goroutine
// has called Provision, then returns the request it was handed.
func waitForProvisionReq(t *testing.T, engine *stubEngine) provision.ProvisionRequest {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if req, called := engine.lastRequest(); called {
			return req
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("engine.Provision was never called")
	return provision.ProvisionRequest{}
}

// problemCode decodes the RFC 7807 body and returns its stable `code`.
func problemCode(t *testing.T, body string) string {
	t.Helper()
	var p struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&p); err != nil {
		t.Fatalf("decode problem body %q: %v", body, err)
	}
	return p.Code
}

func problemDetail(t *testing.T, body string) string {
	t.Helper()
	var p struct {
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&p); err != nil {
		t.Fatalf("decode problem body %q: %v", body, err)
	}
	return p.Detail
}

// TestLaunch_DiskBounds covers the two field-level bounds. They live in
// different places — the floor is catalog-independent so it sits in Validate(),
// the ceiling needs config so it sits in the handler — but both surface as the
// same 422 validation_failed, because to a caller they are one kind of mistake.
//
// No AvailCache is wired, so the capability gate is skipped entirely: these
// subtests exercise bounds and nothing else.
func TestLaunch_DiskBounds(t *testing.T) {
	// instanceTestDeps leaves LaunchMaxDiskGB unset, so the nil-safe accessor
	// resolves the documented default ceiling (250) rather than 0.
	const wantMax = 250

	tests := []struct {
		name     string
		diskGB   int
		wantCode int
		wantIn   string
	}{
		{name: "below_floor_422", diskGB: tmplcatalog.MinDiskGB - 1, wantCode: http.StatusUnprocessableEntity, wantIn: "at least"},
		// An explicit 0 is a request for 0 GB, not an absent field — it must be
		// rejected, never quietly read as "use the default".
		{name: "explicit_zero_rejected", diskGB: 0, wantCode: http.StatusUnprocessableEntity, wantIn: "at least"},
		{name: "at_floor_accepted", diskGB: tmplcatalog.MinDiskGB, wantCode: http.StatusAccepted},
		{name: "at_ceiling_accepted", diskGB: wantMax, wantCode: http.StatusAccepted},
		{name: "above_ceiling_422", diskGB: wantMax + 1, wantCode: http.StatusUnprocessableEntity, wantIn: "at most"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-disk", Hostname: "gpu-disk.example.com"}}
			deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)

			body := map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand", "disk_gb": tc.diskGB}
			w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
			if w.Code != tc.wantCode {
				t.Fatalf("disk_gb=%d: got %d, want %d; body=%s", tc.diskGB, w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantCode != http.StatusAccepted {
				if code := problemCode(t, w.Body.String()); code != "validation_failed" {
					t.Errorf("disk_gb=%d: problem code = %q, want validation_failed", tc.diskGB, code)
				}
				detail := problemDetail(t, w.Body.String())
				if !strings.Contains(detail, tc.wantIn) {
					t.Errorf("disk_gb=%d: detail %q must say %q so the caller learns the bound", tc.diskGB, detail, tc.wantIn)
				}
			}
		})
	}
}

// TestLaunch_DiskThreadedToEngine proves the field reaches the engine, and — the
// half that matters — that omitting it leaves RequestedDiskGB nil.
//
// nil is not a cosmetic difference: the engine treats a nil request as "fill the
// platform default and narrow nothing", and any non-nil value as a hard
// requirement that narrows placement. If the handler ever defaulted the field
// here instead of passing nil through, every launch on the platform would narrow
// to disk-configurable capacity and most of the fleet would stop being reachable.
func TestLaunch_DiskThreadedToEngine(t *testing.T) {
	t.Run("explicit_value_threaded", func(t *testing.T) {
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-d1", Hostname: "gpu-d1.example.com"}}
		deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)

		body := map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand", "disk_gb": 200}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusAccepted {
			t.Fatalf("valid disk_gb must be accepted: got %d; body=%s", w.Code, w.Body.String())
		}

		req := waitForProvisionReq(t, engine)
		if req.RequestedDiskGB == nil {
			t.Fatal("RequestedDiskGB = nil, want 200 — disk_gb was dropped between the handler and the engine")
		}
		if *req.RequestedDiskGB != 200 {
			t.Errorf("RequestedDiskGB = %d, want 200", *req.RequestedDiskGB)
		}
	})

	t.Run("omitted_stays_nil", func(t *testing.T) {
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-d2", Hostname: "gpu-d2.example.com"}}
		deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)

		body := map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand"}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusAccepted {
			t.Fatalf("omitted disk_gb must be accepted: got %d; body=%s", w.Code, w.Body.String())
		}

		req := waitForProvisionReq(t, engine)
		if req.RequestedDiskGB != nil {
			t.Errorf("RequestedDiskGB = %d, want nil. A defaulted value here turns every launch into an "+
				"explicit disk request, narrowing placement to disk-configurable capacity and stranding the "+
				"rest of the fleet.", *req.RequestedDiskGB)
		}
	})
}

// TestLaunch_DiskCapabilityGate covers the synchronous 422. It reads
// disk_configurable off the availability catalog, so it is Redis-gated like the
// other catalog-dependent handler tests.
func TestLaunch_DiskCapabilityGate(t *testing.T) {
	// Capacity that cannot configure disk at all — a full-VM upstream.
	fixedDisk := []availability.AvailableOffering{
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.5, Region: "us-west", Tier: "on_demand",
			AvailableCount: 8, DeploymentType: "vm", InstanceDiskGB: 750, DiskConfigurable: false},
	}

	t.Run("no_capable_offering_422", func(t *testing.T) {
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-d3", Hostname: "gpu-d3.example.com"}}
		deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)
		deps.AvailCache = instanceTestAvailCache(t, fixedDisk)

		body := map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand", "disk_gb": 200}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("disk_gb against capacity that cannot honor it must be a synchronous 422: got %d; body=%s",
				w.Code, w.Body.String())
		}
		if code := problemCode(t, w.Body.String()); code != "disk_unavailable" {
			t.Errorf("problem code = %q, want disk_unavailable", code)
		}
		// Same sentence as the async failure, sourced from the same var so the
		// two paths cannot drift into two explanations of one condition.
		if detail := problemDetail(t, w.Body.String()); detail != provision.ErrDiskUnavailable.Error() {
			t.Errorf("detail = %q, want the ErrDiskUnavailable text %q", detail, provision.ErrDiskUnavailable.Error())
		}
		if _, called := engine.lastRequest(); called {
			t.Error("engine.Provision was called despite a 422 — a rejected request must leave no provisioning work behind")
		}
	})

	t.Run("one_capable_offering_accepted", func(t *testing.T) {
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-d4", Hostname: "gpu-d4.example.com"}}
		deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)
		// Same model, mixed capacity: the gate rejects only when NOTHING can
		// honor the request — one capable offering is enough, and the engine
		// narrows to it.
		deps.AvailCache = instanceTestAvailCache(t, append(append([]availability.AvailableOffering{}, fixedDisk...),
			availability.AvailableOffering{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.9, Region: "us-east",
				Tier: "on_demand", AvailableCount: 3, DeploymentType: "container", InstanceDiskGB: 100, DiskConfigurable: true},
		))

		body := map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand", "disk_gb": 200}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusAccepted {
			t.Fatalf("one capable offering must be enough to pass the gate: got %d; body=%s", w.Code, w.Body.String())
		}
	})

	// THE regression: the gate must be invisible to every launch that does not
	// ask for a disk, even against capacity that could never honor one. Without
	// the req.DiskGB != nil guard this catalog would 422 the whole platform.
	t.Run("omitted_disk_never_gated", func(t *testing.T) {
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-d5", Hostname: "gpu-d5.example.com"}}
		deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)
		deps.AvailCache = instanceTestAvailCache(t, fixedDisk)

		body := map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand"}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusAccepted {
			t.Fatalf("a launch that never asked for a disk must be untouched by the disk gate: got %d; body=%s",
				w.Code, w.Body.String())
		}
	})

	// Fails OPEN: no catalog to consult means no early reject. The engine
	// re-derives configurability and is the trust boundary; this gate is
	// early-reject UX only, so a catalog outage must not block launches.
	t.Run("no_catalog_fails_open", func(t *testing.T) {
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-d6", Hostname: "gpu-d6.example.com"}}
		deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)
		// deps.AvailCache stays nil.

		body := map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand", "disk_gb": 200}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusAccepted {
			t.Fatalf("a catalog outage must not block a disk request: got %d; body=%s", w.Code, w.Body.String())
		}
	})
}

// TestLaunch_DiskExceedsPinnedOffering covers the synchronous 422 for the marketplace
// accepted-but-clamped defect (2026-08-11 prod battery): a launch PINNED to an
// offering whose reported storage_gb cannot fit the explicit disk_gb must be
// refused up front — not accepted, billed at the requested size, and silently
// clamped by the upstream host. The engine re-derives the same cap and is the
// trust boundary (this gate is early-reject UX, like its neighbours), so every
// uncertain case here fails OPEN.
func TestLaunch_DiskExceedsPinnedOffering(t *testing.T) {
	rows := []availability.AvailableOffering{
		// The pinned target: disk-configurable, 144GB host storage.
		{OfferingID: "off-disk-cap", GPUModel: "rtx_3080", GPUCount: 1, VRAMGB: 10, PricePerHour: 0.07,
			Region: "us-east", Tier: "on_demand", AvailableCount: 3, DeploymentType: "container",
			InstanceDiskGB: 100, DiskConfigurable: true, StorageGB: 144},
		// A bigger sibling, so the model-wide capability gate stays satisfied and
		// only the PINNED row's cap decides the outcome.
		{OfferingID: "off-disk-big", GPUModel: "rtx_3080", GPUCount: 1, VRAMGB: 10, PricePerHour: 0.20,
			Region: "us-west", Tier: "on_demand", AvailableCount: 2, DeploymentType: "container",
			InstanceDiskGB: 100, DiskConfigurable: true, StorageGB: 500},
		// A row that reports no storage figure at all.
		{OfferingID: "off-disk-unknown", GPUModel: "rtx_3080", GPUCount: 1, VRAMGB: 10, PricePerHour: 0.30,
			Region: "eu-west", Tier: "on_demand", AvailableCount: 2, DeploymentType: "container",
			InstanceDiskGB: 100, DiskConfigurable: true},
		// A fixed-disk row that DOES report storage: the pinned cap gate must
		// not fire here — "lower disk_gb" is wrong advice on a machine where no
		// disk_gb works (that condition belongs to the capability gate/engine).
		{OfferingID: "off-disk-fixed", GPUModel: "rtx_3080", GPUCount: 1, VRAMGB: 10, PricePerHour: 0.40,
			Region: "ca-central", Tier: "on_demand", AvailableCount: 2, DeploymentType: "vm",
			InstanceDiskGB: 144, DiskConfigurable: false, StorageGB: 144},
	}

	launch := func(t *testing.T, offeringID string, diskGB int) (*stubEngine, string, int) {
		t.Helper()
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-d7", Hostname: "gpu-d7.example.com"}}
		deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)
		deps.AvailCache = instanceTestAvailCache(t, rows)
		body := map[string]any{"gpu_type": "rtx_3080", "gpu_count": 1, "tier": "on_demand",
			"disk_gb": diskGB, "offering_id": offeringID}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		return engine, w.Body.String(), w.Code
	}

	t.Run("over_the_pinned_cap_422", func(t *testing.T) {
		engine, respBody, code := launch(t, "off-disk-cap", 200)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("disk_gb=200 pinned to a 144GB offering must be a synchronous 422: got %d; body=%s", code, respBody)
		}
		if got := problemCode(t, respBody); got != "disk_exceeds_offering" {
			t.Errorf("problem code = %q, want disk_exceeds_offering", got)
		}
		// Same sentence as the async engine failure, sourced from the same var.
		if detail := problemDetail(t, respBody); detail != provision.ErrDiskExceedsOffering.Error() {
			t.Errorf("detail = %q, want the ErrDiskExceedsOffering text %q", detail, provision.ErrDiskExceedsOffering.Error())
		}
		if _, called := engine.lastRequest(); called {
			t.Error("engine.Provision was called despite the 422")
		}
	})

	t.Run("at_the_pinned_cap_accepted", func(t *testing.T) {
		_, respBody, code := launch(t, "off-disk-cap", 144)
		if code != http.StatusAccepted {
			t.Fatalf("disk_gb at the offering's cap is honorable: got %d; body=%s", code, respBody)
		}
	})

	t.Run("unreported_storage_fails_open", func(t *testing.T) {
		_, respBody, code := launch(t, "off-disk-unknown", 200)
		if code != http.StatusAccepted {
			t.Fatalf("a row with no storage figure must not reject (StorageGB=0 is \"unknown\", not \"none\"): got %d; body=%s", code, respBody)
		}
	})

	t.Run("fixed_disk_pin_not_gated_here", func(t *testing.T) {
		// A non-configurable pinned row over its storage figure must NOT draw
		// disk_exceeds_offering ("lower disk_gb" is unactionable there). With a
		// configurable sibling in the catalog the model-wide gate passes too,
		// so the request flows through to the engine, which owns that refusal.
		engine, respBody, code := launch(t, "off-disk-fixed", 200)
		if code != http.StatusAccepted {
			t.Fatalf("fixed-disk pinned row must not trip the pinned cap gate: got %d; body=%s", code, respBody)
		}
		waitForProvisionReq(t, engine)
	})

	t.Run("pin_not_in_catalog_fails_open", func(t *testing.T) {
		// The pinned id can be one poll cycle newer than the cache. Whether the
		// pin still exists is the engine's call (ErrOfferingUnavailable), not
		// this gate's.
		_, respBody, code := launch(t, "off-disk-gone", 200)
		if code != http.StatusAccepted {
			t.Fatalf("an unknown pinned id must fall through to the engine: got %d; body=%s", code, respBody)
		}
	})

	t.Run("unpinned_stays_ungated", func(t *testing.T) {
		// No offering_id: the model-wide capability gate governs (a 500GB row
		// exists, so 200 is honorable somewhere and placement narrows to it).
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-d8", Hostname: "gpu-d8.example.com"}}
		deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)
		deps.AvailCache = instanceTestAvailCache(t, rows)
		body := map[string]any{"gpu_type": "rtx_3080", "gpu_count": 1, "tier": "on_demand", "disk_gb": 200}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusAccepted {
			t.Fatalf("an unpinned oversize request narrows placement instead of failing here: got %d; body=%s", w.Code, w.Body.String())
		}
	})
}
