package db

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

// events_rescued_db_test.go covers the v87 'rescued' instance_events value
// (GPU-89) against a REAL Postgres, because the two facts worth proving both
// live in the schema:
//
//  1. the CHECK constraint accepts 'rescued' after v87 AND still rejects an
//     unlisted type — the GPU-50 gotcha was a migration that widened a value
//     list so far it stopped constraining anything, so "the insert worked" is
//     only half the assertion;
//  2. the org-scoped catch-up read hides 'rescued' while the admin per-instance
//     read shows it (T-q89-01) — a filter that exists only in Go source is not
//     evidence; this runs the actual query.
//
// Skips cleanly when DATABASE_TEST_URL is unset (CI sets it).

// rescueEventFixture seeds an org + user + instance and returns the pool, the
// org id, and the instance id, registering cleanup that deletes the events
// first (instance_events → instances is ON DELETE RESTRICT).
func rescueEventFixture(t *testing.T, ctx context.Context, url, tag string) (*Pool, string, string) {
	t.Helper()
	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	stamp := time.Now().Format("20060102150405.000000000")
	orgID, userID, err := pool.EnsureOrgAndUser(ctx,
		"test_org_"+tag+"_"+stamp, "test_user_"+tag+"_"+stamp,
		tag+"@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org+user: %v", err)
	}
	instID := uniqueInstanceTestID(t)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.pool.Exec(bg, `DELETE FROM instance_events WHERE org_id = $1`, orgID)
		_, _ = pool.pool.Exec(bg, `DELETE FROM instances WHERE org_id = $1`, orgID)
		_, _ = pool.pool.Exec(bg, `DELETE FROM organizations WHERE organization_id = $1`, orgID)
	})

	if err := pool.CreateInstance(ctx, &Instance{
		InstanceID: instID, OrgID: orgID, UserID: userID,
		UpstreamProvider: "stub", UpstreamID: "u-" + instID,
		Hostname: instID + ".example.com",
		GPUType:  "H100_SXM", GPUCount: 1, Tier: "on_demand", Region: "us-east",
		PricePerHour: 2.50, UpstreamPricePerHour: 2.00, Status: "running",
	}); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	return pool, orgID, instID
}

// TestCreateInstanceEvent_RescuedAllowedByCheck proves v87 widened the
// event_type CHECK by exactly one value: 'rescued' inserts, 'bogus' still
// fails with SQLSTATE 23514 (check_violation).
func TestCreateInstanceEvent_RescuedAllowedByCheck(t *testing.T) {
	url := os.Getenv("DATABASE_TEST_URL")
	if url == "" {
		t.Skip("DATABASE_TEST_URL not set")
	}
	ctx := context.Background()
	pool, orgID, instID := rescueEventFixture(t, ctx, url, "rescuedcheck")

	meta, err := json.Marshal(map[string]any{
		"verdict":                     "dead-pull",
		"old_provider":                "stub",
		"old_upstream_id":             "u-old",
		"old_upstream_price_per_hour": 1.0,
		"new_provider":                "backup",
		"new_upstream_id":             "u-new",
		"new_upstream_price_per_hour": 2.0,
		"retail_price_per_hour":       1.5,
	})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	ev := &InstanceEvent{InstanceID: instID, OrgID: orgID, EventType: "rescued", Metadata: meta}
	if err := pool.CreateInstanceEvent(ctx, ev); err != nil {
		t.Fatalf("insert 'rescued' event must succeed after v87: %v", err)
	}
	if ev.EventID == "" || ev.CreatedAt.IsZero() {
		t.Errorf("insert must return event_id + created_at, got %q / %v", ev.EventID, ev.CreatedAt)
	}

	// The constraint must still CONSTRAIN — a widened-to-uselessness CHECK is
	// the failure mode this half exists to catch.
	bogus := &InstanceEvent{InstanceID: instID, OrgID: orgID, EventType: "bogus"}
	err = pool.CreateInstanceEvent(ctx, bogus)
	if err == nil {
		t.Fatal("an unlisted event_type must still be rejected by the CHECK constraint")
	}
	var pgErr interface{ SQLState() string }
	if !errors.As(err, &pgErr) || pgErr.SQLState() != "23514" {
		t.Fatalf("want SQLSTATE 23514 (check_violation), got %v", err)
	}
}
