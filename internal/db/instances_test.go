package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"
)

// uniqueInstanceTestID returns a unique instance_id matching the engine's
// generateInstanceID format ("gpu-" + 8 hex chars = 12 chars). Mirrors the
// helper in internal/api/handlers_resume_test.go so seeded rows fit the
// instances.instance_id VARCHAR(12) column.
func uniqueInstanceTestID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "gpu-" + hex.EncodeToString(b)
}

// TestInstanceUnreachableSinceWriters pins the two halves of the GPU-77 status
// flag (schema v88):
//
//   - SetInstanceUnreachableSince is FIRST-ANNOUNCE-WINS. T0 is the timestamp
//     the credit window and the credit's idempotency reference are both derived
//     from, so a later write must not be able to move it — otherwise a monitor
//     restart mid-outage would shorten the window AND mint a second reference
//     for the same outage, defeating the unique index.
//   - ClearInstanceUnreachableSince returns the row to reachable, which is what
//     drops the "unreachable" status off the API read.
//
// Skips when DATABASE_TEST_URL is unset (needs migrations through v88).
func TestInstanceUnreachableSinceWriters(t *testing.T) {
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
		"test_org_unreach_"+stamp, "test_user_unreach_"+stamp, "unreach@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org+user: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.pool.Exec(bg, `DELETE FROM instances WHERE org_id = $1`, orgID)
		_, _ = pool.pool.Exec(bg, `DELETE FROM organizations WHERE organization_id = $1`, orgID)
	})

	instID := uniqueInstanceTestID(t)
	if err := pool.CreateInstance(ctx, &Instance{
		InstanceID: instID, OrgID: orgID, UserID: userID,
		UpstreamProvider: "stub", UpstreamID: "u-" + instID,
		Hostname: instID + ".example.com",
		GPUType:  "H100_SXM", GPUCount: 1, Tier: "on_demand", Region: "us-east",
		PricePerHour: 2.50, UpstreamPricePerHour: 2.00, Status: "running",
	}); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	// A fresh instance is never flagged.
	inst, err := pool.GetInstance(ctx, instID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if inst.UnreachableSince != nil {
		t.Fatalf("fresh instance UnreachableSince = %v, want nil", inst.UnreachableSince)
	}

	t0 := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Millisecond)
	if err := pool.SetInstanceUnreachableSince(ctx, instID, t0); err != nil {
		t.Fatalf("SetInstanceUnreachableSince: %v", err)
	}
	inst, err = pool.GetInstance(ctx, instID)
	if err != nil {
		t.Fatalf("get instance after set: %v", err)
	}
	if inst.UnreachableSince == nil || !inst.UnreachableSince.UTC().Equal(t0) {
		t.Fatalf("UnreachableSince = %v, want %v", inst.UnreachableSince, t0)
	}

	// Second announce (e.g. after a monitor restart re-confirms the same dead
	// tunnel) must NOT move T0.
	later := t0.Add(15 * time.Minute)
	if err := pool.SetInstanceUnreachableSince(ctx, instID, later); err != nil {
		t.Fatalf("second SetInstanceUnreachableSince: %v", err)
	}
	inst, err = pool.GetInstance(ctx, instID)
	if err != nil {
		t.Fatalf("get instance after second set: %v", err)
	}
	if inst.UnreachableSince == nil || !inst.UnreachableSince.UTC().Equal(t0) {
		t.Fatalf("UnreachableSince after re-announce = %v, want the ORIGINAL %v "+
			"(first announce must win — T0 anchors the credit window and its idempotency key)",
			inst.UnreachableSince, t0)
	}

	if err := pool.ClearInstanceUnreachableSince(ctx, instID); err != nil {
		t.Fatalf("ClearInstanceUnreachableSince: %v", err)
	}
	inst, err = pool.GetInstance(ctx, instID)
	if err != nil {
		t.Fatalf("get instance after clear: %v", err)
	}
	if inst.UnreachableSince != nil {
		t.Fatalf("UnreachableSince after clear = %v, want nil", inst.UnreachableSince)
	}

	// A fresh outage after recovery gets its own T0.
	t2 := time.Now().UTC().Truncate(time.Millisecond)
	if err := pool.SetInstanceUnreachableSince(ctx, instID, t2); err != nil {
		t.Fatalf("SetInstanceUnreachableSince after clear: %v", err)
	}
	inst, err = pool.GetInstance(ctx, instID)
	if err != nil {
		t.Fatalf("get instance after re-set: %v", err)
	}
	if inst.UnreachableSince == nil || !inst.UnreachableSince.UTC().Equal(t2) {
		t.Fatalf("UnreachableSince for the new episode = %v, want %v", inst.UnreachableSince, t2)
	}
}
