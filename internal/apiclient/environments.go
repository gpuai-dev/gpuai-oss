package apiclient

import "context"

// EnvIncludeItem is one "What's included" software entry for a framework
// environment (e.g. {"PyTorch", "2.11.0"}). Version may be empty for items
// whose exact build version isn't pinned (e.g. "git").
type EnvIncludeItem struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// EnvFrameworkVersion is one selectable version of a framework environment (the
// launch version picker) as served by GET /v1/environments. Variant is the CUDA
// variant key the version targets ("cuda128" | "cuda124") — only offerings whose
// GPU maps to that variant can run it, so a client filters by the offering's
// CUDA generation before offering the version.
type EnvFrameworkVersion struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Variant string `json:"variant"`
	Default bool   `json:"default,omitempty"`
}

// EnvVMImage is one raw-VM OS catalog entry (the launch OS picker) — the
// customer-facing label/family/version for an os id carried by an offering's
// os_options list. Per-provider native slugs never cross the wire.
type EnvVMImage struct {
	Label              string `json:"label"`
	Family             string `json:"family"`
	Version            string `json:"version"`
	DriverPreinstalled bool   `json:"driver_preinstalled,omitempty"`
}

// Environments mirrors GET /v1/environments — the server-controlled launch
// environment catalog: the published framework roster, the per-framework version
// picker options, the "What's included" lists (keyed framework → CUDA variant),
// and the raw-VM OS catalog. Feeds `gpu environments` and validates the
// `certified:<fw>@<ver>` / `raw_vm:<os>` selections a create call may pass.
type Environments struct {
	Frameworks []string                         `json:"frameworks"`
	Versions   map[string][]EnvFrameworkVersion `json:"versions"`
	// Includes is framework id (roster ids plus "base") → CUDA variant
	// ("cuda128"|"cuda124") → software list.
	Includes map[string]map[string][]EnvIncludeItem `json:"includes"`
	VMImages map[string]EnvVMImage                  `json:"vm_images"`
}

// GetEnvironments returns the launch environment catalog (public, no auth).
func (c *Client) GetEnvironments(ctx context.Context) (Environments, error) {
	env, _, err := do[Environments](ctx, c, "GET", "/environments", nil, nil)
	return env, err
}
