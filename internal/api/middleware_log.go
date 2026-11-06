package api

import (
	"log/slog"
	"net/http"
	"time"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	return sr.ResponseWriter.Write(b)
}

// Flush preserves http.Flusher through the wrap. Without it, every SSE
// handler's `w.(http.Flusher)` assertion fails behind this middleware and
// /api/v1/events (org + per-instance streams) 500s with sse-not-supported.
func (sr *statusRecorder) Flush() {
	if sr.status == 0 {
		// Flushing headers implicitly commits a 200, same as Write.
		sr.status = http.StatusOK
	}
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer's
// optional interfaces (Flush, deadlines, Hijack) through the wrap.
func (sr *statusRecorder) Unwrap() http.ResponseWriter {
	return sr.ResponseWriter
}

// RequestLogMiddleware logs every HTTP request with method, path, status, duration, and remote address.
func RequestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(sr, r)

		if sr.status == 0 {
			sr.status = http.StatusOK
		}
		duration := time.Since(start)

		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sr.status),
			slog.Duration("duration", duration),
			slog.String("remote_addr", r.RemoteAddr),
		}

		level := slog.LevelInfo
		if sr.status >= 500 {
			level = slog.LevelError
		} else if sr.status >= 400 {
			level = slog.LevelWarn
		}

		slog.LogAttrs(r.Context(), level, "http request", attrs...)
	})
}
