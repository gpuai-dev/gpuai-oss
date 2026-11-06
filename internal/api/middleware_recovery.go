package api

import (
	"log/slog"
	"net/http"
)

// RecoveryMiddleware is the outer panic backstop for the HTTP request path. A
// panic anywhere in a handler is recovered here, logged, and turned into a
// clean RFC 7807 500 instead of crashing the process or leaking a stack trace
// to the client.
//
// Composition (set up in Server.Handler, outer to inner):
//
//	RequestLog -> RecoveryMiddleware -> dispatch
//
// RecoveryMiddleware sits INSIDE RequestLogMiddleware so the 500 it writes flows
// back through statusRecorder and RequestLogMiddleware logs the crashed request
// exactly once at LevelError. It does NOT re-panic: the request goroutine
// returns cleanly after writing the 500, keeping the process alive.
func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// http.ErrAbortHandler is the stdlib's sentinel for a handler that
				// deliberately aborts (e.g. a hijacked/closed connection). Re-panic
				// it so the server's own machinery handles it as intended rather
				// than masking it with a 500.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}

				slog.Error("recovered panic in HTTP handler",
					"method", r.Method,
					"path", r.URL.Path,
					"panic", rec,
				)

				// Clean RFC 7807 500 — same envelope as the rest of internal/api
				// (writeProblem). Best-effort: if the handler already wrote a
				// header, WriteHeader is a no-op and we just stop.
				writeProblem(w, http.StatusInternalServerError, "internal", "internal server error")
			}
		}()

		next.ServeHTTP(w, r)
	})
}
