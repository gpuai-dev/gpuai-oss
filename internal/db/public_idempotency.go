package db

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// PublicIdempotencyKey is one row of public_api_idempotency_keys (Phase 18 v13).
//
// Scope: (api_key_id, idempotency_key). NOT org-scoped — two api_keys in the
// same org with the same Idempotency-Key string are intentionally independent
// (Stripe-compat per D-11).
type PublicIdempotencyKey struct {
	APIKeyID        string
	Key             string
	RequestHash     string
	ResponseCode    *int
	ResponseBody    []byte
	ResponseHeaders json.RawMessage
	CreatedAt       time.Time
}

// GetPublicIdempotencyKey returns the stored response (if any) for a given
// (api_key_id, idempotency_key). Returns ErrNotFound when absent OR when the
// row is older than 24 hours.
//
// The 24h filter is belt-and-suspenders alongside PrunePublicIdempotencyKeys:
// even if the prune ticker is delayed or down, query-time filtering prevents
// stale-entry replays (Stripe-compat 24h dedup window per D-11).
func (p *Pool) GetPublicIdempotencyKey(ctx context.Context, apiKeyID, key string) (*PublicIdempotencyKey, error) {
	var ik PublicIdempotencyKey
	err := p.pool.QueryRow(ctx,
		`SELECT api_key_id, idempotency_key, request_hash, response_code, response_body, response_headers, created_at
		 FROM public_api_idempotency_keys
		 WHERE api_key_id = $1
		   AND idempotency_key = $2
		   AND created_at > NOW() - INTERVAL '24 hours'`,
		apiKeyID, key,
	).Scan(&ik.APIKeyID, &ik.Key, &ik.RequestHash, &ik.ResponseCode, &ik.ResponseBody, &ik.ResponseHeaders, &ik.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ik, nil
}

// CreatePublicIdempotencyKey inserts a new in-progress row (response_code NULL).
// Returns ErrIdempotencyKeyExists on PK conflict (api_key_id, idempotency_key).
//
// This is the Eager Creation pattern (Pattern 2 from RESEARCH.md): INSERT
// first, the middleware re-reads on 23505 conflict to dedup concurrent
// retries.
func (p *Pool) CreatePublicIdempotencyKey(ctx context.Context, apiKeyID, key, requestHash string) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO public_api_idempotency_keys (api_key_id, idempotency_key, request_hash)
		 VALUES ($1, $2, $3)`,
		apiKeyID, key, requestHash,
	)
	if err != nil {
		// Re-uses isDuplicateKeyError from internal/db/idempotency.go (same package).
		if isDuplicateKeyError(err) {
			return ErrIdempotencyKeyExists
		}
		return err
	}
	return nil
}

// CompletePublicIdempotencyKey persists the captured response (status, body,
// and a JSON object of preserved headers — Operation-Id, etc.) on a
// previously-created row. Idempotent: a no-op for unknown keys.
func (p *Pool) CompletePublicIdempotencyKey(ctx context.Context, apiKeyID, key string, responseCode int, responseBody []byte, responseHeaders json.RawMessage) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE public_api_idempotency_keys
		 SET response_code = $1, response_body = $2, response_headers = $3
		 WHERE api_key_id = $4 AND idempotency_key = $5`,
		responseCode, responseBody, responseHeaders, apiKeyID, key,
	)
	return err
}
