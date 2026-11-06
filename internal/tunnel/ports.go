package tunnel

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AllocatePort allocates an FRP remote port within rng. It must be called
// within a transaction.
//
// The range is a PARAMETER, not the MinPort/MaxPort constants it used to read,
// so the allocator and the frps gateway are driven by the same configured
// string and cannot silently disagree — see PortRange for what that divergence
// cost. A zero rng falls back to DefaultPortRange. The function
// acquires a PostgreSQL advisory lock (transaction-scoped) to serialize
// allocations, then walks a monotonic round-robin cursor, returning the first
// candidate port not currently held by an active instance.
//
// Round-robin (not MAX(active)+1) is deliberate. A terminated instance keeps
// its frp_remote_port (only the status filter / partial unique index excludes
// it), and the shared frps gateway can still hold that port after the DB row
// is marked terminated — a lingering frpc registration whose control
// connection has not been reaped, or a zombie VM whose tunnel is still alive.
// Handing the very next instance a just-freed port therefore races the
// gateway: the new frpc fails to bind with "port already used", its SSH tunnel
// never establishes, and a customer connecting to that port is routed to the
// OLD instance. Cycling the cursor across the whole range means a freed port
// is not reused until every other port has been tried first, by which point
// the gateway has released it.
//
// The caller is responsible for storing the allocated port in the instance row
// within the same transaction; the v7 partial unique index
// instances_frp_remote_port_active is the final backstop against a race.
func AllocatePort(ctx context.Context, tx pgx.Tx, rng PortRange) (int, error) {
	rng = rng.OrDefault()
	// Acquire transaction-scoped advisory lock to serialize allocations.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", portLockID); err != nil {
		return 0, fmt.Errorf("tunnel: acquire port lock: %w", err)
	}

	rangeSize := rng.Size()

	// Advance the round-robin cursor and accept the first candidate that no
	// active instance currently holds. Bounded by rangeSize so an exhausted
	// range terminates instead of looping forever.
	for i := 0; i < rangeSize; i++ {
		var cursor int64
		if err := tx.QueryRow(ctx, "SELECT nextval('frp_remote_port_cursor')").Scan(&cursor); err != nil {
			return 0, fmt.Errorf("tunnel: advance port cursor: %w", err)
		}
		// nextval is 1-based and monotonic; map it across the range.
		port := rng.Min + int((cursor-1)%int64(rangeSize))

		var inUse bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (
			   SELECT 1 FROM instances
			   WHERE frp_remote_port = $1 AND status NOT IN ('terminated', 'error')
			 )`,
			port,
		).Scan(&inUse); err != nil {
			return 0, fmt.Errorf("tunnel: check port availability: %w", err)
		}
		if !inUse {
			return port, nil
		}
	}

	return 0, fmt.Errorf("tunnel: port range exhausted (%s)", rng)
}
