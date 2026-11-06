package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// newOperationsTestPool gates Postgres-backed tests on DATABASE_TEST_URL,
// matching the existing pattern in api_keys_test.go and instances_test.go.
// CI sets the env var via the public-api workflow (Plan 18-03); a local
// `go test` without the var skips the test rather than failing.
func newOperationsTestPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_TEST_URL")
	if url == "" {
		t.Skip("DATABASE_TEST_URL not set")
	}
	pool, err := db.NewPool(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return pool
}

// seedAPIKeyForOps creates the (org, user, api_key) tuple required by
// CreateOperation's NOT NULL FK columns. Each test gets a unique key (its
// hash includes time.Now().UnixNano() suffix) so concurrent test runs do
// not collide on the api_keys.key_hash UNIQUE index.
func seedAPIKeyForOps(t *testing.T, p *db.Pool, label string) (apiKeyID, orgID string) {
	t.Helper()
	ctx := context.Background()
	clerkOrg := fmt.Sprintf("test-org-ops-%s-%d", label, time.Now().UnixNano())
	clerkUser := fmt.Sprintf("test-user-ops-%s-%d", label, time.Now().UnixNano())
	orgID, userID, err := p.EnsureOrgAndUser(ctx, clerkOrg, clerkUser, "ops-"+label+"@example.com", "Ops", "", "")
	if err != nil {
		t.Fatalf("seed org+user: %v", err)
	}
	k := &db.APIKey{
		OrgID:     orgID,
		UserID:    userID,
		Name:      "ops-test-" + label,
		KeyHash:   fmt.Sprintf("o%s%055d", label[:1], time.Now().UnixNano()%1e10),
		KeyPrefix: "gpuai_live_ops" + label[:1],
		Last4:     "tst1",
		Scopes:    json.RawMessage(`{"full_access":true}`),
	}
	if err := p.CreateAPIKey(ctx, k); err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	return k.APIKeyID, orgID
}

func TestCreateAndGetOperation(t *testing.T) {
	p := newOperationsTestPool(t)
	defer p.Close()
	ctx := context.Background()
	apiKeyID, orgID := seedAPIKeyForOps(t, p, "create")

	op := &db.Operation{
		APIKeyID: apiKeyID,
		OrgID:    orgID,
		Kind:     "instance.create",
		Metadata: json.RawMessage(`{"gpu_type":"h100"}`),
	}
	if err := p.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create: %v", err)
	}
	if op.OperationID == "" {
		t.Fatal("OperationID not populated by RETURNING")
	}
	if op.State != "pending" {
		t.Errorf("state: got %q, want pending", op.State)
	}
	if op.Version != 1 {
		t.Errorf("version: got %d, want 1", op.Version)
	}

	// Round-trip GET succeeds for the issuing key.
	got, err := p.GetOperation(ctx, op.OperationID, apiKeyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OperationID != op.OperationID || got.State != "pending" {
		t.Errorf("got back %+v, want id=%s state=pending", got, op.OperationID)
	}
	if got.OrgID != orgID {
		t.Errorf("org_id: got %q, want %q", got.OrgID, orgID)
	}

	// Cross-key access returns ErrNotFound (issuer-scoped).
	otherKeyID, _ := seedAPIKeyForOps(t, p, "other")
	if _, err := p.GetOperation(ctx, op.OperationID, otherKeyID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("cross-key access: got %v, want ErrNotFound", err)
	}

	// Bogus operation id under issuing key still returns ErrNotFound.
	if _, err := p.GetOperation(ctx, "00000000-0000-0000-0000-000000000000", apiKeyID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("bogus op id: got %v, want ErrNotFound", err)
	}
}

func TestUpdateOperationState(t *testing.T) {
	p := newOperationsTestPool(t)
	defer p.Close()
	ctx := context.Background()
	apiKeyID, orgID := seedAPIKeyForOps(t, p, "update")

	op := &db.Operation{
		APIKeyID: apiKeyID, OrgID: orgID, Kind: "instance.create",
		Metadata: json.RawMessage(`{}`),
	}
	if err := p.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create: %v", err)
	}

	// pending -> in_progress; version: 1 -> 2.
	v2, err := p.UpdateOperationState(ctx, op.OperationID, op.Version, "in_progress")
	if err != nil {
		t.Fatalf("pending->in_progress: %v", err)
	}
	if v2 != 2 {
		t.Errorf("version after first update: got %d, want 2", v2)
	}
	op.Version = v2

	// in_progress -> succeeded; version: 2 -> 3, completed_at populated.
	v3, err := p.UpdateOperationState(ctx, op.OperationID, op.Version, "succeeded")
	if err != nil {
		t.Fatalf("in_progress->succeeded: %v", err)
	}
	if v3 != 3 {
		t.Errorf("version after succeeded: got %d, want 3", v3)
	}
	op.Version = v3

	// Read back to confirm persistence + completed_at set on terminal state.
	got, err := p.GetOperation(ctx, op.OperationID, apiKeyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != "succeeded" {
		t.Errorf("state: got %q, want succeeded", got.State)
	}
	if got.CompletedAt == nil {
		t.Errorf("completed_at: got nil, want populated on succeeded")
	}
	if got.Version != 3 {
		t.Errorf("persisted version: got %d, want 3", got.Version)
	}
}

func TestUpdateOperationState_Conflict(t *testing.T) {
	p := newOperationsTestPool(t)
	defer p.Close()
	ctx := context.Background()
	apiKeyID, orgID := seedAPIKeyForOps(t, p, "conflict")

	op := &db.Operation{
		APIKeyID: apiKeyID, OrgID: orgID, Kind: "instance.create",
		Metadata: json.RawMessage(`{}`),
	}
	if err := p.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create: %v", err)
	}

	// First UPDATE wins, returns version=2.
	if _, err := p.UpdateOperationState(ctx, op.OperationID, op.Version, "in_progress"); err != nil {
		t.Fatalf("first update: %v", err)
	}

	// Second UPDATE with the now-stale version=1 must surface ErrOperationStateChanged.
	v, err := p.UpdateOperationState(ctx, op.OperationID, 1, "succeeded")
	if !errors.Is(err, db.ErrOperationStateChanged) {
		t.Fatalf("stale update: got %v (v=%d), want ErrOperationStateChanged", err, v)
	}
	if v != 0 {
		t.Errorf("version on conflict: got %d, want 0", v)
	}
}

func TestSetOperationFailure(t *testing.T) {
	p := newOperationsTestPool(t)
	defer p.Close()
	ctx := context.Background()
	apiKeyID, orgID := seedAPIKeyForOps(t, p, "failure")

	op := &db.Operation{
		APIKeyID: apiKeyID, OrgID: orgID, Kind: "instance.create",
		Metadata: json.RawMessage(`{}`),
	}
	if err := p.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create: %v", err)
	}

	v, err := p.SetOperationFailure(ctx, op.OperationID, op.Version, "operation_failed", "engine: no capacity")
	if err != nil {
		t.Fatalf("set failure: %v", err)
	}
	if v != 2 {
		t.Errorf("version after failure: got %d, want 2", v)
	}

	got, err := p.GetOperation(ctx, op.OperationID, apiKeyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != "failed" {
		t.Errorf("state: got %q, want failed", got.State)
	}
	if got.ErrorCode == nil || *got.ErrorCode != "operation_failed" {
		t.Errorf("error_code: got %v, want 'operation_failed'", got.ErrorCode)
	}
	if got.ErrorDetail == nil || *got.ErrorDetail != "engine: no capacity" {
		t.Errorf("error_detail: got %v, want 'engine: no capacity'", got.ErrorDetail)
	}
	if got.CompletedAt == nil {
		t.Error("completed_at: got nil, want populated on failed")
	}

	// Stale-version conflict path.
	if _, err := p.SetOperationFailure(ctx, op.OperationID, 1, "operation_failed", "stale"); !errors.Is(err, db.ErrOperationStateChanged) {
		t.Errorf("stale failure: got %v, want ErrOperationStateChanged", err)
	}
}

func TestSetOperationResource_BumpsVersion(t *testing.T) {
	p := newOperationsTestPool(t)
	defer p.Close()
	ctx := context.Background()
	apiKeyID, orgID := seedAPIKeyForOps(t, p, "resource")

	op := &db.Operation{
		APIKeyID: apiKeyID, OrgID: orgID, Kind: "instance.create",
		Metadata: json.RawMessage(`{}`),
	}
	if err := p.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create: %v", err)
	}

	v2, err := p.SetOperationResource(ctx, op.OperationID, op.Version, "gpu-abc123")
	if err != nil {
		t.Fatalf("set resource: %v", err)
	}
	if v2 != 2 {
		t.Errorf("version after resource: got %d, want 2", v2)
	}
	op.Version = v2

	got, err := p.GetOperation(ctx, op.OperationID, apiKeyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ResourceID == nil || *got.ResourceID != "gpu-abc123" {
		t.Errorf("resource_id: got %v, want 'gpu-abc123'", got.ResourceID)
	}
	// State should NOT have changed — SetOperationResource only writes resource_id.
	if got.State != "pending" {
		t.Errorf("state: got %q, want pending (resource set must not change state)", got.State)
	}

	// Stale-version conflict path.
	if _, err := p.SetOperationResource(ctx, op.OperationID, 1, "gpu-stale"); !errors.Is(err, db.ErrOperationStateChanged) {
		t.Errorf("stale resource: got %v, want ErrOperationStateChanged", err)
	}
}

// TestCreateOperationHonoursASuppliedID pins the COALESCE seam against a real
// Postgres, because the seam is in the SQL and nowhere else.
//
// The auto-launch firer commits to an operation id, writes it into
// autolaunch_rules.launching_operation_id BEFORE any provider call, and then
// creates the operation under that same id. If the INSERT quietly generated its
// own id instead, the durable link would point at a row that does not exist and
// the recovery path would read "this rule never got that far" about a rule that
// bought a GPU.
func TestCreateOperationHonoursASuppliedID(t *testing.T) {
	pool := newOperationsTestPool(t)
	ctx := context.Background()
	apiKeyID, orgID := seedAPIKeyForOps(t, pool, "supplied")

	supplied := "3f2504e0-4f89-11d3-9a0c-" + fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	op := &db.Operation{
		OperationID: supplied,
		APIKeyID:    apiKeyID,
		OrgID:       orgID,
		Kind:        "instance.create",
		Metadata:    json.RawMessage(`{"supplied":true}`),
	}
	if err := pool.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create with supplied id: %v", err)
	}
	if op.OperationID != supplied {
		t.Fatalf("operation id = %q, want the supplied %q", op.OperationID, supplied)
	}
	if op.State != "pending" || op.Version != 1 {
		t.Fatalf("supplied-id insert did not produce the pending/version=1 baseline: state=%q version=%d", op.State, op.Version)
	}
	got, err := pool.GetOperation(ctx, supplied, apiKeyID)
	if err != nil {
		t.Fatalf("read back the supplied id: %v", err)
	}
	if got.OperationID != supplied {
		t.Fatalf("stored id = %q, want %q", got.OperationID, supplied)
	}

	// And the default path is untouched: an empty id still gets one from the
	// column default, which is what every HTTP caller relies on.
	assigned := &db.Operation{
		APIKeyID: apiKeyID,
		OrgID:    orgID,
		Kind:     "instance.create",
		Metadata: json.RawMessage(`{"supplied":false}`),
	}
	if err := pool.CreateOperation(ctx, assigned); err != nil {
		t.Fatalf("create without a supplied id: %v", err)
	}
	if assigned.OperationID == "" || assigned.OperationID == supplied {
		t.Fatalf("assigned id = %q, want a fresh generated uuid", assigned.OperationID)
	}
}
