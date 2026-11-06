package api

// Wave-0 RED test scaffold (Phase 52, BUG-01). Asserts the behavior plan 52-01
// must satisfy: a panic inside a handler wrapped by RecoveryMiddleware yields a
// clean HTTP 500 and never crashes the process. Runs against the Wave-0
// pass-through placeholder in middleware_recovery.go and is RED until 52-01.
// Do NOT "fix" the placeholder to make this pass — 52-01 owns GREEN.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSentryRecovery_Returns500 wraps a panicking handler with
// RecoveryMiddleware and asserts rec.Code == 500. The Wave-0 placeholder does
// NOT recover, so the panic escapes ServeHTTP. We trap that escape with a
// deferred recover so the test suite survives, and record it as a failure —
// which is exactly the RED signal 52-01 turns green.
func TestSentryRecovery_Returns500(t *testing.T) {
	// Sanity: a non-panicking handler passes through untouched.
	t.Run("passthrough_ok", func(t *testing.T) {
		h := RecoveryMiddleware(okHandler())
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("passthrough: expected 200, got %d", rec.Code)
		}
	})

	t.Run("panic_returns_500", func(t *testing.T) {
		panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("boom")
		})
		h := RecoveryMiddleware(panicHandler)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/instances", nil)
		rec := httptest.NewRecorder()

		// Trap an escaped panic so the placeholder (no recover) fails this test
		// instead of crashing the whole suite. The real RecoveryMiddleware
		// recovers internally, so this deferred recover never fires in GREEN.
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic escaped RecoveryMiddleware (no recovery): %v — RED until 52-01", r)
				}
			}()
			h.ServeHTTP(rec, req)
		}()

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("expected 500 on panic, got %d — RED until 52-01", rec.Code)
		}
	})
}
