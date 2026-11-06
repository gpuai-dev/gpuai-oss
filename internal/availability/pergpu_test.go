package availability

import "testing"

// TestPerGPUHourly pins the shared derivation alongside its companion
// EffectiveGPUCount — they are asserted in ONE table on purpose, because the
// contract that matters is that the count the price divides by and the count
// tie-breaks rank on are the same number.
func TestPerGPUHourly(t *testing.T) {
	tests := []struct {
		name          string
		nodePrice     float64
		gpuCount      int
		wantPerGPU    float64
		wantEffective int
	}{
		{
			// The b300 case from production: a $50.62 whole-node 8-GPU rate was
			// compared against AWS's $17.80 per-GPU quote and rendered as "184%
			// more expensive". Per GPU we are $6.3275 — roughly 64% cheaper.
			name:          "b300 8-GPU node, the production case",
			nodePrice:     50.62,
			gpuCount:      8,
			wantPerGPU:    6.3275,
			wantEffective: 8,
		},
		{
			name:          "single GPU node passes through unchanged",
			nodePrice:     2.49,
			gpuCount:      1,
			wantPerGPU:    2.49,
			wantEffective: 1,
		},
		{
			// Unreported count: the honest degrade is "assume one GPU", which
			// leaves the price alone. Dividing by an invented count would
			// advertise a discount we cannot substantiate.
			name:          "unreported count leaves the price alone",
			nodePrice:     12.00,
			gpuCount:      0,
			wantPerGPU:    12.00,
			wantEffective: 1,
		},
		{
			name:          "negative count is treated the same as unreported",
			nodePrice:     12.00,
			gpuCount:      -1,
			wantPerGPU:    12.00,
			wantEffective: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PerGPUHourly(tt.nodePrice, tt.gpuCount); got != tt.wantPerGPU {
				t.Errorf("PerGPUHourly(%v, %d) = %v, want %v", tt.nodePrice, tt.gpuCount, got, tt.wantPerGPU)
			}
			if got := EffectiveGPUCount(tt.gpuCount); got != tt.wantEffective {
				t.Errorf("EffectiveGPUCount(%d) = %d, want %d", tt.gpuCount, got, tt.wantEffective)
			}
		})
	}
}

// TestPerGPUHourlyDoesNotRound proves the helper hands back the raw quotient.
// Rounding is a render-time concern; a helper that rounded would make each
// caller's precision depend on which surface asked — the divergence this
// extraction exists to close.
func TestPerGPUHourlyDoesNotRound(t *testing.T) {
	got := PerGPUHourly(10.0, 3)
	if want := 10.0 / 3.0; got != want {
		t.Errorf("PerGPUHourly(10.0, 3) = %v, want the unrounded %v", got, want)
	}
	if got == 3.33 || got == 3.3333 {
		t.Errorf("PerGPUHourly(10.0, 3) = %v, which is rounded; the helper must not round", got)
	}
}
