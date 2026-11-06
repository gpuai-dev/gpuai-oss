package db

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// A readiness failure lands only on a pre-running row, and exactly one taker
// gets it (two gpuctl tasks run the poll loop machinery; the callback can land
// on either). Skips cleanly when DATABASE_TEST_URL is unset.
func TestInstanceReadinessFailure_SetAndTakeOnce(t *testing.T) {
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
	orgID, userID, err := pool.EnsureOrgAndUser(ctx, "test_org_readiness_"+stamp, "test_user_readiness_"+stamp, "r@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org+user: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.pool.Exec(bg, `DELETE FROM instances WHERE org_id = $1`, orgID)
		_, _ = pool.pool.Exec(bg, `DELETE FROM organizations WHERE organization_id = $1`, orgID)
	})
	mk := func(status string) string {
		id := uniqueInstanceTestID(t)
		if err := pool.CreateInstance(ctx, &Instance{
			InstanceID: id, OrgID: orgID, UserID: userID,
			UpstreamProvider: "stub", UpstreamID: "u-" + id, Hostname: id + ".example.com",
			GPUType: "L40S", GPUCount: 1, Tier: "on_demand", Region: "us-east",
			PricePerHour: 0.9, UpstreamPricePerHour: 0.7, Status: status,
		}); err != nil {
			t.Fatalf("create %s instance: %v", status, err)
		}
		return id
	}

	booting := mk("booting")
	const reason = "host: cuInit returned 999, nvidia-uvm open: Input/output error"
	if ok, err := pool.SetInstanceReadinessFailure(ctx, booting, reason); err != nil || !ok {
		t.Fatalf("set on booting row: ok=%v err=%v", ok, err)
	}

	// Concurrent takers: exactly one sees the reason.
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := pool.TakeInstanceReadinessFailure(ctx, booting)
			if err != nil {
				t.Errorf("take: %v", err)
			}
			got[i] = r
		}(i)
	}
	wg.Wait()
	seen := 0
	for _, r := range got {
		if r == reason {
			seen++
		} else if r != "" {
			t.Errorf("unexpected reason %q", r)
		}
	}
	if seen != 1 {
		t.Fatalf("%d takers saw the verdict; want exactly 1", seen)
	}
	if r, _ := pool.TakeInstanceReadinessFailure(ctx, booting); r != "" {
		t.Errorf("verdict not cleared: %q", r)
	}

	// A running row (already billed or passed) is never rewound.
	running := mk("running")
	if ok, err := pool.SetInstanceReadinessFailure(ctx, running, reason); err != nil || ok {
		t.Errorf("set on running row: ok=%v err=%v; want no change", ok, err)
	}
	if r, _ := pool.TakeInstanceReadinessFailure(ctx, running); r != "" {
		t.Errorf("running row carries a verdict: %q", r)
	}
}

// A verdict belongs to the placement being booted. It must not survive the row
// moving to a replacement host (a rescue rebind) or to running, and it is only
// taken while the poll loop is driving the row. Before this, a verdict about a
// released host could be taken against its healthy replacement.
func TestInstanceReadinessFailure_TiedToPlacement(t *testing.T) {
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
	orgID, userID, err := pool.EnsureOrgAndUser(ctx, "test_org_readiness_pl_"+stamp, "test_user_readiness_pl_"+stamp, "rp@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org+user: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.pool.Exec(bg, `DELETE FROM instances WHERE org_id = $1`, orgID)
		_, _ = pool.pool.Exec(bg, `DELETE FROM organizations WHERE organization_id = $1`, orgID)
	})
	const reason = "host: cuInit returned 999"
	mk := func(status string) string {
		id := uniqueInstanceTestID(t)
		if err := pool.CreateInstance(ctx, &Instance{
			InstanceID: id, OrgID: orgID, UserID: userID,
			UpstreamProvider: "stub", UpstreamID: "u-" + id, Hostname: id + ".example.com",
			GPUType: "L40S", GPUCount: 1, Tier: "on_demand", Region: "us-east",
			PricePerHour: 0.9, UpstreamPricePerHour: 0.7, Status: status,
		}); err != nil {
			t.Fatalf("create %s instance: %v", status, err)
		}
		if ok, err := pool.SetInstanceReadinessFailure(ctx, id, reason); err != nil || !ok {
			t.Fatalf("set on %s row: ok=%v err=%v", status, ok, err)
		}
		return id
	}
	pending := func(id string) bool {
		var n int
		if err := pool.pool.QueryRow(ctx,
			`SELECT count(*) FROM instances WHERE instance_id = $1 AND (readiness_failure IS NOT NULL OR readiness_failed_at IS NOT NULL)`,
			id).Scan(&n); err != nil {
			t.Fatalf("read verdict: %v", err)
		}
		return n > 0
	}

	// Rescue rebind to a replacement host clears the old host's verdict.
	rebound := mk("booting")
	if ok, err := pool.RebindInstanceUpstream(ctx, rebound, "stub", "u-replacement", "us-east", 0.9, 0.7, nil); err != nil || !ok {
		t.Fatalf("rebind: ok=%v err=%v", ok, err)
	}
	if pending(rebound) {
		t.Error("rebind kept the old host's verdict")
	}
	if r, _ := pool.TakeInstanceReadinessFailure(ctx, rebound); r != "" {
		t.Errorf("verdict taken against the replacement host: %q", r)
	}

	// Running clears it.
	ran := mk("booting")
	if ok, _, err := pool.SetInstanceRunning(ctx, ran, ""); err != nil || !ok {
		t.Fatalf("set running: ok=%v err=%v", ok, err)
	}
	if pending(ran) {
		t.Error("a running row still carries a verdict")
	}

	// Recorded at creating: not taken until the poll loop drives the row.
	early := mk("creating")
	if r, _ := pool.TakeInstanceReadinessFailure(ctx, early); r != "" {
		t.Errorf("verdict taken at creating: %q", r)
	}
	if ok, err := pool.UpdateInstanceStatus(ctx, early, "creating", "provisioning"); err != nil || !ok {
		t.Fatalf("advance to provisioning: ok=%v err=%v", ok, err)
	}
	if r, _ := pool.TakeInstanceReadinessFailure(ctx, early); r != reason {
		t.Errorf("provisioning row: took %q; want %q", r, reason)
	}

	// A row that left the boot (errored) is never acted on.
	gone := mk("booting")
	if ok, err := pool.UpdateInstanceStatus(ctx, gone, "booting", "error"); err != nil || !ok {
		t.Fatalf("advance to error: ok=%v err=%v", ok, err)
	}
	if r, _ := pool.TakeInstanceReadinessFailure(ctx, gone); r != "" {
		t.Errorf("verdict taken on an errored row: %q", r)
	}
}
