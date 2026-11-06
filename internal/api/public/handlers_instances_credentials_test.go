// GPU-59: the instance web-console basic-auth password must be REDACTED by
// default from every public read surface. The only way to obtain it is the
// explicit opt-in `GET /v1/instances/{id}?include=credentials` — the list
// endpoint never honors the param, and invalid values are a 400
// validation_failed (mirroring the ?status filter convention).
//
// These are never-again pins in the operations_credential_surface_test.go
// style: they assert on the literal wire substrings ("app_password", the
// seeded password value) so a future handler re-adding the field fails loudly.

package public_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// seedCredentialInstance seeds the stub store with a running template
// instance that carries the persisted v74 basic-auth credential and every
// field required for instanceToPublic to build the full connection block
// (FRP port + template + terminal port).
func seedCredentialInstance(instStore *stubInstanceStore, instID, user, pass string) {
	frpPort := 10005
	termPort := 7681
	tmpl := "comfyui"
	instStore.put(&db.Instance{
		InstanceID:    instID,
		OrgID:         "org-A",
		Status:        "running",
		GPUType:       "rtx4090",
		GPUCount:      1,
		Tier:          "on_demand",
		Region:        "us-west",
		FRPRemotePort: &frpPort,
		TerminalPort:  &termPort,
		TemplateID:    &tmpl,
		AppAuthUser:   &user,
		AppAuthPass:   &pass,
		CreatedAt:     time.Now().UTC(),
	})
}

func TestGetInstance_RedactsPasswordByDefault(t *testing.T) {
	const (
		instID = "gpu-cred1"
		pass   = "s3cret-gpu59"
	)
	instStore := newStubInstanceStore()
	seedCredentialInstance(instStore, instID, "gpuai", pass)
	deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})
	deps.Config.FRPPublicHost = "frp.example.com"

	w := invokeInstance(t, deps, http.MethodGet, "/v1/instances/"+instID, "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get instance: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Non-secret connection surfaces stay.
	for _, keep := range []string{"app_user", "app_url", "terminal_url"} {
		if !strings.Contains(body, keep) {
			t.Errorf("plain read lost non-secret field %q: %s", keep, body)
		}
	}
	// The password — key and value — must be gone.
	for _, leak := range []string{"app_password", pass} {
		if strings.Contains(body, leak) {
			t.Errorf("plain read leaked %q (GPU-59 never-again pin): %s", leak, body)
		}
	}
}

func TestGetInstance_IncludeCredentialsReturnsPassword(t *testing.T) {
	const (
		instID = "gpu-cred2"
		pass   = "s3cret-gpu59-optin"
	)
	instStore := newStubInstanceStore()
	seedCredentialInstance(instStore, instID, "gpuai", pass)
	deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})
	deps.Config.FRPPublicHost = "frp.example.com"

	w := invokeInstance(t, deps, http.MethodGet, "/v1/instances/"+instID+"?include=credentials", "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get instance with include=credentials: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "app_password") || !strings.Contains(body, pass) {
		t.Errorf("opt-in read missing app_password/%q: %s", pass, body)
	}
}

func TestListInstances_NeverReturnsPassword(t *testing.T) {
	const (
		instID = "gpu-cred3"
		pass   = "s3cret-gpu59-list"
	)
	instStore := newStubInstanceStore()
	seedCredentialInstance(instStore, instID, "gpuai", pass)
	deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})
	deps.Config.FRPPublicHost = "frp.example.com"

	// Even with the param, the list must not return the password.
	for _, target := range []string{"/v1/instances", "/v1/instances?include=credentials"} {
		w := invokeInstance(t, deps, http.MethodGet, target, "org-A", "user-A", "ak-1", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("list %s: got %d, want 200; body=%s", target, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, instID) {
			t.Fatalf("list %s did not return the seeded instance: %s", target, body)
		}
		for _, leak := range []string{"app_password", pass} {
			if strings.Contains(body, leak) {
				t.Errorf("list %s leaked %q (GPU-59 never-again pin): %s", target, leak, body)
			}
		}
	}
}

func TestGetInstance_InvalidIncludeIs400(t *testing.T) {
	const instID = "gpu-cred4"
	instStore := newStubInstanceStore()
	seedCredentialInstance(instStore, instID, "gpuai", "whatever")
	deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})
	deps.Config.FRPPublicHost = "frp.example.com"

	w := invokeInstance(t, deps, http.MethodGet, "/v1/instances/"+instID+"?include=bogus", "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("include=bogus: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "validation_failed") {
		t.Errorf("include=bogus problem body missing code validation_failed: %s", w.Body.String())
	}
}
