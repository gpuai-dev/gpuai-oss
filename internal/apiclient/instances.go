package apiclient

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// Instance mirrors the public-API instance shape (GET /v1/instances/{id}).
// Fields use pointers where the
// server emits omitempty so unset/nil renders cleanly.
type Instance struct {
	ID           string          `json:"id"`
	Name         *string         `json:"name,omitempty"`
	Status       string          `json:"status"`
	GPUType      string          `json:"gpu_type"`
	GPUCount     int             `json:"gpu_count"`
	Tier         string          `json:"tier"`
	Region       string          `json:"region"`
	PricePerHour float64         `json:"price_per_hour"`
	Connection   *ConnectionInfo `json:"connection,omitempty"`
	// DiskGB is the instance filesystem size (GB) the platform configured for
	// this launch. nil/absent = supplier-determined (the machine's own flavor
	// disk) — cliprint omits the row rather than fabricating a number.
	DiskGB *int `json:"disk_gb,omitempty"`
	// Image is the customer-supplied container image this instance was
	// launched from (custom-image launches only), digest-pinned by the
	// server's pre-flight. nil/absent for raw and certified
	// launches — cliprint omits the row.
	Image *string `json:"image,omitempty"`
	// StatusReason explains WHY the instance is in the error state (server
	// wording, e.g. "provisioning stalled: …"). nil/absent for healthy
	// instances — cliprint renders it only when present.
	StatusReason *string    `json:"status_reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ReadyAt      *time.Time `json:"ready_at,omitempty"`
	TerminatedAt *time.Time `json:"terminated_at,omitempty"`
	// LastReachableAt is the last time the platform verified SSH reachability. nil/absent = not yet probed, which is NOT the same as
	// unreachable — cliprint omits the row rather than printing "never".
	LastReachableAt *time.Time `json:"last_reachable_at,omitempty"`
	// UnreachableSince is the start of the confirmed SSH outage the instance
	// is currently in. Present exactly when Status is "unreachable".
	UnreachableSince *time.Time `json:"unreachable_since,omitempty"`

	// AutoTerminateHours is the runtime limit this instance was launched with
	//, echoed back by the server. nil/absent = no limit — cliprint
	// omits the row rather than printing "none".
	AutoTerminateHours *int `json:"auto_terminate_hours,omitempty"`
	// AutoTerminateAt is when that limit expires and the platform terminates
	// the instance. Anchored at the running transition (ready_at + hours), so
	// it is NOT derivable client-side from the create call. nil/absent when no
	// limit was set, when the instance is not running yet, or once the limit
	// has fired.
	AutoTerminateAt *time.Time `json:"auto_terminate_at,omitempty"`

	// Environment is the launch environment class the server resolved for this
	// instance: "certified", "raw_vm", or "provider_template" —
	// certified may carry a framework sub-choice as "certified:<framework>"
	//. The CLI renders it as a branding
	// string. Empty when the server does not (yet) report it — cliprint omits
	// the row rather than showing a blank.
	Environment string `json:"environment,omitempty"`
	// DriverVersion, CUDAVersion and ImageDigest are optional facts reported by the
	// instance's ready callback. They render only when present; the
	// server owns these values — they are never derived from client input.
	DriverVersion string `json:"driver_version,omitempty"`
	CUDAVersion   string `json:"cuda_version,omitempty"`
	ImageDigest   string `json:"image_digest,omitempty"`
}

// ConnectionInfo is the connect-time SSH info attached to a running instance.
// AppURL is the HTTPS URL of the instance's primary http port, derived
// server-side as https://<id>.<apps-host> when the optional tunnel is enabled;
// it is empty otherwise. Mirrors the server's connection.app_url.
type ConnectionInfo struct {
	Hostname   string `json:"hostname"`
	Port       int    `json:"port"`
	SSHCommand string `json:"ssh_command"`
	AppURL     string `json:"app_url,omitempty"`
	// AppUser / AppPassword are the HTTP Basic Auth login for AppURL and
	// TerminalURL, persisted server-side. The server sends
	// AppPassword ONLY on the ?include=credentials opt-in read
	// (GetInstanceWithCredentials) — plain GetInstance/ListInstances responses
	// carry AppUser and the URLs but never the password.
	AppUser     string `json:"app_user,omitempty"`
	AppPassword string `json:"app_password,omitempty"`
	// TerminalURL is the browser web console — a shell on the instance
	// with no SSH key required, https://<id>-term.<apps-host>, guarded by the
	// same AppUser/AppPassword login. Empty when the server does not provision
	// a console (it needs the optional tunnel).
	TerminalURL string `json:"terminal_url,omitempty"`
}

// Operation mirrors GET /v1/operations/{id}. State is one of
// "pending", "running", "succeeded", "failed", "cancelled".
type Operation struct {
	OperationID string           `json:"operation_id"`
	Kind        string           `json:"kind"`
	State       string           `json:"state"`
	ResourceID  *string          `json:"resource_id,omitempty"`
	Error       *OpError         `json:"error,omitempty"`
	Result      *OperationResult `json:"result,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
	CompletedAt *time.Time       `json:"completed_at,omitempty"`
	// Warnings are create-time advisories on the synchronous 202 from
	// POST /instances. Never present on GET /operations/{id} polls
	// — surface them at submit time or not at all.
	Warnings []string `json:"warnings,omitempty"`
}

// OperationResult is the terminal-success payload. It carries the non-secret
// app coordinates when the server provides them (the `result` wire field), and
// is nil for any read where the stored result is empty.
//
// The server strips app_basic_auth_pass from every operation read,
// so there is no password field here — fetch the credential via
// GetInstanceWithCredentials instead.
type OperationResult struct {
	AppURL           string `json:"app_url,omitempty"`
	TerminalURL      string `json:"terminal_url,omitempty"`
	AppBasicAuthUser string `json:"app_basic_auth_user,omitempty"`
}

// OpError is the structured failure payload on a terminal-failed Operation.
type OpError struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// InstancePage is the cursor-paginated envelope returned by GET /instances.
type InstancePage struct {
	Data       []Instance `json:"data"`
	NextCursor *string    `json:"next_cursor,omitempty"`
}

// CreateInstanceRequest is the POST /instances body. Either Region or Regions
// may be set; the server normalizes to a regions list.
type CreateInstanceRequest struct {
	GPUType   string   `json:"gpu_type"`
	GPUCount  int      `json:"gpu_count,omitempty"`
	Region    string   `json:"region,omitempty"`
	Regions   []string `json:"regions,omitempty"`
	Tier      string   `json:"tier"`
	SSHKeyIDs []string `json:"ssh_key_ids,omitempty"`
	Name      *string  `json:"name,omitempty"`

	// Environment picks the launch environment class: "certified"
	// (the certified image), "raw_vm" (a bootstrapped VM with root access), or
	// "provider_template" (the supplier's own image). "certified"
	// may carry a framework sub-choice as "certified:<framework>": a thin, digest-pinned framework layer on
	// the certified base. Empty => server default. Mirrors the
	// server's `environment` field.
	Environment *string `json:"environment,omitempty"`
	// OfferingID pins the launch to one exact server-issued offering.
	// Empty => price-sorted placement. Opaque token — the server re-derives
	// the offering and fails explicitly rather than substituting a pricier one.
	OfferingID *string `json:"offering_id,omitempty"`
	// MaxPricePerHour caps placement at a $/hr (whole node). The server
	// enforces it on every fallback attempt, so a launch never lands above
	// it; if the cheapest available exceeds it the create fails with a
	// price-changed conflict carrying the current price. Empty => no cap.
	MaxPricePerHour *float64 `json:"max_price_per_hour,omitempty"`
	// ViewedPricePerHour is the $/hr that was DISPLAYED to the user
	// before launching. A pin fixes which offering we place on but
	// carries no price, so the launch-time re-quote would otherwise be accepted silently.
	// Set this and a re-quote more than a cent above it fails with
	// price_changed carrying the new price; leave it nil to accept the
	// launch-time price (the previous behavior).
	//
	// Not a cap: the server reads it only to refuse, never as consent, so it
	// can never raise the price or widen what the server may launch.
	ViewedPricePerHour *float64 `json:"viewed_price_per_hour,omitempty"`
	// DiskGB requests a specific instance filesystem size in GB. nil => the
	// platform default (100 GB), which never triggers the configurable-disk
	// gate (though placement still avoids machines whose real storage cannot
	// fit the default). Setting it is a
	// hard requirement: placement narrows to capacity that can honor it
	// (disk_configurable on GET /v1/pricing) and an unhonorable request fails
	// with 422 disk_unavailable rather than launching a different size. When
	// OfferingID is also set, a size above that offering's displayed
	// storage_gb fails with 422 disk_exceeds_offering rather than launching
	// with a clamped filesystem.
	DiskGB *int `json:"disk_gb,omitempty"`
	// AutoTerminateHours terminates the instance automatically after that many
	// hours of runtime. nil => no limit, the default. The countdown
	// starts when the instance reaches running, not when the request is made,
	// and the resolved deadline comes back as AutoTerminateAt.
	//
	// Enforced server-side, which is the whole point: a timer in this process
	// dies with this process, and the case the field exists for is an
	// unattended run whose client is already gone. Bounds are 1..720 (30
	// days); an explicit 0 is refused rather than read as "no limit".
	AutoTerminateHours *int `json:"auto_terminate_hours,omitempty"`
	// Image launches the instance from a customer-supplied container image
	//, e.g. "pytorch/pytorch:2.4.1-cuda12.4-cudnn9-devel"
	// or "ghcr.io/acme/trainer:v1". The image must be publicly pullable and
	// linux/amd64 — the server verifies both synchronously (a bad image fails
	// the request, not the launch) and digest-pins the launch. Mutually
	// exclusive with Environment. The image's own ENTRYPOINT/CMD is started
	// after SSH setup.
	Image *string `json:"image,omitempty"`
	// Env carries plain container environment variables for a custom-image
	// launch. Server-side, keys must be POSIX names and the platform's
	// reserved namespace (PUBLIC_KEY, GPUAI_*) is refused. Ignored unless
	// Image is set.
	Env map[string]string `json:"env,omitempty"`
	// RegistryCredentialID references a stored private-registry login
	// (POST /v1/registry-credentials) to pull a private Image. Requires Image;
	// the credential's registry must match the image's.
	RegistryCredentialID *string `json:"registry_credential_id,omitempty"`
	// Entrypoint / Cmd override the image's process defaults (docker
	// semantics: entrypoint replaces ENTRYPOINT and drops the image CMD
	// unless cmd is also given). nil = use the image's own. Require Image.
	Entrypoint *[]string `json:"entrypoint,omitempty"`
	Cmd        *[]string `json:"cmd,omitempty"`
	// Ports declares exposed container ports for a custom-image launch. At
	// most one may be primary (http): it is served at the instance's app URL
	// behind the per-instance basic-auth login. Requires Image.
	Ports []InstancePort `json:"ports,omitempty"`
}

// InstancePort is a declared container port on a custom-image launch.
type InstancePort struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"` // "http" | "tcp"
	Primary  bool   `json:"primary,omitempty"`
}

// ListInstancesOpts contains cursor pagination knobs for ListInstances.
// Status filters by customer-facing state (starting | running | stopping |
// stopped | terminated | error | all); empty means the server default,
// which is every non-terminated instance.
type ListInstancesOpts struct {
	Cursor string
	Limit  int
	Status string
}

// ListInstances returns a cursor-paginated page of instances.
func (c *Client) ListInstances(ctx context.Context, opts ListInstancesOpts) (InstancePage, error) {
	q := url.Values{}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", opts.Limit))
	}
	path := "/instances"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	page, _, err := do[InstancePage](ctx, c, "GET", path, nil, nil)
	return page, err
}

// GetInstance returns a single instance by ID. The response never carries
// connection.app_password — callers that display the web-console
// login must use GetInstanceWithCredentials.
func (c *Client) GetInstance(ctx context.Context, id string) (Instance, error) {
	inst, _, err := do[Instance](ctx, c, "GET", "/instances/"+url.PathEscape(id), nil, nil)
	return inst, err
}

// GetInstanceWithCredentials returns a single instance by ID with the
// ?include=credentials opt-in, restoring connection.app_password
// for instances that persisted their web-console login. Use only where the
// credential is genuinely displayed — everything else stays on GetInstance
// so bare reads can be logged without leaking the secret.
func (c *Client) GetInstanceWithCredentials(ctx context.Context, id string) (Instance, error) {
	inst, _, err := do[Instance](ctx, c, "GET", "/instances/"+url.PathEscape(id)+"?include=credentials", nil, nil)
	return inst, err
}

// CreateInstance returns (Operation, operationID, error). operationID is from
// the Operation-Id response header; Operation is from the body.
func (c *Client) CreateInstance(ctx context.Context, req CreateInstanceRequest) (Operation, string, error) {
	return do[Operation](ctx, c, "POST", "/instances", req, nil)
}

// DeleteInstance issues DELETE /instances/{id}. Returns the async Operation
// envelope plus the Operation-Id header.
func (c *Client) DeleteInstance(ctx context.Context, id string) (Operation, string, error) {
	return do[Operation](ctx, c, "DELETE", "/instances/"+url.PathEscape(id), nil, nil)
}

// PatchInstance renames an instance. Currently only the name field is mutable.
func (c *Client) PatchInstance(ctx context.Context, id, name string) (Instance, error) {
	inst, _, err := do[Instance](ctx, c, "PATCH", "/instances/"+url.PathEscape(id),
		map[string]string{"name": name}, nil)
	return inst, err
}

// GetOperation fetches a single Operation by ID.
func (c *Client) GetOperation(ctx context.Context, opID string) (Operation, error) {
	op, _, err := do[Operation](ctx, c, "GET", "/operations/"+url.PathEscape(opID), nil, nil)
	return op, err
}

// PollOperation polls /operations/{id} every 2s (ramping to 10s) until the
// operation reaches a terminal state OR the deadline elapses. The
// CLI imposes its own deadline since the server has no recovery worker.
func (c *Client) PollOperation(ctx context.Context, opID string, deadline time.Duration) (Operation, error) {
	end := time.Now().Add(deadline)
	interval := 2 * time.Second
	for time.Now().Before(end) {
		op, err := c.GetOperation(ctx, opID)
		if err != nil {
			return Operation{}, err
		}
		switch op.State {
		case "succeeded", "failed", "cancelled":
			return op, nil
		}
		select {
		case <-ctx.Done():
			return Operation{}, ctx.Err()
		case <-time.After(interval):
		}
		if interval < 10*time.Second {
			interval += 1 * time.Second
		}
	}
	return Operation{}, fmt.Errorf("operation %s timed out — check `gpu operations get %s`", opID, opID)
}
