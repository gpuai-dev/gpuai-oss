// Phase 64 (D-64-01) tests for the certified:<framework> launch grammar on
// POST /v1/instances:
//
//   - malformed framework suffix         → 400 (charset/shape, format gate)
//   - well-formed but unknown framework  → 422 (manifest roster gate, sync —
//     never a 202 that dies later in the async provision path)
//
// The 422 case uses a framework id that will never exist in the manifest so
// the test stays green after the real pytorch/cuda-devel/jupyter families are
// published and pinned. The published-framework happy path is pinned at the
// engine layer (provision/engine_environment_test.go) where the manifest is
// injectable — the handler's roster gate reads the binary-embedded manifest.
package public_test

import (
	"net/http"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/api/public"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
)

func TestLaunch_FrameworkEnvironmentGate(t *testing.T) {
	newDeps := func() public.PublicDeps {
		opStore := newStubOperationStore()
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-fw", Hostname: "gpu-fw.example.com"}}
		return instanceTestDeps(opStore, newStubInstanceStore(), engine)
	}

	// Shape violations are 400s from the cheap format gate — uppercase, empty,
	// leading dash, and a sub token on a class that takes none. (raw_vm:<os>
	// is VALID grammar since the OS picker — its unknown-id case is the 422
	// below, mirroring certified:<framework>.)
	for _, env := range []string{"certified:PyTorch", "certified:", "certified:-pytorch", "provider_template:x", "raw_vm:Ubuntu", "raw_vm:.ubuntu"} {
		t.Run("format_400_"+env, func(t *testing.T) {
			deps := newDeps()
			body := map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand", "environment": env}
			w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("environment=%q must be 400 from the format gate: got %d; body=%s", env, w.Code, w.Body.String())
			}
		})
	}

	// A well-formed os id the vm_images catalog will never carry → synchronous
	// 422 from the OS catalog gate (mirrors the unknown-framework 422).
	t.Run("unknown_os_422", func(t *testing.T) {
		deps := newDeps()
		body := map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand", "environment": "raw_vm:no-such-os"}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("unknown os id must be a synchronous 422: got %d; body=%s", w.Code, w.Body.String())
		}
	})

	// A well-formed framework id the manifest will never carry → synchronous
	// 422 from the roster gate (h100_sxm has a manifest row, so the gate does
	// not fail open).
	t.Run("unknown_framework_422", func(t *testing.T) {
		deps := newDeps()
		body := map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand", "environment": "certified:no-such-framework"}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("unknown framework must be a synchronous 422: got %d; body=%s", w.Code, w.Body.String())
		}
	})
}
