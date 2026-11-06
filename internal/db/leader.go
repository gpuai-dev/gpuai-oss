package db

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AdvisoryLockKeyLifecycle elects the single gpuctl process allowed to run
// the instance-lifecycle sweeper (stuck-provision reaper and per-instance
// auto-terminate). Every replica runs the loop, but only the lease holder
// ticks, so a deadline is acted on once however many replicas run.
// ASCII "GPU_LIFE".
const AdvisoryLockKeyLifecycle int64 = 0x4750555F4C494645

const (
	// leaseKeepaliveInterval is how often the held lease pings its dedicated
	// connection. A leader whose connection died (Postgres restart, network
	// partition) finds out within one interval and stops ticking — well under
	// the 60s tick period, so a split brain lasts at most one tick.
	leaseKeepaliveInterval = 15 * time.Second
	// leaseKeepaliveTimeout bounds a single keepalive ping.
	leaseKeepaliveTimeout = 10 * time.Second
	// leaseCloseTimeout bounds the graceful close of the lease connection.
	leaseCloseTimeout = 5 * time.Second
)

// TickerLease is a held cluster-wide leadership lease.
//
// Done is closed when the lease ends for any reason — Release was called, the
// acquiring context was cancelled, or the underlying connection died (detected
// by keepalive). Once Done is closed the holder MUST stop ticking; another
// process may already hold the lock.
//
// Release ends the lease explicitly. It is idempotent and safe to call
// concurrently; the connection teardown (and therefore the server-side lock
// release) completes asynchronously shortly after.
type TickerLease interface {
	Done() <-chan struct{}
	Release()
}

// AcquireLifecycleLease blocks until this process holds the lifecycle
// sweeper's advisory lock, then returns the held lease. The wait is
// server-side (pg_advisory_lock parks until the current holder's session ends)
// and ctx-cancellable, so a standby replica takes over the instant the old
// holder's connection closes. The lease occupies one dedicated connection from
// the pool for as long as it is held.
func (p *Pool) AcquireLifecycleLease(ctx context.Context) (TickerLease, error) {
	return p.acquireLease(ctx, AdvisoryLockKeyLifecycle)
}

// acquireLease is the lease implementation, parameterized by advisory-lock
// key. Everything below the pg_advisory_lock call is key-agnostic.
func (p *Pool) acquireLease(ctx context.Context, key int64) (TickerLease, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		conn.Release()
		return nil, err
	}
	l := &advisoryLease{
		conn:    conn,
		done:    make(chan struct{}),
		release: make(chan struct{}),
	}
	go l.watch(ctx)
	return l, nil
}

// advisoryLease is the production TickerLease: a dedicated pool connection
// holding pg_advisory_lock, plus a watch goroutine that keepalive-pings it.
// All connection I/O is confined to the watch goroutine so Release can be
// called from any goroutine without racing an in-flight ping.
type advisoryLease struct {
	conn        *pgxpool.Conn
	done        chan struct{} // closed by watch when the lease is over
	release     chan struct{} // closed by Release to request teardown
	releaseOnce sync.Once
}

func (l *advisoryLease) Done() <-chan struct{} { return l.done }

func (l *advisoryLease) Release() {
	l.releaseOnce.Do(func() { close(l.release) })
}

// watch keepalive-pings the lease connection until the lease ends (ctx cancel,
// explicit Release, or ping failure), then destroys the connection and closes
// done. Destroying — not pooling — the connection is what releases the
// advisory lock: pg_advisory_lock is session-scoped, so returning a healthy
// connection to the pool would strand the lock on a pooled session forever.
func (l *advisoryLease) watch(ctx context.Context) {
	defer close(l.done)
	defer l.destroyConn()

	ticker := time.NewTicker(leaseKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.release:
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(context.Background(), leaseKeepaliveTimeout)
			err := l.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				// Connection (and with it the lock) is gone — end the lease so
				// the holder stops ticking.
				return
			}
		}
	}
}

// destroyConn ends the underlying Postgres session. Hijack detaches the
// connection from the pool (it will never be handed out again) and Close
// terminates the session; even if the graceful Terminate times out, the
// socket closes and the server frees the advisory lock.
func (l *advisoryLease) destroyConn() {
	pgConn := l.conn.Hijack()
	closeCtx, cancel := context.WithTimeout(context.Background(), leaseCloseTimeout)
	defer cancel()
	_ = pgConn.Close(closeCtx)
}
