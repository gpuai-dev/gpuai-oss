package public_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/api/public"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// stubLookup is an in-memory ApiKeyLookup for middleware tests.
// touched is atomic because ApiKeyAuthMiddleware fires TouchAPIKeyLastUsed
// in a detached goroutine; the test reads the counter from the request
// goroutine. Plain int trips the race detector under `go test -race`.
type stubLookup struct {
	keyHash string
	row     *db.APIKey
	err     error
	touched atomic.Int64
	// role / roleErr drive GetUserRoleByUserID (Phase 39, SEC-01). Zero
	// values ("", nil) exercise the middleware's fail-closed "member"
	// fallback; tests set role explicitly to assert propagation.
	role    string
	roleErr error
}

func (s *stubLookup) LookupAPIKeyByHash(ctx context.Context, hash string) (*db.APIKey, error) {
	if s.err != nil {
		return nil, s.err
	}
	if hash != s.keyHash {
		return nil, db.ErrNotFound
	}
	return s.row, nil
}

func (s *stubLookup) TouchAPIKeyLastUsed(ctx context.Context, id string) error {
	s.touched.Add(1)
	return nil
}

func (s *stubLookup) GetUserRoleByUserID(ctx context.Context, userID string) (string, error) {
	if s.roleErr != nil {
		return "", s.roleErr
	}
	return s.role, nil
}

func TestApiKeyAuth_MissingHeader(t *testing.T) {
	h := public.ApiKeyAuthMiddleware(&stubLookup{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("downstream should not run")
	}))
	req := httptest.NewRequest("GET", "/v1/instances", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", w.Code)
	}
	var p public.Problem
	if err := json.NewDecoder(w.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Code != "unauthenticated" {
		t.Errorf("code: got %q", p.Code)
	}
}

func TestApiKeyAuth_MalformedKey(t *testing.T) {
	h := public.ApiKeyAuthMiddleware(&stubLookup{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("downstream should not run")
	}))
	req := httptest.NewRequest("GET", "/v1/instances", nil)
	req.Header.Set("Authorization", "Bearer notvalidkey")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", w.Code)
	}
}

func TestApiKeyAuth_NotFound(t *testing.T) {
	h := public.ApiKeyAuthMiddleware(&stubLookup{err: db.ErrNotFound})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("downstream should not run")
	}))
	req := httptest.NewRequest("GET", "/v1/instances", nil)
	// valid format, won't be in the lookup.
	raw := "gpuai_live_" + "abcdefghijklmnopqrstuvwx"
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", w.Code)
	}
}

func TestApiKeyAuth_HappyPath(t *testing.T) {
	raw := "gpuai_live_" + "abcdefghijklmnopqrstuvwx"
	hash := public.HashKey(raw)
	// Force LastUsedAt nil → throttle gate allows touch.
	stub := &stubLookup{
		keyHash: hash,
		row: &db.APIKey{
			APIKeyID: "00000000-0000-0000-0000-000000000001",
			OrgID:    "org-uuid",
			UserID:   "user-uuid",
			Name:     "test",
			KeyHash:  hash, KeyPrefix: "gpuai_live_abcd", Last4: "uvwx",
			Scopes:       json.RawMessage(`{"full_access":true}`),
			RateLimitRPS: 100, RateLimitBurst: 200,
			LastUsedAt: nil,
		},
	}
	stub.role = "admin" // assert DB-role propagation (SEC-01)
	var seen *public.APIKey
	h := public.ApiKeyAuthMiddleware(stub)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = public.APIKeyFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/v1/instances", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if seen == nil || seen.APIKeyID != stub.row.APIKeyID {
		t.Fatalf("APIKey not propagated to ctx: got %+v", seen)
	}
	if seen.Role != "admin" {
		t.Errorf("Role not propagated from DB lookup: got %q, want admin", seen.Role)
	}
	// Touch is fire-and-forget; allow the goroutine a moment.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && stub.touched.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if stub.touched.Load() == 0 {
		t.Errorf("TouchAPIKeyLastUsed never invoked (race-window grace expired)")
	}
}

func TestApiKeyAuth_TouchThrottled(t *testing.T) {
	// LastUsedAt within 30s → touch should be SKIPPED.
	raw := "gpuai_live_" + "abcdefghijklmnopqrstuvwx"
	hash := public.HashKey(raw)
	recent := time.Now().Add(-5 * time.Second)
	stub := &stubLookup{
		keyHash: hash,
		row: &db.APIKey{
			APIKeyID: "id-throttle", OrgID: "o", UserID: "u",
			KeyHash: hash, KeyPrefix: "gpuai_live_thrt", Last4: "lst4",
			Scopes:     json.RawMessage(`{"full_access":true}`),
			LastUsedAt: &recent,
		},
	}
	h := public.ApiKeyAuthMiddleware(stub)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/v1/instances", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	h.ServeHTTP(httptest.NewRecorder(), req)
	// Wait briefly to ensure no goroutine fires.
	time.Sleep(200 * time.Millisecond)
	if got := stub.touched.Load(); got != 0 {
		t.Errorf("Touch should have been throttled (LastUsedAt was 5s ago); got %d invocations", got)
	}
}

func TestRequireScope_Allows(t *testing.T) {
	// RequireScope reads scopes from context — we exercise it via the auth
	// middleware end-to-end so the context is populated correctly.
	raw := "gpuai_live_" + "abcdefghijklmnopqrstuvwx"
	hash := public.HashKey(raw)
	stub := &stubLookup{
		keyHash: hash,
		row: &db.APIKey{
			APIKeyID: "id1", OrgID: "o", UserID: "u",
			KeyHash: hash, KeyPrefix: "gpuai_live_abcd", Last4: "uvwx",
			Scopes: json.RawMessage(`{"instances":"write"}`),
		},
	}
	var ran bool
	chained := public.ApiKeyAuthMiddleware(stub)(public.RequireScope("instances", "write")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest("GET", "/v1/instances", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	chained.ServeHTTP(w, req)
	if !ran || w.Code != http.StatusOK {
		t.Errorf("expected handler to run with 200; got code %d ran=%v body=%s", w.Code, ran, w.Body.String())
	}
}

func TestRequireScope_Denies(t *testing.T) {
	raw := "gpuai_live_" + "abcdefghijklmnopqrstuvwx"
	hash := public.HashKey(raw)
	stub := &stubLookup{
		keyHash: hash,
		row: &db.APIKey{
			APIKeyID: "id1", OrgID: "o", UserID: "u",
			KeyHash: hash, KeyPrefix: "gpuai_live_abcd", Last4: "uvwx",
			Scopes: json.RawMessage(`{"instances":"read"}`),
		},
	}
	chained := public.ApiKeyAuthMiddleware(stub)(public.RequireScope("instances", "write")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("downstream should not run")
	})))
	req := httptest.NewRequest("DELETE", "/v1/instances/x", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	chained.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", w.Code)
	}
	var p public.Problem
	if err := json.NewDecoder(w.Body).Decode(&p); err != nil || p.Code != "insufficient_scope" {
		t.Errorf("body: code=%q err=%v", p.Code, err)
	}
}

// TestApiKeyAuth_RoleLookupFailsClosedToMember pins T-39-07: a role-lookup
// error (DB hiccup or missing users row) must NOT 500 — auth succeeds and
// the caller is degraded to "member" (least privilege, never escalation).
func TestApiKeyAuth_RoleLookupFailsClosedToMember(t *testing.T) {
	raw := "gpuai_live_" + "abcdefghijklmnopqrstuvwx"
	hash := public.HashKey(raw)
	recent := time.Now() // suppress the touch goroutine; not under test here
	stub := &stubLookup{
		keyHash: hash,
		row: &db.APIKey{
			APIKeyID: "id-rolefail", OrgID: "o", UserID: "u",
			KeyHash: hash, KeyPrefix: "gpuai_live_role", Last4: "fail",
			Scopes:     json.RawMessage(`{"full_access":true}`),
			LastUsedAt: &recent,
		},
		roleErr: errors.New("db hiccup"),
	}
	for name, mw := range map[string]func(public.ApiKeyLookup) func(http.Handler) http.Handler{
		"problem-json": public.ApiKeyAuthMiddleware,
	} {
		t.Run(name, func(t *testing.T) {
			var seen *public.APIKey
			h := mw(stub)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = public.APIKeyFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest("GET", "/v1/instances", nil)
			req.Header.Set("Authorization", "Bearer "+raw)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("role-lookup failure must not break auth: got %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if seen == nil || seen.Role != "member" {
				t.Errorf("Role must fail closed to member on lookup error: got %+v", seen)
			}
		})
	}
}

// TestApiKeyAuth_EmptyRoleCoercedToMember asserts the belt-and-braces guard:
// an empty role string (impossible from the v29-constrained DB, possible from
// a stub or future regression) is coerced to "member" rather than attached
// verbatim.
func TestApiKeyAuth_EmptyRoleCoercedToMember(t *testing.T) {
	raw := "gpuai_live_" + "abcdefghijklmnopqrstuvwx"
	hash := public.HashKey(raw)
	recent := time.Now()
	stub := &stubLookup{
		keyHash: hash,
		row: &db.APIKey{
			APIKeyID: "id-emptyrole", OrgID: "o", UserID: "u",
			KeyHash: hash, KeyPrefix: "gpuai_live_role", Last4: "mpty",
			Scopes:     json.RawMessage(`{"full_access":true}`),
			LastUsedAt: &recent,
		},
		role: "", // zero value on purpose
	}
	var seen *public.APIKey
	h := public.ApiKeyAuthMiddleware(stub)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = public.APIKeyFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/v1/instances", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if seen == nil || seen.Role != "member" {
		t.Errorf("empty role must coerce to member: got %+v", seen)
	}
}

func TestApiKeyAuth_InternalErrorOnLookupFailure(t *testing.T) {
	h := public.ApiKeyAuthMiddleware(&stubLookup{err: errors.New("db down")})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("downstream should not run")
	}))
	req := httptest.NewRequest("GET", "/v1/instances", nil)
	raw := "gpuai_live_" + "abcdefghijklmnopqrstuvwx"
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", w.Code)
	}
}
