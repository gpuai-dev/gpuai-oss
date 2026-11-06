package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestAcquireLifecycleLease exercises the advisory-lock lease against a
// real Postgres: mutual exclusion (a second acquire blocks until cancelled),
// release-and-takeover (releasing frees the lock for the next acquirer), and
// Done closing after Release. Skips when DATABASE_TEST_URL is unset.
func TestAcquireLifecycleLease(t *testing.T) {
	url := os.Getenv("DATABASE_TEST_URL")
	if url == "" {
		t.Skip("DATABASE_TEST_URL not set")
	}
	ctx := context.Background()
	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Holder acquires immediately (nobody holds the key).
	acquireCtx, cancelAcquire := context.WithTimeout(ctx, 10*time.Second)
	defer cancelAcquire()
	l1, err := pool.AcquireLifecycleLease(acquireCtx)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// A second acquire must BLOCK while the lease is held — the server-side
	// pg_advisory_lock wait is what makes takeover instantaneous. Cancel it
	// after 500ms and expect an error, not a lease.
	blockedCtx, cancelBlocked := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancelBlocked()
	if l2, err := pool.AcquireLifecycleLease(blockedCtx); err == nil {
		l2.Release()
		t.Fatal("second acquire succeeded while the lease was held; advisory lock is not exclusive")
	}

	// Release the holder: Done must close and the lock must free, so a fresh
	// acquire succeeds promptly (the lease connection is torn down async).
	l1.Release()
	select {
	case <-l1.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("lease Done did not close after Release")
	}
	takeoverCtx, cancelTakeover := context.WithTimeout(ctx, 10*time.Second)
	defer cancelTakeover()
	l3, err := pool.AcquireLifecycleLease(takeoverCtx)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	l3.Release()
	select {
	case <-l3.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("takeover lease Done did not close after Release")
	}
}
