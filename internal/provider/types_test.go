package provider

import "testing"

// TestInstanceTier_Reserved pins that TierReserved exists as a first-class
// tier constant with the canonical string value "reserved". This is the
// front door for reserved-cluster routing — the engine (Plan 30-03)
// branches on Tier == TierReserved to dispatch to the reservation slot
// allocator instead of the spot/on-demand provider path.
//
// Plan 30-04 RED gate: this test must fail before TierReserved is added.
func TestInstanceTier_Reserved(t *testing.T) {
	if string(TierReserved) != "reserved" {
		t.Fatalf("TierReserved: got %q, want %q", string(TierReserved), "reserved")
	}
}

// TestInstanceTier_AllValues pins the full set of supported tiers so a
// future change to add or rename a tier touches this guardrail and shows
// up in code review. Order is irrelevant; identity is what matters.
func TestInstanceTier_AllValues(t *testing.T) {
	want := map[InstanceTier]string{
		TierOnDemand: "on_demand",
		TierSpot:     "spot",
		TierReserved: "reserved",
	}
	for tier, str := range want {
		if string(tier) != str {
			t.Errorf("tier %v: got %q, want %q", tier, string(tier), str)
		}
	}
}
