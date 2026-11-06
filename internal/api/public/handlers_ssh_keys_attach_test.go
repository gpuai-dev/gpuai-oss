package public_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/api/public"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// invokeWithRole is invokeWithAPIKey's role-aware sibling: it injects an APIKey
// carrying the given DB role so the v33 admin-vs-member scoping can be tested.
// Routes are registered via HandlersForTesting (no auth/rate-limit chain).
func invokeWithRole(t *testing.T, deps public.PublicDeps, method, target, orgID, userID, role string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(public.WithAPIKey(req.Context(), &public.APIKey{
		APIKeyID: "ak-test",
		OrgID:    orgID,
		UserID:   userID,
		Role:     role,
		Scopes:   json.RawMessage(`{"full_access":true}`),
	}))
	mux := http.NewServeMux()
	public.HandlersForTesting(mux, deps)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestHandleListSSHKeys_AdminSeesAllOrgKeys: an admin/owner API key lists every
// key in the org (v33), not just its own — mirroring the dashboard. The response
// also carries attach_on_provision.
func TestHandleListSSHKeys_AdminSeesAllOrgKeys(t *testing.T) {
	stub := newStubSSHKeyDB()
	deps := public.PublicDepsWithSSHKeyStore(public.PublicDeps{}, stub)

	_ = stub.CreateSSHKey(context.Background(), &db.SSHKey{
		OrgID: "org-A", UserID: "user-A", Name: "mine",
		PublicKey: validEd25519Key, Fingerprint: "SHA256:fp-mine", AttachOnProvision: true,
	})
	_ = stub.CreateSSHKey(context.Background(), &db.SSHKey{
		OrgID: "org-A", UserID: "user-B", Name: "theirs",
		PublicKey: validEd25519Key, Fingerprint: "SHA256:fp-theirs", AttachOnProvision: false,
	})

	// Admin (user-A) sees BOTH keys.
	w := invokeWithRole(t, deps, http.MethodGet, "/v1/ssh-keys", "org-A", "user-A", "admin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("admin list status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var page public.Page[public.SSHKey]
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Data) != 2 {
		t.Fatalf("admin org-wide view: got %d rows, want 2", len(page.Data))
	}
	// attach_on_provision must round-trip per key.
	byName := map[string]bool{}
	for _, k := range page.Data {
		byName[k.Name] = k.AttachOnProvision
	}
	if !byName["mine"] || byName["theirs"] {
		t.Fatalf("attach_on_provision not reflected: %+v", byName)
	}

	// Member (user-B) sees only its own key.
	w = invokeWithRole(t, deps, http.MethodGet, "/v1/ssh-keys", "org-A", "user-B", "member", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("member list status: got %d, want 200", w.Code)
	}
	_ = json.NewDecoder(w.Body).Decode(&page)
	if len(page.Data) != 1 || page.Data[0].Name != "theirs" {
		t.Fatalf("member scoping broken: got %+v, want only %q", page.Data, "theirs")
	}
}
