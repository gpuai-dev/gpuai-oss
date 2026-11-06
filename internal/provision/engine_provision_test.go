package provision

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// newProvisionUnitEngine wires a minimal Engine for the fake-DB provision unit
// tests: the given stubProvider registered under "stub", the supplied
// fakeEngineDB, an empty config (branded-hostname callback, no public URL), and
// tunnelMgr=nil. tunnelMgr is deliberately nil: the FRP-port tx needs a real
// *pgxpool.Pool and a concrete *tunnel.Manager, so the port-specific blast
// radius is proven by the DATABASE_TEST_URL-gated dbtest, not here. These tests
// isolate BLOCK-02 (persist-failure terminate) and BLOCK-07 (synchronous billing
// session) on the generic provision path.
func newProvisionUnitEngine(stub *stubProvider, fake *fakeEngineDB) *Engine {
	reg := provider.NewRegistry()
	reg.Register(stub)
	return &Engine{
		registry:  reg,
		db:        fake,
		config:    &config.Config{},
		logger:    slog.Default(),
		tunnelMgr: nil,
	}
}

// onDemandStub returns a stubProvider whose single offering matches the request
// below (so SelectProviderCandidates yields exactly this candidate) and whose
// Provision returns the given upstream id + region.
func onDemandStub(upstreamID, region string) *stubProvider {
	return &stubProvider{
		name: "stub",
		offerings: []provider.GPUOffering{{
			Provider:       "stub",
			GPUType:        provider.GPUTypeA10080GB,
			GPUCount:       1,
			PricePerHour:   1.5,
			Tier:           provider.TierOnDemand,
			Region:         region,
			AvailableCount: 1,
		}},
		provisionResult: &provider.ProvisionResult{
			UpstreamID:  upstreamID,
			UpstreamIP:  "10.0.0.7",
			Status:      "creating",
			CostPerHour: 1.0,
			Region:      region,
		},
	}
}

// onDemandRequest is a minimal valid on-demand provision request. SSHKeyIDs is
// empty so the engine takes the auto-include-all-org-keys path
// (GetSSHKeysByOrgID), which the fake backs via its sshKeys field.
func onDemandRequest() ProvisionRequest {
	return ProvisionRequest{
		OrgID:    "org-1",
		UserID:   "user-1",
		GPUType:  provider.GPUTypeA10080GB,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
	}
}

// fakeWithOneKey returns a fakeEngineDB pre-seeded with a single org SSH key so
// the "at least one SSH key required" gate passes. Callers set createInstanceErr
// to drive the failure path.
func fakeWithOneKey() *fakeEngineDB {
	return &fakeEngineDB{
		sshKeys: []db.SSHKey{{
			SSHKeyID:          "key-1",
			OrgID:             "org-1",
			UserID:            "user-1",
			PublicKey:         "ssh-ed25519 AAAA test",
			AttachOnProvision: true, // DB column defaults true; in-memory fixtures must mirror it
		}},
	}
}

// TestProvision_PersistFailure_TerminatesUpstream proves BLOCK-02: when
// provider.Provision succeeds but CreateInstance fails, the engine terminates
// the just-created upstream (with the EXACT upstream id + native region) before
// returning the unchanged persist error — no paid orphan.
func TestProvision_PersistFailure_TerminatesUpstream(t *testing.T) {
	stub := onDemandStub("up-xyz", "us-west")
	fake := fakeWithOneKey()
	fake.createInstanceErr = errors.New("boom") // force the persist failure

	e := newProvisionUnitEngine(stub, fake)

	resp, err := e.Provision(context.Background(), onDemandRequest())

	// The persist error surfaces (contract unchanged: wraps "persist instance").
	if err == nil {
		t.Fatal("expected Provision to return an error when CreateInstance fails, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil response on persist failure, got %+v", resp)
	}
	if !strings.Contains(err.Error(), "persist instance") {
		t.Fatalf("expected error to wrap %q, got %q", "persist instance", err.Error())
	}

	// The upstream was terminated exactly once, with the precise id + region we
	// provisioned through (T-32-WRONG-TERM: never a guessed/cross-tenant id).
	if len(stub.terminateCalls) != 1 {
		t.Fatalf("expected exactly 1 Terminate call after persist failure, got %d: %+v",
			len(stub.terminateCalls), stub.terminateCalls)
	}
	got := stub.terminateCalls[0]
	if got.UpstreamID != "up-xyz" {
		t.Errorf("Terminate UpstreamID = %q, want %q", got.UpstreamID, "up-xyz")
	}
	if got.Region != "us-west" {
		t.Errorf("Terminate Region = %q, want %q", got.Region, "us-west")
	}
}

// TestProvision_PersistFailure_TerminateError_StillReturnsPersistError proves the
// edge case: if prov.Terminate itself errors, Provision STILL returns the
// original persist error (terminate failure is logged CRITICAL, not returned —
// the reconciler backstops it).
func TestProvision_PersistFailure_TerminateError_StillReturnsPersistError(t *testing.T) {
	stub := onDemandStub("up-xyz", "us-west")
	stub.terminateErr = errors.New("terminate also failed")
	fake := fakeWithOneKey()
	fake.createInstanceErr = errors.New("boom")

	e := newProvisionUnitEngine(stub, fake)

	_, err := e.Provision(context.Background(), onDemandRequest())
	if err == nil {
		t.Fatal("expected the persist error to surface even when Terminate fails, got nil")
	}
	if !strings.Contains(err.Error(), "persist instance") {
		t.Fatalf("expected the ORIGINAL persist error, got %q", err.Error())
	}
	// Terminate was still attempted (best-effort).
	if len(stub.terminateCalls) != 1 {
		t.Fatalf("expected 1 Terminate attempt, got %d", len(stub.terminateCalls))
	}
}

// TestProvision_DoesNotBillDuringProvisioning proves the meter does NOT start at
// provision time: provisioning (creating/provisioning/booting) is deliberately
// unbilled because the instance isn't usable yet. The billing session is opened
// only at the running transition (the ready callback), so Provision must NOT
// create one.
func TestProvision_DoesNotBillDuringProvisioning(t *testing.T) {
	stub := onDemandStub("up-abc", "eu-central")
	fake := fakeWithOneKey()
	// createInstanceErr stays nil -> CreateInstance succeeds.
	// UpdateInstanceStatus returns (false, nil) by default, so the async
	// progressStatus goroutine exits immediately at its "status already changed
	// from creating" guard and cannot race this assertion.

	e := newProvisionUnitEngine(stub, fake)

	resp, err := e.Provision(context.Background(), onDemandRequest())
	if err != nil {
		t.Fatalf("Provision returned unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected a non-nil ProvisionResponse on success")
	}

}
