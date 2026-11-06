package availability

import (
	"testing"
	"time"
)

// feed replays a launch outcome sequence against a breaker. true = failure.
//
// It honours Allow, because the engine does: a suppressed SKU is never launched
// and therefore produces NO outcome. Recording outcomes a suppressed key could
// not have generated would let post-trip successes heal a key that never got
// probed.
func feed(b *Breaker, key string, outcomes []bool) {
	for _, f := range outcomes {
		if !b.Allow(key) {
			continue
		}
		if f {
			b.RecordFailure(key, errBoom)
		} else {
			b.RecordSuccess(key)
		}
	}
}

// seq builds a steady-state sequence with the failures spread EVENLY rather
// than front-loaded. Real traffic is burstier, but a steady stream is the right
// model for "does this long-run rate trip the rule": front-loading would make
// every shape trip on its opening run and prove nothing about the rate.
func seq(fails, total int) []bool {
	out := make([]bool, 0, total)
	acc := 0
	for i := 0; i < total; i++ {
		acc += fails
		if acc >= total {
			acc -= total
			out = append(out, true)
		} else {
			out = append(out, false)
		}
	}
	return out
}

func newLaunchBreaker() *Breaker {
	return NewRateBreaker(DefaultLaunchBreakerWindow, DefaultLaunchBreakerMinSamples,
		DefaultLaunchBreakerFailRate, DefaultLaunchBreakerCooldown, nil)
}

// TestRateBreaker_TripsMeasuredDeadShapes replays the shapes measured at
// 79-100% boot failure on prod over the 14 days to 2026-09-17. Every one must
// be suppressed.
func TestRateBreaker_TripsMeasuredDeadShapes(t *testing.T) {
	cases := []struct {
		name         string
		fails, total int
	}{
		{"provider A l40 x4 (10/10)", 10, 10},
		{"provider A b200 x8 (5/5)", 5, 5},
		{"provider A rtx_a6000 x2 (10/10)", 10, 10},
		{"provider A h100_pcie x1 (8/9)", 8, 9},
		{"provider A l40 x8 (4/5)", 4, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newLaunchBreaker()
			feed(b, "k", seq(c.fails, c.total))
			if b.Allow("k") {
				t.Errorf("%s must be suppressed: %d/%d failures is far above the 16.8%% fleet baseline", c.name, c.fails, c.total)
			}
		})
	}
}

// TestRateBreaker_SparesSellableShapes is the regression test for what this
// rule actually fixes. The consecutive-3 rule it replaces suppressed all of
// these — including a marketplace SKU with 594 launches and a 72% success rate, which
// tripped after 29. Suppressing sellable supply is lost revenue.
func TestRateBreaker_SparesSellableShapes(t *testing.T) {
	cases := []struct {
		name         string
		fails, total int
	}{
		{"provider A l40 x1 (2/46, 4.3%)", 2, 46},
		{"provider C rtx_5090 x1 (166/594, 27.9%)", 166, 594},
		{"provider C rtx_3090 x2 (14/46, 30.4%)", 14, 46},
		{"provider B a100_80gb x8 (22/60, 36.7%)", 22, 60},
		{"provider C rtx_5090 x4 (14/37, 37.8%)", 14, 37},
		{"provider B a100_80gb x4 (19/30, 63.3%)", 19, 30},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newLaunchBreaker()
			feed(b, "k", seq(c.fails, c.total))
			if !b.Allow("k") {
				t.Errorf("%s must keep selling: %d/%d is below the 0.70 trip rate", c.name, c.fails, c.total)
			}
		})
	}
}

// TestRateBreaker_SuccessDoesNotResetWindow pins the semantic difference from
// consecutive mode. RecordSuccess there set consecutiveFails to 0 and forced
// state back to Healthy; here a success while healthy is just another sample.
func TestRateBreaker_SuccessDoesNotResetWindow(t *testing.T) {
	b := newLaunchBreaker()
	// 4 failures, then a success, then one more failure: 5 of 6 = 83% >= 0.70.
	feed(b, "k", []bool{true, true, true, true, false, true})
	if b.Allow("k") {
		t.Error("an interleaved success must not wipe the window — that is what made the old rule unable to see a mostly-failing SKU")
	}
}

// TestRateBreaker_RespectsMinSamples — a key must not be judged on too little
// evidence. Three failures is a 100% rate but only 3 samples.
func TestRateBreaker_RespectsMinSamples(t *testing.T) {
	b := newLaunchBreaker()
	feed(b, "k", []bool{true, true, true})
	if !b.Allow("k") {
		t.Errorf("3 failures is below minSamples=%d and must not trip — tripping at 3 is exactly the over-suppression being removed", DefaultLaunchBreakerMinSamples)
	}
	b.RecordFailure("k", errBoom) // 4/4
	b.RecordFailure("k", errBoom) // 5/5 -> reaches minSamples at 100%
	if b.Allow("k") {
		t.Error("5 consecutive failures is 100% over minSamples and must trip")
	}
}

// TestRateBreaker_HalfOpenProbe covers both probe outcomes.
func TestRateBreaker_HalfOpenProbe(t *testing.T) {
	now := time.Now()
	b := newLaunchBreaker()
	b.SetClock(func() time.Time { return now })
	feed(b, "k", seq(5, 5))
	if b.Allow("k") {
		t.Fatal("want tripped")
	}

	now = now.Add(DefaultLaunchBreakerCooldown + time.Second)
	if !b.Allow("k") {
		t.Fatal("cooldown elapsed — want a half-open probe admitted")
	}
	b.RecordFailure("k", errBoom)
	if b.Allow("k") {
		t.Error("a failed probe must re-trip immediately and restart the cooldown")
	}

	now = now.Add(DefaultLaunchBreakerCooldown + time.Second)
	if !b.Allow("k") {
		t.Fatal("want a second half-open probe")
	}
	b.RecordSuccess("k")
	if !b.Allow("k") {
		t.Error("a successful probe must heal the key")
	}
	// The pre-trip failures must not survive the heal, or one later failure
	// would re-trip the key on history that has already been re-tested.
	b.RecordFailure("k", errBoom)
	if !b.Allow("k") {
		t.Error("a healed key must start a fresh window, not inherit its pre-trip failures")
	}
}

// TestRateBreaker_DoesNotChangeConsecutiveMode — the availability poller and the
// public request breaker still use NewBreaker, and this change must be invisible
// to them.
func TestRateBreaker_DoesNotChangeConsecutiveMode(t *testing.T) {
	b := NewBreaker(3, time.Minute, nil)
	b.RecordFailure("p", errBoom)
	b.RecordFailure("p", errBoom)
	b.RecordSuccess("p") // consecutive mode: resets to healthy
	b.RecordFailure("p", errBoom)
	b.RecordFailure("p", errBoom)
	if !b.Allow("p") {
		t.Error("consecutive mode must still reset on success — the poller depends on it")
	}
	b.RecordFailure("p", errBoom)
	if b.Allow("p") {
		t.Error("consecutive mode must still trip on 3 in a row")
	}
}

// TestRateBreaker_ZeroArgsUseDerivedDefaults — config passes 0 for "unset" so
// the derived numbers live in exactly one place.
func TestRateBreaker_ZeroArgsUseDerivedDefaults(t *testing.T) {
	b := NewRateBreaker(0, 0, 0, 0, nil)
	if b.rate == nil {
		t.Fatal("rate policy must be set")
	}
	if b.rate.window != DefaultLaunchBreakerWindow ||
		b.rate.minSamples != DefaultLaunchBreakerMinSamples ||
		b.rate.failRate != DefaultLaunchBreakerFailRate {
		t.Errorf("zero args must fall back to the derived defaults, got %+v", *b.rate)
	}
}
