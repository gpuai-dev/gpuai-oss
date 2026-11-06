package availability

import "testing"

// TestRetailPrice_NegativeFloor pins the v101 loss-floor semantics: a NEGATIVE
// minMarkupPct is a per-supplier sanction to publish below cost by at most
// that percentage. The floor keeps its protective shape — it bounds how far
// below cost the price can go, it does not push prices down by itself.
func TestRetailPrice_NegativeFloor(t *testing.T) {
	tests := []struct {
		name         string
		costBasis    float64
		markupPct    float64
		minMarkupPct float64
		want         float64
	}{
		{
			// The undercut configuration: markup −10 prices the loss, floor −10
			// sanctions it. 1.00 × 0.90 = 0.90 exactly.
			name:      "sanctioned 10% loss sells below cost",
			costBasis: 1.00, markupPct: -10, minMarkupPct: -10, want: 0.90,
		},
		{
			// The floor still clamps a fat-fingered markup at the sanctioned
			// rate: −50 markup with a −10 floor loses 10%, never 50%.
			name:      "floor clamps a deeper markup at the sanctioned loss",
			costBasis: 1.00, markupPct: -50, minMarkupPct: -10, want: 0.90,
		},
		{
			// A negative floor alone must NOT lower the price — it is a bound,
			// not a target. Markup 0 keeps passthrough pricing.
			name:      "negative floor with zero markup stays at cost",
			costBasis: 1.00, markupPct: 0, minMarkupPct: -10, want: 1.00,
		},
		{
			// Sub-cent rounding under a loss floor still rounds the FLOOR up:
			// 1.188 × 0.9 (true cost) × 0.9 (10% loss) = 0.96228 → markup branch
			// rounds to 0.96, floor ceils to 0.97 — the realized loss can be
			// slightly SMALLER than sanctioned, never larger.
			name:      "floor rounds up so the loss never exceeds the sanction",
			costBasis: 1.0692, markupPct: -50, minMarkupPct: -10, want: 0.97,
		},
		{
			// Default floor 0 preserves the pre-v101 clamp exactly: negative
			// markup floors at cost.
			name:      "zero floor still refuses below-cost",
			costBasis: 1.00, markupPct: -10, minMarkupPct: 0, want: 1.00,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RetailPrice(tt.costBasis, tt.markupPct, tt.minMarkupPct); got != tt.want {
				t.Errorf("RetailPrice(%v, %v, %v) = %v, want %v",
					tt.costBasis, tt.markupPct, tt.minMarkupPct, got, tt.want)
			}
		})
	}
}
