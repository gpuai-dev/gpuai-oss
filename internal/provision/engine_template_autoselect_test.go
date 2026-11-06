package provision

import (
	"context"
	"errors"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// vramOffering builds a GPUOffering carrying the fields the template
// auto-select path filters on (TMPL-04): GPU type, per-GPU VRAM, price, and
// availability. One GPU per offering (templates are single-GPU in v1).
func vramOffering(gpuType string, vramGB int, price float64, avail int) provider.GPUOffering {
	return provider.GPUOffering{
		GPUType:        provider.GPUType(gpuType),
		GPUCount:       1,
		VRAMPerGPUGB:   vramGB,
		PricePerHour:   price,
		Tier:           provider.TierOnDemand,
		AvailableCount: avail,
	}
}

// A template deploy that omits gpu_type must auto-select the cheapest offering
// that meets the template's VRAM floor — and drop offerings below it.
func TestSelectProviderCandidates_TemplateAutoSelectsCheapestEligible(t *testing.T) {
	tid := "comfyui"
	podco := &stubProvider{
		name: "podco", // SupportsCustomImage == true
		offerings: []provider.GPUOffering{
			vramOffering("rtx4090", 24, 0.74, 3),
			vramOffering("t4", 16, 0.30, 2), // cheapest that still meets a 16GB floor
			vramOffering("a10", 8, 0.10, 5), // below 16GB floor -> dropped
		},
	}
	e := newTestEngineWithProviders(podco)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		TemplateID: &tid,
		GPUType:    "", // auto-select
		GPUCount:   1,
		Tier:       provider.TierOnDemand,
		Image:      "ghcr.io/gpuai/comfyui:latest", // non-empty => requireCustomImage
		MinVRAMGB:  16,
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 eligible candidates (t4 + rtx4090), got %d", len(candidates))
	}
	if got := string(candidates[0].Offering.GPUType); got != "t4" {
		t.Errorf("auto-select cheapest eligible = %q; want t4 (0.30)", got)
	}
	for _, c := range candidates {
		if c.Offering.VRAMPerGPUGB < 16 {
			t.Errorf("offering %q (%dGB) below the VRAM floor leaked through", c.Offering.GPUType, c.Offering.VRAMPerGPUGB)
		}
	}
}

// Auto-select must compose with the custom-image gate: a cheaper offering on a
// provider that does NOT honor custom images (vmimageco) must be skipped, so
// a template never lands on a provider that would boot the wrong image.
func TestSelectProviderCandidates_TemplateAutoSelectRespectsCustomImageGate(t *testing.T) {
	tid := "comfyui"
	vmImage := &stubProvider{
		name:      "vmimageco",                                             // SupportsCustomImage == false
		offerings: []provider.GPUOffering{vramOffering("t4", 16, 0.10, 5)}, // cheapest overall
	}
	podco := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{vramOffering("rtx4090", 24, 0.74, 3)},
	}
	e := newTestEngineWithProviders(vmImage, podco)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		TemplateID: &tid,
		GPUType:    "",
		GPUCount:   1,
		Tier:       provider.TierOnDemand,
		Image:      "ghcr.io/gpuai/comfyui:latest",
		MinVRAMGB:  16,
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	for _, c := range candidates {
		if c.Prov.Name() == "vmimageco" {
			t.Fatalf("vmimageco (no custom-image support) leaked into auto-select candidates")
		}
	}
	if got := string(candidates[0].Offering.GPUType); got != "rtx4090" {
		t.Errorf("expected podco rtx4090, got %q", got)
	}
}

// When no offering meets the template's VRAM floor, auto-select returns
// ErrNoProvider (mapped to a clear 4xx upstream) rather than silently
// provisioning an under-spec GPU.
func TestSelectProviderCandidates_TemplateAutoSelectNoEligibleGPU(t *testing.T) {
	tid := "vllm"
	podco := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{vramOffering("a10", 8, 0.10, 5)}, // below the 24GB floor
	}
	e := newTestEngineWithProviders(podco)

	_, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		TemplateID: &tid,
		GPUType:    "",
		GPUCount:   1,
		Tier:       provider.TierOnDemand,
		Image:      "ghcr.io/gpuai/vllm:latest",
		MinVRAMGB:  24,
	})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("expected ErrNoProvider when no GPU meets the VRAM floor, got %v", err)
	}
}

// An offering whose upstream reports MEASURED VRAM just under the nameplate
// (an L4 reads 22GiB via nvidia-smi for a 24GB card) must still clear a
// 24GB template floor. Template floors are authored in nameplate terms, so
// comparing them against the raw upstream figure dropped every L4 from a
// vLLM deploy — a "no capacity" for hardware the catalog advertises as 24GB.
func TestSelectProviderCandidates_TemplateFloorUsesNameplateVRAM(t *testing.T) {
	tid := "vllm"
	podco := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{vramOffering("l4", 22, 0.30, 5)},
	}
	e := newTestEngineWithProviders(podco)

	got, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		TemplateID: &tid,
		GPUType:    "",
		GPUCount:   1,
		Tier:       provider.TierOnDemand,
		Image:      "ghcr.io/gpuai/vllm:latest",
		MinVRAMGB:  24,
	})
	if err != nil {
		t.Fatalf("an L4 measuring 22GB must clear its own 24GB nameplate floor, got %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least one candidate")
	}

	// A card genuinely below the floor must still be dropped.
	small := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{vramOffering("rtx_a4000", 15, 0.10, 5)},
	}
	if _, err := newTestEngineWithProviders(small).SelectProviderCandidates(context.Background(), ProvisionRequest{
		TemplateID: &tid, GPUCount: 1, Tier: provider.TierOnDemand,
		Image: "ghcr.io/gpuai/vllm:latest", MinVRAMGB: 24,
	}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("a 16GB-nameplate card must not clear a 24GB floor, got %v", err)
	}
}
