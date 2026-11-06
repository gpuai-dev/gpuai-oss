package public_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/api/public"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
)

// TestHandleGetOperation_RequiresAPIKey pins the 401 short-circuit when
// no APIKey is on the request context (a defense-in-depth guard, since
// production-path requests get the APIKey populated by ApiKeyAuthMiddleware
// before reaching the handler).
func TestHandleGetOperation_RequiresAPIKey(t *testing.T) {
	deps := public.PublicDepsWithOperationStore(public.PublicDeps{}, newStubOperationStore())

	req := httptest.NewRequest(http.MethodGet, "/v1/operations/op-1", nil)
	mux := http.NewServeMux()
	public.InstanceHandlersForTesting(mux, deps)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleGetOperation_HappyPath round-trips a created operation and
// asserts the wire shape matches the public.Operation contract — kind,
// state, created_at all populated; api_key_id / metadata absent.
func TestHandleGetOperation_HappyPath(t *testing.T) {
	opStore := newStubOperationStore()
	deps := public.PublicDepsWithOperationStore(public.PublicDeps{}, opStore)

	// Seed an operation under api_key=ak-1.
	op := &db.Operation{APIKeyID: "ak-1", OrgID: "org-A", Kind: "instance.create"}
	if err := opStore.CreateOperation(context.Background(), op); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := invokeInstance(t, deps, http.MethodGet, "/v1/operations/"+op.OperationID, "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got public.Operation
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OperationID != op.OperationID {
		t.Errorf("operation_id: got %q, want %q", got.OperationID, op.OperationID)
	}
	if got.Kind != "instance.create" {
		t.Errorf("kind: got %q, want instance.create", got.Kind)
	}
	if got.State != "pending" {
		t.Errorf("state: got %q, want pending", got.State)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("created_at: zero")
	}
	if got.Error != nil {
		t.Errorf("error: got %+v on pending state, want nil", got.Error)
	}

	// Privacy: response body must NOT include api_key_id, org_id, metadata, or version.
	body := w.Body.String()
	for _, banned := range []string{"api_key_id", "org_id", "metadata", "version"} {
		if jsonContainsKey(body, banned) {
			t.Errorf("response leaks %q: %s", banned, body)
		}
	}
}

// TestHandleGetOperation_FailedShape pins the failure-path wire shape:
// state="failed" + Error{code, detail}.
func TestHandleGetOperation_FailedShape(t *testing.T) {
	opStore := newStubOperationStore()
	deps := public.PublicDepsWithOperationStore(public.PublicDeps{}, opStore)

	op := &db.Operation{APIKeyID: "ak-1", OrgID: "org-A", Kind: "instance.create"}
	if err := opStore.CreateOperation(context.Background(), op); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := opStore.SetOperationFailure(context.Background(), op.OperationID, op.Version,
		"operation_failed", "engine: synthetic failure"); err != nil {
		t.Fatalf("seed failure: %v", err)
	}

	w := invokeInstance(t, deps, http.MethodGet, "/v1/operations/"+op.OperationID, "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got public.Operation
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != "failed" {
		t.Errorf("state: got %q, want failed", got.State)
	}
	if got.Error == nil {
		t.Fatalf("error: got nil, want populated on failed state")
	}
	if got.Error.Code != "operation_failed" {
		t.Errorf("error.code: got %q, want operation_failed", got.Error.Code)
	}
	// The raw engine string stays in operations.error_detail; the wire carries
	// the customer-safe sentence for its class (an unclassifiable failure ->
	// the generic sentence), never the engine text.
	if got.Error.Detail != provision.MsgGeneric {
		t.Errorf("error.detail: got %q, want the generic customer sentence", got.Error.Detail)
	}
	if got.CompletedAt == nil || got.CompletedAt.IsZero() {
		t.Errorf("completed_at: got %v, want populated on failed", got.CompletedAt)
	}
}

// TestHandleGetOperation_PrivacyAcrossKinds runs the privacy guard for a
// representative shape (instance.delete with resource_id populated) so
// the wire surface is covered for both kinds in v1.
func TestHandleGetOperation_PrivacyAcrossKinds(t *testing.T) {
	opStore := newStubOperationStore()
	deps := public.PublicDepsWithOperationStore(public.PublicDeps{}, opStore)

	op := &db.Operation{
		APIKeyID: "ak-1", OrgID: "org-A", Kind: "instance.delete",
		Metadata: json.RawMessage(`{"upstream_provider":"vultr","internal_token":"super-secret"}`),
	}
	if err := opStore.CreateOperation(context.Background(), op); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := opStore.SetOperationResource(context.Background(), op.OperationID, op.Version, "gpu-priv"); err != nil {
		t.Fatalf("seed resource: %v", err)
	}

	w := invokeInstance(t, deps, http.MethodGet, "/v1/operations/"+op.OperationID, "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Even though metadata was seeded with a "leaky" payload, the wire
	// shape must NOT round-trip it — Operation has no metadata field.
	for _, banned := range []string{"upstream_provider", "internal_token", "super-secret", "vultr"} {
		if contains(body, banned) {
			t.Errorf("Operation JSON leaks %q: %s", banned, body)
		}
	}
}

// jsonContainsKey returns true when JSON contains "<key>": as a string
// (cheap heuristic — adequate for the privacy guard since wire JSON in
// our tests is a single line).
func jsonContainsKey(body, key string) bool {
	return contains(body, "\""+key+"\":")
}

// contains is a thin wrapper around strings.Contains so the test file
// doesn't need to import "strings" twice (once is already in
// handlers_instances_test.go but separate compilation units in the same
// package share imports).
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// suppress unused-time import compile warnings in the future when
// fixtures grow. time is currently used inside the stubs from
// handlers_instances_test.go which share the package; pin the import
// to avoid go vet drift if this file is read in isolation.
var _ = time.Now

// TestHandleGetOperation_SupplierUnfundedIsCustomerSafe replays the exact
// 2026-09-22 prod failure: the stored detail names the supplier and carries
// its raw "insufficient_credit" body. The wire must carry neither — only the
// supplier-unfunded sentence that says it is not the customer's balance.
func TestHandleGetOperation_SupplierUnfundedIsCustomerSafe(t *testing.T) {
	opStore := newStubOperationStore()
	deps := public.PublicDepsWithOperationStore(public.PublicDeps{}, opStore)

	op := &db.Operation{APIKeyID: "ak-1", OrgID: "org-A", Kind: "instance.create"}
	if err := opStore.CreateOperation(context.Background(), op); err != nil {
		t.Fatalf("seed: %v", err)
	}
	raw := `provision: all providers failed (last: vultr): create instance: HTTP 400: {"error":"insufficient_credit","msg":"Your account lacks credit; see the billing page."}`
	if _, err := opStore.SetOperationFailure(context.Background(), op.OperationID, op.Version,
		"operation_failed", raw); err != nil {
		t.Fatalf("seed failure: %v", err)
	}

	w := invokeInstance(t, deps, http.MethodGet, "/v1/operations/"+op.OperationID, "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, bad := range []string{"vultr", "insufficient_credit", "lacks credit", "HTTP 400", "all providers failed"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(bad)) {
			t.Errorf("wire body leaks %q: %s", bad, body)
		}
	}
	var got public.Operation
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != "operation_failed" {
		t.Fatalf("error: got %+v, want code operation_failed", got.Error)
	}
	if got.Error.Detail != provision.MsgSupplierUnfunded {
		t.Errorf("error.detail: got %q, want the supplier-unfunded sentence", got.Error.Detail)
	}
}

// A failed delete names the supplier in the engine error ("terminate:
// provider <name>: …"); the wire carries the fixed teardown sentence.
func TestHandleGetOperation_DeleteFailureIsCustomerSafe(t *testing.T) {
	opStore := newStubOperationStore()
	deps := public.PublicDepsWithOperationStore(public.PublicDeps{}, opStore)

	op := &db.Operation{APIKeyID: "ak-1", OrgID: "org-A", Kind: "instance.delete"}
	if err := opStore.CreateOperation(context.Background(), op); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := opStore.SetOperationFailure(context.Background(), op.OperationID, op.Version,
		"operation_failed", `terminate: provider vultr: HTTP 409: {"message":"busy"}`); err != nil {
		t.Fatalf("seed failure: %v", err)
	}
	w := invokeInstance(t, deps, http.MethodGet, "/v1/operations/"+op.OperationID, "org-A", "user-A", "ak-1", nil)
	body := strings.ToLower(w.Body.String())
	for _, bad := range []string{"vultr", "http 409", "busy"} {
		if strings.Contains(body, bad) {
			t.Errorf("wire body leaks %q: %s", bad, body)
		}
	}
	if !strings.Contains(body, "torn down") {
		t.Errorf("want the teardown sentence, got %s", body)
	}
}
