package db

// registry_credentials (schema v90, custom images Phase 2): per-org private
// registry logins, referenced by id from POST /v1/instances. password_enc is
// SEALED (AES-256-GCM, internal/registrycred) before it reaches this layer —
// this file stores and returns opaque bytes and never sees a plaintext
// password. Reads for the API surface deliberately exclude password_enc; only
// GetRegistryCredentialSealed (the launch path) returns it.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// RegistryCredential is the metadata view (no secret material).
type RegistryCredential struct {
	CredentialID string
	OrgID        string
	Name         string
	Registry     string
	Username     string
	CreatedAt    time.Time
}

// ErrRegistryCredentialNotFound is returned when the (org, id) pair matches
// nothing — org-scoped by construction so one org can never read another's.
var ErrRegistryCredentialNotFound = errors.New("registry credential not found")

// ErrRegistryCredentialNameTaken maps the (org_id, name) UNIQUE violation.
var ErrRegistryCredentialNameTaken = errors.New("registry credential name already exists")

// CreateRegistryCredential inserts a credential with a pre-generated id (the
// sealer binds the id as AAD, so the id must exist before sealing).
func (p *Pool) CreateRegistryCredential(ctx context.Context, cred RegistryCredential, passwordEnc []byte) error {
	// created_at comes from the caller (echoed on the create response), not
	// the column default, so the 201 body and later GETs can never disagree.
	_, err := p.pool.Exec(ctx, `
		INSERT INTO registry_credentials (credential_id, org_id, name, registry, username, password_enc, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		cred.CredentialID, cred.OrgID, cred.Name, cred.Registry, cred.Username, passwordEnc, cred.CreatedAt,
	)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
		return ErrRegistryCredentialNameTaken
	}
	return err
}

// ListRegistryCredentialsByOrg returns the org's credentials, newest first.
func (p *Pool) ListRegistryCredentialsByOrg(ctx context.Context, orgID string) ([]RegistryCredential, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT credential_id::TEXT, org_id, name, registry, username, created_at
		FROM registry_credentials WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegistryCredential
	for rows.Next() {
		var c RegistryCredential
		if err := rows.Scan(&c.CredentialID, &c.OrgID, &c.Name, &c.Registry, &c.Username, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetRegistryCredentialSealed returns a credential WITH its sealed password —
// the launch path only. Org-scoped: a wrong org gets not-found, not forbidden.
func (p *Pool) GetRegistryCredentialSealed(ctx context.Context, orgID, credentialID string) (*RegistryCredential, []byte, error) {
	var c RegistryCredential
	var enc []byte
	err := p.pool.QueryRow(ctx, `
		SELECT credential_id::TEXT, org_id, name, registry, username, created_at, password_enc
		FROM registry_credentials WHERE org_id = $1 AND credential_id = $2`, orgID, credentialID,
	).Scan(&c.CredentialID, &c.OrgID, &c.Name, &c.Registry, &c.Username, &c.CreatedAt, &enc)
	// isNoRowsOrInvalidID: a malformed (non-UUID) customer-supplied id is the
	// SAME customer error as an unknown one — 22P02 must not surface as a 500
	// (the GPU-65 invariant, internal/db/notfound.go).
	if isNoRowsOrInvalidID(err) {
		return nil, nil, ErrRegistryCredentialNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	return &c, enc, nil
}

// DeleteRegistryCredential removes an org's credential by id.
func (p *Pool) DeleteRegistryCredential(ctx context.Context, orgID, credentialID string) error {
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM registry_credentials WHERE org_id = $1 AND credential_id = $2`, orgID, credentialID)
	if isInvalidIDExec(err) {
		return ErrRegistryCredentialNotFound
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRegistryCredentialNotFound
	}
	return nil
}
