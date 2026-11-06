package db

// Tests for the Phase 39 role seam (SEC-01, SEC-02): normalizeClerkRole is a
// pure unit test; the EnsureOrgAndUser role-sync and GetUserRole/UpdateUserRole
// tests are DB-gated (skip without DATABASE_TEST_URL, like every other db
// integration test).
//
// This file is in package db (not db_test) because normalizeClerkRole is
// unexported.

import (
	"context"
	"os"
	"strings"
	"testing"
)

func newRoleTestPool(t *testing.T) *Pool {
	t.Helper()
	url := os.Getenv("DATABASE_TEST_URL")
	if url == "" {
		t.Skip("DATABASE_TEST_URL not set")
	}
	pool, err := NewPool(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return pool
}

// ---------------------------------------------------------------------------
// A WR-08: a malformed id is "no such user", not "I could not tell"
// ---------------------------------------------------------------------------

// TestOrganizationsSource_MalformedIDIsNotFound is the no-Postgres half of the
// contract, and it is the half that runs on every PR.
//
// Both lookups key on the users.user_id UUID column, so an empty or non-UUID id
// fails the cast inside Postgres and arrives as SQLSTATE 22P02 rather than
// pgx.ErrNoRows. Returning that raw is what let a permanently unresolvable id
// read as a transient failure one layer up (86-REVIEW A WR-08): the identity
// resolver and the fail-closed role gate both split "definitively absent" from
// "could not determine", and a driver error lands on the wrong side of that
// split every time.
//
// Source-level, matching internal/db/finetune_test.go's precedent: the
// behaviour under test is which predicate the method calls, and a live
// Postgres is not required to answer that. The DB-gated proof is below.
func TestOrganizationsSource_MalformedIDIsNotFound(t *testing.T) {
	src, err := os.ReadFile("organizations.go")
	if err != nil {
		t.Fatalf("read organizations.go: %v", err)
	}
	body := string(src)

	for _, fn := range []string{"GetUserRoleByUserID"} {
		start := strings.Index(body, "func (p *Pool) "+fn+"(")
		if start < 0 {
			t.Fatalf("%s not found in organizations.go", fn)
		}
		fnBody := body[start:]
		if end := strings.Index(body[start+1:], "\nfunc "); end >= 0 {
			fnBody = body[start : start+1+end]
		}
		if !strings.Contains(fnBody, "isNoRowsOrInvalidID(err)") {
			t.Errorf("%s must fold a malformed id via isNoRowsOrInvalidID; a raw driver error reads as transient to every caller", fn)
		}
		if strings.Contains(fnBody, "errors.Is(err, pgx.ErrNoRows)") {
			t.Errorf("%s still uses the bare ErrNoRows check; SQLSTATE 22P02 would escape as a driver error", fn)
		}
	}
}
