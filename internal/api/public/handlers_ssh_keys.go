package public

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/authz"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"golang.org/x/crypto/ssh"
)

// SSHKey is the public-API SSH key response shape.
//
// Privacy contract (PRIV-08): no `org_id`, no `user_id`, no `public_key`
// (the raw key body is sensitive — clients already have it). Only the
// stable identifiers + fingerprint round-trip on the wire. The
// TestNoUpstreamLeakage_Catalog regression test in privacy_test.go
// asserts struct-tag drift cannot reintroduce upstream/internal fields.
type SSHKey struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	// AttachOnProvision reports whether this key is auto-attached to a new
	// instance's authorized_keys at launch when the request carries no explicit
	// ssh_key_ids (v33). It is the owner's own per-key setting — not an
	// upstream/internal field — so it does not breach the PRIV-08 contract.
	AttachOnProvision bool      `json:"attach_on_provision"`
	CreatedAt         time.Time `json:"created_at"`
}

// CreateSSHKeyRequest is POST /v1/ssh-keys body.
type CreateSSHKeyRequest struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

// UpdateSSHKeyRequest is PATCH /v1/ssh-keys/{id} body. AttachOnProvision is a
// pointer so an absent field is rejected rather than silently disabling the key.
type UpdateSSHKeyRequest struct {
	AttachOnProvision *bool `json:"attach_on_provision"`
}

// allowedSSHKeyTypes — copied verbatim from internal/api/handlers_ssh_keys.go
// to keep the public package self-contained (banned_imports_test enforces no
// internal/api dependency). DSA (ssh-dss) is intentionally absent — it is
// cryptographically deprecated by OpenSSH 7.0+ and we never want a customer
// re-using a DSA key against a freshly-deployed instance.
var allowedSSHKeyTypes = map[string]bool{
	"ssh-rsa":             true,
	"ssh-ed25519":         true,
	"ecdsa-sha2-nistp256": true,
	"ecdsa-sha2-nistp384": true,
	"ecdsa-sha2-nistp521": true,
}

// sshKeyDeps abstracts the DB methods this group of handlers needs. *db.Pool
// satisfies it in production; tests substitute an in-memory stub via
// PublicDepsWithSSHKeyStore. Mirrors the ApiKeyLookup / PublicIdempotencyDeps
// pattern already in this package — narrow interface, easy to test, no
// reflection.
type sshKeyDeps interface {
	CreateSSHKey(ctx context.Context, k *db.SSHKey) error
	ListSSHKeysByOrg(ctx context.Context, orgID string) ([]db.SSHKey, error)
	ListSSHKeysByOrgAndUser(ctx context.Context, orgID, userID string) ([]db.SSHKey, error)
	GetSSHKeyForOrg(ctx context.Context, keyID, orgID string) (*db.SSHKey, error)
	SetSSHKeyAttachOnProvision(ctx context.Context, keyID, orgID string, attach bool) (bool, error)
	DeleteSSHKey(ctx context.Context, keyID, orgID string) (bool, error)
}

// resolveSSHKeyStore returns the SSH-key store the handlers should use.
// Test deps populated via PublicDepsWithSSHKeyStore set sshKeyStore directly;
// production deps fall through to *db.Pool which already implements the
// sshKeyDeps interface.
func (deps PublicDeps) resolveSSHKeyStore() sshKeyDeps {
	if deps.sshKeyStore != nil {
		return deps.sshKeyStore
	}
	return deps.DB
}

// RegisterSSHKeyRoutes wires the SSH-key endpoints onto mux using the
// standard chain/chainWrite middleware composition from chain.go.
//
// Wave 5 router-serialization convention: each handler module owns this
// helper so router.go gains exactly ONE LINE per plan and Wave 6+ plans
// (18-14/15/16) do not collide on router.go edits.
func RegisterSSHKeyRoutes(mux *http.ServeMux, deps PublicDeps) {
	mux.Handle("GET /v1/ssh-keys",
		chain(deps, "ssh_keys", "read")(http.HandlerFunc(deps.handleListSSHKeys)))
	mux.Handle("GET /v1/ssh-keys/{id}",
		chain(deps, "ssh_keys", "read")(http.HandlerFunc(deps.handleGetSSHKey)))
	mux.Handle("POST /v1/ssh-keys",
		chainWrite(deps, "ssh_keys")(http.HandlerFunc(deps.handleCreateSSHKey)))
	mux.Handle("DELETE /v1/ssh-keys/{id}",
		chainWrite(deps, "ssh_keys")(http.HandlerFunc(deps.handleDeleteSSHKey)))
}

func (deps PublicDeps) handleListSSHKeys(w http.ResponseWriter, r *http.Request) {
	apiKey := APIKeyFromContext(r.Context())
	if apiKey == nil {
		WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	store := deps.resolveSSHKeyStore()
	params := ParsePageParams(r)
	// Role-aware scoping (v33), mirroring the dashboard: an admin/owner API key
	// sees ALL org keys; a member sees only the keys they own. Legacy API keys
	// without user attribution keep the org-wide view. Role is resolved from
	// users.role by the auth middleware (SEC-01: DB-authoritative, fails closed
	// to "member").
	var rows []db.SSHKey
	var err error
	if apiKey.UserID != "" && !authz.IsAdminPlus(apiKey.Role) {
		rows, err = store.ListSSHKeysByOrgAndUser(r.Context(), apiKey.OrgID, apiKey.UserID)
	} else {
		rows, err = store.ListSSHKeysByOrg(r.Context(), apiKey.OrgID)
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "ssh-keys: list failed", "error", err.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to list SSH keys")
		return
	}
	// Apply cursor (rows already DESC by created_at). Cursor encodes
	// (created_at, ssh_key_id); we drop rows whose timestamp comes
	// after the cursor's, with id as the deterministic tiebreak.
	if params.Cursor != "" {
		ts, id, derr := DecodeCursor(params.Cursor)
		if derr != nil {
			WriteProblem(w, r, http.StatusBadRequest, "validation_failed", "Invalid cursor")
			return
		}
		filtered := rows[:0]
		for _, k := range rows {
			if k.CreatedAt.Before(ts) || (k.CreatedAt.Equal(ts) && k.SSHKeyID < id) {
				filtered = append(filtered, k)
			}
		}
		rows = filtered
	}
	items := make([]SSHKey, len(rows))
	for i, k := range rows {
		items[i] = SSHKey{ID: k.SSHKeyID, Name: k.Name, Fingerprint: k.Fingerprint, AttachOnProvision: k.AttachOnProvision, CreatedAt: k.CreatedAt}
	}
	page := BuildPage(items, params.Limit, func(s SSHKey) (time.Time, string) {
		return s.CreatedAt, s.ID
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(page)
}

func (deps PublicDeps) handleCreateSSHKey(w http.ResponseWriter, r *http.Request) {
	apiKey := APIKeyFromContext(r.Context())
	if apiKey == nil {
		WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	var req CreateSSHKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "validation_failed", "Invalid JSON body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if l := len(req.Name); l < 1 || l > 128 {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Name must be 1-128 chars")
		return
	}
	pk, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(req.PublicKey)))
	if parseErr != nil {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Invalid SSH public key")
		return
	}
	if !allowedSSHKeyTypes[pk.Type()] {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed",
			"SSH key type not allowed: "+pk.Type())
		return
	}
	fingerprint := ssh.FingerprintSHA256(pk)
	row := &db.SSHKey{
		OrgID:       apiKey.OrgID,
		UserID:      apiKey.UserID,
		Name:        req.Name,
		PublicKey:   req.PublicKey,
		Fingerprint: fingerprint,
	}
	store := deps.resolveSSHKeyStore()
	if err := store.CreateSSHKey(r.Context(), row); err != nil {
		if errors.Is(err, db.ErrDuplicateKey) {
			// 409 with code "validation_failed" matches Stripe's pattern of
			// returning a typed validation error for "this thing already
			// exists" — the client cannot retry blindly, so it MUST be
			// reflected in the public error registry. (See errors.go: the
			// validation_failed code is already mapped to 422 by default;
			// here we deliberately use 409 because the duplicate is a
			// resource-state conflict, not a request-shape error. The code
			// stays validation_failed so the SDK switch-statement
			// registers ONE handler for "this key already exists" instead
			// of two.)
			WriteProblem(w, r, http.StatusConflict, "validation_failed",
				"An SSH key with this fingerprint already exists in your organization")
			return
		}
		slog.ErrorContext(r.Context(), "ssh-keys: create failed", "error", err.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to create SSH key")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(SSHKey{
		ID: row.SSHKeyID, Name: row.Name, Fingerprint: row.Fingerprint, AttachOnProvision: row.AttachOnProvision, CreatedAt: row.CreatedAt,
	})
}

func (deps PublicDeps) handleGetSSHKey(w http.ResponseWriter, r *http.Request) {
	apiKey := APIKeyFromContext(r.Context())
	if apiKey == nil {
		WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	id := r.PathValue("id")
	store := deps.resolveSSHKeyStore()
	row, err := store.GetSSHKeyForOrg(r.Context(), id, apiKey.OrgID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			WriteProblem(w, r, http.StatusNotFound, "not_found", "SSH key not found")
			return
		}
		slog.ErrorContext(r.Context(), "ssh-keys: get failed", "error", err.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to fetch SSH key")
		return
	}
	// Same role-aware scoping as list: a member cannot see another member's key
	// (404, not 403 — a forbidden would leak that the ID exists in the org). An
	// admin/owner key may read any key in its org.
	if apiKey.UserID != "" && !authz.IsAdminPlus(apiKey.Role) && row.UserID != apiKey.UserID {
		WriteProblem(w, r, http.StatusNotFound, "not_found", "SSH key not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(SSHKey{
		ID: row.SSHKeyID, Name: row.Name, Fingerprint: row.Fingerprint, AttachOnProvision: row.AttachOnProvision, CreatedAt: row.CreatedAt,
	})
}

func (deps PublicDeps) handleDeleteSSHKey(w http.ResponseWriter, r *http.Request) {
	apiKey := APIKeyFromContext(r.Context())
	if apiKey == nil {
		WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	id := r.PathValue("id")
	store := deps.resolveSSHKeyStore()
	// Ownership check before destroy: a user-attributed API key may only
	// delete keys its user owns. 404 (not 403) so the existence of another
	// member's key ID does not leak. Org-wide deletion stays a dashboard
	// admin capability.
	if apiKey.UserID != "" {
		row, err := store.GetSSHKeyForOrg(r.Context(), id, apiKey.OrgID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				WriteProblem(w, r, http.StatusNotFound, "not_found", "SSH key not found")
				return
			}
			slog.ErrorContext(r.Context(), "ssh-keys: delete lookup failed", "error", err.Error())
			WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to delete SSH key")
			return
		}
		if row.UserID != apiKey.UserID {
			WriteProblem(w, r, http.StatusNotFound, "not_found", "SSH key not found")
			return
		}
	}
	deleted, err := store.DeleteSSHKey(r.Context(), id, apiKey.OrgID)
	if err != nil {
		slog.ErrorContext(r.Context(), "ssh-keys: delete failed", "error", err.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to delete SSH key")
		return
	}
	if !deleted {
		// D-07: idempotent destroy returns 404 (not 204) the second time —
		// the contract surface lets clients distinguish "key was never
		// there" from "key was just deleted". The dashboard handler in
		// internal/api/handlers_ssh_keys.go does the same.
		WriteProblem(w, r, http.StatusNotFound, "not_found", "SSH key not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandlersForTesting registers the 4 SSH-key handlers WITHOUT the auth /
// rate-limit / idempotency chain. Tests inject an APIKey on the request
// context directly (via WithAPIKey) and exercise handler logic in isolation.
//
// Production code MUST NOT call this — it bypasses the very middleware that
// enforces tenancy and rate limits. The function is kept here (rather than
// in a *_test.go file) so external _test packages — which cannot reach
// unexported handler methods — have a stable seam.
func HandlersForTesting(mux *http.ServeMux, deps PublicDeps) {
	mux.Handle("GET /v1/ssh-keys", http.HandlerFunc(deps.handleListSSHKeys))
	mux.Handle("GET /v1/ssh-keys/{id}", http.HandlerFunc(deps.handleGetSSHKey))
	mux.Handle("POST /v1/ssh-keys", http.HandlerFunc(deps.handleCreateSSHKey))
	mux.Handle("DELETE /v1/ssh-keys/{id}", http.HandlerFunc(deps.handleDeleteSSHKey))
}

// PublicDepsWithSSHKeyStore returns a copy of deps wired with a custom SSH-key
// store. Tests use this to inject in-memory fixtures without spinning up
// Postgres. Production callers MUST NOT use this seam — *db.Pool already
// implements sshKeyDeps and is wired automatically when DB is set.
func PublicDepsWithSSHKeyStore(deps PublicDeps, store sshKeyDeps) PublicDeps {
	deps.sshKeyStore = store
	return deps
}
