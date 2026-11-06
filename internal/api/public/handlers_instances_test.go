package public_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/api/public"
	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
	tmplcatalog "github.com/gpuai-dev/gpuai-oss/internal/template/catalog"
	"github.com/redis/go-redis/v9"
)

// stubInstanceStore is an in-memory fixture for the public.instanceStore
// interface. Only the three methods the public handlers consume are
// implemented (List/Get/UpdateName); cross-org isolation mirrors the
// real GetInstanceForOrg behaviour.
type stubInstanceStore struct {
	mu        sync.Mutex
	instances map[string]*db.Instance // by instance_id
}

func newStubInstanceStore() *stubInstanceStore {
	return &stubInstanceStore{instances: map[string]*db.Instance{}}
}

func (s *stubInstanceStore) put(inst *db.Instance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instances[inst.InstanceID] = inst
}

func (s *stubInstanceStore) ListInstances(_ context.Context, orgID string, statuses []string, _ *time.Time, _ string, limit int) ([]db.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]db.Instance, 0, limit+1)
	for _, inst := range s.instances {
		if inst.OrgID != orgID {
			continue
		}
		// Mirror the SQL `status = ANY(statuses)` predicate: empty list
		// means no filter, otherwise the row's status must be in the set.
		if len(statuses) > 0 {
			match := false
			for _, st := range statuses {
				if inst.Status == st {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		out = append(out, *inst)
	}
	return out, nil
}

func (s *stubInstanceStore) GetInstanceForOrg(_ context.Context, instanceID, orgID string) (*db.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[instanceID]
	if !ok || inst.OrgID != orgID {
		return nil, db.ErrNotFound
	}
	cp := *inst
	return &cp, nil
}

func (s *stubInstanceStore) UpdateInstanceName(_ context.Context, instanceID, orgID string, name *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[instanceID]
	if !ok || inst.OrgID != orgID {
		return nil // mimic UpdateInstanceName: no-op on missing rows
	}
	inst.Name = name
	return nil
}

// stubOperationStore is an in-memory fixture for public.operationStore.
// Versions increment correctly so the FSM transitions are exercised
// end-to-end. Concurrent UPDATE on a stale version returns
// ErrOperationStateChanged so MAJOR 13 paths are testable.
type stubOperationStore struct {
	mu          sync.Mutex
	operations  map[string]*db.Operation // by operation_id
	idSeq       int
	createErr   error
	updateState func(opID string, prev int, newState string) (int, error) // optional override
}

func newStubOperationStore() *stubOperationStore {
	return &stubOperationStore{operations: map[string]*db.Operation{}}
}

// operationCount returns how many operation rows have been created — used by
// the gpu_type-validation tests to assert a rejected request creates none.
func (s *stubOperationStore) operationCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.operations)
}

func (s *stubOperationStore) CreateOperation(_ context.Context, o *db.Operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return s.createErr
	}
	// A caller-supplied primary key wins, exactly as the production statement's
	// COALESCE($1::uuid, gen_random_uuid()) does. Overwriting it here would let
	// a test "prove" the firer launches under the id it checkpointed while
	// production used a different one.
	if o.OperationID == "" {
		s.idSeq++
		o.OperationID = "op-" + intToBase10(s.idSeq)
	}
	o.State = "pending"
	o.Version = 1
	now := time.Now().UTC()
	o.CreatedAt = now
	o.UpdatedAt = now
	stored := *o
	s.operations[o.OperationID] = &stored
	return nil
}

func (s *stubOperationStore) GetOperation(_ context.Context, operationID, apiKeyID string) (*db.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.operations[operationID]
	if !ok || o.APIKeyID != apiKeyID {
		return nil, db.ErrNotFound
	}
	cp := *o
	return &cp, nil
}

func (s *stubOperationStore) UpdateOperationState(_ context.Context, operationID string, prevVersion int, newState string) (int, error) {
	if s.updateState != nil {
		return s.updateState(operationID, prevVersion, newState)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.operations[operationID]
	if !ok {
		return 0, db.ErrOperationStateChanged
	}
	if o.Version != prevVersion {
		return 0, db.ErrOperationStateChanged
	}
	o.Version++
	o.State = newState
	o.UpdatedAt = time.Now().UTC()
	if newState == "succeeded" || newState == "failed" || newState == "cancelled" {
		now := time.Now().UTC()
		o.CompletedAt = &now
	}
	return o.Version, nil
}

func (s *stubOperationStore) SetOperationResource(_ context.Context, operationID string, prevVersion int, resourceID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.operations[operationID]
	if !ok {
		return 0, db.ErrOperationStateChanged
	}
	if o.Version != prevVersion {
		return 0, db.ErrOperationStateChanged
	}
	o.Version++
	rid := resourceID
	o.ResourceID = &rid
	o.UpdatedAt = time.Now().UTC()
	return o.Version, nil
}

func (s *stubOperationStore) SetOperationResult(_ context.Context, operationID string, prevVersion int, result json.RawMessage) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.operations[operationID]
	if !ok {
		return 0, db.ErrOperationStateChanged
	}
	if o.Version != prevVersion {
		return 0, db.ErrOperationStateChanged
	}
	o.Version++
	o.Result = result
	o.UpdatedAt = time.Now().UTC()
	return o.Version, nil
}

func (s *stubOperationStore) SetOperationFailure(_ context.Context, operationID string, prevVersion int, code, detail string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.operations[operationID]
	if !ok {
		return 0, db.ErrOperationStateChanged
	}
	if o.Version != prevVersion {
		return 0, db.ErrOperationStateChanged
	}
	o.Version++
	o.State = "failed"
	c := code
	d := detail
	o.ErrorCode = &c
	o.ErrorDetail = &d
	now := time.Now().UTC()
	o.UpdatedAt = now
	o.CompletedAt = &now
	return o.Version, nil
}

// stubEngine implements public.engineRunner without spinning up
// providers, FRP, or DB. Provision/Terminate behaviors are scriptable
// via the fixture fields so tests can drive both happy and error paths.
type stubEngine struct {
	provisionResp *provision.ProvisionResponse
	provisionErr  error
	terminateErr  error

	mu             sync.Mutex
	provisionCalls int
	terminateCalls int
	// lastProvisionReq is the request the handler actually assembled. Captured so
	// tests can assert what was THREADED into the engine, not merely that the
	// engine was called. Read it via lastRequest(): Provision runs on a handler
	// goroutine, so touching the field directly races.
	lastProvisionReq provision.ProvisionRequest
}

// lastRequest returns the most recent ProvisionRequest and whether Provision has
// been called at all, both under the mutex. Poll it to synchronize with the
// handler's async engine goroutine.
func (s *stubEngine) lastRequest() (provision.ProvisionRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastProvisionReq, s.provisionCalls > 0
}

func (s *stubEngine) Provision(_ context.Context, req provision.ProvisionRequest) (*provision.ProvisionResponse, error) {
	s.mu.Lock()
	s.provisionCalls++
	s.lastProvisionReq = req
	s.mu.Unlock()
	if s.provisionErr != nil {
		return nil, s.provisionErr
	}
	return s.provisionResp, nil
}

func (s *stubEngine) Terminate(_ context.Context, _ string) error {
	s.mu.Lock()
	s.terminateCalls++
	s.mu.Unlock()
	return s.terminateErr
}

// intToBase10 is a 5-line strconv-free integer formatter. Tests live in
// public_test, which imports the package surface only — adding strconv
// to the bag of imports for one small helper is bigger churn than this
// inline routine.
func intToBase10(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// invokeInstance fires a request through InstanceHandlersForTesting with
// an injected APIKey context. Mirrors invokeWithAPIKey from
// handlers_ssh_keys_test.go but targets the instance routes.
func invokeInstance(t *testing.T, deps public.PublicDeps, method, target, orgID, userID, apiKeyID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	k := &public.APIKey{
		APIKeyID: apiKeyID,
		OrgID:    orgID,
		UserID:   userID,
		Scopes:   json.RawMessage(`{"full_access":true}`),
	}
	req = req.WithContext(public.WithAPIKey(req.Context(), k))

	mux := http.NewServeMux()
	public.InstanceHandlersForTesting(mux, deps)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func instanceTestDeps(opStore *stubOperationStore, instStore *stubInstanceStore, engine *stubEngine) public.PublicDeps {
	deps := public.PublicDeps{}
	deps = public.PublicDepsWithOperationStore(deps, opStore)
	deps = public.PublicDepsWithInstanceStore(deps, instStore)
	deps = public.PublicDepsWithEngineRunner(deps, engine)
	// Wire the embedded template catalog so the template-deploy path
	// (template_id resolution) works in handler tests. Harmless for raw-instance
	// tests, which never set template_id. comfyui/vllm are the seeded entries.
	if cat, err := tmplcatalog.LoadEmpty(); err == nil {
		deps.TemplateCatalog = cat
	}
	// A configured apps host so the template-deploy happy path passes the HG-02
	// guard (service templates have a primary HTTP port and require APPS_HOST).
	// Tests that exercise the missing-APPS_HOST 503 clear deps.Config themselves.
	deps.Config = &config.Config{AppsHost: "apps.example.com"}
	return deps
}

// validBody is the canonical valid POST /v1/instances body used across
// happy-path tests.
var validBody = map[string]any{
	"gpu_type":  "h100_sxm",
	"gpu_count": 1,
	"tier":      "on_demand",
}

func TestHandleCreateInstance_RequiresAPIKey(t *testing.T) {
	deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), &stubEngine{})
	req := httptest.NewRequest(http.MethodPost, "/v1/instances", bytes.NewReader([]byte(`{}`)))
	mux := http.NewServeMux()
	public.InstanceHandlersForTesting(mux, deps)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// instanceTestAvailCache seeds offerings into a per-test Redis key and returns
// a Cache wired to it (issue #99 gpu_type validation). Redis-gated via
// REDIS_TEST_URL like the catalog tests; skips cleanly without Redis.
func instanceTestAvailCache(t *testing.T, offerings []availability.AvailableOffering) *availability.Cache {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL not set — skipping Redis-gated gpu_type validation test")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_TEST_URL: %v", err)
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	key := fmt.Sprintf("test:gpu:offerings:inst:%d", time.Now().UnixNano())
	cache := availability.NewCacheWithKey(rdb, time.Minute, key)
	if err := cache.SetOfferings(context.Background(), offerings); err != nil {
		t.Fatalf("seed offerings: %v", err)
	}
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), key).Err()
		_ = rdb.Close()
	})
	return cache
}

// instanceValidationOfferings is the seeded catalog for the gpu_type-validation
// tests: h100_sxm is offered, not_a_real_gpu is not.
func instanceValidationOfferings() []availability.AvailableOffering {
	return []availability.AvailableOffering{
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.5, Region: "us-west", Tier: "on_demand", AvailableCount: 8},
		{GPUModel: "a100_pcie", GPUCount: 1, VRAMGB: 40, PricePerHour: 1.2, Region: "us-east", Tier: "on_demand", AvailableCount: 16},
	}
}

// TestHandleCreateInstance_RejectsUnknownGPUType — issue #99: an unknown
// gpu_type is rejected synchronously with 422 (naming gpu_type) and NO
// operation row is created, instead of a fake-success 202 that fails ~7s later.
func TestHandleCreateInstance_RejectsUnknownGPUType(t *testing.T) {
	opStore := newStubOperationStore()
	deps := instanceTestDeps(opStore, newStubInstanceStore(), &stubEngine{})
	deps.AvailCache = instanceTestAvailCache(t, instanceValidationOfferings())

	w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "o1", "u1", "k1",
		map[string]any{"gpu_type": "not_a_real_gpu", "gpu_count": 1, "tier": "on_demand"})

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d want 422; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gpu_type") {
		t.Errorf("error should name gpu_type; body=%s", w.Body.String())
	}
	if n := opStore.operationCount(); n != 0 {
		t.Errorf("operation rows created = %d want 0 (rejected before CreateOperation)", n)
	}
}

// TestHandleCreateInstance_AcceptsKnownGPUType — issue #99 no-regression: a
// gpu_type present in the availability cache passes synchronous validation and
// proceeds to the async create (202). A sold-out-but-listed type behaves the
// same — validation gates membership, not stock.
func TestHandleCreateInstance_AcceptsKnownGPUType(t *testing.T) {
	opStore := newStubOperationStore()
	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-known1", Hostname: "gpu-known1.example.com"}}
	deps := instanceTestDeps(opStore, newStubInstanceStore(), engine)
	deps.AvailCache = instanceTestAvailCache(t, instanceValidationOfferings())

	w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "o1", "u1", "k1",
		map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand"})

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d want 202 (known gpu_type must not be rejected); body=%s", w.Code, w.Body.String())
	}
	if n := opStore.operationCount(); n != 1 {
		t.Errorf("operation rows created = %d want 1", n)
	}
}

func TestHandleCreateInstance_ValidationError(t *testing.T) {
	opStore := newStubOperationStore()
	deps := instanceTestDeps(opStore, newStubInstanceStore(), &stubEngine{})

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing gpu_type", map[string]any{"gpu_count": 1, "tier": "spot"}},
		{"gpu_count zero", map[string]any{"gpu_type": "h100", "gpu_count": 0, "tier": "spot"}},
		{"gpu_count too high", map[string]any{"gpu_type": "h100", "gpu_count": 9, "tier": "spot"}},
		{"bad tier", map[string]any{"gpu_type": "h100", "gpu_count": 1, "tier": "free"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", c.body)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status: got %d, want 422; body=%s", w.Code, w.Body.String())
			}
			var p public.Problem
			_ = json.NewDecoder(w.Body).Decode(&p)
			if p.Code != "validation_failed" {
				t.Errorf("code: got %q, want validation_failed", p.Code)
			}
		})
	}

	// No operation row should have been created on validation failure.
	if got, _ := opStore.GetOperation(context.Background(), "op-1", "ak-1"); got != nil {
		t.Errorf("validation failure must not create operation row")
	}
}

// TestHandleCreateInstance_TierReservedAccepted is the public-API
// counterpart to plan 30-04's dashboard validator update: tier="reserved"
// must pass CreateInstanceRequest.Validate() and reach the engine. The
// engine stub returns the canned ProvisionResponse so the request
// completes 202 Accepted just like the on-demand happy path.
func TestHandleCreateInstance_TierReservedAccepted(t *testing.T) {
	opStore := newStubOperationStore()
	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-res1", Hostname: "gpu-res1.example.com"}}
	deps := instanceTestDeps(opStore, newStubInstanceStore(), engine)

	body := map[string]any{
		"gpu_type":  "h100_sxm",
		"gpu_count": 1,
		"tier":      "reserved",
	}
	w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleCreateInstance_BadJSON(t *testing.T) {
	deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), &stubEngine{})
	req := httptest.NewRequest(http.MethodPost, "/v1/instances", strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	k := &public.APIKey{APIKeyID: "ak", OrgID: "org-A", UserID: "user-A"}
	req = req.WithContext(public.WithAPIKey(req.Context(), k))
	mux := http.NewServeMux()
	public.InstanceHandlersForTesting(mux, deps)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleCreateInstance_HappyPath(t *testing.T) {
	opStore := newStubOperationStore()
	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-abc", Hostname: "gpu-abc.example.com"}}
	deps := instanceTestDeps(opStore, newStubInstanceStore(), engine)

	w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", validBody)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Operation-Id"); got == "" {
		t.Fatalf("Operation-Id header not set")
	}
	var op public.Operation
	if err := json.NewDecoder(w.Body).Decode(&op); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if op.State != "pending" {
		t.Errorf("state: got %q, want pending", op.State)
	}
	if op.Kind != "instance.create" {
		t.Errorf("kind: got %q, want instance.create", op.Kind)
	}

	// Wait for the engine goroutine to finish; the FSM must end at
	// 'succeeded' with resource_id populated.
	deadline := time.Now().Add(2 * time.Second)
	var finalOp *db.Operation
	for time.Now().Before(deadline) {
		finalOp, _ = opStore.GetOperation(context.Background(), op.OperationID, "ak-1")
		if finalOp != nil && finalOp.State == "succeeded" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if finalOp == nil || finalOp.State != "succeeded" {
		t.Fatalf("FSM did not reach succeeded; got %+v", finalOp)
	}
	if finalOp.ResourceID == nil || *finalOp.ResourceID != "gpu-abc" {
		t.Errorf("resource_id: got %v, want 'gpu-abc'", finalOp.ResourceID)
	}
	// Version increments: 1 (create) + 1 (in_progress) + 1 (resource) + 1 (succeeded) = 4
	if finalOp.Version != 4 {
		t.Errorf("final version: got %d, want 4", finalOp.Version)
	}
	if engine.provisionCalls != 1 {
		t.Errorf("provision calls: got %d, want 1", engine.provisionCalls)
	}

	// Operation row must carry the issuing api_key_id.
	if finalOp.APIKeyID != "ak-1" {
		t.Errorf("api_key_id: got %q, want ak-1", finalOp.APIKeyID)
	}
	if finalOp.OrgID != "org-A" {
		t.Errorf("org_id: got %q, want org-A", finalOp.OrgID)
	}
	if finalOp.Kind != "instance.create" {
		t.Errorf("kind on stored row: got %q, want instance.create", finalOp.Kind)
	}
}

func TestHandleCreateInstance_EngineFailureMarksFailed(t *testing.T) {
	opStore := newStubOperationStore()
	engine := &stubEngine{provisionErr: provider.ErrNoCapacity}
	deps := instanceTestDeps(opStore, newStubInstanceStore(), engine)

	w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", validBody)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202; body=%s", w.Code, w.Body.String())
	}
	opID := w.Header().Get("Operation-Id")

	deadline := time.Now().Add(2 * time.Second)
	var finalOp *db.Operation
	for time.Now().Before(deadline) {
		finalOp, _ = opStore.GetOperation(context.Background(), opID, "ak-1")
		if finalOp != nil && finalOp.State == "failed" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if finalOp == nil || finalOp.State != "failed" {
		t.Fatalf("FSM did not reach failed; got %+v", finalOp)
	}
	if finalOp.ErrorCode == nil || *finalOp.ErrorCode != "operation_failed" {
		t.Errorf("error_code: got %v, want operation_failed", finalOp.ErrorCode)
	}
	if finalOp.ErrorDetail == nil || !strings.Contains(*finalOp.ErrorDetail, "no GPU capacity") {
		t.Errorf("error_detail: got %v, want substring 'no GPU capacity'", finalOp.ErrorDetail)
	}
}

func TestHandleGetOperation_NotFound(t *testing.T) {
	opStore := newStubOperationStore()
	deps := instanceTestDeps(opStore, newStubInstanceStore(), &stubEngine{})

	w := invokeInstance(t, deps, http.MethodGet, "/v1/operations/op-missing", "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", w.Code, w.Body.String())
	}
	var p public.Problem
	_ = json.NewDecoder(w.Body).Decode(&p)
	if p.Code != "not_found" {
		t.Errorf("code: got %q, want not_found", p.Code)
	}
}

func TestHandleGetOperation_CrossKeyIsolation(t *testing.T) {
	opStore := newStubOperationStore()
	deps := instanceTestDeps(opStore, newStubInstanceStore(), &stubEngine{
		provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-iso"},
	})

	// Create an operation under api_key=ak-1 (org-A).
	w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", validBody)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create: got %d, want 202; body=%s", w.Code, w.Body.String())
	}
	opID := w.Header().Get("Operation-Id")

	// Same org, different api_key (ak-2) must NOT see the operation.
	w = invokeInstance(t, deps, http.MethodGet, "/v1/operations/"+opID, "org-A", "user-A", "ak-2", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-key access: got %d, want 404; body=%s", w.Code, w.Body.String())
	}

	// Issuing key still observes its own row.
	w = invokeInstance(t, deps, http.MethodGet, "/v1/operations/"+opID, "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("issuer access: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleListInstances_OrgScoped(t *testing.T) {
	instStore := newStubInstanceStore()
	instStore.put(&db.Instance{
		InstanceID: "gpu-A1", OrgID: "org-A", UserID: "user-A",
		GPUType: "h100", GPUCount: 1, Tier: "on_demand", Region: "us-west",
		Status: "running", PricePerHour: 2.5, CreatedAt: time.Now().UTC(),
	})
	instStore.put(&db.Instance{
		InstanceID: "gpu-B1", OrgID: "org-B", UserID: "user-B",
		GPUType: "h100", GPUCount: 1, Tier: "on_demand", Region: "us-west",
		Status: "running", PricePerHour: 2.5, CreatedAt: time.Now().UTC(),
	})
	deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})

	w := invokeInstance(t, deps, http.MethodGet, "/v1/instances", "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var page public.Page[public.Instance]
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].ID != "gpu-A1" {
		t.Errorf("got %+v, want [gpu-A1]", page.Data)
	}
}

func TestHandleGetInstance_NotFoundCrossOrg(t *testing.T) {
	instStore := newStubInstanceStore()
	instStore.put(&db.Instance{
		InstanceID: "gpu-A1", OrgID: "org-A", UserID: "user-A",
		GPUType: "h100", GPUCount: 1, Tier: "on_demand", Region: "us-west",
		Status: "running", PricePerHour: 2.5, CreatedAt: time.Now().UTC(),
	})
	deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})

	// Cross-org access — must 404.
	w := invokeInstance(t, deps, http.MethodGet, "/v1/instances/gpu-A1", "org-B", "user-B", "ak-2", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-org: got %d, want 404; body=%s", w.Code, w.Body.String())
	}

	// Same-org access — 200.
	w = invokeInstance(t, deps, http.MethodGet, "/v1/instances/gpu-A1", "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("same-org: got %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Privacy: response must not leak any upstream fields.
	body := w.Body.String()
	for _, banned := range []string{"upstream_", "wg_", "frp_", "internal_token", "hostname"} {
		if strings.Contains(body, banned) {
			t.Errorf("response leaks %q: %s", banned, body)
		}
	}
}

func TestHandlePatchInstance_UpdatesName(t *testing.T) {
	instStore := newStubInstanceStore()
	originalName := "old"
	instStore.put(&db.Instance{
		InstanceID: "gpu-A1", OrgID: "org-A", UserID: "user-A",
		GPUType: "h100", GPUCount: 1, Tier: "on_demand", Region: "us-west",
		Status: "running", PricePerHour: 2.5,
		Name: &originalName, CreatedAt: time.Now().UTC(),
	})
	deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})

	newName := "renamed"
	body := map[string]any{"name": newName}
	w := invokeInstance(t, deps, http.MethodPatch, "/v1/instances/gpu-A1", "org-A", "user-A", "ak-1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got public.Instance
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name == nil || *got.Name != "renamed" {
		t.Errorf("name: got %v, want 'renamed'", got.Name)
	}
}

func TestHandlePatchInstance_NotFound(t *testing.T) {
	deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), &stubEngine{})

	body := map[string]any{"name": "irrelevant"}
	w := invokeInstance(t, deps, http.MethodPatch, "/v1/instances/gpu-missing", "org-A", "user-A", "ak-1", body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleDeleteInstance_AsyncOperation(t *testing.T) {
	instStore := newStubInstanceStore()
	instStore.put(&db.Instance{
		InstanceID: "gpu-A1", OrgID: "org-A", UserID: "user-A",
		GPUType: "h100", GPUCount: 1, Tier: "on_demand", Region: "us-west",
		Status: "running", PricePerHour: 2.5, CreatedAt: time.Now().UTC(),
	})
	opStore := newStubOperationStore()
	engine := &stubEngine{}
	deps := instanceTestDeps(opStore, instStore, engine)

	w := invokeInstance(t, deps, http.MethodDelete, "/v1/instances/gpu-A1", "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202; body=%s", w.Code, w.Body.String())
	}
	opID := w.Header().Get("Operation-Id")
	if opID == "" {
		t.Fatal("Operation-Id header not set on DELETE")
	}

	deadline := time.Now().Add(2 * time.Second)
	var finalOp *db.Operation
	for time.Now().Before(deadline) {
		finalOp, _ = opStore.GetOperation(context.Background(), opID, "ak-1")
		if finalOp != nil && finalOp.State == "succeeded" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if finalOp == nil || finalOp.State != "succeeded" {
		t.Fatalf("FSM did not reach succeeded; got %+v", finalOp)
	}
	if finalOp.Kind != "instance.delete" {
		t.Errorf("kind: got %q, want instance.delete", finalOp.Kind)
	}
	if finalOp.ResourceID == nil || *finalOp.ResourceID != "gpu-A1" {
		t.Errorf("resource_id: got %v, want 'gpu-A1'", finalOp.ResourceID)
	}
	if engine.terminateCalls != 1 {
		t.Errorf("terminate calls: got %d, want 1", engine.terminateCalls)
	}
}

func TestHandleDeleteInstance_AlreadyGone(t *testing.T) {
	deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), &stubEngine{})

	// Instance doesn't exist — D-07 says 404 with code not_found.
	w := invokeInstance(t, deps, http.MethodDelete, "/v1/instances/gpu-missing", "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", w.Code, w.Body.String())
	}
	var p public.Problem
	_ = json.NewDecoder(w.Body).Decode(&p)
	if p.Code != "not_found" {
		t.Errorf("code: got %q, want not_found", p.Code)
	}
}

func TestHandleDeleteInstance_CrossOrg(t *testing.T) {
	instStore := newStubInstanceStore()
	instStore.put(&db.Instance{
		InstanceID: "gpu-A1", OrgID: "org-A", UserID: "user-A",
		GPUType: "h100", GPUCount: 1, Tier: "on_demand", Region: "us-west",
		Status: "running", CreatedAt: time.Now().UTC(),
	})
	deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})

	// org-B should get 404 — cross-org delete must NOT go through.
	w := invokeInstance(t, deps, http.MethodDelete, "/v1/instances/gpu-A1", "org-B", "user-B", "ak-2", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", w.Code, w.Body.String())
	}

	// Verify the instance is still in org-A.
	if got, _ := instStore.GetInstanceForOrg(context.Background(), "gpu-A1", "org-A"); got == nil {
		t.Errorf("cross-org delete leaked through")
	}
}

// TestRunCreateInstance_FSMVersionDriftAborts pins MAJOR 13: a goroutine
// hitting an optimistic-lock conflict on the in_progress transition must
// log + return rather than continuing with a stale version (which would
// silently corrupt the FSM by writing past the real terminal state).
func TestRunCreateInstance_FSMVersionDriftAborts(t *testing.T) {
	// Inject a mid-transition concurrent update that bumps the version
	// out from under the goroutine. The first UpdateOperationState call
	// returns ErrOperationStateChanged; the goroutine MUST abort —
	// engine.Provision must NOT be called (would be wasteful + may
	// corrupt billing).
	opStore := newStubOperationStore()
	op := &db.Operation{APIKeyID: "ak-1", OrgID: "org-A", Kind: "instance.create"}
	if err := opStore.CreateOperation(context.Background(), op); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	// override updateState to record observed prev + return conflict.
	opStore.updateState = func(_ string, prev int, _ string) (int, error) {
		if prev != 1 {
			t.Errorf("prevVersion: got %d, want 1 (goroutine should not retry with stale version)", prev)
		}
		return 0, db.ErrOperationStateChanged
	}

	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-x"}}

	// Call the goroutine entry point synchronously.
	public.RunCreateInstanceForTesting(opStore, engine, op, provision.ProvisionRequest{
		OrgID: "org-A", UserID: "user-A",
		GPUType: provider.GPUType("h100"), GPUCount: 1, Tier: provider.InstanceTier("on_demand"),
	})

	if engine.provisionCalls != 0 {
		t.Errorf("engine.Provision called after FSM conflict: got %d calls, want 0", engine.provisionCalls)
	}
}

// TestPrivacyContractSampleInstance round-trips a fully-populated
// db.Instance through the public Instance shape and asserts the
// upstream/internal fields never appear in the wire JSON. Acts as a
// belt-and-braces check alongside privacy_test.go's struct-tag scan.
func TestPrivacyContractSampleInstance(t *testing.T) {
	deps := public.PublicDeps{}
	instStore := newStubInstanceStore()
	upIP := "203.0.113.5"
	wgPub := "wg-pubkey-secret"
	wgPriv := "wg-privkey-secret"
	wgAddr := "10.42.0.5/24"
	port := 10042
	internalToken := "tk-internal-do-not-leak"
	instStore.put(&db.Instance{
		InstanceID:       "gpu-leak",
		OrgID:            "org-A",
		UserID:           "user-A",
		UpstreamProvider: "secret-provider",
		UpstreamID:       "up-secret-123",
		UpstreamIP:       &upIP,
		Hostname:         "gpu-leak.example.com",
		WGPublicKey:      &wgPub,
		WGPrivateKeyEnc:  &wgPriv,
		WGAddress:        &wgAddr,
		FRPRemotePort:    &port,
		InternalToken:    &internalToken,
		GPUType:          "h100",
		GPUCount:         1,
		Tier:             "on_demand",
		Region:           "us-west",
		Status:           "running",
		PricePerHour:     2.5,
		CreatedAt:        time.Now().UTC(),
	})
	deps = public.PublicDepsWithInstanceStore(deps, instStore)
	deps = public.PublicDepsWithOperationStore(deps, newStubOperationStore())
	deps = public.PublicDepsWithEngineRunner(deps, &stubEngine{})

	w := invokeInstance(t, deps, http.MethodGet, "/v1/instances/gpu-leak", "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, banned := range []string{
		"upstream_provider", "upstream_id", "upstream_ip",
		"wg_public", "wg_private", "wg_address",
		"frp_remote_port", "internal_token",
		"secret-provider", "up-secret-123", "wg-pubkey-secret", "wg-privkey-secret",
		"tk-internal-do-not-leak",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("Instance JSON leaks %q: %s", banned, body)
		}
	}
	// Sanity: hostname is excluded from the public shape too.
	if strings.Contains(body, "gpu-leak.example.com") {
		t.Errorf("Instance JSON leaks raw hostname: %s", body)
	}
}

// guard against accidental nil deref in handleListInstances when no rows.
func TestHandleListInstances_Empty(t *testing.T) {
	deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), &stubEngine{})
	w := invokeInstance(t, deps, http.MethodGet, "/v1/instances", "org-empty", "user-empty", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var page public.Page[public.Instance]
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Data) != 0 {
		t.Errorf("empty list: got %+v, want []", page.Data)
	}
	if page.NextCursor != nil {
		t.Errorf("next_cursor: got %v, want nil", page.NextCursor)
	}
}

// suppress unused-error import compile warnings.
var _ = errors.Is

// TestHandleListInstances_DefaultExcludesTerminated is the regression test
// for the 2026-06-10 API review finding: GET /v1/instances returned every
// terminated instance back to day one. The default view is the live fleet;
// history is opt-in via ?status=terminated or ?status=all.
func TestHandleListInstances_DefaultExcludesTerminated(t *testing.T) {
	instStore := newStubInstanceStore()
	instStore.put(&db.Instance{
		InstanceID: "gpu-live", OrgID: "org-A", UserID: "user-A",
		GPUType: "h100", GPUCount: 1, Tier: "on_demand", Region: "us-west",
		Status: provision.StateRunning, PricePerHour: 2.5, CreatedAt: time.Now().UTC(),
	})
	instStore.put(&db.Instance{
		InstanceID: "gpu-old", OrgID: "org-A", UserID: "user-A",
		GPUType: "h100", GPUCount: 1, Tier: "on_demand", Region: "us-west",
		Status: provision.StateTerminated, PricePerHour: 2.5, CreatedAt: time.Now().UTC(),
	})
	deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})

	w := invokeInstance(t, deps, http.MethodGet, "/v1/instances", "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var page public.Page[public.Instance]
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].ID != "gpu-live" {
		t.Errorf("default list: got %+v, want only gpu-live", page.Data)
	}

	// ?status=terminated → only the historical row.
	w = invokeInstance(t, deps, http.MethodGet, "/v1/instances?status=terminated", "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=terminated: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	page = public.Page[public.Instance]{}
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].ID != "gpu-old" {
		t.Errorf("status=terminated: got %+v, want only gpu-old", page.Data)
	}

	// ?status=all → both.
	w = invokeInstance(t, deps, http.MethodGet, "/v1/instances?status=all", "org-A", "user-A", "ak-1", nil)
	page = public.Page[public.Instance]{}
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Data) != 2 {
		t.Errorf("status=all: got %d rows, want 2", len(page.Data))
	}
}

// TestHandleListInstances_InvalidStatus400: unknown ?status= values are a
// validation error, not a silent empty list.
func TestHandleListInstances_InvalidStatus400(t *testing.T) {
	deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), &stubEngine{})
	w := invokeInstance(t, deps, http.MethodGet, "/v1/instances?status=bogus", "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleListInstances_PriceRoundedToCents: rows written before the
// RetailPrice formula carry 3-decimal rates (1.188, 13.728); the wire shape
// rounds to whole cents so customers see consistent precision.
func TestHandleListInstances_PriceRoundedToCents(t *testing.T) {
	instStore := newStubInstanceStore()
	instStore.put(&db.Instance{
		InstanceID: "gpu-legacy", OrgID: "org-A", UserID: "user-A",
		GPUType: "a100_80gb", GPUCount: 1, Tier: "on_demand", Region: "ca-central",
		Status: provision.StateRunning, PricePerHour: 1.188, CreatedAt: time.Now().UTC(),
	})
	deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})

	w := invokeInstance(t, deps, http.MethodGet, "/v1/instances", "org-A", "user-A", "ak-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var page public.Page[public.Instance]
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].PricePerHour != 1.19 {
		t.Errorf("price: got %+v, want price_per_hour 1.19", page.Data)
	}
}

// ---------------------------------------------------------------------------
// Phase 39 Plan 05 (SEC-05/SEC-09, D-04): member-scoped public terminate via
// the shared authorization rule + actor-attributed instance.terminated audit
// emit on the public surface.
// ---------------------------------------------------------------------------

// stubAuditStore captures CreateAuditEntry calls so tests can assert the
// public-surface terminate audit emit (SEC-09) without a live Postgres.
type stubAuditStore struct {
	mu      sync.Mutex
	entries []db.AuditEntry
}

func (s *stubAuditStore) CreateAuditEntry(_ context.Context, entry *db.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, *entry)
	return nil
}

func (s *stubAuditStore) snapshot() []db.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]db.AuditEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

// invokeInstanceAs mirrors invokeInstance but takes a fully-specified APIKey
// so tests can set Role (admin/member) and UserID independently — the
// member-scoping cases need role x ownership combinations the positional
// helper cannot express.
func invokeInstanceAs(t *testing.T, deps public.PublicDeps, method, target string, key *public.APIKey, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(public.WithAPIKey(req.Context(), key))

	mux := http.NewServeMux()
	public.InstanceHandlersForTesting(mux, deps)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// seedOwnedInstance puts a running instance owned by ownerUserID in org-A.
func seedOwnedInstance(store *stubInstanceStore, instanceID, orgID, ownerUserID string) {
	store.put(&db.Instance{
		InstanceID: instanceID, OrgID: orgID, UserID: ownerUserID,
		GPUType: "h100", GPUCount: 1, Tier: "on_demand", Region: "us-west",
		Status: "running", PricePerHour: 2.5, CreatedAt: time.Now().UTC(),
	})
}

// TestHandleDeleteInstance_MemberScopingEnforce pins the SEC-05 rule in
// ENFORCE mode (AuthzEnforce=true): members terminate only instances they
// created; admin+ terminates org-wide; cross-org stays 404 (D-07, never leak
// existence).
func TestHandleDeleteInstance_MemberScopingEnforce(t *testing.T) {
	cases := []struct {
		name       string
		key        *public.APIKey
		orgID      string // org that owns the seeded instance
		wantStatus int
		wantCode   string
	}{
		{
			name:       "member on own instance -> 202",
			key:        &public.APIKey{APIKeyID: "ak-1", OrgID: "org-A", UserID: "user-owner", Role: "member"},
			orgID:      "org-A",
			wantStatus: http.StatusAccepted,
		},
		{
			name:       "member on another user's instance -> 403 forbidden",
			key:        &public.APIKey{APIKeyID: "ak-2", OrgID: "org-A", UserID: "user-other", Role: "member"},
			orgID:      "org-A",
			wantStatus: http.StatusForbidden,
			wantCode:   "forbidden",
		},
		{
			name:       "admin on another user's instance -> 202 (org-wide)",
			key:        &public.APIKey{APIKeyID: "ak-3", OrgID: "org-A", UserID: "user-admin", Role: "admin"},
			orgID:      "org-A",
			wantStatus: http.StatusAccepted,
		},
		{
			name:       "owner-role key on another user's instance -> 202 (admin+)",
			key:        &public.APIKey{APIKeyID: "ak-4", OrgID: "org-A", UserID: "user-owner-role", Role: "owner"},
			orgID:      "org-A",
			wantStatus: http.StatusAccepted,
		},
		{
			name:       "cross-org admin -> 404 (never leak existence)",
			key:        &public.APIKey{APIKeyID: "ak-5", OrgID: "org-B", UserID: "user-b", Role: "admin"},
			orgID:      "org-A",
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			instStore := newStubInstanceStore()
			seedOwnedInstance(instStore, "gpu-T1", c.orgID, "user-owner")
			deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})
			deps = public.PublicDepsWithAuditStore(deps, &stubAuditStore{})
			deps.Config = &config.Config{AppsHost: "apps.example.com", AuthzEnforce: true}

			w := invokeInstanceAs(t, deps, http.MethodDelete, "/v1/instances/gpu-T1", c.key, nil)
			if w.Code != c.wantStatus {
				t.Fatalf("status: got %d, want %d; body=%s", w.Code, c.wantStatus, w.Body.String())
			}
			if c.wantStatus == http.StatusAccepted && w.Header().Get("Operation-Id") == "" {
				t.Errorf("Operation-Id header not set on allowed terminate (D-08)")
			}
			if c.wantCode != "" {
				var p public.Problem
				_ = json.NewDecoder(w.Body).Decode(&p)
				if p.Code != c.wantCode {
					t.Errorf("code: got %q, want %q", p.Code, c.wantCode)
				}
			}
		})
	}
}

// TestHandleDeleteInstance_MemberScopingShadow pins SEC-07 observe-only
// semantics: with AuthzEnforce=false a member terminating another user's
// instance is NOT blocked (202 proceeds) but a would_deny diff line is
// emitted for the soak grep.
func TestHandleDeleteInstance_MemberScopingShadow(t *testing.T) {
	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	instStore := newStubInstanceStore()
	seedOwnedInstance(instStore, "gpu-S1", "org-A", "user-owner")
	audit := &stubAuditStore{}
	deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})
	deps = public.PublicDepsWithAuditStore(deps, audit)
	deps.Config = &config.Config{AppsHost: "apps.example.com", AuthzEnforce: false}

	key := &public.APIKey{APIKeyID: "ak-9", OrgID: "org-A", UserID: "user-other", Role: "member"}
	w := invokeInstanceAs(t, deps, http.MethodDelete, "/v1/instances/gpu-S1", key, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("shadow mode must NOT block: got %d, want 202; body=%s", w.Code, w.Body.String())
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "authz decision") {
		t.Errorf("expected 'authz decision' shadow log line, got logs: %s", logs)
	}
	if !strings.Contains(logs, "would_deny=true") {
		t.Errorf("expected would_deny=true in shadow log, got logs: %s", logs)
	}
	if !strings.Contains(logs, "enforce=false") {
		t.Errorf("expected enforce=false in shadow log, got logs: %s", logs)
	}

	// The shadow-allowed terminate still proceeds -> it MUST be audited.
	entries := audit.snapshot()
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry for shadow-proceed terminate, got %d", len(entries))
	}
}

// TestHandlePatchInstance_MemberScopingEnforce pins the D-04 rule for
// ActionUpdate in ENFORCE mode on the public surface: members rename only
// instances they created; admin+ renames org-wide; cross-org stays 404 —
// keeping PATCH in lockstep with the dashboard handleUpdateInstance gate.
func TestHandlePatchInstance_MemberScopingEnforce(t *testing.T) {
	cases := []struct {
		name       string
		key        *public.APIKey
		wantStatus int
		wantCode   string
	}{
		{
			name:       "member on own instance -> 200",
			key:        &public.APIKey{APIKeyID: "ak-1", OrgID: "org-A", UserID: "user-owner", Role: "member"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "member on another user's instance -> 403 forbidden",
			key:        &public.APIKey{APIKeyID: "ak-2", OrgID: "org-A", UserID: "user-other", Role: "member"},
			wantStatus: http.StatusForbidden,
			wantCode:   "forbidden",
		},
		{
			name:       "admin on another user's instance -> 200 (org-wide)",
			key:        &public.APIKey{APIKeyID: "ak-3", OrgID: "org-A", UserID: "user-admin", Role: "admin"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "cross-org admin -> 404 (never leak existence)",
			key:        &public.APIKey{APIKeyID: "ak-4", OrgID: "org-B", UserID: "user-b", Role: "admin"},
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			instStore := newStubInstanceStore()
			seedOwnedInstance(instStore, "gpu-P1", "org-A", "user-owner")
			deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})
			deps.Config = &config.Config{AppsHost: "apps.example.com", AuthzEnforce: true}

			body := map[string]any{"name": "renamed"}
			w := invokeInstanceAs(t, deps, http.MethodPatch, "/v1/instances/gpu-P1", c.key, body)
			if w.Code != c.wantStatus {
				t.Fatalf("status: got %d, want %d; body=%s", w.Code, c.wantStatus, w.Body.String())
			}
			if c.wantCode != "" {
				var p public.Problem
				_ = json.NewDecoder(w.Body).Decode(&p)
				if p.Code != c.wantCode {
					t.Errorf("code: got %q, want %q", p.Code, c.wantCode)
				}
			}
		})
	}
}

// TestHandlePatchInstance_MemberScopingShadow pins SEC-07 observe-only
// semantics for the PATCH gate: with AuthzEnforce=false a member renaming
// another user's instance is NOT blocked (200 proceeds) but a would_deny
// diff line is emitted for the soak grep.
func TestHandlePatchInstance_MemberScopingShadow(t *testing.T) {
	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	instStore := newStubInstanceStore()
	seedOwnedInstance(instStore, "gpu-P2", "org-A", "user-owner")
	deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})
	deps.Config = &config.Config{AppsHost: "apps.example.com", AuthzEnforce: false}

	key := &public.APIKey{APIKeyID: "ak-9", OrgID: "org-A", UserID: "user-other", Role: "member"}
	body := map[string]any{"name": "renamed"}
	w := invokeInstanceAs(t, deps, http.MethodPatch, "/v1/instances/gpu-P2", key, body)
	if w.Code != http.StatusOK {
		t.Fatalf("shadow mode must NOT block: got %d, want 200; body=%s", w.Code, w.Body.String())
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "authz decision") {
		t.Errorf("expected 'authz decision' shadow log line, got logs: %s", logs)
	}
	if !strings.Contains(logs, "would_deny=true") {
		t.Errorf("expected would_deny=true in shadow log, got logs: %s", logs)
	}
}

// TestHandleDeleteInstance_AuditEmit pins SEC-09 on the public surface: every
// ALLOWED terminate writes an instance.terminated audit entry with the API
// key's user as actor (non-nil) and metadata source="user_api" — synchronous
// in the handler, distinguishable from the dashboard emit (source="user") and
// the system cap-kill (nil actor).
func TestHandleDeleteInstance_AuditEmit(t *testing.T) {
	instStore := newStubInstanceStore()
	seedOwnedInstance(instStore, "gpu-A9", "org-A", "user-owner")
	audit := &stubAuditStore{}
	deps := instanceTestDeps(newStubOperationStore(), instStore, &stubEngine{})
	deps = public.PublicDepsWithAuditStore(deps, audit)
	deps.Config = &config.Config{AppsHost: "apps.example.com", AuthzEnforce: true}

	key := &public.APIKey{APIKeyID: "ak-7", OrgID: "org-A", UserID: "user-owner", Role: "member"}
	w := invokeInstanceAs(t, deps, http.MethodDelete, "/v1/instances/gpu-A9", key, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202; body=%s", w.Code, w.Body.String())
	}

	entries := audit.snapshot()
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 audit entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Action != "instance.terminated" {
		t.Errorf("action: got %q, want instance.terminated", e.Action)
	}
	if e.UserID == nil || *e.UserID != "user-owner" {
		t.Errorf("actor: got %v, want non-nil user-owner", e.UserID)
	}
	if e.OrgID != "org-A" {
		t.Errorf("org_id: got %q, want org-A", e.OrgID)
	}
	if e.ResourceType == nil || *e.ResourceType != "instance" {
		t.Errorf("resource_type: got %v, want instance", e.ResourceType)
	}
	if e.ResourceID == nil || *e.ResourceID != "gpu-A9" {
		t.Errorf("resource_id: got %v, want gpu-A9", e.ResourceID)
	}
	var meta map[string]any
	if err := json.Unmarshal(e.Metadata, &meta); err != nil {
		t.Fatalf("metadata unmarshal: %v", err)
	}
	if meta["source"] != "user_api" {
		t.Errorf("metadata source: got %v, want user_api", meta["source"])
	}

	// A DENIED terminate (enforce mode) must NOT be audited as terminated.
	denyKey := &public.APIKey{APIKeyID: "ak-8", OrgID: "org-A", UserID: "user-other", Role: "member"}
	w = invokeInstanceAs(t, deps, http.MethodDelete, "/v1/instances/gpu-A9", denyKey, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("deny status: got %d, want 403", w.Code)
	}
	if got := len(audit.snapshot()); got != 1 {
		t.Errorf("denied terminate must not add audit entries; got %d total", got)
	}
}

// TestLaunch_EnvironmentCapabilityGate (Phase 63, T-63-01) pins the
// environment-selection capability contract on POST /v1/instances:
//
//   - environment=nil            → accepted (server default, D-63-04)
//   - unrecognized value         → 400 from Validate() (cheap format check)
//   - "raw_vm" on a container-only offering  → 422 (provider cannot honor it)
//   - "certified" on a non-certifiable offering → 422
//
// The two capability subtests are catalog-dependent, so they seed an offering
// into the availability cache (Redis-gated, like the gpu_type-validation tests)
// and classify it via the AvailableOffering.DeploymentType / CertifiedImage
// fields added in Task 01. RED until Wave 2 (Plan 05) wires the gate: today the
// handler ignores EnvironmentSelection entirely, so a bad value is accepted
// (202/known-gpu) instead of rejected.
func TestLaunch_EnvironmentCapabilityGate(t *testing.T) {
	// nil environment → accepted (default). No AvailCache: gpu_type membership
	// check is skipped, so the request reaches the engine and returns 202. This
	// is a green guard against the gate rejecting the default path.
	t.Run("nil_environment_accepted", func(t *testing.T) {
		opStore := newStubOperationStore()
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-env-nil", Hostname: "gpu-env-nil.example.com"}}
		deps := instanceTestDeps(opStore, newStubInstanceStore(), engine)

		body := map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand"}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusAccepted {
			t.Fatalf("nil environment must be accepted: got %d, want 202; body=%s", w.Code, w.Body.String())
		}
	})

	// Unrecognized environment value → 400 from Validate() (format check,
	// independent of the catalog). RED: the field is currently ignored so the
	// request is accepted (202).
	t.Run("invalid_environment_value_400", func(t *testing.T) {
		opStore := newStubOperationStore()
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-env-bad", Hostname: "gpu-env-bad.example.com"}}
		deps := instanceTestDeps(opStore, newStubInstanceStore(), engine)

		body := map[string]any{
			"gpu_type":    "h100_sxm",
			"gpu_count":   1,
			"tier":        "on_demand",
			"environment": "banana",
		}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("unrecognized environment value must be 400 from Validate(): got %d; body=%s", w.Code, w.Body.String())
		}
	})

	// "raw_vm" on a container-only offering → 422. The seeded offering is
	// DeploymentType="container", so a raw-VM request cannot be honored.
	t.Run("raw_vm_on_container_only_422", func(t *testing.T) {
		opStore := newStubOperationStore()
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-env-c", Hostname: "gpu-env-c.example.com"}}
		deps := instanceTestDeps(opStore, newStubInstanceStore(), engine)
		deps.AvailCache = instanceTestAvailCache(t, []availability.AvailableOffering{
			{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.5, Region: "us-west", Tier: "on_demand", AvailableCount: 8,
				DeploymentType: "container", CertifiedImage: true},
		})

		body := map[string]any{
			"gpu_type":    "h100_sxm",
			"gpu_count":   1,
			"tier":        "on_demand",
			"environment": "raw_vm",
		}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("raw_vm on container-only offering must be 422: got %d; body=%s", w.Code, w.Body.String())
		}
	})

	// "certified" on a non-certifiable offering → 422. The seeded offering
	// is CertifiedImage=false (D-63-02: some providers cannot run the certified image).
	t.Run("certified_on_noncertifiable_422", func(t *testing.T) {
		opStore := newStubOperationStore()
		engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-env-j", Hostname: "gpu-env-j.example.com"}}
		deps := instanceTestDeps(opStore, newStubInstanceStore(), engine)
		deps.AvailCache = instanceTestAvailCache(t, []availability.AvailableOffering{
			{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.5, Region: "us-west", Tier: "on_demand", AvailableCount: 8,
				DeploymentType: "container", CertifiedImage: false},
		})

		body := map[string]any{
			"gpu_type":    "h100_sxm",
			"gpu_count":   1,
			"tier":        "on_demand",
			"environment": "certified",
		}
		w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", body)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("certified on non-certifiable offering must be 422: got %d; body=%s", w.Code, w.Body.String())
		}
	})
}
