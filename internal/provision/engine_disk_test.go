package provision

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// diskOffering builds an offering attributed to a named provider, so
// provider.ConfigurableDisk (which is name-keyed) resolves the way it would in
// production. makeOffering hardcodes "stub", which is never disk-configurable.
func diskOffering(providerName string, gt provider.GPUType, region string, price float64) provider.GPUOffering {
	return provider.GPUOffering{
		Provider:           providerName,
		GPUType:            gt,
		GPUCount:           1,
		VRAMPerGPUGB:       80,
		PricePerHour:       price,
		Tier:               provider.TierOnDemand,
		Region:             region,
		DatacenterLocation: "US-TX-3",
		StockStatus:        "High",
		AvailableCount:     4,
	}
}

// TestSelectProviderCandidates_NoDiskRequestDoesNotNarrow is THE regression
// test for this feature. The disk default is filled on every launch, but it
// must never remove a provider from candidacy — only an EXPLICIT customer
// disk_gb may narrow.
//
// If this fails, every fixed-disk provider (vmco, cloudvmco, metalco and
// friends) has silently dropped out of placement for every launch on the
// platform, including all the ones that never mentioned disk. That is a far
// worse outcome than the 40 GB ceiling this feature exists to fix.
func TestSelectProviderCandidates_NoDiskRequestDoesNotNarrow(t *testing.T) {
	// vmco: VM flavor fixes the root volume — NOT disk-configurable, and
	// also the cheapest, so any narrowing changes both membership and ordering.
	hyper := &stubProvider{
		name:      "vmco",
		offerings: []provider.GPUOffering{diskOffering("vmco", provider.GPUTypeH100SXM, "us-east", 2.40)},
	}
	rp := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{diskOffering("podco", provider.GPUTypeH100SXM, "us-west", 2.80)},
	}
	e := newTestEngineWithProviders(hyper, rp)

	req := ProvisionRequest{
		GPUType:  provider.GPUTypeH100SXM,
		GPUCount: 1,
		Tier:     provider.TierOnDemand,
		// RequestedDiskGB deliberately nil — the overwhelming majority of real
		// launches, and every launch that predates this feature.
	}
	candidates, err := e.SelectProviderCandidates(context.Background(), req)
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want 2 — an unset disk_gb must not narrow placement", len(candidates))
	}
	// Ordering is part of "unchanged": candidates[0] is what gets provisioned.
	if got := candidates[0].Prov.Name(); got != "vmco" {
		t.Errorf("cheapest candidate = %q, want \"vmco\" — price order must be untouched", got)
	}
	if got := candidates[1].Prov.Name(); got != "podco" {
		t.Errorf("second candidate = %q, want \"podco\"", got)
	}
}

// TestSelectProviderCandidates_ExplicitDiskNarrows is the other half: once the
// customer actually asks, we must either honor the request or refuse it. A
// provider that cannot configure disk would boot a silently different size
// than the one quoted and billed.
func TestSelectProviderCandidates_ExplicitDiskNarrows(t *testing.T) {
	hyper := &stubProvider{
		name:      "vmco",
		offerings: []provider.GPUOffering{diskOffering("vmco", provider.GPUTypeH100SXM, "us-east", 2.40)},
	}
	rp := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{diskOffering("podco", provider.GPUTypeH100SXM, "us-west", 2.80)},
	}
	e := newTestEngineWithProviders(hyper, rp)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:         provider.GPUTypeH100SXM,
		GPUCount:        1,
		Tier:            provider.TierOnDemand,
		RequestedDiskGB: diskPtr(200),
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1 — only disk-configurable providers may serve an explicit request", len(candidates))
	}
	if got := candidates[0].Prov.Name(); got != "podco" {
		t.Errorf("surviving candidate = %q, want \"podco\"", got)
	}
	// Note this also means we deliberately passed over the CHEAPER vmco
	// offering. Honoring the request beats the lower price.
}

// TestSelectProviderCandidates_ExplicitDiskNoCapableProvider pins the honest
// failure: when nothing can serve the request we return a typed
// ErrDiskUnavailable, never ErrNoProvider (which reads as "out of capacity" and
// sends the customer chasing the wrong problem) and never a silent placement at
// the provider's own fixed size.
func TestSelectProviderCandidates_ExplicitDiskNoCapableProvider(t *testing.T) {
	hyper := &stubProvider{
		name:      "vmco",
		offerings: []provider.GPUOffering{diskOffering("vmco", provider.GPUTypeH100SXM, "us-east", 2.40)},
	}
	e := newTestEngineWithProviders(hyper)

	_, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:         provider.GPUTypeH100SXM,
		GPUCount:        1,
		Tier:            provider.TierOnDemand,
		RequestedDiskGB: diskPtr(200),
	})
	if !errors.Is(err, ErrDiskUnavailable) {
		t.Fatalf("err = %v, want ErrDiskUnavailable", err)
	}
}

// TestErrDiskUnavailableNamesNoProvider enforces the T-63-04 defense-by-omission
// contract its neighbours carry: customer-facing provisioning errors must not
// leak which upstream we resell.
func TestErrDiskUnavailableNamesNoProvider(t *testing.T) {
	msg := strings.ToLower(ErrDiskUnavailable.Error())
	for _, name := range []string{"podco", "marketco", "vmco", "cloudvmco", "metalco", "fixedimageco"} {
		if strings.Contains(msg, name) {
			t.Errorf("ErrDiskUnavailable names provider %q: %q", name, msg)
		}
	}
}

func diskPtr(v int) *int { return &v }

// diskCapOffering is diskOffering plus the storage the adapter reports for the
// host (Offering.StorageGB). Attributed to marketco, which models a marketplace adapter that
// stamps live per-fragment disk space and whose upstream
// silently CLAMPS an oversize disk request instead of rejecting it (the
// 2026-08-11 prod battery: --disk 150 accepted and billed, 134G delivered).
func diskCapOffering(providerName string, storageGB int, region string, price float64) provider.GPUOffering {
	o := diskOffering(providerName, provider.GPUTypeRTX3080, region, price)
	o.StorageGB = storageGB
	return o
}

// pinFor computes the deterministic offering id EXACTLY the way the engine's
// pin filter does (availability.OfferingID over the Normalize'd region), so
// these tests pin the same token the catalog would have handed a client.
func pinFor(o provider.GPUOffering, providerName string) string {
	return availability.OfferingID(o, availability.Normalize(o.Region, o.DatacenterLocation, providerName))
}

// TestSelectProviderCandidates_DiskExceedsPinnedOfferingCap is THE regression
// test for the marketplace accepted-but-clamped defect (2026-08-11 prod battery): a
// PINNED launch whose explicit disk_gb exceeds the offering's reported storage
// was accepted, billed at the requested size, and delivered the host cap. The
// `--disk` contract promises an unhonorable request FAILS rather than
// launching with a different size — so the pinned offering must be refused
// with the disk-specific typed error, never accepted and never misreported as
// "offering drained" (ErrOfferingUnavailable), which reads as transient and
// invites a retry that can only fail the same way.
func TestSelectProviderCandidates_DiskExceedsPinnedOfferingCap(t *testing.T) {
	off := diskCapOffering("marketco", 144, "us-east", 0.07)
	v := &stubProvider{name: "marketco", offerings: []provider.GPUOffering{off}}
	e := newTestEngineWithProviders(v)

	base := ProvisionRequest{
		GPUType:            provider.GPUTypeRTX3080,
		GPUCount:           1,
		Tier:               provider.TierOnDemand,
		SelectedOfferingID: pinFor(off, "marketco"),
	}

	over := base
	over.RequestedDiskGB = diskPtr(200)
	candidates, err := e.SelectProviderCandidates(context.Background(), over)
	if !errors.Is(err, ErrDiskExceedsOffering) {
		t.Fatalf("disk 200 pinned to a 144GB offering: err = %v, want ErrDiskExceedsOffering", err)
	}
	if len(candidates) != 0 {
		t.Errorf("got %d candidates, want 0 — an unhonorable pinned request must never launch", len(candidates))
	}

	// Within the cap the same pin launches: the check is a cap, not a lockout.
	within := base
	within.RequestedDiskGB = diskPtr(120)
	candidates, err = e.SelectProviderCandidates(context.Background(), within)
	if err != nil {
		t.Fatalf("disk 120 pinned to a 144GB offering must be honorable: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
}

// TestSelectProviderCandidates_DiskCapDoesNotNarrowUnpinned pins the deliberate
// SCOPE of the cap: pinned launches only. A marketplace offering row is a
// GROUP whose StorageGB is one representative host's figure — sibling hosts in
// the same group can carry far more disk, so rejecting an unpinned request on
// the representative's figure would refuse capacity that can honor it. The
// honor boundary for unpinned placement is the adapter's MinDiskGB predicate
// against live per-host data.
func TestSelectProviderCandidates_DiskCapDoesNotNarrowUnpinned(t *testing.T) {
	small := diskCapOffering("marketco", 144, "us-east", 0.07) // rep host too small; siblings may not be
	big := diskCapOffering("marketco", 500, "us-west", 0.20)
	v := &stubProvider{name: "marketco", offerings: []provider.GPUOffering{small, big}}
	e := newTestEngineWithProviders(v)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:         provider.GPUTypeRTX3080,
		GPUCount:        1,
		Tier:            provider.TierOnDemand,
		RequestedDiskGB: diskPtr(200),
	})
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want 2 — unpinned placement must not reject on the "+
			"representative host's storage figure; the adapter's live-host predicate governs", len(candidates))
	}
	// Cheapest-first ordering is untouched; the adapter will walk past the
	// representative host if it genuinely cannot fit the request.
	if candidates[0].Offering.StorageGB != 144 {
		t.Errorf("cheapest candidate should still sort first, got StorageGB=%d", candidates[0].Offering.StorageGB)
	}
}

// TestSelectProviderCandidates_UnknownStorageFailsOpen pins the fail-open rule:
// StorageGB == 0 means the adapter reports no storage figure (podco here),
// and an unreported cap must never reject a request — the pre-existing
// behavior for that capacity is preserved bit-for-bit, and the adapter/upstream
// remains the honor boundary there.
func TestSelectProviderCandidates_UnknownStorageFailsOpen(t *testing.T) {
	off := diskOffering("podco", provider.GPUTypeRTX3080, "us-east", 0.30) // StorageGB zero-valued
	rp := &stubProvider{name: "podco", offerings: []provider.GPUOffering{off}}
	e := newTestEngineWithProviders(rp)

	candidates, err := e.SelectProviderCandidates(context.Background(), ProvisionRequest{
		GPUType:            provider.GPUTypeRTX3080,
		GPUCount:           1,
		Tier:               provider.TierOnDemand,
		SelectedOfferingID: pinFor(off, "podco"),
		RequestedDiskGB:    diskPtr(200),
	})
	if err != nil {
		t.Fatalf("unreported storage must not reject an explicit disk request: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
}

// TestErrDiskExceedsOfferingNamesNoProvider — the same T-63-04
// defense-by-omission contract every customer-facing provisioning error
// carries.
func TestErrDiskExceedsOfferingNamesNoProvider(t *testing.T) {
	msg := strings.ToLower(ErrDiskExceedsOffering.Error())
	for _, name := range []string{"podco", "marketco", "vmco", "cloudvmco", "metalco", "fixedimageco"} {
		if strings.Contains(msg, name) {
			t.Errorf("ErrDiskExceedsOffering names provider %q: %q", name, msg)
		}
	}
}

// TestEffectiveDiskGB pins the one function that answers "how much disk?" for
// narrowing, pricing, the adapter payload and the persisted row. The cases that
// matter are the precedence rules: the server default fills an absent request,
// an explicit request wins over the default, and a template's ContainerDiskGB
// acts as a FLOOR rather than a ceiling.
func TestEffectiveDiskGB(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *config.Config
		requested *int
		template  int
		want      int
		why       string
	}{
		{
			name: "no request, no template, no config",
			want: 100,
			why:  "nil config must resolve to the real default, never a 0 GB filesystem",
		},
		{
			name: "no request uses the configured default",
			cfg:  &config.Config{LaunchDefaultDiskGB: 150},
			want: 150,
		},
		{
			name:      "explicit request wins over the default",
			cfg:       &config.Config{LaunchDefaultDiskGB: 100},
			requested: diskPtr(200),
			want:      200,
		},
		{
			name:      "explicit request below the default is honored",
			cfg:       &config.Config{LaunchDefaultDiskGB: 100},
			requested: diskPtr(40),
			want:      40,
			why:       "the default is a default, not a floor — only the template floors",
		},
		{
			name:     "template raises the default",
			cfg:      &config.Config{LaunchDefaultDiskGB: 100},
			template: 250,
			want:     250,
			why:      "a template declares the disk its image needs to boot; landing under it ENOSPCs",
		},
		{
			name:     "template at or below the default does not lower it",
			cfg:      &config.Config{LaunchDefaultDiskGB: 100},
			template: 40,
			want:     100,
			why:      "template is a floor, never a ceiling",
		},
		{
			name:      "request above the template wins",
			cfg:       &config.Config{LaunchDefaultDiskGB: 100},
			requested: diskPtr(300),
			template:  100,
			want:      300,
			why:       "a customer asking for more room than the template is always safe to honor",
		},
		{
			name:      "template above the request floors it",
			cfg:       &config.Config{LaunchDefaultDiskGB: 100},
			requested: diskPtr(20),
			template:  100,
			want:      100,
			why:       "no template deploy may land under-spec, even if the customer asks",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{config: tt.cfg}
			got := e.effectiveDiskGB(ProvisionRequest{
				RequestedDiskGB: tt.requested,
				ContainerDiskGB: tt.template,
			})
			if got != tt.want {
				t.Errorf("effectiveDiskGB() = %d, want %d — %s", got, tt.want, tt.why)
			}
		})
	}
}

// TestEffectiveDiskGB_FinetuneFloorPreserved pins the fine-tune path
// specifically. internal/finetune/orchestrator.go sets ContainerDiskGB: 100
// explicitly; that predates LAUNCH_DEFAULT_DISK_GB and is currently equal to
// it, so it reads like dead code someone could delete. It is not: because the
// template value is a floor, a future operator lowering LAUNCH_DEFAULT_DISK_GB
// cannot silently shrink a training run's disk out from under its weights and
// checkpoints.
func TestEffectiveDiskGB_FinetuneFloorPreserved(t *testing.T) {
	e := &Engine{config: &config.Config{LaunchDefaultDiskGB: 40}}
	got := e.effectiveDiskGB(ProvisionRequest{ContainerDiskGB: 100})
	if got != 100 {
		t.Errorf("effectiveDiskGB() = %d, want 100; the fine-tune orchestrator's explicit "+
			"ContainerDiskGB must survive a lower platform default", got)
	}
}

// TestRetailWithDisk pins the single launch-price formula that the price cap,
// the per-fallback guard, the billed rate and the candidate sort all share.
// The billing invariant — the rate shown at rent time equals the rate billed —
// holds only because those four sites compute this one number.
func TestRetailWithDisk(t *testing.T) {
	metering := &config.Config{
		LaunchDefaultDiskGB:       100,
		LaunchIncludedDiskGB:      100,
		PricingDiskPerGBHour:      0.000137,
		PricingMarkupPct:          20,
		PricingMinRetailMarkupPct: 5,
	}
	inert := &config.Config{
		LaunchDefaultDiskGB:       100,
		LaunchIncludedDiskGB:      100,
		PricingDiskPerGBHour:      0, // shipped default
		PricingMarkupPct:          20,
		PricingMinRetailMarkupPct: 5,
	}

	t.Run("shipped config never charges for disk", func(t *testing.T) {
		e := &Engine{config: inert}
		base := e.retailWithDisk(context.Background(), 2.50, "podco", 100)
		for _, disk := range []int{0, 100, 250, 1000} {
			if got := e.retailWithDisk(context.Background(), 2.50, "podco", disk); got != base {
				t.Errorf("disk=%d quoted %v, want %v — DISK_PRICE_PER_GB_HOUR=0 must meter nothing", disk, got, base)
			}
		}
	})

	t.Run("disk at or under the allowance costs nothing extra", func(t *testing.T) {
		e := &Engine{config: metering}
		base := e.retailWithDisk(context.Background(), 2.50, "podco", 0)
		for _, disk := range []int{40, 100} {
			if got := e.retailWithDisk(context.Background(), 2.50, "podco", disk); got != base {
				t.Errorf("disk=%d quoted %v, want %v — a default launch must cost nothing new", disk, got, base)
			}
		}
	})

	t.Run("disk beyond the allowance raises the quote", func(t *testing.T) {
		e := &Engine{config: metering}
		base := e.retailWithDisk(context.Background(), 2.50, "podco", 100)
		big := e.retailWithDisk(context.Background(), 2.50, "podco", 1000)
		if big <= base {
			t.Errorf("900 GB over the allowance quoted %v, not above the %v baseline", big, base)
		}
	})

	t.Run("per-provider rate override wins over the global rate", func(t *testing.T) {
		perProvider := &config.Config{
			LaunchDefaultDiskGB:  100,
			LaunchIncludedDiskGB: 100,
			PricingDiskPerGBHour: 0.00014, // global fallback
			ProviderDiskPricePerGBHour: map[string]float64{
				"marketco": 0.00028, // marketplace median — ~2× the flat-rate provider
			},
			PricingMarkupPct:          20,
			PricingMinRetailMarkupPct: 5,
		}
		e := &Engine{config: perProvider}
		fallback := e.retailWithDisk(context.Background(), 2.50, "podco", 250)
		override := e.retailWithDisk(context.Background(), 2.50, "marketco", 250)
		if override <= fallback {
			t.Errorf("marketco (override 0.00028) quoted %v, podco (global 0.00014) quoted %v — the per-provider rate must win", override, fallback)
		}
		// The fallback provider must price exactly as if only the global knob
		// existed — an override for one provider must not leak into another.
		globalOnly := &config.Config{
			LaunchDefaultDiskGB:       100,
			LaunchIncludedDiskGB:      100,
			PricingDiskPerGBHour:      0.00014,
			PricingMarkupPct:          20,
			PricingMinRetailMarkupPct: 5,
		}
		eg := &Engine{config: globalOnly}
		if want := eg.retailWithDisk(context.Background(), 2.50, "podco", 250); fallback != want {
			t.Errorf("podco with an unrelated marketco override quoted %v, want %v", fallback, want)
		}
	})

	t.Run("reserved-tier zero rate stays zero", func(t *testing.T) {
		// Reserved slots carry PricePerHour 0 (flat-fee, billed off-platform).
		// A disk term must not accidentally turn that into a billable rate.
		e := &Engine{config: inert}
		if got := e.retailWithDisk(context.Background(), 0, "podco", 250); got != 0 {
			t.Errorf("reserved-tier rate = %v, want 0", got)
		}
	})
}

// fakeFlags is a flagsChecker standing in for *admin.Store: it answers from
// fixed maps instead of the DB, modelling "a provider_markup_overrides row
// exists" (present in markup) vs "no row, fall back to env" (absent).
type fakeFlags struct {
	disabled  map[string]bool
	markup    map[string]float64
	discount  map[string]float64
	minMargin map[string]float64
	// envFallback is what the Store would return when no DB row exists — the
	// real Store folds config.MarkupPctFor in itself, so the fake must too.
	envFallback func(name string) float64
}

func (f fakeFlags) IsProviderDisabled(_ context.Context, name string) bool {
	return f.disabled[strings.ToLower(name)]
}

func (f fakeFlags) CostDiscountPctFor(_ context.Context, name string) float64 {
	return f.discount[strings.ToLower(name)]
}

func (f fakeFlags) MinMarginPctFor(_ context.Context, name string) float64 {
	return f.minMargin[strings.ToLower(name)]
}

func (f fakeFlags) MarkupPctFor(_ context.Context, name string) float64 {
	if pct, ok := f.markup[strings.ToLower(name)]; ok {
		return pct
	}
	if f.envFallback != nil {
		return f.envFallback(name)
	}
	return 0
}

// TestRetailWithDiskResolvesMarkupThroughFlags pins the divergence bug behind the
// passthrough-pricing incident: an admin markup edit re-priced the CATALOG (the
// availability poller resolves markup DB-over-env through admin.Store) while the
// BILL kept using the env markup forever, because retailWithDisk read
// config.MarkupPctFor directly. The rate shown at rent time and the rate written
// to BillingSession.PricePerHour must move in lockstep, so both must come from the
// one resolver.
func TestRetailWithDiskResolvesMarkupThroughFlags(t *testing.T) {
	cfg := &config.Config{
		LaunchDefaultDiskGB:       100,
		LaunchIncludedDiskGB:      100,
		PricingDiskPerGBHour:      0,
		PricingMarkupPct:          0, // passthrough policy: zero upcharge sitewide
		PricingMinRetailMarkupPct: 0,
	}
	ctx := context.Background()

	t.Run("db override moves the billed rate", func(t *testing.T) {
		envOnly := &Engine{config: cfg}
		withDB := &Engine{config: cfg, flags: fakeFlags{
			markup:      map[string]float64{"podco": 25},
			envFallback: cfg.MarkupPctFor,
		}}

		base := envOnly.retailWithDisk(ctx, 2.00, "podco", 100)
		overridden := withDB.retailWithDisk(ctx, 2.00, "podco", 100)
		if overridden <= base {
			t.Fatalf("a +25%% markup override quoted %v, not above the %v env-only rate — "+
				"the bill is still reading env-only markup, which is the bug that let the "+
				"catalog and the billed rate diverge", overridden, base)
		}
		if want := 2.50; overridden != want {
			t.Errorf("2.00 wholesale at a +25%% db override = %v, want %v", overridden, want)
		}
	})

	t.Run("no override falls through to env", func(t *testing.T) {
		perProviderEnv := &config.Config{
			LaunchDefaultDiskGB:       100,
			LaunchIncludedDiskGB:      100,
			PricingMarkupPct:          0,
			ProviderMarkupPct:         map[string]float64{"vmco": -10},
			PricingMinRetailMarkupPct: 0,
		}
		// Only podco carries a DB row; vmco must still get its env -10%.
		e := &Engine{config: perProviderEnv, flags: fakeFlags{
			markup:      map[string]float64{"podco": 25},
			envFallback: perProviderEnv.MarkupPctFor,
		}}
		envOnly := &Engine{config: perProviderEnv}

		got := e.retailWithDisk(ctx, 2.00, "vmco", 100)
		want := envOnly.retailWithDisk(ctx, 2.00, "vmco", 100)
		if got != want {
			t.Errorf("vmco (no db row) quoted %v, want the env-resolved %v — "+
				"an override for one provider must not leak into another", got, want)
		}
	})

	t.Run("nil flags stays env-only", func(t *testing.T) {
		// Engines built without SetFlags (unit tests, any process that skips the
		// wiring) must behave exactly as before rather than silently pricing at 0%.
		perProviderEnv := &config.Config{
			LaunchIncludedDiskGB: 100,
			PricingMarkupPct:     20,
		}
		e := &Engine{config: perProviderEnv}
		if got, want := e.retailWithDisk(ctx, 2.00, "podco", 100), 2.40; got != want {
			t.Errorf("nil-flags engine quoted %v, want the env-only %v", got, want)
		}
	})
}

// mutableFlags is a flagsChecker whose markup can change between calls, and
// which counts resolutions — modelling the admin.Store's 30s TTL cache being
// refreshed by the availability poller (or an operator's panel edit) partway
// through a launch.
type mutableFlags struct {
	pct        float64
	discount   float64
	minMargin  float64
	calls      int
	discCalls  int
	floorCalls int
}

func (f *mutableFlags) IsProviderDisabled(context.Context, string) bool { return false }

func (f *mutableFlags) MarkupPctFor(context.Context, string) float64 {
	f.calls++
	return f.pct
}

func (f *mutableFlags) CostDiscountPctFor(context.Context, string) float64 {
	f.discCalls++
	return f.discount
}

func (f *mutableFlags) MinMarginPctFor(context.Context, string) float64 {
	f.floorCalls++
	return f.minMargin
}

// TestPricingSnapshotFreezesOneLaunch pins the invariant that the price CEILING
// checked before upstream placement and the rate PERSISTED after it are the same
// number.
//
// Provision checks req.MaxPricePerHour against retailWithDisk early, then
// computes BillingSession.PricePerHour from retailWithDisk again after the
// provider call returns — with no cap re-check in between, and no way to fail
// the request by then (the instance exists upstream). Before markup moved onto
// admin.Store both reads hit the same immutable env map. Through the Store they
// are two reads of a TTL cache that the poller refreshes underneath the request
// and that fails open to env on a transient DB error, so without the freeze an
// operator's mid-launch edit bills above the approved cap.
func TestPricingSnapshotFreezesOneLaunch(t *testing.T) {
	cfg := &config.Config{
		LaunchDefaultDiskGB:       100,
		LaunchIncludedDiskGB:      100,
		PricingMarkupPct:          0,
		PricingMinRetailMarkupPct: 0,
	}
	flags := &mutableFlags{pct: 0}
	e := &Engine{config: cfg, flags: flags}

	ctx := withPricingSnapshot(context.Background())

	// The ceiling check at the top of a launch.
	ceiling := e.retailWithDisk(ctx, 2.00, "podco", 100)
	if ceiling != 2.00 {
		t.Fatalf("ceiling = %v, want 2.00", ceiling)
	}

	// An operator raises the markup mid-launch (or the TTL expires and the
	// poller has repopulated the cache).
	flags.pct = 25

	// The rate persisted to the billing session, after upstream placement.
	billed := e.retailWithDisk(ctx, 2.00, "podco", 100)
	if billed != ceiling {
		t.Errorf("billed %v != ceiling %v — a markup change mid-launch moved the rate "+
			"the customer is charged above the one they approved", billed, ceiling)
	}
	if flags.calls != 1 {
		t.Errorf("resolved markup %d times in one launch, want exactly 1 — the freeze "+
			"is what makes the two reads agree", flags.calls)
	}
	if flags.discCalls != 1 {
		t.Errorf("resolved the partner discount %d times in one launch, want exactly 1", flags.discCalls)
	}

	// The NEXT launch picks the new markup up: the freeze is per-launch, not a
	// cache that outlives the request.
	next := e.retailWithDisk(withPricingSnapshot(context.Background()), 2.00, "podco", 100)
	if next != 2.50 {
		t.Errorf("next launch billed %v, want 2.50 — the freeze must not outlive its launch", next)
	}
}

// A ctx with no snapshot (non-launch readers) must resolve live every time.
func TestMarkupWithoutSnapshotResolvesLive(t *testing.T) {
	cfg := &config.Config{LaunchIncludedDiskGB: 100, PricingMarkupPct: 0, PricingMinRetailMarkupPct: 0}
	flags := &mutableFlags{pct: 0}
	e := &Engine{config: cfg, flags: flags}

	if got := e.retailWithDisk(context.Background(), 2.00, "podco", 100); got != 2.00 {
		t.Fatalf("first = %v, want 2.00", got)
	}
	flags.pct = 25
	if got := e.retailWithDisk(context.Background(), 2.00, "podco", 100); got != 2.50 {
		t.Errorf("second = %v, want 2.50 — no snapshot means resolve live", got)
	}
}

// TestPricingSnapshotFreezesTheDiscountToo is the discount half of the same
// invariant, and the more dangerous half.
//
// Since v85 retail is DERIVED from the partner discount. A discount set ONLY in
// the admin panel has no env value to fall back to, so when the Store's read
// fails open mid-launch it resolves to 0 — re-pricing the launch at full LIST.
// A launch quoted and capped at 0.9 x list would bill at list.
func TestPricingSnapshotFreezesTheDiscountToo(t *testing.T) {
	cfg := &config.Config{
		LaunchIncludedDiskGB:      100,
		PricingMarkupPct:          0,
		PricingMinRetailMarkupPct: 0,
	}
	// A panel-only partner deal: 10% off list, nothing in the env.
	flags := &mutableFlags{pct: 0, discount: 10}
	e := &Engine{config: cfg, flags: flags}

	ctx := withPricingSnapshot(context.Background())

	quoted := e.retailWithDisk(ctx, 1.188, "vmco", 100)
	if quoted != 1.07 {
		t.Fatalf("quoted = %v, want 1.07 (0.9 x 1.188, rounded up to the cent)", quoted)
	}

	// The Store's read fails open mid-launch — with no env discount, that means 0.
	flags.discount = 0

	billed := e.retailWithDisk(ctx, 1.188, "vmco", 100)
	if billed != quoted {
		t.Errorf("billed %v != quoted %v — a mid-launch discount read that fell back to "+
			"no-discount re-priced the launch at list, an %.0f%% overcharge above the "+
			"approved cap", billed, quoted, (billed/quoted-1)*100)
	}

	// And the freeze is still per-launch.
	next := e.retailWithDisk(withPricingSnapshot(context.Background()), 1.188, "vmco", 100)
	if next != 1.19 {
		t.Errorf("next launch = %v, want 1.19 (no discount) — the freeze must not outlive its launch", next)
	}
}

// TestListBasisCeiling pins the currency the adapter-side cost ceiling is
// expressed in.
//
// Adapters compare req.MaxCostPerHour against their provider's LIST prices
// (a marketplace adapter walks offers on list hourly cost). Retail is derived from TRUE cost
// since v85, so on a discounted supplier a raw retail ceiling is ~(1−d) too low
// and rejects hosts we could serve profitably — turning every launch on that
// supplier into "no capacity". Setting a partner discount is now one click in
// the Suppliers panel, so this is a click away rather than a redeploy away.
func TestListBasisCeiling(t *testing.T) {
	cfg := &config.Config{LaunchIncludedDiskGB: 100, PricingMarkupPct: 0, PricingMinRetailMarkupPct: 0}
	ctx := context.Background()

	t.Run("no discount is a no-op", func(t *testing.T) {
		// Every marketplace supplier today. The ceiling must be byte-identical
		// to the old behavior, or this fix would itself move behavior.
		e := &Engine{config: cfg, flags: fakeFlags{}}
		for _, retail := range []float64{0.42, 1.07, 2.50, 48.0} {
			if got := e.listBasisCeiling(ctx, retail, "marketco"); got != retail {
				t.Errorf("no-discount ceiling for %v = %v, want %v unchanged", retail, got, retail)
			}
		}
	})

	t.Run("a discounted supplier grosses back up to list", func(t *testing.T) {
		e := &Engine{config: cfg, flags: fakeFlags{discount: map[string]float64{"marketco": 10}}}
		// We charge $1.80. We are invoiced 90% of list, so we can afford a host
		// listed up to $2.00 — at which point true cost is exactly $1.80.
		got := e.listBasisCeiling(ctx, 1.80, "marketco")
		if math.Abs(got-2.00) > 1e-9 {
			t.Fatalf("ceiling = %v, want 2.00 (retail / (1 − 0.10))", got)
		}
		// The invariant that matters: a host priced AT the ceiling costs us
		// exactly retail — never more.
		if trueCost := got * 0.9; math.Abs(trueCost-1.80) > 1e-9 {
			t.Errorf("a host at the ceiling costs us %v, want exactly the %v retail", trueCost, 1.80)
		}
		// And the un-grossed ceiling would have rejected that host outright.
		if 2.00 <= 1.80 {
			t.Fatal("premise broken")
		}
	})

	t.Run("out-of-range discounts degrade to retail", func(t *testing.T) {
		// TrueCost ignores these too; the safe direction is the LOWER ceiling
		// (skip more hosts) rather than one divided by a nonsense denominator.
		for _, d := range []float64{-5, 100, 150} {
			e := &Engine{config: cfg, flags: fakeFlags{discount: map[string]float64{"marketco": d}}}
			if got := e.listBasisCeiling(ctx, 1.80, "marketco"); got != 1.80 {
				t.Errorf("discount %v: ceiling = %v, want the un-grossed 1.80", d, got)
			}
		}
	})
}
