package provider

import "sort"

// vramByGPUType is the canonical per-GPU VRAM (GB) for every GPUType constant.
// Several upstream catalogs omit per-GPU VRAM (some flavor or plan rows carry
// no memory figure at all), so adapters fall back to this
// table to keep GPUOffering.VRAMPerGPUGB populated — a 0 here surfaces to
// customers as "0 GB" in /v1/gpu-types and the dashboard.
//
// Keep this in sync with the GPUType constants in types.go: every new GPU type
// needs a row (TestVRAMByGPUTypeCoversAllGPUTypes enforces it).
var vramByGPUType = map[GPUType]int{
	// Blackwell
	GPUTypeB200: 192,
	GPUTypeB300: 288,

	// Hopper
	GPUTypeH200SXM:  141,
	GPUTypeH200NVL:  141,
	GPUTypeH100SXM:  80,
	GPUTypeH100NVL:  94,
	GPUTypeH100PCIE: 80,

	// Ampere data center
	GPUTypeA10080GB: 80,
	GPUTypeA10040GB: 40,
	GPUTypeA40:      48,
	GPUTypeA30:      24,
	GPUTypeA16:      16, // per-GPU: each of the A16 board's four GPUs carries 16 GB

	// Ada Lovelace / Ampere professional / Blackwell professional
	GPUTypeL40S:       48,
	GPUTypeL40:        48,
	GPUTypeL4:         24,
	GPUTypeRTX6000Ada: 48,
	GPUTypeRTX5000Ada: 32,
	GPUTypeRTX4000Ada: 20,
	GPUTypeRTX2000Ada: 16,
	GPUTypeRTXA6000:   48,
	GPUTypeRTXA5000:   24,
	GPUTypeRTXA4500:   20,
	GPUTypeRTXA4000:   16,
	GPUTypeRTXPro6000: 96,
	GPUTypeRTXPro4500: 32,

	// Consumer
	GPUTypeRTX5090: 32,
	GPUTypeRTX5080: 16,
	GPUTypeRTX4090: 24,
	GPUTypeRTX4080: 16,
	GPUTypeRTX3090: 24,
	GPUTypeRTX3080: 10,

	// Legacy
	GPUTypeV100: 16,
	GPUTypeT4:   16,
}

// VRAMByGPUType returns the canonical per-GPU VRAM (GB) for a known GPU type,
// or 0 for an unknown type. Use as the fallback whenever an upstream catalog
// omits (or zeroes) per-GPU VRAM.
func VRAMByGPUType(t GPUType) int {
	return vramByGPUType[t]
}

// GPUTypesWithVRAMAtLeast returns every GPU type whose canonical VRAM is at
// least minGB, ordered by canonical VRAM ascending with name-ascending
// tie-breaks (so the cheapest-class qualifying GPU leads the list and the order
// is stable across runs — map iteration is not).
//
// This is the escape hatch behind GPU-87's template-floor rejection: telling a
// customer only that their chosen GPU is too small leaves them guessing the
// next one, which is exactly the retry loop the ticket exists to end. Sourced
// from the canonical nameplate table, so the answer carries no supplier
// identity (T-63-04) — it is the same figure /v1/gpu-types advertises.
func GPUTypesWithVRAMAtLeast(minGB int) []GPUType {
	out := make([]GPUType, 0, len(vramByGPUType))
	for gt, vram := range vramByGPUType {
		if vram >= minGB {
			out = append(out, gt)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if vi, vj := vramByGPUType[out[i]], vramByGPUType[out[j]]; vi != vj {
			return vi < vj
		}
		return out[i] < out[j]
	})
	return out
}

// vramNoiseFloor is the fraction of canonical VRAM above which an upstream's
// reported figure is treated as the SAME silicon reported differently, rather
// than a different card. 0.9 clears every measurement artifact we see (the
// widest is the L4's 22-vs-24, at 0.917) while staying far below the smallest
// real capacity split we sell (A100 40-vs-80 and V100 16-vs-32 are both 0.5).
const vramNoiseFloor = 0.9

// NormalizeVRAM resolves the per-GPU VRAM (GB) to advertise for a GPU type
// given one upstream's reported figure.
//
// Upstreams disagree about the same card. Some pass through
// nvidia-smi-derived totals, which are MiB→GiB
// conversions net of ECC reservation and therefore land just BELOW the
// nameplate GB every spec sheet quotes: B200 179 vs 192, H200 140 vs 141,
// L40S 45 vs 48, L4 22 vs 24, RTX A4000 15 vs 16. Others report nothing and
// fall back to the nameplate table here. Advertising
// whichever provider happened to be sampled made identical hardware look
// worse on some rows than others.
//
// So: a reported figure within vramNoiseFloor of canonical is measurement
// noise about the SAME card — advertise the nameplate. A figure materially
// below canonical is a genuinely smaller variant sharing one upstream name
// (the V100 16-vs-32 and A100 40-vs-80 cases) — advertise what was reported,
// because that is what the customer receives. A figure ABOVE canonical is
// also honored: over-delivery is never a mislabel.
//
// Returns canonical when the upstream reported nothing, and the reported
// value unchanged for a GPU type absent from the canonical table.
func NormalizeVRAM(t GPUType, reportedGB int) int {
	canonical := vramByGPUType[t]
	if canonical <= 0 {
		return reportedGB
	}
	if reportedGB <= 0 {
		return canonical
	}
	if reportedGB < canonical && float64(reportedGB) >= vramNoiseFloor*float64(canonical) {
		return canonical
	}
	return reportedGB
}
