package db

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// APIKey is the public-API key record (Phase 18, migration v13).
// The raw key is NEVER stored — only the SHA-256 hex digest in KeyHash.
type APIKey struct {
	APIKeyID       string
	OrgID          string
	UserID         string
	Name           string
	KeyHash        string          // 64-char SHA-256 hex
	KeyPrefix      string          // safe to log / display
	Last4          string          // for ID-in-list UX
	Scopes         json.RawMessage // see internal/api/public.ParseScopes
	RateLimitRPS   int
	RateLimitBurst int
	LastUsedAt     *time.Time
	CreatedAt      time.Time
	RevokedAt      *time.Time
	ExpiresAt      *time.Time
}

const apiKeyColumns = `api_key_id, org_id, user_id, name, key_hash, key_prefix, last4,
        scopes, rate_limit_rps, rate_limit_burst,
        last_used_at, created_at, revoked_at, expires_at`

func scanAPIKey(row pgx.Row) (*APIKey, error) {
	var k APIKey
	err := row.Scan(
		&k.APIKeyID, &k.OrgID, &k.UserID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.Last4,
		&k.Scopes, &k.RateLimitRPS, &k.RateLimitBurst,
		&k.LastUsedAt, &k.CreatedAt, &k.RevokedAt, &k.ExpiresAt,
	)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// apiKeyInserter is the one-method seam insertAPIKey writes through. Both
// *pgxpool.Pool (CreateAPIKey) and pgx.Tx (CreateAPIKeyTx) satisfy it, so the
// INSERT statement and its defaults exist exactly once.
type apiKeyInserter interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CreateAPIKey inserts a new key. api_key_id and created_at are populated by RETURNING.
// Defaults rate_limit_rps/burst to 100/200 if zero (defense-in-depth alongside DB DEFAULT).
func (p *Pool) CreateAPIKey(ctx context.Context, k *APIKey) error {
	return insertAPIKey(ctx, p.pool, k)
}

func insertAPIKey(ctx context.Context, q apiKeyInserter, k *APIKey) error {
	if k.RateLimitRPS == 0 {
		k.RateLimitRPS = 100 // Q-4 default; defense in depth alongside DB DEFAULT
	}
	if k.RateLimitBurst == 0 {
		k.RateLimitBurst = 200
	}
	return q.QueryRow(ctx,
		`INSERT INTO api_keys
                (org_id, user_id, name, key_hash, key_prefix, last4, scopes,
                 rate_limit_rps, rate_limit_burst, expires_at)
             VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
             RETURNING api_key_id, created_at`,
		k.OrgID, k.UserID, k.Name, k.KeyHash, k.KeyPrefix, k.Last4, k.Scopes,
		k.RateLimitRPS, k.RateLimitBurst, k.ExpiresAt,
	).Scan(&k.APIKeyID, &k.CreatedAt)
}

// LookupAPIKeyByHash returns the key matching key_hash if it is active
// (revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())).
// Returns ErrNotFound otherwise — middleware maps ErrNotFound to 401.
func (p *Pool) LookupAPIKeyByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	row := p.pool.QueryRow(ctx,
		`SELECT `+apiKeyColumns+`
             FROM api_keys
             WHERE key_hash = $1
               AND revoked_at IS NULL
               AND (expires_at IS NULL OR expires_at > NOW())`,
		keyHash,
	)
	k, err := scanAPIKey(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return k, err
}

// TouchAPIKeyLastUsed bumps last_used_at to NOW() for the given key id.
// Fire-and-forget: callers spawn a goroutine and ignore the error.
func (p *Pool) TouchAPIKeyLastUsed(ctx context.Context, apiKeyID string) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE api_keys SET last_used_at = NOW() WHERE api_key_id = $1`,
		apiKeyID,
	)
	return err
}
