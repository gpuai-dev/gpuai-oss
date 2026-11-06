package db

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// provision_failures.go owns the write side of the append-only provision-failure
// ledger (v82). Two callers write to it — the provisioning engine (pre-instance
// rejections, where no instance row exists yet) and SetInstanceError (failures a
// created instance ran into) — and the admin Key Metrics read consumes it.
//
// The ledger replaces the current-state `instances.status='error'` read the
// failures panel used to run on. That read could not survive instance cleanup
// and could not see a launch rejected before the INSERT — the loudest sourcing
// signal there is. Events here are immutable and windowed on occurred_at, so the
// series is a true history.
//
// PRIVACY (INFPRIV-06): ProvisionFailureEvent.Reason carries raw engine/adapter
// error text and CAN name an upstream provider. It is backend-only. Nothing that
// reads this table for the admin browser may select it — see
// ProvisionFailuresByGPUModel, which returns counts and gpu_type only.

// provisionFailureReasonMaxLen bounds the stored reason. Provider errors can
// carry an entire upstream response body; the column exists for operator
// debugging, not archival, and the admin read never selects it.
const provisionFailureReasonMaxLen = 500

// provisionFailureRetention is how long events are kept. Long enough for
// year-over-year sourcing comparisons, short enough that the table stays small
// without a dedicated reaper.
const provisionFailureRetention = 365 * 24 * time.Hour

// ProvisionFailureEvent is one user-facing provisioning failure.
//
// OrgID and InstanceID are OPTIONAL: an empty string inserts as SQL NULL. That
// distinction is load-bearing for InstanceID — NULL means "rejected before any
// instance row existed", which is precisely the class of failure the old
// instances-table read was blind to.
//
// StockRelated is decided by the writer (via StockRelatedReason) rather than
// re-derived at read time, so the classification is pinned to the error text as
// it actually was, not to whatever the patterns look like months later.
type ProvisionFailureEvent struct {
	GPUType      string
	GPUCount     int
	OrgID        string
	InstanceID   string
	StockRelated bool
	Reason       string
}

// StockRelatedReason reports whether a failure reason says the platform had
// nothing to give, as opposed to the launch having broken.
//
// It matches the same two engine sentinels #595's SQL matched with ILIKE —
// ErrNoProvider ("no suitable provider available") and ErrOfferingUnavailable
// ("selected machine is no longer available…") — case-insensitively, and is the
// ONE classifier every write path shares so the engine hook and the
// SetInstanceError hook can never disagree.
//
// Treat the result as a LOWER BOUND on stock failures, not a classification:
// reasons are free text, and an adapter can report an out-of-capacity condition
// in wording neither pattern covers.
func StockRelatedReason(reason string) bool {
	r := strings.ToLower(reason)
	return strings.Contains(r, "no suitable provider") ||
		strings.Contains(r, "no longer available")
}

// truncateReason clips a reason to provisionFailureReasonMaxLen RUNES (not
// bytes), so a cut never lands mid-UTF-8 and stores a broken sequence.
func truncateReason(reason string) string {
	runes := []rune(reason)
	if len(runes) <= provisionFailureReasonMaxLen {
		return reason
	}
	return string(runes[:provisionFailureReasonMaxLen])
}

// RecordProvisionFailure appends one failure event, then opportunistically
// prunes past the retention horizon.
//
// Callers treat this as BEST-EFFORT — it rides customer-facing provisioning
// paths, so the returned error is meant to be logged by the caller and dropped,
// never surfaced or retried. A nil Pool returns nil so a DB-less engine (tests,
// degraded boot) records nothing instead of panicking.
//
// The prune runs only after a SUCCESSFUL insert and its error is swallowed
// (logged, not returned): retention is housekeeping, and failing to trim must
// never make a recorded failure look unrecorded. Same prune-at-write pattern as
// PruneAvailabilitySnapshots.
func (p *Pool) RecordProvisionFailure(ctx context.Context, ev ProvisionFailureEvent) error {
	if p == nil || p.pool == nil {
		return nil
	}

	gpuCount := ev.GPUCount
	if gpuCount < 1 {
		gpuCount = 1
	}

	// NULLIF maps the zero value to SQL NULL: an unattributed event still counts
	// toward failures while staying out of COUNT(DISTINCT org_id), and a NULL
	// instance_id is the pre-instance-rejection marker.
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO provision_failure_events
			(gpu_type, gpu_count, org_id, instance_id, stock_related, reason)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6)`,
		ev.GPUType, gpuCount, ev.OrgID, ev.InstanceID, ev.StockRelated, truncateReason(ev.Reason),
	); err != nil {
		return err
	}

	if _, err := p.pool.Exec(ctx,
		`DELETE FROM provision_failure_events WHERE occurred_at < $1`,
		time.Now().Add(-provisionFailureRetention),
	); err != nil {
		slog.Warn("provision failure ledger: retention prune failed", "error", err)
	}
	return nil
}
