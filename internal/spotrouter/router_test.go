package spotrouter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// flatMarkup returns a MarkupResolver applying pct to every provider.
func flatMarkup(pct float64) MarkupResolver {
	return func(string) float64 { return pct }
}

func TestEligible(t *testing.T) {
	plain := &fakeProvider{name: "ondemand-only"}
	if Eligible(plain) {
		t.Error("provider without SpotCapability must not be eligible")
	}
	off := &fakeSpotProvider{fakeProvider: fakeProvider{name: "spot-off"}, supportsSpot: false}
	if Eligible(off) {
		t.Error("SupportsSpot()==false must not be eligible")
	}
	on := &fakeSpotProvider{fakeProvider: fakeProvider{name: "spot-on"}, supportsSpot: true}
	if !Eligible(on) {
		t.Error("SpotCapability with SupportsSpot()==true must be eligible")
	}
}

// TestCandidatesCheapestFirst: three spot-capable providers at different
// wholesale prices — the router must rank by RETAIL price ascending.
func TestCandidatesCheapestFirst(t *testing.T) {
	cheap := &fakeSpotProvider{fakeProvider: fakeProvider{
		name:      "cheap",
		offerings: []provider.GPUOffering{spotOffering("cheap", "A100", 1, 1.00, "us-east")},
	}, supportsSpot: true}
	mid := &fakeSpotProvider{fakeProvider: fakeProvider{
		name:      "mid",
		offerings: []provider.GPUOffering{spotOffering("mid", "A100", 1, 1.50, "us-east")},
	}, supportsSpot: true}
	pricey := &fakeSpotProvider{fakeProvider: fakeProvider{
		name:      "pricey",
		offerings: []provider.GPUOffering{spotOffering("pricey", "A100", 1, 2.00, "us-east")},
	}, supportsSpot: true}

	r := New(staticSource{[]provider.Provider{pricey, cheap, mid}}, flatMarkup(10), nil, nil, testLogger())
	got, err := r.Candidates(context.Background(), SpotRequest{GPUType: "A100", GPUCount: 1})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 candidates, got %d", len(got))
	}
	wantOrder := []string{"cheap", "mid", "pricey"}
	for i, w := range wantOrder {
		if got[i].Prov.Name() != w {
			t.Errorf("candidate[%d] = %s, want %s", i, got[i].Prov.Name(), w)
		}
	}
	// Retail = wholesale * 1.10 rounded to cents.
	if got[0].RetailPricePerHour != 1.10 {
		t.Errorf("cheapest retail = %v, want 1.10", got[0].RetailPricePerHour)
	}
}

// TestCandidatesPerProviderMarkupCanReorder: the ranking uses the SAME
// per-provider markup the catalog uses, so a cheaper wholesale price with a
// heavy markup can lose to a pricier wholesale price with a light markup.
func TestCandidatesPerProviderMarkupCanReorder(t *testing.T) {
	a := &fakeSpotProvider{fakeProvider: fakeProvider{
		name:      "heavy-markup",
		offerings: []provider.GPUOffering{spotOffering("heavy-markup", "A100", 1, 1.00, "us-east")},
	}, supportsSpot: true}
	b := &fakeSpotProvider{fakeProvider: fakeProvider{
		name:      "light-markup",
		offerings: []provider.GPUOffering{spotOffering("light-markup", "A100", 1, 1.05, "us-east")},
	}, supportsSpot: true}
	markup := func(name string) float64 {
		if name == "heavy-markup" {
			return 20 // retail 1.20
		}
		return 5 // retail 1.10
	}
	r := New(staticSource{[]provider.Provider{a, b}}, markup, nil, nil, testLogger())
	got, err := r.Candidates(context.Background(), SpotRequest{GPUType: "A100", GPUCount: 1})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 2 || got[0].Prov.Name() != "light-markup" {
		t.Fatalf("expected light-markup retail (1.10) to beat heavy-markup retail (1.20); got order %+v", names(got))
	}
}

// TestCandidatesExcludesNonEligibleAndNonSpot: on-demand-only providers,
// SupportsSpot()==false providers, and non-spot offerings never appear.
func TestCandidatesExcludesNonEligibleAndNonSpot(t *testing.T) {
	onDemandOffering := spotOffering("mixed", "A100", 1, 0.10, "us-east")
	onDemandOffering.Tier = provider.TierOnDemand

	plain := &fakeProvider{name: "plain", offerings: []provider.GPUOffering{spotOffering("plain", "A100", 1, 0.01, "us-east")}}
	disabled := &fakeSpotProvider{fakeProvider: fakeProvider{
		name:      "disabled",
		offerings: []provider.GPUOffering{spotOffering("disabled", "A100", 1, 0.02, "us-east")},
	}, supportsSpot: false}
	mixed := &fakeSpotProvider{fakeProvider: fakeProvider{
		name:      "mixed",
		offerings: []provider.GPUOffering{onDemandOffering, spotOffering("mixed", "A100", 1, 3.00, "us-east")},
	}, supportsSpot: true}

	r := New(staticSource{[]provider.Provider{plain, disabled, mixed}}, flatMarkup(0), nil, nil, testLogger())
	got, err := r.Candidates(context.Background(), SpotRequest{GPUType: "A100", GPUCount: 1})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 || got[0].Prov.Name() != "mixed" || got[0].Offering.Tier != provider.TierSpot {
		t.Fatalf("want exactly the one spot offering from 'mixed', got %+v", names(got))
	}
}

// TestCandidatesFiltersFitAndRegion: GPU type/count, stock, and canonical
// region constraints are enforced.
func TestCandidatesFiltersFitAndRegion(t *testing.T) {
	wrongType := spotOffering("p", "H100", 1, 0.10, "us-east")
	tooFew := spotOffering("p", "A100", 1, 0.20, "us-east")
	noStock := spotOffering("p", "A100", 4, 0.30, "us-east")
	noStock.AvailableCount = 0
	wrongRegion := spotOffering("p", "A100", 4, 0.40, "eu-west")
	fits := spotOffering("p", "A100", 4, 0.50, "us-east")

	p := &fakeSpotProvider{fakeProvider: fakeProvider{
		name:      "p",
		offerings: []provider.GPUOffering{wrongType, tooFew, noStock, wrongRegion, fits},
	}, supportsSpot: true}

	r := New(staticSource{[]provider.Provider{p}}, flatMarkup(0), nil, nil, testLogger())
	got, err := r.Candidates(context.Background(), SpotRequest{
		GPUType: "A100", GPUCount: 4, Regions: []string{"us-east"},
	})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 || got[0].Offering.PricePerHour != 0.50 {
		t.Fatalf("want only the fitting us-east 4xA100 offering, got %+v", got)
	}
}

// TestCandidatesAutoSelectByVRAM: empty GPUType selects on the MinVRAMGB
// floor, mirroring the engine's auto-select path.
func TestCandidatesAutoSelectByVRAM(t *testing.T) {
	small := spotOffering("p", "RTX4090", 1, 0.10, "us-east")
	small.VRAMPerGPUGB = 24
	big := spotOffering("p", "A100", 1, 0.90, "us-east")
	big.VRAMPerGPUGB = 80

	p := &fakeSpotProvider{fakeProvider: fakeProvider{
		name:      "p",
		offerings: []provider.GPUOffering{small, big},
	}, supportsSpot: true}

	r := New(staticSource{[]provider.Provider{p}}, flatMarkup(0), nil, nil, testLogger())
	got, err := r.Candidates(context.Background(), SpotRequest{GPUCount: 1, MinVRAMGB: 40})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 || got[0].Offering.GPUType != "A100" {
		t.Fatalf("want only the 80GB offering, got %+v", names(got))
	}
}

// TestCandidatesSkipsFailingProvider: one flaky upstream must not blank the
// pool — its error is skipped and the healthy provider still ranks.
func TestCandidatesSkipsFailingProvider(t *testing.T) {
	broken := &fakeSpotProvider{fakeProvider: fakeProvider{
		name:    "broken",
		listErr: errors.New("upstream 500"),
	}, supportsSpot: true}
	healthy := &fakeSpotProvider{fakeProvider: fakeProvider{
		name:      "healthy",
		offerings: []provider.GPUOffering{spotOffering("healthy", "A100", 1, 1.00, "us-east")},
	}, supportsSpot: true}

	r := New(staticSource{[]provider.Provider{broken, healthy}}, flatMarkup(0), nil, nil, testLogger())
	got, err := r.Candidates(context.Background(), SpotRequest{GPUType: "A100", GPUCount: 1})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 || got[0].Prov.Name() != "healthy" {
		t.Fatalf("want the healthy provider only, got %+v", names(got))
	}
}

func names(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Prov.Name()
	}
	return out
}
