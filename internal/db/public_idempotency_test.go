package db

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

// uniquePublicIdemSuffix returns a per-test suffix so concurrent runs do not
// collide on (api_key_id, idempotency_key) PK rows. Mirrors the
// uniqueAuditPrefix pattern used elsewhere in this package.
func uniquePublicIdemSuffix() string {
	return time.Now().Format("20060102150405.000000000")
}

// seedPublicIdemAPIKey inserts an api_keys row directly (no DB helper exists
// for api_keys yet — Plan 18-11 ships the dashboard CRUD). Returns the
// generated api_key_id and registers cleanup that cascades the org row.
func seedPublicIdemAPIKey(t *testing.T, ctx context.Context, pool *Pool, prefix string) (apiKeyID, orgID, userID string) {
	t.Helper()
	suffix := uniquePublicIdemSuffix()
	clerkOrgID := "test_org_" + prefix + "_" + suffix
	clerkUserID := "test_user_" + prefix + "_" + suffix
	o, u, err := pool.EnsureOrgAndUser(ctx, clerkOrgID, clerkUserID, prefix+"@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org+user: %v", err)
	}
	t.Cleanup(func() {
		// Cascade through organizations → api_keys → public_api_idempotency_keys.
		_, _ = pool.pool.Exec(context.Background(),
			`DELETE FROM organizations WHERE organization_id = $1`, o)
	})

	// Direct SQL insert — no Go helper exists for api_keys yet.
	keyHash := "00000000000000000000000000000000000000000000000000000000" + suffix[len(suffix)-8:]
	// key_hash is CHAR(64); pad/truncate to exactly 64 chars.
	keyHash = (keyHash + "0000000000000000000000000000000000000000000000000000000000000000")[:64]
	var keyID string
	err = pool.pool.QueryRow(ctx, `
		INSERT INTO api_keys (org_id, user_id, name, key_hash, key_prefix, last4, scopes)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		RETURNING api_key_id`,
		o, u, "test-"+prefix, keyHash, "gpuai_test_", "test", `{"instances":"write"}`,
	).Scan(&keyID)
	if err != nil {
		t.Fatalf("seed api_key: %v", err)
	}
	return keyID, o, u
}

// TestPublicIdempotency_CreateGetComplete walks Create → Get (in-flight) →
// Complete → Get (replay) on a single (api_key_id, key) pair.
func TestPublicIdempotency_CreateGetComplete(t *testing.T) {
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

	apiKeyID, _, _ := seedPublicIdemAPIKey(t, ctx, pool, "ccc")
	key := "key_" + uniquePublicIdemSuffix()
	hash := "abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234"

	// Create row (in-flight).
	if err := pool.CreatePublicIdempotencyKey(ctx, apiKeyID, key, hash); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Get returns row with response_code NULL.
	got, err := pool.GetPublicIdempotencyKey(ctx, apiKeyID, key)
	if err != nil {
		t.Fatalf("get in-flight: %v", err)
	}
	if got.ResponseCode != nil {
		t.Fatalf("expected ResponseCode nil (in-flight); got %v", *got.ResponseCode)
	}
	if got.RequestHash != hash {
		t.Errorf("RequestHash = %q, want %q", got.RequestHash, hash)
	}

	// Complete with response.
	hdrs := json.RawMessage(`{"Operation-Id":"op_123"}`)
	body := []byte(`{"id":"thing_42"}`)
	if err := pool.CompletePublicIdempotencyKey(ctx, apiKeyID, key, 201, body, hdrs); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Get returns row with response_code, response_body, response_headers populated.
	got, err = pool.GetPublicIdempotencyKey(ctx, apiKeyID, key)
	if err != nil {
		t.Fatalf("get completed: %v", err)
	}
	if got.ResponseCode == nil || *got.ResponseCode != 201 {
		t.Errorf("ResponseCode: got %v, want 201", got.ResponseCode)
	}
	if string(got.ResponseBody) != string(body) {
		t.Errorf("ResponseBody: got %q, want %q", string(got.ResponseBody), string(body))
	}
	var hMap map[string]string
	if err := json.Unmarshal(got.ResponseHeaders, &hMap); err != nil {
		t.Fatalf("unmarshal headers: %v", err)
	}
	if hMap["Operation-Id"] != "op_123" {
		t.Errorf("Operation-Id: got %q, want op_123", hMap["Operation-Id"])
	}
}

// TestPublicIdempotency_DuplicateKey verifies a second insert on the same
// (api_key_id, idempotency_key) pair returns ErrIdempotencyKeyExists (PK 23505).
func TestPublicIdempotency_DuplicateKey(t *testing.T) {
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

	apiKeyID, _, _ := seedPublicIdemAPIKey(t, ctx, pool, "dup")
	key := "key_" + uniquePublicIdemSuffix()
	hash := "1111111111111111111111111111111111111111111111111111111111111111"

	if err := pool.CreatePublicIdempotencyKey(ctx, apiKeyID, key, hash); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err = pool.CreatePublicIdempotencyKey(ctx, apiKeyID, key, hash)
	if !errors.Is(err, ErrIdempotencyKeyExists) {
		t.Fatalf("second create: got %v, want ErrIdempotencyKeyExists", err)
	}
}

// TestPublicIdempotency_OrgIsolation: two api_keys (same org) using the same
// idempotency_key string must not collide — the PK is api_key_id-scoped.
func TestPublicIdempotency_OrgIsolation(t *testing.T) {
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

	keyA, orgA, userA := seedPublicIdemAPIKey(t, ctx, pool, "isoA")
	// Insert a SECOND api_key in the SAME org so we prove same-org / different-key
	// idempotency entries are independent.
	suffix := uniquePublicIdemSuffix()
	keyHash := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	keyHash = (keyHash[:56] + suffix[len(suffix)-8:])[:64]
	var keyB string
	err = pool.pool.QueryRow(ctx, `
		INSERT INTO api_keys (org_id, user_id, name, key_hash, key_prefix, last4, scopes)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		RETURNING api_key_id`,
		orgA, userA, "test-isoB", keyHash, "gpuai_test_", "isoB", `{"instances":"write"}`,
	).Scan(&keyB)
	if err != nil {
		t.Fatalf("seed second api_key in same org: %v", err)
	}

	idemKey := "shared_key_" + uniquePublicIdemSuffix()
	hashA := "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
	hashB := "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222"

	if err := pool.CreatePublicIdempotencyKey(ctx, keyA, idemKey, hashA); err != nil {
		t.Fatalf("create on keyA: %v", err)
	}
	if err := pool.CreatePublicIdempotencyKey(ctx, keyB, idemKey, hashB); err != nil {
		t.Fatalf("create on keyB (same org, same idem key): %v", err)
	}

	gotA, err := pool.GetPublicIdempotencyKey(ctx, keyA, idemKey)
	if err != nil {
		t.Fatalf("get keyA: %v", err)
	}
	if gotA.RequestHash != hashA {
		t.Errorf("keyA hash = %q, want %q (cross-key contamination)", gotA.RequestHash, hashA)
	}
	gotB, err := pool.GetPublicIdempotencyKey(ctx, keyB, idemKey)
	if err != nil {
		t.Fatalf("get keyB: %v", err)
	}
	if gotB.RequestHash != hashB {
		t.Errorf("keyB hash = %q, want %q", gotB.RequestHash, hashB)
	}
}

// TestPublicIdempotency_Get24hWindow verifies query-time 24h filter:
// even without running Prune, a row older than 24h reads back as ErrNotFound.
func TestPublicIdempotency_Get24hWindow(t *testing.T) {
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

	apiKeyID, _, _ := seedPublicIdemAPIKey(t, ctx, pool, "win")
	key := "key_" + uniquePublicIdemSuffix()
	hash := "3333333333333333333333333333333333333333333333333333333333333333"

	if err := pool.CreatePublicIdempotencyKey(ctx, apiKeyID, key, hash); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Backdate to 25h ago — outside the 24h window.
	if _, err := pool.pool.Exec(ctx,
		`UPDATE public_api_idempotency_keys SET created_at = NOW() - INTERVAL '25 hours'
		 WHERE api_key_id = $1 AND idempotency_key = $2`, apiKeyID, key,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	_, err = pool.GetPublicIdempotencyKey(ctx, apiKeyID, key)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on >24h row: got %v, want ErrNotFound (query-time filter)", err)
	}
}
