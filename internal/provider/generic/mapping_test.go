package generic

import (
	"testing"
)

// TestToGPUOfferings_WholeNodePricePassthrough pins the whole-node price
// invariant on multi-GPU offerings: the supplier's price_per_hour covers the
// whole gpu_count-GPU node and must be copied through unchanged — never
// multiplied (an N-fold overcharge) and never divided (the GPU-64 2-3×
// underquote theory, disproven 2026-09-01). The communitycloud twin carries the
// same assertion; this test is what fails if either transform sneaks in here.
func TestToGPUOfferings_WholeNodePricePassthrough(t *testing.T) {
	resp := &availableResponse{
		Offerings: []offeringJSON{
			{GPUType: "rtx_pro_6000", GPUCount: 1, PricePerHour: 1.76, Tier: "spot", Region: "US", AvailableCount: 1},
			{GPUType: "rtx_pro_6000", GPUCount: 2, PricePerHour: 3.52, Tier: "spot", Region: "US", AvailableCount: 1},
			{GPUType: "rtx_pro_6000", GPUCount: 8, PricePerHour: 14.08, Tier: "spot", Region: "US", AvailableCount: 2},
		},
	}

	got := toGPUOfferings(resp, "somesupplier")
	if len(got) != len(resp.Offerings) {
		t.Fatalf("toGPUOfferings returned %d offerings, want %d", len(got), len(resp.Offerings))
	}
	for i, o := range got {
		raw := resp.Offerings[i]
		if o.PricePerHour != raw.PricePerHour {
			t.Errorf("offering %d (gpu_count=%d): PricePerHour = %v, want the raw whole-node rate %v copied through unchanged",
				i, raw.GPUCount, o.PricePerHour, raw.PricePerHour)
		}
		if o.GPUCount != raw.GPUCount {
			t.Errorf("offering %d: GPUCount = %d, want %d", i, o.GPUCount, raw.GPUCount)
		}
	}
}

// TestToInstanceStatus_SSHPort: an optional ssh_port becomes the 22/tcp port
// mapping (the direct-connect SSH endpoint); omitting it adds no mapping, so
// the instance is reached on port 22.
func TestToInstanceStatus_SSHPort(t *testing.T) {
	got := toInstanceStatus(&statusResponse{UpstreamID: "i-1", Status: "running", IP: "192.0.2.7", SSHPort: 40022}, "acme")
	if got.UpstreamID != "acme:i-1" {
		t.Fatalf("UpstreamID = %q, want acme:i-1", got.UpstreamID)
	}
	if len(got.Ports) != 1 || got.Ports[0].PrivatePort != 22 || got.Ports[0].PublicPort != 40022 {
		t.Fatalf("Ports = %+v, want one 22->40022 mapping", got.Ports)
	}
	if none := toInstanceStatus(&statusResponse{UpstreamID: "i-2", Status: "running", IP: "192.0.2.8"}, "acme"); len(none.Ports) != 0 {
		t.Fatalf("no ssh_port must add no mapping, got %+v", none.Ports)
	}
	if bad := toInstanceStatus(&statusResponse{UpstreamID: "i-3", Status: "running", SSHPort: 70000}, "acme"); len(bad.Ports) != 0 {
		t.Fatalf("an out-of-range ssh_port must be ignored, got %+v", bad.Ports)
	}
}
