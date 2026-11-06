package public

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestRequestIDFromContext_Empty(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestRequestIDMiddleware_MintsWhenAbsent(t *testing.T) {
	var seenID string
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	respID := w.Header().Get("X-Request-Id")
	if respID == "" {
		t.Fatal("X-Request-Id header missing on response")
	}
	if seenID != respID {
		t.Fatalf("context id %q != header id %q", seenID, respID)
	}
	// 16 random bytes hex-encoded = 32 chars
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(respID) {
		t.Fatalf("minted id %q does not match 32-hex pattern", respID)
	}
}

func TestRequestIDMiddleware_HonorsCallerHeader(t *testing.T) {
	var seenID string
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = RequestIDFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("X-Request-Id", "caller-supplied-id")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Header().Get("X-Request-Id") != "caller-supplied-id" {
		t.Fatalf("response header: got %q, want caller-supplied-id", w.Header().Get("X-Request-Id"))
	}
	if seenID != "caller-supplied-id" {
		t.Fatalf("context id: got %q, want caller-supplied-id", seenID)
	}
}

func TestRequestIDMiddleware_RejectsOverlongHeader(t *testing.T) {
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'a'
	}
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("X-Request-Id", string(long))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	respID := w.Header().Get("X-Request-Id")
	if respID == string(long) {
		t.Fatal("middleware echoed back overlong caller id; should have minted fresh")
	}
	if len(respID) != 32 {
		t.Fatalf("expected freshly-minted 32-char id, got len %d", len(respID))
	}
}
