package availability

// pergpu.go owns the ONE derivation of a per-GPU hourly rate from a whole-node
// rate. Every surface that speaks "$/GPU/hr" — the pricing comparison, the
// public catalog teaser, the market history rollup — divides here and nowhere
// else.
//
// It is exported and shared because the duplicate-copy version of this divide
// is what put b300 on the hosted GPU.ai site reading "184% more expensive than AWS": a $50.62
// 8-GPU node rate compared against AWS's $17.80 PER-GPU quote, when per GPU we
// are $6.3275 and roughly 64% CHEAPER. A second copy of this logic is a second
// chance to diverge on the zero-guard or the rounding, at a seam nobody looks
// at, producing a materially false public price claim.

// PerGPUHourly converts a whole-node hourly rate to a per-GPU hourly rate.
//
// An unreported or nonsensical GPUCount is treated as a single GPU, which
// leaves the price unchanged — the honest degrade, since the alternative is
// inventing a discount we cannot substantiate.
//
// The result is deliberately UNROUNDED. Rounding belongs at render time; a
// helper that rounded would make every caller's precision depend on which
// surface asked, which is the divergence this file exists to prevent.
func PerGPUHourly(nodePrice float64, gpuCount int) float64 {
	if gpuCount < 1 {
		return nodePrice
	}
	return nodePrice / float64(gpuCount)
}

// EffectiveGPUCount is the count PerGPUHourly actually divided by, so tie
// comparisons rank an unreported count the same way the price treats it.
//
// It lives beside PerGPUHourly and MUST move with it: splitting the two lets a
// caller rank on a raw count while pricing on a floored one, re-opening exactly
// the divergence the shared helper closes.
func EffectiveGPUCount(gpuCount int) int {
	if gpuCount < 1 {
		return 1
	}
	return gpuCount
}
