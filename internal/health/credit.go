package health

import (
	"context"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// Dead-window auto-credit (paying customers back for a tunnel outage) is a
// billing feature and is not part of GPU.ai Open Source: a self-hosted
// deployment has no customer balance to credit. The reachability tracker
// still detects, announces and escalates outages exactly as before; these
// hooks keep its call sites intact and do nothing.

// creditWindowStart resolves T0 for an outage window, preferring the DURABLE
// flag. instances.unreachable_since is written once per outage (first announce
// wins) and survives a gpuctl restart; the tracker's firstFailAt does not.
func creditWindowStart(inst *db.Instance, trackerT0 time.Time) time.Time {
	if inst.UnreachableSince != nil {
		return inst.UnreachableSince.UTC()
	}
	return trackerT0
}

// queueRecoveryCredit is a no-op: there is no balance to credit.
func (m *Monitor) queueRecoveryCredit(_ *db.Instance, _, _ time.Time, _ int) {}

// creditEvictedEpisodes is a no-op: there is no balance to credit.
func (m *Monitor) creditEvictedEpisodes(_ context.Context, _ []evictedEpisode) {}

// settlePendingRecoveryCredits is a no-op: there is no balance to credit.
func (m *Monitor) settlePendingRecoveryCredits(_ context.Context) {}
