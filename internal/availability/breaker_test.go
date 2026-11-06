package availability

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// silentLogger discards breaker log output during unit tests.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fixedClock returns a clock fn whose value can be advanced by tests.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(t time.Time) *fixedClock { return &fixedClock{now: t} }

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestBreaker_AllowsHealthyProviders(t *testing.T) {
	b := NewBreaker(5, 60*time.Second, silentLogger())
	if !b.Allow("bravo") {
		t.Fatal("fresh breaker should allow")
	}
	if b.State("bravo") != Healthy {
		t.Errorf("expected Healthy, got %s", b.State("bravo"))
	}
}

func TestBreaker_TripsAtThreshold(t *testing.T) {
	b := NewBreaker(5, 60*time.Second, silentLogger())
	clk := newClock(time.Now())
	b.SetClock(clk.Now)

	for i := 0; i < 4; i++ {
		b.RecordFailure("bravo", errors.New("boom"))
		if b.State("bravo") != Healthy {
			t.Fatalf("after %d failures, state should still be Healthy, got %s", i+1, b.State("bravo"))
		}
		if !b.Allow("bravo") {
			t.Fatalf("after %d failures, Allow should still be true", i+1)
		}
	}

	b.RecordFailure("bravo", errors.New("boom"))
	if b.State("bravo") != Tripped {
		t.Fatalf("after 5th failure, state should be Tripped, got %s", b.State("bravo"))
	}
	if b.Allow("bravo") {
		t.Fatal("tripped breaker should not allow")
	}
	if b.DegradedCount() != 1 {
		t.Errorf("DegradedCount = %d, want 1", b.DegradedCount())
	}
}

func TestBreaker_HalfOpenAfterCooldown_Recovers(t *testing.T) {
	b := NewBreaker(3, 60*time.Second, silentLogger())
	clk := newClock(time.Now())
	b.SetClock(clk.Now)

	for i := 0; i < 3; i++ {
		b.RecordFailure("charlie", errors.New("nope"))
	}
	if b.State("charlie") != Tripped {
		t.Fatal("expected Tripped after 3 failures")
	}
	if b.Allow("charlie") {
		t.Fatal("tripped breaker should not allow before cooldown")
	}

	clk.Advance(59 * time.Second)
	if b.Allow("charlie") {
		t.Fatal("should still be tripped at 59s")
	}

	clk.Advance(2 * time.Second)
	if !b.Allow("charlie") {
		t.Fatal("breaker should promote to half_open after cooldown")
	}
	if b.State("charlie") != HalfOpen {
		t.Fatalf("expected HalfOpen, got %s", b.State("charlie"))
	}

	b.RecordSuccess("charlie")
	if b.State("charlie") != Healthy {
		t.Fatalf("expected Healthy after probe success, got %s", b.State("charlie"))
	}
	if b.DegradedCount() != 0 {
		t.Errorf("DegradedCount = %d, want 0", b.DegradedCount())
	}
}

func TestBreaker_HalfOpenFailureResetsCooldown(t *testing.T) {
	b := NewBreaker(2, 60*time.Second, silentLogger())
	clk := newClock(time.Now())
	b.SetClock(clk.Now)

	b.RecordFailure("e2e", errors.New("dial tcp"))
	b.RecordFailure("e2e", errors.New("dial tcp"))
	if b.State("e2e") != Tripped {
		t.Fatal("expected Tripped")
	}

	clk.Advance(61 * time.Second)
	if !b.Allow("e2e") {
		t.Fatal("expected promotion to half_open")
	}

	b.RecordFailure("e2e", errors.New("still down"))
	if b.State("e2e") != Tripped {
		t.Fatalf("half_open + failure should re-trip, got %s", b.State("e2e"))
	}

	clk.Advance(30 * time.Second)
	if b.Allow("e2e") {
		t.Fatal("cooldown should have reset; should still be tripped at 30s past re-trip")
	}

	clk.Advance(35 * time.Second)
	if !b.Allow("e2e") {
		t.Fatal("expected half_open after second cooldown")
	}
}

func TestBreaker_RecordSuccessClearsFailures(t *testing.T) {
	b := NewBreaker(5, 60*time.Second, silentLogger())
	b.RecordFailure("bravo", errors.New("blip"))
	b.RecordFailure("bravo", errors.New("blip"))
	b.RecordSuccess("bravo")

	snap := b.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 snapshot entry, got %d", len(snap))
	}
	if snap[0].ConsecutiveFailures != 0 {
		t.Errorf("consecutiveFails not reset after success: %d", snap[0].ConsecutiveFailures)
	}
	if snap[0].LastError != "" {
		t.Errorf("lastError not cleared after success: %q", snap[0].LastError)
	}
	if snap[0].LastSuccessAt.IsZero() {
		t.Error("lastSuccessAt should be set after RecordSuccess")
	}
}

func TestBreaker_SnapshotIsStableSorted(t *testing.T) {
	b := NewBreaker(5, 60*time.Second, silentLogger())
	b.RecordFailure("echo", errors.New("x"))
	b.RecordSuccess("charlie")
	b.RecordSuccess("alpha")
	b.RecordSuccess("generic")

	snap := b.Snapshot()
	if len(snap) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(snap))
	}
	want := []string{"alpha", "charlie", "echo", "generic"}
	for i, w := range want {
		if snap[i].Name != w {
			t.Errorf("snap[%d].Name = %s, want %s", i, snap[i].Name, w)
		}
	}
}

func TestBreaker_RecordFailureIgnoresNil(t *testing.T) {
	b := NewBreaker(2, 60*time.Second, silentLogger())
	b.RecordFailure("p", nil)
	b.RecordFailure("p", nil)
	if b.State("p") != Healthy {
		t.Fatalf("nil errors must not trip; got %s", b.State("p"))
	}
}

func TestBreaker_DegradedCountExcludesHalfOpen(t *testing.T) {
	b := NewBreaker(2, 30*time.Second, silentLogger())
	clk := newClock(time.Now())
	b.SetClock(clk.Now)

	b.RecordFailure("a", errors.New("x"))
	b.RecordFailure("a", errors.New("x"))
	b.RecordFailure("b", errors.New("y"))
	b.RecordFailure("b", errors.New("y"))
	if b.DegradedCount() != 2 {
		t.Fatalf("expected 2 tripped, got %d", b.DegradedCount())
	}

	clk.Advance(31 * time.Second)
	b.Allow("a") // promotes to half_open
	if b.DegradedCount() != 1 {
		t.Fatalf("half_open must not count as degraded; got %d", b.DegradedCount())
	}
}

// TestBreaker_ConcurrentSafety runs Allow/RecordFailure/RecordSuccess/Snapshot
// in parallel under -race to surface map and counter races.
func TestBreaker_ConcurrentSafety(t *testing.T) {
	b := NewBreaker(50, 10*time.Millisecond, silentLogger())

	const goroutines = 16
	const iters = 500
	var wg sync.WaitGroup
	var ops atomic.Int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			provName := "prov-" + string(rune('a'+id%4))
			for i := 0; i < iters; i++ {
				switch i % 4 {
				case 0:
					b.Allow(provName)
				case 1:
					b.RecordFailure(provName, errors.New("x"))
				case 2:
					b.RecordSuccess(provName)
				case 3:
					_ = b.Snapshot()
				}
				ops.Add(1)
			}
		}(g)
	}
	wg.Wait()
	if ops.Load() != int64(goroutines*iters) {
		t.Errorf("expected %d ops, ran %d", goroutines*iters, ops.Load())
	}
}

func TestBreaker_SheddableTracksCooldown(t *testing.T) {
	clk := newClock(time.Unix(1_700_000_000, 0))
	b := NewBreaker(2, 60*time.Second, silentLogger())
	b.SetClock(clk.Now)

	if b.Sheddable("echo") {
		t.Fatal("unknown/healthy provider must not be sheddable")
	}

	b.RecordFailure("echo", errors.New("boom"))
	b.RecordFailure("echo", errors.New("boom"))
	if b.State("echo") != Tripped {
		t.Fatalf("expected Tripped, got %s", b.State("echo"))
	}
	if !b.Sheddable("echo") {
		t.Fatal("tripped provider inside cooldown must be sheddable")
	}

	clk.Advance(59 * time.Second)
	if !b.Sheddable("echo") {
		t.Fatal("still inside cooldown: must remain sheddable")
	}

	// Cooldown elapsed: the provider is due the Allow call that promotes it to
	// half_open. Sheddable must release even though State still reads Tripped —
	// filtering on raw state here is what wedged prod on 2026-08-30.
	clk.Advance(2 * time.Second)
	if b.Sheddable("echo") {
		t.Fatal("cooldown elapsed: must NOT be sheddable while State is still Tripped")
	}
	if b.State("echo") != Tripped {
		t.Fatalf("Sheddable must be read-only; state changed to %s", b.State("echo"))
	}

	// Half-open (via Allow) is not sheddable either.
	if !b.Allow("echo") {
		t.Fatal("Allow should promote to half_open and admit the probe")
	}
	if b.Sheddable("echo") {
		t.Fatal("half_open provider must not be sheddable")
	}
}
