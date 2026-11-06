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
	"github.com/gpuai-dev/gpuai-oss/internal/tunnel"
)

// TestWebConsoleAvailable pins the reachability predicate that lets a keyless
// launch through the SSH-key gate: the web console requires the FRP bootstrap
// (tunnelMgr), the ops kill switch off, and no template-app collision on the
// ttyd port. Every false case here means "keyless would be an unreachable
// brick" and Provision must keep refusing with ErrSSHKeysNotFound (covered by
// TestProvision_AllToggledOff_FailsClosed and the failure-ledger test, both of
// which run with tunnelMgr == nil).
func TestWebConsoleAvailable(t *testing.T) {
	bindPort := freeTCPPort(t)
	tm, err := tunnel.NewManager(bindPort, "", "10000-10255", slog.Default())
	if err != nil {
		t.Fatalf("tunnel.NewManager(bindPort=%d): %v", bindPort, err)
	}
	t.Cleanup(func() { _ = tm.Close() })

	cases := []struct {
		name     string
		mgr      *tunnel.Manager
		disabled bool
		appPort  int
		want     bool
	}{
		{"no tunnel manager", nil, false, 0, false},
		{"kill switch on", tm, true, 0, false},
		{"template app on the ttyd port", tm, false, webTerminalPort, false},
		{"raw instance", tm, false, 0, true},
		{"template app on another port", tm, false, 8888, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := &Engine{
				tunnelMgr: c.mgr,
				config:    &config.Config{WebTerminalDisabled: c.disabled},
			}
			if got := e.webConsoleAvailable(c.appPort); got != c.want {
				t.Errorf("webConsoleAvailable(%d) = %v, want %v", c.appPort, got, c.want)
			}
		})
	}
}

// TestProvision_ExplicitMissingKeys_RefusedEvenWithConsole: the keyless
// allowance applies ONLY to the auto-include branch. Explicitly requested
// ssh_key_ids that resolve to zero rows (deleted key, typo'd UUID in a launch
// script) keep failing with ErrSSHKeysNotFound even when the web console is
// available — the caller's stated intent is "provision with these keys" and we
// cannot satisfy it, so never silently bill for an instance whose SSH doesn't
// work. Uses a REAL tunnel manager so webConsoleAvailable is true, proving the
// refusal is branch-specific and not the console-off fallback.
func TestProvision_ExplicitMissingKeys_RefusedEvenWithConsole(t *testing.T) {
	bindPort := freeTCPPort(t)
	tm, err := tunnel.NewManager(bindPort, "", "10000-10255", slog.Default())
	if err != nil {
		t.Fatalf("tunnel.NewManager(bindPort=%d): %v", bindPort, err)
	}
	t.Cleanup(func() { _ = tm.Close() })

	stub := onDemandStub("up-explicit-missing", "us-west")
	fake := fakeWithKeys() // zero keys — every requested ID resolves to nothing
	reg := provider.NewRegistry()
	reg.Register(stub)
	e := &Engine{
		registry:  reg,
		db:        fake,
		config:    &config.Config{},
		logger:    slog.Default(),
		tunnelMgr: tm,
	}

	req := onDemandRequest()
	req.SSHKeyIDs = []string{"00000000-0000-0000-0000-000000000000"}

	_, err = e.Provision(context.Background(), req)
	if !errors.Is(err, ErrSSHKeysNotFound) {
		t.Fatalf("expected ErrSSHKeysNotFound for explicit unresolvable key IDs (console on), got: %v", err)
	}
	if len(stub.provisionReqs) != 0 {
		t.Errorf("provider.Provision must not be called, got %d calls", len(stub.provisionReqs))
	}
}

// TestProvision_Keyless_WebConsoleOn_Succeeds: with ZERO SSH keys on the org
// and a live tunnel manager (web console available), Provision no longer
// refuses with ErrSSHKeysNotFound — the customer opted into web-terminal-only
// access (the launch UI warns before submit). The provider must receive an
// empty key set and the instance row must carry the ttyd terminal port plus
// the app basic-auth credential, because the console is now the ONLY way in.
//
// Hermeticity mirrors TestProvision_PortConflict_PersistFails_TerminatesUpstream
// (same file conventions): stamped ids, non-nil stub status so the detached
// progressStatus goroutine exits cleanly, org-scoped cleanup of RESTRICT child
// tables before the parent rows. Skips cleanly when DATABASE_TEST_URL is unset.
func TestProvision_Keyless_WebConsoleOn_Succeeds(t *testing.T) {
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
	clerkOrgID := "test_org_keyless_" + stamp
	clerkUserID := "test_user_keyless_" + stamp
	orgID, userID, err := pool.EnsureOrgAndUser(ctx, clerkOrgID, clerkUserID, "keyless-test@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org+user: %v", err)
	}
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

	// Deliberately NO ssh_keys row seeded — the keyless path is the test.

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
			UpstreamID:  "up-keyless-" + stamp,
			UpstreamIP:  "10.0.0.8",
			Status:      "creating",
			CostPerHour: 1.0,
			Region:      "us-west",
		},
		statusResult: &provider.InstanceStatus{
			Status: "running",
			IP:     "10.0.0.8",
		},
	}
	reg := provider.NewRegistry()
	reg.Register(stub)

	engine := &Engine{
		registry: reg,
		db:       pool,
		config: &config.Config{
			FRPPublicHost: "frps.test.local",
			FRPBindPort:   bindPort,
			FRPToken:      "test-frp-token",
		},
		logger:           slog.Default(),
		tunnelMgr:        tm,
		provisionTimeout: 10 * time.Minute,
	}

	req := ProvisionRequest{
		OrgID:    orgID,
		UserID:   userID,
		GPUType:  provider.GPUTypeA10080GB,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
	}

	resp, err := engine.Provision(ctx, req)
	if err != nil {
		t.Fatalf("Provision (keyless, web console on): %v", err)
	}
	if resp == nil {
		t.Fatal("Provision returned nil response")
	}

	if len(stub.provisionReqs) != 1 {
		t.Fatalf("expected exactly 1 provider provision call, got %d", len(stub.provisionReqs))
	}
	if got := stub.provisionReqs[0].SSHPublicKeys; len(got) != 0 {
		t.Errorf("provider must receive no SSH keys on a keyless launch, got %v", got)
	}

	inst, err := pool.GetInstance(ctx, resp.InstanceID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.TerminalPort == nil {
		t.Error("keyless instance must carry the web-console terminal_port — the console is its only access path")
	}
	if inst.AppAuthUser == nil || *inst.AppAuthUser == "" || inst.AppAuthPass == nil || *inst.AppAuthPass == "" {
		t.Error("keyless instance must carry the app basic-auth credential for the web console")
	}
}
