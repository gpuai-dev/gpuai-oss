package availability

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

type stubVerdicts map[string]bool

func (s stubVerdicts) Delisted(id string) bool { return s[id] }

// A fleet-probe delist hides exactly the probed offering from the customer
// catalog in enforce mode, keeps it in the staff twin, and changes nothing in
// shadow mode.
func TestPoller_FleetProbeVerdicts(t *testing.T) {
	dead := provider.GPUOffering{Provider: "alpha", GPUType: provider.GPUTypeL40S, GPUCount: 1, PricePerHour: 0.79,
		Tier: provider.TierOnDemand, Region: "asia-pacific", AvailableCount: 1}
	fine := provider.GPUOffering{Provider: "alpha", GPUType: provider.GPUTypeRTX4090, GPUCount: 1, PricePerHour: 0.34,
		Tier: provider.TierOnDemand, Region: "asia-pacific", AvailableCount: 4}
	registry := provider.NewRegistry()
	registry.Register(newFakeProvider("alpha", []provider.GPUOffering{dead, fine}))
	verdicts := stubVerdicts{OfferingID(dead, Normalize(dead.Region, dead.DatacenterLocation, dead.Provider)): true}

	for _, tc := range []struct {
		name     string
		enforce  bool
		customer int
	}{
		{"shadow keeps the row", false, 2},
		{"enforce hides the row", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Poller{registry: registry, interval: 30 * time.Second, probeTimeout: time.Second, logger: silentLogger()}
			WithOfferingVerdicts(verdicts, tc.enforce)(p)
			got, staff := p.collectOfferings(context.Background())
			if len(got) != tc.customer {
				t.Fatalf("customer catalog has %d rows; want %d", len(got), tc.customer)
			}
			if len(staff) != 2 {
				t.Errorf("staff twin has %d rows; it must keep what the customer view hides", len(staff))
			}
			if tc.enforce {
				for _, o := range got {
					if o.GPUModel == string(provider.GPUTypeL40S) {
						t.Errorf("delisted offering still sold: %+v", o)
					}
				}
			}
		})
	}
}

// The supported wiring (serve.go): verdicts passed to NewPoller as an option,
// before Start. Catalog cycles running concurrently then only read them, so
// -race stays quiet; applying the option to a started poller (the old wiring)
// raced on p.verdicts with every cycle.
func TestPoller_FleetProbeVerdictsWiredAtConstruction(t *testing.T) {
	o := provider.GPUOffering{Provider: "alpha", GPUType: provider.GPUTypeRTX4090, GPUCount: 1, PricePerHour: 0.34,
		Tier: provider.TierOnDemand, Region: "asia-pacific", AvailableCount: 4}
	registry := provider.NewRegistry()
	registry.Register(newFakeProvider("alpha", []provider.GPUOffering{o}))
	delisted := stubVerdicts{OfferingID(o, Normalize(o.Region, o.DatacenterLocation, o.Provider)): true}
	p := NewPoller(registry, nil, 30*time.Second, nil, silentLogger(), WithOfferingVerdicts(delisted, false))
	p.probeTimeout = time.Second

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if got, _ := p.collectOfferings(context.Background()); len(got) != 1 {
					t.Errorf("shadow mode hid a row: %d", len(got))
					return
				}
			}
		}()
	}
	wg.Wait()
}
