package public

import (
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
)

// siblingOffer builds one cache row for a /v1/pricing merge test. Every field
// except price, specs and the pin token is shared, so the rows are
// customer-INDISTINGUISHABLE (identical pricingOfferKey) while being different
// underlying products — the shape GPU-55 makes the offering id express.
func siblingOffer(offeringID string, price float64, vcpu, ramGB, available int) availability.AvailableOffering {
	return availability.AvailableOffering{
		GPUModel:       "rtx_a6000",
		GPUCount:       4,
		VRAMGB:         48,
		CPUCores:       vcpu,
		RAMGB:          ramGB,
		StorageGB:      1000,
		PricePerHour:   price,
		Region:         "us-central",
		Tier:           "on_demand",
		AvailableCount: available,
		CapacityClass:  "secure",
		DeploymentType: "container",
		InstanceDiskGB: 100,
		OfferingID:     offeringID,
	}
}

// TestPricing_PriceTiedSiblingsSingleRowAndPricedPin — the GPU-55 catalog-side
// blast-radius contract for /v1/pricing.
//
// Folding UpstreamGPUID into the offering id splits price-tied sibling products
// (one supplier's gpu_4x_A6000_high_perf vs gpu_4x_a6000; another's A100 PCIe vs
// A100-SXM4; separate vultr plans / deploy ids) into DISTINCT ids where
// they previously shared one. Two invariants must survive that:
//
//  1. NO DUPLICATE ROWS. Row cardinality is a function of pricingOfferKey alone,
//     which deliberately excludes offering_id, so siblings still collapse to one
//     card. `gpu pricing` output shape and the dashboard's row counts are
//     unchanged by the seed change.
//  2. THE PIN NAMES THE PRICED MACHINE. The row publishes a cheapest-wins price
//     and cheapest-wins specs; the pin must come from that SAME offer. The old
//     first-sighting pin was masked while siblings shared an id (the engine's
//     price sort landed on the cheap one anyway) — once ids split it would hand
//     the customer a pricier machine than the row advertised.
func TestPricing_PriceTiedSiblingsSingleRowAndPricedPin(t *testing.T) {
	// Case one: same price, different product, different specs.
	// The HIGHER-spec sibling deliberately carries the HIGHER offering id, so a
	// pure lowest-id tie-break would publish the 30/192 machine — the outcome
	// the customer-favorable ladder exists to prevent.
	highPerf := siblingOffer("ffff99998888", 1.88, 54, 256, 2)
	base := siblingOffer("aaaa11112222", 1.88, 30, 192, 3)

	rows := mergePricingOffers([]availability.AvailableOffering{highPerf, base}, false)
	if len(rows) != 1 {
		t.Fatalf("price-tied siblings produced %d rows, want 1 — splitting offering ids must not duplicate customer-visible cards; rows=%+v", len(rows), rows)
	}
	if rows[0].Available != 5 {
		t.Errorf("available = %d, want 5 (summed across merged siblings)", rows[0].Available)
	}
	// Customer-favorable AND deterministic: at an identical price the richer
	// host wins (more RAM, then more vCPU, then deeper stock), and the result
	// must not depend on the order the poller happened to append offers in.
	for _, in := range [][]availability.AvailableOffering{
		{highPerf, base},
		{base, highPerf},
	} {
		got := mergePricingOffers(in, false)
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1", len(got))
		}
		if got[0].OfferingID != "ffff99998888" {
			t.Errorf("tied pin = %q, want the higher-spec sibling ffff99998888 — at the same price the customer must not be pinned to the 30/192 machine because its hash sorts lower", got[0].OfferingID)
		}
		if got[0].RAMGB != 256 || got[0].CPUCores != 54 {
			t.Errorf("tied specs = %d vCPU / %d GB, want the higher-spec 54 / 256", got[0].CPUCores, got[0].RAMGB)
		}
	}

	// Genuinely indistinguishable machines still fall through to the id, which
	// is what keeps the projection deterministic across poll cycles.
	twinLow := siblingOffer("1111aaaabbbb", 1.88, 30, 192, 3)
	twinHigh := siblingOffer("2222ccccdddd", 1.88, 30, 192, 3)
	for _, in := range [][]availability.AvailableOffering{
		{twinHigh, twinLow},
		{twinLow, twinHigh},
	} {
		got := mergePricingOffers(in, false)
		if len(got) != 1 || got[0].OfferingID != "1111aaaabbbb" {
			t.Errorf("identical tied machines published %+v, want the lowest id 1111aaaabbbb as the final deterministic fallback", got)
		}
	}

	// Equal price and equal specs: the deeper stock wins before the id does.
	shallow := siblingOffer("3333aaaabbbb", 1.88, 30, 192, 1)
	deep := siblingOffer("4444ccccdddd", 1.88, 30, 192, 9)
	for _, in := range [][]availability.AvailableOffering{
		{shallow, deep},
		{deep, shallow},
	} {
		got := mergePricingOffers(in, false)
		if len(got) != 1 || got[0].OfferingID != "4444ccccdddd" {
			t.Errorf("spec-tied siblings published %+v, want the deeper-stock 4444ccccdddd (a launch is likelier to succeed and the pin likelier to survive)", got)
		}
	}

	// Case two: same key, DIFFERENT prices. The pricier product is seen
	// first, which is exactly the ordering the old first-sighting pin got wrong.
	pricier := siblingOffer("cccc55556666", 2.50, 64, 512, 1)
	cheaper := siblingOffer("dddd77778888", 1.99, 32, 256, 4)

	split := mergePricingOffers([]availability.AvailableOffering{pricier, cheaper}, false)
	if len(split) != 1 {
		t.Fatalf("price-split siblings produced %d rows, want 1; rows=%+v", len(split), split)
	}
	row := split[0]
	if row.PricePerHour != 1.99 {
		t.Errorf("price = %v, want the cheapest 1.99", row.PricePerHour)
	}
	if row.CPUCores != 32 || row.RAMGB != 256 {
		t.Errorf("specs = %d vCPU / %d GB, want the priced machine's 32 / 256", row.CPUCores, row.RAMGB)
	}
	if row.OfferingID != "dddd77778888" {
		t.Errorf("pin = %q, want the PRICED offer's id dddd77778888 — publishing a $1.99 row that pins the $2.50 machine bills the customer more than the card showed", row.OfferingID)
	}

	// Control: genuinely distinguishable offerings still split into separate
	// rows (the merge key, not the pin, decides cardinality).
	certified := siblingOffer("eeee99990000", 1.88, 54, 256, 2)
	certified.CertifiedImage = true
	both := mergePricingOffers([]availability.AvailableOffering{highPerf, certified}, false)
	if len(both) != 2 {
		t.Errorf("certified vs non-certified merged into %d rows, want 2 — certified_image is a distinguishing key field", len(both))
	}
}

// TestPricing_MergeUnchangedForUnstampedOfferings — the eight adapters that
// never stamp UpstreamGPUID keep one offering id per (type, region, count, tier,
// class), so their rows merge exactly as before: one row, summed availability,
// cheapest price, and a pin equal to the single shared id no matter how many
// per-datacenter sightings collapse into it.
func TestPricing_MergeUnchangedForUnstampedOfferings(t *testing.T) {
	shared := "ffff11112222"
	a := siblingOffer(shared, 1.10, 32, 128, 2)
	b := siblingOffer(shared, 1.10, 32, 128, 3)
	c := siblingOffer(shared, 1.05, 32, 128, 1)

	rows := mergePricingOffers([]availability.AvailableOffering{a, b, c}, false)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].OfferingID != shared {
		t.Errorf("pin = %q, want the shared id %q", rows[0].OfferingID, shared)
	}
	if rows[0].PricePerHour != 1.05 || rows[0].Available != 6 {
		t.Errorf("row = %v/hr with %d available, want 1.05 with 6", rows[0].PricePerHour, rows[0].Available)
	}
}

// TestPricing_ZeroPriceSiblingNeverShadowsRealPrice — regression guard on the
// promote ladder: a $0 listing (dedicated-only / reserved-style capacity) must
// never take the price, the specs OR the pin from a real priced offer, in either
// input order.
func TestPricing_ZeroPriceSiblingNeverShadowsRealPrice(t *testing.T) {
	free := siblingOffer("0000aaaabbbb", 0, 8, 32, 1)
	real := siblingOffer("9999ccccdddd", 1.42, 48, 384, 2)

	for _, in := range [][]availability.AvailableOffering{
		{free, real},
		{real, free},
	} {
		rows := mergePricingOffers(in, false)
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1", len(rows))
		}
		if rows[0].PricePerHour != 1.42 {
			t.Errorf("price = %v, want the only positive price 1.42", rows[0].PricePerHour)
		}
		if rows[0].OfferingID != "9999ccccdddd" || rows[0].CPUCores != 48 {
			t.Errorf("row published pin %q / %d vCPU, want the priced offer 9999ccccdddd / 48", rows[0].OfferingID, rows[0].CPUCores)
		}
	}
}
