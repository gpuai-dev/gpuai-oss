package availability

import (
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// TestToAvailableOffering_InstantBoot verifies the boot class is computed
// from the provider at cache-write time — the ONLY moment provider identity
// is still attached to the offering. Downstream (dashboard handler, public
// /v1/pricing, CLI) must never need the provider name to know boot speed.
func TestToAvailableOffering_InstantBoot(t *testing.T) {
	slow := ToAvailableOffering(provider.GPUOffering{
		Provider: fixtureSlow,
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Region:   "ca-central",
		Tier:     provider.TierOnDemand,
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
	if slow.InstantBoot {
		t.Error("slow-boot offering: InstantBoot = true, want false (slow-boot provider)")
	}

	fast := ToAvailableOffering(provider.GPUOffering{
		Provider: "alpha",
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Region:   "us-east",
		Tier:     provider.TierOnDemand,
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
	if !fast.InstantBoot {
		t.Error("container-native offering: InstantBoot = false, want true")
	}
}
