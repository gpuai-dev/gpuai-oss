package db

import (
	"context"
	"strings"
)

// GPU.ai Open Source is single tenant (see cmd/gpuctl/bootstrap.go): one
// organization and its admin user own every key and instance. The tables keep
// the hosted product's multi-org shape, keyed by a stable external id, so the
// rest of the code base (org-scoped queries, per-user SSH keys, role checks)
// runs unchanged.

// Roles a user row may carry. Unknown values normalize to RoleMember
// (fail-closed).
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// EnsureOrg upserts an organization by its external id and returns the
// internal UUID. Concurrency-safe: INSERT ... ON CONFLICT DO NOTHING.
func (p *Pool) EnsureOrg(ctx context.Context, externalOrgID string) (string, error) {
	if _, err := p.pool.Exec(ctx,
		`INSERT INTO organizations (organization_id, name, external_org_id)
		 VALUES (gen_random_uuid(), $1, $1)
		 ON CONFLICT (external_org_id) DO NOTHING`,
		externalOrgID,
	); err != nil {
		return "", err
	}
	var orgUUID string
	err := p.pool.QueryRow(ctx,
		`SELECT organization_id FROM organizations WHERE external_org_id = $1`,
		externalOrgID,
	).Scan(&orgUUID)
	return orgUUID, err
}

// normalizeRole maps a role string onto RoleAdmin or RoleMember. "owner" and
// the "org:"-prefixed spellings are accepted; anything else is RoleMember.
func normalizeRole(role string) string {
	switch strings.TrimPrefix(strings.ToLower(strings.TrimSpace(role)), "org:") {
	case "admin", "owner":
		return RoleAdmin
	default:
		return RoleMember
	}
}

// EnsureOrgAndUser upserts an organization and a user within it, both keyed
// by external id, and returns their internal UUIDs. Profile fields only
// overwrite stored values when non-empty; role always follows the argument.
func (p *Pool) EnsureOrgAndUser(ctx context.Context, externalOrgID, externalUserID, email, name, imageURL, role string) (orgID string, userID string, err error) {
	orgUUID, err := p.EnsureOrg(ctx, externalOrgID)
	if err != nil {
		return "", "", err
	}
	var userUUID string
	err = p.pool.QueryRow(ctx,
		`INSERT INTO users (user_id, org_id, external_user_id, email, name, role, image_url)
		 VALUES (gen_random_uuid(), $1, $2, NULLIF($3, ''), $4, $6, NULLIF($5, ''))
		 ON CONFLICT (external_user_id) DO UPDATE
		    SET email     = COALESCE(NULLIF(EXCLUDED.email, ''),     users.email),
		        name      = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE users.name END,
		        image_url = COALESCE(NULLIF(EXCLUDED.image_url, ''), users.image_url),
		        role      = EXCLUDED.role
		 RETURNING user_id`,
		orgUUID, externalUserID, email, name, imageURL, normalizeRole(role),
	).Scan(&userUUID)
	if err != nil {
		return "", "", err
	}
	return orgUUID, userUUID, nil
}

// GetUserRoleByUserID resolves a user's role by internal user UUID. Returns
// ErrNotFound if no row exists (including a malformed id) — callers MUST
// treat ErrNotFound as RoleMember (fail-closed), never as an elevated role.
func (p *Pool) GetUserRoleByUserID(ctx context.Context, userID string) (string, error) {
	var role string
	err := p.pool.QueryRow(ctx,
		`SELECT role FROM users WHERE user_id = $1`,
		userID,
	).Scan(&role)
	if isNoRowsOrInvalidID(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return role, nil
}
