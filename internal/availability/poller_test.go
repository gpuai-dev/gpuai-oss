package availability

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// fakeProvider is a controllable provider.Provider implementation for the
// poller integration tests. It records every ListAvailable call and returns
// either a stub offering set or a configured error.
type fakeProvider struct {
	name        string
	calls       atomic.Int32
	failNext    atomic.Int32 // when > 0, ListAvailable returns errBoom and decrements
	offerings   []provider.GPUOffering
	listLatency time.Duration
}

var errBoom = errors.New("simulated provider failure")

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) ListAvailable(ctx context.Context) ([]provider.GPUOffering, error) {
	f.calls.Add(1)
	if f.listLatency > 0 {
		select {
		case <-time.After(f.listLatency):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.failNext.Load() > 0 {
		f.failNext.Add(-1)
		return nil, errBoom
	}
	return f.offerings, nil
}

func (f *fakeProvider) Provision(context.Context, provider.ProvisionRequest) (*provider.ProvisionResult, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeProvider) GetStatus(context.Context, string, string) (*provider.InstanceStatus, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeProvider) Terminate(context.Context, string, string) error {
	return errors.New("not implemented in fake")
}
func (f *fakeProvider) ListProvisioned(context.Context) ([]provider.ProvisionedInstance, error) {
	return nil, nil
}

func newFakeProvider(name string, offerings []provider.GPUOffering) *fakeProvider {
	return &fakeProvider{name: name, offerings: offerings}
}

func sampleOfferings(name string) []provider.GPUOffering {
	return []provider.GPUOffering{{
		Provider:       name,
		GPUType:        provider.GPUTypeH100SXM,
		GPUCount:       1,
		VRAMPerGPUGB:   80,
		PricePerHour:   2.5,
		Tier:           provider.TierOnDemand,
		Region:         "us-east",
		AvailableCount: 4,
	}}
}

// TestPoller_BreakerDropsFailingProviderAndRecovers walks the full circuit
// breaker lifecycle through the real Poller.collectOfferings code path:
//
//  1. Healthy: both providers contribute offerings.
//  2. Failing: provider "flaky" returns errors 5 cycles in a row → trips.
//  3. Tripped: poller skips "flaky" (no ListAvailable call), aggregated
//     response only contains "stable" offerings, breaker reports degraded=1.
//  4. Cooldown advances: next Allow promotes "flaky" to half_open.
//  5. Probe succeeds: state returns to healthy, offerings reappear.
//
// Uses a fake clock injected into the Breaker so the test runs in
// microseconds instead of waiting on real timers.
func TestPoller_BreakerDropsFailingProviderAndRecovers(t *testing.T) {
	registry := provider.NewRegistry()
	stable := newFakeProvider("stable", sampleOfferings("stable"))
	flaky := newFakeProvider("flaky", sampleOfferings("flaky"))
	registry.Register(stable)
	registry.Register(flaky)

	threshold := 5
	cooldown := 60 * time.Second
	clk := newClock(time.Now())
	breaker := NewBreaker(threshold, cooldown, silentLogger())
	breaker.SetClock(clk.Now)

	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: 1 * time.Second,
		breaker:      breaker,
		logger:       silentLogger(),
	}

	ctx := context.Background()

	// Cycle 1: both healthy.
	got, _ := poller.collectOfferings(ctx)
	if len(got) != 2 {
		t.Fatalf("cycle 1: expected 2 offerings (1 per provider), got %d", len(got))
	}
	if breaker.DegradedCount() != 0 {
		t.Fatalf("cycle 1: degraded=%d, want 0", breaker.DegradedCount())
	}

	// Force flaky to fail exactly threshold times so the breaker trips on the
	// last call. Run threshold poll cycles.
	flaky.failNext.Store(int32(threshold))
	for i := 0; i < threshold; i++ {
		got, _ = poller.collectOfferings(ctx)
		// Stable always returns 1 offering; flaky returns nothing (error).
		if len(got) != 1 {
			t.Fatalf("failing cycle %d: expected only stable offering, got %d", i+1, len(got))
		}
	}

	// Breaker should now be tripped. flaky has been called threshold+1 times
	// (1 in the healthy cycle + threshold failures).
	if state := breaker.State("flaky"); state != Tripped {
		t.Fatalf("expected flaky Tripped, got %s", state)
	}
	if breaker.DegradedCount() != 1 {
		t.Fatalf("expected degraded=1, got %d", breaker.DegradedCount())
	}
	flakyCallsAtTrip := flaky.calls.Load()

	// Cycle inside cooldown: flaky must NOT be called.
	got, _ = poller.collectOfferings(ctx)
	if len(got) != 1 {
		t.Fatalf("cooldown cycle: expected 1 offering (stable only), got %d", len(got))
	}
	if flaky.calls.Load() != flakyCallsAtTrip {
		t.Fatalf("flaky was polled while tripped: calls went %d → %d",
			flakyCallsAtTrip, flaky.calls.Load())
	}

	// Advance past cooldown — next Allow promotes to half_open and lets one
	// probe through. flaky.failNext is 0, so the probe succeeds.
	clk.Advance(cooldown + time.Second)
	got, _ = poller.collectOfferings(ctx)
	if len(got) != 2 {
		t.Fatalf("recovery cycle: expected 2 offerings, got %d", len(got))
	}
	if state := breaker.State("flaky"); state != Healthy {
		t.Fatalf("expected flaky Healthy after probe success, got %s", state)
	}
	if breaker.DegradedCount() != 0 {
		t.Fatalf("expected degraded=0 after recovery, got %d", breaker.DegradedCount())
	}
}

// TestPoller_HalfOpenFailureReTrips: half-open + failure → tripped, cooldown
// resets so the next Allow cycle still skips the provider.
func TestPoller_HalfOpenFailureReTrips(t *testing.T) {
	registry := provider.NewRegistry()
	flaky := newFakeProvider("flaky", sampleOfferings("flaky"))
	registry.Register(flaky)

	threshold := 3
	cooldown := 30 * time.Second
	clk := newClock(time.Now())
	breaker := NewBreaker(threshold, cooldown, silentLogger())
	breaker.SetClock(clk.Now)

	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: 1 * time.Second,
		breaker:      breaker,
		logger:       silentLogger(),
	}

	flaky.failNext.Store(int32(threshold))
	for i := 0; i < threshold; i++ {
		_, _ = poller.collectOfferings(context.Background())
	}
	if breaker.State("flaky") != Tripped {
		t.Fatalf("expected Tripped, got %s", breaker.State("flaky"))
	}

	clk.Advance(cooldown + time.Second)
	// Queue a failure for the half-open probe.
	flaky.failNext.Store(1)
	_, _ = poller.collectOfferings(context.Background())
	if breaker.State("flaky") != Tripped {
		t.Fatalf("half-open probe failure should re-trip, got %s", breaker.State("flaky"))
	}

	// Inside the new cooldown — provider must stay skipped.
	callsBefore := flaky.calls.Load()
	_, _ = poller.collectOfferings(context.Background())
	if flaky.calls.Load() != callsBefore {
		t.Fatalf("flaky was polled while in second cooldown")
	}
}

// TestPoller_ProbeTimeoutCountsAsFailure ensures a hung provider whose
// ListAvailable exceeds probeTimeout records a context-deadline failure
// against the breaker (rather than blocking the whole poll cycle).
func TestPoller_ProbeTimeoutCountsAsFailure(t *testing.T) {
	registry := provider.NewRegistry()
	hung := newFakeProvider("hung", sampleOfferings("hung"))
	hung.listLatency = 200 * time.Millisecond
	registry.Register(hung)

	breaker := NewBreaker(2, 60*time.Second, silentLogger())
	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: 20 * time.Millisecond, // shorter than provider latency
		breaker:      breaker,
		logger:       silentLogger(),
	}

	got, _ := poller.collectOfferings(context.Background())
	if len(got) != 0 {
		t.Fatalf("expected 0 offerings (provider timed out), got %d", len(got))
	}
	snap := breaker.Snapshot()
	if len(snap) != 1 || snap[0].ConsecutiveFailures != 1 {
		t.Fatalf("expected 1 recorded failure, got snapshot %+v", snap)
	}
	if snap[0].LastError == "" {
		t.Fatal("expected non-empty LastError after timeout")
	}
}

// TestPoller_NoBreakerStillWorks confirms backward compatibility — Poller
// constructed without WithBreaker continues to merge offerings without
// touching any breaker state.
func TestPoller_NoBreakerStillWorks(t *testing.T) {
	registry := provider.NewRegistry()
	registry.Register(newFakeProvider("ok", sampleOfferings("ok")))
	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: 1 * time.Second,
		logger:       silentLogger(),
	}

	got, _ := poller.collectOfferings(context.Background())
	if len(got) != 1 {
		t.Fatalf("expected 1 offering, got %d", len(got))
	}
}

// TestCollectOfferings_PerProviderMarkup proves the poller resolves the markup
// per provider: with markupFor returning 10% for "alpha" and 0% for "beta",
// alpha's cached retail price is wholesale*1.10 while beta's stays wholesale.
func TestCollectOfferings_PerProviderMarkup(t *testing.T) {
	registry := provider.NewRegistry()
	registry.Register(newFakeProvider("alpha", []provider.GPUOffering{{
		Provider:       "alpha",
		GPUType:        provider.GPUTypeH100SXM,
		GPUCount:       1,
		VRAMPerGPUGB:   80,
		PricePerHour:   2.0,
		Tier:           provider.TierOnDemand,
		Region:         "us-east",
		AvailableCount: 4,
	}}))
	registry.Register(newFakeProvider("beta", []provider.GPUOffering{{
		Provider:       "beta",
		GPUType:        provider.GPUTypeA10080GB,
		GPUCount:       1,
		VRAMPerGPUGB:   80,
		PricePerHour:   2.0,
		Tier:           provider.TierOnDemand,
		Region:         "us-east",
		AvailableCount: 4,
	}}))

	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: 1 * time.Second,
		markupFor: func(name string) float64 {
			if name == "alpha" {
				return 10.0
			}
			return 0
		},
		logger: silentLogger(),
	}

	got, _ := poller.collectOfferings(context.Background())
	if len(got) != 2 {
		t.Fatalf("expected 2 offerings, got %d", len(got))
	}
	// Providers are intentionally anonymized on AvailableOffering — identify
	// alpha/beta by their distinct GPU models.
	prices := map[string]float64{}
	for _, o := range got {
		prices[o.GPUModel] = o.PricePerHour
	}
	if want := 2.2; prices[string(provider.GPUTypeH100SXM)] != want {
		t.Errorf("alpha (h100_sxm) retail = %v, want %v (2.0 + 10%%)", prices[string(provider.GPUTypeH100SXM)], want)
	}
	if want := 2.0; prices[string(provider.GPUTypeA10080GB)] != want {
		t.Errorf("beta (a100_80gb) retail = %v, want %v (0%% markup)", prices[string(provider.GPUTypeA10080GB)], want)
	}
}

// TestCollectOfferings_MinRetailMarkupFloor proves WithMinRetailMarkup threads
// the retail floor into cache-write pricing: a 0%-markup provider whose
// sub-cent wholesale used to round DOWN below cost (the audited l40 0.792 →
// 0.79 regression) is published at the floored 0.84 (0.792 × 1.05, ceil to
// cent) on BOTH the customer snapshot and its staff twin.
func TestCollectOfferings_MinRetailMarkupFloor(t *testing.T) {
	registry := provider.NewRegistry()
	registry.Register(newFakeProvider("gamma", []provider.GPUOffering{{
		Provider:       "gamma",
		GPUType:        provider.GPUTypeL40,
		GPUCount:       1,
		VRAMPerGPUGB:   48,
		PricePerHour:   0.792,
		Tier:           provider.TierOnDemand,
		Region:         "us-east",
		AvailableCount: 2,
	}}))

	poller := NewPoller(
		registry,
		nil, // cache unused by collectOfferings
		30*time.Second,
		func(string) float64 { return 0 }, // misconfigured 0% markup
		silentLogger(),
		WithMinRetailMarkup(DefaultMinRetailMarkupPct),
	)
	poller.probeTimeout = time.Second

	customer, staff := poller.collectOfferings(context.Background())
	if len(customer) != 1 || len(staff) != 1 {
		t.Fatalf("customer/staff rows = %d/%d, want 1/1", len(customer), len(staff))
	}
	if want := 0.84; customer[0].PricePerHour != want {
		t.Errorf("floored customer retail = %v, want %v (never below the 0.792 wholesale)", customer[0].PricePerHour, want)
	}
	if customer[0].PricePerHour != staff[0].RetailPerHour {
		t.Errorf("staff twin retail = %v, want the customer snapshot's %v (same pass, same floor)",
			staff[0].RetailPerHour, customer[0].PricePerHour)
	}
}

// TestPoller_DropsSpotOfferingsSitewide pins the sitewide spot kill-switch in
// poller.go (PR #318, e9a7b222, 2026-07-18 "disable spot instances sitewide
// (unreliable)"). The guard is provider-agnostic and has NO environment
// conditional, so demo and production both publish zero spot rows — verified
// live on 2026-09-03 (0 of 230 prod rows, 0 of 200 demo rows).
//
// This is a PIN, not a behavioral assertion we want to keep forever: the
// customer-facing surfaces (CLI --tier help, docs, MCP tool descriptions,
// README, the API reference page and the OpenAPI tier enums) all say spot
// capacity is currently unavailable BECAUSE of this guard. Deleting the guard
// without also flipping that copy would silently make the docs lie in the
// other direction, so the guard and the copy must change in lockstep. Re-enabling
// spot is a future item gated on reliability monitoring (phase 85, D-09).
//
// Full poll cycle on purpose: the drop must hold at the cache boundary that
// the API, CLI, MCP and frontend all read from — both the customer catalog
// and its staff twin.
func TestPoller_DropsSpotOfferingsSitewide(t *testing.T) {
	registry := provider.NewRegistry()
	registry.Register(newFakeProvider("mixed", []provider.GPUOffering{
		{
			// On-demand row for the same GPU type — must survive.
			Provider:       "mixed",
			GPUType:        provider.GPUTypeH100SXM,
			GPUCount:       1,
			VRAMPerGPUGB:   80,
			PricePerHour:   2.5,
			Tier:           provider.TierOnDemand,
			Region:         "us-east",
			AvailableCount: 4,
		},
		{
			// Spot row for the same GPU type — must be dropped sitewide.
			Provider:       "mixed",
			GPUType:        provider.GPUTypeH100SXM,
			GPUCount:       1,
			VRAMPerGPUGB:   80,
			PricePerHour:   0.9,
			Tier:           provider.TierSpot,
			Region:         "us-east",
			AvailableCount: 9,
		},
	}))

	// Redis is unreachable so the poll lands in the in-process fallback; the
	// guard runs before either write, so the assertion is identical.
	unreachable := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	cache := NewCache(unreachable, 90*time.Second)

	poller := &Poller{
		registry:     registry,
		cache:        cache,
		probeTimeout: time.Second,
		logger:       silentLogger(),
	}
	ctx := context.Background()
	poller.poll(ctx)

	got, err := cache.GetOfferings(ctx)
	if err != nil {
		t.Fatalf("GetOfferings after poll: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("customer catalog = %d rows, want exactly 1 (the on_demand row; the spot row must be dropped)", len(got))
	}
	if got[0].Tier != string(provider.TierOnDemand) {
		t.Errorf("surviving offering tier = %q, want %q", got[0].Tier, provider.TierOnDemand)
	}
	for _, o := range got {
		if o.Tier == string(provider.TierSpot) {
			t.Errorf("spot offering leaked into the customer catalog: %+v", o)
		}
	}

	staff, err := cache.GetStaffOfferings(ctx)
	if err != nil {
		t.Fatalf("GetStaffOfferings after poll: %v", err)
	}
	if len(staff) != 1 {
		t.Fatalf("staff catalog = %d rows, want exactly 1 (the spot row must be dropped from the staff twin too)", len(staff))
	}
	for _, s := range staff {
		if s.Tier == string(provider.TierSpot) {
			t.Errorf("spot offering leaked into the staff catalog: %+v", s)
		}
	}
}

// M1: an offering whose source region is empty must never reach the public
// catalog (it normalizes to RegionUnknown — not customer-routable). A second,
// well-formed offering from the same provider must still pass through.
func TestCollectOfferings_DropsUnknownRegion(t *testing.T) {
	registry := provider.NewRegistry()
	registry.Register(newFakeProvider("charlie", []provider.GPUOffering{
		{
			// Empty region — the M1 data-quality row (e.g. a supplier
			// row with no `region`). Must be dropped.
			Provider:       "charlie",
			GPUType:        provider.GPUTypeH100SXM,
			GPUCount:       1,
			VRAMPerGPUGB:   80,
			PricePerHour:   3.0,
			Tier:           provider.TierOnDemand,
			Region:         "",
			AvailableCount: 48,
		},
		{
			// Well-formed row from the same provider — must survive.
			Provider:       "charlie",
			GPUType:        provider.GPUTypeA10080GB,
			GPUCount:       1,
			VRAMPerGPUGB:   80,
			PricePerHour:   2.0,
			Tier:           provider.TierOnDemand,
			Region:         "india-south",
			AvailableCount: 4,
		},
	}))

	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: 1 * time.Second,
		logger:       silentLogger(),
	}

	got, _ := poller.collectOfferings(context.Background())
	if len(got) != 1 {
		t.Fatalf("expected 1 offering (empty-region row dropped), got %d", len(got))
	}
	if got[0].GPUModel != string(provider.GPUTypeA10080GB) {
		t.Errorf("surviving offering = %q, want the well-formed a100_80gb row", got[0].GPUModel)
	}
	for _, o := range got {
		if o.Region == RegionUnknown {
			t.Errorf("offering with region %q leaked into the catalog", o.Region)
		}
	}
}

// TestCollectOfferings_LastKnownGoodOnFailure: with retention wired, a failed
// poll serves the provider's last successful snapshot — customer and staff
// rows alike — instead of blanking it from the cycle.
func TestCollectOfferings_LastKnownGoodOnFailure(t *testing.T) {
	registry := provider.NewRegistry()
	flaky := newFakeProvider("flaky", sampleOfferings("flaky"))
	registry.Register(flaky)

	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: 1 * time.Second,
		retention:    5 * time.Minute,
		logger:       silentLogger(),
	}

	ctx := context.Background()

	// Cycle 1: healthy — snapshot recorded.
	got, staff := poller.collectOfferings(ctx)
	if len(got) != 1 || len(staff) != 1 {
		t.Fatalf("healthy cycle: got %d customer / %d staff rows, want 1/1", len(got), len(staff))
	}

	// Cycle 2: poll fails — retained rows stand in on both views.
	flaky.failNext.Store(1)
	got, staff = poller.collectOfferings(ctx)
	if len(got) != 1 || len(staff) != 1 {
		t.Fatalf("failing cycle: got %d customer / %d staff rows, want retained 1/1", len(got), len(staff))
	}
	if staff[0].Provider != "flaky" {
		t.Fatalf("retained staff row provider = %q, want flaky", staff[0].Provider)
	}
}

// TestCollectOfferings_LastKnownGoodExpires: a retained snapshot past the
// retention window is dropped, restoring the strict clear-on-failure shape.
func TestCollectOfferings_LastKnownGoodExpires(t *testing.T) {
	registry := provider.NewRegistry()
	flaky := newFakeProvider("flaky", sampleOfferings("flaky"))
	registry.Register(flaky)

	clk := newClock(time.Now())
	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: 1 * time.Second,
		retention:    5 * time.Minute,
		now:          clk.Now,
		logger:       silentLogger(),
	}

	ctx := context.Background()
	if got, _ := poller.collectOfferings(ctx); len(got) != 1 {
		t.Fatalf("healthy cycle: got %d rows, want 1", len(got))
	}

	// Within the window: retained.
	flaky.failNext.Store(1)
	clk.Advance(4 * time.Minute)
	if got, _ := poller.collectOfferings(ctx); len(got) != 1 {
		t.Fatalf("failing cycle inside window: got %d rows, want retained 1", len(got))
	}

	// The stand-in does NOT refresh the snapshot's age: 6 minutes after the
	// last real success the entry expires even though cycle 2 served it.
	flaky.failNext.Store(1)
	clk.Advance(2 * time.Minute)
	if got, _ := poller.collectOfferings(ctx); len(got) != 0 {
		t.Fatalf("failing cycle past window: got %d rows, want 0 (expired)", len(got))
	}
}

// TestCollectOfferings_RetentionZeroKeepsClearOnFailure: the zero-value poller
// (retention unset) preserves the pre-retention contract — a failed poll
// clears the provider's rows within one cycle.
func TestCollectOfferings_RetentionZeroKeepsClearOnFailure(t *testing.T) {
	registry := provider.NewRegistry()
	flaky := newFakeProvider("flaky", sampleOfferings("flaky"))
	registry.Register(flaky)

	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: 1 * time.Second,
		logger:       silentLogger(),
	}

	ctx := context.Background()
	if got, _ := poller.collectOfferings(ctx); len(got) != 1 {
		t.Fatalf("healthy cycle: got %d rows, want 1", len(got))
	}
	flaky.failNext.Store(1)
	if got, _ := poller.collectOfferings(ctx); len(got) != 0 {
		t.Fatalf("failing cycle with retention off: got %d rows, want 0", len(got))
	}
}

// TestCollectOfferings_DisableClearsLastKnownGood: the PROVIDERS_DISABLED
// kill-switch clears a provider within one cycle AND discards its retained
// snapshot, so a later failure cannot resurrect rows the operator pulled.
func TestCollectOfferings_DisableClearsLastKnownGood(t *testing.T) {
	registry := provider.NewRegistry()
	flaky := newFakeProvider("flaky", sampleOfferings("flaky"))
	registry.Register(flaky)

	disabled := false
	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: 1 * time.Second,
		retention:    5 * time.Minute,
		isDisabled:   func(string) bool { return disabled },
		logger:       silentLogger(),
	}

	ctx := context.Background()
	if got, _ := poller.collectOfferings(ctx); len(got) != 1 {
		t.Fatalf("healthy cycle: got %d rows, want 1", len(got))
	}

	disabled = true
	if got, _ := poller.collectOfferings(ctx); len(got) != 0 {
		t.Fatalf("disabled cycle: got %d rows, want 0", len(got))
	}

	// Re-enable but fail the poll: the snapshot was dropped on disable, so
	// nothing is retained to serve.
	disabled = false
	flaky.failNext.Store(1)
	if got, _ := poller.collectOfferings(ctx); len(got) != 0 {
		t.Fatalf("post-disable failing cycle: got %d rows, want 0 (snapshot dropped)", len(got))
	}
}

// TestCollectOfferings_EmptySuccessReplacesLastKnownGood: a successful empty
// listing is a legitimate state, not a failure — it must replace the retained
// snapshot, so later failures serve "out of stock", not resurrected rows.
func TestCollectOfferings_EmptySuccessReplacesLastKnownGood(t *testing.T) {
	registry := provider.NewRegistry()
	flaky := newFakeProvider("flaky", sampleOfferings("flaky"))
	registry.Register(flaky)

	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: 1 * time.Second,
		retention:    5 * time.Minute,
		logger:       silentLogger(),
	}

	ctx := context.Background()
	if got, _ := poller.collectOfferings(ctx); len(got) != 1 {
		t.Fatalf("stocked cycle: got %d rows, want 1", len(got))
	}

	flaky.offerings = nil // upstream legitimately empties out
	if got, _ := poller.collectOfferings(ctx); len(got) != 0 {
		t.Fatalf("empty-success cycle: got %d rows, want 0", len(got))
	}

	flaky.failNext.Store(1)
	if got, _ := poller.collectOfferings(ctx); len(got) != 0 {
		t.Fatalf("failing cycle after empty success: got %d rows, want 0 (empty retained)", len(got))
	}
}

// TestCollectOfferings_BreakerSkipServesLastKnownGood: a tripped breaker skips
// the upstream call but the retained snapshot still stands in, so a breaker
// episode no longer blanks the provider for threshold×interval + cooldown.
func TestCollectOfferings_BreakerSkipServesLastKnownGood(t *testing.T) {
	registry := provider.NewRegistry()
	flaky := newFakeProvider("flaky", sampleOfferings("flaky"))
	registry.Register(flaky)

	clk := newClock(time.Now())
	breaker := NewBreaker(1, 60*time.Second, silentLogger())
	breaker.SetClock(clk.Now)

	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: 1 * time.Second,
		retention:    5 * time.Minute,
		breaker:      breaker,
		logger:       silentLogger(),
	}

	ctx := context.Background()
	if got, _ := poller.collectOfferings(ctx); len(got) != 1 {
		t.Fatalf("healthy cycle: got %d rows, want 1", len(got))
	}

	// One failure trips the threshold-1 breaker; retained rows stand in.
	flaky.failNext.Store(1)
	if got, _ := poller.collectOfferings(ctx); len(got) != 1 {
		t.Fatalf("tripping cycle: got %d rows, want retained 1", len(got))
	}
	if breaker.State("flaky") != Tripped {
		t.Fatalf("breaker state = %s, want tripped", breaker.State("flaky"))
	}

	// Tripped cycle: upstream is NOT called, retained rows still served.
	callsBefore := flaky.calls.Load()
	if got, _ := poller.collectOfferings(ctx); len(got) != 1 {
		t.Fatalf("tripped cycle: got %d rows, want retained 1", len(got))
	}
	if flaky.calls.Load() != callsBefore {
		t.Fatalf("provider polled while tripped: calls %d → %d", callsBefore, flaky.calls.Load())
	}
}

// TestCollectOfferings_RetainedMergesWithFreshProviders: one provider's
// retained rows merge alongside another's fresh rows in the same cycle — the
// stand-in degrades only the failing provider, never the healthy ones.
func TestCollectOfferings_RetainedMergesWithFreshProviders(t *testing.T) {
	registry := provider.NewRegistry()
	stable := newFakeProvider("stable", sampleOfferings("stable"))
	flaky := newFakeProvider("flaky", sampleOfferings("flaky"))
	registry.Register(stable)
	registry.Register(flaky)

	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: 1 * time.Second,
		retention:    5 * time.Minute,
		logger:       silentLogger(),
	}

	ctx := context.Background()
	if got, _ := poller.collectOfferings(ctx); len(got) != 2 {
		t.Fatalf("healthy cycle: got %d rows, want 2", len(got))
	}

	flaky.failNext.Store(1)
	got, staff := poller.collectOfferings(ctx)
	if len(got) != 2 || len(staff) != 2 {
		t.Fatalf("mixed cycle: got %d customer / %d staff rows, want 2/2 (fresh stable + retained flaky)", len(got), len(staff))
	}
	byProvider := map[string]int{}
	for _, s := range staff {
		byProvider[s.Provider]++
	}
	if byProvider["stable"] != 1 || byProvider["flaky"] != 1 {
		t.Fatalf("staff rows per provider = %v, want stable:1 flaky:1", byProvider)
	}
}

// TestCollectOfferings_FloorWarnOncePerProvider — a provider whose configured
// markup prices below the cost floor triggers exactly ONE aggregated warn per
// cycle (count + distinct gpu types), no matter how many of its offerings the
// floor clamps; a healthy-margin provider in the same cycle emits none. The
// per-offer warn this replaced logged 12.8k lines/hour for one 0%-markup
// provider (2026-08-10 demo measurement).
func TestCollectOfferings_FloorWarnOncePerProvider(t *testing.T) {
	registry := provider.NewRegistry()
	registry.Register(newFakeProvider("cheap", []provider.GPUOffering{
		{Provider: "cheap", GPUType: provider.GPUTypeL40, GPUCount: 1, PricePerHour: 0.792, Region: "us-east", Tier: provider.TierOnDemand},
		{Provider: "cheap", GPUType: provider.GPUTypeL40, GPUCount: 2, PricePerHour: 1.584, Region: "us-east", Tier: provider.TierOnDemand},
		{Provider: "cheap", GPUType: provider.GPUTypeH100SXM, GPUCount: 1, PricePerHour: 1.5048, Region: "us-east", Tier: provider.TierOnDemand},
	}))
	registry.Register(newFakeProvider("healthy", sampleOfferings("healthy")))

	var buf bytes.Buffer
	poller := &Poller{
		registry:     registry,
		probeTimeout: time.Second,
		// "cheap" at 0% markup → the 5% floor engages on all 3 of its rows;
		// "healthy" at 15% clears the floor so it must stay silent.
		markupFor: func(name string) float64 {
			if name == "healthy" {
				return 15.0
			}
			return 0
		},
		minRetailMarkupPct: 5.0,
		logger:             slog.New(slog.NewTextHandler(&buf, nil)),
	}

	if got, _ := poller.collectOfferings(context.Background()); len(got) != 4 {
		t.Fatalf("expected 4 offerings, got %d", len(got))
	}

	logged := buf.String()
	if n := strings.Count(logged, "retail price floor engaged"); n != 1 {
		t.Fatalf("floor warn emitted %d times, want exactly 1; log: %s", n, logged)
	}
	for _, kv := range []string{"provider=cheap", "offerings=3", "gpu_types=h100_sxm,l40", "markup_pct=0", "min_markup_pct=5"} {
		if !strings.Contains(logged, kv) {
			t.Errorf("aggregated floor warn missing %q; log: %s", kv, logged)
		}
	}
	if strings.Contains(logged, "provider=healthy") {
		t.Errorf("healthy-margin provider must not warn; log: %s", logged)
	}
}

// TestPoll_SeedsLocalFallback — poll() seeds the cache's in-process fallback
// with the cycle's snapshot BEFORE the Redis write, so a read that misses
// Redis (key expired between a slow cycle's writes, or Redis down entirely —
// here: an unreachable client) serves the last collected catalog instead of
// an empty 200. Also locks the clone contract: mutating a served slice must
// not corrupt the shared snapshot.
func TestPoll_SeedsLocalFallback(t *testing.T) {
	registry := provider.NewRegistry()
	registry.Register(newFakeProvider("stable", sampleOfferings("stable")))

	unreachable := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	cache := NewCache(unreachable, 90*time.Second)

	poller := &Poller{
		registry:     registry,
		cache:        cache,
		probeTimeout: time.Second,
		logger:       silentLogger(),
	}
	ctx := context.Background()
	poller.poll(ctx) // Redis writes fail; the local seed must land anyway

	got, err := cache.GetOfferings(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("GetOfferings after poll = %d rows, err %v; want 1 row from the local fallback", len(got), err)
	}
	staff, err := cache.GetStaffOfferings(ctx)
	if err != nil || len(staff) != 1 {
		t.Fatalf("GetStaffOfferings after poll = %d rows, err %v; want 1 row from the local fallback", len(staff), err)
	}

	// Served slices are clones: corrupting one read must not leak into the next.
	got[0].GPUModel = "mutated"
	again, err := cache.GetOfferings(ctx)
	if err != nil || len(again) != 1 {
		t.Fatalf("second GetOfferings = %d rows, err %v; want 1", len(again), err)
	}
	if again[0].GPUModel == "mutated" {
		t.Errorf("fallback snapshot was corrupted by mutating a served slice — GetOfferings must clone")
	}
}

// TestLocalFallback_ExpiresWithTTL — the in-process fallback honors the same
// freshness contract as the Redis key: a seed older than the cache TTL serves
// nothing, so a dead or wedged poller degrades to the old loud empty-catalog
// failure instead of silently freezing prices and stock forever.
func TestLocalFallback_ExpiresWithTTL(t *testing.T) {
	unreachable := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	cache := NewCache(unreachable, 60*time.Millisecond)
	cache.SetLocalOfferings([]AvailableOffering{{GPUModel: "h100_sxm"}})
	cache.SetLocalStaffOfferings([]StaffOffering{{GPUModel: "h100_sxm"}})

	ctx := context.Background()
	if got, err := cache.GetOfferings(ctx); err != nil || len(got) != 1 {
		t.Fatalf("fresh seed: got %d rows, err %v; want 1 row", len(got), err)
	}

	time.Sleep(80 * time.Millisecond) // outlive the 60ms TTL

	if got, err := cache.GetOfferings(ctx); err == nil && got != nil {
		t.Errorf("expired seed still served %d offerings; want the fallback disabled", len(got))
	}
	if got, err := cache.GetStaffOfferings(ctx); err == nil && got != nil {
		t.Errorf("expired staff seed still served %d rows; want the fallback disabled", len(got))
	}
}

// The catalog price a customer is quoted must equal the price the engine bills,
// for a provider carrying a partner cost discount — through the REAL poller
// path, not a direct RetailPrice call.
//
// This pins the wiring seam, which is the actual defect class here: the
// discount reached recorded COGS but never reached pricing, so a value that was
// configured correctly still produced the wrong price. A unit test on
// RetailPrice cannot catch that — only a test that goes config → poller →
// published PricePerHour can. Numbers are a real prod partner shape: list
// $1.188 invoiced at 0.9x, configured to pass the discount through (-10%).
func TestPoller_PartnerDiscountReachesPublishedCatalogPrice(t *testing.T) {
	const (
		list       = 1.188
		markup     = -10.0
		discount   = 10.0
		wantRetail = 1.07 // 0.9 x list, rounded up to the cent
	)
	registry := provider.NewRegistry()
	p := newFakeProvider("discounted", []provider.GPUOffering{{
		Provider:       "discounted",
		GPUType:        provider.GPUTypeH100SXM,
		GPUCount:       1,
		VRAMPerGPUGB:   80,
		PricePerHour:   list,
		Tier:           provider.TierOnDemand,
		Region:         "us-east",
		AvailableCount: 4,
	}})
	registry.Register(p)

	poller := &Poller{
		registry:        registry,
		interval:        30 * time.Second,
		probeTimeout:    time.Second,
		markupFor:       func(string) float64 { return markup },
		costDiscountFor: func(string) float64 { return discount },
		logger:          silentLogger(),
	}

	offerings, staff := poller.collectOfferings(context.Background())
	if len(offerings) != 1 || len(staff) != 1 {
		t.Fatalf("collectOfferings: got %d customer / %d staff rows, want 1 each", len(offerings), len(staff))
	}
	if got := offerings[0].PricePerHour; math.Abs(got-wantRetail) > 0.0001 {
		t.Errorf("published catalog price = %v, want %v (list %v less the 10%% partner discount)", got, wantRetail, list)
	}
	// The staff twin must agree, or ops debugs against a price no customer saw.
	if got := staff[0].RetailPerHour; math.Abs(got-wantRetail) > 0.0001 {
		t.Errorf("staff twin retail = %v, want %v", got, wantRetail)
	}
	// And the floor must still hold against what we actually pay.
	if got := offerings[0].PricePerHour; got < list*0.9-0.0001 {
		t.Errorf("published price %v is below true cost %v", got, list*0.9)
	}
}

// A nil discount resolver must not panic and must leave prices on the list
// basis. This is deliberately arithmetic-identical to the old code (TrueCost
// with a 0 discount is the identity), so it cannot fail on a pricing
// regression — its job is covering the nil branch the new wiring introduced,
// which WOULD panic if costDiscountFor were called unconditionally.
func TestPoller_NoDiscountResolverLeavesPriceOnListBasis(t *testing.T) {
	registry := provider.NewRegistry()
	registry.Register(newFakeProvider("plain", sampleOfferings("plain")))

	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: time.Second,
		markupFor:    func(string) float64 { return 0 },
		// costDiscountFor deliberately nil.
		logger: silentLogger(),
	}
	offerings, _ := poller.collectOfferings(context.Background())
	if len(offerings) != 1 {
		t.Fatalf("got %d offerings, want 1", len(offerings))
	}
	if got := offerings[0].PricePerHour; math.Abs(got-2.5) > 0.0001 {
		t.Errorf("price = %v, want 2.50 (bare wholesale, no discount wired)", got)
	}
}
