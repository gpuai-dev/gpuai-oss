package apiclient

// Registry-credential endpoints: stored private
// registry logins referenced by id from CreateInstanceRequest.
// The password crosses the wire exactly once, on create.

import (
	"context"
	"time"
)

// RegistryCredential is the metadata view of a stored login — the server
// never returns the password on any read.
type RegistryCredential struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Registry  string    `json:"registry"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateRegistryCredentialRequest is POST /v1/registry-credentials.
type CreateRegistryCredentialRequest struct {
	Name     string `json:"name"`
	Registry string `json:"registry"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// registryCredentialList is the list envelope.
type registryCredentialList struct {
	Data []RegistryCredential `json:"data"`
}

// CreateRegistryCredential stores a private-registry login and returns its
// metadata (the password is sealed server-side and never returned).
func (c *Client) CreateRegistryCredential(ctx context.Context, req CreateRegistryCredentialRequest) (RegistryCredential, error) {
	out, _, err := do[RegistryCredential](ctx, c, "POST", "/registry-credentials", req, nil)
	return out, err
}

// ListRegistryCredentials lists the org's stored logins.
func (c *Client) ListRegistryCredentials(ctx context.Context) ([]RegistryCredential, error) {
	out, _, err := do[registryCredentialList](ctx, c, "GET", "/registry-credentials", nil, nil)
	return out.Data, err
}

// DeleteRegistryCredential removes a stored login by id.
func (c *Client) DeleteRegistryCredential(ctx context.Context, id string) error {
	_, _, err := do[struct{}](ctx, c, "DELETE", "/registry-credentials/"+id, nil, nil)
	return err
}
