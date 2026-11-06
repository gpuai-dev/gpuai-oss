package health

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// notFoundProvider is a fake Provider whose GetStatus reports that the upstream
// node is gone — exactly how a container adapter surfaces a hard-deleted pod
// ("pod <id> not found"). All other Provider methods are no-op stubs; the
// health monitor only calls GetStatus.
type notFoundProvider struct {
	name      string
	statusErr error
}

func (p *notFoundProvider) Name() string { return p.name }

func (p *notFoundProvider) ListAvailable(context.Context) ([]provider.GPUOffering, error) {
	return nil, nil
}

func (p *notFoundProvider) Provision(context.Context, provider.ProvisionRequest) (*provider.ProvisionResult, error) {
	return nil, provider.ErrNotImplemented
}

func (p *notFoundProvider) GetStatus(context.Context, string, string) (*provider.InstanceStatus, error) {
	return nil, p.statusErr
}

func (p *notFoundProvider) Terminate(context.Context, string, string) error { return nil }

func (p *notFoundProvider) ListProvisioned(context.Context) ([]provider.ProvisionedInstance, error) {
	return nil, provider.ErrNotImplemented
}

// TestIsNotFoundErr verifies the local not-found detector matches the messages
// adapters actually emit ("pod <id> not found" and a raw 404), and
// does NOT match transient failures that must keep a live instance running.
// Pure unit test — no DB required.
func TestIsNotFoundErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"pod not found", fmt.Errorf("pod abc123 not found"), true},
		{"http 404", fmt.Errorf("provider returned HTTP 404"), true},
		{"transient 500", fmt.Errorf("provider returned HTTP 500"), false},
		{"timeout", fmt.Errorf("context deadline exceeded"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNotFoundErr(tc.err); got != tc.want {
				t.Errorf("isNotFoundErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestCheckInstance_GhostClosesOut is the regression test for AUDIT-05-003:
// a running instance whose upstream pod is hard-deleted (GetStatus returns a
// not-found ERROR, not a synthetic "terminated" status) must be closed out
// instead of being reported as running forever.
//
// Skips when DATABASE_TEST_URL is unset (same guard as the sibling DB-backed
// tests in internal/provision) — the Monitor holds a concrete *db.Pool, so the
// closeout path can only be exercised against a real Postgres.
func TestCheckInstance_GhostClosesOut(t *testing.T) {
	url := testDatabaseURL(t)

	ctx := context.Background()
	pool, err := db.NewPool(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	suffix := time.Now().Format("20060102150405.000000000")
	orgID, userID, err := pool.EnsureOrgAndUser(ctx,
		"test_org_ghost_"+suffix, "test_user_ghost_"+suffix,
		"ghost-test@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org+user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.PgxPool().Exec(context.Background(),
			`DELETE FROM organizations WHERE organization_id = $1`, orgID)
	})

	// Seed a running instance whose upstream pod will report "not found".
	instanceID := newTestID(t)
	inst := &db.Instance{
		InstanceID:           instanceID,
		OrgID:                orgID,
		UserID:               userID,
		UpstreamProvider:     "ghostprov",
		UpstreamID:           "pod-" + instanceID,
		Hostname:             instanceID + ".example.com",
		GPUType:              "H100_SXM",
		GPUCount:             1,
		Tier:                 "on_demand",
		Region:               "us-east",
		PricePerHour:         2.50,
		UpstreamPricePerHour: 2.00,
		Status:               "running",
	}
	if err := pool.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	// Register a provider that reports the pod is gone, then run one check.
	reg := provider.NewRegistry()
	reg.Register(&notFoundProvider{
		name:      "ghostprov",
		statusErr: fmt.Errorf("pod %s not found", inst.UpstreamID),
	})

	var events []db.InstanceEvent
	m := &Monitor{
		db:       pool,
		registry: reg,
		logger:   slog.Default(),
		interval: time.Minute,
		onEvent:  func(e db.InstanceEvent) { events = append(events, e) },
	}
	m.checkInstance(ctx, inst)

	// Instance must no longer be running.
	got, err := pool.GetInstance(ctx, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if got.Status == "running" {
		t.Errorf("instance status = %q, want non-running (ghost should terminate it)", got.Status)
	}
	if got.Status != "error" {
		t.Errorf("instance status = %q, want %q", got.Status, "error")
	}
	if got.ErrorReason == nil || *got.ErrorReason == "" {
		t.Errorf("instance error_reason not set, want a ghost reason")
	}

	// A "failed" health event should have been emitted to SSE subscribers.
	if len(events) != 1 {
		t.Fatalf("got %d health events, want 1", len(events))
	}
	if events[0].EventType != "failed" {
		t.Errorf("event type = %q, want %q", events[0].EventType, "failed")
	}
}

// TestCheckInstance_TransientErrorKeepsRunning guards the other side of the
// fix: a transient poll failure (NOT not-found) must leave a running instance
// untouched, so a flaky provider poll can't kill a live instance.
//
// Skips when DATABASE_TEST_URL is unset.
func TestCheckInstance_TransientErrorKeepsRunning(t *testing.T) {
	url := testDatabaseURL(t)

	ctx := context.Background()
	pool, err := db.NewPool(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	suffix := time.Now().Format("20060102150405.000000000")
	orgID, userID, err := pool.EnsureOrgAndUser(ctx,
		"test_org_transient_"+suffix, "test_user_transient_"+suffix,
		"transient-test@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org+user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.PgxPool().Exec(context.Background(),
			`DELETE FROM organizations WHERE organization_id = $1`, orgID)
	})

	instanceID := newTestID(t)
	inst := &db.Instance{
		InstanceID:           instanceID,
		OrgID:                orgID,
		UserID:               userID,
		UpstreamProvider:     "ghostprov",
		UpstreamID:           "pod-" + instanceID,
		Hostname:             instanceID + ".example.com",
		GPUType:              "H100_SXM",
		GPUCount:             1,
		Tier:                 "on_demand",
		Region:               "us-east",
		PricePerHour:         2.50,
		UpstreamPricePerHour: 2.00,
		Status:               "running",
	}
	if err := pool.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	reg := provider.NewRegistry()
	reg.Register(&notFoundProvider{
		name:      "ghostprov",
		statusErr: fmt.Errorf("provider returned HTTP 503: upstream unavailable"),
	})

	m := &Monitor{
		db:       pool,
		registry: reg,
		logger:   slog.Default(),
		interval: time.Minute,
	}
	m.checkInstance(ctx, inst)

	got, err := pool.GetInstance(ctx, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if got.Status != "running" {
		t.Errorf("instance status = %q, want %q (transient error must not terminate)", got.Status, "running")
	}

}

// testDatabaseURL returns the DB URL for integration tests, or skips the test
// when it is unset (matching the guard used across the DB-backed suites).
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("DATABASE_TEST_URL")
	if url == "" {
		t.Skip("DATABASE_TEST_URL not set")
	}
	return url
}

// TestNewMonitorKnobDefaults pins the shipped defaults for every GPU-77 knob.
// They are Monitor fields rather than consts so an incident can retune the
// confirmation threshold, the grace, or the credit guardrails from the
// environment — but a deployment that sets none of them must behave exactly
// like the code that shipped before the knobs existed.
func TestNewMonitorKnobDefaults(t *testing.T) {
	m := NewMonitor(MonitorDeps{Logger: slog.Default()})

	if m.failThreshold != reachabilityFailThreshold {
		t.Errorf("failThreshold = %d, want %d", m.failThreshold, reachabilityFailThreshold)
	}
	if m.escalateAfter != unreachableEscalateAfter {
		t.Errorf("escalateAfter = %v, want %v", m.escalateAfter, unreachableEscalateAfter)
	}
	// The tracker must inherit the same resolved values, not re-derive them.
	if m.reachability.failThreshold != reachabilityFailThreshold {
		t.Errorf("tracker failThreshold = %d, want %d", m.reachability.failThreshold, reachabilityFailThreshold)
	}
}

func TestNewMonitorKnobOverrides(t *testing.T) {
	m := NewMonitor(MonitorDeps{
		Logger:        slog.Default(),
		FailThreshold: 5,
		EscalateAfter: 25 * time.Minute,
	})

	if m.failThreshold != 5 || m.escalateAfter != 25*time.Minute {
		t.Errorf("threshold/grace overrides not honored: %d / %v", m.failThreshold, m.escalateAfter)
	}
	if m.reachability.escalateAfter != 25*time.Minute {
		t.Errorf("tracker escalateAfter = %v, want the overridden 25m", m.reachability.escalateAfter)
	}
}

// newTestID returns a fresh instance id in the canonical "gpu-<8hex>" form
// (12 chars — instances.instance_id is varchar(12)), mirroring the engine's
// generateInstanceID.
func newTestID(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "gpu-" + hex.EncodeToString(b[:])
}
