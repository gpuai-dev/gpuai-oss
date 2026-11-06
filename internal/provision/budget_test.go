package provision

import (
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// TestProvisioningBudget verifies the per-instance poll budget: it scales from
// the provider's create→ready estimate with 2× headroom, never drops below the
// engine default floor, and falls back to the floor when no estimate is given.
// The baremetal case (15 min estimate → 30 min budget) is the one that the old
// flat 10 min would have reaped mid-boot.
func TestProvisioningBudget(t *testing.T) {
	e := &Engine{provisionTimeout: 10 * time.Minute}
	cases := []struct {
		name string
		est  int
		want time.Duration
	}{
		{"no estimate falls back to floor", 0, 10 * time.Minute},
		{"negative falls back to floor", -5, 10 * time.Minute},
		{"fast instant boot stays at floor", 60, 10 * time.Minute},
		{"virtual (2m) stays at floor", 120, 10 * time.Minute},
		{"baremetal (15m) gets 2x headroom", 900, 30 * time.Minute},
	}
	for _, c := range cases {
		if got := e.provisioningBudget(c.est); got != c.want {
			t.Errorf("%s: provisioningBudget(%d) = %s, want %s", c.name, c.est, got, c.want)
		}
	}
}

// TestReapBackstopExceedsEveryProviderBudget enforces the reapStuckProvisionAfter
// sizing contract that used to live only in a comment: the durable flat
// backstop must exceed every registered provider's per-instance provisioning
// budget (2× its create→ready estimate), or the backstop preempts legitimately
// slow boots the watchdog would have allowed. This is exactly what happened
// live once: a 20-min estimate yields a 40-min budget, and the
// then-30-min backstop reaped a healthy T4 launch at minute 32.
func TestReapBackstopExceedsEveryProviderBudget(t *testing.T) {
	e := &Engine{provisionTimeout: 10 * time.Minute}
	for _, name := range []string{"generic", "vultr"} {
		est := int(provider.ProvisioningEstimate(name).Seconds())
		budget := e.provisioningBudget(est)
		if reapStuckProvisionAfter <= budget {
			t.Errorf("reapStuckProvisionAfter (%s) must exceed %s's provisioning budget (%s) — raise the backstop when adding a slower boot class",
				reapStuckProvisionAfter, name, budget)
		}
	}
}
