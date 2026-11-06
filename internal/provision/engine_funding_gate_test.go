package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// --- account-unfunded circuit (2026-09-21→23 empty-supplier incident) ---

// unfundedErr is the shape an adapter returns when OUR account at the supplier
// has no money: a wrapped provider.ErrAccountUnfunded.
func unfundedErr() error {
	return fmt.Errorf("create instance: HTTP 400: {\"error\":\"insufficient_credit\"}: %w", provider.ErrAccountUnfunded)
}

// TestWalk_UnfundedSkipsRestOfProviderAndTripsGate — the incident shape. The
// three cheapest candidates are all on the empty supplier; without the circuit
// the bounded 3-attempt walk spends every attempt on doomed creates and the
// launch fails even though a funded supplier had stock. With it, the first
// refusal drops the empty supplier's remaining candidates from the walk, the
// launch lands on the funded one, and the supplier is suspended sitewide.
func TestWalk_UnfundedSkipsRestOfProviderAndTripsGate(t *testing.T) {
	broke := &stubProvider{name: "broke", provisionErr: unfundedErr()}
	funded := &stubProvider{
		name:            "funded",
		provisionResult: &provider.ProvisionResult{UpstreamID: "up-ok", Provider: "funded"},
	}
	gate := availability.NewFundingGate(10*time.Minute, slog.Default())
	e := &Engine{config: passthroughCfg(), logger: slog.Default(), fundingGate: gate}

	candidates := []ProviderCandidate{
		{Prov: broke, Offering: a100Offering("broke", 1.0)},
		{Prov: broke, Offering: a100Offering("broke", 1.1)},
		{Prov: broke, Offering: a100Offering("broke", 1.2)},
		{Prov: funded, Offering: a100Offering("funded", 2.0)},
	}
	orig := append([]ProviderCandidate(nil), candidates...)
	provReq := &provider.ProvisionRequest{InstanceID: "gpu-unf", GPUType: provider.GPUTypeA10080GB, GPUCount: 1, Tier: provider.TierOnDemand}

	prov, _, res, err := e.walkProvisionCandidates(context.Background(), provReq, candidates,
		provisionWalkParams{instanceID: "gpu-unf", effectiveDisk: 100})
	if err != nil {
		t.Fatalf("walk must reach the funded supplier, got %v", err)
	}
	if prov.Name() != "funded" || res.UpstreamID != "up-ok" {
		t.Fatalf("placed on %s/%v, want funded/up-ok", prov.Name(), res)
	}
	if got := len(broke.provisionReqs); got != 1 {
		t.Errorf("empty supplier got %d creates, want exactly 1 (the rest must be skipped)", got)
	}
	if !gate.Suspended("broke") {
		t.Error("an unfunded refusal must suspend the supplier")
	}
	if gate.Suspended("funded") {
		t.Error("the funded supplier must not be suspended")
	}
	for i := range orig {
		if candidates[i].Prov != orig[i].Prov || candidates[i].Offering.PricePerHour != orig[i].Offering.PricePerHour {
			t.Fatal("the caller's candidate slice must not be mutated")
		}
	}
}

// TestWalk_UnfundedSurfacesSentinel — when the empty supplier was the ONLY
// option the walk still fails, and the error carries ErrAccountUnfunded so the
// API layer can answer "machine-unlaunchable" rather than a retryable 409.
func TestWalk_UnfundedSurfacesSentinel(t *testing.T) {
	broke := &stubProvider{name: "broke", provisionErr: unfundedErr()}
	e := &Engine{config: passthroughCfg(), logger: slog.Default(),
		fundingGate: availability.NewFundingGate(0, slog.Default())}
	provReq := &provider.ProvisionRequest{InstanceID: "gpu-unf2", GPUType: provider.GPUTypeA10080GB, GPUCount: 1, Tier: provider.TierOnDemand}

	_, _, _, err := e.walkProvisionCandidates(context.Background(), provReq,
		[]ProviderCandidate{{Prov: broke, Offering: a100Offering("broke", 1.0)}, {Prov: broke, Offering: a100Offering("broke", 1.1)}},
		provisionWalkParams{instanceID: "gpu-unf2", effectiveDisk: 100})
	if !errors.Is(err, provider.ErrAccountUnfunded) {
		t.Fatalf("want ErrAccountUnfunded, got %v", err)
	}
	if len(broke.provisionReqs) != 1 {
		t.Errorf("got %d creates, want 1", len(broke.provisionReqs))
	}
}

// TestWalk_NilGateStillSkipsRestOfProvider — the walk-local skip does not
// depend on the gate being wired (every directly-constructed test Engine).
func TestWalk_NilGateStillSkipsRestOfProvider(t *testing.T) {
	broke := &stubProvider{name: "broke", provisionErr: unfundedErr()}
	funded := &stubProvider{name: "funded", provisionResult: &provider.ProvisionResult{UpstreamID: "up-ok", Provider: "funded"}}
	e := &Engine{config: passthroughCfg(), logger: slog.Default()}
	provReq := &provider.ProvisionRequest{InstanceID: "gpu-unf3", GPUType: provider.GPUTypeA10080GB, GPUCount: 1, Tier: provider.TierOnDemand}

	prov, _, _, err := e.walkProvisionCandidates(context.Background(), provReq, []ProviderCandidate{
		{Prov: broke, Offering: a100Offering("broke", 1.0)},
		{Prov: broke, Offering: a100Offering("broke", 1.1)},
		{Prov: broke, Offering: a100Offering("broke", 1.2)},
		{Prov: funded, Offering: a100Offering("funded", 2.0)},
	}, provisionWalkParams{instanceID: "gpu-unf3", effectiveDisk: 100})
	if err != nil || prov.Name() != "funded" {
		t.Fatalf("want placement on funded, got %v / %v", prov, err)
	}
}

// TestWalk_CapacityErrorDoesNotTripGate — an ordinary stock race is not an
// account problem and must neither suspend the supplier nor skip its other
// candidates.
func TestWalk_CapacityErrorDoesNotTripGate(t *testing.T) {
	racy := &stubProvider{name: "racy", provisionErr: provider.ErrNoCapacity}
	gate := availability.NewFundingGate(0, slog.Default())
	e := &Engine{config: passthroughCfg(), logger: slog.Default(), fundingGate: gate}
	provReq := &provider.ProvisionRequest{InstanceID: "gpu-cap", GPUType: provider.GPUTypeA10080GB, GPUCount: 1, Tier: provider.TierOnDemand}

	_, _, _, _ = e.walkProvisionCandidates(context.Background(), provReq, []ProviderCandidate{
		{Prov: racy, Offering: a100Offering("racy", 1.0)},
		{Prov: racy, Offering: a100Offering("racy", 1.1)},
	}, provisionWalkParams{instanceID: "gpu-cap", effectiveDisk: 100})
	if len(racy.provisionReqs) != 2 {
		t.Errorf("capacity failures must keep walking the supplier's candidates, got %d creates", len(racy.provisionReqs))
	}
	if gate.Suspended("racy") {
		t.Error("a capacity error must not suspend the supplier")
	}
}

// TestSelectProviderCandidates_FundingGateSkipsSuspendedProvider — a
// suspended supplier is not a candidate until the cooldown lapses, after which
// it re-enters (the next launch is the re-probe).
func TestSelectProviderCandidates_FundingGateSkipsSuspendedProvider(t *testing.T) {
	dying, backup, rescue, _ := failoverFixture()
	e := failoverEngine(&fakeEngineDB{}, dying, backup)
	gate := availability.NewFundingGate(10*time.Minute, e.logger)
	now := time.Now()
	clock := now
	gate.SetClock(func() time.Time { return clock })
	e.fundingGate = gate

	req := rescue.req
	req.SelectedOfferingID = ""

	gate.Trip("dying", "create_rejected", "test", false)
	candidates, err := e.SelectProviderCandidates(context.Background(), req)
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("the funded provider must survive")
	}
	for _, c := range candidates {
		if c.Prov.Name() == "dying" {
			t.Error("a suspended supplier must not be a placement candidate")
		}
	}

	clock = now.Add(11 * time.Minute)
	candidates, err = e.SelectProviderCandidates(context.Background(), req)
	if err != nil {
		t.Fatalf("SelectProviderCandidates after cooldown: %v", err)
	}
	found := false
	for _, c := range candidates {
		if c.Prov.Name() == "dying" {
			found = true
		}
	}
	if !found {
		t.Error("after the cooldown the supplier must be re-probed")
	}
}

// TestSelectProviderCandidates_FundingGatePinnedProviderBypasses — a staff
// provider pin is how an operator confirms a top-up landed without waiting
// out the cooldown.
func TestSelectProviderCandidates_FundingGatePinnedProviderBypasses(t *testing.T) {
	dying, backup, rescue, _ := failoverFixture()
	e := failoverEngine(&fakeEngineDB{}, dying, backup)
	gate := availability.NewFundingGate(10*time.Minute, e.logger)
	e.fundingGate = gate
	gate.Trip("dying", "create_rejected", "test", false)

	req := rescue.req
	req.SelectedOfferingID = ""
	req.PinnedProvider = "dying"
	candidates, err := e.SelectProviderCandidates(context.Background(), req)
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("staff pin must bypass the funding gate")
	}
}
