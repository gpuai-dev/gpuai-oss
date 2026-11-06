package apiclient

import (
	"context"
	"net/url"
)

// GPUType mirrors one row of GET /v1/gpu-types: {gpu_type, vram_gb} plus the
// representative host specs (cpu_cores, ram_gb, storage_gb). VRAM is invariant
// per model; the CPU/RAM/storage values vary per offering, so they are the
// model's representative config — `gpu pricing` carries the exact per-offering
// specs. 0 marks a spec the catalog did not report.
type GPUType struct {
	GPUType   string `json:"gpu_type"`
	VRAMGB    int    `json:"vram_gb"`
	CPUCores  int    `json:"cpu_cores"`
	RAMGB     int    `json:"ram_gb"`
	StorageGB int    `json:"storage_gb"`
	// InstanceDiskGB is the representative instance filesystem (GB) a launch of
	// this model receives; 0 marks unknown. `gpu pricing` carries the exact
	// per-offering value.
	InstanceDiskGB int `json:"instance_disk_gb"`
	// DiskConfigurable is true when at least one offering of this model accepts
	// a custom --disk at launch (an OR across offerings).
	DiskConfigurable bool `json:"disk_configurable"`
}

// PricingRow mirrors one row of GET /v1/pricing. Available is the count
// of units currently provisionable at this offer — not a boolean; the
// server merges customer-identical offers and sums their availability.
// GPUCount is the number of GPUs in the listed configuration and
// PricePerHour covers the WHOLE configuration (an 8-GPU node lists ~8x the
// single-GPU price). InstantBoot classifies boot speed: false marks
// capacity that takes ~3-5 minutes to become reachable rather than booting
// in under a minute.
type PricingRow struct {
	GPUType  string `json:"gpu_type"`
	GPUCount int    `json:"gpu_count"`
	// CPUCores, RAMGB and StorageGB are the exact host hardware for this
	// offering, tied to the shown (cheapest) price. 0 marks an unreported spec.
	CPUCores     int     `json:"cpu_cores"`
	RAMGB        int     `json:"ram_gb"`
	StorageGB    int     `json:"storage_gb"`
	Region       string  `json:"region"`
	Tier         string  `json:"tier"`
	PricePerHour float64 `json:"price_per_hour"`
	Available    int     `json:"available"`
	InstantBoot  bool    `json:"instant_boot"`
	// Community marks supplier-owned community-tier capacity (softer SLA) —
	// a capacity attribute, never supplier identity.
	Community bool `json:"community"`
	// CapacityClass is the per-offering secure/community classification
	// ("secure" | "community") — the string twin of Community, classified per
	// offering rather than per upstream. "" marks a pre-field catalog row.
	CapacityClass string `json:"capacity_class"`
	// CertifiedImage reports whether this offering can run the certified
	// image (a capability class, never supplier identity).
	CertifiedImage bool `json:"certified_image"`
	// DeploymentType is "vm" (we bootstrap the machine ourselves) or "container"
	// (the provider runs our image natively). "" marks a pre-field catalog row.
	// A container offering runs the workload AS an unprivileged container, so
	// Docker cannot run inside it (no Docker-in-Docker, no --privileged); vm
	// offerings give root on the machine and can.
	DeploymentType string `json:"deployment_type"`
	// InstanceDiskGB is the instance filesystem (GB) a launch of THIS offering
	// receives — the platform default where the upstream honors a configured
	// size, otherwise the machine's reported disk; 0 marks unknown, not zero.
	InstanceDiskGB int `json:"instance_disk_gb"`
	// DiskConfigurable is true when a launch of this offering may choose its own
	// disk size (`gpu instances create --disk`).
	DiskConfigurable bool `json:"disk_configurable"`
	// DiskPricePerGBHour is the price of one GB of instance disk beyond
	// InstanceDiskGB, per hour, folded into the instance's hourly rate at
	// launch. 0 = larger sizes are currently free (or not configurable).
	DiskPricePerGBHour float64 `json:"disk_price_per_gb_hour"`
	// OfferingID is the opaque deterministic ID used to pin an exact offering at
	// launch. It carries no provider identity.
	OfferingID string `json:"offering_id"`
	// Environment is the published pinned-environment contract,
	// present only for certified-capable rows; nil otherwise.
	Environment *EnvContract `json:"environment,omitempty"`
}

// EnvContract is the published pinned-environment contract for a catalog row
// : the digest-pinned certified image plus the driver/CUDA floors the
// server guarantees. All fields are server-derived from the manifest — never
// customer input.
type EnvContract struct {
	Image       string `json:"image"`        // human-readable image ref (may be tag-form for display)
	ImageDigest string `json:"image_digest"` // the sha256 digest the launch path actually pins
	DriverMin   string `json:"driver_min"`   // minimum NVIDIA driver, e.g. "570.26"
	CUDA        string `json:"cuda"`         // CUDA toolkit generation, e.g. "12.8"
}

// ListPricingOpts narrows the GET /v1/pricing query. IncludeUnavailable
// asks the server to keep zero-availability offers (dropped by default).
type ListPricingOpts struct {
	GPUType            string
	Region             string
	Tier               string
	IncludeUnavailable bool
	// Certified, when true, restricts results to offerings that can run the
	// Certified Image (certified_image=true server-side filter).
	Certified bool
	// DeploymentType, when set to "vm" or "container", restricts results to that
	// deployment class. Empty means no deployment-type filter. Use "vm" for
	// workloads that run their own Docker: container offerings run as an
	// unprivileged container and cannot do Docker-in-Docker (--privileged).
	// Any other value is a 400 from the server (strict enum).
	DeploymentType string
}

type gpuTypesEnvelope struct {
	Data []GPUType `json:"data"`
}
type pricingEnvelope struct {
	Data       []PricingRow `json:"data"`
	NextCursor *string      `json:"next_cursor"`
}

// maxPricingPages bounds the cursor-follow loop in ListPricing: 50 pages x
// 200 rows = 10k offers, far above any real catalog. Purely a runaway guard
// against a server bug that returns a non-advancing cursor.
const maxPricingPages = 50

// ListGPUTypes returns the catalog of GPU types the server offers.
func (c *Client) ListGPUTypes(ctx context.Context) ([]GPUType, error) {
	env, _, err := do[gpuTypesEnvelope](ctx, c, "GET", "/gpu-types", nil, nil)
	return env.Data, err
}

// ListPricing returns the full price grid, following next_cursor until the
// server reports no more pages — callers always get the complete catalog,
// not just the first page (the pre-cursor-follow behavior silently truncated
// `gpu pricing` at the default page size).
func (c *Client) ListPricing(ctx context.Context, opts ListPricingOpts) ([]PricingRow, error) {
	var all []PricingRow
	cursor := ""
	for page := 0; page < maxPricingPages; page++ {
		q := url.Values{}
		q.Set("limit", "200") // server MaxPageSize — fewest round-trips
		if opts.GPUType != "" {
			q.Set("gpu_type", opts.GPUType)
		}
		if opts.Region != "" {
			q.Set("region", opts.Region)
		}
		if opts.Tier != "" {
			q.Set("tier", opts.Tier)
		}
		if opts.IncludeUnavailable {
			q.Set("include_unavailable", "true")
		}
		if opts.Certified {
			q.Set("certified_image", "true")
		}
		if opts.DeploymentType != "" {
			q.Set("deployment_type", opts.DeploymentType)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		env, _, err := do[pricingEnvelope](ctx, c, "GET", "/pricing?"+q.Encode(), nil, nil)
		if err != nil {
			return nil, err
		}
		all = append(all, env.Data...)
		if env.NextCursor == nil || *env.NextCursor == "" {
			break
		}
		cursor = *env.NextCursor
	}
	return all, nil
}
