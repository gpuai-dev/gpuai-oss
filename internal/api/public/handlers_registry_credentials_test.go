package public_test

// Registry-credential CRUD + private-image launch tests (custom images
// Phase 2): sealer-dark 503s, create/list/delete round trip (password never
// echoed), and the create-instance path — unknown credential 422, registry
// mismatch 422, and the full private-image happy path threading RegistryAuth
// + auth-gated pre-flight against an httptest registry that requires Basic
// credentials on its token endpoint.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/api/public"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
	"github.com/gpuai-dev/gpuai-oss/internal/registrycheck"
	"github.com/gpuai-dev/gpuai-oss/internal/registrycred"
)

// memCredStore is an in-memory registryCredentialDeps.
type memCredStore struct {
	mu    sync.Mutex
	creds map[string]db.RegistryCredential
	blobs map[string][]byte
}

func newMemCredStore() *memCredStore {
	return &memCredStore{creds: map[string]db.RegistryCredential{}, blobs: map[string][]byte{}}
}

func (m *memCredStore) CreateRegistryCredential(_ context.Context, cred db.RegistryCredential, enc []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.creds {
		if c.OrgID == cred.OrgID && c.Name == cred.Name {
			return db.ErrRegistryCredentialNameTaken
		}
	}
	cred.CreatedAt = time.Now()
	m.creds[cred.CredentialID] = cred
	m.blobs[cred.CredentialID] = enc
	return nil
}

func (m *memCredStore) ListRegistryCredentialsByOrg(_ context.Context, orgID string) ([]db.RegistryCredential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []db.RegistryCredential
	for _, c := range m.creds {
		if c.OrgID == orgID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memCredStore) GetRegistryCredentialSealed(_ context.Context, orgID, id string) (*db.RegistryCredential, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[id]
	if !ok || c.OrgID != orgID {
		return nil, nil, db.ErrRegistryCredentialNotFound
	}
	return &c, m.blobs[id], nil
}

func (m *memCredStore) DeleteRegistryCredential(_ context.Context, orgID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[id]
	if !ok || c.OrgID != orgID {
		return db.ErrRegistryCredentialNotFound
	}
	delete(m.creds, id)
	delete(m.blobs, id)
	return nil
}

func testSealer(t *testing.T) *registrycred.Sealer {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	s, err := registrycred.NewSealer(base64.StdEncoding.EncodeToString(k))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func credTestMux(t *testing.T, deps public.PublicDeps) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	public.RegistryCredentialHandlersForTesting(mux, deps)
	return mux
}

func doCredReq(t *testing.T, mux *http.ServeMux, method, path, org string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(public.WithAPIKey(req.Context(), &public.APIKey{APIKeyID: "ak", OrgID: org, UserID: "u"}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestRegistryCredentialsDarkWithoutSealer(t *testing.T) {
	deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), &stubEngine{})
	deps = public.PublicDepsWithRegistryCredentialStore(deps, newMemCredStore())
	mux := credTestMux(t, deps)
	w := doCredReq(t, mux, http.MethodGet, "/v1/registry-credentials", "org-A", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no sealer: got %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

func TestRegistryCredentialsCRUD(t *testing.T) {
	deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), &stubEngine{})
	deps.RegistryCredSealer = testSealer(t)
	deps = public.PublicDepsWithRegistryCredentialStore(deps, newMemCredStore())
	mux := credTestMux(t, deps)

	// Create.
	w := doCredReq(t, mux, http.MethodPost, "/v1/registry-credentials", "org-A", map[string]string{
		"name": "ghcr-acme", "registry": "GHCR.io", "username": "acme-bot", "password": "ghp_tok123",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ghp_tok123") {
		t.Fatal("create response echoed the password")
	}
	var created public.RegistryCredentialResponse
	_ = json.NewDecoder(w.Body).Decode(&created)
	if created.Registry != "ghcr.io" {
		t.Errorf("registry not normalized: %q", created.Registry)
	}

	// Duplicate name → 409.
	w = doCredReq(t, mux, http.MethodPost, "/v1/registry-credentials", "org-A", map[string]string{
		"name": "ghcr-acme", "registry": "ghcr.io", "username": "x", "password": "y",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate: got %d, want 409", w.Code)
	}

	// Whitespace in password → 422.
	w = doCredReq(t, mux, http.MethodPost, "/v1/registry-credentials", "org-A", map[string]string{
		"name": "bad", "registry": "ghcr.io", "username": "x", "password": "a b",
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("whitespace password: got %d, want 422", w.Code)
	}

	// List never carries secret material.
	w = doCredReq(t, mux, http.MethodGet, "/v1/registry-credentials", "org-A", nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "ghp_tok123") || strings.Contains(w.Body.String(), "password") {
		t.Fatalf("list: code=%d body=%s", w.Code, w.Body.String())
	}

	// Cross-org delete → 404; own delete → 204.
	if w = doCredReq(t, mux, http.MethodDelete, "/v1/registry-credentials/"+created.ID, "org-B", nil); w.Code != http.StatusNotFound {
		t.Fatalf("cross-org delete: got %d, want 404", w.Code)
	}
	if w = doCredReq(t, mux, http.MethodDelete, "/v1/registry-credentials/"+created.ID, "org-A", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204", w.Code)
	}
}

// privateImageRegistry requires a Basic-authed token fetch and serves one
// amd64 repo acme/secret. Returns the server and the manifest digest.
func privateImageRegistry(t *testing.T, user, pass string) (*httptest.Server, string) {
	t.Helper()
	manifest, _ := json.Marshal(map[string]any{
		"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
		"config":    map[string]string{"digest": "sha256:cfg"},
	})
	sum := sha256.Sum256(manifest)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	var srvURL string
	const tokenVal = "priv-token"
	mux.HandleFunc("GET /v2/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+srvURL+`/token",service="priv"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": tokenVal})
	})
	authed := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+tokenVal }
	mux.HandleFunc("GET /v2/acme/secret/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write(manifest)
	})
	mux.HandleFunc("GET /v2/acme/secret/blobs/{d}", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"architecture": "amd64", "os": "linux",
			"config": map[string]any{"Entrypoint": []string{"/private.sh"}},
		})
	})
	srv := httptest.NewServer(mux)
	srvURL = srv.URL
	t.Cleanup(srv.Close)
	return srv, digest
}

func TestHandleCreateInstance_PrivateImage(t *testing.T) {
	const user, pass = "acme-bot", "sekret-tok"
	srv, digest := privateImageRegistry(t, user, pass)

	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-priv1", Hostname: "gpu-priv1.example.com"}}
	deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)
	deps.RegistryCredSealer = testSealer(t)
	store := newMemCredStore()
	deps = public.PublicDepsWithRegistryCredentialStore(deps, store)
	host := strings.TrimPrefix(srv.URL, "http://")
	deps.RegistryChecker = registrycheck.New(registrycheck.WithTestTransport(func(string) string { return host }))

	// Seed a credential the launch will reference. reg.example.com matches
	// the image's registry.
	credID := registrycred.NewID()
	sealed, err := deps.RegistryCredSealer.Seal(credID, pass)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRegistryCredential(context.Background(), db.RegistryCredential{
		CredentialID: credID, OrgID: "org-A", Name: "priv", Registry: "reg.example.com", Username: user,
	}, sealed); err != nil {
		t.Fatal(err)
	}
	// A second credential on a DIFFERENT registry for the mismatch case.
	otherID := registrycred.NewID()
	otherSealed, _ := deps.RegistryCredSealer.Seal(otherID, "x")
	_ = store.CreateRegistryCredential(context.Background(), db.RegistryCredential{
		CredentialID: otherID, OrgID: "org-A", Name: "other", Registry: "ghcr.io", Username: "u",
	}, otherSealed)

	// Unknown credential id → 422.
	w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1",
		imageBody(map[string]any{"image": "reg.example.com/acme/secret:v1", "registry_credential_id": registrycred.NewID()}))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown credential: got %d; body=%s", w.Code, w.Body.String())
	}
	// Registry mismatch → 422.
	w = invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1",
		imageBody(map[string]any{"image": "reg.example.com/acme/secret:v1", "registry_credential_id": otherID}))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "ghcr.io") {
		t.Fatalf("registry mismatch: got %d; body=%s", w.Code, w.Body.String())
	}
	// Credential on another org → 422 (not found under org-B's scope).
	w = invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-B", "user-B", "ak-2",
		imageBody(map[string]any{"image": "reg.example.com/acme/secret:v1", "registry_credential_id": credID}))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cross-org credential: got %d; body=%s", w.Code, w.Body.String())
	}

	// Happy path: private image + credential → 202, RegistryAuth threaded,
	// digest-pinned, entrypoint captured through the authed pre-flight.
	w = invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1",
		imageBody(map[string]any{"image": "reg.example.com/acme/secret:v1", "registry_credential_id": credID}))
	if w.Code != http.StatusAccepted {
		t.Fatalf("private happy path: got %d; body=%s", w.Code, w.Body.String())
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
	if got.RegistryAuth == nil || got.RegistryAuth.Username != user || got.RegistryAuth.Password != pass || got.RegistryAuth.Registry != "reg.example.com" {
		t.Fatalf("RegistryAuth not threaded: %+v", got.RegistryAuth)
	}
	if got.Image != "reg.example.com/acme/secret:v1@"+digest {
		t.Errorf("Image: got %q", got.Image)
	}
	if len(got.ImageEntrypoint) != 1 || got.ImageEntrypoint[0] != "/private.sh" {
		t.Errorf("ImageEntrypoint: got %v", got.ImageEntrypoint)
	}
}

func TestHandleCreateInstance_RuntimeOverridesAndPorts(t *testing.T) {
	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-rt1", Hostname: "gpu-rt1.example.com"}}
	srv, _ := imageTestRegistry(t)
	deps := imageTestDeps(t, engine, srv)

	// Runtime fields without image → 422.
	for name, extra := range map[string]map[string]any{
		"entrypoint sans image": {"entrypoint": []string{"/x"}},
		"ports sans image":      {"ports": []map[string]any{{"port": 8000, "protocol": "http"}}},
		"credential sans image": {"registry_credential_id": "abc"},
	} {
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", imageBody(extra))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: got %d, want 422; body=%s", name, w.Code, w.Body.String())
		}
	}
	// Bad ports.
	for name, ports := range map[string]any{
		"port 22":        []map[string]any{{"port": 22, "protocol": "tcp"}},
		"two primaries":  []map[string]any{{"port": 80, "protocol": "http", "primary": true}, {"port": 81, "protocol": "http", "primary": true}},
		"tcp primary":    []map[string]any{{"port": 80, "protocol": "tcp", "primary": true}},
		"bad protocol":   []map[string]any{{"port": 80, "protocol": "udp"}},
		"duplicate port": []map[string]any{{"port": 80, "protocol": "http"}, {"port": 80, "protocol": "tcp"}},
	} {
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1",
			imageBody(map[string]any{"image": "reg.example.com/acme/good:v1", "ports": ports}))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: got %d, want 422; body=%s", name, w.Code, w.Body.String())
		}
	}

	// Happy path: entrypoint override (drops image cmd), ports threaded,
	// primary port becomes AppPort.
	w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1",
		imageBody(map[string]any{
			"image":      "reg.example.com/acme/good:v1",
			"entrypoint": []string{"python", "-m", "server"},
			"ports": []map[string]any{
				{"port": 8000, "protocol": "http", "primary": true},
				{"port": 9000, "protocol": "tcp"},
			},
		}))
	if w.Code != http.StatusAccepted {
		t.Fatalf("happy path: got %d; body=%s", w.Code, w.Body.String())
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
	if len(got.ImageEntrypoint) != 3 || got.ImageEntrypoint[0] != "python" {
		t.Errorf("entrypoint override: got %v", got.ImageEntrypoint)
	}
	// Docker semantics: entrypoint override drops the image's CMD.
	if len(got.ImageCmd) != 0 {
		t.Errorf("image cmd should be dropped on entrypoint override, got %v", got.ImageCmd)
	}
	if got.AppPort != 8000 {
		t.Errorf("AppPort: got %d, want 8000", got.AppPort)
	}
	if len(got.Ports) != 2 || !got.Ports[0].Primary || got.Ports[1].Port != 9000 {
		t.Errorf("Ports: got %+v", got.Ports)
	}
}
