package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// variantStub returns a stubProvider with one on-demand A100 offering that
// carries the given provider-native GPU variant id (UpstreamGPUID) at the
// given price, whose Provision succeeds unless the caller sets provisionErr.
func variantStub(name, upstreamGPUID string, price float64) *stubProvider {
	o := makeOffering(provider.GPUTypeA10080GB, "us-west", "US-WA-1", price, 3)
	o.Provider = name
	o.UpstreamGPUID = upstreamGPUID
	return &stubProvider{
		name:      name,
		offerings: []provider.GPUOffering{o},
		provisionResult: &provider.ProvisionResult{
			UpstreamID:  "up-" + name,
			UpstreamIP:  "10.0.0.9",
			Status:      "creating",
			CostPerHour: price,
			Region:      "us-west",
		},
	}
}

// TestProvision_UpstreamGPUIDForwardedPerAttempt proves the engine forwards
// each matched offering's provider-native GPU variant (UpstreamGPUID) into the
// per-attempt ProvisionRequest — including across failover, where the fallback
// candidate can be a different variant. This is the engine half of the
// 2026-08-03 A100/us-west fix: without the forward, the adapter reverse-maps
// the canonical type to a preferred variant the advertised DC may not stock and
// every launch fails "no capacity" while the poller keeps advertising the row.
func TestProvision_UpstreamGPUIDForwardedPerAttempt(t *testing.T) {
	cheap := variantStub("cheap", "NVIDIA A100-SXM4-80GB", 1.00)
	cheap.provisionErr = errors.New("no GPU capacity available")
	backup := variantStub("backup", "NVIDIA A100 80GB PCIe", 2.00)

	fake := fakeWithOneKey()
	e := newSpotUnitEngine(fake, backup, cheap) // registry order != price order on purpose

	req := onDemandRequest()
	if _, err := e.Provision(context.Background(), req); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if len(cheap.provisionReqs) != 1 {
		t.Fatalf("cheapest candidate must be attempted first, got %d attempts", len(cheap.provisionReqs))
	}
	if got := cheap.provisionReqs[0].UpstreamGPUID; got != "NVIDIA A100-SXM4-80GB" {
		t.Errorf("attempt 1 UpstreamGPUID = %q, want the matched offering's variant %q", got, "NVIDIA A100-SXM4-80GB")
	}
	if len(backup.provisionReqs) != 1 {
		t.Fatalf("failover must land on the next candidate, got %d attempts", len(backup.provisionReqs))
	}
	if got := backup.provisionReqs[0].UpstreamGPUID; got != "NVIDIA A100 80GB PCIe" {
		t.Errorf("attempt 2 UpstreamGPUID = %q, want the fallback offering's variant %q (set per attempt, not carried over)", got, "NVIDIA A100 80GB PCIe")
	}
}

// mcVariant builds one startupvmco A6000 4-GPU offering for a given native
// product name. Every field except the product identity and its host specs is
// shared, and the price is deliberately IDENTICAL across variants — that price
// tie is what made the two products indistinguishable to a class-only pin.
func mcVariant(productName string, vcpu, ramGB int) provider.GPUOffering {
	return provider.GPUOffering{
		Provider:           "startupvmco",
		GPUType:            provider.GPUTypeRTXA6000,
		GPUCount:           4,
		VRAMPerGPUGB:       48,
		CPUCores:           vcpu,
		RAMGB:              ramGB,
		PricePerHour:       1.88, // price-TIED siblings
		Tier:               provider.TierOnDemand,
		Region:             "us-central-3",
		DatacenterLocation: "us-central-3",
		StockStatus:        "in_stock",
		AvailableCount:     2,
		CapacityClass:      provider.CapacityClassSecure,
		UpstreamGPUID:      productName,
	}
}

// TestPlacement_PinnedProductIdentity_NoSiblingSubstitution — GPU-55, the
// residual half of the 2026-08-14 startupvmco substitution (PR #634 fixed the
// nondeterministic tie-break; this is the identity).
//
// The customer's card advertised gpu_4x_A6000_high_perf (54 vCPU / 256 GB). By
// launch time that product had drained and the adapter's dedup winner for the
// same (type, count, region) was gpu_4x_a6000 (30 / 192) at the SAME price.
// While the pin id hashed only class attributes the two products shared ONE id,
// so the pin matched the sibling and the engine forwarded the SIBLING's
// UpstreamGPUID — a same-price, lower-spec node, silently. Folding
// UpstreamGPUID into the seed makes the pin name the product, so the drained
// case fails EXPLICITLY (ErrOfferingUnavailable, which the API surfaces as a
// 409 carrying live alternatives) instead of resolving wrong.
func TestPlacement_PinnedProductIdentity_NoSiblingSubstitution(t *testing.T) {
	advertised := mcVariant("gpu_4x_A6000_high_perf", 54, 256)
	sibling := mcVariant("gpu_4x_a6000", 30, 192)

	pin := pinFor(advertised, "startupvmco")
	if pin == pinFor(sibling, "startupvmco") {
		t.Fatal("price-tied sibling products share a pin id — the class-only seed is back and substitution is possible again")
	}

	req := ProvisionRequest{
		GPUType:            provider.GPUTypeRTXA6000,
		GPUCount:           4,
		Tier:               provider.TierOnDemand,
		SelectedOfferingID: pin,
	}

	// The drained case: only the same-price sibling is live at launch.
	drained := newTestEngineWithProviders(&stubProvider{
		name:      "startupvmco",
		offerings: []provider.GPUOffering{sibling},
	})
	candidates, err := drained.SelectProviderCandidates(context.Background(), req)
	if !errors.Is(err, ErrOfferingUnavailable) {
		t.Fatalf("drained pinned product: err = %v, want ErrOfferingUnavailable — a same-price sibling must never be substituted", err)
	}
	if len(candidates) != 0 {
		t.Errorf("got %d candidates for a drained pinned product, want 0 (any candidate here is a silent substitution)", len(candidates))
	}
	// T-63-04: the failure must not leak provider identity.
	for _, name := range []string{"startupvmco", "podco", "vultr", "templateonlyco"} {
		if strings.Contains(strings.ToLower(err.Error()), name) {
			t.Errorf("placement error leaks provider name %q: %v", name, err)
		}
	}

	// The live case: with BOTH products in the live set the pin still resolves,
	// and it resolves to the advertised product — not to whichever sibling the
	// adapter happened to rank first.
	both := newTestEngineWithProviders(&stubProvider{
		name:      "startupvmco",
		offerings: []provider.GPUOffering{sibling, advertised},
	})
	live, err := both.SelectProviderCandidates(context.Background(), req)
	if err != nil {
		t.Fatalf("pinned product still live: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("a pin must narrow to exactly one candidate, got %d", len(live))
	}
	if got := live[0].Offering.UpstreamGPUID; got != "gpu_4x_A6000_high_perf" {
		t.Errorf("pin resolved to product %q, want the advertised gpu_4x_A6000_high_perf", got)
	}
	if live[0].Offering.CPUCores != 54 || live[0].Offering.RAMGB != 256 {
		t.Errorf("pin resolved to %d vCPU / %d GB, want the advertised 54 / 256", live[0].Offering.CPUCores, live[0].Offering.RAMGB)
	}

	// Legacy safety: an unpinned request is untouched by the seed change — both
	// price-tied siblings remain placeable candidates.
	unpinned := req
	unpinned.SelectedOfferingID = ""
	legacy, err := both.SelectProviderCandidates(context.Background(), unpinned)
	if err != nil {
		t.Fatalf("unpinned placement errored: %v", err)
	}
	if len(legacy) != 2 {
		t.Errorf("unpinned placement returned %d candidates, want both siblings (2) — legacy placement must be unchanged", len(legacy))
	}
}
