package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gpuai-dev/gpuai-oss/internal/api/public"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// GPU.ai Open Source is single tenant: one organization, with one admin user,
// owns every API key and instance. Its external ids are fixed so every
// replica and every `gpuctl apikey create` resolves the same rows.
const (
	defaultOrgExternalID  = "default"
	defaultUserExternalID = "admin"
)

// fullAccessScopes is the scope document of an operator key.
var fullAccessScopes = json.RawMessage(`{"full_access":true}`)

// ensureDefaultOrg creates the default organization and its admin user if
// they do not exist yet, and returns their internal ids.
func ensureDefaultOrg(ctx context.Context, pool *db.Pool) (orgID, userID string, err error) {
	return pool.EnsureOrgAndUser(ctx, defaultOrgExternalID, defaultUserExternalID,
		"", "Administrator", "", db.RoleAdmin)
}

// seedBootstrapAPIKey stores GPUCTL_BOOTSTRAP_API_KEY (hashed) as a
// full-access key for the default organization. Idempotent: a key whose hash
// is already present is left alone. Empty raw is a no-op.
func seedBootstrapAPIKey(ctx context.Context, pool *db.Pool, raw string) error {
	if raw == "" {
		return nil
	}
	if err := public.ParseRawKey(raw); err != nil {
		return fmt.Errorf("GPUCTL_BOOTSTRAP_API_KEY is not a valid API key (expect gpuai_live_ followed by at least 24 characters): %w", err)
	}
	hash := public.HashKey(raw)
	if _, err := pool.LookupAPIKeyByHash(ctx, hash); err == nil {
		return nil
	} else if !errors.Is(err, db.ErrNotFound) {
		return err
	}
	orgID, userID, err := ensureDefaultOrg(ctx, pool)
	if err != nil {
		return err
	}
	k := &db.APIKey{
		OrgID:     orgID,
		UserID:    userID,
		Name:      "bootstrap",
		KeyHash:   hash,
		KeyPrefix: raw[:len(public.KeyPrefixLive)+4],
		Last4:     raw[len(raw)-4:],
		Scopes:    fullAccessScopes,
	}
	return pool.CreateAPIKey(ctx, k)
}

// createAPIKey mints a new key for the default organization and returns the
// raw secret, which is never stored.
func createAPIKey(ctx context.Context, pool *db.Pool, name string, scopes json.RawMessage) (raw string, k *db.APIKey, err error) {
	if _, err := public.ParseScopes(scopes); err != nil {
		return "", nil, fmt.Errorf("invalid scopes: %w", err)
	}
	orgID, userID, err := ensureDefaultOrg(ctx, pool)
	if err != nil {
		return "", nil, err
	}
	raw, hash, prefix, last4, err := public.GenerateAPIKey()
	if err != nil {
		return "", nil, err
	}
	k = &db.APIKey{
		OrgID:     orgID,
		UserID:    userID,
		Name:      name,
		KeyHash:   hash,
		KeyPrefix: prefix,
		Last4:     last4,
		Scopes:    scopes,
	}
	if err := pool.CreateAPIKey(ctx, k); err != nil {
		return "", nil, err
	}
	return raw, k, nil
}
