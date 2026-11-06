package provision

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/tunnel"
	"github.com/jackc/pgx/v5/pgconn"
)

// freeTCPPort asks the OS for an unused TCP port, then releases it. The frps
// service in tunnel.NewManager binds eagerly, so the gated test needs a port
// that is almost certainly free (a fixed 7000 collides with a running gpuctl).
// There is a benign TOCTOU window between releasing here and NewManager binding;
// for a single local test run that is acceptable.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// TestProvision_PortConflict_PersistFails_TerminatesUpstream is the
// DATABASE_TEST_URL-gated proof of the BLOCK-03 / D-05 blast radius. It replaces
// the deferred TestPortAllocationAtomic, which asserted a single-tx port↔row
// atomicity that D-05 deliberately does NOT implement (holding a tx across the
// slow upstream HTTP call would exhaust the pgx pool — Pitfall 1).
//
// FORM IMPLEMENTED: the v7-index-rejection invariant, exercised against a REAL
// Provision through a real *tunnel.Manager + real *db.Pool, plus a direct
// duplicate-active-port INSERT that must be rejected. Rationale (documented per
// the plan): the FULL provision-path port-conflict cannot be triggered
// DETERMINISTICALLY in a single-threaded test, because AllocatePort skips any
// port held by an active instance and the v7 partial unique index uses the SAME
// "status NOT IN ('terminated','error')" predicate — so any active row at port P
// is also visible to AllocatePort's availability check, making it skip P (it
// never collides via normal seeding). The genuine collision is a
// concurrent-allocator race (two provisions landing on the same round-robin
// cursor value before either writes its row); reproducing
// that deterministically would need a mid-Provision injection hook, which is not
// a seam worth adding to production for this proof. So this test proves the two
// load-bearing facts compositionally:
//
//	(1) the REAL port path works end-to-end (real Manager + real pool): a
//	    successful Provision allocates a port and persists it on the instance row;
//	(2) the v7 partial unique index instances_frp_remote_port_active REJECTS a
//	    second ACTIVE row holding the same port (SQLSTATE 23505) — this is exactly
//	    the DB-level backstop that converts a concurrent-allocator collision into a
//	    CreateInstance failure.
//
// The terminate-on-failure response to (2)'s CreateInstance error is proven
// DETERMINISTICALLY by TestProvision_PersistFailure_TerminatesUpstream (fake-DB
// unit test, 4a), which fires on ANY CreateInstance error. (1)+(2)+4a jointly
// establish the full chain: race -> unique violation -> CreateInstance error ->
// terminate-on-failure -> no paid orphan; the reconciler (Plan 32-05) is the
// residual backstop.
//
// Hermeticity (this runs under `go test -race ./...` against a SHARED CI
// Postgres): all upstream ids are stamped unique so a leftover or parallel row
// cannot pre-trip a unique index; the stub returns a non-nil "running" status so
// the detached progressStatus goroutine cannot nil-panic and crash the suite;
// the backstop reuses instance #1's IN-RANGE allocated port (a duplicate ACTIVE
// row at that port is what must trip the unique index); and the orphan count is
// org-scoped.
//
// Skips cleanly when DATABASE_TEST_URL is unset.
func TestProvision_PortConflict_PersistFails_TerminatesUpstream(t *testing.T) {
	url := os.Getenv("DATABASE_TEST_URL")
	if url == "" {
		t.Skip("DATABASE_TEST_URL not set")
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	stamp := time.Now().Format("20060102150405.000000000")
	clerkOrgID := "test_org_portconflict_" + stamp
	clerkUserID := "test_user_portconflict_" + stamp
	orgID, userID, err := pool.EnsureOrgAndUser(ctx, clerkOrgID, clerkUserID, "portconflict-test@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org+user: %v", err)
	}
	// Cleanup the whole org. instances and its sibling child tables
	// (billing_sessions, usage_records, instance_events) are ON DELETE RESTRICT
	// against BOTH organizations and instances, so they must be cleared before
	// the parent rows or the deletes fail and the rows leak — Provision opens a
	// billing_sessions row synchronously (BLOCK-07), which is exactly what made
	// the old single-DELETE cleanup fail silently and pollute the shared CI DB.
	// organizations -> users/ssh_keys is ON DELETE CASCADE, so the final org
	// delete sweeps those.
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM billing_sessions WHERE org_id = $1`,
			`DELETE FROM usage_records WHERE org_id = $1`,
			`DELETE FROM instance_events WHERE org_id = $1`,
			`DELETE FROM instances WHERE org_id = $1`,
			`DELETE FROM organizations WHERE organization_id = $1`,
		} {
			_, _ = pool.PgxPool().Exec(bg, q, orgID)
		}
	})

	// Seed an org SSH key so the "at least one SSH key required" gate passes.
	sshKey := &db.SSHKey{
		OrgID:       orgID,
		UserID:      userID,
		Name:        "test-portconflict-key",
		PublicKey:   "ssh-ed25519 AAAAC3test test@portconflict",
		Fingerprint: "SHA256:portconflict-" + stamp,
	}
	if err := pool.CreateSSHKey(ctx, sshKey); err != nil {
		t.Fatalf("seed ssh key: %v", err)
	}

	// Real *tunnel.Manager so the engine takes the FRP-port tx branch
	// (tunnelMgr != nil). NewManager binds the frps listen socket at construction
	// (frpserver.NewService opens it eagerly), so we pick a free ephemeral bind
	// port to avoid colliding with a gpuctl already running on this host. The
	// AllocatePort path only needs the real *pgxpool.Pool, not a running frps.
	bindPort := freeTCPPort(t)
	tm, err := tunnel.NewManager(bindPort, "", "10000-10255", slog.Default())
	if err != nil {
		t.Fatalf("tunnel.NewManager(bindPort=%d): %v", bindPort, err)
	}
	t.Cleanup(func() { _ = tm.Close() })

	stub := &stubProvider{
		name: "stub",
		offerings: []provider.GPUOffering{{
			Provider:       "stub",
			GPUType:        provider.GPUTypeA10080GB,
			GPUCount:       1,
			PricePerHour:   1.5,
			Tier:           provider.TierOnDemand,
			Region:         "us-west",
			AvailableCount: 1,
		}},
		provisionResult: &provider.ProvisionResult{
			// Unique per run. idx_instances_upstream_unique is a FULL unique
			// index on (upstream_provider, upstream_id) — a fixed id collides
			// with a leftover row from a prior run (or any parallel package on
			// the shared CI test DB) and fails Provision at persist BEFORE this
			// test can exercise the frp-port backstop at all.
			UpstreamID:  "up-pc-primary-" + stamp,
			UpstreamIP:  "10.0.0.7",
			Status:      "creating",
			CostPerHour: 1.0,
			Region:      "us-west",
		},
		// A non-nil GetStatus result is REQUIRED. Provision spawns a DETACHED
		// progressStatus goroutine that polls GetStatus every 5s; with a nil
		// result it dereferences status.Status and panics. In a full
		// `go test ./...` run (the CI invocation) the provision package binary
		// outlives the 5s tick, so that panic crashes the binary mid-suite and
		// skips this test's t.Cleanup — leaking rows that then break later runs.
		// A valid "running" status lets the goroutine drive
		// creating->provisioning->booting->running (all ACTIVE, so instance #1
		// keeps holding the port for the backstop below) and then exit cleanly.
		statusResult: &provider.InstanceStatus{
			Status: "running",
			IP:     "10.0.0.7",
		},
	}
	reg := provider.NewRegistry()
	reg.Register(stub)

	engine := &Engine{
		registry: reg,
		db:       pool,
		// FRP bootstrap fields must be non-empty/positive — RenderBootstrap
		// validates ProxyHost / FRPServerPort / FRPToken. Values are inert here
		// (no frpc actually connects); they only satisfy the bootstrap template.
		config: &config.Config{
			FRPPublicHost: "frps.test.local",
			FRPBindPort:   bindPort,
			FRPToken:      "test-frp-token",
		},
		logger:    slog.Default(),
		tunnelMgr: tm,
		// Struct-literal construction bypasses NewEngine's 10-minute default,
		// leaving provisionTimeout zero — and time.After(0) fires the timeout
		// watchdog (terminateStuckUpstream + SetInstanceError) immediately,
		// racing the pinned-running assertions below: whichever UPDATE lands
		// last decides the run. Set it explicitly so the watchdog cannot fire
		// within this test's lifetime.
		provisionTimeout: 10 * time.Minute,
	}

	req := ProvisionRequest{
		OrgID:    orgID,
		UserID:   userID,
		GPUType:  provider.GPUTypeA10080GB,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
	}

	// (1) A real Provision through the real Manager + pool exercises the FRP-port
	// tx: AllocatePort + CreateInstance persisting the allocated port. Assert it
	// succeeds and the row carries the port.
	resp, err := engine.Provision(ctx, req)
	if err != nil {
		t.Fatalf("Provision (real port path): %v", err)
	}
	if resp == nil {
		t.Fatal("Provision returned nil response")
	}

	inst, err := pool.GetInstance(ctx, resp.InstanceID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.FRPRemotePort == nil {
		t.Fatal("expected the provisioned instance to carry an allocated FRP remote port, got nil")
	}
	allocatedPort := *inst.FRPRemotePort
	if allocatedPort < tunnel.MinPort || allocatedPort > tunnel.MaxPort {
		t.Fatalf("allocated port %d outside [%d,%d]", allocatedPort, tunnel.MinPort, tunnel.MaxPort)
	}

	// Pin instance #1 to a stable active status before exercising the index
	// backstop. Provision spawns a DETACHED progressStatus goroutine
	// (engine.go: `go e.progressStatus(instanceID)`) that drives
	// creating->provisioning->... on its own schedule with a context.Background().
	// The assertions below require instance #1 to remain an active row holding
	// allocatedPort; under a loaded scheduler (e.g. a parallel package on the
	// shared CI test DB also running Provision) that goroutine interleaves with
	// these synchronous assertions and the row's status at assert-time becomes
	// non-deterministic — which is exactly the flake this test exhibited.
	// Forcing 'running' here is a synchronous write on the test goroutine that
	// lands the row squarely inside the partial index's active predicate
	// (status NOT IN ('terminated','error')) and makes the async progression a
	// no-op: its creating->provisioning CAS can no longer match. This removes the
	// race without weakening the backstop (instance #1 is still a real, active,
	// port-holding row created by the real AllocatePort+CreateInstance path above).
	if _, err := pool.PgxPool().Exec(ctx,
		`UPDATE instances SET status = $1 WHERE instance_id = $2`,
		StateRunning, resp.InstanceID); err != nil {
		t.Fatalf("pin instance #1 to a stable active status: %v", err)
	}

	// (2) v7 backstop: a SECOND active instance row holding the SAME port must be
	// rejected by the partial unique index instances_frp_remote_port_active. This
	// is exactly the failure a concurrent-allocator race produces, and it lands as
	// a CreateInstance error -> terminate-on-failure (proven deterministically by
	// TestProvision_PersistFailure_TerminatesUpstream).
	dupID, err := generateInstanceID()
	if err != nil {
		t.Fatalf("generate dup id: %v", err)
	}
	internalTok := "dup-internal-token"
	dup := &db.Instance{
		InstanceID:       dupID,
		OrgID:            orgID,
		UserID:           userID,
		UpstreamProvider: "stub",
		// Unique upstream id so the ONLY unique constraint this row can violate
		// is the frp-port index. A fixed id risks tripping
		// idx_instances_upstream_unique instead — also SQLSTATE 23505 — which
		// would let the assertion below pass for the WRONG reason.
		UpstreamID:    "up-pc-dup-" + stamp,
		Hostname:      dupID + ".example.com",
		FRPRemotePort: &allocatedPort, // SAME active port -> must violate the index
		GPUType:       string(provider.GPUTypeA10080GB),
		GPUCount:      1,
		Tier:          string(provider.TierOnDemand),
		Region:        "us-west",
		PricePerHour:  1.5,
		Status:        StateRunning, // active -> inside the partial index predicate
		InternalToken: &internalTok,
	}
	dupErr := pool.CreateInstance(ctx, dup)
	if dupErr == nil {
		// Clean up the row we did NOT expect to be created.
		_, _ = pool.PgxPool().Exec(ctx, `DELETE FROM instances WHERE instance_id = $1`, dupID)
		t.Fatalf("expected the v7 partial unique index to REJECT a second active row at port %d, but CreateInstance succeeded", allocatedPort)
	}
	// Assert it is SPECIFICALLY the frp-port partial unique index that fired
	// (SQLSTATE 23505 + the constraint name), not some other unique violation.
	// The dup's unique upstream id already rules out idx_instances_upstream_unique;
	// pinning the constraint name keeps this proof bound to the v7 backstop.
	var pgErr *pgconn.PgError
	if !errors.As(dupErr, &pgErr) || pgErr.SQLState() != "23505" {
		t.Fatalf("expected a unique-violation (SQLSTATE 23505) on frp_remote_port, got %v", dupErr)
	}
	if pgErr.ConstraintName != "instances_frp_remote_port_active" {
		t.Fatalf("expected the violation on the v7 frp-port index instances_frp_remote_port_active, got constraint %q (%v)",
			pgErr.ConstraintName, dupErr)
	}

	// (3) No orphaned active row beyond the one successful provision: exactly one
	// active row holds the conflicting port.
	var activeAtPort int
	if err := pool.PgxPool().QueryRow(ctx,
		// Scoped to THIS org. The frp index is global, but on the shared CI test
		// DB this count must not be perturbed by unrelated orgs' rows.
		`SELECT COUNT(*) FROM instances
		 WHERE frp_remote_port = $1 AND org_id = $2 AND status NOT IN ('terminated','error')`,
		allocatedPort, orgID,
	).Scan(&activeAtPort); err != nil {
		t.Fatalf("count active rows at port: %v", err)
	}
	if activeAtPort != 1 {
		t.Fatalf("expected exactly 1 active row holding port %d for this org (no orphan from the rejected dup), got %d",
			allocatedPort, activeAtPort)
	}
}
