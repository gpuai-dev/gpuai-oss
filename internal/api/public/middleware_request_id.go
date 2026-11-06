package public

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// requestIDKey is the unexported context key for X-Request-Id values.
type requestIDKey struct{}

// WithRequestID returns a child context carrying the supplied request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext returns the request id stored in ctx, or "" if absent.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

// RequestIDMiddleware honors a caller-supplied X-Request-Id header (up to
// 128 chars) or mints a fresh one. The id is set on the response header
// (D-12) and injected into the request context so handlers and writeProblem
// can include it in payload bodies.
//
// Legacy /api/* + /v1/* (non-inference) callers use this entry point; ids
// are 32-char hex. The inference chain (Plan 20-07) uses
// RequestIDMiddlewareWithPrefix("gpuai_inf_") instead so OpenAI SDKs see
// the gpuai_inf_ prefix in error messages (SERV-09).
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if len(id) == 0 || len(id) > 128 {
			id = mintRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// mintRequestID generates a 16-byte hex random id (32 chars).
// Falls back to a fixed prefix on the unlikely rand.Read failure so the
// header is always populated.
func mintRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "req-fallback"
	}
	return hex.EncodeToString(b)
}

// RequestIDMiddlewareWithPrefix returns middleware that always mints a
// fresh id with the given prefix — caller-supplied X-Request-Id headers
// are IGNORED so the prefix invariant cannot be broken by clients.
// SERV-09: inference routes use prefix "gpuai_inf_" so OpenAI SDKs surface
// the id with our distinct namespace.
func RequestIDMiddlewareWithPrefix(prefix string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := prefix + mintRequestID()
			w.Header().Set("X-Request-Id", id)
			ctx := WithRequestID(r.Context(), id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
