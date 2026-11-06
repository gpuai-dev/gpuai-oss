package provision

import (
	"context"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/config"
)

// TestWholesaleFundedBy_NegativeMargin pins the v101 inversion fix: under a
// sanctioned loss floor (markup −10, floor −10) an approved retail of $0.90
// genuinely funds a $1.00 wholesale cost. The old `markup < 0 → 0` clamp
// inverted at 0% and handed back ~$0.905 — refusing the very price the
// customer just confirmed, the infinite-409 loop wholesaleFundedBy's own
// comment predicted.
func TestWholesaleFundedBy_NegativeMargin(t *testing.T) {
	cfg := &config.Config{LaunchIncludedDiskGB: 100}
	ctx := context.Background()

	t.Run("sanctioned loss raises the ceiling to what the retail funds", func(t *testing.T) {
		e := &Engine{config: cfg, flags: fakeFlags{
			markup:    map[string]float64{"generic": -10},
			minMargin: map[string]float64{"generic": -10},
		}}
		funded := e.wholesaleFundedBy(ctx, 0.90, "generic", 100)
		if funded < 1.00 {
			t.Fatalf("approved retail 0.90 at a sanctioned -10%% funds %v wholesale, want >= 1.00 — "+
				"under-funding here 409s the price the customer just confirmed", funded)
		}
		// Round-trip: the funded wholesale re-prices within a cent of the
		// approved retail. It may exceed it by up to one cent (the slack in
		// the numerator plus the floor's round-UP) — that overhang is the
		// documented design: billedRetail clamps the bill at the approved cap,
		// so the slack widens what we can BUY, never what the customer pays.
		if back := e.retailWithDisk(ctx, funded, "generic", 100); back > 0.91 {
			t.Fatalf("funded wholesale %v re-prices at %v, more than a cent above the approved 0.90", funded, back)
		}
	})

	t.Run("floor wins over a deeper markup in the inversion", func(t *testing.T) {
		// Pricing takes max(markup, floor) = −10, so the inversion must too:
		// inverting through the −50 markup would over-fund by ~80%.
		e := &Engine{config: cfg, flags: fakeFlags{
			markup:    map[string]float64{"generic": -50},
			minMargin: map[string]float64{"generic": -10},
		}}
		funded := e.wholesaleFundedBy(ctx, 0.90, "generic", 100)
		if funded > 1.01 {
			t.Fatalf("funded %v — inverted through the markup instead of the clamping floor", funded)
		}
	})

	t.Run("at or below -100 is unfundable, not a divide-by-zero", func(t *testing.T) {
		e := &Engine{config: cfg, flags: fakeFlags{
			markup:    map[string]float64{"generic": -150},
			minMargin: map[string]float64{"generic": -150},
		}}
		if funded := e.wholesaleFundedBy(ctx, 0.90, "generic", 100); funded != 0 {
			t.Fatalf("funded %v at an effective -150%%, want 0 (no ceiling widened)", funded)
		}
	})
}
