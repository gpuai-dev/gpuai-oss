package availability

import (
	"context"
	"testing"

	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// TestToStaffOffering — the staff view keeps everything the customer strip
// removes (provider, wholesale, native region/site) while mirroring the
// customer derivations (canonical region, retail price, boot class).
func TestToStaffOffering(t *testing.T) {
	o := provider.GPUOffering{
		Provider:           fixtureVM,
		GPUType:            provider.GPUTypeH200NVL,
		GPUCount:           1,
		VRAMPerGPUGB:       0, // omitted upstream — must fall back to the canonical table
		CPUCores:           16,
		RAMGB:              192,
		PricePerHour:       3.20,
		Tier:               provider.TierOnDemand,
		Region:             "fra",
		DatacenterLocation: "fra",
		StockStatus:        "in_stock",
		AvailableCount:     1,
	}
	so := ToStaffOffering(o, 10.0, DefaultMinRetailMarkupPct, 0)

	if so.Provider != fixtureVM {
		t.Errorf("Provider = %q, want %s (staff view keeps identity)", so.Provider, fixtureVM)
	}
	if so.WholesalePerHour != 3.20 {
		t.Errorf("WholesalePerHour = %v, want 3.20", so.WholesalePerHour)
	}
	if want := RetailPrice(3.20, 10.0, DefaultMinRetailMarkupPct); so.RetailPerHour != want {
		t.Errorf("RetailPerHour = %v, want %v", so.RetailPerHour, want)
	}
	if so.MarkupPct != 10.0 {
		t.Errorf("MarkupPct = %v, want 10", so.MarkupPct)
	}
	if so.Region != "eu-west" {
		t.Errorf("Region = %q, want canonical eu-west", so.Region)
	}
	if so.NativeRegion != "fra" {
		t.Errorf("NativeRegion = %q, want the raw native id", so.NativeRegion)
	}
	if so.VRAMGB != provider.VRAMByGPUType(provider.GPUTypeH200NVL) {
		t.Errorf("VRAMGB = %d, want the canonical fallback", so.VRAMGB)
	}
	if so.InstantBoot { // the VM fixture is a slow-boot provider
		t.Error("InstantBoot = true, want false for a slow-boot VM provider")
	}
	if so.DeploymentType != "vm" {
		t.Errorf("DeploymentType = %q, want vm", so.DeploymentType)
	}
}

// TestCollectOfferings_StaffSnapshotMirrorsCustomer — the poller builds the
// staff twin in the same pass: same row count, provider attribution intact,
// and rows dropped from the customer catalog (unknown region) are dropped
// from the staff view too.
func TestCollectOfferings_StaffSnapshotMirrorsCustomer(t *testing.T) {
	fake := &fakeProvider{
		name: "stubprov",
		offerings: []provider.GPUOffering{
			{
				Provider:     "stubprov",
				GPUType:      provider.GPUTypeA10080GB,
				GPUCount:     1,
				PricePerHour: 1.5,
				Tier:         provider.TierOnDemand,
				Region:       "us-east",
			},
			{
				Provider:     "stubprov",
				GPUType:      provider.GPUTypeA10080GB,
				GPUCount:     1,
				PricePerHour: 1.5,
				Tier:         provider.TierOnDemand,
				Region:       "", // normalizes to unknown — dropped from BOTH views
			},
		},
	}
	reg := provider.NewRegistry()
	reg.Register(fake)
	poller := &Poller{
		registry:     reg,
		probeTimeout: time.Second,
		logger:       silentLogger(),
	}

	customer, staff := poller.collectOfferings(context.Background())
	if len(customer) != 1 || len(staff) != 1 {
		t.Fatalf("customer/staff rows = %d/%d, want 1/1 (unknown-region row dropped from both)",
			len(customer), len(staff))
	}
	if staff[0].Provider != "stubprov" {
		t.Errorf("staff row Provider = %q, want stubprov", staff[0].Provider)
	}
}
