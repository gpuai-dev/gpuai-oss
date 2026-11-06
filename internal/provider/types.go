package provider

import (
	"fmt"
	"time"
)

// FallbackDiskGB is the container-disk size an adapter provisions when its
// ProvisionRequest carries no ContainerDiskGB. It is a BACKSTOP, not the
// product default: the provision engine fills ContainerDiskGB from
// LAUNCH_DEFAULT_DISK_GB on every customer launch, so this value is only
// reached on paths that construct a ProvisionRequest directly — live tests and
// reconcile. It is kept numerically equal to the engine default so those paths
// don't silently provision a different size than a real launch.
//
// Previously each adapter carried its own defaultContainerDisk = 40.
const FallbackDiskGB = 100

// GPUType enumerates supported GPU models.
type GPUType string

const (
	// Blackwell
	GPUTypeB200 GPUType = "b200"
	GPUTypeB300 GPUType = "b300"

	// Hopper
	GPUTypeH200SXM  GPUType = "h200_sxm"
	GPUTypeH200NVL  GPUType = "h200_nvl"
	GPUTypeH100SXM  GPUType = "h100_sxm"
	GPUTypeH100NVL  GPUType = "h100_nvl"
	GPUTypeH100PCIE GPUType = "h100_pcie"

	// Ampere data center
	GPUTypeA10080GB GPUType = "a100_80gb"
	GPUTypeA10040GB GPUType = "a100_40gb"
	GPUTypeA40      GPUType = "a40"
	GPUTypeA30      GPUType = "a30"
	GPUTypeA16      GPUType = "a16"

	// Ada Lovelace / Ampere professional
	GPUTypeL40S       GPUType = "l40s"
	GPUTypeL40        GPUType = "l40"
	GPUTypeL4         GPUType = "l4"
	GPUTypeRTX6000Ada GPUType = "rtx_6000_ada"
	GPUTypeRTX5000Ada GPUType = "rtx_5000_ada"
	GPUTypeRTX4000Ada GPUType = "rtx_4000_ada"
	GPUTypeRTX2000Ada GPUType = "rtx_2000_ada"
	GPUTypeRTXA6000   GPUType = "rtx_a6000"
	GPUTypeRTXA5000   GPUType = "rtx_a5000"
	GPUTypeRTXA4500   GPUType = "rtx_a4500"
	GPUTypeRTXA4000   GPUType = "rtx_a4000"
	GPUTypeRTXPro6000 GPUType = "rtx_pro_6000"
	GPUTypeRTXPro4500 GPUType = "rtx_pro_4500"

	// Consumer
	GPUTypeRTX5090 GPUType = "rtx_5090"
	GPUTypeRTX5080 GPUType = "rtx_5080"
	GPUTypeRTX4090 GPUType = "rtx_4090"
	GPUTypeRTX4080 GPUType = "rtx_4080"
	GPUTypeRTX3090 GPUType = "rtx_3090"
	GPUTypeRTX3080 GPUType = "rtx_3080"

	// Legacy
	GPUTypeV100 GPUType = "v100"
	GPUTypeT4   GPUType = "t4"
)

// InstanceTier represents the pricing/availability tier.
// Three tiers: on_demand, spot, reserved. Reserved instances are bound to
// a reservation_capacity slot — see internal/reservation/.
type InstanceTier string

const (
	TierOnDemand InstanceTier = "on_demand"
	TierSpot     InstanceTier = "spot"
	TierReserved InstanceTier = "reserved"
)

// GPUOffering represents an available GPU configuration from an upstream provider.
//
// PricePerHour is the hourly price for the WHOLE configuration as listed —
// i.e. it covers all GPUCount GPUs. Adapters whose upstream prices per GPU
// (e.g. flavor pricebooks) must multiply by the flavor's GPU count before
// emitting. Everything downstream (catalog markup, instance persist, billing)
// treats this as the full-node rate and never multiplies by GPUCount again.
type GPUOffering struct {
	Provider           string       `json:"provider"`
	GPUType            GPUType      `json:"gpu_type"`
	GPUCount           int          `json:"gpu_count"`
	VRAMPerGPUGB       int          `json:"vram_per_gpu_gb"`
	CPUCores           int          `json:"cpu_cores"`
	RAMGB              int          `json:"ram_gb"`
	StorageGB          int          `json:"storage_gb"`
	PricePerHour       float64      `json:"price_per_hour"`
	Tier               InstanceTier `json:"tier"`
	Region             string       `json:"region"`
	DatacenterLocation string       `json:"datacenter_location"`
	StockStatus        string       `json:"stock_status"`
	AvailableCount     int          `json:"available_count"`
	// ProvisioningEstimateMinutes is the adapter's per-offering estimate of the
	// create→ready wall-clock, in minutes. Non-zero only for adapters whose boot
	// time varies by offering (e.g. a virtual ~2 min vs a bare-metal ~15 min). Zero
	// means "use the per-provider default" — availability.Normalize fills it from
	// provider.ProvisioningEstimate. Surfaced to customers as
	// AvailableOffering.provisioning_estimate_minutes.
	ProvisioningEstimateMinutes int `json:"provisioning_estimate_minutes,omitempty"`
	// ReliabilityScore is the machine's measured uptime/reliability score
	// (0-100), nil when unscored. Populated by communitycloud.toGPUOffering
	// (community_machines.reliability_score) and by any adapter that reports
	// per-host marketplace reliability (scaled ×100); consumed by
	// availability.ToAvailableOffering as the honest AvgUptimePct. omitempty
	// keeps it absent from every provider that doesn't measure it, and the
	// value is a bare number that carries no supplier or machine identity
	// (TestPrivacyCIGuard stays green, T-60-14). Display metadata only: it
	// must never enter any poll/ranking/selection path (D-60-04).
	ReliabilityScore *float64 `json:"reliability_score,omitempty"`
	// CapacityClass is the offering's per-OFFERING secure/community
	// classification (CapacityClassSecure | CapacityClassCommunity), for
	// providers that straddle the line WITHIN one catalog (for example a
	// supplier that prices spot off both a secure and a community pool, or a
	// marketplace whose hosts are a mix of datacenters and individuals). Empty means "derive from
	// the provider name" (provider.CapacityClassFor) — the pre-existing
	// provider-level rule — so adapters that are uniformly one class need not
	// stamp anything. Like InstantBoot this is an ATTRIBUTE that survives the
	// privacy strip, never provider identity.
	CapacityClass string `json:"capacity_class,omitempty"`
	// VMCapable marks that at least one live host backing this offering can
	// hand off a full virtual machine (fail-closed: an absent or unverified
	// VM flag classifies false). Like CapacityClass it
	// is a per-OFFERING attribute that survives the privacy strip.
	//
	// INGESTION ONLY for now: no VM launch path exists for marketplace
	// providers, so this must NOT feed DeploymentType — that would flip the
	// catalog's VM badge and the engine's bootstrap mode (driver install +
	// docker-run) for capacity we can only launch as containers.
	VMCapable bool `json:"vm_capable,omitempty"`
	// UpstreamGPUID is the provider-NATIVE GPU identifier backing this exact
	// offering row (e.g. "NVIDIA A100-SXM4-80GB").
	// Several native ids can normalize to one canonical GPUType (A100 PCIe +
	// SXM4 → a100_80gb), and the variants are stocked independently — so a
	// launch that reverse-maps the canonical type can request a variant the
	// advertised datacenter does not stock and fail "no capacity" forever
	// (the 2026-08-03 A100/us-west incident). Carrying the native id lets the
	// engine launch exactly the variant whose stock was advertised. Empty for
	// adapters whose catalog has no variant ambiguity — the adapter's own
	// reverse mapping remains the fallback. Server-side only: this field is
	// never copied onto the customer wire shape (availability.AvailableOffering).
	UpstreamGPUID string `json:"upstream_gpu_id,omitempty"`
	// RAMFloorGuaranteed and VCPUFloorGuaranteed mark that RAMGB / CPUCores came
	// from LIVE upstream data scoped to THIS row's capacity class — i.e. the
	// adapter is willing to have that figure enforced as a placement floor at
	// launch (ProvisionRequest.MinRAMPerGPUGB / .MinVCPUPerGPU). False means the
	// number is display-only: good enough to show, not sound enough to reject
	// hosts with.
	//
	// Deliberately PER FIELD rather than one flag, because an adapter can stand
	// behind one figure and not the other: both RAMGB and CPUCores may be live
	// data, yet the adapter's placement may enforce a RAM predicate and no vCPU
	// one, so there is nothing behind a core floor at placement time. One boolean cannot
	// express "RAM yes, vCPU no", and a prose caveat is not a guard: the next
	// person to add a core-count filter would reasonably trust the vouch and take
	// the whole marketplace dark. The type enforces it instead.
	//
	// The distinction is not pedantic. An adapter may resolve specs through
	// several tiers (live data, a last-known-good cache, a hand-maintained table
	// of proxied guesses), and its live query may be scoped to a different
	// capacity class than the row launches into. Enforcing a
	// cached, guessed, or wrong-cloud figure would filter out every host that
	// actually exists and take an in-stock SKU dark — converting a wrong-spec bug
	// into a total launch failure. Only the live class-matched tier sets this.
	//
	// Fail-closed by default: an adapter that says nothing gets no floor, which is
	// exactly the pre-existing behavior.
	RAMFloorGuaranteed  bool `json:"ram_floor_guaranteed,omitempty"`
	VCPUFloorGuaranteed bool `json:"vcpu_floor_guaranteed,omitempty"`
}

// Capacity classes an offering (and a provision request) can carry. Secure =
// datacenter-operated hardware; community = supplier/peer-hosted hardware with
// a softer SLA. The class is the customer-visible signal — provider identity
// never is.
const (
	CapacityClassSecure    = "secure"
	CapacityClassCommunity = "community"
)

// ProvisionRequest contains the parameters for provisioning a new instance.
type ProvisionRequest struct {
	InstanceID string       `json:"instance_id"`
	GPUType    GPUType      `json:"gpu_type"`
	GPUCount   int          `json:"gpu_count"`
	Tier       InstanceTier `json:"tier"`
	Region     string       `json:"region"`
	// DatacenterLocation is the provider-specific datacenter id (e.g., "US-TX-3")
	// that the engine passes through from a matched GPUOffering. Optional — empty
	// means "any DC", preserving backward compatibility.
	// REGION-02: enables pinning an instance to a specific datacenter.
	DatacenterLocation string   `json:"datacenter_location,omitempty"`
	SSHPublicKeys      []string `json:"ssh_public_keys"`
	DockerImage        string   `json:"docker_image,omitempty"`
	InternalToken      string   `json:"internal_token"`
	CallbackURL        string   `json:"callback_url"`
	StartupScript      string   `json:"startup_script,omitempty"`
	BidPricePerGPU     float64  `json:"bid_price_per_gpu,omitempty"` // spot bid price (provider's raw price, pre-markup)
	// Ports, Env, and ContainerDiskGB carry a deployable-application template's
	// provisioning shape (TMPL-06). They are populated by the catalog → engine
	// (Plan 06) and rendered into the provider's create call. All optional —
	// empty preserves the raw-instance behavior (SSH-only port, default disk).
	// ContainerDiskGB is also the general instance-disk request: the engine
	// fills it from LAUNCH_DEFAULT_DISK_GB on every launch, so adapters that
	// honor it (see ConfigurableDisk) rarely see a zero. Adapters reached
	// outside the engine — live tests, reconcile — still fall back to
	// FallbackDiskGB.
	Ports           []Port   `json:"ports,omitempty"`
	Env             []EnvVar `json:"env,omitempty"`
	ContainerDiskGB int      `json:"container_disk_gb,omitempty"`
	// FRPRemotePort is the engine-allocated FRP remote port for this instance
	// (additive per research A3). ONLY the community adapter reads it — it persists
	// the port at claim time so GetStatus/Terminate can dial the customer-facing
	// FRP endpoint for restart-durable independent confirmation (D-59-01). Every
	// other adapter ignores the field, so zero is a no-op and behavior is
	// unchanged for every other provider.
	FRPRemotePort int `json:"frp_remote_port,omitempty"`
	// CapacityClass narrows WHERE the instance may land when the matched
	// offering was class-specific (CapacityClassSecure | CapacityClassCommunity):
	// an adapter whose catalog straddles both classes routes the launch to the
	// matching pool. Adapters that are uniformly one class ignore it. Empty
	// preserves each adapter's legacy behavior, so every existing caller is
	// byte-for-byte unchanged.
	CapacityClass string `json:"capacity_class,omitempty"`
	// UpstreamGPUID pins the launch to the provider-native GPU variant the
	// matched offering advertised (GPUOffering.UpstreamGPUID, forwarded per
	// attempt by the engine like CapacityClass). Adapters that read it MUST
	// validate it maps to GPUType and fall back to their own reverse mapping
	// otherwise — it is placement routing, never trusted input for a different
	// GPU. Empty preserves each adapter's legacy reverse-map behavior.
	UpstreamGPUID string `json:"upstream_gpu_id,omitempty"`
	// PinnedImageRef is the server-controlled, digest-pinned container image
	// (contains "@sha256:") resolved from the GPU.ai manifest for this GPU
	// (Phase 63, D-63-07). It is NEVER derived from customer input (V5 / T-63-02
	// supply-chain tamper): when non-empty it MUST override DockerImage so a
	// container-native provider launches exactly the certified image and cannot
	// be tricked into booting a customer-supplied one. Empty = no manifest pin;
	// the adapter keeps its existing DockerImage/default behavior unchanged.
	PinnedImageRef string `json:"pinned_image_ref,omitempty"`
	// RegistryAuth carries the decrypted private-registry login for a
	// customer-image launch (Phase 2). NEVER serialized (json:"-") and never
	// logged. nil for public images and every non-custom-image launch —
	// adapters that don't read it are byte-for-byte unchanged.
	RegistryAuth *RegistryAuth `json:"-"`

	// OSImage is the provider-NATIVE boot image slug/alias/display name for a
	// raw_vm:<os> launch (the OS picker), resolved SERVER-SIDE by the engine
	// from the manifest's vm_images map — never customer input (the customer
	// sends only our os id; V5). Empty keeps each adapter's default boot image,
	// so every existing path is byte-for-byte unchanged. Only VM adapters read
	// it; container-native adapters ignore it.
	OSImage string `json:"os_image,omitempty"`
	// MinRAMPerGPUGB and MinVCPUPerGPU are the host-spec FLOORS the launch must
	// satisfy, expressed per GPU (Offering.RAMGB / Offering.CPUCores divided by
	// the offering's GPU count, mirroring BidPricePerGPU). The engine forwards
	// them from the matched offering so the machine we hand over actually
	// delivers the CPU/RAM the customer was shown on the card.
	//
	// Without a floor, marketplace-style upstreams treat the advertised figure
	// as an observation, not a contract: a catalog RAM figure resolved as a
	// cross-DC minimum across configs, with launches placed on any host carrying
	// the GPU, let a card advertising 258GB deliver 125GB and OOM-kill the job
	// (2026-08-07). Adapters whose RAM comes from a named
	// flavor/plan that the launch then requests by id already guarantee the spec
	// structurally and ignore these fields; zero is a no-op everywhere, so every
	// pre-existing caller is byte-for-byte unchanged.
	MinRAMPerGPUGB int `json:"min_ram_per_gpu_gb,omitempty"`
	MinVCPUPerGPU  int `json:"min_vcpu_per_gpu,omitempty"`
	// MinDiskGB is the host-storage FLOOR for an EXPLICIT customer disk request:
	// the engine sets it (to the resolved instance disk) only when the customer
	// sent disk_gb, and adapters with per-host placement must skip hosts whose
	// storage cannot fit it. It exists because "accept and clamp" is a real
	// upstream behavior, not a hypothetical: a marketplace upstream accepted a
	// create whose disk exceeded the host's capacity, billed the requested size,
	// and silently capped the container at what the host had (2026-08-11 prod
	// battery, --disk 150 on a 134GB host). Unlike ContainerDiskGB — which is filled on EVERY launch
	// and is a payload, not a constraint — this field is a placement predicate.
	// Zero (every default launch, every non-reading adapter) is a no-op for
	// THIS field; an adapter may still independently floor placement on the
	// disk its create will request, so default launches are floored too
	// (GPU-73).
	MinDiskGB int `json:"min_disk_gb,omitempty"`
	// MinCUDA is the CUDA version the image being launched requires (the
	// template catalog's min_cuda, read from the image's own
	// NVIDIA_REQUIRE_CUDA). Like MinDiskGB this is a PLACEMENT PREDICATE, not
	// a payload: adapters with per-host placement must skip hosts whose driver
	// cannot run the image. A provider-wide driver floor is a property of the
	// pool, not a promise about any one image — a host can clear that baseline
	// and still be too old for the image that lands on it, in which case the
	// container refuses to start with "the NVIDIA driver on your system is too
	// old" and which host you drew decides whether your launch works
	// (GPU-111/GPU-112). Zero (every non-template launch, every adapter that
	// does not read it) is a no-op, so existing behavior is unchanged.
	//
	// It matters for adapters whose hosts are a heterogeneous marketplace,
	// where a per-host driver differs from the pool's baseline. Adapters that
	// ignore the field give a template no CUDA floor. Do not read this as a
	// platform-wide guarantee until every adapter honours it.
	MinCUDA float64 `json:"min_cuda,omitempty"`
	// ExcludeUpstreamHostIDs lists provider-native host ids (the vocabulary of
	// InstanceStatus.UpstreamHostID) the adapter must NOT place this instance
	// on. Populated only by the engine's boot-stall rescue path, which retries
	// a stalled launch while excluding the host that just proved itself
	// broken — without this, cheapest-first offer selection would immediately
	// re-pick the same stalled machine. Adapters without per-host placement
	// ignore it; empty is a no-op everywhere.
	ExcludeUpstreamHostIDs []string `json:"exclude_upstream_host_ids,omitempty"`
	// MaxCostPerHour is the WHOLESALE whole-node ceiling ($/hr, same basis as
	// GPUOffering.PricePerHour) this launch may cost us. It exists because a
	// catalog row is a QUOTE, not a booking: on per-machine marketplace supply
	// the adapter re-searches at accept time and the ask that anchored the row
	// is routinely gone, so cheapest-available can land far above the price the
	// customer was shown (live: +109% on a 3090, 2026-08-13). Retail is fixed
	// from the catalog row, so every dollar of that drift comes out of margin.
	//
	// An adapter that honors it MUST refuse to accept an offer above the
	// ceiling and return *CostCeilingError carrying the cheapest placeable
	// price, which the engine turns into the price-changed re-quote the
	// customer confirms against. Zero disables the ceiling entirely, so
	// adapters that ignore the field — and every non-engine caller (live
	// tests, reconcile) — are byte-for-byte unchanged.
	MaxCostPerHour float64 `json:"max_cost_per_hour,omitempty"`
}

// ProvisionResult is returned after a successful provisioning call.
type ProvisionResult struct {
	UpstreamID            string  `json:"upstream_id"`
	UpstreamIP            string  `json:"upstream_ip"`
	Provider              string  `json:"provider"`
	Status                string  `json:"status"`
	EstimatedReadySeconds int     `json:"estimated_ready_seconds"`
	CostPerHour           float64 `json:"cost_per_hour"`
	DatacenterLocation    string  `json:"datacenter_location"`
	Region                string  `json:"region"`
}

// Port is a container port the template exposes. Protocol is "http" (vhost-
// proxied to *.apps.example.com) or "tcp" (e.g. SSH on 22). Exactly one port in a
// template's set is Primary (the app's main web port).
type Port struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Primary  bool   `json:"primary"`
}

// RegistryAuth is a private-registry login for a CUSTOMER-image launch
// (custom images Phase 2), decrypted just-in-time by the create handler.
// Password is a live secret: the struct is excluded from JSON serialization
// entirely (`json:"-"` on the ProvisionRequest field) and must never be
// logged; adapters map it to their provider's native mechanism (for
// example a registry-auth object or an image-login field) and drop it.
type RegistryAuth struct {
	Registry string
	Username string
	Password string
}

// EnvVar is an environment variable injected into the provisioned container.
// Secret drives downstream log/metadata scrubbing (TMPL-13): Secret==true
// values (e.g. HF tokens, generated app basic-auth) are redacted before any
// persistence or log line.
type EnvVar struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret,omitempty"`
}

// PortMapping represents a port mapping on an upstream instance.
type PortMapping struct {
	IP          string `json:"ip"`
	PrivatePort int    `json:"private_port"`
	PublicPort  int    `json:"public_port"`
	Protocol    string `json:"protocol"`
}

// ProvisionedInstance is a single live entry returned by Provider.ListProvisioned.
// Used by the reconciler to detect orphan upstream instances — instances that
// the provider still has running but our DB does not track (or has marked as
// terminated). The Name field carries the tag we set during Provision (= our
// internal instance UUID) so the reconciler can correlate without ambiguity.
type ProvisionedInstance struct {
	UpstreamID string `json:"upstream_id"`
	Name       string `json:"name"`             // tag we set during Provision; expected to be a UUID (our InstanceID)
	Region     string `json:"region,omitempty"` // native upstream region; empty for providers without regional routing
	Status     string `json:"status,omitempty"` // upstream's view: running/starting/stopping/etc.
}

// InstanceStatus represents the current state of an upstream instance.
type InstanceStatus struct {
	UpstreamID    string        `json:"upstream_id"`
	Status        string        `json:"status"` // creating, running, stopping, terminated, error
	IP            string        `json:"ip,omitempty"`
	CreatedAt     *time.Time    `json:"created_at,omitempty"`
	CostPerHour   float64       `json:"cost_per_hour,omitempty"`
	UptimeSeconds int           `json:"uptime_seconds,omitempty"`
	Ports         []PortMapping `json:"ports,omitempty"`
	// BootDetail is the upstream's free-form boot-progress message (for
	// example trimmed live docker-pull output while the container loads).
	// The engine's boot-stall watchdog treats a CHANGING BootDetail as proof
	// the boot is progressing (a 12+ min image pull on a fat pipe succeeds and
	// must not be killed) and an unchanged one as a dead pull. Empty for
	// adapters without boot telemetry — the watchdog only reads it from
	// adapters declaring BootRescueCapability.
	BootDetail string `json:"boot_detail,omitempty"`
	// UpstreamHostID is the opaque provider-native id of the PHYSICAL host
	// backing this instance (on a marketplace one machine can list many offers,
	// so the contract id alone cannot identify the hardware). The engine feeds
	// it back as ProvisionRequest.ExcludeUpstreamHostIDs when a rescue
	// reprovision must avoid the host that just stalled. Internal-only, never
	// serialized toward customers (CustomerInstance has no such field).
	UpstreamHostID string `json:"upstream_host_id,omitempty"`
}

// CustomerInstance is the customer-facing representation of an instance.
// It structurally excludes all upstream provider details (defense by omission).
// If a field does not exist here, it can never leak via json.Marshal.
//
// ReservationID (Plan 30-06) surfaces the **contract_id** of the reservation
// this instance is bound to, or nil for on-demand / spot. The frontend uses
// this to render the reserved badge and to suppress per-second cost copy in
// favor of "Included in reservation". Capacity_id is an internal sharding
// detail and is intentionally NOT exposed — see Instance.ReservationContractID
// for how it's populated via JOIN at marshal time.
type CustomerInstance struct {
	ID            string    `json:"id"`
	Hostname      string    `json:"hostname"`
	SSHCommand    string    `json:"ssh_command"`
	Status        string    `json:"status"`
	GPUType       string    `json:"gpu_type"`
	GPUCount      int       `json:"gpu_count"`
	Tier          string    `json:"tier"`
	Region        string    `json:"region"`
	PricePerHour  float64   `json:"price_per_hour"`
	ReservationID *string   `json:"reservation_id"`
	CreatedAt     time.Time `json:"created_at"`
}

// Instance represents the full internal instance with upstream details.
// Only used by provisioning engine and DB layer -- never serialized to customers.
//
// ReservationContractID is populated at read-time via a JOIN through
// reservation_capacity (Plan 30-06). It is exported only so the DB layer can
// scan into it directly, but the struct itself is internal-only — it has no
// json tags, so nothing here leaks through the public API surface. The
// customer-facing view is CustomerInstance.ReservationID, populated by
// ToCustomer below.
type Instance struct {
	InstanceID            string
	OrgID                 string
	UserID                string
	UpstreamProvider      string
	UpstreamID            string
	UpstreamIP            string
	Hostname              string
	WGPublicKey           string
	WGAddress             string
	GPUType               string
	GPUCount              int
	Tier                  string
	Region                string
	PricePerHour          float64
	Status                string
	ReservationContractID *string
	CreatedAt             time.Time
}

// ToCustomer converts an internal Instance to a customer-safe representation.
// Upstream provider, ID, and IP are structurally absent from the result.
// ReservationContractID is surfaced as ReservationID (Plan 30-06) — the
// customer is told which reservation contract their instance belongs to, not
// which capacity slot (an internal sharding detail).
func (i *Instance) ToCustomer() CustomerInstance {
	return CustomerInstance{
		ID:            i.InstanceID,
		Hostname:      i.Hostname,
		SSHCommand:    fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null root@%s", i.Hostname),
		Status:        i.Status,
		GPUType:       i.GPUType,
		GPUCount:      i.GPUCount,
		Tier:          i.Tier,
		Region:        i.Region,
		PricePerHour:  i.PricePerHour,
		ReservationID: i.ReservationContractID,
		CreatedAt:     i.CreatedAt,
	}
}
