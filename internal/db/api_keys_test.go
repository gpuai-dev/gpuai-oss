package db_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

func newAPIKeyTestPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_TEST_URL")
	if url == "" {
		t.Skip("DATABASE_TEST_URL not set")
	}
	pool, err := db.NewPool(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return pool
}

// seedAPIKeyOrgUser inserts a minimal org+user pair and returns their UUIDs.
func seedAPIKeyOrgUser(t *testing.T, p *db.Pool) (orgID, userID string) {
	t.Helper()
	ctx := context.Background()
	clerkOrg := fmt.Sprintf("test-org-apikey-%d", time.Now().UnixNano())
	clerkUser := fmt.Sprintf("test-user-apikey-%d", time.Now().UnixNano())
	var err error
	orgID, userID, err = p.EnsureOrgAndUser(ctx, clerkOrg, clerkUser, "apikey-test@example.com", "Test", "", "")
	if err != nil {
		t.Fatalf("seed org+user: %v", err)
	}
	return orgID, userID
}

func TestCreateAndLookupAPIKey(t *testing.T) {
	p := newAPIKeyTestPool(t)
	defer p.Close()
	ctx := context.Background()
	orgID, userID := seedAPIKeyOrgUser(t, p)

	k := &db.APIKey{
		OrgID:     orgID,
		UserID:    userID,
		Name:      "test-key",
		KeyHash:   fmt.Sprintf("h%063d", time.Now().UnixNano()%1e10),
		KeyPrefix: "gpuai_live_xyz1",
		Last4:     "abcd",
		Scopes:    json.RawMessage(`{"full_access":true}`),
	}
	if err := p.CreateAPIKey(ctx, k); err != nil {
		t.Fatalf("create: %v", err)
	}
	if k.APIKeyID == "" || k.CreatedAt.IsZero() {
		t.Fatalf("CreateAPIKey did not populate api_key_id/created_at")
	}
	if k.RateLimitRPS != 100 || k.RateLimitBurst != 200 {
		t.Errorf("default rate limits: got %d/%d, want 100/200", k.RateLimitRPS, k.RateLimitBurst)
	}

	got, err := p.LookupAPIKeyByHash(ctx, k.KeyHash)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.APIKeyID != k.APIKeyID {
		t.Errorf("api_key_id: got %q, want %q", got.APIKeyID, k.APIKeyID)
	}
	if got.OrgID != orgID || got.UserID != userID {
		t.Errorf("org/user: got %s/%s, want %s/%s", got.OrgID, got.UserID, orgID, userID)
	}
}

func TestTouchAPIKeyLastUsed(t *testing.T) {
	p := newAPIKeyTestPool(t)
	defer p.Close()
	ctx := context.Background()
	orgID, userID := seedAPIKeyOrgUser(t, p)

	k := &db.APIKey{
		OrgID: orgID, UserID: userID, Name: "touch-test",
		KeyHash:   fmt.Sprintf("t%063d", time.Now().UnixNano()%1e10),
		KeyPrefix: "gpuai_live_ttt1", Last4: "tchd",
		Scopes: json.RawMessage(`{"full_access":true}`),
	}
	if err := p.CreateAPIKey(ctx, k); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := p.TouchAPIKeyLastUsed(ctx, k.APIKeyID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, err := p.LookupAPIKeyByHash(ctx, k.KeyHash)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.LastUsedAt == nil {
		t.Fatal("LastUsedAt still nil after Touch")
	}
	if time.Since(*got.LastUsedAt) > 5*time.Second {
		t.Errorf("LastUsedAt seems stale: %v", got.LastUsedAt)
	}
}
