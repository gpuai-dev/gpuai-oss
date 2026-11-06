// Empty-argv-element validation + config-entrypoint threading (pre-prod fixes
// from the 2026-09-02 live matrix sweep). Two behaviors pinned here:
//
//  1. `entrypoint: [""]` / `cmd: [""]` — the docker-CLI-style clear spelling —
//     must 422 instead of rendering `exec ”` and killing the workload while
//     the instance reports running (live-confirmed failure mode).
//  2. ImageConfigEntrypoint threads the pre-flight-captured CONFIG entrypoint
//     to the engine UNCONDITIONALLY — even when the request clears/overrides
//     the entrypoint — because the ENTRYPOINT placement steer keys on what
//     the container runtime will apply, not on what our launch script execs.
package public_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/provision"
)

func TestHandleCreateInstance_EmptyArgvElementRejected(t *testing.T) {
	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-ee1", Hostname: "gpu-ee1.example.com"}}
	srv, _ := imageTestRegistry(t)
	deps := imageTestDeps(t, engine, srv)

	for name, extra := range map[string]map[string]any{
		"entrypoint clear spelling": {"entrypoint": []string{""}},
		"entrypoint embedded empty": {"entrypoint": []string{"python", ""}},
		"cmd clear spelling":        {"cmd": []string{""}},
	} {
		body := imageBody(extra)
		body["image"] = "reg.example.com/acme/good:v1"
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: got %d, want 422; body=%s", name, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "empty") {
			t.Errorf("%s: error should name the empty element; body=%s", name, w.Body.String())
		}
	}
}

func TestHandleCreateInstance_ConfigEntrypointThreadsThroughClear(t *testing.T) {
	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-ee2", Hostname: "gpu-ee2.example.com"}}
	srv, _ := imageTestRegistry(t)
	deps := imageTestDeps(t, engine, srv)

	// The documented clear idiom (explicit empty array) stays accepted, and
	// the CONFIG entrypoint still reaches the engine for the placement steer.
	w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1",
		imageBody(map[string]any{
			"image":      "reg.example.com/acme/good:v1",
			"entrypoint": []string{},
			"cmd":        []string{"sleep", "infinity"},
		}))
	if w.Code != http.StatusAccepted {
		t.Fatalf("entrypoint clear: got %d; body=%s", w.Code, w.Body.String())
	}
	var got provision.ProvisionRequest
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if req, ok := engine.lastRequest(); ok {
			got = req
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(got.ImageEntrypoint) != 0 {
		t.Errorf("cleared entrypoint should thread empty, got %v", got.ImageEntrypoint)
	}
	if len(got.ImageCmd) != 2 || got.ImageCmd[0] != "sleep" {
		t.Errorf("cmd override: got %v", got.ImageCmd)
	}
	// acme/good's manifest config carries Entrypoint ["/serve.sh"] — the
	// clear must not scrub it from the placement-steer field.
	if len(got.ImageConfigEntrypoint) != 1 || got.ImageConfigEntrypoint[0] != "/serve.sh" {
		t.Errorf("ImageConfigEntrypoint: got %v, want [/serve.sh]", got.ImageConfigEntrypoint)
	}
}

// Review-790 regression: clearing the entrypoint with no cmd override must
// PRESERVE the image CMD (docker `--entrypoint ""` semantics). Dropping it
// rendered an empty launch script — SSH-only instance, workload silently
// never started.
func TestHandleCreateInstance_EntrypointClearPreservesImageCmd(t *testing.T) {
	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-ee3", Hostname: "gpu-ee3.example.com"}}
	srv, _ := imageTestRegistry(t)
	deps := imageTestDeps(t, engine, srv)

	w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1",
		imageBody(map[string]any{
			"image":      "reg.example.com/acme/good:v1",
			"entrypoint": []string{},
		}))
	if w.Code != http.StatusAccepted {
		t.Fatalf("entrypoint clear sans cmd: got %d; body=%s", w.Code, w.Body.String())
	}
	var got provision.ProvisionRequest
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if req, ok := engine.lastRequest(); ok {
			got = req
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(got.ImageEntrypoint) != 0 {
		t.Errorf("cleared entrypoint should thread empty, got %v", got.ImageEntrypoint)
	}
	// acme/good's config CMD is ["--port","8000"] — it must survive the clear.
	if len(got.ImageCmd) != 2 || got.ImageCmd[0] != "--port" || got.ImageCmd[1] != "8000" {
		t.Errorf("image CMD must survive an entrypoint clear, got %v", got.ImageCmd)
	}
}
