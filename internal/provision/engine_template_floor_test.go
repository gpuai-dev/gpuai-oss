// GPU-87: the engine-side half of the template min_vram_gb hard block.
//
// The auto-select path has filtered by the template's VRAM floor since TMPL-04,
// but an EXPLICIT gpu_type took the exact-match branch and sailed straight past
// it — which is how sglang (floor 24) landed on 16 GB rtx_a4000s in prod and
// died on boot. The handlers reject that combination synchronously; these tests
// pin the engine backstop that catches any caller who skips the handler gate
// (old CLI, internal caller), and the cross-layer matrix that makes handler and
// engine structurally incapable of disagreeing.
package provision

import (
	"context"
	"errors"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// templateFloorReq builds the explicit-gpu_type template deploy shape: custom
// image (so the custom-image gate applies), one GPU, on-demand.
func templateFloorReq(templateID string, gpuType provider.GPUType, floor int) ProvisionRequest {
	return ProvisionRequest{
		TemplateID: &templateID,
		GPUType:    gpuType,
		GPUCount:   1,
		Tier:       provider.TierOnDemand,
		Image:      "ghcr.io/gpuai/" + templateID + ":latest",
		MinVRAMGB:  floor,
	}
}

// The exact prod shape: plenty of in-stock rtx_a4000 capacity, but the template
// declares it cannot run there. The customer must get the actionable typed
// error, never a candidate and never a generic "no provider".
func TestSelectProviderCandidates_ExplicitTypeBelowTemplateFloor(t *testing.T) {
	podco := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{vramOffering("rtx_a4000", 16, 0.17, 9)},
	}
	e := newTestEngineWithProviders(podco)

	candidates, err := e.SelectProviderCandidates(context.Background(),
		templateFloorReq("sglang", provider.GPUTypeRTXA4000, 24))

	var tvErr *TemplateVRAMError
	if !errors.As(err, &tvErr) {
		t.Fatalf("expected *TemplateVRAMError for an under-floor explicit type, got %v", err)
	}
	if errors.Is(err, ErrNoProvider) {
		t.Error("must not degrade into ErrNoProvider — 'no capacity' is not the problem, the GPU is too small")
	}
	if len(candidates) != 0 {
		t.Errorf("expected zero candidates, got %d", len(candidates))
	}
	if tvErr.MinVRAMGB != 24 || tvErr.GPUVRAMGB != 16 || tvErr.GPUType != provider.GPUTypeRTXA4000 {
		t.Errorf("typed error = %+v; want floor 24, gpu 16, rtx_a4000", tvErr)
	}
}

// An explicit type AT the floor is untouched — the exact-match branch still
// runs and still returns candidates.
func TestSelectProviderCandidates_ExplicitTypeAtTemplateFloorUnchanged(t *testing.T) {
	podco := &stubProvider{
		name: "podco",
		// 22 is what an upstream reports for a 24 GB L4; the floor is written
		// in nameplate terms, so this must clear it.
		offerings: []provider.GPUOffering{vramOffering("l4", 22, 0.43, 4)},
	}
	e := newTestEngineWithProviders(podco)

	candidates, err := e.SelectProviderCandidates(context.Background(),
		templateFloorReq("sglang", provider.GPUTypeL4, 24))
	if err != nil {
		t.Fatalf("an l4 at the 24 GB nameplate floor must still be a candidate, got %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("expected at least one candidate")
	}
	if got := candidates[0].Offering.GPUType; got != provider.GPUTypeL4 {
		t.Errorf("candidate gpu type = %q, want l4", got)
	}
}

// A raw (no-template) launch on the same small GPU is byte-unchanged: no
// template means no floor to enforce.
func TestSelectProviderCandidates_RawLaunchNoTemplateFloorUnchanged(t *testing.T) {
	podco := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{vramOffering("rtx_a4000", 16, 0.17, 9)},
	}
	e := newTestEngineWithProviders(podco)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:  provider.GPUTypeRTXA4000,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
	})
	if err != nil {
		t.Fatalf("raw rtx_a4000 launch must be unaffected, got %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("expected the raw launch to produce candidates")
	}
}

// The backstop must be TYPE-level and PRE-loop: with zero live offerings of the
// under-floor type, the old code path would fall through to ErrNoProvider and
// tell the customer to retry later — advice that can never work. The actionable
// error has to win even when there is nothing in stock to compare against.
func TestSelectProviderCandidates_UnderFloorBeatsNoCapacity(t *testing.T) {
	podco := &stubProvider{name: "podco"} // zero offerings
	e := newTestEngineWithProviders(podco)

	_, err := e.SelectProviderCandidates(context.Background(),
		templateFloorReq("sglang", provider.GPUTypeRTXA4000, 24))

	var tvErr *TemplateVRAMError
	if !errors.As(err, &tvErr) {
		t.Fatalf("expected *TemplateVRAMError with zero offerings listed, got %v", err)
	}
	if errors.Is(err, ErrNoProvider) {
		t.Error("an unfixable-by-retry condition must not be reported as no-capacity")
	}
}

// TestTemplateFloorHandlerEngineAgreement is the cross-layer pin required by
// GPU-87: the engine's verdict must equal TemplateVRAMShortfall's verdict for
// EVERY GPU type in the canonical table at every interesting floor. Both
// handlers call TemplateVRAMShortfall directly and assert its exact message, so
// this equivalence is what makes handler and engine one source of truth rather
// than two comparisons that can drift apart.
func TestTemplateFloorHandlerEngineAgreement(t *testing.T) {
	// GPUTypesWithVRAMAtLeast(1) is the whole canonical table (every row is
	// > 0 GB, enforced by TestVRAMByGPUTypeCoversAllGPUTypes).
	allTypes := provider.GPUTypesWithVRAMAtLeast(1)
	if len(allTypes) < 20 {
		t.Fatalf("expected the full canonical GPU table, got %d types", len(allTypes))
	}
	floors := []int{1, 16, 17, 24, 48, 141, 999}

	for _, gt := range allTypes {
		for _, floor := range floors {
			canonical := provider.NormalizeVRAM(gt, provider.VRAMByGPUType(gt))
			podco := &stubProvider{
				name:      "podco",
				offerings: []provider.GPUOffering{vramOffering(string(gt), canonical, 1.00, 5)},
			}
			e := newTestEngineWithProviders(podco)

			_, err := e.SelectProviderCandidates(context.Background(),
				templateFloorReq("sglang", gt, floor))

			var tvErr *TemplateVRAMError
			engineBlocked := errors.As(err, &tvErr)
			predicateBlocked := TemplateVRAMShortfall("sglang", floor, gt) != nil

			if engineBlocked != predicateBlocked {
				t.Errorf("disagreement for %q at floor %d: engine blocked=%v, predicate blocked=%v (err=%v)",
					gt, floor, engineBlocked, predicateBlocked, err)
			}
			if !predicateBlocked && err != nil {
				t.Errorf("%q at floor %d: predicate allows but engine failed with %v", gt, floor, err)
			}
		}
	}
}
