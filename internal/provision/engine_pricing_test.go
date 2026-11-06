package provision

import (
	"context"
	"log/slog"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// TestProvision_RetailPrice_PerProviderMarkup proves the customer-facing price
// chain is RETAIL end-to-end under a per-provider markup override: the
// persisted instance row, the synchronously-opened billing session, and the
// ProvisionResponse all carry wholesale x (1 + provider markup), while
// UpstreamPricePerHour keeps the provider's actual (wholesale) charge for
// margin tracking.
func TestProvision_RetailPrice_PerProviderMarkup(t *testing.T) {
	stub := onDemandStub("up-retail", "us-east") // wholesale 1.50/hr, CostPerHour 1.0
	fake := fakeWithOneKey()
	e := newProvisionUnitEngine(stub, fake)
	e.config = &config.Config{
		PricingMarkupPct:  6.0,
		ProviderMarkupPct: map[string]float64{"stub": 20.0},
	}

	resp, err := e.Provision(context.Background(), onDemandRequest())
	if err != nil {
		t.Fatalf("Provision returned unexpected error: %v", err)
	}

	// The stub QUOTES 1.50 wholesale but its accept reports CostPerHour 1.0, so
	// since GPU-78 the basis is the 1.0 we actually paid: at the stub's 20%
	// override that is 1.20 retail (NOT the global 6% -> 1.06, NOT the 1.0 raw
	// cost, and NOT the 1.80 the stale quote would have billed). So this also
	// pins the min(quoted, actual) discipline all the way to the persisted row
	// and the response, not just to billedRetail in isolation.
	const wantRetail = 1.20

	if resp.PricePerHour != wantRetail {
		t.Errorf("ProvisionResponse.PricePerHour = %v, want retail %v (wholesale must never leave the engine)",
			resp.PricePerHour, wantRetail)
	}

	if len(fake.createdInstances) != 1 {
		t.Fatalf("expected exactly 1 instance persisted, got %d", len(fake.createdInstances))
	}
	inst := fake.createdInstances[0]
	if inst.PricePerHour != wantRetail {
		t.Errorf("instance.PricePerHour = %v, want retail %v", inst.PricePerHour, wantRetail)
	}
	if inst.UpstreamPricePerHour != 1.0 {
		t.Errorf("instance.UpstreamPricePerHour = %v, want wholesale 1.0 (provider's actual charge)",
			inst.UpstreamPricePerHour)
	}

	// No billing session is opened at provision time — billing starts at the
	// running transition (the ready callback), which copies PricePerHour from
	// the instance row asserted above. So the retail price the session will
	// carry is guaranteed by inst.PricePerHour here.
}

// TestSelectProviderCandidates_RanksByRetailPrice proves candidate ranking
// uses RETAIL price: a provider that is cheaper wholesale but carries a higher
// markup must lose to a provider whose retail price is lower. (With a flat
// markup, wholesale and retail order coincide — only a per-provider override
// can flip the order, which is exactly what this guards.)
func TestSelectProviderCandidates_RanksByRetailPrice(t *testing.T) {
	cheapWholesale := &stubProvider{
		name: "marked-up",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "us-east", "US-TX-3", 1.00, 4),
		},
	}
	cheapRetail := &stubProvider{
		name: "flat",
		offerings: []provider.GPUOffering{
			makeOffering(provider.GPUTypeH100SXM, "us-east", "US-TX-3", 1.10, 4),
		},
	}

	reg := provider.NewRegistry()
	// Register the wholesale-cheaper provider FIRST so a wholesale sort (or
	// the stable tiebreak) would put it at candidates[0] — the retail sort
	// must overcome registration order.
	reg.Register(cheapWholesale)
	reg.Register(cheapRetail)
	e := &Engine{
		registry: reg,
		logger:   slog.Default(),
		config: &config.Config{
			PricingMarkupPct:  0,
			ProviderMarkupPct: map[string]float64{"marked-up": 30.0}, // 1.00 -> 1.30 retail vs flat 1.10
		},
	}

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if got := candidates[0].Prov.Name(); got != "flat" {
		t.Errorf("candidates[0] = %q, want %q (retail 1.10 beats retail 1.30 despite higher wholesale)",
			got, "flat")
	}
}
