package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrDuplicateKey is returned when an SSH key with the same fingerprint
// already exists for the organization.
var ErrDuplicateKey = errors.New("ssh key already exists")

// sshKeyColumns is the canonical SELECT list, kept in one place so every reader
// scans the same shape (including attach_on_provision, added in v33).
const sshKeyColumns = "ssh_key_id, org_id, user_id, name, public_key, fingerprint, attach_on_provision, created_at"

// SSHKey represents an SSH key record from the database.
type SSHKey struct {
	SSHKeyID    string
	OrgID       string
	UserID      string
	Name        string
	PublicKey   string
	Fingerprint string
	// AttachOnProvision controls whether this key is auto-attached to a new
	// instance's authorized_keys at launch when the request carries no explicit
	// ssh_key_ids (v33). Defaults to true.
	AttachOnProvision bool
	CreatedAt         time.Time
}

// scanSSHKey scans a row in sshKeyColumns order.
func scanSSHKey(row pgx.Row, k *SSHKey) error {
	return row.Scan(&k.SSHKeyID, &k.OrgID, &k.UserID, &k.Name, &k.PublicKey, &k.Fingerprint, &k.AttachOnProvision, &k.CreatedAt)
}

func scanSSHKeys(rows pgx.Rows) ([]SSHKey, error) {
	defer rows.Close()
	var keys []SSHKey
	for rows.Next() {
		var k SSHKey
		if err := scanSSHKey(rows, &k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// CreateSSHKey inserts a new SSH key into the database.
// The ssh_key_id and created_at fields are populated by the database via RETURNING.
// attach_on_provision defaults to true at the DB level (v33).
// Returns ErrDuplicateKey if a key with the same fingerprint already exists for the org.
func (p *Pool) CreateSSHKey(ctx context.Context, key *SSHKey) error {
	err := p.pool.QueryRow(ctx,
		`INSERT INTO ssh_keys (user_id, org_id, name, public_key, fingerprint)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING ssh_key_id, attach_on_provision, created_at`,
		key.UserID, key.OrgID, key.Name, key.PublicKey, key.Fingerprint,
	).Scan(&key.SSHKeyID, &key.AttachOnProvision, &key.CreatedAt)
	if err != nil {
		// Check for unique violation (SQLSTATE 23505) on (org_id, fingerprint).
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			return ErrDuplicateKey
		}
		return err
	}
	return nil
}

// ListSSHKeysByOrg retrieves all SSH keys for an organization, ordered by newest first.
// Used for the admin dashboard view (all org keys, with their attach flag).
func (p *Pool) ListSSHKeysByOrg(ctx context.Context, orgID string) ([]SSHKey, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT `+sshKeyColumns+`
		 FROM ssh_keys WHERE org_id = $1
		 ORDER BY created_at DESC`,
		orgID,
	)
	if err != nil {
		return nil, err
	}
	return scanSSHKeys(rows)
}

// ListSSHKeysByOrgAndUser retrieves the SSH keys a specific user owns within
// an organization, ordered by newest first. Both filters matter: a user can
// belong to multiple orgs, so user_id alone would leak keys across orgs.
// Used for org members (non-admins), who only see their own keys.
func (p *Pool) ListSSHKeysByOrgAndUser(ctx context.Context, orgID, userID string) ([]SSHKey, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT `+sshKeyColumns+`
		 FROM ssh_keys WHERE org_id = $1 AND user_id = $2
		 ORDER BY created_at DESC`,
		orgID, userID,
	)
	if err != nil {
		return nil, err
	}
	return scanSSHKeys(rows)
}

// GetSSHKeyForOrg retrieves a single SSH key by ID, scoped to an organization.
// Returns ErrNotFound if the key doesn't exist or belongs to a different org.
func (p *Pool) GetSSHKeyForOrg(ctx context.Context, keyID, orgID string) (*SSHKey, error) {
	var k SSHKey
	err := scanSSHKey(p.pool.QueryRow(ctx,
		`SELECT `+sshKeyColumns+`
		 FROM ssh_keys WHERE ssh_key_id = $1 AND org_id = $2`,
		keyID, orgID,
	), &k)
	if err != nil {
		if isNoRowsOrInvalidID(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &k, nil
}

// SetSSHKeyAttachOnProvision updates the attach-on-provision flag for a key,
// scoped to an organization. Returns false (not error) when the key is not
// found or belongs to a different org. Caller is responsible for the role check
// (members may only toggle their own keys; admins may toggle any org key).
func (p *Pool) SetSSHKeyAttachOnProvision(ctx context.Context, keyID, orgID string, attach bool) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE ssh_keys SET attach_on_provision = $3 WHERE ssh_key_id = $1 AND org_id = $2`,
		keyID, orgID, attach,
	)
	if err != nil {
		if isInvalidIDExec(err) {
			return false, nil
		}
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteSSHKey removes an SSH key by ID, scoped to an organization.
// Returns false (not error) when the key is not found or belongs to a different org,
// preventing existence leaking across orgs.
func (p *Pool) DeleteSSHKey(ctx context.Context, keyID, orgID string) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM ssh_keys WHERE ssh_key_id = $1 AND org_id = $2`,
		keyID, orgID,
	)
	if err != nil {
		if isInvalidIDExec(err) {
			return false, nil
		}
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// GetSSHKeysByOrgID retrieves all SSH keys belonging to an organization.
func (p *Pool) GetSSHKeysByOrgID(ctx context.Context, orgID string) ([]SSHKey, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT `+sshKeyColumns+`
		 FROM ssh_keys WHERE org_id = $1
		 ORDER BY created_at DESC`,
		orgID,
	)
	if err != nil {
		return nil, err
	}
	return scanSSHKeys(rows)
}

// GetSSHKeysByIDs retrieves SSH keys by a list of IDs.
// Used by the provisioning engine to resolve ssh_key_ids to public key content.
// Returns only the keys that exist; missing IDs are silently skipped.
func (p *Pool) GetSSHKeysByIDs(ctx context.Context, ids []string) ([]SSHKey, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT `+sshKeyColumns+`
		 FROM ssh_keys WHERE ssh_key_id = ANY($1)`,
		ids,
	)
	if err != nil {
		return nil, err
	}
	return scanSSHKeys(rows)
}
