package apiclient

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// SSHKey mirrors the public-API SSH key resource.
type SSHKey struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	// AttachOnProvision reports whether this key is auto-attached to a new
	// instance's authorized_keys at launch when no explicit --ssh-key-id is
	// passed.
	AttachOnProvision bool      `json:"attach_on_provision"`
	CreatedAt         time.Time `json:"created_at"`
}

// SSHKeyPage is the cursor-paginated envelope returned by GET /ssh-keys.
type SSHKeyPage struct {
	Data       []SSHKey `json:"data"`
	NextCursor *string  `json:"next_cursor,omitempty"`
}

// ListSSHKeysOpts contains cursor pagination knobs for ListSSHKeys.
type ListSSHKeysOpts struct {
	Cursor string
	Limit  int
}

// ListSSHKeys returns a cursor-paginated page of SSH keys for the caller's org.
func (c *Client) ListSSHKeys(ctx context.Context, opts ListSSHKeysOpts) (SSHKeyPage, error) {
	q := url.Values{}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", opts.Limit))
	}
	path := "/ssh-keys"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	page, _, err := do[SSHKeyPage](ctx, c, "GET", path, nil, nil)
	return page, err
}

// CreateSSHKey registers a new SSH public key under the caller's org.
func (c *Client) CreateSSHKey(ctx context.Context, name, publicKey string) (SSHKey, error) {
	body := map[string]string{"name": name, "public_key": publicKey}
	k, _, err := do[SSHKey](ctx, c, "POST", "/ssh-keys", body, nil)
	return k, err
}

// DeleteSSHKey removes the SSH key by ID. Idempotent — 404 is surfaced as
// *ProblemError.
func (c *Client) DeleteSSHKey(ctx context.Context, id string) error {
	_, _, err := do[struct{}](ctx, c, "DELETE", "/ssh-keys/"+url.PathEscape(id), nil, nil)
	return err
}

// GetSSHKey fetches one SSH key by id.
func (c *Client) GetSSHKey(ctx context.Context, id string) (SSHKey, error) {
	k, _, err := do[SSHKey](ctx, c, "GET", "/ssh-keys/"+url.PathEscape(id), nil, nil)
	return k, err
}
