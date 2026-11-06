package provision

import (
	"context"
	"log/slog"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// TestSelectProviderCandidates_PinnedProvider — the staff dev-tools pin
// (ProvisionRequest.PinnedProvider) narrows the candidate set to the named
// provider even when a cheaper provider exists; no capacity on the pinned
// provider is an explicit failure, never a fallback.
func TestSelectProviderCandidates_PinnedProvider(t *testing.T) {
	cheap := &stubProvider{
		name: "cheap",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "us-east", "US-TX-3", 1.00, 4),
		},
	}
	pinned := &stubProvider{
		name: "pinnedprov",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "us-east", "US-TX-3", 5.00, 2),
		},
	}
	reg := provider.NewRegistry()
	reg.Register(cheap)
	reg.Register(pinned)
	e := &Engine{registry: reg, logger: slog.Default(), config: &config.Config{}}

	// Pinned: only the named provider survives, despite being 5× the price.
	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:        provider.GPUTypeH100SXM,
		GPUCount:       1,
		Tier:           provider.TierOnDemand,
		PinnedProvider: "pinnedprov",
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected exactly 1 candidate (the pinned provider), got %d", len(candidates))
	}
	if got := candidates[0].Prov.Name(); got != "pinnedprov" {
		t.Errorf("candidates[0] = %q, want pinnedprov", got)
	}

	// Pin to a provider with no matching capacity: explicit failure, never a
	// silent fallback to the cheap provider.
	_, err = e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:        provider.GPUTypeB200, // neither stub offers B200
		GPUCount:       1,
		Tier:           provider.TierOnDemand,
		PinnedProvider: "pinnedprov",
	})
	if err == nil {
		t.Error("expected an error for a pin with no matching capacity, got candidates")
	}

	// Unset pin: byte-for-byte legacy behavior — cheapest first, both present.
	candidates, err = e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates (no pin): %v", err)
	}
	if len(candidates) != 2 || candidates[0].Prov.Name() != "cheap" {
		t.Errorf("no-pin path changed: got %d candidates, first %q", len(candidates), candidates[0].Prov.Name())
	}
}

// TestSelectProviderCandidates_PinCannotOverrideKillSwitch — PROVIDERS_DISABLED
// outranks the staff pin: pinning a disabled provider yields no candidates.
func TestSelectProviderCandidates_PinCannotOverrideKillSwitch(t *testing.T) {
	pinned := &stubProvider{
		name: "offprov",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "us-east", "US-TX-3", 2.00, 2),
		},
	}
	reg := provider.NewRegistry()
	reg.Register(pinned)
	e := &Engine{
		registry: reg,
		logger:   slog.Default(),
		config:   &config.Config{DisabledProviders: map[string]bool{"offprov": true}},
	}

	_, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:        provider.GPUTypeH100SXM,
		GPUCount:       1,
		Tier:           provider.TierOnDemand,
		PinnedProvider: "offprov",
	})
	if err == nil {
		t.Error("expected an error pinning a PROVIDERS_DISABLED provider, got candidates")
	}
}
