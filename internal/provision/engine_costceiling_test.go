package provision

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// passthroughCfg mirrors the shipped prod pricing policy: 0% markup, 0% floor,
// no disk metering — so retail == wholesale rounded to the cent, and any
// upstream drift at all comes straight out of margin.
func passthroughCfg() *config.Config {
	return &config.Config{
		LaunchDefaultDiskGB:          100,
		LaunchIncludedDiskGB:         100,
		PricingDiskPerGBHour:         0,
		PricingMarkupPct:             0,
		PricingMinRetailMarkupPct:    0,
		PricingCostDriftTolerancePct: 5.0,
		PricingCostDriftToleranceAbs: 0.02,
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestCostCeilingFor pins the allowance arithmetic. The whole design turns on
// taking the SMALLER of the two bounds: a percentage alone lets an expensive
// node eat a large absolute number silently, which is precisely the loss this
// guard exists to stop.
func TestCostCeilingFor(t *testing.T) {
	e := &Engine{config: passthroughCfg()}

	t.Run("cheap node is bounded by the percentage", func(t *testing.T) {
		// 5% of 0.16 = 0.008, which is under the $0.02 absolute.
		if got := e.costCeilingFor(context.Background(), 0.16, "marketco", 100, nil, false); !approx(got, 0.168) {
			t.Errorf("ceiling = %g, want 0.168", got)
		}
	})

	t.Run("expensive node is bounded by the absolute", func(t *testing.T) {
		// 5% of 8.35 would be $0.42/hr of silent margin loss. The absolute
		// cap is what keeps a big node from quietly eating a big number.
		if got := e.costCeilingFor(context.Background(), 8.35, "marketco", 100, nil, false); !approx(got, 8.37) {
			t.Errorf("ceiling = %g, want 8.37 (8.35 + 0.02)", got)
		}
	})

	t.Run("the real prod drift is refused", func(t *testing.T) {
		// gpu-def366db: quoted 0.16, upstream repriced to 0.3348 (+109%).
		ceiling := e.costCeilingFor(context.Background(), 0.16, "marketco", 100, nil, false)
		if 0.3348 <= ceiling {
			t.Errorf("ceiling %g must refuse the observed 0.3348 accept", ceiling)
		}
	})

	t.Run("non-positive wholesale disables the ceiling", func(t *testing.T) {
		// Reserved tier is flat-fee and off-platform; 0 means "no ceiling" to
		// the adapter, which is the pre-existing behavior.
		if got := e.costCeilingFor(context.Background(), 0, "marketco", 100, nil, false); got != 0 {
			t.Errorf("ceiling = %g, want 0", got)
		}
	})

	t.Run("a confirmed re-quote raises the ceiling", func(t *testing.T) {
		// Without this the customer could never accept the new price: the
		// re-confirm would re-derive the same ceiling off the same stale row
		// and 409 forever.
		approved := 0.34
		got := e.costCeilingFor(context.Background(), 0.16, "marketco", 100, &approved, false)
		if got < 0.3348 {
			t.Errorf("ceiling = %g, must fund the 0.3348 the customer confirmed", got)
		}
	})

	t.Run("a lower explicit cap never widens the ceiling", func(t *testing.T) {
		// An explicit cap BELOW the tolerance is a tighter customer intent;
		// the approved-retail term may only ever raise, never lower — the
		// retail cap check in the walk is what enforces the tighter side.
		low := 0.05
		got := e.costCeilingFor(context.Background(), 0.16, "marketco", 100, &low, false)
		if !approx(got, 0.168) {
			t.Errorf("ceiling = %g, want the 0.168 tolerance ceiling", got)
		}
	})

	t.Run("zero tolerance is exact-or-re-quote", func(t *testing.T) {
		strict := passthroughCfg()
		strict.PricingCostDriftToleranceAbs = 0
		se := &Engine{config: strict}
		if got := se.costCeilingFor(context.Background(), 0.16, "marketco", 100, nil, false); !approx(got, 0.16) {
			t.Errorf("ceiling = %g, want bare wholesale 0.16", got)
		}
	})
}

// TestWholesaleFundedBy pins the retail→wholesale inversion. It must never
// over-fund: over-funding sanctions the below-cost accept the ceiling exists
// to prevent, while under-funding only re-quotes a marginal launch.
func TestWholesaleFundedBy(t *testing.T) {
	// Every expectation below carries halfCentSlack, deliberately. Inverting
	// exactly is what creates the infinite-409 loop TestWholesaleFundedByRoundTrip
	// covers, so the slack is the behavior under test, not tolerated error —
	// these assert it lands where it should rather than merely "close enough".
	t.Run("passthrough inverts to itself plus the slack", func(t *testing.T) {
		e := &Engine{config: passthroughCfg()}
		if got := e.wholesaleFundedBy(context.Background(), 0.34, "marketco", 100); !approx(got, 0.34+halfCentSlack) {
			t.Errorf("funded = %g, want %g", got, 0.34+halfCentSlack)
		}
	})

	t.Run("divides by the larger of markup and floor", func(t *testing.T) {
		cfg := passthroughCfg()
		cfg.PricingMarkupPct = 10
		cfg.PricingMinRetailMarkupPct = 25 // floor dominates
		e := &Engine{config: cfg}
		want := (1.25 + halfCentSlack) / 1.25
		if got := e.wholesaleFundedBy(context.Background(), 1.25, "marketco", 100); !approx(got, want) {
			t.Errorf("funded = %g, want %g (divided by the 25%% floor, not the 10%% markup)", got, want)
		}
		// The mistake that matters: dividing by the SMALLER markup over-funds,
		// which sanctions buying above what the approved retail can pay for.
		if got := e.wholesaleFundedBy(context.Background(), 1.25, "marketco", 100); got > 1.25/1.10 {
			t.Errorf("funded = %g, over-funded past the 10%% markup inversion", got)
		}
	})

	t.Run("metered disk is removed before funding GPU cost", func(t *testing.T) {
		cfg := passthroughCfg()
		cfg.PricingDiskPerGBHour = 0.001
		e := &Engine{config: cfg}
		// 50 GB over the 100 GB allowance × $0.001 = $0.05 of the retail rate
		// is disk, and cannot be spent on the GPU.
		want := 1.00 + halfCentSlack - 0.05
		if got := e.wholesaleFundedBy(context.Background(), 1.00, "marketco", 150); !approx(got, want) {
			t.Errorf("funded = %g, want %g", got, want)
		}
	})

	t.Run("retail that cannot fund any GPU cost returns zero", func(t *testing.T) {
		cfg := passthroughCfg()
		cfg.PricingDiskPerGBHour = 0.001
		e := &Engine{config: cfg}
		if got := e.wholesaleFundedBy(context.Background(), 0.01, "marketco", 500); got != 0 {
			t.Errorf("funded = %g, want 0", got)
		}
	})
}

// TestWalkTranslatesCostCeilingToRequote is the seam that makes the guard
// user-visible. The adapter refuses in WHOLESALE terms; the customer must be
// re-quoted in RETAIL terms through the SAME PriceExceededError the catalog-side
// cap already produces, because that is what the price-changed 409 and the
// launch modal's re-confirm are built on. A ceiling that failed as a generic
// provision error would read to the customer as "no capacity" and silently kill
// a launch that is one confirmation away from succeeding.
func TestWalkTranslatesCostCeilingToRequote(t *testing.T) {
	offering := provider.GPUOffering{
		Provider:     "stub",
		GPUType:      provider.GPUTypeRTX3090,
		GPUCount:     1,
		PricePerHour: 0.16, // what the customer was quoted
		Tier:         provider.TierOnDemand,
		Region:       "eu-west",
	}

	t.Run("re-quotes at the real price", func(t *testing.T) {
		stub := &stubProvider{
			name: "stub",
			// The adapter found nothing placeable under the ceiling; the real
			// market price is the prod-observed 0.3348.
			provisionErr: &provider.CostCeilingError{
				CheapestCostPerHour: 0.3348,
				CeilingCostPerHour:  0.168,
			},
		}
		e := &Engine{config: passthroughCfg(), logger: slog.Default()}
		provReq := &provider.ProvisionRequest{InstanceID: "gpu-rq", GPUType: offering.GPUType, GPUCount: 1, Tier: offering.Tier}

		_, _, _, err := e.walkProvisionCandidates(
			context.Background(), provReq,
			[]ProviderCandidate{{Prov: stub, Offering: offering}},
			provisionWalkParams{instanceID: "gpu-rq", effectiveDisk: 100},
		)
		if !errors.Is(err, ErrPriceExceeded) {
			t.Fatalf("must surface as a price change, got %v", err)
		}
		var priceErr *PriceExceededError
		if !errors.As(err, &priceErr) {
			t.Fatalf("must carry the re-quote detail, got %T", err)
		}
		// Retail terms, not wholesale — this number is rendered to the customer.
		if !approx(priceErr.CurrentBestPerHour, 0.34) {
			t.Errorf("re-quote price = %g, want 0.34 (retail of 0.3348)", priceErr.CurrentBestPerHour)
		}
		if !approx(priceErr.MaxPricePerHour, 0.16) {
			t.Errorf("approved price = %g, want the 0.16 quote", priceErr.MaxPricePerHour)
		}
	})

	t.Run("the ceiling reaches the adapter", func(t *testing.T) {
		stub := &stubProvider{
			name:            "stub",
			provisionResult: &provider.ProvisionResult{UpstreamID: "up-1", Provider: "stub"},
		}
		e := &Engine{config: passthroughCfg(), logger: slog.Default()}
		provReq := &provider.ProvisionRequest{InstanceID: "gpu-ceil", GPUType: offering.GPUType, GPUCount: 1, Tier: offering.Tier}

		if _, _, _, err := e.walkProvisionCandidates(
			context.Background(), provReq,
			[]ProviderCandidate{{Prov: stub, Offering: offering}},
			provisionWalkParams{instanceID: "gpu-ceil", effectiveDisk: 100},
		); err != nil {
			t.Fatalf("walk: %v", err)
		}
		if len(stub.provisionReqs) != 1 {
			t.Fatalf("want 1 Provision call, got %d", len(stub.provisionReqs))
		}
		// Without this the adapter has no ceiling to enforce and the entire
		// guard is inert — the exact shape of the original bug.
		if got := stub.provisionReqs[0].MaxCostPerHour; !approx(got, 0.168) {
			t.Errorf("MaxCostPerHour = %g, want 0.168", got)
		}
	})

	t.Run("a fallback candidate above the viewed card price re-quotes", func(t *testing.T) {
		// The unpinned-fallback leg of GPU-78. The cheapest candidate ghosted;
		// the next one costs more than the card the caller displayed. Placing it
		// silently is the same upcharge the pinned pre-flight refuses, just one
		// attempt later, so the walk must break with the SAME re-quote error.
		refuser := &stubProvider{name: "refuser", provisionErr: provider.ErrNoCapacity}
		pricier := &stubProvider{
			name:            "pricier",
			provisionResult: &provider.ProvisionResult{UpstreamID: "up-2", Provider: "pricier"},
		}
		pricierOffering := offering
		pricierOffering.Provider, pricierOffering.PricePerHour = "pricier", 0.50

		e := &Engine{config: passthroughCfg(), logger: slog.Default()}
		provReq := &provider.ProvisionRequest{InstanceID: "gpu-vw", GPUType: offering.GPUType, GPUCount: 1, Tier: offering.Tier}
		viewed := 0.16

		_, _, _, err := e.walkProvisionCandidates(
			context.Background(), provReq,
			[]ProviderCandidate{{Prov: refuser, Offering: offering}, {Prov: pricier, Offering: pricierOffering}},
			provisionWalkParams{instanceID: "gpu-vw", effectiveDisk: 100, viewedPricePerHour: &viewed},
		)
		var priceErr *PriceExceededError
		if !errors.As(err, &priceErr) {
			t.Fatalf("must re-quote against the viewed price, got %v", err)
		}
		if !approx(priceErr.CurrentBestPerHour, 0.50) || !approx(priceErr.MaxPricePerHour, 0.16) {
			t.Errorf("re-quote = %+v, want current 0.50 against the 0.16 card", priceErr)
		}
		if len(pricier.provisionReqs) != 0 {
			t.Errorf("must not buy above the price the customer saw, got %d calls", len(pricier.provisionReqs))
		}
	})

	t.Run("a fallback within a cent of the viewed price still places", func(t *testing.T) {
		// Same cent-rounding slack as the pre-flight: the modal's card price and
		// the backend's ceil-to-cent retail can legitimately differ by up to 1c,
		// and that must not kill an otherwise fine fallback.
		refuser := &stubProvider{name: "refuser", provisionErr: provider.ErrNoCapacity}
		next := &stubProvider{
			name:            "next",
			provisionResult: &provider.ProvisionResult{UpstreamID: "up-2", Provider: "next"},
		}
		nextOffering := offering
		nextOffering.Provider, nextOffering.PricePerHour = "next", 0.17

		e := &Engine{config: passthroughCfg(), logger: slog.Default()}
		provReq := &provider.ProvisionRequest{InstanceID: "gpu-vw1c", GPUType: offering.GPUType, GPUCount: 1, Tier: offering.Tier}
		viewed := 0.16

		if _, _, _, err := e.walkProvisionCandidates(
			context.Background(), provReq,
			[]ProviderCandidate{{Prov: refuser, Offering: offering}, {Prov: next, Offering: nextOffering}},
			provisionWalkParams{instanceID: "gpu-vw1c", effectiveDisk: 100, viewedPricePerHour: &viewed},
		); err != nil {
			t.Fatalf("a one-cent divergence must not refuse the launch, got %v", err)
		}
		if len(next.provisionReqs) != 1 {
			t.Errorf("want the fallback placed, got %d calls", len(next.provisionReqs))
		}
	})

	t.Run("a cheaper candidate after the refusing one is still tried", func(t *testing.T) {
		// Placement memory demotes a held offering behind fresh ones, so a
		// candidate under the refusing one's ceiling can come after it. It
		// can place without a re-quote, so the walk goes on to it, skipping
		// the pricier candidate in between; when it refuses too, the walk
		// ends on the re-quote it would have stopped with. The refusal that
		// did not end the walk is not ledgered as the terminal cost_ceiling.
		for _, tc := range []struct {
			name         string
			cheapErr     error
			wantPlaced   bool
			wantCurrent  float64
			wantOutcomes string
		}{
			{name: "places", wantPlaced: true, wantOutcomes: "provider_rejected,placed"},
			{name: "refuses", cheapErr: provider.ErrNoCapacity, wantCurrent: 0.34,
				wantOutcomes: "provider_rejected,provider_rejected,cost_ceiling"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				refuser := &stubProvider{
					name:         "refuser",
					provisionErr: &provider.CostCeilingError{CheapestCostPerHour: 0.3348, CeilingCostPerHour: 0.168},
				}
				pricier := &stubProvider{
					name:            "pricier",
					provisionResult: &provider.ProvisionResult{UpstreamID: "up-2", Provider: "pricier"},
				}
				cheap := &stubProvider{
					name:            "cheap",
					provisionErr:    tc.cheapErr,
					provisionResult: &provider.ProvisionResult{UpstreamID: "up-3", Provider: "cheap"},
				}
				pricierOffering, cheapOffering := offering, offering
				pricierOffering.Provider, pricierOffering.PricePerHour = "pricier", 0.50
				cheapOffering.Provider, cheapOffering.PricePerHour = "cheap", 0.12

				fake := fakeWithOneKey()
				e := &Engine{config: passthroughCfg(), logger: slog.Default(), db: fake}
				provReq := &provider.ProvisionRequest{InstanceID: "gpu-dem", GPUType: offering.GPUType, GPUCount: 1, Tier: offering.Tier}
				seq := 0

				prov, _, _, err := e.walkProvisionCandidates(
					context.Background(), provReq,
					[]ProviderCandidate{
						{Prov: refuser, Offering: offering},
						{Prov: pricier, Offering: pricierOffering},
						{Prov: cheap, Offering: cheapOffering},
					},
					provisionWalkParams{instanceID: "gpu-dem", effectiveDisk: 100, ledger: attemptLedgerCtx{seq: &seq}},
				)
				if got := strings.Join(outcomesOf(fake.recordedAttempts), ","); got != tc.wantOutcomes {
					t.Errorf("ledger outcomes = %s, want %s", got, tc.wantOutcomes)
				}
				if len(cheap.provisionReqs) != 1 || len(pricier.provisionReqs) != 0 {
					t.Errorf("creates: cheap %d pricier %d; want the cheaper candidate tried and the pricier one skipped",
						len(cheap.provisionReqs), len(pricier.provisionReqs))
				}
				if tc.wantPlaced {
					if err != nil || prov != cheap {
						t.Fatalf("walk = (%v, %v); want placed on the cheaper candidate", prov, err)
					}
					return
				}
				var priceErr *PriceExceededError
				if !errors.As(err, &priceErr) || !approx(priceErr.CurrentBestPerHour, tc.wantCurrent) {
					t.Errorf("err = %v; want the refusing candidate's re-quote at %g", err, tc.wantCurrent)
				}
			})
		}
	})

	t.Run("a walk does not stampede into pricier candidates", func(t *testing.T) {
		// Candidates are sorted ascending, so once one prices out, walking on
		// can only buy something MORE expensive. The re-quote must terminate
		// the walk rather than fall through to the next offering.
		refuser := &stubProvider{
			name:         "refuser",
			provisionErr: &provider.CostCeilingError{CheapestCostPerHour: 0.3348, CeilingCostPerHour: 0.168},
		}
		pricier := &stubProvider{
			name:            "pricier",
			provisionResult: &provider.ProvisionResult{UpstreamID: "up-2", Provider: "pricier"},
		}
		pricierOffering := offering
		pricierOffering.Provider, pricierOffering.PricePerHour = "pricier", 0.50

		e := &Engine{config: passthroughCfg(), logger: slog.Default()}
		provReq := &provider.ProvisionRequest{InstanceID: "gpu-stmp", GPUType: offering.GPUType, GPUCount: 1, Tier: offering.Tier}

		_, _, _, err := e.walkProvisionCandidates(
			context.Background(), provReq,
			[]ProviderCandidate{{Prov: refuser, Offering: offering}, {Prov: pricier, Offering: pricierOffering}},
			provisionWalkParams{instanceID: "gpu-stmp", effectiveDisk: 100},
		)
		if !errors.Is(err, ErrPriceExceeded) {
			t.Fatalf("must re-quote, got %v", err)
		}
		if len(pricier.provisionReqs) != 0 {
			t.Errorf("must not launch the pricier candidate behind the customer's back, got %d calls",
				len(pricier.provisionReqs))
		}
	})
}

// TestBilledRetail is the half of the guard that decides what the customer is
// CHARGED, and it is the half a ceiling alone silently gets wrong.
//
// Refusing the accept only helps if declining is the customer's alternative.
// When they instead confirm the new price, the confirmed number has to reach
// the invoice — otherwise the launch buys at the new cost, bills at the stale
// row, and loses exactly what it lost before the ceiling existed, now with a
// consent dialog in front of it.
func TestBilledRetail(t *testing.T) {
	e := &Engine{config: passthroughCfg()}

	t.Run("a confirmed re-quote is what gets billed", func(t *testing.T) {
		// The prod case: quoted 0.16, upstream repriced to 0.3348, customer
		// confirmed 0.34. Billing 0.16 here would reproduce the whole bug.
		confirmed := 0.34
		got := e.billedRetail(context.Background(), 0.16, 0.3348, "marketco", 100, &confirmed)
		if !approx(got, 0.34) {
			t.Errorf("billed = %g, want 0.34 — the price the customer confirmed", got)
		}
		if got <= 0.3348 {
			t.Errorf("billed %g is at or below the %g we paid — the loss survived the fix", got, 0.3348)
		}
	})

	t.Run("never bills above the approved cap", func(t *testing.T) {
		// Upstream moved again between the confirm and the accept. The ceiling
		// bounds what we buy; this bounds what we charge for it.
		confirmed := 0.34
		if got := e.billedRetail(context.Background(), 0.16, 0.60, "marketco", 100, &confirmed); !approx(got, 0.34) {
			t.Errorf("billed = %g, want the 0.34 cap", got)
		}
	})

	t.Run("silent drift is eaten, not passed on", func(t *testing.T) {
		// No cap sent => no customer approval => the allowance is ours to eat.
		// Charging more than the number they clicked would be the worse bug.
		if got := e.billedRetail(context.Background(), 0.16, 0.168, "marketco", 100, nil); !approx(got, 0.16) {
			t.Errorf("billed = %g, want the quoted 0.16", got)
		}
	})

	t.Run("a cheaper accept lowers the bill to retail of what we paid", func(t *testing.T) {
		// GPU-78. Marketplace prices drift DOWN more often than up (90 of 102 launches), and
		// under a 0%-passthrough policy every one of those launches was billing a
		// margin the pricing page says does not exist. The bill follows the buy.
		if got := e.billedRetail(context.Background(), 0.16, 0.12, "marketco", 100, nil); !approx(got, 0.12) {
			t.Errorf("billed = %g, want retail of the 0.12 we actually paid", got)
		}
	})

	t.Run("a cheaper accept lowers the bill even under a consent cap", func(t *testing.T) {
		// Was pinned the other way before GPU-78 ("a cheaper accept keeps the
		// quoted price"). A consent cap is permission to be charged UP TO a
		// number when the buy comes in dear; it is not a floor that keeps the
		// stale quote alive when the buy comes in cheap.
		cap := 0.34
		if got := e.billedRetail(context.Background(), 0.16, 0.12, "marketco", 100, &cap); !approx(got, 0.12) {
			t.Errorf("billed = %g, want retail of the 0.12 we actually paid", got)
		}
	})

	t.Run("an unreported cost keeps the quoted price", func(t *testing.T) {
		// Some adapters report CostPerHour=0 because they echo no per-instance charge, and the
		// reserved tier quotes 0 by construction. Reading either as "we paid
		// nothing" would zero the bill — the min() discipline is gated on a
		// POSITIVE actual cost for exactly this reason.
		if got := e.billedRetail(context.Background(), 0.16, 0, "metalco", 100, nil); !approx(got, 0.16) {
			t.Errorf("billed = %g, want the quoted 0.16 — an unreported cost is not a free instance", got)
		}
	})

	t.Run("the GPU-78 incident shape", func(t *testing.T) {
		// Prod 2026-08-23: card showed 0.34, the launch-time re-quote was
		// 0.4037 (retail ceils to 0.41), and the ask we actually bought cost
		// 0.337. Billing 0.41 on a 0.337 buy is a ~22% hidden margin.
		got := e.billedRetail(context.Background(), 0.4037, 0.337, "marketco", 100, nil)
		if !approx(got, 0.34) {
			t.Errorf("billed = %g, want 0.34 — retail of the ask we bought", got)
		}
	})

	t.Run("an explicit cap alone never raises the price", func(t *testing.T) {
		// A cap is a CEILING the customer set, not an offer to pay it. With no
		// drift there is nothing to re-price.
		cap := 5.00
		if got := e.billedRetail(context.Background(), 0.16, 0.16, "marketco", 100, &cap); !approx(got, 0.16) {
			t.Errorf("billed = %g, want 0.16 — a generous cap is not a price", got)
		}
	})

	t.Run("a generous cap cannot be bought against and billed low", func(t *testing.T) {
		// `gpu launch --max-price 5` on a $0.16 row licenses the adapter to buy
		// up to $5/hr. Whatever it buys must also be what we charge for.
		cap := 5.00
		got := e.billedRetail(context.Background(), 0.16, 4.80, "marketco", 100, &cap)
		if got < 4.80 {
			t.Errorf("billed %g below the %g paid — a wide cap must not become a subsidy", got, 4.80)
		}
	})
}

// TestWholesaleFundedByRoundTrip guards the re-confirm against an infinite 409
// loop. The 409 quotes retail(cost); the re-confirm inverts that retail back to
// a wholesale ceiling. If the inversion returns LESS than the cost that
// produced it, the same launch is refused at the price the customer was just
// asked to accept — and there is no price at which they can ever succeed.
func TestWholesaleFundedByRoundTrip(t *testing.T) {
	markups := []struct {
		name        string
		markup      float64
		floor       float64
		diskPerGBHr float64
		disk        int
	}{
		{"prod passthrough", 0, 0, 0, 100},
		{"the 5% marketco markup that was considered", 5, 0, 0, 100},
		{"markup above floor", 10, 5, 0, 100},
		{"floor above markup", 5, 25, 0, 100},
		{"metered disk", 5, 0, 0.001, 150},
	}
	costs := []float64{0.0137, 0.16, 0.3348, 0.44, 1.0, 8.35, 23.997}

	for _, m := range markups {
		t.Run(m.name, func(t *testing.T) {
			cfg := passthroughCfg()
			cfg.PricingMarkupPct = m.markup
			cfg.PricingMinRetailMarkupPct = m.floor
			cfg.PricingDiskPerGBHour = m.diskPerGBHr
			e := &Engine{config: cfg}
			for _, cost := range costs {
				quoted := e.retailWithDisk(context.Background(), cost, "marketco", m.disk) // the 409's price
				funded := e.wholesaleFundedBy(context.Background(), quoted, "marketco", m.disk)
				if funded < cost {
					t.Errorf("cost %g quoted at %g funds only %g — the customer confirms and is refused again, forever",
						cost, quoted, funded)
				}
			}
		})
	}
}

// TestCostCeilingForRescue — a boot-stall rescue spends against its own
// allowance. It excludes the stalled host, which is routinely the very machine
// whose cheap ask set the row's price, so the replacement is pricier by
// construction rather than by drift. Under the quote allowance nearly every
// community rescue prices out and the customer gets a dead instance instead of
// a working one.
func TestCostCeilingForRescue(t *testing.T) {
	cfg := passthroughCfg()
	cfg.PricingRescueCostAllowancePct = 150.0
	cfg.PricingRescueCostAllowanceAbs = 0.25
	e := &Engine{config: cfg}

	t.Run("rescues a cheap community card past the worst observed drift", func(t *testing.T) {
		// The quote path refuses 0.3348 on a 0.16 row (that is its job). The
		// rescue path must be able to save it.
		quote := e.costCeilingFor(context.Background(), 0.16, "marketco", 100, nil, false)
		rescue := e.costCeilingFor(context.Background(), 0.16, "marketco", 100, nil, true)
		if 0.3348 <= quote {
			t.Errorf("quote ceiling %g should refuse 0.3348", quote)
		}
		if rescue < 0.3348 {
			t.Errorf("rescue ceiling %g cannot save the +109%% case — the instance just dies", rescue)
		}
	})

	t.Run("an expensive node is still bounded by the absolute", func(t *testing.T) {
		// 150% of an $8.35 node would be $12.53/hr of subsidy. Generous where a
		// rescue is cheap, bounded where it is not.
		if got := e.costCeilingFor(context.Background(), 8.35, "marketco", 100, nil, true); !approx(got, 8.60) {
			t.Errorf("rescue ceiling = %g, want 8.60 (8.35 + 0.25)", got)
		}
	})

	t.Run("zero allowance means no subsidy", func(t *testing.T) {
		strict := passthroughCfg()
		strict.PricingRescueCostAllowancePct = 0
		strict.PricingRescueCostAllowanceAbs = 0
		se := &Engine{config: strict}
		if got := se.costCeilingFor(context.Background(), 0.16, "marketco", 100, nil, true); !approx(got, 0.16) {
			t.Errorf("rescue ceiling = %g, want bare cost 0.16", got)
		}
	})

	t.Run("the rescue walk spends against the rescue allowance", func(t *testing.T) {
		// The flag has to reach the adapter, or the wiring is decorative.
		stub := &stubProvider{
			name:            "stub",
			provisionResult: &provider.ProvisionResult{UpstreamID: "up-r", Provider: "stub"},
		}
		offering := provider.GPUOffering{
			Provider: "stub", GPUType: provider.GPUTypeRTX3090, GPUCount: 1,
			PricePerHour: 0.16, Tier: provider.TierOnDemand, Region: "eu-west",
		}
		provReq := &provider.ProvisionRequest{InstanceID: "gpu-resc", GPUType: offering.GPUType, GPUCount: 1, Tier: offering.Tier}
		we := &Engine{config: cfg, logger: slog.Default()}
		if _, _, _, err := we.walkProvisionCandidates(
			context.Background(), provReq,
			[]ProviderCandidate{{Prov: stub, Offering: offering}},
			provisionWalkParams{instanceID: "gpu-resc", effectiveDisk: 100, rescue: true},
		); err != nil {
			t.Fatalf("walk: %v", err)
		}
		if got := stub.provisionReqs[0].MaxCostPerHour; !approx(got, 0.40) {
			t.Errorf("MaxCostPerHour = %g, want the 0.40 rescue ceiling", got)
		}
	})
}

// TestWholesaleFundedByDiscountBasis — the funded ceiling must come back on the
// LIST basis, because that is what the adapter compares it against. Retail is
// derived from partner-discounted TRUE cost (v85), so on a discounted supplier
// an un-grossed inversion refuses hosts the confirmed retail genuinely funds:
// the customer confirms the exact price the 409 quoted and is refused again,
// forever. Same latent failure #641 closed on the walk's base ceiling, now
// pinned on the funded branch — and one Suppliers-panel click away from firing.
func TestWholesaleFundedByDiscountBasis(t *testing.T) {
	cfg := passthroughCfg()
	e := &Engine{config: cfg, flags: fakeFlags{discount: map[string]float64{"marketco": 10}}}
	ctx := context.Background()

	for _, listCost := range []float64{0.16, 0.3348, 1.0, 8.35} {
		// What the 409 would quote for a host at this LIST price…
		quoted := e.retailWithDisk(ctx, listCost, "marketco", 100)
		// …must, when confirmed, fund buying that same host back.
		funded := e.wholesaleFundedBy(ctx, quoted, "marketco", 100)
		if funded < listCost {
			t.Errorf("list %g quoted at %g funds only %g — confirming the quoted price is refused, forever",
				listCost, quoted, funded)
		}
	}
}

// TestProvision_ViewedPricePin covers the second half of GPU-78: the card price
// the customer actually looked at reaches the engine, and a launch-time re-quote
// above it refuses instead of quietly billing the higher number.
//
// The pin alone was never enough. SelectedOfferingID fixes WHERE we place but
// carries no price (price is deliberately not in the offering-id digest), so a
// pinned launch skipped the price-changed protection on the card -> quote leg
// entirely — which is how prod billed $0.41 against a $0.34 card.
//
// The fixture's offering quotes $1.50 wholesale and the empty config applies no
// markup and no disk metering, so retail is $1.50 on the nose.
func TestProvision_ViewedPricePin(t *testing.T) {
	// f64 is a local address-taker: every case here turns on the difference
	// between "absent" and "set to a number", which is what the *float64 is for.
	f64 := func(v float64) *float64 { return &v }

	t.Run("a re-quote above the card price refuses the launch", func(t *testing.T) {
		stub := onDemandStub("up-viewed", "us-east")
		fake := fakeWithOneKey()
		e := newProvisionUnitEngine(stub, fake)

		req := onDemandRequest()
		req.SelectedOfferingID = pinFor(stub.offerings[0], "stub")
		req.ViewedPricePerHour = f64(1.48) // two cents under the live $1.50 re-quote

		resp, err := e.Provision(context.Background(), req)
		var priceErr *PriceExceededError
		if !errors.As(err, &priceErr) {
			t.Fatalf("want a re-quote error, got resp=%+v err=%v", resp, err)
		}
		if !errors.Is(err, ErrPriceExceeded) {
			t.Error("must satisfy errors.Is(ErrPriceExceeded) — that is what maps it to price_changed rather than a retryable failure")
		}
		if !approx(priceErr.CurrentBestPerHour, 1.50) || !approx(priceErr.MaxPricePerHour, 1.48) {
			t.Errorf("re-quote = %+v, want the new 1.50 against the 1.48 card", priceErr)
		}
		// No buy behind the 409: the customer is being asked, not billed.
		if len(stub.provisionReqs) != 0 {
			t.Errorf("nothing may be provisioned before the customer re-confirms, got %d calls", len(stub.provisionReqs))
		}
		if len(fake.createdInstances) != 0 {
			t.Errorf("no instance row may be persisted, got %d", len(fake.createdInstances))
		}
	})

	// Every one of these must reproduce today's behavior exactly. The field is
	// opt-in: API and CLI callers who never saw a card must never meet a 409
	// they cannot explain.
	proceeds := []struct {
		name   string
		viewed *float64
	}{
		{"no viewed price at all is today's behavior", nil},
		{"a card price equal to the re-quote", f64(1.50)},
		{"a card price one cent under the re-quote", f64(1.49)},
		{"a zero viewed price reads as absent", f64(0)},
		{"a negative viewed price reads as absent", f64(-1)},
		{"a viewed price above the re-quote is simply satisfied", f64(9.99)},
	}
	for _, tc := range proceeds {
		t.Run(tc.name, func(t *testing.T) {
			stub := onDemandStub("up-ok", "us-east")
			fake := fakeWithOneKey()
			e := newProvisionUnitEngine(stub, fake)

			req := onDemandRequest()
			req.SelectedOfferingID = pinFor(stub.offerings[0], "stub")
			req.ViewedPricePerHour = tc.viewed

			if _, err := e.Provision(context.Background(), req); err != nil {
				t.Fatalf("launch must proceed, got %v", err)
			}
			if len(fake.createdInstances) != 1 {
				t.Fatalf("want the instance persisted, got %d", len(fake.createdInstances))
			}
		})
	}

	// T-l6l-01. ViewedPricePerHour is UNTRUSTED client input landing in
	// billing-adjacent code, and the whole safety argument for accepting it is
	// that it can only ever ABORT a launch. If it leaked into the consent cap
	// (MaxPricePerHour doubles as #640's approved cap) then "I saw this price"
	// would silently become "I consent to be charged this much" — a client could
	// widen its own cost ceiling by claiming to have seen a big number.
	t.Run("a high viewed price widens neither the cost ceiling nor the bill", func(t *testing.T) {
		stub := onDemandStub("up-tamper", "us-east") // quotes 1.50, accept costs 1.00
		fake := fakeWithOneKey()
		e := newProvisionUnitEngine(stub, fake)

		req := onDemandRequest()
		req.SelectedOfferingID = pinFor(stub.offerings[0], "stub")
		req.ViewedPricePerHour = f64(100.00) // "I saw $100/hr" — must buy nothing extra

		if _, err := e.Provision(context.Background(), req); err != nil {
			t.Fatalf("Provision: %v", err)
		}
		if len(stub.provisionReqs) != 1 {
			t.Fatalf("want 1 provision attempt, got %d", len(stub.provisionReqs))
		}
		// The ceiling is the quote plus the (zero, in this config) drift
		// allowance. If viewed had reached costCeilingFor's approvedRetail this
		// would be ~100 and the adapter would be licensed to buy a $100/hr host.
		if got := stub.provisionReqs[0].MaxCostPerHour; !approx(got, 1.50) {
			t.Errorf("MaxCostPerHour = %g, want 1.50 — a claimed card price must not raise our authority to BUY", got)
		}
		// And it must not reach the invoice either: the bill is still
		// min(quoted, actual) = retail of the 1.00 we paid.
		if got := fake.createdInstances[0].PricePerHour; !approx(got, 1.00) {
			t.Errorf("billed = %g, want 1.00 — a claimed card price must not raise the bill", got)
		}
	})
}
