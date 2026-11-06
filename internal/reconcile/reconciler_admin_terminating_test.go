package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// TestSkipAdminTerminating proves the reconciler does NOT act on an instance
// that an admin force-terminate has freshly CLAIMED via
// instances.admin_terminating_at (RECON-03 / D-04).
//
// The primary guard is the DB predicate in ListReconcilableInstances (a
// freshly-claimed row is never returned), which is exercised against a live
// PostgreSQL pool in CI. Here we exercise the in-loop defense-in-depth guard
// isAdminClaimed: even if a claimed row leaks through the read seam (e.g. a
// claim set after the query ran), RunOnce must skip it so the running reaper
// cannot misclassify the in-flight instance as a GHOST and double-terminate it
// (Pitfall 2).
func TestSkipAdminTerminating(t *testing.T) {
	now := time.Now().UTC()

	// A row shaped exactly like a GHOST (active DB status, upstream reports
	// not-found) so that, absent the claim, the reconciler WOULD fire the
	// Terminator. The fresh admin_terminating_at claim must suppress that.
	freshClaim := now.Add(-1 * time.Minute) // well inside the 15-minute window
	claimedRow := db.Instance{
		InstanceID:         ourInstanceID,
		UpstreamProvider:   testProviderName,
		UpstreamID:         "up-claimed",
		Status:             "running",
		Region:             "us",
		CreatedAt:          now.Add(-10 * time.Minute), // older than minInstanceAge
		UpdatedAt:          now.Add(-10 * time.Minute),
		AdminTerminatingAt: &freshClaim,
	}
	makeProv := func() *fakeProvider {
		return &fakeProvider{
			statusByUpstream: map[string]fakeStatus{
				"up-claimed": {status: nil, err: errors.New("instance not found")},
			},
		}
	}

	t.Run("fresh_claim_is_skipped", func(t *testing.T) {
		term := &fakeTerminator{}
		// Enforce mode, GHOST enabled: the ONLY reason the terminator stays
		// untouched is the admin-terminating claim.
		r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{claimedRow}}, makeProv(), term, false /* dryRun */, false /* ghostDryRun */)
		r.RunOnce(context.Background())
		if got := term.called(); len(got) != 0 {
			t.Fatalf("fresh admin claim: terminator called %v; want none (reconciler must skip the in-flight force-terminate)", got)
		}
		snap := r.Snapshot()
		if snap.Ghosts != 0 || snap.GhostsActed != 0 {
			t.Fatalf("fresh admin claim: Ghosts=%d GhostsActed=%d; want both 0 (claimed row must not be classified)", snap.Ghosts, snap.GhostsActed)
		}
	})

	t.Run("stale_claim_self_heals", func(t *testing.T) {
		// A claim older than adminTerminatingClaimWindow is no longer honored:
		// a crashed force-terminate must not pin the instance out of
		// reconciliation forever (T-48-12). Such a row is acted on normally.
		staleClaim := now.Add(-(adminTerminatingClaimWindow + time.Minute))
		staleRow := claimedRow
		staleRow.AdminTerminatingAt = &staleClaim

		term := &fakeTerminator{}
		r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{staleRow}}, makeProv(), term, false /* dryRun */, false /* ghostDryRun */)
		r.RunOnce(context.Background())
		got := term.called()
		if len(got) != 1 || got[0] != ourInstanceID {
			t.Fatalf("stale admin claim: terminator calls = %v; want [%s] (stale claim must self-heal)", got, ourInstanceID)
		}
	})
}
