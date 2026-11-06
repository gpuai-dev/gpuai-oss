package public

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
)

// Operation is the public-API operation response shape (D-08, D-09).
//
// Privacy contract (PRIV-08): no api_key_id, no org_id, no metadata.
// Metadata contains the original request body (snapshotted on
// CreateOperation) which may include MaxPricePerHour or other fields the
// caller already has — we deliberately do NOT round-trip it on the wire
// because the schema's `metadata JSONB` could grow into upstream-leaking
// territory in future plans. Surface only the FSM-relevant fields.
//
// `Error` is omitted on succeeded/in_progress/pending and populated on
// failed/cancelled. `ResourceID` is omitted until the engine attaches
// the instance_id (typically right after the upstream provider returns).
//
// `CompletedAt` is the timestamp at which a terminal state was first
// reached. Non-terminal operations (`pending`, `in_progress`) emit
// `completed_at: null` via omitempty so SDKs can distinguish "still
// running" from "completed at unknown time".
type Operation struct {
	OperationID string        `json:"operation_id"`
	Kind        string        `json:"kind"`
	State       string        `json:"state"`
	ResourceID  *string       `json:"resource_id,omitempty"`
	Error       *OperationErr `json:"error,omitempty"`
	// Result is the terminal-success payload. For a template deploy it carries
	// the non-secret app coordinates (app_basic_auth_user/app_url/terminal_url).
	// The basic-auth PASSWORD is stripped at read time (GPU-59) — the stored
	// row may still carry app_basic_auth_pass (pre-GPU-59 writes and the
	// create-FSM's persist both do), but operationToResponse removes the key
	// before it reaches the wire. Clients retrieve the password via
	// GET /v1/instances/{id}?include=credentials instead. omitempty so
	// non-template operations — and reads where the stored result is NULL —
	// emit no `result` key.
	Result      json.RawMessage `json:"result,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	// Warnings are create-time advisories (e.g. the model-size guard failing
	// open on a gated Hugging Face repo, GPU-40). Set ONLY on the synchronous
	// 202 from POST /v1/instances — never persisted, so GET /v1/operations/{id}
	// reads omit the field.
	Warnings []string `json:"warnings,omitempty"`
}

// OperationErr is the on-failure body. Code is a stable string from the
// public.codeRegistry (see errors.go). Detail is a customer-safe sentence
// derived from the engine error (customerOperationDetail) — never the raw
// engine/supplier text, which stays in operations.error_detail and the logs.
// SDKs render it as the human-readable reason.
type OperationErr struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// operationToResponse maps a db.Operation row to the public wire shape.
// Excludes api_key_id, org_id, metadata, version (FSM-internal). Error
// pointer is populated only when ErrorCode is non-nil — pending/succeeded
// operations omit the error envelope entirely. The result is passed through
// redactOperationResult so app_basic_auth_pass never reaches the wire (GPU-59).
func operationToResponse(o *db.Operation) Operation {
	out := Operation{
		OperationID: o.OperationID,
		Kind:        o.Kind,
		State:       o.State,
		ResourceID:  o.ResourceID,
		Result:      redactOperationResult(o.OperationID, o.Result),
		CreatedAt:   o.CreatedAt,
		UpdatedAt:   o.UpdatedAt,
		CompletedAt: o.CompletedAt,
	}
	if o.ErrorCode != nil {
		out.Error = &OperationErr{Code: *o.ErrorCode}
		raw := ""
		if o.ErrorDetail != nil {
			raw = *o.ErrorDetail
		}
		out.Error.Detail = customerOperationDetail(o.Kind, raw)
	}
	return out
}

// Customer-facing detail for a failed instance.delete. The engine's terminate
// error names the supplier ("terminate: provider <name>: …"), and the only
// thing the customer can act on is that we keep retrying.
const msgDeleteFailed = "We couldn't confirm the instance was torn down yet. We'll keep retrying the teardown automatically — check the instance status shortly, and contact support if it stays in error."

// customerOperationDetail turns the stored engine error (operations.
// error_detail — an INTERNAL column that keeps the full operator detail) into
// the customer-safe Operation.Error.Detail.
//
// GPU-61 used to scrub only the provider brand and pass the rest through, but
// the rest is the supplier's raw HTTP body: the 2026-09-22 "HTTP 400:
// {"error":"insufficient_credit",…}" and another supplier's "Your account
// balance is too low to rent a pod" read as the CUSTOMER's balance being
// empty. So the raw
// string is now never shown: provision.CustomerFailureMessageFromText maps it
// to a fixed sentence per failure class (supplier-unfunded / capacity /
// price-changed / our own sentence / generic). Rendering at read time also
// fixes every historical row. scrubProviderNames still runs last as a
// belt-and-braces guard.
func customerOperationDetail(kind, raw string) string {
	msg := msgDeleteFailed
	if kind != "instance.delete" {
		_, msg = provision.CustomerFailureMessageFromText(raw)
	}
	return scrubProviderNames(msg)
}

// redactOperationResult strips the app basic-auth password from a stored
// operation result before it reaches the wire (GPU-59). Map-based so every
// other (unknown/future) result key survives untouched — historical rows
// written before GPU-59 are covered because the strip happens at read time.
// Fails CLOSED: a result that cannot be parsed is dropped entirely rather
// than passed through unstripped.
func redactOperationResult(operationID string, result json.RawMessage) json.RawMessage {
	if len(result) == 0 {
		return result
	}
	var m map[string]any
	if err := json.Unmarshal(result, &m); err != nil {
		slog.Error("operations: result unmarshal failed during credential redaction; dropping result",
			"operation_id", operationID, "error", err.Error())
		return nil
	}
	if _, ok := m["app_basic_auth_pass"]; !ok {
		return result
	}
	delete(m, "app_basic_auth_pass")
	stripped, err := json.Marshal(m)
	if err != nil {
		slog.Error("operations: result re-marshal failed during credential redaction; dropping result",
			"operation_id", operationID, "error", err.Error())
		return nil
	}
	return stripped
}

// operationStore is the narrow DB seam GET /v1/operations/{id} consumes.
// *db.Pool satisfies it; tests substitute an in-memory stub via
// PublicDepsWithOperationStore — same pattern as sshKeyDeps.
//
// Kept package-private so external packages cannot fabricate the type;
// tests use the exported helper to swap implementations.
type operationStore interface {
	CreateOperation(ctx context.Context, o *db.Operation) error
	GetOperation(ctx context.Context, operationID, apiKeyID string) (*db.Operation, error)
	UpdateOperationState(ctx context.Context, operationID string, prevVersion int, newState string) (int, error)
	SetOperationResource(ctx context.Context, operationID string, prevVersion int, resourceID string) (int, error)
	SetOperationResult(ctx context.Context, operationID string, prevVersion int, result json.RawMessage) (int, error)
	SetOperationFailure(ctx context.Context, operationID string, prevVersion int, code, detail string) (int, error)
}

// resolveOperationStore returns the test override when present, otherwise
// falls through to deps.DB which already implements operationStore.
func (deps PublicDeps) resolveOperationStore() operationStore {
	if deps.opStore != nil {
		return deps.opStore
	}
	return deps.DB
}

// handleGetOperation serves GET /v1/operations/{id}. Issuer-scoped: only
// the api_key that issued the operation can read it. Cross-key access
// returns 404 (matches PRIV-08 / D-07 idempotent-destroy semantics — we
// must not leak existence to other keys in the same org).
func (deps PublicDeps) handleGetOperation(w http.ResponseWriter, r *http.Request) {
	apiKey := APIKeyFromContext(r.Context())
	if apiKey == nil {
		WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	id := r.PathValue("id")
	store := deps.resolveOperationStore()
	op, err := store.GetOperation(r.Context(), id, apiKey.APIKeyID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			WriteProblem(w, r, http.StatusNotFound, "not_found", "Operation not found")
			return
		}
		slog.ErrorContext(r.Context(), "operations: get failed", "error", err.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to fetch operation")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(operationToResponse(op))
}
