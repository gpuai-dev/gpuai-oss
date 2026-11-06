package availability

import (
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// TestToAvailableOffering_InstanceDiskAttributes pins the two additive catalog
// attributes, and specifically pins the ONE property that makes instance_disk_gb
// worth publishing at all: it is a promise the platform can keep, never a
// display assumption.
//
// The discriminator is provider.ConfigurableDisk, not the deployment type. Only
// adapters registered with Traits.ConfigurableDisk consume
// ProvisionRequest.ContainerDiskGB; generic omits it from its provision request
// entirely. Keying on "container" would
// therefore promise the configured default to launches that demonstrably do
// not receive it — the exact fabrication class this feature exists to remove
// (see the 40 GB storage fill).
func TestToAvailableOffering_InstanceDiskAttributes(t *testing.T) {
	const configuredDefault = 100

	tests := []struct {
		name             string
		providerName     string
		reportedStorage  int
		defaultDiskGB    int
		wantDisk         int
		wantConfigurable bool
		why              string
	}{
		{
			name:             "configurable container publishes the configured default",
			providerName:     fixtureDisk,
			reportedStorage:  500, // host disk — NOT what a container launch gets
			defaultDiskGB:    configuredDefault,
			wantDisk:         configuredDefault,
			wantConfigurable: true,
			why:              "a disk-configurable adapter honors ContainerDiskGB, so the disk we ask for is the disk that arrives",
		},
		{
			name:             "second configurable provider behaves identically",
			providerName:     fixtureMarket,
			reportedStorage:  2000,
			defaultDiskGB:    configuredDefault,
			wantDisk:         configuredDefault,
			wantConfigurable: true,
			why:              "a second ConfigurableDisk adapter gets the same treatment",
		},
		{
			name:             "vm provider publishes the machine's reported disk",
			providerName:     fixtureVM,
			reportedStorage:  750,
			defaultDiskGB:    configuredDefault,
			wantDisk:         750,
			wantConfigurable: false,
			why:              "we boot the flavor the upstream hands us; its root volume is the disk",
		},
		{
			name:             "vm provider reporting nothing publishes unknown, not the default",
			providerName:     fixtureVM,
			reportedStorage:  0,
			defaultDiskGB:    configuredDefault,
			wantDisk:         0,
			wantConfigurable: false,
			why:              "0 is an honest unknown; publishing the default here would promise disk we do not control",
		},
		{
			name:             "generic is excluded from the allowlist and is treated as unknown",
			providerName:     "generic",
			reportedStorage:  0,
			defaultDiskGB:    configuredDefault,
			wantDisk:         0,
			wantConfigurable: false,
			why:              "generic's provisionRequest literal omits ContainerDiskGB — the field is dropped with no error",
		},
		{
			name:             "the published default tracks config, it is not a constant",
			providerName:     fixtureDisk,
			reportedStorage:  0,
			defaultDiskGB:    150,
			wantDisk:         150,
			wantConfigurable: true,
			why:              "LAUNCH_DEFAULT_DISK_GB is threaded from the SAME config the engine resolves the default from",
		},
		{
			name:             "an unwired caller degrades to the adapter fallback, never to 0",
			providerName:     fixtureDisk,
			reportedStorage:  0,
			defaultDiskGB:    0,
			wantDisk:         provider.FallbackDiskGB,
			wantConfigurable: true,
			why:              "a struct-literal poller still publishes the number a launch would actually receive",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ToAvailableOffering(provider.GPUOffering{
				Provider:     tc.providerName,
				GPUType:      "h100_sxm",
				GPUCount:     1,
				PricePerHour: 2.50,
				Region:       "us-west",
				Tier:         provider.TierOnDemand,
				StorageGB:    tc.reportedStorage,
			}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, tc.defaultDiskGB, 0)

			if got.InstanceDiskGB != tc.wantDisk {
				t.Errorf("InstanceDiskGB = %d, want %d — %s", got.InstanceDiskGB, tc.wantDisk, tc.why)
			}
			if got.DiskConfigurable != tc.wantConfigurable {
				t.Errorf("DiskConfigurable = %v, want %v (provider %q)", got.DiskConfigurable, tc.wantConfigurable, tc.providerName)
			}
		})
	}
}

// TestInstanceDiskGB_NeverInheritsTheStorageFill is the regression that keeps
// the promise honest as the display fill is removed.
//
// StorageGB carries a 40 GB fabrication for datacenter rows whose upstream
// reported nothing. instance_disk_gb must read the RAW reported value, so it
// publishes 0 (unknown) where storage_gb still publishes 40. This test is
// written to survive that fill's deletion unchanged: it asserts only what
// instance_disk_gb does, never what storage_gb does.
func TestInstanceDiskGB_NeverInheritsTheStorageFill(t *testing.T) {
	got := ToAvailableOffering(provider.GPUOffering{
		Provider:     fixtureVM,
		GPUType:      "h100_sxm",
		GPUCount:     1,
		PricePerHour: 2.50,
		Region:       "us-west",
		Tier:         provider.TierOnDemand,
		// StorageGB deliberately unset — the upstream reported no disk.
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 100, 0)

	if got.InstanceDiskGB != 0 {
		t.Errorf("InstanceDiskGB = %d, want 0: an unreported disk is UNKNOWN. %d would mean the launch promise "+
			"inherited a display fill — the same fabrication the 40 GB default was.", got.InstanceDiskGB, got.InstanceDiskGB)
	}
}

// TestWithDefaultInstanceDisk covers the option's guard: a positive value is
// applied, a non-positive one leaves the zero value for instanceDiskGB to
// degrade (config.Load already rejects non-positive, so this only protects
// hand-built pollers).
func TestWithDefaultInstanceDisk(t *testing.T) {
	p := &Poller{}
	WithDefaultInstanceDisk(150)(p)
	if p.defaultInstanceDiskGB != 150 {
		t.Errorf("defaultInstanceDiskGB = %d, want 150", p.defaultInstanceDiskGB)
	}
	for _, bad := range []int{0, -1} {
		WithDefaultInstanceDisk(bad)(p)
		if p.defaultInstanceDiskGB != 150 {
			t.Errorf("WithDefaultInstanceDisk(%d) overwrote a good value with %d; non-positive must be ignored",
				bad, p.defaultInstanceDiskGB)
		}
	}
}

// TestToAvailableOffering_DiskPricePerGBHour pins the published RETAIL disk
// rate: the provider's wholesale rate scaled by the SAME effective markup
// branch RetailPrice's clamp picks (max of markup and the min-retail floor),
// so the UI's "+$/hr" deltas track the billed uplift. And it pins the two
// zero cases — non-configurable capacity and metering switched off — because
// a nonzero rate on either would advertise a price for disk that either
// cannot be bought or will not be billed.
func TestToAvailableOffering_DiskPricePerGBHour(t *testing.T) {
	const wholesaleRate = 0.00014

	base := provider.GPUOffering{
		GPUType:      "h100_sxm",
		GPUCount:     1,
		PricePerHour: 2.50,
		Region:       "us-west",
		Tier:         provider.TierOnDemand,
	}

	tests := []struct {
		name         string
		providerName string
		rate         float64
		markupPct    float64
		minMarkupPct float64
		want         float64
	}{
		{
			name:         "markup above the floor scales by the markup",
			providerName: fixtureDisk,
			rate:         wholesaleRate,
			markupPct:    20,
			minMarkupPct: 5,
			want:         wholesaleRate * 1.20,
		},
		{
			name:         "zero markup under the floor scales by the floor (passthrough config)",
			providerName: fixtureDisk,
			rate:         wholesaleRate,
			markupPct:    0,
			minMarkupPct: 5,
			want:         wholesaleRate * 1.05,
		},
		{
			name:         "negative markup never scales disk below cost",
			providerName: fixtureDisk,
			rate:         wholesaleRate,
			markupPct:    -10,
			minMarkupPct: 0,
			want:         wholesaleRate,
		},
		{
			name:         "non-configurable capacity publishes no rate",
			providerName: fixtureVM,
			rate:         wholesaleRate,
			markupPct:    20,
			minMarkupPct: 5,
			want:         0,
		},
		{
			name:         "metering off publishes no rate",
			providerName: fixtureDisk,
			rate:         0,
			markupPct:    20,
			minMarkupPct: 5,
			want:         0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := base
			o.Provider = tc.providerName
			got := ToAvailableOffering(o, tc.markupPct, tc.minMarkupPct, 0, 100, tc.rate)
			if got.DiskPricePerGBHour != tc.want {
				t.Errorf("DiskPricePerGBHour = %v, want %v", got.DiskPricePerGBHour, tc.want)
			}
		})
	}
}

// TestWithDiskPriceFor covers the option wiring and the nil degradation: an
// unwired poller publishes no disk rate — indistinguishable from metering
// switched off — never a fabricated one.
func TestWithDiskPriceFor(t *testing.T) {
	p := &Poller{}
	if p.diskPriceFor != nil {
		t.Fatal("zero-value poller must have no disk rate resolver")
	}
	WithDiskPriceFor(func(name string) float64 { return 0.00014 })(p)
	if p.diskPriceFor == nil || p.diskPriceFor(fixtureDisk) != 0.00014 {
		t.Error("WithDiskPriceFor did not install the resolver")
	}
}
