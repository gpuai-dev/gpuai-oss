package provision

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// Plan 30-08 Task 1 — SSH cross-org assertion in Engine.Provision.
//
// Background: SSH keys are org-scoped (migration v5 added ssh_keys.org_id NOT NULL
// FK to organizations). The engine's Provision path already auto-includes only
// org-scoped keys when SSHKeyIDs is empty (it calls GetSSHKeysByOrgID(req.OrgID)).
// But when a caller passes explicit SSHKeyIDs the engine ONLY calls
// GetSSHKeysByIDs — which does not filter by org. A user who is a member of two
// orgs (A and B) could pass a key UUID owned by org B while provisioning under
// org A, and the engine would embed org B's public key into a tunnel that is
// allocated to org A's reservation.
//
// This is a defense-in-depth gate (the Clerk JWT already prevents wrong-org slot
// allocation), but reserved-cluster customers reasonably ask that their tunnels
// only ever authorize keys held by their own org. The assertion runs in the
// engine — not just the HTTP handler — so direct callers (CLI, future internal
// tooling) inherit the same guarantee. Applies to ALL tiers, not just reserved,
// because wrong-org keys are a security gap everywhere.
//
// All tests in this file are gated on DATABASE_TEST_URL (live Postgres) because
// the assertion runs after GetSSHKeysByIDs returns real ssh_keys rows — there is
// no useful mock seam there.

// seedTwoOrgsWithKey provisions two orgs (A and B) with one user and one SSH
// key each. Returns (orgAID, userAID, orgBID, userBID, keyAID, keyBID). Used by
// the cross-org assertion tests below.
func seedTwoOrgsWithKey(t *testing.T, ctx context.Context, pool *db.Pool, stamp string) (
	orgAID, userAID, orgBID, userBID, keyAID, keyBID string,
) {
	t.Helper()

	clerkOrgA := "test_org_xorg_a_" + stamp
	clerkUserA := "test_user_xorg_a_" + stamp
	var err error
	orgAID, userAID, err = pool.EnsureOrgAndUser(ctx, clerkOrgA, clerkUserA, "xorg-a-"+stamp+"@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org A + user A: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.PgxPool().Exec(context.Background(),
			`DELETE FROM organizations WHERE organization_id = $1`, orgAID)
	})

	clerkOrgB := "test_org_xorg_b_" + stamp
	clerkUserB := "test_user_xorg_b_" + stamp
	orgBID, userBID, err = pool.EnsureOrgAndUser(ctx, clerkOrgB, clerkUserB, "xorg-b-"+stamp+"@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org B + user B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.PgxPool().Exec(context.Background(),
			`DELETE FROM organizations WHERE organization_id = $1`, orgBID)
	})

	// SSH key owned by org A.
	keyA := &db.SSHKey{
		OrgID:       orgAID,
		UserID:      userAID,
		Name:        "key-a-" + stamp,
		PublicKey:   "ssh-ed25519 AAAAC3xorgA key-a@xorg",
		Fingerprint: "SHA256:xorg-a-" + stamp,
	}
	if err := pool.CreateSSHKey(ctx, keyA); err != nil {
		t.Fatalf("create ssh key A: %v", err)
	}
	keyAID = keyA.SSHKeyID

	// SSH key owned by org B.
	keyB := &db.SSHKey{
		OrgID:       orgBID,
		UserID:      userBID,
		Name:        "key-b-" + stamp,
		PublicKey:   "ssh-ed25519 AAAAC3xorgB key-b@xorg",
		Fingerprint: "SHA256:xorg-b-" + stamp,
	}
	if err := pool.CreateSSHKey(ctx, keyB); err != nil {
		t.Fatalf("create ssh key B: %v", err)
	}
	keyBID = keyB.SSHKeyID

	return orgAID, userAID, orgBID, userBID, keyAID, keyBID
}

// TestProvision_RejectsCrossOrgSSHKey is the security-floor assertion: a
// request for org A that lists an SSH key owned by org B must be rejected with
// ErrSSHKeyOrgMismatch BEFORE any side effects (no slot allocation, no FRP port,
// no provider.Provision, no instance row).
//
// Skips when DATABASE_TEST_URL is unset.
func TestProvision_RejectsCrossOrgSSHKey(t *testing.T) {
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
	orgAID, userAID, _, _, _, keyBID := seedTwoOrgsWithKey(t, ctx, pool, stamp)

	// Build engine with a stub registry + no reservation.Service — the
	// assertion runs BEFORE the tier branch, so reserved-vs-on-demand
	// doesn't matter. Use on-demand for the simpler code path.
	stub := &stubProvider{name: "stub"}
	reg := provider.NewRegistry()
	reg.Register(stub)

	engine := &Engine{
		registry: reg,
		db:       pool,
		config:   &config.Config{},
		logger:   slog.Default(),
	}

	// Request under org A, passing org B's key.
	req := ProvisionRequest{
		OrgID:     orgAID,
		UserID:    userAID,
		GPUType:   provider.GPUTypeH100SXM,
		GPUCount:  1,
		Tier:      provider.TierOnDemand,
		SSHKeyIDs: []string{keyBID},
	}

	_, err = engine.Provision(ctx, req)
	if !errors.Is(err, ErrSSHKeyOrgMismatch) {
		t.Fatalf("expected ErrSSHKeyOrgMismatch, got: %v", err)
	}

	// No instance row created.
	var count int
	if err := pool.PgxPool().QueryRow(ctx,
		`SELECT COUNT(*) FROM instances WHERE org_id = $1`, orgAID,
	).Scan(&count); err != nil {
		t.Fatalf("count instances: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 instance rows after cross-org SSH rejection, got %d", count)
	}
}

// TestProvision_RejectsMixedSSHKeys: even one wrong-org key in a multi-key
// request rejects the whole provision. No partial success.
func TestProvision_RejectsMixedSSHKeys(t *testing.T) {
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
	orgAID, userAID, _, _, keyAID, keyBID := seedTwoOrgsWithKey(t, ctx, pool, stamp)

	stub := &stubProvider{name: "stub"}
	reg := provider.NewRegistry()
	reg.Register(stub)

	engine := &Engine{
		registry: reg,
		db:       pool,
		config:   &config.Config{},
		logger:   slog.Default(),
	}

	// Mix: one valid key (key A, owned by org A) + one invalid (key B).
	req := ProvisionRequest{
		OrgID:     orgAID,
		UserID:    userAID,
		GPUType:   provider.GPUTypeH100SXM,
		GPUCount:  1,
		Tier:      provider.TierOnDemand,
		SSHKeyIDs: []string{keyAID, keyBID},
	}

	_, err = engine.Provision(ctx, req)
	if !errors.Is(err, ErrSSHKeyOrgMismatch) {
		t.Fatalf("expected ErrSSHKeyOrgMismatch even with one valid key in the mix, got: %v", err)
	}
}

// TestProvision_AllowsSameOrgSSHKey: control case — an explicit key owned by
// the requesting org passes the assertion. This exercises the happy path of
// the new check (loop completes without returning the typed error). We assert
// that the engine moves past the SSH-key block by checking the error is NOT
// ErrSSHKeyOrgMismatch — downstream errors (ErrNoProvider from an empty
// availability list) are fine.
func TestProvision_AllowsSameOrgSSHKey(t *testing.T) {
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
	orgAID, userAID, _, _, keyAID, _ := seedTwoOrgsWithKey(t, ctx, pool, stamp)

	// Stub has zero offerings → SelectProviderCandidates returns ErrNoProvider.
	// That's downstream of the SSH assertion — what we're asserting here is
	// that we reach the downstream path without tripping the assertion.
	stub := &stubProvider{name: "stub"}
	reg := provider.NewRegistry()
	reg.Register(stub)

	engine := &Engine{
		registry: reg,
		db:       pool,
		config:   &config.Config{},
		logger:   slog.Default(),
	}

	req := ProvisionRequest{
		OrgID:     orgAID,
		UserID:    userAID,
		GPUType:   provider.GPUTypeH100SXM,
		GPUCount:  1,
		Tier:      provider.TierOnDemand,
		SSHKeyIDs: []string{keyAID},
	}

	_, err = engine.Provision(ctx, req)
	if errors.Is(err, ErrSSHKeyOrgMismatch) {
		t.Fatalf("ErrSSHKeyOrgMismatch fired on same-org key — assertion is over-strict: %v", err)
	}
	// Downstream ErrNoProvider is expected and proves the assertion passed.
	if !errors.Is(err, ErrNoProvider) {
		t.Logf("Provision moved past SSH assertion (downstream error: %v)", err)
	}
}

// TestProvision_AutoIncludeSkipsAssertion: when SSHKeyIDs is empty, the engine
// calls GetSSHKeysByOrgID which is already org-scoped. The cross-org assertion
// is unnecessary in that branch (and would do nothing — every returned key is
// org-scoped). Verify the assertion doesn't fire when SSHKeyIDs is empty.
func TestProvision_AutoIncludeSkipsAssertion(t *testing.T) {
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
	orgAID, userAID, _, _, _, _ := seedTwoOrgsWithKey(t, ctx, pool, stamp)

	stub := &stubProvider{name: "stub"}
	reg := provider.NewRegistry()
	reg.Register(stub)

	engine := &Engine{
		registry: reg,
		db:       pool,
		config:   &config.Config{},
		logger:   slog.Default(),
	}

	// Empty SSHKeyIDs → engine auto-includes via GetSSHKeysByOrgID(orgA).
	// Only org-A keys come back, so the assertion is a no-op.
	req := ProvisionRequest{
		OrgID:    orgAID,
		UserID:   userAID,
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
	}

	_, err = engine.Provision(ctx, req)
	if errors.Is(err, ErrSSHKeyOrgMismatch) {
		t.Fatalf("auto-include path tripped ErrSSHKeyOrgMismatch — should never happen: %v", err)
	}
}
