package db

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// events_preemption_db_test.go covers the v91 'preemption_warned' /
// 'preemption_reclaimed' instance_events values (GPU-50) against a REAL
// Postgres. The source-only drift guard (instance_events_constraint_test.go)
// proves the migration and the Go writers agree on the value list; this proves
// the migrated database actually accepts the inserts, which is the half a
// regex over .sql files cannot claim.
//
// Skips cleanly when DATABASE_TEST_URL is unset (CI sets it).
//
// Fixture note: rescueEventFixture (events_rescued_db_test.go) is the shared
// org+user+instance seeder for instance_events tests — its name records where
// it was introduced, not an exclusive scope.

// TestCreateInstanceEvent_PreemptionAllowedByCheck proves v91 widened the
// event_type CHECK by exactly the two spot-preemption values the watcher emits,
// and that the widened constraint still constrains.
//
// It also pins the second half of the GPU-50 bug: recordEvent hands the SAME
// struct to the SSE callback that the INSERT was supposed to populate via
// RETURNING, so a rejected insert published an event with an empty event_id and
// a zero created_at. Asserting both fields come back non-zero is what makes a
// successful insert distinguishable from the silent failure.
func TestCreateInstanceEvent_PreemptionAllowedByCheck(t *testing.T) {
	url := os.Getenv("DATABASE_TEST_URL")
	if url == "" {
		t.Skip("DATABASE_TEST_URL not set")
	}
	ctx := context.Background()
	pool, orgID, instID := rescueEventFixture(t, ctx, url, "preemptcheck")

	for _, eventType := range []string{"preemption_warned", "preemption_reclaimed"} {
		meta, err := json.Marshal(map[string]string{
			"reason":   "spot_" + eventType,
			"gpu_type": "H100_SXM",
			"region":   "us-east",
			"tier":     "spot",
		})
		if err != nil {
			t.Fatalf("marshal metadata: %v", err)
		}
		ev := &InstanceEvent{InstanceID: instID, OrgID: orgID, EventType: eventType, Metadata: meta}
		if err := pool.CreateInstanceEvent(ctx, ev); err != nil {
			t.Fatalf("insert %q must succeed after v91: %v", eventType, err)
		}
		if ev.EventID == "" || ev.CreatedAt.IsZero() {
			t.Errorf("insert of %q must return event_id + created_at (the SSE callback republishes this "+
				"struct — zero values are the GPU-50 malformed-event symptom), got %q / %v",
				eventType, ev.EventID, ev.CreatedAt)
		}
	}

	// The constraint must still CONSTRAIN — a widened-to-uselessness CHECK is
	// the failure mode this half exists to catch.
	bogus := &InstanceEvent{InstanceID: instID, OrgID: orgID, EventType: "preemption_bogus"}
	err := pool.CreateInstanceEvent(ctx, bogus)
	if err == nil {
		t.Fatal("an unlisted event_type must still be rejected by the CHECK constraint")
	}
	var pgErr interface{ SQLState() string }
	if !errors.As(err, &pgErr) || pgErr.SQLState() != "23514" {
		t.Fatalf("want SQLSTATE 23514 (check_violation), got %v", err)
	}
}
