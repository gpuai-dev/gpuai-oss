package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// SetInstanceReadinessFailure records that the startup GPU compute check failed
// on the instance's current placement. It only lands while the instance is
// still pre-running: once running, the check has already been passed (or was
// in shadow mode) and the row must not be rewound. updated reports whether the
// row changed.
func (p *Pool) SetInstanceReadinessFailure(ctx context.Context, instanceID, reason string) (updated bool, err error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE instances
		    SET readiness_failure = $2, readiness_failed_at = NOW(), updated_at = NOW()
		  WHERE instance_id = $1
		    AND status IN ('creating', 'provisioning', 'booting')`,
		instanceID, reason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// TakeInstanceReadinessFailure returns a pending readiness failure and clears
// it in the same statement, so exactly one poll loop acts on it even when two
// gpuctl tasks race. Empty means nothing is pending. Only a row the poll loop
// is still driving (provisioning or booting) yields one: the verdict belongs to
// the placement being booted, and a row that has moved on (running, errored,
// terminated) must never be acted on. One recorded at 'creating' waits for the
// poll loop's first transition. The verdict is also cleared whenever the row
// leaves that placement (RebindInstanceUpstream, SetInstanceRunning).
func (p *Pool) TakeInstanceReadinessFailure(ctx context.Context, instanceID string) (string, error) {
	var reason string
	err := p.pool.QueryRow(ctx,
		`WITH pending AS (
			SELECT readiness_failure FROM instances
			 WHERE instance_id = $1 AND readiness_failure IS NOT NULL
			   AND status IN ('provisioning', 'booting')
			 FOR UPDATE
		)
		UPDATE instances
		   SET readiness_failure = NULL, readiness_failed_at = NULL
		  FROM pending
		 WHERE instances.instance_id = $1
		RETURNING pending.readiness_failure`,
		instanceID).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return reason, err
}
