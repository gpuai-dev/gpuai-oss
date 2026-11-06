package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// okHandler is a simple handler that writes 200 + "ok" body.
// Used as the "next" handler in middleware tests.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

// --- LocalhostOnly tests ---

func TestLocalhostOnly_AllowsLoopbackIPv4(t *testing.T) {
	handler := LocalhostOnly(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("expected body 'ok', got %q", rec.Body.String())
	}
}

func TestLocalhostOnly_AllowsLoopbackIPv6(t *testing.T) {
	handler := LocalhostOnly(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = net.JoinHostPort("::1", "12345")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("expected body 'ok', got %q", rec.Body.String())
	}
}

func TestLocalhostOnly_RejectsExternalIPv4(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := LocalhostOnly(next)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "192.168.1.100:12345"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
	if called {
		t.Error("next handler should not have been called for external IP")
	}
}

func TestLocalhostOnly_RejectsExternalIPv6(t *testing.T) {
	handler := LocalhostOnly(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "[2001:db8::1]:12345"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestLocalhostOnly_RejectsMalformedAddr(t *testing.T) {
	handler := LocalhostOnly(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "garbage"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

// --- InternalAuthMiddleware regression tests ---

func TestInternalAuthMiddleware_ValidToken(t *testing.T) {
	token := "test-secret-token"
	handler := InternalAuthMiddleware(token, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("expected body 'ok', got %q", rec.Body.String())
	}
}

func TestInternalAuthMiddleware_InvalidToken(t *testing.T) {
	token := "test-secret-token"
	handler := InternalAuthMiddleware(token, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

// --- InstanceTokenAuth tests ---
// These test cases exercise the header-parsing and early-rejection paths
// that do not require a database connection. Cases that reach the DB lookup
// (valid token, invalid token, instance not found) are covered by
// integration tests.

func TestInstanceTokenAuth_MissingInstanceID(t *testing.T) {
	// Pass nil dbPool -- the middleware should reject before hitting the DB.
	handler := InstanceTokenAuth(nil, okHandler())

	req := httptest.NewRequest(http.MethodPost, "/internal/instances//ready", nil)
	// PathValue("id") returns "" when there's no {id} match.
	// Since we're not using a real mux, PathValue will return "".
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestInstanceTokenAuth_MissingAuthorizationHeader(t *testing.T) {
	handler := InstanceTokenAuth(nil, okHandler())

	req := httptest.NewRequest(http.MethodPost, "/internal/instances/{id}/ready", nil)
	req.SetPathValue("id", "inst-123")
	// No Authorization header set.
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestInstanceTokenAuth_InvalidAuthorizationFormat(t *testing.T) {
	handler := InstanceTokenAuth(nil, okHandler())

	req := httptest.NewRequest(http.MethodPost, "/internal/instances/{id}/ready", nil)
	req.SetPathValue("id", "inst-123")
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz") // Not Bearer
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}
