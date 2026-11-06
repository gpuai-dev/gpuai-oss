package public

import (
	"encoding/json"
	"net/http"
)

// Problem is the RFC 7807 application/problem+json response shape (D-10).
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Code      string `json:"code"`       // stable string from codeRegistry
	RequestID string `json:"request_id"` // X-Request-Id (D-12)
}

// codeRegistry is the single source of truth for stable error codes.
// Adding a new code requires adding a row here AND in openapi/v1.yaml's
// ErrorCode schema (Plan 18-10). The registry-coherence test
// (TestCodeRegistryCoherence) ensures handlers cannot drift.
var codeRegistry = map[string]int{
	"unauthenticated":      http.StatusUnauthorized,
	"invalid_api_key":      http.StatusUnauthorized,
	"insufficient_scope":   http.StatusForbidden,
	"not_found":            http.StatusNotFound,
	"idempotency_conflict": http.StatusConflict,
	"idempotency_mismatch": http.StatusUnprocessableEntity,
	"validation_failed":    http.StatusUnprocessableEntity,
	"invalid_gpu_type":     http.StatusUnprocessableEntity,
	"disk_unavailable":     http.StatusUnprocessableEntity,
	// disk_exceeds_offering: the pinned offering CAN configure disk, just not
	// as much as requested (its displayed storage_gb is smaller) — distinct
	// from disk_unavailable so clients can steer "lower disk_gb" vs "pick a
	// configurable machine".
	"disk_exceeds_offering": http.StatusUnprocessableEntity,
	"model_too_large":       http.StatusUnprocessableEntity,
	// gpu_vram_too_small: the requested template declares a per-GPU VRAM floor
	// the explicitly named gpu_type cannot meet — the template's OWN
	// declaration says this combination cannot serve, so it is a hard block
	// rather than a warning (D-2026-08-31). Not retryable as-sent: the same
	// request fails identically forever, so an SDK backoff loop can only spin.
	// The detail names both numbers and the compatible GPU types, so the caller
	// can fix it in one edit instead of guessing the next type.
	"gpu_vram_too_small": http.StatusUnprocessableEntity,
	// price_changed: upstream repriced above what this launch was approved for
	// (the caller's max_price_per_hour, or the quoted price of the capacity
	// when none was sent). Nothing was provisioned and nothing was billed.
	// Distinct from operation_failed because it is NOT retryable as-sent — the
	// same request will fail identically until the caller re-prices against
	// the new number, so an SDK backoff loop can only spin. 409: the request
	// conflicts with the current state of the market, not with its own shape.
	"price_changed":        http.StatusConflict,
	"insufficient_balance": http.StatusPaymentRequired,
	"quota_exceeded":       http.StatusPaymentRequired,
	"rate_limited":         http.StatusTooManyRequests,
	// creator_check_unavailable: the watch-rule re-enable path could not
	// determine whether the rule's creator is still reachable — the seam
	// errored, or it is not wired. It means "could not tell", never "gone",
	// and that distinction is the whole reason it is its own code: answering a
	// 422 on an inconclusive check would refuse a legitimate re-enable and
	// tell the customer their creator is invalid. 503 says retry.
	//
	// Registered here (plan 86-23) for the code plan 86-22 introduced. Note
	// that four other 503s in this package are still unregistered; this row is
	// the shape they should follow, not an exception to a rule.
	"creator_check_unavailable": http.StatusServiceUnavailable,
	"internal_error":            http.StatusInternalServerError,
	"operation_failed":          http.StatusInternalServerError,
}

// writeProblem emits an RFC 7807 problem-details response with the public-API
// type URI prefix and request-id propagation. The status argument is the
// canonical status code; callers MUST match it to a registered code (the
// registry-coherence test enforces).
func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	rid := RequestIDFromContext(r.Context())
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{
		Type:      "https://api.gpu.ai/errors/" + code,
		Title:     http.StatusText(status),
		Status:    status,
		Detail:    detail,
		Code:      code,
		RequestID: rid,
	})
}

// writeJSON encodes v as the response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// CodeRegistry exposes a copy of the registered code → status map for tests.
// Returns a copy so callers cannot mutate the package-private state.
func CodeRegistry() map[string]int {
	m := make(map[string]int, len(codeRegistry))
	for k, v := range codeRegistry {
		m[k] = v
	}
	return m
}
