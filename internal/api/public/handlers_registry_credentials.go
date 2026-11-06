package public

// Registry-credential endpoints (custom images Phase 2): per-org private
// registry logins, stored sealed (internal/registrycred) and referenced by id
// from POST /v1/instances. The password crosses the wire exactly once — on
// create — and is never returned by any read; list/create responses carry
// metadata only. When no sealing key is configured (registrycred disabled)
// every endpoint answers 503 so the feature ships dark until the environment
// is provisioned.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/imageref"
	"github.com/gpuai-dev/gpuai-oss/internal/registrycred"
)

// RegistryCredentialResponse is the metadata view — no secret material, ever.
type RegistryCredentialResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Registry  string    `json:"registry"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateRegistryCredentialRequest is POST /v1/registry-credentials body.
type CreateRegistryCredentialRequest struct {
	// Name is the org-unique handle (e.g. "ghcr-acme").
	Name string `json:"name"`
	// Registry is the registry host the login belongs to (e.g. "ghcr.io",
	// "docker.io"). Validated with the same host rules as image refs.
	Registry string `json:"registry"`
	Username string `json:"username"`
	// Password is the registry password or access token. Write-only: sealed
	// before storage, absent from every response and every log.
	Password string `json:"password"`
}

type registryCredentialDeps interface {
	CreateRegistryCredential(ctx context.Context, cred db.RegistryCredential, passwordEnc []byte) error
	ListRegistryCredentialsByOrg(ctx context.Context, orgID string) ([]db.RegistryCredential, error)
	GetRegistryCredentialSealed(ctx context.Context, orgID, credentialID string) (*db.RegistryCredential, []byte, error)
	DeleteRegistryCredential(ctx context.Context, orgID, credentialID string) error
}

func (deps PublicDeps) resolveRegistryCredentialStore() registryCredentialDeps {
	if deps.registryCredStore != nil {
		return deps.registryCredStore
	}
	return deps.DB
}

// RegisterRegistryCredentialRoutes wires the endpoints (one line in router.go).
// Scoped under "instances" (not a new resource): a registry credential exists
// solely to launch instances, so the permission that can launch is the
// permission that can manage the logins those launches use — and the
// KnownResources roster (pinned by scopes_test) stays closed.
func RegisterRegistryCredentialRoutes(mux *http.ServeMux, deps PublicDeps) {
	mux.Handle("GET /v1/registry-credentials",
		chain(deps, "instances", "read")(http.HandlerFunc(deps.handleListRegistryCredentials)))
	mux.Handle("POST /v1/registry-credentials",
		chainWrite(deps, "instances")(http.HandlerFunc(deps.handleCreateRegistryCredential)))
	mux.Handle("DELETE /v1/registry-credentials/{id}",
		chainWrite(deps, "instances")(http.HandlerFunc(deps.handleDeleteRegistryCredential)))
}

// hasControlBytes reports whether s contains any C0 control byte or DEL.
func hasControlBytes(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// ValidateRegistryCredentialInput is the shared create-credential validation
// ladder: field presence + length bounds, the no-whitespace/control-byte rule
// for both credential halves, and the imageref host grammar + SSRF rules for
// the registry. Returns the NORMALIZED registry host to store. Exported so the
// dashboard mirror (internal/api) judges the identical predicate — a credential
// accepted on one surface must be accepted, byte-for-byte, on the other.
func ValidateRegistryCredentialInput(name, registry, username, password string) (string, error) {
	switch {
	case name == "" || len(name) > 64:
		return "", errors.New("name is required (at most 64 characters)")
	case registry == "":
		return "", errors.New("registry is required (e.g. ghcr.io, docker.io)")
	case username == "" || len(username) > 256:
		return "", errors.New("username is required (at most 256 characters)")
	case password == "" || len(password) > 4096:
		return "", errors.New("password is required (at most 4096 characters)")
	}
	// No whitespace/control bytes in either half: real registry usernames and
	// tokens never contain them, and downstream mechanisms (a provider's
	// docker-login flags string) are whitespace-delimited.
	if strings.ContainsAny(username, " \t\r\n") || hasControlBytes(username) ||
		strings.ContainsAny(password, " \t\r\n") || hasControlBytes(password) {
		return "", errors.New("username and password must not contain whitespace or control characters")
	}
	// The registry host obeys the same grammar + SSRF rules as image refs —
	// a credential for localhost/a private IP could never be used anyway.
	return imageref.NormalizeRegistryHost(registry)
}

// requireRegistryCredSealer 503s when the sealing key is not configured.
func (deps PublicDeps) requireRegistryCredSealer(w http.ResponseWriter, r *http.Request) bool {
	if deps.RegistryCredSealer == nil || !deps.RegistryCredSealer.Enabled() {
		WriteProblem(w, r, http.StatusServiceUnavailable, "registry_credentials_unavailable",
			"Private registry credentials are not enabled on this environment yet")
		return false
	}
	return true
}

func (deps PublicDeps) handleCreateRegistryCredential(w http.ResponseWriter, r *http.Request) {
	apiKey := APIKeyFromContext(r.Context())
	if apiKey == nil {
		WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	if !deps.requireRegistryCredSealer(w, r) {
		return
	}
	var req CreateRegistryCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "validation_failed", "Invalid JSON body")
		return
	}
	registry, err := ValidateRegistryCredentialInput(req.Name, req.Registry, req.Username, req.Password)
	if err != nil {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}

	cred := db.RegistryCredential{
		CredentialID: registrycred.NewID(),
		OrgID:        apiKey.OrgID,
		Name:         req.Name,
		Registry:     registry,
		Username:     req.Username,
		CreatedAt:    time.Now().UTC(),
	}
	sealed, err := deps.RegistryCredSealer.Seal(cred.CredentialID, req.Password)
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to store credential")
		return
	}
	if err := deps.resolveRegistryCredentialStore().CreateRegistryCredential(r.Context(), cred, sealed); err != nil {
		if errors.Is(err, db.ErrRegistryCredentialNameTaken) {
			WriteProblem(w, r, http.StatusConflict, "name_taken", "A registry credential with this name already exists")
			return
		}
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to store credential")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(RegistryCredentialResponse{
		ID: cred.CredentialID, Name: cred.Name, Registry: cred.Registry,
		Username: cred.Username, CreatedAt: cred.CreatedAt,
	})
}

func (deps PublicDeps) handleListRegistryCredentials(w http.ResponseWriter, r *http.Request) {
	apiKey := APIKeyFromContext(r.Context())
	if apiKey == nil {
		WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	if !deps.requireRegistryCredSealer(w, r) {
		return
	}
	creds, err := deps.resolveRegistryCredentialStore().ListRegistryCredentialsByOrg(r.Context(), apiKey.OrgID)
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to list credentials")
		return
	}
	out := make([]RegistryCredentialResponse, 0, len(creds))
	for _, c := range creds {
		out = append(out, RegistryCredentialResponse{
			ID: c.CredentialID, Name: c.Name, Registry: c.Registry,
			Username: c.Username, CreatedAt: c.CreatedAt,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
}

func (deps PublicDeps) handleDeleteRegistryCredential(w http.ResponseWriter, r *http.Request) {
	apiKey := APIKeyFromContext(r.Context())
	if apiKey == nil {
		WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	if !deps.requireRegistryCredSealer(w, r) {
		return
	}
	err := deps.resolveRegistryCredentialStore().DeleteRegistryCredential(r.Context(), apiKey.OrgID, r.PathValue("id"))
	if errors.Is(err, db.ErrRegistryCredentialNotFound) {
		WriteProblem(w, r, http.StatusNotFound, "not_found", "Registry credential not found")
		return
	}
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to delete credential")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PublicDepsWithRegistryCredentialStore injects an in-memory store for tests.
func PublicDepsWithRegistryCredentialStore(deps PublicDeps, store registryCredentialDeps) PublicDeps {
	deps.registryCredStore = store
	return deps
}

// RegistryCredentialHandlersForTesting registers the handlers without the
// auth/rate-limit chain (tests inject APIKey on the context directly).
func RegistryCredentialHandlersForTesting(mux *http.ServeMux, deps PublicDeps) {
	mux.Handle("GET /v1/registry-credentials", http.HandlerFunc(deps.handleListRegistryCredentials))
	mux.Handle("POST /v1/registry-credentials", http.HandlerFunc(deps.handleCreateRegistryCredential))
	mux.Handle("DELETE /v1/registry-credentials/{id}", http.HandlerFunc(deps.handleDeleteRegistryCredential))
}
