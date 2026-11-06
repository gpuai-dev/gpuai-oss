// Tests for the HTTP-free create-instance path (D-28).
//
// The extraction in create_instance.go is only worth anything if the two
// callers stay identical. `TestCreateInstanceValidationParity` is the mechanism
// that keeps them so: it drives one request object down BOTH paths and fails if
// the status or the problem code diverges by so much as a token. A future gate
// added to the handler instead of to CreateInstance — which is exactly how the
// auto-launch firer would quietly stop enforcing it — turns red here rather
// than launching an unapproved instance at 3 a.m. (D-24, the GPU-55 lesson).
//
// `TestCreateInstanceBackgroundCallerNeedsNoRequest` pins the other half: the
// call shape the firer will use, with an APIKey synthesized in-process and no
// http.Request anywhere in sight.
//
// `TestCreateInstanceWithOperationID*` pin the operation-id seam (review B
// CR-01): a background caller that must record "this rule started this launch"
// before any money can move needs the id BEFORE the row exists, because this
// function provisions on a detached goroutine and its return value arrives too
// late to abort anything.
package public_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/api/public"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
)

// parityKey is the API key both paths run under. The HTTP path gets it via
// WithAPIKey on the request context; the direct path gets the same pointer as
// an argument — so a divergence can only come from the code under test, never
// from a differently-shaped caller identity.
func parityKey() *public.APIKey {
	return &public.APIKey{
		APIKeyID: "k-parity",
		OrgID:    "org-parity",
		UserID:   "user-parity",
		Role:     "member",
		Scopes:   json.RawMessage(`{"full_access":true}`),
	}
}

// TestCreateInstanceValidationParity drives identical invalid requests through
// handleCreateInstance and deps.CreateInstance and asserts both reject with the
// same status and the same problem code.
//
// The request is built ONCE per row as a CreateInstanceRequest and marshalled
// to JSON for the HTTP leg, rather than maintaining a struct and a parallel map
// literal. Two hand-written representations of "the same" request is precisely
// the drift this test exists to detect, so the test must not contain any.
func TestCreateInstanceValidationParity(t *testing.T) {
	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }

	opStore := newStubOperationStore()
	// A provisionResp is wired even though no row here should reach the engine:
	// if a gate silently stops firing, the request runs on into
	// runCreateInstance, and a nil response would panic the goroutine (killing
	// the whole test binary) instead of failing this test's assertions.
	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-parity", Hostname: "gpu-parity.example.com"}}
	deps := instanceTestDeps(opStore, newStubInstanceStore(), engine)

	// The unknown-gpu_type gate is the one row that needs a live availability
	// catalog (it fails OPEN without one, so the request would sail past it).
	// Wire the Redis-backed cache when REDIS_TEST_URL is set — CI always sets
	// it; a laptop without Redis skips that single row and still runs the other
	// eleven, rather than skipping the whole parity contract.
	catalogWired := os.Getenv("REDIS_TEST_URL") != ""
	if catalogWired {
		deps.AvailCache = instanceTestAvailCache(t, instanceValidationOfferings())
	}

	tests := []struct {
		name string
		req  public.CreateInstanceRequest
		// wantStatus/wantCode pin WHICH gate fires, not merely that the two
		// paths agree — parity alone would be satisfied by both paths being
		// broken in the same way.
		wantStatus int
		wantCode   string
		// needsCatalog marks a row whose gate fails open without the
		// availability cache.
		needsCatalog bool
	}{
		{
			name:       "gpu_count above the 8 maximum",
			req:        public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 9, Tier: "on_demand"},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
		},
		{
			name:       "gpu_count omitted on a raw instance",
			req:        public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 0, Tier: "on_demand"},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
		},
		{
			name:       "tier outside the enum",
			req:        public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "preemptible"},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
		},
		{
			name:       "malformed image reference",
			req:        public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand", Image: str("acme/trainer:not a tag")},
			wantStatus: http.StatusBadRequest,
			wantCode:   "validation_failed",
		},
		{
			name:       "image combined with template_id",
			req:        public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand", Image: str("acme/trainer:v1"), TemplateID: str("comfyui")},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
		},
		{
			name:       "environment outside the enum",
			req:        public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand", EnvironmentSelection: str("bare_metal")},
			wantStatus: http.StatusBadRequest,
			wantCode:   "validation_failed",
		},
		{
			name:       "offering_id with an illegal character",
			req:        public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand", OfferingID: str("off ering!")},
			wantStatus: http.StatusBadRequest,
			wantCode:   "validation_failed",
		},
		{
			name:       "auto_terminate_hours above the 30-day ceiling",
			req:        public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand", AutoTerminateHours: num(24*30 + 1)},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
		},
		{
			name:       "disk_gb above the configured ceiling",
			req:        public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand", DiskGB: num(5000)},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
		},
		{
			name:       "disk_gb below the floor",
			req:        public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand", DiskGB: num(1)},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
		},
		{
			// The stale-reference case D-24 is written for: a template the
			// catalog no longer carries must be refused, never substituted.
			name:       "unknown template_id",
			req:        public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand", TemplateID: str("not_a_real_template")},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
		},
		{
			name:         "unknown gpu_type",
			req:          public.CreateInstanceRequest{GPUType: "not_a_real_gpu", GPUCount: 1, Tier: "on_demand"},
			wantStatus:   http.StatusUnprocessableEntity,
			wantCode:     "validation_failed",
			needsCatalog: true,
		},
	}

	if len(tests) < 6 {
		t.Fatalf("parity table has %d rows; the contract requires at least 6", len(tests))
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsCatalog && !catalogWired {
				t.Skip("REDIS_TEST_URL not set — this gate fails open without the availability catalog")
			}

			// (a) the HTTP path.
			body, err := json.Marshal(tc.req)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			httpReq := httptest.NewRequest(http.MethodPost, "/v1/instances", bytes.NewReader(body))
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq = httpReq.WithContext(public.WithAPIKey(httpReq.Context(), parityKey()))
			mux := http.NewServeMux()
			public.InstanceHandlersForTesting(mux, deps)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httpReq)

			var problem public.Problem
			if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decode problem body: %v; body=%s", err, w.Body.String())
			}

			// (b) the direct path — same deps, same key, same request.
			op, derr := deps.CreateInstance(context.Background(), parityKey(), tc.req)
			if derr == nil {
				t.Fatalf("direct path accepted an invalid request (operation=%+v); HTTP path returned %d", op, w.Code)
			}
			var cerr *public.CreateInstanceError
			if !errors.As(derr, &cerr) {
				t.Fatalf("direct path error is %T (%v), want *public.CreateInstanceError", derr, derr)
			}

			// Parity: the two callers must refuse identically.
			if w.Code != cerr.Status {
				t.Errorf("status divergence: HTTP=%d direct=%d (detail=%q)", w.Code, cerr.Status, cerr.Detail)
			}
			if problem.Code != cerr.Type {
				t.Errorf("problem-type divergence: HTTP=%q direct=%q", problem.Code, cerr.Type)
			}
			if problem.Detail != cerr.Detail {
				t.Errorf("detail divergence:\n HTTP  =%q\n direct=%q", problem.Detail, cerr.Detail)
			}

			// And the gate that fired is the one we expect.
			if cerr.Status != tc.wantStatus {
				t.Errorf("status = %d, want %d (detail=%q)", cerr.Status, tc.wantStatus, cerr.Detail)
			}
			if cerr.Type != tc.wantCode {
				t.Errorf("problem type = %q, want %q (detail=%q)", cerr.Type, tc.wantCode, cerr.Detail)
			}
			if cerr.Detail == "" {
				t.Error("rejection carries no detail — a background caller has nothing to put in the abort notification")
			}
		})
	}

	// Every row above is a rejection, on both paths, so nothing may have been
	// provisioned or even recorded. This catches a gate that stopped firing
	// even if some other gate happened to produce a matching status.
	if n := opStore.operationCount(); n != 0 {
		t.Errorf("operation rows created = %d, want 0 — a rejected request reached CreateOperation", n)
	}
	if _, called := engine.lastRequest(); called {
		t.Error("engine.Provision was called for a rejected request")
	}
}

// TestCreateInstanceBackgroundCallerNeedsNoRequest is the auto-launch firer's
// call shape, proven today so plan 19 inherits a path it knows works: an APIKey
// built in-process from stored creator identity, a plain context, and a
// CreateInstanceRequest struct — no http.Request, no ResponseWriter, no
// problem-details body to parse back out.
//
// It also pins T-86-06: the synthesized key's org and user reach
// provision.ProvisionRequest, so the burn cap and spending limit bind a fired
// launch exactly as they bind a typed one. No real provider is contacted — the
// engine seam is the in-process stub.
func TestCreateInstanceBackgroundCallerNeedsNoRequest(t *testing.T) {
	opStore := newStubOperationStore()
	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-bg01", Hostname: "gpu-bg01.example.com"}}
	deps := instanceTestDeps(opStore, newStubInstanceStore(), engine)

	// The firer builds this from the rule's stored creator — never a wildcard.
	// Scopes are carried because scope enforcement lives in the HTTP route's
	// middleware, not in CreateInstance (T-86-08): a background caller that
	// leaves them empty has silently opted out of them.
	apiKey := &public.APIKey{
		APIKeyID: "k-firer",
		OrgID:    "org-firer",
		UserID:   "user-firer",
		Role:     "member",
		Scopes:   json.RawMessage(`{"full_access":true}`),
	}

	// The frozen launch spec (D-23), replayed exactly as stored.
	req := public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand"}

	op, err := deps.CreateInstance(context.Background(), apiKey, req)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if op == nil {
		t.Fatal("CreateInstance returned a nil operation with no error")
	}
	if op.OperationID == "" {
		t.Error("operation has no id — the firer has nothing to record or poll")
	}
	if op.Kind != "instance.create" {
		t.Errorf("kind = %q, want %q", op.Kind, "instance.create")
	}
	if op.State != "pending" {
		t.Errorf("state = %q, want %q", op.State, "pending")
	}

	// The engine runs on a detached goroutine; poll rather than sleep so the
	// test finishes as soon as the dispatch lands.
	var provReq provision.ProvisionRequest
	deadline := time.Now().Add(3 * time.Second)
	for {
		r, called := engine.lastRequest()
		if called {
			provReq = r
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("engine.Provision was never called — the background call did not reach provisioning")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if provReq.OrgID != apiKey.OrgID {
		t.Errorf("provision OrgID = %q, want %q — org scoping (and the burn cap) would not bind", provReq.OrgID, apiKey.OrgID)
	}
	if provReq.UserID != apiKey.UserID {
		t.Errorf("provision UserID = %q, want %q", provReq.UserID, apiKey.UserID)
	}
	if provReq.IsAdmin {
		t.Error("provision IsAdmin = true for a member key — a fired launch must not inherit admin placement")
	}
}

// TestCreateInstanceRejectsNilAPIKey pins the other half of T-86-06: the
// function refuses to provision without an identity, rather than nil-derefing
// or (worse) provisioning against an empty org. The HTTP handler checks this
// first and never reaches here with nil, but a background caller has no
// middleware in front of it.
func TestCreateInstanceRejectsNilAPIKey(t *testing.T) {
	opStore := newStubOperationStore()
	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-nil", Hostname: "gpu-nil.example.com"}}
	deps := instanceTestDeps(opStore, newStubInstanceStore(), engine)

	op, err := deps.CreateInstance(context.Background(), nil, public.CreateInstanceRequest{
		GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand",
	})
	if err == nil {
		t.Fatalf("CreateInstance accepted a nil API key (operation=%+v)", op)
	}
	var cerr *public.CreateInstanceError
	if !errors.As(err, &cerr) {
		t.Fatalf("error is %T (%v), want *public.CreateInstanceError", err, err)
	}
	if cerr.Status != http.StatusUnauthorized || cerr.Type != "unauthenticated" {
		t.Errorf("got %d/%q, want 401/%q", cerr.Status, cerr.Type, "unauthenticated")
	}
	if n := opStore.operationCount(); n != 0 {
		t.Errorf("operation rows created = %d, want 0", n)
	}
}

// TestCreateInstanceWithOperationIDUsesTheSuppliedID: the id the caller already
// wrote into its own durable record must be the id the operation row gets, or
// the link points at an operation that does not exist and the recovery path
// resolves the wrong thing (or nothing).
func TestCreateInstanceWithOperationIDUsesTheSuppliedID(t *testing.T) {
	opStore := newStubOperationStore()
	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-supplied", Hostname: "gpu-supplied.example.com"}}
	deps := instanceTestDeps(opStore, newStubInstanceStore(), engine)

	apiKey := &public.APIKey{
		APIKeyID: "k-firer",
		OrgID:    "org-firer",
		UserID:   "user-firer",
		Role:     "member",
		Scopes:   json.RawMessage(`{"full_access":true}`),
	}
	req := public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand"}
	const supplied = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

	op, err := deps.CreateInstanceWithOperationID(context.Background(), apiKey, req, supplied)
	if err != nil {
		t.Fatalf("CreateInstanceWithOperationID: %v", err)
	}
	if op.OperationID != supplied {
		t.Fatalf("operation id = %q, want the supplied %q — the durable link would point at nothing", op.OperationID, supplied)
	}
}

// TestCreateInstanceDelegatesWithNoOperationID: the HTTP surface is unchanged.
// CreateInstance passes an empty id, so the database assigns one exactly as
// before, and nothing about a typed request can influence it — the id is
// deliberately not a field on CreateInstanceRequest.
func TestCreateInstanceDelegatesWithNoOperationID(t *testing.T) {
	opStore := newStubOperationStore()
	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-assigned", Hostname: "gpu-assigned.example.com"}}
	deps := instanceTestDeps(opStore, newStubInstanceStore(), engine)

	apiKey := &public.APIKey{
		APIKeyID: "k-http",
		OrgID:    "org-http",
		UserID:   "user-http",
		Role:     "member",
		Scopes:   json.RawMessage(`{"full_access":true}`),
	}
	req := public.CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand"}

	op, err := deps.CreateInstance(context.Background(), apiKey, req)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if op.OperationID == "" {
		t.Fatal("no id was assigned; the caller has nothing to poll")
	}
	// The store assigns "op-<n>" when the caller supplies nothing; a caller-
	// supplied id would look nothing like it.
	if !strings.HasPrefix(op.OperationID, "op-") {
		t.Fatalf("operation id = %q, want a store-assigned one — CreateInstance must not invent an id of its own", op.OperationID)
	}
}
