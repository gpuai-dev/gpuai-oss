package public

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// PublicIdempotencyDeps abstracts the DB methods required by the public-API
// idempotency middleware. Backed in production by *db.Pool; backed in tests
// by an in-memory stub (see middleware_idempotency_test.go).
type PublicIdempotencyDeps interface {
	GetPublicIdempotencyKey(ctx context.Context, apiKeyID, key string) (*db.PublicIdempotencyKey, error)
	CreatePublicIdempotencyKey(ctx context.Context, apiKeyID, key, requestHash string) error
	CompletePublicIdempotencyKey(ctx context.Context, apiKeyID, key string, code int, body []byte, headers json.RawMessage) error
}

// publicResponseCapture wraps http.ResponseWriter to record the status code,
// body, and any preserved headers so the response can be persisted on
// completion and replayed on retry.
type publicResponseCapture struct {
	http.ResponseWriter
	statusCode int
	body       bytes.Buffer
	written    bool
}

func (rc *publicResponseCapture) WriteHeader(code int) {
	rc.statusCode = code
	rc.written = true
	rc.ResponseWriter.WriteHeader(code)
}

func (rc *publicResponseCapture) Write(b []byte) (int, error) {
	rc.body.Write(b)
	if !rc.written {
		rc.statusCode = http.StatusOK
		rc.written = true
	}
	return rc.ResponseWriter.Write(b)
}

// PublicIdempotencyMiddleware enforces D-11: Stripe-compatible 24h dedup of
// POST/PATCH/DELETE writes scoped per (api_key_id, idempotency_key).
//
// Behavior:
//   - No Idempotency-Key header: pass through.
//   - No APIKey on context: pass through (auth middleware enforces upstream).
//   - Key not in DB: eager-INSERT (Pattern 2), run handler, persist captured
//     response (status + body + Operation-Id header).
//   - Key exists with response_code set + matching request_hash: replay the
//     stored response byte-for-byte (status, body, Operation-Id).
//   - Key exists with response_code set + mismatched request_hash: 422
//     idempotency_mismatch.
//   - Key exists with response_code NULL (in-flight): 409 idempotency_conflict.
//   - Concurrent retry race (CreatePublicIdempotencyKey returns
//     ErrIdempotencyKeyExists): re-read and decide between replay / 409 / 422.
//
// Mirrors the Eager Creation pattern from internal/api/idempotency.go but
// scoped per api_key_id (not org_id) and using the public-API writeProblem
// + error-code registry.
func PublicIdempotencyMiddleware(deps PublicIdempotencyDeps) func(http.Handler) http.Handler {
	return publicIdempotencyMiddleware(deps)
}

func publicIdempotencyMiddleware(deps PublicIdempotencyDeps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Nil-deps guard (WR-12): without it, a request that DOES carry an
			// Idempotency-Key would dereference a nil interface below — a
			// customer-triggerable panic in any wiring that leaves Idempotency
			// unset. With nil deps the layer is a true no-op.
			if deps == nil {
				next.ServeHTTP(w, r)
				return
			}
			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			apiKey := APIKeyFromContext(r.Context())
			if apiKey == nil {
				// Defensive — auth middleware should have already rejected.
				next.ServeHTTP(w, r)
				return
			}

			// Read + hash request body, then reset for the handler.
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				writeProblem(w, r, http.StatusBadRequest, "validation_failed",
					"Failed to read request body")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			hash := sha256.Sum256(bodyBytes)
			requestHash := hex.EncodeToString(hash[:])

			// Lookup existing.
			existing, err := deps.GetPublicIdempotencyKey(r.Context(), apiKey.APIKeyID, key)
			if err != nil && !errors.Is(err, db.ErrNotFound) {
				slog.ErrorContext(r.Context(), "idempotency lookup failed",
					"api_key_id", apiKey.APIKeyID,
					"error", err.Error(),
				)
				writeProblem(w, r, http.StatusInternalServerError, "internal_error",
					"Idempotency check failed")
				return
			}
			if existing != nil {
				if existing.ResponseCode != nil {
					if existing.RequestHash != requestHash {
						writeProblem(w, r, http.StatusUnprocessableEntity, "idempotency_mismatch",
							"Idempotency key reused with a different request body")
						return
					}
					replayCaptured(w, *existing.ResponseCode, existing.ResponseBody, existing.ResponseHeaders)
					return
				}
				// In-flight — another request holds this key.
				writeProblem(w, r, http.StatusConflict, "idempotency_conflict",
					"A request with this Idempotency-Key is already in progress")
				return
			}

			// Eager Create (Pattern 2). On 23505 race, re-read.
			if err := deps.CreatePublicIdempotencyKey(r.Context(), apiKey.APIKeyID, key, requestHash); err != nil {
				if errors.Is(err, db.ErrIdempotencyKeyExists) {
					existing, _ = deps.GetPublicIdempotencyKey(r.Context(), apiKey.APIKeyID, key)
					if existing != nil && existing.ResponseCode != nil {
						if existing.RequestHash != requestHash {
							writeProblem(w, r, http.StatusUnprocessableEntity, "idempotency_mismatch",
								"Idempotency key reused with a different request body")
							return
						}
						replayCaptured(w, *existing.ResponseCode, existing.ResponseBody, existing.ResponseHeaders)
						return
					}
					writeProblem(w, r, http.StatusConflict, "idempotency_conflict",
						"A request with this Idempotency-Key is already in progress")
					return
				}
				slog.ErrorContext(r.Context(), "idempotency create failed",
					"api_key_id", apiKey.APIKeyID,
					"error", err.Error(),
				)
				writeProblem(w, r, http.StatusInternalServerError, "internal_error",
					"Idempotency check failed")
				return
			}

			// Capture response from downstream handler.
			capture := &publicResponseCapture{ResponseWriter: w}
			next.ServeHTTP(capture, r)

			// Persist Operation-Id (D-08) and any other domain-specific
			// headers we want to round-trip on replay. Only Operation-Id is
			// captured currently — bloating the JSON with every header would
			// cost storage for no clear benefit.
			hdrs := map[string]string{}
			if v := capture.Header().Get("Operation-Id"); v != "" {
				hdrs["Operation-Id"] = v
			}
			hdrJSON, _ := json.Marshal(hdrs)

			if err := deps.CompletePublicIdempotencyKey(
				r.Context(), apiKey.APIKeyID, key,
				capture.statusCode, capture.body.Bytes(), hdrJSON,
			); err != nil {
				slog.WarnContext(r.Context(), "idempotency complete failed (response already sent)",
					"api_key_id", apiKey.APIKeyID,
					"error", err.Error(),
				)
			}
		})
	}
}

// replayCaptured re-emits a previously-stored idempotency response. Sets
// Content-Type to application/json (the public API only ever returns JSON
// for write endpoints) and any preserved headers (Operation-Id, etc.).
func replayCaptured(w http.ResponseWriter, code int, body []byte, headers json.RawMessage) {
	if len(headers) > 0 {
		var hdrs map[string]string
		if err := json.Unmarshal(headers, &hdrs); err == nil {
			for k, v := range hdrs {
				w.Header().Set(k, v)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}
