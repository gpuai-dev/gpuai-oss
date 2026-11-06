package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// StageInstanceBootstrap stores the rendered bootstrap script for an instance
// so the instance can fetch it at boot (GET /internal/instances/{id}/bootstrap).
// Upsert: a provision retry for the same instance_id replaces the script.
// Opportunistically sweeps rows older than 24h — by then the instance either
// booted (row deleted on the running transition) or was reaped, and the sweep
// here avoids dedicated janitor machinery.
func (p *Pool) StageInstanceBootstrap(ctx context.Context, instanceID, script string) error {
	if _, err := p.pool.Exec(ctx,
		`DELETE FROM instance_bootstraps WHERE created_at < now() - interval '24 hours'`); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx,
		`INSERT INTO instance_bootstraps (instance_id, script)
		 VALUES ($1, $2)
		 ON CONFLICT (instance_id) DO UPDATE SET script = EXCLUDED.script, created_at = now()`,
		instanceID, script)
	return err
}

// GetStagedBootstrap returns the staged bootstrap script for an instance.
// Returns ErrNotFound when nothing is staged (already consumed+deleted, swept,
// or never staged).
func (p *Pool) GetStagedBootstrap(ctx context.Context, instanceID string) (string, error) {
	var script string
	err := p.pool.QueryRow(ctx,
		`SELECT script FROM instance_bootstraps WHERE instance_id = $1`,
		instanceID,
	).Scan(&script)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return script, nil
}

// DeleteStagedBootstrap removes the staged bootstrap for an instance.
// Idempotent — deleting an absent row is not an error.
func (p *Pool) DeleteStagedBootstrap(ctx context.Context, instanceID string) error {
	_, err := p.pool.Exec(ctx,
		`DELETE FROM instance_bootstraps WHERE instance_id = $1`, instanceID)
	return err
}
