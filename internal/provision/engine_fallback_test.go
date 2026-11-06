package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// This file proves two GENERIC engine contracts around candidate eligibility
// and the provision fall-through loop. Both are provider-agnostic — they use
// FAKE stubs, not any real adapter:
//
//  1. The availability-only capability gate (CR-01 fix): ANY provider that
//     implements provider.ProvisionCapability with Provisionable()==false never
//     enters SelectProviderCandidates' candidate set at all, so N cheap
//     same-SKU offerings from such a provider can never burn the engine's
//     bounded provision attempts or starve same-SKU datacenter stock
//     (TestProvision_AvailabilityOnlyProviderNeverCandidate /
//     TestSelectProviderCandidates_ExcludesAvailabilityOnly). NOTE: as of Phase
//     59 (Plan 59-08) the community adapter reports Provisionable()==true and is
//     a LIVE candidate — its end-to-end path is proven by
//     internal/provision.TestProvision_Community. These two tests therefore pin
//     the engine's GENERIC gate with a synthetic availabilityOnlyStub, and no
//     longer model the community adapter.
//
//  2. The BACKSTOP is the existing fallback loop: a candidate whose Provision
//     returns a wrapped provider.ErrNotImplemented burns one of the (max 3)
//     attempts and the engine falls through to the next candidate. This
//     remains the behavior for any provider that fails to declare the
//     capability (the two fall-through tests below, kept as the generic
//     fall-through contract).
//
// All tests assert errors.Is on the sentinel, never string matching.

// newFallbackEngine wires a minimal Engine with the given ordered providers
// registered into a fresh registry (insertion order = the price-tie tiebreak)
// and the supplied fakeEngineDB. Mirrors newProvisionUnitEngine
// (engine_provision_test.go) but takes N providers so a cheapest-candidate
// fall-through can be exercised end-to-end through e.Provision.
func newFallbackEngine(fake *fakeEngineDB, provs ...provider.Provider) *Engine {
	reg := provider.NewRegistry()
	for _, p := range provs {
		reg.Register(p)
	}
	return &Engine{
		registry:  reg,
		db:        fake,
		config:    &config.Config{},
		logger:    slog.Default(),
		tunnelMgr: nil,
	}
}

// fallbackStub returns a stubProvider offering a single A100-80GB on-demand node
// at the given whole-node price. When notImplemented is true its Provision
// returns a wrapped provider.ErrNotImplemented (the communitycloud stub shape);
// otherwise it succeeds with the given upstream id.
func fallbackStub(name string, price float64, notImplemented bool, upstreamID string) *stubProvider {
	s := &stubProvider{
		name: name,
		offerings: []provider.GPUOffering{{
			Provider:       name,
			GPUType:        provider.GPUTypeA10080GB,
			GPUCount:       1,
			PricePerHour:   price,
			Tier:           provider.TierOnDemand,
			Region:         "us-west",
			AvailableCount: 1,
		}},
	}
	if notImplemented {
		// Mirror communitycloud.Adapter.Provision: a human-readable message
		// wrapping the shared sentinel so errors.Is still resolves.
		s.provisionErr = fmt.Errorf("community-tier provisioning is not yet available: %w", provider.ErrNotImplemented)
		return s
	}
	s.provisionResult = &provider.ProvisionResult{
		UpstreamID:  upstreamID,
		UpstreamIP:  "10.0.0.9",
		Status:      "creating",
		CostPerHour: price,
		Region:      "us-west",
	}
	return s
}

// availabilityOnlyStub decorates a stubProvider with the availability-only
// capability (Provisionable() == false). It is a SYNTHETIC test double for the
// engine's generic eligibility gate — NOT a model of any real adapter. (The
// community adapter is a live, provisionable candidate as of Phase 59, Plan
// 59-08; see internal/communitycloud/adapter.go and
// internal/provision.TestProvision_Community.)
type availabilityOnlyStub struct{ *stubProvider }

func (availabilityOnlyStub) Provisionable() bool { return false }

// TestProvision_AvailabilityOnlyProviderNeverCandidate is the CR-01 regression
// test for the GENERIC eligibility gate: a synthetic availability-only provider
// emitting THREE cheap same-SKU offerings (one candidate each pre-fix, enough to
// exhaust all maxProvisionAttempts=3 on ErrNotImplemented stubs) plus one pricier
// datacenter provider. The capability gate must keep every availability-only
// offering out of the candidate set, so the datacenter provision succeeds on the
// FIRST attempt and the availability-only provider's Provision is never invoked.
func TestProvision_AvailabilityOnlyProviderNeverCandidate(t *testing.T) {
	mkOffering := func(price float64) provider.GPUOffering {
		return provider.GPUOffering{
			Provider:       "availonly",
			GPUType:        provider.GPUTypeA10080GB,
			GPUCount:       1,
			PricePerHour:   price,
			Tier:           provider.TierOnDemand,
			Region:         "us-west",
			AvailableCount: 1,
		}
	}
	availOnly := availabilityOnlyStub{&stubProvider{
		name: "availonly",
		// Three offerings, all cheaper than the datacenter row.
		offerings:    []provider.GPUOffering{mkOffering(0.9), mkOffering(1.0), mkOffering(1.1)},
		provisionErr: fmt.Errorf("provisioning is not available for this provider: %w", provider.ErrNotImplemented),
	}}
	datacenter := fallbackStub("datacenter", 2.0, false, "up-dc")
	fake := fakeWithOneKey()

	// The availability-only provider is registered FIRST so, absent the gate, its
	// three cheaper candidates would occupy candidates[0..2] and exhaust every
	// attempt.
	e := newFallbackEngine(fake, availOnly, datacenter)

	resp, err := e.Provision(context.Background(), onDemandRequest())
	if err != nil {
		t.Fatalf("expected the datacenter provision to succeed with the availability-only provider gated out, got error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected a non-nil ProvisionResponse from the datacenter candidate")
	}
	if len(availOnly.provisionReqs) != 0 {
		t.Fatalf("availability-only provider was attempted %d times; want 0 (never a candidate)", len(availOnly.provisionReqs))
	}
	if len(datacenter.provisionReqs) != 1 {
		t.Fatalf("datacenter candidate attempted %d times; want exactly 1 (first and only attempt)", len(datacenter.provisionReqs))
	}
}

// TestSelectProviderCandidates_ExcludesAvailabilityOnly pins the GENERIC gate at
// the candidate-selection seam itself: with a synthetic availability-only
// provider and a datacenter provider both offering the SKU, the candidate set
// contains ONLY datacenter rows — eligibility filtering, with the surviving
// candidates' price ordering untouched.
func TestSelectProviderCandidates_ExcludesAvailabilityOnly(t *testing.T) {
	availOnly := availabilityOnlyStub{&stubProvider{
		name: "availonly",
		offerings: []provider.GPUOffering{{
			Provider:       "availonly",
			GPUType:        provider.GPUTypeA10080GB,
			GPUCount:       1,
			PricePerHour:   1.0,
			Tier:           provider.TierOnDemand,
			Region:         "us-west",
			AvailableCount: 1,
		}},
	}}
	datacenter := fallbackStub("datacenter", 2.0, false, "up-dc")

	e := newFallbackEngine(fakeWithOneKey(), availOnly, datacenter)
	candidates, err := e.SelectProviderCandidates(context.Background(), onDemandRequest())
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d; want 1 (datacenter only)", len(candidates))
	}
	if candidates[0].Prov.Name() != "datacenter" {
		t.Fatalf("candidates[0] = %q; want datacenter", candidates[0].Prov.Name())
	}
}

// TestProvision_ErrNotImplementedCandidate_FallsThrough proves that the cheapest
// candidate returning ErrNotImplemented burns one attempt and the engine
// provisions on the next candidate — the pre-existing fallback loop, unchanged.
// (The stub here deliberately does NOT declare the availability-only
// capability: this pins the generic backstop for any provider that returns
// ErrNotImplemented without declaring itself non-provisionable. The real
// community adapter is excluded earlier by the capability gate — see
// TestProvision_AvailabilityOnlyProviderNeverCandidate.)
func TestProvision_ErrNotImplementedCandidate_FallsThrough(t *testing.T) {
	// Cheapest cannot provision (community stub shape); next succeeds.
	cheap := fallbackStub("community", 1.0, true, "")
	next := fallbackStub("datacenter", 2.0, false, "up-ok")
	fake := fakeWithOneKey()

	// Register in a deliberately non-sorted order to prove price-sort (not
	// insertion order) puts the ErrNotImplemented candidate first.
	e := newFallbackEngine(fake, next, cheap)

	resp, err := e.Provision(context.Background(), onDemandRequest())
	if err != nil {
		t.Fatalf("expected fall-through to succeed on the next candidate, got error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected a non-nil ProvisionResponse from the second candidate")
	}

	// The cheapest (community) candidate was attempted exactly once and burned.
	if len(cheap.provisionReqs) != 1 {
		t.Fatalf("expected the ErrNotImplemented candidate to be attempted exactly once, got %d", len(cheap.provisionReqs))
	}
	// The next candidate provisioned exactly once (the successful attempt).
	if len(next.provisionReqs) != 1 {
		t.Fatalf("expected the succeeding candidate to be attempted exactly once, got %d", len(next.provisionReqs))
	}
}

// TestProvision_AllErrNotImplemented_FailsCleanly proves the degenerate case:
// when EVERY candidate returns ErrNotImplemented, the engine exhausts its
// bounded attempts and returns the "all providers failed" error wrapping the
// sentinel — no panic, no hang (T-58-06 bounded to at most 3 attempts).
func TestProvision_AllErrNotImplemented_FailsCleanly(t *testing.T) {
	c1 := fallbackStub("community", 1.0, true, "")
	c2 := fallbackStub("community2", 2.0, true, "")
	fake := fakeWithOneKey()

	e := newFallbackEngine(fake, c1, c2)

	resp, err := e.Provision(context.Background(), onDemandRequest())
	if err == nil {
		t.Fatal("expected an error when all candidates return ErrNotImplemented")
	}
	if resp != nil {
		t.Fatalf("expected nil response when all candidates fail, got %+v", resp)
	}
	// The sentinel propagates via %w wrapping — assert on errors.Is, never the
	// human-readable message text.
	if !errors.Is(err, provider.ErrNotImplemented) {
		t.Fatalf("expected the returned error to wrap provider.ErrNotImplemented, got %v", err)
	}
	// No production instance row was persisted (all attempts burned before persist).
	if len(fake.createdInstances) != 0 {
		t.Fatalf("expected zero CreateInstance calls when every candidate fails, got %d", len(fake.createdInstances))
	}
}
