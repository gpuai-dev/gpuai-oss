package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// autoTerminateTestPool connects the shared test database and seeds an org+user
// for one test, cleaning both up afterwards. Skips cleanly when
// DATABASE_TEST_URL is unset (the same contract as every other test in this
// package): the auto_terminate SQL is real SQL — the deadline arithmetic and
// the claim's atomicity live in Postgres, not in Go — so it is verified against
// a real database or not at all.
func autoTerminateTestPool(t *testing.T, tag string) (*Pool, string, string) {
	t.Helper()
	url := os.Getenv("DATABASE_TEST_URL")
	if url == "" {
		t.Skip("DATABASE_TEST_URL not set")
	}
	ctx := context.Background()
	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	stamp := time.Now().Format("20060102150405.000000000")
	orgID, userID, err := pool.EnsureOrgAndUser(ctx,
		"test_org_"+tag+"_"+stamp, "test_user_"+tag+"_"+stamp, tag+"@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org+user: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.pool.Exec(bg, `DELETE FROM instance_events WHERE org_id = $1`, orgID)
		_, _ = pool.pool.Exec(bg, `DELETE FROM instances WHERE org_id = $1`, orgID)
		_, _ = pool.pool.Exec(bg, `DELETE FROM organizations WHERE organization_id = $1`, orgID)
	})
	return pool, orgID, userID
}

func seedAutoTerminateInstance(t *testing.T, pool *Pool, orgID, userID string, hours *int) string {
	t.Helper()
	id := uniqueInstanceTestID(t)
	if err := pool.CreateInstance(context.Background(), &Instance{
		InstanceID: id, OrgID: orgID, UserID: userID,
		UpstreamProvider: "stub", UpstreamID: "u-" + id,
		Hostname: id + ".example.com",
		GPUType:  "H100_SXM", GPUCount: 1, Tier: "on_demand", Region: "us-east",
		PricePerHour: 2.50, UpstreamPricePerHour: 2.00, Status: "booting",
		AutoTerminateHours: hours,
	}); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	return id
}

// TestSetInstanceRunning_ArmsAutoTerminateDeadline is the GPU-60 arming guard:
// the deadline is computed at the RUNNING transition as ready_at + the
// requested hours — not at create time — so a slow provisioning window (which
// the customer is never billed for) cannot eat into the runtime they asked for.
//
// The nil case is the other half of the contract: an instance that asked for no
// bound must come out of the same statement with auto_terminate_at still NULL,
// which is what keeps every pre-feature launch byte-for-byte unchanged.
func TestSetInstanceRunning_ArmsAutoTerminateDeadline(t *testing.T) {
	pool, orgID, userID := autoTerminateTestPool(t, "autoterm_arm")
	ctx := context.Background()

	hours := 3
	bounded := seedAutoTerminateInstance(t, pool, orgID, userID, &hours)
	unbounded := seedAutoTerminateInstance(t, pool, orgID, userID, nil)

	// Before the transition neither row has a deadline: create records only the
	// request.
	pre, err := pool.GetInstance(ctx, bounded)
	if err != nil {
		t.Fatalf("get bounded pre-running: %v", err)
	}
	if pre.AutoTerminateAt != nil {
		t.Fatalf("auto_terminate_at set before the instance was running: %v", pre.AutoTerminateAt)
	}
	if pre.AutoTerminateHours == nil || *pre.AutoTerminateHours != hours {
		t.Fatalf("auto_terminate_hours = %v, want %d", pre.AutoTerminateHours, hours)
	}

	for _, id := range []string{bounded, unbounded} {
		updated, _, err := pool.SetInstanceRunning(ctx, id, "")
		if err != nil {
			t.Fatalf("SetInstanceRunning(%s): %v", id, err)
		}
		if !updated {
			t.Fatalf("SetInstanceRunning(%s) reported no update", id)
		}
	}

	got, err := pool.GetInstance(ctx, bounded)
	if err != nil {
		t.Fatalf("get bounded: %v", err)
	}
	if got.ReadyAt == nil || got.AutoTerminateAt == nil {
		t.Fatalf("ready_at = %v, auto_terminate_at = %v; want both set", got.ReadyAt, got.AutoTerminateAt)
	}
	want := got.ReadyAt.Add(time.Duration(hours) * time.Hour)
	if delta := got.AutoTerminateAt.Sub(want); delta > time.Second || delta < -time.Second {
		t.Errorf("auto_terminate_at = %v, want ready_at+%dh (%v), off by %v",
			got.AutoTerminateAt, hours, want, delta)
	}

	none, err := pool.GetInstance(ctx, unbounded)
	if err != nil {
		t.Fatalf("get unbounded: %v", err)
	}
	if none.AutoTerminateAt != nil {
		t.Errorf("auto_terminate_at = %v on an instance that requested no bound; want NULL", none.AutoTerminateAt)
	}
}

// TestClaimInstanceAutoTerminate_ExactlyOnce is the exactly-once guard the
// sweep's safety rests on: the first claim of an armed, elapsed deadline wins;
// every repeat loses, because the claim clears the very column its predicate
// requires. A not-yet-due instance is never claimable, and the requested hours
// survive the claim so a read after the fact still shows what was asked for.
func TestClaimInstanceAutoTerminate_ExactlyOnce(t *testing.T) {
	pool, orgID, userID := autoTerminateTestPool(t, "autoterm_claim")
	ctx := context.Background()

	hours := 1
	id := seedAutoTerminateInstance(t, pool, orgID, userID, &hours)
	if _, _, err := pool.SetInstanceRunning(ctx, id, ""); err != nil {
		t.Fatalf("SetInstanceRunning: %v", err)
	}

	// Not due yet — the deadline is an hour out.
	now := time.Now().UTC()
	due, err := pool.ListInstancesDueForAutoTerminate(ctx, now, 0)
	if err != nil {
		t.Fatalf("ListInstancesDueForAutoTerminate: %v", err)
	}
	for _, inst := range due {
		if inst.InstanceID == id {
			t.Fatalf("instance %s selected as due an hour before its deadline", id)
		}
	}
	if claimed, err := pool.ClaimInstanceAutoTerminate(ctx, id, now); err != nil || claimed {
		t.Fatalf("ClaimInstanceAutoTerminate before the deadline = (%v, %v), want (false, nil)", claimed, err)
	}

	// Past the deadline: selected once, claimed once, then never again.
	after := now.Add(2 * time.Hour)
	due, err = pool.ListInstancesDueForAutoTerminate(ctx, after, 0)
	if err != nil {
		t.Fatalf("ListInstancesDueForAutoTerminate (due): %v", err)
	}
	var found bool
	for _, inst := range due {
		if inst.InstanceID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("instance %s not selected after its deadline elapsed", id)
	}
	claimed, err := pool.ClaimInstanceAutoTerminate(ctx, id, after)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !claimed {
		t.Fatal("first claim lost; want won")
	}
	claimed, err = pool.ClaimInstanceAutoTerminate(ctx, id, after)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Fatal("second claim won; the deadline must be claimable exactly once")
	}

	// A claimed row drops out of the due set (it has no deadline any more) but
	// keeps the customer's requested hours.
	due, err = pool.ListInstancesDueForAutoTerminate(ctx, after, 0)
	if err != nil {
		t.Fatalf("ListInstancesDueForAutoTerminate (post-claim): %v", err)
	}
	for _, inst := range due {
		if inst.InstanceID == id {
			t.Fatalf("claimed instance %s still selected as due", id)
		}
	}
	got, err := pool.GetInstance(ctx, id)
	if err != nil {
		t.Fatalf("get post-claim: %v", err)
	}
	if got.AutoTerminateAt != nil {
		t.Errorf("auto_terminate_at = %v after a won claim; want NULL", got.AutoTerminateAt)
	}
	if got.AutoTerminateHours == nil || *got.AutoTerminateHours != hours {
		t.Errorf("auto_terminate_hours = %v after a won claim; want %d preserved", got.AutoTerminateHours, hours)
	}
}

// TestRearmInstanceAutoTerminate_GuardsExactlyOnce covers the retry path's
// safety: a claim spent on a terminate that failed early can be put back, but
// the re-arm must never resurrect a deadline on a row that has already left
// 'running' (that would be a terminated instance carrying a live limit) and
// must never overwrite a deadline someone else already restored.
func TestRearmInstanceAutoTerminate_GuardsExactlyOnce(t *testing.T) {
	pool, orgID, userID := autoTerminateTestPool(t, "autoterm_rearm")
	ctx := context.Background()

	hours := 1
	id := seedAutoTerminateInstance(t, pool, orgID, userID, &hours)
	if _, _, err := pool.SetInstanceRunning(ctx, id, ""); err != nil {
		t.Fatalf("SetInstanceRunning: %v", err)
	}
	after := time.Now().UTC().Add(2 * time.Hour)
	claimed, err := pool.ClaimInstanceAutoTerminate(ctx, id, after)
	if err != nil || !claimed {
		t.Fatalf("claim = (%v, %v), want (true, nil)", claimed, err)
	}

	// The spent claim is restorable, verbatim.
	deadline := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	rearmed, err := pool.RearmInstanceAutoTerminate(ctx, id, deadline)
	if err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	if !rearmed {
		t.Fatal("re-arm reported no change on a running row with a spent claim")
	}
	got, err := pool.GetInstance(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AutoTerminateAt == nil || !got.AutoTerminateAt.UTC().Equal(deadline) {
		t.Fatalf("auto_terminate_at = %v, want the restored %v", got.AutoTerminateAt, deadline)
	}

	// A second re-arm cannot overwrite the deadline now standing.
	other := deadline.Add(5 * time.Hour)
	rearmed, err = pool.RearmInstanceAutoTerminate(ctx, id, other)
	if err != nil {
		t.Fatalf("second re-arm: %v", err)
	}
	if rearmed {
		t.Error("re-arm overwrote a deadline that was already restored")
	}

	// Once the row leaves 'running', a re-arm is inert — a terminated instance
	// must never carry a live deadline.
	if _, err := pool.TerminateInstance(ctx, id); err != nil {
		t.Fatalf("TerminateInstance: %v", err)
	}
	if _, err := pool.ClaimInstanceAutoTerminate(ctx, id, after); err != nil {
		t.Fatalf("claim on terminated row: %v", err)
	}
	rearmed, err = pool.RearmInstanceAutoTerminate(ctx, id, deadline)
	if err != nil {
		t.Fatalf("re-arm on terminated row: %v", err)
	}
	if rearmed {
		t.Error("re-armed a deadline on a terminated instance")
	}
}

// TestListInstancesDueForAutoTerminate_LimitIsOldestFirst pins the per-tick
// cap's shape: a bounded batch, ordered by deadline, so the most overdue
// instance is always in it and no instance can be starved by a backlog.
func TestListInstancesDueForAutoTerminate_LimitIsOldestFirst(t *testing.T) {
	pool, orgID, userID := autoTerminateTestPool(t, "autoterm_limit")
	ctx := context.Background()

	hours := 1
	ids := make([]string, 3)
	for i := range ids {
		ids[i] = seedAutoTerminateInstance(t, pool, orgID, userID, &hours)
		if _, _, err := pool.SetInstanceRunning(ctx, ids[i], ""); err != nil {
			t.Fatalf("SetInstanceRunning: %v", err)
		}
		// Stagger the deadlines so "oldest first" is observable: ids[0] is the
		// most overdue.
		if _, err := pool.pool.Exec(ctx,
			`UPDATE instances SET auto_terminate_at = NOW() - ($2::int * INTERVAL '1 minute')
			  WHERE instance_id = $1`, ids[i], 30-i*10); err != nil {
			t.Fatalf("stagger deadline: %v", err)
		}
	}

	now := time.Now().UTC()
	batch, err := pool.ListInstancesDueForAutoTerminate(ctx, now, 2)
	if err != nil {
		t.Fatalf("ListInstancesDueForAutoTerminate: %v", err)
	}
	// Other orgs' rows may share this database, so assert on OUR ids only via
	// the ordering property: the batch is capped, and it is sorted.
	if len(batch) > 2 {
		t.Fatalf("limit ignored: got %d rows, want at most 2", len(batch))
	}
	for i := 1; i < len(batch); i++ {
		if batch[i].AutoTerminateAt.Before(*batch[i-1].AutoTerminateAt) {
			t.Fatalf("batch is not oldest-deadline-first: %v then %v",
				batch[i-1].AutoTerminateAt, batch[i].AutoTerminateAt)
		}
	}
}

// TestInstances_AutoTerminateHoursCheckConstraint proves the column itself
// refuses a 0 (and a negative). The API already rejects both, but the column is
// what the arming arithmetic reads: a 0 persisted by some future writer — a
// backfill, an admin tool, a psql session — would arm NOW() + 0 hours and kill
// the instance seconds after it became usable, for a limit nobody expressed.
func TestInstances_AutoTerminateHoursCheckConstraint(t *testing.T) {
	pool, orgID, userID := autoTerminateTestPool(t, "autoterm_check")
	ctx := context.Background()

	for _, bad := range []int{0, -1, 721} {
		hours := bad
		err := pool.CreateInstance(ctx, &Instance{
			InstanceID: uniqueInstanceTestID(t), OrgID: orgID, UserID: userID,
			UpstreamProvider: "stub", UpstreamID: "u-bad",
			Hostname: "bad.example.com", GPUType: "H100_SXM", GPUCount: 1,
			Tier: "on_demand", Region: "us-east",
			PricePerHour: 1, UpstreamPricePerHour: 1, Status: "booting",
			AutoTerminateHours: &hours,
		})
		if err == nil {
			t.Errorf("auto_terminate_hours = %d was accepted; the CHECK must refuse it", bad)
		}
	}
}
