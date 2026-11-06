package db

import (
	"context"
	"testing"
	"time"
)

// uniqueAuditPrefix returns a token unique enough to identify a test's audit
// rows without colliding across parallel runs. Mirrors the
// uniqueResumeOrgID convention from internal/provision/engine_test.go.
func uniqueAuditPrefix(prefix string) string {
	return prefix + "_" + time.Now().Format("20060102150405.000000000")
}

// seedAuditOrgUser creates an org + user pair via EnsureOrgAndUser and
// schedules an org-cascade DELETE on cleanup. Returns internal UUIDs only.
func seedAuditOrgUser(t *testing.T, ctx context.Context, pool *Pool, prefix string) (orgID, userID string) {
	t.Helper()
	clerkOrgID := uniqueAuditPrefix("test_org_" + prefix)
	clerkUserID := uniqueAuditPrefix("test_user_" + prefix)
	o, u, err := pool.EnsureOrgAndUser(ctx, clerkOrgID, clerkUserID, prefix+"@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org+user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(),
			`DELETE FROM organizations WHERE organization_id = $1`, o)
	})
	return o, u
}

// seedAuditUser creates an additional user inside an existing org and returns
// its internal UUID. Useful for tests that need two users in the same org
// (IDOR / multi-actor scenarios).
func seedAuditUser(t *testing.T, ctx context.Context, pool *Pool, orgID, prefix string) string {
	t.Helper()
	var userID string
	err := pool.pool.QueryRow(ctx, `
		INSERT INTO users (user_id, org_id, clerk_user_id, email, name, role)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, 'member')
		RETURNING user_id`,
		orgID,
		uniqueAuditPrefix("test_user_"+prefix),
		prefix+"@example.com",
		prefix+"-display-name",
	).Scan(&userID)
	if err != nil {
		t.Fatalf("seed extra user: %v", err)
	}
	return userID
}

// insertAuditRow is a low-level seed helper for tests that need control over
// resource_type / resource_id / action / user_id without going through the
// production logAudit path (which we test indirectly elsewhere).
func insertAuditRow(t *testing.T, ctx context.Context, pool *Pool,
	orgID string, userID *string, action, resourceType, resourceID string, createdAt time.Time,
) {
	t.Helper()
	_, err := pool.pool.Exec(ctx, `
		INSERT INTO audit_log (id, org_id, user_id, action, resource_type, resource_id, metadata, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, '{}'::jsonb, $6)`,
		orgID, userID, action, resourceType, resourceID, createdAt,
	)
	if err != nil {
		t.Fatalf("insert audit row: %v", err)
	}
}
