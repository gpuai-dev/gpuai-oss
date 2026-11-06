package provision

import (
	"strconv"
	"strings"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// GPU-87: a template's min_vram_gb was enforced ONLY when the deploy let the
// engine auto-select the GPU. An EXPLICIT gpu_type took the exact-match branch
// in SelectProviderCandidates and skipped the floor entirely, so sglang
// (min_vram_gb 24) was happily provisioned onto 16 GB rtx_a4000s — accepted
// with a 202, then dead on boot 1-2 minutes later. One org burned five launches
// that way before giving up.
//
// TemplateVRAMShortfall below is THE single comparison for that condition, and
// TemplateVRAMError renders THE single sentence explaining it. Three call sites
// route through them — the public create handler, the dashboard create handler,
// and SelectProviderCandidates — so the sync 422, the dashboard 422 and the
// engine failure cannot drift into three different explanations of one
// condition (the same discipline the disk gate applies via ErrDiskUnavailable).
//
// NEVER inline this comparison at a call site. The handlers reject early for UX;
// the engine check is the trust boundary that also catches callers who skip the
// handler gate. Both must reach the same verdict from the same inputs, and
// TestTemplateFloorHandlerEngineAgreement pins that for every GPU type in the
// canonical table.

// maxSuggestedGPUTypes caps how many compatible types the message names before
// folding the rest into "and N more". Eight fits on a terminal line while still
// spanning several price classes.
const maxSuggestedGPUTypes = 8

// TemplateVRAMError reports that an explicitly requested GPU type carries less
// VRAM than the template declares it needs. Its Error() is the customer-facing
// sentence: both numbers (so the gap is unambiguous), the template, the type,
// and the compatible types (so the fix does not require guessing).
type TemplateVRAMError struct {
	TemplateID string
	// MinVRAMGB is the EFFECTIVE floor: the template's static min_vram_gb,
	// possibly raised by the GPU-40 model-size guard.
	MinVRAMGB int
	GPUType   provider.GPUType
	// GPUVRAMGB is the type's normalized nameplate VRAM — the same figure
	// /v1/gpu-types advertises, not one supplier's measured report.
	GPUVRAMGB int
}

func (e *TemplateVRAMError) Error() string {
	var b strings.Builder
	b.WriteString("template ")
	b.WriteString(e.TemplateID)
	b.WriteString(" requires at least ")
	b.WriteString(strconv.Itoa(e.MinVRAMGB))
	b.WriteString(" GB of VRAM per GPU; ")
	b.WriteString(string(e.GPUType))
	b.WriteString(" has ")
	b.WriteString(strconv.Itoa(e.GPUVRAMGB))
	b.WriteString(" GB. ")

	compatible := provider.GPUTypesWithVRAMAtLeast(e.MinVRAMGB)
	if len(compatible) == 0 {
		// Unreachable for catalog templates (every floor we ship is met by
		// something), but a guard-raised floor is model-derived and could in
		// principle exceed the largest card we sell.
		b.WriteString("No GPU type currently offered meets that floor — see GET /v1/gpu-types.")
		return b.String()
	}
	shown := compatible
	remainder := 0
	if len(shown) > maxSuggestedGPUTypes {
		remainder = len(shown) - maxSuggestedGPUTypes
		shown = shown[:maxSuggestedGPUTypes]
	}
	b.WriteString("Compatible GPU types: ")
	for i, gt := range shown {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(string(gt))
	}
	if remainder > 0 {
		b.WriteString(", and ")
		b.WriteString(strconv.Itoa(remainder))
		b.WriteString(" more")
	}
	b.WriteString(" — see GET /v1/gpu-types.")
	return b.String()
}

// TemplateVRAMShortfall reports whether gpuType is too small for a template
// floor of minVRAMGB, returning the actionable typed error when it is and nil
// when the launch is allowed.
//
// Allowed (nil) when:
//   - minVRAMGB <= 0 — no floor declared, nothing to enforce (raw launches and
//     floorless templates stay byte-for-byte unchanged);
//   - the type is absent from the canonical VRAM table — FAIL OPEN, because
//     junk gpu_types belong to the invalid_gpu_type validation and a 422 from
//     here would be unfixable by the customer;
//   - the type's normalized VRAM is at or above the floor.
//
// The comparison is TYPE-level and runs through provider.NormalizeVRAM, which
// is the same normalization the auto-select per-offering filter resolves noisy
// upstream reports to (an L4 reports 22 GB for a 24 GB card, and template
// floors are authored in nameplate terms). Comparing per-offering instead would
// let the handler and the engine disagree whenever an upstream over-reports or
// ships a smaller variant under a shared name — and the type's nameplate is
// what the customer was shown before launching.
func TemplateVRAMShortfall(templateID string, minVRAMGB int, gpuType provider.GPUType) *TemplateVRAMError {
	if minVRAMGB <= 0 {
		return nil
	}
	canonical := provider.VRAMByGPUType(gpuType)
	if canonical <= 0 {
		return nil // unknown type: not this gate's decision
	}
	normalized := provider.NormalizeVRAM(gpuType, canonical)
	if normalized >= minVRAMGB {
		return nil
	}
	return &TemplateVRAMError{
		TemplateID: templateID,
		MinVRAMGB:  minVRAMGB,
		GPUType:    gpuType,
		GPUVRAMGB:  normalized,
	}
}
