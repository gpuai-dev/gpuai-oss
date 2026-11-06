package provision

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// spotStub is a stubProvider that additionally implements the frozen
// provider.SpotCapability contract, so the engine's tier=spot eligibility
// gate (spotrouter.Eligible) admits it. supportsSpot=false models an adapter
// that implements the interface but has spot switched off.
type spotStub struct {
	stubProvider
	supportsSpot bool
}

func (s *spotStub) SupportsSpot() bool { return s.supportsSpot }

func (s *spotStub) PreemptionState(ctx context.Context, upstreamID, region string) (provider.PreemptionStatus, error) {
	return provider.PreemptionNone, nil
}

// spotEngineOffering mirrors makeOffering but at Tier == spot.
func spotEngineOffering(providerName string, price float64) provider.GPUOffering {
	o := makeOffering(provider.GPUTypeA10080GB, "us-east", "US-TX-3", price, 3)
	o.Provider = providerName
	o.Tier = provider.TierSpot
	return o
}

// newSpotStub returns a spot-capable provider with one spot A100 offering at
// the given wholesale price whose Provision succeeds.
func newSpotStub(name string, price float64) *spotStub {
	return &spotStub{
		stubProvider: stubProvider{
			name:      name,
			offerings: []provider.GPUOffering{spotEngineOffering(name, price)},
			provisionResult: &provider.ProvisionResult{
				UpstreamID:  "up-" + name,
				UpstreamIP:  "10.0.0.9",
				Status:      "creating",
				CostPerHour: price,
				Region:      "us-east",
			},
		},
		supportsSpot: true,
	}
}

// newSpotUnitEngine mirrors newProvisionUnitEngine but registers multiple
// providers so failover across candidates can be exercised.
func newSpotUnitEngine(fake *fakeEngineDB, provs ...provider.Provider) *Engine {
	reg := provider.NewRegistry()
	for _, p := range provs {
		reg.Register(p)
	}
	return &Engine{
		registry:  reg,
		db:        fake,
		config:    &config.Config{},
		logger:    slog.Default(),
		tunnelMgr: nil,
	}
}

func spotRequest() ProvisionRequest {
	return ProvisionRequest{
		OrgID:    "org-1",
		UserID:   "user-1",
		GPUType:  provider.GPUTypeA10080GB,
		GPUCount: 1,
		Tier:     provider.TierSpot,
	}
}

// TestSelectProviderCandidates_SpotGateExcludesNonSpotCapable proves the
// tier=spot eligibility gate: providers that don't implement SpotCapability —
// or implement it with SupportsSpot()==false — are skipped even when they
// advertise cheaper spot-tier offerings.
func TestSelectProviderCandidates_SpotGateExcludesNonSpotCapable(t *testing.T) {
	// Plain stub advertises a (bogus) spot offering but lacks SpotCapability.
	plain := &stubProvider{
		name:      "plain",
		offerings: []provider.GPUOffering{spotEngineOffering("plain", 0.10)},
	}
	switchedOff := newSpotStub("switched-off", 0.20)
	switchedOff.supportsSpot = false
	capable := newSpotStub("capable", 3.00)

	e := newTestEngineWithProviders(plain, switchedOff, capable)
	e.config = &config.Config{}

	candidates, err := e.SelectProviderCandidates(context.Background(), spotRequest())
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Prov.Name() != "capable" {
		got := make([]string, len(candidates))
		for i, c := range candidates {
			got[i] = c.Prov.Name()
		}
		t.Fatalf("want only the spot-capable provider, got %v", got)
	}
}

// TestSelectProviderCandidates_SpotCheapestFirst proves spot candidates are
// ranked by retail price ascending, reusing the catalog's per-provider markup.
func TestSelectProviderCandidates_SpotCheapestFirst(t *testing.T) {
	pricey := newSpotStub("pricey", 2.00)
	cheap := newSpotStub("cheap", 1.00)
	mid := newSpotStub("mid", 1.50)

	e := newTestEngineWithProviders(pricey, cheap, mid)
	e.config = &config.Config{PricingMarkupPct: 10}

	candidates, err := e.SelectProviderCandidates(context.Background(), spotRequest())
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	wantOrder := []string{"cheap", "mid", "pricey"}
	if len(candidates) != len(wantOrder) {
		t.Fatalf("want %d candidates, got %d", len(wantOrder), len(candidates))
	}
	for i, w := range wantOrder {
		if candidates[i].Prov.Name() != w {
			t.Errorf("candidate[%d] = %s, want %s", i, candidates[i].Prov.Name(), w)
		}
	}
}

// TestSelectProviderCandidates_OnDemandIgnoresSpotGate proves the on-demand
// path is unaffected by the spot gate: a provider WITHOUT SpotCapability still
// serves tier=on_demand requests.
func TestSelectProviderCandidates_OnDemandIgnoresSpotGate(t *testing.T) {
	plain := &stubProvider{
		name:      "plain",
		offerings: []provider.GPUOffering{makeOffering(provider.GPUTypeA10080GB, "us-east", "US-TX-3", 1.50, 3)},
	}
	e := newTestEngineWithProviders(plain)
	e.config = &config.Config{}

	req := spotRequest()
	req.Tier = provider.TierOnDemand
	candidates, err := e.SelectProviderCandidates(context.Background(), req)
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Prov.Name() != "plain" {
		t.Fatalf("on-demand request must still admit non-spot-capable providers, got %d candidates", len(candidates))
	}
}

// TestProvision_SpotFailoverToNextCheapest proves the bounded-fallback loop
// for tier=spot: the cheapest spot candidate is attempted first; when its
// upstream Provision fails, the engine lands on the next-cheapest instead of
// erroring out.
func TestProvision_SpotFailoverToNextCheapest(t *testing.T) {
	cheap := newSpotStub("cheap", 1.00)
	cheap.provisionErr = errors.New("capacity reclaimed at allocation")
	backup := newSpotStub("backup", 2.00)

	fake := fakeWithOneKey()
	e := newSpotUnitEngine(fake, backup, cheap) // registry order != price order on purpose

	resp, err := e.Provision(context.Background(), spotRequest())
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(cheap.provisionReqs) != 1 {
		t.Errorf("cheapest spot provider must be attempted first, got %d attempts", len(cheap.provisionReqs))
	}
	if len(backup.provisionReqs) != 1 {
		t.Errorf("failover must land on the next-cheapest provider, got %d attempts", len(backup.provisionReqs))
	}
	if len(fake.createdInstances) != 1 {
		t.Fatalf("want exactly 1 persisted instance, got %d", len(fake.createdInstances))
	}
	inst := fake.createdInstances[0]
	if inst.UpstreamProvider != "backup" {
		t.Errorf("persisted instance provider = %s, want backup", inst.UpstreamProvider)
	}
	if inst.Tier != string(provider.TierSpot) {
		t.Errorf("persisted instance tier = %s, want spot", inst.Tier)
	}
	if resp.InstanceID == "" {
		t.Error("response must carry the new instance id")
	}
}

// TestProvision_PerAttemptPriceCap_FallbackExceedsCap proves the per-attempt
// price guard: when the cheapest candidate ghosts and the next-cheapest exceeds
// the caller's max_price_per_hour, the engine STOPS rather than placing above
// the approved price — and returns a *PriceExceededError carrying the current
// best price so the client can re-quote. Without this guard the fallback loop
// would silently launch on the pricier candidate and bill above the cap.
func TestProvision_PerAttemptPriceCap_FallbackExceedsCap(t *testing.T) {
	cheap := newSpotStub("cheap", 1.00)
	cheap.provisionErr = errors.New("capacity reclaimed at allocation") // ghost
	backup := newSpotStub("backup", 2.00)

	fake := fakeWithOneKey()
	e := newSpotUnitEngine(fake, backup, cheap) // zero markup → retail == wholesale

	req := spotRequest()
	priceCap := 1.50 // above cheap (1.00), below backup (2.00)
	req.MaxPricePerHour = &priceCap

	_, err := e.Provision(context.Background(), req)
	if err == nil {
		t.Fatal("Provision must fail when the only fallback exceeds the price cap")
	}
	if !errors.Is(err, ErrPriceExceeded) {
		t.Fatalf("error must satisfy errors.Is(ErrPriceExceeded), got %v", err)
	}
	var priceErr *PriceExceededError
	if !errors.As(err, &priceErr) {
		t.Fatalf("error must unwrap to *PriceExceededError, got %v", err)
	}
	if priceErr.CurrentBestPerHour != 2.00 {
		t.Errorf("CurrentBestPerHour = %v, want 2.00 (the next-cheapest retail)", priceErr.CurrentBestPerHour)
	}
	if priceErr.MaxPricePerHour != 1.50 {
		t.Errorf("MaxPricePerHour = %v, want 1.50 (the cap the caller sent)", priceErr.MaxPricePerHour)
	}
	if len(cheap.provisionReqs) != 1 {
		t.Errorf("cheapest candidate must be attempted once, got %d", len(cheap.provisionReqs))
	}
	if len(backup.provisionReqs) != 0 {
		t.Errorf("over-cap fallback must NOT be provisioned, got %d attempts", len(backup.provisionReqs))
	}
	if len(fake.createdInstances) != 0 {
		t.Errorf("no instance may be persisted above the cap, got %d", len(fake.createdInstances))
	}
}

// TestProvision_PriceCap_CheapestExceedsCap proves the fast-fail path: when even
// the cheapest candidate is above the cap, Provision returns *PriceExceededError
// carrying that price BEFORE attempting any provider.
func TestProvision_PriceCap_CheapestExceedsCap(t *testing.T) {
	cheap := newSpotStub("cheap", 1.00)
	fake := fakeWithOneKey()
	e := newSpotUnitEngine(fake, cheap)

	req := spotRequest()
	priceCap := 0.50 // below the cheapest (1.00)
	req.MaxPricePerHour = &priceCap

	_, err := e.Provision(context.Background(), req)
	var priceErr *PriceExceededError
	if !errors.As(err, &priceErr) {
		t.Fatalf("error must be *PriceExceededError, got %v", err)
	}
	if priceErr.CurrentBestPerHour != 1.00 {
		t.Errorf("CurrentBestPerHour = %v, want 1.00", priceErr.CurrentBestPerHour)
	}
	if len(cheap.provisionReqs) != 0 {
		t.Errorf("no provider may be attempted when the cheapest is over cap, got %d", len(cheap.provisionReqs))
	}
}

// TestRerouteSpot_LandsOnCheapestSpot proves the preemption re-route path:
// RerouteSpot rebuilds a tier=spot request from the reclaimed instance's shape
// (GPU type/count, canonical region) and the normal Provision path places it
// on the current cheapest spot candidate.
func TestRerouteSpot_LandsOnCheapestSpot(t *testing.T) {
	cheap := newSpotStub("cheap", 1.00)
	pricey := newSpotStub("pricey", 2.00)

	fake := fakeWithOneKey()
	e := newSpotUnitEngine(fake, pricey, cheap)

	resp, err := e.RerouteSpot(context.Background(), db.Instance{
		InstanceID:       "inst-old",
		OrgID:            "org-1",
		UserID:           "user-1",
		UpstreamProvider: "pricey",
		UpstreamID:       "up-old",
		GPUType:          string(provider.GPUTypeA10080GB),
		GPUCount:         1,
		Tier:             string(provider.TierSpot),
		Region:           "us-east",
	})
	if err != nil {
		t.Fatalf("RerouteSpot: %v", err)
	}
	if len(cheap.provisionReqs) != 1 {
		t.Errorf("reroute must provision on the cheapest spot candidate, got %d attempts", len(cheap.provisionReqs))
	}
	if len(pricey.provisionReqs) != 0 {
		t.Errorf("pricier provider must not be attempted when the cheapest succeeds, got %d attempts", len(pricey.provisionReqs))
	}
	if len(fake.createdInstances) != 1 {
		t.Fatalf("want exactly 1 persisted replacement instance, got %d", len(fake.createdInstances))
	}
	inst := fake.createdInstances[0]
	if inst.UpstreamProvider != "cheap" {
		t.Errorf("replacement instance provider = %s, want cheap", inst.UpstreamProvider)
	}
	if inst.Region != "us-east" {
		t.Errorf("replacement instance region = %s, want us-east (pinned to the reclaimed instance's region)", inst.Region)
	}
	if resp.InstanceID == "" {
		t.Error("reroute response must carry the replacement instance id")
	}
}

// TestRerouteSpot_CarriesAutoTerminateHours pins the GPU-60 reroute contract:
// a bounded spot instance that gets preempted must be replaced by a bounded
// instance. Dropping the bound here would mean the PLATFORM launched the
// unattended, unbounded instance the feature exists to prevent — the customer
// asked for a machine that stops on its own and never saw the preemption.
//
// The replacement carries the HOURS, not a remainder: it re-arms at its own
// running transition, so the customer gets a fresh N hours on a machine they
// did not ask to lose. (The old row's deadline is gone by reroute time anyway.)
func TestRerouteSpot_CarriesAutoTerminateHours(t *testing.T) {
	cheap := newSpotStub("cheap", 1.00)
	fake := fakeWithOneKey()
	e := newSpotUnitEngine(fake, cheap)

	hours := 6
	if _, err := e.RerouteSpot(context.Background(), db.Instance{
		InstanceID:         "inst-old",
		OrgID:              "org-1",
		UserID:             "user-1",
		UpstreamProvider:   "cheap",
		UpstreamID:         "up-old",
		GPUType:            string(provider.GPUTypeA10080GB),
		GPUCount:           1,
		Tier:               string(provider.TierSpot),
		Region:             "us-east",
		AutoTerminateHours: &hours,
	}); err != nil {
		t.Fatalf("RerouteSpot: %v", err)
	}
	if len(fake.createdInstances) != 1 {
		t.Fatalf("want exactly 1 persisted replacement instance, got %d", len(fake.createdInstances))
	}
	got := fake.createdInstances[0].AutoTerminateHours
	if got == nil {
		t.Fatal("replacement instance has NO runtime bound — a preemption silently turned a " +
			"bounded launch into an unbounded one, which is the GPU-60 incident")
	}
	if *got != hours {
		t.Errorf("replacement auto_terminate_hours = %d, want %d", *got, hours)
	}
	// The deadline itself is NOT copied: it is armed by the replacement's own
	// SetInstanceRunning, so a row that never boots never carries a deadline.
	if fake.createdInstances[0].AutoTerminateAt != nil {
		t.Errorf("replacement must not be persisted with a deadline (%v); arming belongs to "+
			"the running transition", fake.createdInstances[0].AutoTerminateAt)
	}
}

// TestRerouteSpot_UnboundedStaysUnbounded is the other half: an instance with
// no limit must not acquire one across a reroute.
func TestRerouteSpot_UnboundedStaysUnbounded(t *testing.T) {
	cheap := newSpotStub("cheap", 1.00)
	fake := fakeWithOneKey()
	e := newSpotUnitEngine(fake, cheap)

	if _, err := e.RerouteSpot(context.Background(), db.Instance{
		InstanceID: "inst-old", OrgID: "org-1", UserID: "user-1",
		UpstreamProvider: "cheap", UpstreamID: "up-old",
		GPUType:  string(provider.GPUTypeA10080GB),
		GPUCount: 1, Tier: string(provider.TierSpot), Region: "us-east",
	}); err != nil {
		t.Fatalf("RerouteSpot: %v", err)
	}
	if len(fake.createdInstances) != 1 {
		t.Fatalf("want exactly 1 persisted replacement instance, got %d", len(fake.createdInstances))
	}
	if got := fake.createdInstances[0].AutoTerminateHours; got != nil {
		t.Errorf("replacement acquired a runtime bound of %d hours nobody asked for", *got)
	}
}

// TestRerouteSpot_NoCapacityReturnsError proves a reroute with no live spot
// capacity surfaces an error (the watcher logs it; the customer's instance is
// already closed out by the caller).
func TestRerouteSpot_NoCapacityReturnsError(t *testing.T) {
	// Only an on-demand provider is registered — no spot candidates exist.
	plain := onDemandStub("up-1", "us-east")
	fake := fakeWithOneKey()
	e := newSpotUnitEngine(fake, plain)

	_, err := e.RerouteSpot(context.Background(), db.Instance{
		InstanceID: "inst-old",
		OrgID:      "org-1",
		UserID:     "user-1",
		GPUType:    string(provider.GPUTypeA10080GB),
		GPUCount:   1,
		Tier:       string(provider.TierSpot),
		Region:     "us-east",
	})
	if err == nil {
		t.Fatal("RerouteSpot with no spot capacity must return an error")
	}
	if len(fake.createdInstances) != 0 {
		t.Errorf("no instance may be persisted when reroute fails, got %d", len(fake.createdInstances))
	}
}

// TestProvision_CustomImageSpotRefused pins the GPU-92 launch-time gate: a
// customer-image request on the spot tier fails with
// ErrCustomImageSpotUnsupported before any provider is attempted or any row is
// persisted — a preemption re-route cannot carry the image, so the shape is
// refused rather than launched into a silent-swap hazard.
func TestProvision_CustomImageSpotRefused(t *testing.T) {
	cheap := newSpotStub("cheap", 1.00)
	fake := fakeWithOneKey()
	e := newSpotUnitEngine(fake, cheap)

	req := spotRequest()
	req.Image = "ghcr.io/acme/trainer:v1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.CustomerImage = true

	_, err := e.Provision(context.Background(), req)
	if !errors.Is(err, ErrCustomImageSpotUnsupported) {
		t.Fatalf("Provision(custom image, tier=spot) error = %v, want ErrCustomImageSpotUnsupported", err)
	}
	if len(cheap.provisionReqs) != 0 {
		t.Errorf("no provider may be attempted for a refused custom-image spot launch, got %d", len(cheap.provisionReqs))
	}
	if len(fake.createdInstances) != 0 {
		t.Errorf("no instance row may be persisted for a refused launch, got %d", len(fake.createdInstances))
	}

	// The same shape on on_demand is not gated here (it proceeds into the
	// normal placement path — the spot stubs advertise spot only, so it fails
	// later on ErrNoProvider, which proves the gate is tier-specific).
	req.Tier = provider.TierOnDemand
	_, err = e.Provision(context.Background(), req)
	if errors.Is(err, ErrCustomImageSpotUnsupported) {
		t.Fatalf("on_demand custom-image launch must not trip the spot gate, got %v", err)
	}
}

// TestRerouteSpot_RefusesCustomEnvironment pins the GPU-92 reroute guard: a
// reclaimed instance whose row carries a custom image or a template id is NOT
// re-placed from its GPU shape alone (that would boot a default-image
// replacement and present it as the customer's instance). RerouteSpot fails
// with ErrSpotRerouteCustomEnvironment and touches no provider and no row.
func TestRerouteSpot_RefusesCustomEnvironment(t *testing.T) {
	base := func() db.Instance {
		return db.Instance{
			InstanceID:       "inst-old",
			OrgID:            "org-1",
			UserID:           "user-1",
			UpstreamProvider: "pricey",
			UpstreamID:       "up-old",
			GPUType:          string(provider.GPUTypeA10080GB),
			GPUCount:         1,
			Tier:             string(provider.TierSpot),
			Region:           "us-east",
		}
	}
	img := "ghcr.io/acme/trainer:v1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tmpl := "vllm"
	cases := []struct {
		name string
		mut  func(*db.Instance)
	}{
		{"custom image", func(i *db.Instance) { i.Image = &img }},
		{"template deploy", func(i *db.Instance) { i.TemplateID = &tmpl }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cheap := newSpotStub("cheap", 1.00)
			pricey := newSpotStub("pricey", 2.00)
			fake := fakeWithOneKey()
			e := newSpotUnitEngine(fake, pricey, cheap)

			inst := base()
			c.mut(&inst)
			_, err := e.RerouteSpot(context.Background(), inst)
			if !errors.Is(err, ErrSpotRerouteCustomEnvironment) {
				t.Fatalf("RerouteSpot error = %v, want ErrSpotRerouteCustomEnvironment", err)
			}
			if n := len(cheap.provisionReqs) + len(pricey.provisionReqs); n != 0 {
				t.Errorf("a refused reroute must attempt no provider, got %d attempts", n)
			}
			if len(fake.createdInstances) != 0 {
				t.Errorf("a refused reroute must persist no replacement row, got %d", len(fake.createdInstances))
			}
		})
	}

	// Control: an empty image/template pointer (a plain raw launch) still
	// reroutes — the guard keys on content, not on pointer presence.
	empty := ""
	cheap := newSpotStub("cheap", 1.00)
	fake := fakeWithOneKey()
	e := newSpotUnitEngine(fake, cheap)
	inst := base()
	inst.Image, inst.TemplateID = &empty, &empty
	if _, err := e.RerouteSpot(context.Background(), inst); err != nil {
		t.Fatalf("plain instance with empty image/template pointers must still reroute, got %v", err)
	}
}
