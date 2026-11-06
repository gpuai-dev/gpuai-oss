package availability

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

func TestLaunchKey(t *testing.T) {
	if got := LaunchKey("alpha", "a100_80gb", 1); got != "alpha|a100_80gb|x1" {
		t.Errorf("LaunchKey = %q", got)
	}
	// Distinct providers with the same GPU type never share a circuit.
	if LaunchKey("alpha", "a100_80gb", 1) == LaunchKey("bravo", "a100_80gb", 1) {
		t.Error("keys must be provider-scoped")
	}
}

// TestLaunchKey_ShapeScoped is the reason gpu_count joined the key. On prod over
// the 14 days to 2026-09-17, one provider's l40 x1 failed 2/46 (4.3%, healthy and
// selling) while l40 x4 failed 10/10 and b200 x8 failed 5/5. Sharing one circuit
// means either the dead shape keeps selling (x1 successes reset the counter) or
// the healthy one is suppressed with it. A 4-GPU placement needs four free GPUs
// on one host; a 1-GPU placement needs one. Different questions, different
// circuits.
func TestLaunchKey_ZeroCountNormalizesToOne(t *testing.T) {
	// Record-side and allow-side must never disagree on the key; an unset count
	// would otherwise create a circuit that only one side can address.
	if LaunchKey("alpha", "l40", 0) != LaunchKey("alpha", "l40", 1) {
		t.Error("an unset gpu_count must key the same circuit as x1, or failures are recorded against a key no placement lookup produces")
	}
	if LaunchKey("alpha", "l40", -3) != LaunchKey("alpha", "l40", 1) {
		t.Error("a negative gpu_count must normalize to x1")
	}
}

func TestLaunchKey_ShapeScoped(t *testing.T) {
	if LaunchKey("alpha", "l40", 1) == LaunchKey("alpha", "l40", 4) {
		t.Error("l40 x1 (4.3% failure) and l40 x4 (100% failure) must not share a circuit")
	}
	for _, n := range []int{1, 2, 4, 8} {
		if got := LaunchKey("alpha", "l40", n); !strings.HasSuffix(got, "|x"+strconv.Itoa(n)) {
			t.Errorf("LaunchKey(...,%d) = %q, want it to carry the count", n, got)
		}
	}
}

// TestLaunchBreaker_ShapeIsolation proves the isolation end-to-end on the real
// state machine: tripping the dead shape must leave the healthy one admitting
// launches. Under the old (provider, gpu_type) key this test could not pass.
func TestLaunchBreaker_ShapeIsolation(t *testing.T) {
	b := NewRateBreaker(DefaultLaunchBreakerWindow, DefaultLaunchBreakerMinSamples,
		DefaultLaunchBreakerFailRate, DefaultLaunchBreakerCooldown, nil)

	dead := LaunchKey("alpha", "l40", 4)
	healthy := LaunchKey("alpha", "l40", 1)

	for i := 0; i < DefaultLaunchBreakerMinSamples; i++ {
		b.RecordFailure(dead, errors.New("upstream reported error before running"))
	}
	if b.Allow(dead) {
		t.Error("l40 x4 should be suppressed after consecutive boot failures")
	}
	if !b.Allow(healthy) {
		t.Error("l40 x1 must keep selling — it is a different shape and a different circuit")
	}
}

// TestPoller_LaunchBreakerSuppressesDeadSKU — the GPU-108 shape: the
// provider's availability API is perfectly healthy and keeps advertising a
// SKU whose launches die at boot. Once the launch-outcome breaker trips for
// that (provider, gpu_type), the poller drops the row from the catalog while
// the provider's OTHER SKUs — and other providers — keep flowing. A recorded
// success (a probe that booted) restores the row.
func TestPoller_LaunchBreakerSuppressesDeadSKU(t *testing.T) {
	registry := provider.NewRegistry()
	// "lying" advertises two SKUs; only its a100 is dead.
	lying := newFakeProvider("lying", []provider.GPUOffering{
		{Provider: "lying", GPUType: provider.GPUTypeA10080GB, GPUCount: 1, PricePerHour: 1.0,
			Tier: provider.TierOnDemand, Region: "us-east", AvailableCount: 4},
		{Provider: "lying", GPUType: provider.GPUTypeH100SXM, GPUCount: 1, PricePerHour: 2.5,
			Tier: provider.TierOnDemand, Region: "us-east", AvailableCount: 4},
	})
	honest := newFakeProvider("honest", sampleOfferings("honest"))
	registry.Register(lying)
	registry.Register(honest)

	clk := newClock(time.Now())
	lb := NewBreaker(3, 10*time.Minute, silentLogger())
	lb.SetClock(clk.Now)

	poller := &Poller{
		registry:      registry,
		interval:      30 * time.Second,
		probeTimeout:  time.Second,
		launchBreaker: lb,
		logger:        silentLogger(),
	}
	ctx := context.Background()
	key := LaunchKey("lying", string(provider.GPUTypeA10080GB), 1)

	// Healthy: all three rows.
	if got, _ := poller.collectOfferings(ctx); len(got) != 3 {
		t.Fatalf("healthy cycle: want 3 offerings, got %d", len(got))
	}

	// Three boot failures trip the a100 circuit.
	for range 3 {
		lb.RecordFailure(key, errors.New("upstream reported error before running"))
	}
	got, staff := poller.collectOfferings(ctx)
	if len(got) != 2 {
		t.Fatalf("tripped cycle: want 2 customer offerings (dead SKU suppressed), got %d", len(got))
	}
	for _, o := range got {
		if o.GPUModel == string(provider.GPUTypeA10080GB) && o.PricePerHour == 1.0 {
			t.Errorf("the dead SKU must not appear in the customer catalog: %+v", o)
		}
	}
	// The STAFF snapshot keeps the suppressed row — ops must be able to see
	// what the customer catalog is hiding.
	if len(staff) != 3 {
		t.Fatalf("tripped cycle: staff snapshot must retain the suppressed row, got %d rows", len(staff))
	}

	// Cooldown elapsed → hiding is bounded: the row RETURNS so real traffic
	// can probe the SKU (a hidden SKU generates no launches and could never
	// heal). The circuit is still Tripped until an outcome arrives.
	clk.Advance(11 * time.Minute)
	if got, _ := poller.collectOfferings(ctx); len(got) != 3 {
		t.Fatalf("post-cooldown cycle: row must return for probing, got %d offerings", len(got))
	}

	// A failed probe re-trips → hidden again for another cooldown. The Allow
	// models the engine admitting the probe at placement (Tripped→HalfOpen);
	// its failure then re-trips with a fresh cooldown clock.
	if !lb.Allow(key) {
		t.Fatal("post-cooldown placement must admit the probe")
	}
	lb.RecordFailure(key, errors.New("upstream reported error before running"))
	if got, _ := poller.collectOfferings(ctx); len(got) != 2 {
		t.Fatalf("re-tripped cycle: want 2 offerings, got %d", len(got))
	}

	// A successful boot heals the circuit and the row returns immediately.
	lb.RecordSuccess(key)
	if got, _ := poller.collectOfferings(ctx); len(got) != 3 {
		t.Fatalf("healed cycle: want 3 offerings, got %d", len(got))
	}
}
