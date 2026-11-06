// Package provision orchestrates the instance provisioning flow.
package provision

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/manifest"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/spotrouter"
	"github.com/gpuai-dev/gpuai-oss/internal/tunnel"
	"github.com/jackc/pgx/v5/pgxpool"
)

// engineDB is the narrow database surface the provisioning engine needs. It
// lists EXACTLY the *db.Pool methods the engine calls — no more, no less.
// *db.Pool satisfies this in production (deps.DB is a *db.Pool, assigned to the
// engineDB field in NewEngine); the package's tests inject a fakeEngineDB
// (engine_seam_test.go) so failure modes that would otherwise need a live
// Postgres — a failing CreateInstance (BLOCK-02), a synchronous billing-session
// assertion (BLOCK-07), a scripted GetInstance for the timeout path (BLOCK-04) —
// become unit-testable.
//
// This mirrors the proven tickerDB pattern (internal/billing/ticker.go:23): a
// private lowercase interface held as the struct field type, with a
// compile-time `var _ engineDB = (*db.Pool)(nil)` guard immediately below that
// fails the build if any production DB signature drifts from the interface.
//
// PgxPool() *pgxpool.Pool is included because the engine opens the FRP
// port-allocation transaction through it (Provision step 6). Port-tx behavior
// is exercised by the real-DB harness, not the fake, so fakeEngineDB may return
// nil from PgxPool.
type engineDB interface {
	// Instance lifecycle reads + writes.
	CreateInstance(ctx context.Context, inst *db.Instance) error
	GetInstance(ctx context.Context, instanceID string) (*db.Instance, error)
	UpdateInstanceStatus(ctx context.Context, instanceID, fromStatus, toStatus string) (bool, error)
	SetInstanceRunning(ctx context.Context, instanceID, upstreamIP string) (updated bool, prevStatus string, err error)
	SetInstanceUpstreamIP(ctx context.Context, instanceID, upstreamIP string) error
	// SetInstanceSSHPort records a direct-connect instance's public SSH port.
	SetInstanceSSHPort(ctx context.Context, instanceID string, port int) error
	// SetInstanceError flips a row to error and records the reason. updated
	// reports whether the row actually changed (guarded against already-error
	// / terminated rows) — it is the exactly-once gate for instance.failed
	// webhook emission (GPU-45).
	SetInstanceError(ctx context.Context, instanceID, reason string) (updated bool, err error)
	// SetInstanceStatusReason records a customer-facing reason WITHOUT changing
	// status — used by the spending-cap / balance stops, which terminate rows
	// deliberately and must still explain themselves in the dashboard.
	SetInstanceStatusReason(ctx context.Context, instanceID, reason string) error
	// RebindInstanceUpstream re-points a mid-provision row at a rescue's
	// replacement upstream (bootstall.go); false = the row left its
	// mid-provision state concurrently and the caller must tear the
	// replacement down.
	RebindInstanceUpstream(ctx context.Context, instanceID, upstreamProvider, upstreamID, region string, pricePerHour, upstreamPricePerHour float64, diskGB *int) (bool, error)
	TerminateInstance(ctx context.Context, instanceID string) (bool, error)
	ListRunningInstancesByOrg(ctx context.Context, orgID string) ([]db.Instance, error)
	// ListStuckProvisions backs the durable reaper (AUDIT-05-001/002): instances
	// in a pre-ready state (creating/provisioning/booting) older than the cutoff.
	ListStuckProvisions(ctx context.Context, cutoff time.Time) ([]db.Instance, error)
	// ListInstancesDueForAutoTerminate / ClaimInstanceAutoTerminate back the
	// per-instance runtime bound (schema v92, GPU-60). The claim clears
	// auto_terminate_at under the same due-predicate that selected the row, so
	// it returns true to exactly one caller per armed deadline — the
	// exactly-once gate for SweepAutoTerminate.
	ListInstancesDueForAutoTerminate(ctx context.Context, now time.Time, limit int) ([]db.Instance, error)
	ClaimInstanceAutoTerminate(ctx context.Context, instanceID string, now time.Time) (bool, error)
	// RearmInstanceAutoTerminate restores a spent claim when the terminate it
	// was spent on failed with the row still running, so the next sweep retries.
	RearmInstanceAutoTerminate(ctx context.Context, instanceID string, at time.Time) (bool, error)

	// Bootstrap staging (fetched at boot via /internal/instances/{id}/bootstrap).
	StageInstanceBootstrap(ctx context.Context, instanceID, script string) error

	// Instance event log.
	CreateInstanceEvent(ctx context.Context, event *db.InstanceEvent) error

	// RecordProvisionFailure appends one row to the v82 provision-failure ledger.
	// The engine calls it ONLY for pre-instance rejections — failures that never
	// created an instance row and so leave no other trace. Post-instance failures
	// are recorded inside SetInstanceError itself, off its exactly-once flip.
	// Best-effort at every call site: the error is logged and dropped.
	RecordProvisionFailure(ctx context.Context, ev db.ProvisionFailureEvent) error

	// RecordProvisionAttempt appends one row to the v112 per-attempt ledger —
	// the finer grain the v82 ledger above deliberately excludes. Written once
	// per candidate the walk reaches, plus once when a walk ends without
	// reaching another candidate. Best-effort at every call site: the error is
	// logged and dropped, and a nil DB records nothing.
	RecordProvisionAttempt(ctx context.Context, a db.ProvisionAttempt) error

	// SSH key resolution.
	GetSSHKeysByIDs(ctx context.Context, ids []string) ([]db.SSHKey, error)
	GetSSHKeysByOrgID(ctx context.Context, orgID string) ([]db.SSHKey, error)
	ListSSHKeysByOrgAndUser(ctx context.Context, orgID, userID string) ([]db.SSHKey, error)

	// Raw pool access for the FRP port-allocation transaction (Provision step 6).
	PgxPool() *pgxpool.Pool
}

// Compile-time guard: signature drift in any *db.Pool method the engine relies
// on breaks the build here (same mechanism as tickerDB), not a goroutine at
// runtime.
var _ engineDB = (*db.Pool)(nil)

// ProviderCandidate pairs a provider with a matching offering for price-sorted selection.
type ProviderCandidate struct {
	Prov     provider.Provider
	Offering provider.GPUOffering
}

// CapKind values. "balance" separates a prepaid-balance depletion from a
// spending-cap breach: both stop instances, but they are different events and
// must not describe themselves to the customer with the same words.
const (
	CapKindOrg     = "org"
	CapKindMember  = "member"
	CapKindBalance = "balance"
)

// ProvisionRequest contains the parameters for provisioning a new instance.
// This is the engine-level request, distinct from provider.ProvisionRequest.
type ProvisionRequest struct {
	OrgID  string
	UserID string // internal user UUID from users.user_id (not an external id)
	// IsAdmin reports whether the launching principal is an org admin/owner
	// (authz.IsAdminPlus). It scopes SSH-key auto-include and the explicit
	// ssh_key_ids permission check (v33): admins may attach any org key, members
	// only their own. Set by the public handler from apiKey.Role.
	IsAdmin  bool
	GPUType  provider.GPUType
	GPUCount int
	Tier     provider.InstanceTier
	// Regions is the set of CANONICAL region codes (e.g. "india-south") the
	// caller is willing to provision in. Empty = any region. Membership match;
	// the engine sorts surviving candidates by price and picks cheapest. The
	// offering's NATIVE region is what flows back to the provider adapter at
	// provision time — see Provision() where provReq.Region is populated from
	// the matched offering, not from this canonical filter.
	Regions         []string
	Name            *string  // optional user display label
	SSHKeyIDs       []string // UUIDs of ssh_keys to resolve
	MaxPricePerHour *float64 // optional price cap

	// ViewedPricePerHour is the RETAIL, disk-inclusive $/hr the customer SAW on
	// the card they clicked launch from (GPU-78). The catalog row is a quote,
	// not a booking: between the card render and the launch-time re-quote the
	// marketplace can reprice, and a PINNED launch carries no price of its own
	// (price is deliberately not in the offering-id digest — GPU-55 owns that
	// seed), so before this field the customer was simply billed the new number.
	// Prod 2026-08-23: card $0.34, billed $0.41.
	//
	// It is a 409 TRIGGER ONLY. It must NEVER be fed to billedRetail's
	// approvedCap or costCeilingFor's approvedRetail. MaxPricePerHour doubles as
	// the #640 CONSENT cap, so folding this into it would turn "I saw this
	// price" into "I consent to be charged up to this price" and let untrusted
	// client input widen our authority to buy — the riskiest interaction in this
	// change, pinned by TestProvision_ViewedPricePin. The only thing it can do
	// is abort a launch; it can never raise a bill, a ceiling, or a cap.
	//
	// nil or <= 0 means absent, which reproduces today's behavior exactly. The
	// dashboard modal sends it on every pinned launch; for public-API and CLI
	// callers it is optional, so nobody who never saw a card meets a new 409.
	// Tolerance is one cent: see viewedPriceExceeded.
	ViewedPricePerHour *float64

	// Deployable-template fields (Phase 31, TMPL-06/07/11). All zero/empty for
	// a raw instance — that path is byte-for-byte unchanged. Populated by the
	// public deploy entrypoint (handleCreateInstance) from the resolved
	// catalog.TemplateSpec after the env-override allowlist check.
	//
	// TemplateID flags the deploy as templated (persisted on the instance row
	// so reads can derive app_url and the app-ready gate can defer the
	// booting->running flip). Image/Ports/Env/ContainerDiskGB are threaded into
	// provider.ProvisionRequest (the adapter renders them). AppPort != 0
	// switches the bootstrap into vhost-proxy + basic-auth mode; AppReadyTimeout
	// bounds the service warm-up health check.
	TemplateID      *string
	Image           string          // spec.Image; "" => adapter default (raw)
	Ports           []provider.Port // spec.Ports projected to provider shape
	Env             []provider.EnvVar
	ContainerDiskGB int
	AppPort         int // primary HTTP port (0 = raw, SSH-only)
	AppReadyTimeout int // spec.ReadyTimeoutSecond; 0 => engine default (300)

	// Custom-image fields (custom-images Phase 1). CustomerImage marks Image
	// as a CUSTOMER-supplied reference — already normalized, digest-pinned and
	// pre-flight-verified by the create handler (internal/imageref +
	// internal/registrycheck) — rather than a server-controlled template
	// image. It narrows placement to SupportsCustomerImage providers (a strict
	// subset of SupportsCustomImage) and makes the
	// bootstrap launch the image's own process after sshd/tunnel setup.
	// ImageEntrypoint/ImageCmd carry the image config's process defaults as
	// the pre-flight captured them; both empty => the instance is SSH-only.
	// False (with Image set) preserves the template deploy path byte-for-byte.
	CustomerImage bool
	// LockedDeploymentClass pins customer-image candidate matching to one
	// deployment class ("container" | "vm") with NO cross-class fallback. Set
	// ONLY by the engine when it stores boot-rescue state — the bootstrap has
	// rendered against the placed class by then, and a rescue onto the other
	// class would run the wrong script. Empty (every fresh launch) keeps the
	// prefer-container behavior.
	LockedDeploymentClass string
	ImageEntrypoint       []string
	// ImageConfigEntrypoint is the image manifest config's ENTRYPOINT exactly
	// as the registry pre-flight captured it, BEFORE any request override.
	// Used only for placement: providers that start the bootstrap as
	// ENTRYPOINT arguments (BootstrapRidesImageEntrypoint) cannot host an
	// image whose config defines one, regardless of what the rendered launch
	// script later execs — the provider applies the CONFIG entrypoint at
	// container start, so an API-level override cannot defuse it.
	ImageConfigEntrypoint []string
	// RegistryAuth (Phase 2) is the decrypted private-registry login for a
	// customer-image pull. Threaded verbatim to provider.ProvisionRequest
	// (which excludes it from serialization); nil for public images.
	RegistryAuth *provider.RegistryAuth
	ImageCmd     []string

	// RequestedDiskGB is the CUSTOMER's explicit instance-disk request in GB
	// (public/dashboard wire field disk_gb). nil means "didn't ask" — NOT zero,
	// which is why this is a *int like MaxPricePerHour: the distinction is
	// load-bearing rather than stylistic.
	//
	// nil  => the engine fills the server default (LAUNCH_DEFAULT_DISK_GB) and
	//         threads it as a plain hint. Providers that can configure disk
	//         honor it; the rest ignore it. Placement is NOT narrowed, so the
	//         candidate set for every launch that omits the field is
	//         byte-for-byte what it was before this feature existed. That
	//         property is the safety guarantee of the whole change and is
	//         pinned by TestSelectProviderCandidates_NoDiskRequestDoesNotNarrow.
	// set  => the customer asked for a specific size, so we must either honor
	//         it or refuse. SelectProviderCandidates drops providers whose
	//         adapters can't configure disk and returns ErrDiskUnavailable when
	//         that empties the set. Handlers additionally 422 up front for a
	//         synchronous error rather than a late failure.
	//
	// The value the adapter actually receives is effectiveDiskGB(), which takes
	// max(this-or-default, ContainerDiskGB) so a template's disk acts as a
	// floor: no template can land under-spec, and no customer is refused more
	// room than the template asked for.
	RequestedDiskGB *int

	// MinVRAMGB is the template's per-GPU VRAM floor (catalog MinVRAMGB,
	// possibly raised by the GPU-40 model-size guard). It is enforced on BOTH
	// template paths:
	//
	//   - auto-select (gpu_type omitted, autoSelectsGPU): TMPL-04 eligibility —
	//     SelectProviderCandidates keeps offerings whose normalized VRAM is
	//     >= MinVRAMGB and the cheapest survivor is chosen;
	//   - explicit gpu_type (GPU-87): a type-level pre-check via
	//     TemplateVRAMShortfall against the type's canonical/normalized VRAM,
	//     which fails the request outright rather than filtering offerings.
	//
	// The GPU-COUNT floor is folded into GPUCount (the handler floors it to
	// max(1, spec.MinGPUCount)), so no separate field is needed. Zero on the raw
	// (no-template) path, where there is no floor to enforce.
	MinVRAMGB int

	// MinCUDA is the template's CUDA floor (catalog MinCUDA, read from the
	// image's own NVIDIA_REQUIRE_CUDA). Unlike MinVRAMGB it is NOT enforced
	// here against catalog offerings, because it cannot be: an offering is an
	// aggregate over a price/region group, while the actual HOST — and so the
	// driver — is chosen inside the adapter at accept time. So it travels to
	// the adapter as a placement predicate instead (GPU-112). Zero on the raw
	// (no-template) path.
	MinCUDA float64

	// SelectedOfferingID pins placement to one exact offering (D-63-08). "" =>
	// legacy price-sorted placement (byte-for-byte unchanged). When non-empty,
	// SelectProviderCandidates must return ONLY the offering whose deterministic
	// ID equals this value; if that offering is no longer available it must
	// return an explicit error and NEVER silently substitute a cheaper offering.
	// Mirrors the requireCustomImage eligibility gate (SelectProviderCandidates,
	// ~line 2049): a set field narrows the candidate set rather than widening it.
	SelectedOfferingID string

	// PinnedProvider restricts placement to one provider by registry name
	// (STAFF DEV TOOLS ONLY — e.g. UAT-ing a freshly wired adapter without
	// waiting for it to win the price sort). "" => normal cheapest-first
	// placement, byte-for-byte unchanged. Like SelectedOfferingID it only ever
	// NARROWS the candidate set; when the pinned provider has no matching
	// capacity the provision fails explicitly rather than falling back to
	// another provider. The dashboard handler accepts the wire field ONLY on
	// the staff-gated dev-tools launch route (AdminEmailFromContext must be
	// present) — customers can never route themselves to a named provider.
	PinnedProvider string

	// ExcludeProviders removes named providers from candidate selection.
	// INTERNAL ONLY — never populated from the wire. Set by the
	// upstream-terminal failover rescue (progressStatus) so the re-match can
	// never land back on the provider whose scheduler just killed the launch:
	// that provider's catalog may still be advertising the phantom capacity
	// that caused the failure (live 2026-09: a VM supplier's stock endpoint reported
	// stock its scheduler could not place, so every create died in ERROR), and
	// cheapest-first selection would re-pick it immediately.
	ExcludeProviders []string

	// Environment is the SERVER-VALIDATED environment class (Phase 63, D-63-04):
	// "certified" (GPU.ai Certified Image), "raw_vm" (explicit root VM), or
	// "provider_template" (provider's own fixed template). "certified" may carry
	// a framework sub-choice as "certified:<framework>" (D-64-01, e.g.
	// "certified:pytorch") — the engine resolves the framework family's pinned
	// image from the manifest exactly like the base. "" => legacy path,
	// byte-for-byte unchanged (no env-field population). The public handler sets
	// this only AFTER re-validating the client's hint against the placed
	// provider's capability (SupportsRawVM / CertifiedImageCapable); the engine
	// re-derives the concrete BootstrapData/adapter fields from the
	// server-controlled manifest here (V5 — never customer input).
	Environment string

	// FleetProbe marks a fleet-probe launch (internal/fleetprobe): the
	// bootstrap downloads the on-host probe from
	// /internal/instances/{id}/probe, runs it and posts its report back there.
	// Never set from a customer request.
	FleetProbe bool

	// AutoTerminateHours bounds THIS instance's runtime (GPU-60): the platform
	// terminates it that many hours after it becomes running, through the same
	// path a user terminate takes. nil (every launch that omits the field, and
	// every pre-feature caller) means no bound — byte-for-byte the previous
	// behavior.
	//
	// Persisted on the row and enforced by a server-side sweep, deliberately:
	// the case it exists for is an unattended agent that crashes after
	// launching, and any timer living in the client dies with the process that
	// set it. Bounds are validated at the API edge (1..autoTerminateMaxHours);
	// the engine only records what it is given.
	AutoTerminateHours *int
}

// ProvisionResponse is returned after a successful provisioning call.
//
// AppBasicAuthUser / AppBasicAuthPass are populated whenever the instance has
// a vhost-proxied surface: a template app (req.AppPort != 0), a web console
// (v76), or both — the two share one per-instance credential. They carry the
// credential up to the create-FSM (runCreateInstance), which surfaces it on
// the operation RESULT (D6; since v74 the credential also persists on the
// instance row for org-scoped re-reads). AppURL is template-deploys-only;
// TerminalURL is set whenever the web console was provisioned.
type ProvisionResponse struct {
	InstanceID   string
	Hostname     string
	PricePerHour float64
	Status       string
	// ProvisioningBudget is how long the engine lets this launch take to
	// reach running before it reaps the upstream and errors the row. The
	// fleet prober waits at least this long, so the engine, not a shorter
	// caller deadline, decides that a boot failed. Internal; never rendered.
	ProvisioningBudget time.Duration

	AppURL           string // https://<id>.<appsHost> (template deploys only)
	TerminalURL      string // https://<id>-term.<appsHost> (web console, v76)
	AppBasicAuthUser string // D6: per-instance generated; surfaced via the operation result
	AppBasicAuthPass string // D6: SECRET — never logged
}

// EngineDeps holds the dependencies for the provisioning engine.
// Follows the project's ServerDeps constructor injection pattern.
type EngineDeps struct {
	Registry *provider.Registry
	DB       *db.Pool
	Config   *config.Config
	Logger   *slog.Logger
	// Optional: set to nil if FRP tunneling not configured.
	TunnelMgr *tunnel.Manager
	// SSHReady, in direct-connect mode, decides whether an instance's SSH
	// endpoint answers before it is reported running. Nil skips the check.
	SSHReady func(ctx context.Context, ip string, port int) bool
	// OnStatusChange is called when progressStatus changes an instance's state.
	// Used to publish SSE events. Nil means no notification.
	OnStatusChange func(instanceID, status string)
	// AssetStore presigns short-lived GET URLs for the bootstrap-assets
	// bucket (the frpc tarball mirror). Optional: nil => bootstraps render
	// without a mirror URL and download frpc from the GitHub release only.
	// *storage.Store satisfies it; wired in cmd/gpuctl/deps.go when
	// BOOTSTRAP_ASSETS_BUCKET is configured.
	AssetStore assetPresigner
	// LaunchBreaker is the launch-outcome breaker (availability.LaunchKey
	// docs) shared with the availability poller. Optional: nil disables
	// outcome-based suppression entirely (existing behavior, and the state
	// every directly-constructed test Engine gets). The engine both FEEDS it
	// (upstream-terminal boot failure / first upstream-running per launch)
	// and CONSULTS it (candidate selection skips non-Allow'd offerings).
	LaunchBreaker *availability.Breaker
	// FundingGate is the provider-level "account unfunded" circuit
	// (availability/fundinggate.go), shared with the availability poller and
	// the supplier balance poller. The engine TRIPS it when a create is
	// refused with provider.ErrAccountUnfunded and CONSULTS it at candidate
	// selection. nil disables (every directly-constructed test Engine).
	FundingGate *availability.FundingGate
}

// assetPresigner is the narrow storage surface the engine needs to mint a
// bootstrap-time mirror URL for the pinned frpc tarball. A private interface
// (like flagsChecker) so the engine does not import internal/storage; the
// deps.go assignment is the compile-time check.
type assetPresigner interface {
	PresignGetObject(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// progressStatusStore is the narrow DB seam the poll-loop booting->running
// transition needs: read the instance (to learn its TemplateID) and record the
// provider-reported IP. The booting->running flip itself is owned by the ready
// callback (handlers_internal.go) for raw and template instances alike, so
// SetInstanceRunning stays in the seam only for interface stability. *db.Pool
// satisfies it in production; the two-writer-race unit test wires a spy so the
// defer behavior is asserted without a live Postgres.
type progressStatusStore interface {
	GetInstance(ctx context.Context, instanceID string) (*db.Instance, error)
	SetInstanceRunning(ctx context.Context, instanceID, upstreamIP string) (updated bool, prevStatus string, err error)
	SetInstanceUpstreamIP(ctx context.Context, instanceID, upstreamIP string) error
	SetInstanceSSHPort(ctx context.Context, instanceID string, port int) error
}

// Compile-time guard: the production *db.Pool satisfies the poll-loop seam, so
// pollStore()'s nil-fallback to e.db is type-safe.
var _ progressStatusStore = (*db.Pool)(nil)

// Engine orchestrates instance provisioning and termination.
// It coordinates provider adapters, FRP tunneling, and DB persistence.
type Engine struct {
	registry  *provider.Registry
	db        engineDB
	config    *config.Config
	logger    *slog.Logger
	tunnelMgr *tunnel.Manager
	// directConnect is set by NewEngine when no FRP tunnel manager is
	// configured: instances are reached at the supplier's address, and the
	// poll loop (not a bootstrap callback) owns the booting->running flip.
	// A directly-constructed test Engine leaves it false.
	directConnect bool
	// sshReady gates the direct-connect running flip; see EngineDeps.SSHReady.
	sshReady       func(ctx context.Context, ip string, port int) bool
	onStatusChange func(instanceID, status string)
	// eventSink receives customer-facing instance lifecycle webhook events
	// (GPU-45). Injected post-construction via SetInstanceEventSink (same
	// pattern as SetOnStatusChange, and for the same reason: the concrete
	// publisher lives in internal/api/public, which imports this package).
	// Nil means no webhook publishing — every emission site is nil-guarded.
	eventSink InstanceEventSink
	// pollStore is the DB seam for the poll-loop booting->running transition
	// (Plan 31-08, TMPL-10 two-writer race). nil in production -> the loop
	// uses the concrete *db.Pool above; the race unit test injects a spy.
	pollStore progressStatusStore
	// provisionTimeout caps how long progressStatus polls a booting instance
	// before declaring a provisioning timeout. NewEngine defaults it to
	// 10*time.Minute (byte-identical to the prior hardcoded literal); the
	// BLOCK-04 timeout test sets it to a few milliseconds directly on the
	// struct (same override pattern as `reservation`), so the timeout path is
	// asserted without a 10-minute test. Zero production behavior change.
	provisionTimeout time.Duration
	// Boot-stall watchdog knobs (see bootstall.go). NewEngine fills them from
	// config; a directly-constructed test Engine leaves them zero, which
	// disables the watchdog entirely — existing tests are untouched.
	tunnelStallAfter   time.Duration
	pullStallAfter     time.Duration
	bootRescueAttempts int
	// defaultEnvironment is applied to launches that name no environment,
	// image or template (see defaultEnvironmentFor), on container-native
	// placements only (see placementEnvironment). Empty keeps the legacy
	// adapter defaults; a directly-constructed test Engine leaves it empty.
	defaultEnvironment string
	// defaultEnvironmentAll widens the default from GPUs the legacy images
	// cannot run (Blackwell) to every manifest-covered GPU.
	defaultEnvironmentAll bool
	// gpuCheckMode renders the bootstrap's startup GPU compute check: "" (off),
	// "shadow" or "enforce". A directly-constructed test Engine leaves it off.
	gpuCheckMode string
	// offeringVerdicts are the fleet prober's per-offering verdicts
	// (internal/fleetprobe); enforceOfferingVerdicts skips delisted offerings
	// at placement. nil (the default and every test engine) skips nothing.
	offeringVerdicts        availability.OfferingVerdicts
	enforceOfferingVerdicts bool
	// fleetProbeLaunches holds the instance IDs of fleet-probe launches
	// (ProvisionRequest.FleetProbe) whose progressStatus loop is still
	// running, so that loop knows not to feed the launch-outcome breaker.
	// In-memory like the loop itself: a restart loses both at once.
	fleetProbeLaunches sync.Map
	// pollInterval is progressStatus's status-poll cadence. Zero means the
	// production default (5s); tests set it to drive the poll loop's
	// per-tick branches (e.g. the upstream-terminal failover) without
	// real-time waits.
	pollInterval time.Duration
	// launchBreaker: see EngineDeps.LaunchBreaker. nil disables.
	launchBreaker *availability.Breaker
	// fundingGate: see EngineDeps.FundingGate. nil disables.
	fundingGate *availability.FundingGate
	// hostQuarantine holds machines whose bootstrap never phoned home out of
	// placement on adapters that can exclude a machine (see
	// availability.HostQuarantine). nil (every test engine) holds nothing.
	hostQuarantine *availability.HostQuarantine
	// placement remembers offerings that keep failing to place (see
	// availability.PlacementMemory). nil (every test engine) remembers nothing.
	placement *availability.PlacementMemory
	// flags is the runtime feature-flag store (Plan 48-02). When non-nil the
	// provider-disable check resolves DB-over-env (a panel flip takes effect
	// without a redeploy); nil falls back to e.config.IsProviderDisabled, so
	// existing tests and partial wiring behave byte-identically. *admin.Store
	// satisfies flagsChecker; injected via SetFlags to avoid an import cycle.
	flags flagsChecker
	// manifest is the pinned-base-environment source (Phase 63): per-GPU driver
	// floors / CUDA minimums / digest-pinned certified image refs. Injected via
	// SetManifest post-construction (same pattern as SetFlags) because the manifest
	// and the engine are loaded/built in different places (manifest.LoadEmbedded in
	// serve.go, engine in deps.go). Reachability seam only in Wave 0 — no
	// provisioning path reads it yet — so nil is production-safe.
	manifest *manifest.Manifest
	// assetStore presigns bootstrap-asset GET URLs (frpc tarball mirror).
	// nil => no mirror URL is rendered (GitHub-only download). See
	// EngineDeps.AssetStore.
	assetStore assetPresigner
}

// flagsChecker is the narrow runtime-config surface the engine needs. *admin.Store
// satisfies it. Kept a private interface (injected via SetFlags) so the engine
// does not import internal/admin at the type level — internal/admin already
// imports internal/db/config but NOT internal/provision, and this seam keeps the
// dependency direction one-way and the engine unit-testable with a fake.
//
// Every value here is admin-dashboard-mutable and MUST be resolved through this
// seam, never through e.config directly: the config layer only sees the env, so
// an env-only read makes the admin panel's value and the engine's value diverge
// silently. MarkupPctFor is on this interface because it used to be exactly that
// bug — the catalog re-priced off a panel edit within ~30s while the BILL kept
// using the env markup forever.
type flagsChecker interface {
	IsProviderDisabled(ctx context.Context, name string) bool
	MarkupPctFor(ctx context.Context, name string) float64
	CostDiscountPctFor(ctx context.Context, name string) float64
	MinMarginPctFor(ctx context.Context, name string) float64
}

// pollBackend returns the seam the poll-loop booting->running transition
// reads/writes through. Defaults to the concrete *db.Pool.
func (e *Engine) pollBackend() progressStatusStore {
	if e.pollStore != nil {
		return e.pollStore
	}
	return e.db
}

// newEngineForTest builds a minimal *Engine whose poll-loop transition
// (progressStatusStep) reads/writes through the supplied seam. Used by the
// two-writer-race unit test (TMPL-10); production wiring goes through NewEngine.
func newEngineForTest(store progressStatusStore) *Engine {
	return &Engine{
		pollStore: store,
		logger:    slog.Default(),
	}
}

// NewEngine creates a new provisioning engine with the given dependencies.
func NewEngine(deps EngineDeps) *Engine {
	e := &Engine{
		registry:       deps.Registry,
		db:             deps.DB,
		config:         deps.Config,
		logger:         deps.Logger,
		tunnelMgr:      deps.TunnelMgr,
		directConnect:  deps.TunnelMgr == nil,
		sshReady:       deps.SSHReady,
		onStatusChange: deps.OnStatusChange,
		assetStore:     deps.AssetStore,
	}
	// Default the provisioning-timeout to the historical hardcoded value. Tests
	// override the unexported field directly to exercise the timeout path fast.
	if e.provisionTimeout == 0 {
		e.provisionTimeout = 10 * time.Minute
	}
	// Boot-stall watchdog knobs from config (bootstall.go). deps.Config is
	// always non-nil in production wiring; a nil config (bare test engines)
	// leaves the zero values, which disable the watchdog.
	if deps.Config != nil {
		e.tunnelStallAfter = deps.Config.ProvisionTunnelStallAfter
		e.pullStallAfter = deps.Config.ProvisionPullStallAfter
		e.bootRescueAttempts = deps.Config.ProvisionBootRescues
		e.defaultEnvironment = deps.Config.ProvisionDefaultEnvironment
		e.defaultEnvironmentAll = deps.Config.ProvisionDefaultEnvScope == "all"
		if mode := deps.Config.ProvisionGPUCheck; mode == "shadow" || mode == "enforce" {
			e.gpuCheckMode = mode
		}
	}
	e.launchBreaker = deps.LaunchBreaker
	e.fundingGate = deps.FundingGate
	return e
}

// SetOnStatusChange sets the callback invoked when progressStatus changes
// an instance's state. Allows post-construction wiring when Engine is created
// before the API server (which owns the SSE broker).
func (e *Engine) SetOnStatusChange(fn func(instanceID, status string)) {
	e.onStatusChange = fn
}

// InstanceEventSink receives instance lifecycle webhook events (GPU-45). The
// concrete implementation (public.InstanceEventPublisher) fans each event out
// to the owning org's registered webhook endpoints; this narrow seam exists
// because internal/api/public imports this package, so the engine cannot
// import the publisher directly.
//
// Emission contract — exactly-once per genuine transition per instance:
//
//   - InstanceCreating fires once, when the instance ROW is created
//     (Provision step 8). A boot-stall rescue (bootstall.go) re-points the
//     SAME row at replacement capacity and resets it to provisioning — it
//     does NOT create a record, so it deliberately does NOT re-emit
//     instance.creating (conservative choice: the customer's resource never
//     stopped existing, it just changed upstream under the hood).
//   - InstanceTerminated / InstanceFailed fire only when the caller's atomic
//     DB transition reported updated=true, so requeues, poll retries, and
//     concurrent writers cannot double-emit.
type InstanceEventSink interface {
	InstanceCreating(ctx context.Context, inst *db.Instance)
	InstanceTerminated(ctx context.Context, inst *db.Instance)
	InstanceFailed(ctx context.Context, inst *db.Instance, reason string)
}

// SetInstanceEventSink injects the instance webhook event sink (GPU-45).
// Post-construction wiring, same pattern as SetOnStatusChange. Nil disables
// webhook publishing.
func (e *Engine) SetInstanceEventSink(sink InstanceEventSink) {
	e.eventSink = sink
}

// emitInstanceCreating / emitInstanceTerminated / emitInstanceFailed are the
// nil-guarded emission helpers. Best-effort by design: the sink itself only
// enqueues delivery rows and logs failures — a webhook problem must never
// fail a lifecycle transition that already committed.
func (e *Engine) emitInstanceCreating(ctx context.Context, inst *db.Instance) {
	if e.eventSink != nil {
		e.eventSink.InstanceCreating(ctx, inst)
	}
}

func (e *Engine) emitInstanceTerminated(ctx context.Context, inst *db.Instance) {
	if e.eventSink != nil {
		e.eventSink.InstanceTerminated(ctx, inst)
	}
}

func (e *Engine) emitInstanceFailed(ctx context.Context, inst *db.Instance, reason string) {
	if e.eventSink != nil {
		e.eventSink.InstanceFailed(ctx, inst, reason)
	}
}

// SetFlags injects the runtime feature-flag store (Plan 48-02). Post-construction
// wiring (same pattern as SetOnStatusChange) because the flags Store and the
// engine are both built in main.go. nil leaves the engine on env-only
// provider-disable resolution. *admin.Store satisfies flagsChecker.
func (e *Engine) SetFlags(f flagsChecker) {
	e.flags = f
}

// SetHostQuarantine wires the host quarantine: a tunnel-stall verdict holds
// the machine out of the catalog and placement for a while. Post-construction,
// like SetFlags.
func (e *Engine) SetHostQuarantine(q *availability.HostQuarantine) { e.hostQuarantine = q }

// noteStallVerdict quarantines the machine behind a stalled boot when the
// stall follows the machine. Only a tunnel stall does: the container ran and
// the bootstrap never phoned home, which repeated on the same machine launch
// after launch. A dead pull does not (most such hosts boot when rented again),
// and a verdict with no host identity has nothing to hold.
//
// A customer image never quarantines either. Its stall can be the image's own
// doing: an adapter may ship the bootstrap as base64 | gunzip | bash, so an image
// without those tools (Alpine, distroless) never phones home on any machine,
// and holding the healthy machine under it would only take it off sale.
func (e *Engine) noteStallVerdict(providerName, hostID, verdict string, customerImage bool) {
	if verdict != verdictTunnelStall || customerImage {
		return
	}
	e.hostQuarantine.RecordTunnelStall(providerName, hostID)
}

// SetOfferingVerdicts wires the fleet prober's verdicts into placement. With
// enforce, an offering a probe found broken is skipped like a tripped breaker
// (probe launches and staff provider pins excepted). Post-construction, like
// SetFlags, because the verdict cache is built after the engine; call it
// before anything can launch (it is not synchronized).
func (e *Engine) SetOfferingVerdicts(v availability.OfferingVerdicts, enforce bool) {
	e.offeringVerdicts, e.enforceOfferingVerdicts = v, enforce
}

// SetPlacementMemory wires per-offering placement memory: failures are
// recorded from the walk and from vanished pins, successes clear them, and
// unpinned selection skips offerings the memory is hiding. Post-construction,
// like SetFlags.
func (e *Engine) SetPlacementMemory(m *availability.PlacementMemory) { e.placement = m }

// candidateOfferingID is the catalog's offering ID for a candidate, the key
// placement memory and the catalog share.
func candidateOfferingID(c ProviderCandidate) string {
	return availability.OfferingID(c.Offering, availability.Normalize(c.Offering.Region, c.Offering.DatacenterLocation, c.Prov.Name()))
}

// selectionTrace records what candidate selection actually consulted, so a
// pinned launch that matched nothing can be judged afterwards (pinVanished).
type selectionTrace struct {
	// listed holds the providers that passed every provider gate and whose
	// live listing succeeded.
	listed map[string]bool
	// breakerSkipped holds the LaunchKeys whose offerings the launch-outcome
	// breaker skipped.
	breakerSkipped map[string]bool
}

func newSelectionTrace() *selectionTrace {
	return &selectionTrace{listed: map[string]bool{}, breakerSkipped: map[string]bool{}}
}

// pinVanished reports whether a pinned launch that selection could not match
// (ErrOfferingUnavailable) failed because the offering itself is gone: the
// catalog listed it, the request asks for exactly what the catalog showed
// (GPU type, a count it covers, tier, region), its provider passed every gate
// and listed successfully, and the breaker did not skip it. Then the only
// ways left to miss are that the offering is absent from its provider's live
// listing or listed with no stock, which is what placement memory counts.
//
// Every other way a pin drops out says nothing about the offering, and
// counting it would let any caller hide any listing for free: a request with
// a mismatched gpu_count, tier or region, a provider gate the request fails
// (certified, customer image, disk_gb, raw_vm OS), a disabled or unfunded
// provider, or a listing error (expired key, rate limit, outage). An ID the
// catalog never listed is not counted either, so callers cannot fill the
// memory with made-up IDs.
func (e *Engine) pinVanished(req ProvisionRequest, trace *selectionTrace) bool {
	if e.placement == nil || trace == nil || req.autoSelectsGPU() {
		return false
	}
	l, ok := e.placement.Listed(req.SelectedOfferingID)
	if !ok || !trace.listed[l.Provider] {
		return false
	}
	if l.GPUType != req.GPUType || l.GPUCount < req.GPUCount || l.Tier != req.Tier {
		return false
	}
	if len(req.Regions) > 0 && !slices.Contains(req.Regions, l.Region) {
		return false
	}
	return !trace.breakerSkipped[availability.LaunchKey(l.Provider, string(l.GPUType), l.GPUCount)]
}

// SetManifest injects the pinned-base-environment manifest (Phase 63): per-GPU
// driver floors / CUDA minimums / digest-pinned certified image refs. Post-
// construction wiring (same pattern as SetFlags / SetProvisionCounter) because
// the manifest is loaded in serve.go (manifest.LoadEmbedded, fail-fast at boot)
// while the engine is built earlier in deps.go. Reachability seam only in Wave 0
// — no provisioning path reads it yet — so a nil manifest is production-safe.
func (e *Engine) SetManifest(m *manifest.Manifest) {
	e.manifest = m
}

// providerDisabled reports whether the named provider is disabled, resolving
// DB-over-env when the flags store is wired (a panel flip takes effect at
// runtime, D-08) and falling back to the env config otherwise. nil-safe.
func (e *Engine) providerDisabled(ctx context.Context, name string) bool {
	if e.flags != nil {
		return e.flags.IsProviderDisabled(ctx, name)
	}
	return e.config.IsProviderDisabled(name)
}

// markupFor and costDiscountFor resolve a provider's TWO pricing knobs the same
// way every other reader does: the provider_markup_overrides DB row (admin
// Suppliers panel) wins over the PRICING_MARKUP_PCT_<NAME> /
// PROVIDER_COST_DISCOUNT_PCT_<NAME> env values. They mirror providerDisabled —
// the Store folds the env fallback in itself, so the nil-flags path (unit tests,
// engines built without SetFlags) is the env-only resolution.
//
// The engine reads both through here and NOWHERE else. Reading e.config
// directly is the divergence bug: it makes the rate BILLED ignore a value the
// admin panel already applied to the catalog.
//
// Within one launch each resolves a provider AT MOST ONCE and reuses the value —
// see pricingSnapshot for why that is load-bearing rather than an optimization.
func (e *Engine) markupFor(ctx context.Context, name string) float64 {
	return e.pricingKnob(ctx, name, knobMarkup)
}

func (e *Engine) costDiscountFor(ctx context.Context, name string) float64 {
	return e.pricingKnob(ctx, name, knobCostDiscount)
}

// minMarginFor resolves the per-supplier retail floor (the loss floor, which
// may be negative — see availability.RetailPrice). Frozen per launch like the
// other two knobs: the floor is part of the billed price whenever it clamps,
// so an unfrozen floor is the same cap/bill divergence one knob over.
func (e *Engine) minMarginFor(ctx context.Context, name string) float64 {
	return e.pricingKnob(ctx, name, knobMinMargin)
}

// pricingKnob is the shared freeze-then-resolve path for both knobs.
func (e *Engine) pricingKnob(ctx context.Context, name string, k knob) float64 {
	snap := pricingSnapshotFrom(ctx)
	if snap == nil {
		return e.resolveKnob(ctx, name, k)
	}
	// The lock is held ACROSS the resolve, not just around the map access.
	// Nothing in a launch fans out today, so a check-then-resolve-then-store
	// would be correct in practice — but then "resolves at most once" would be
	// true only by accident, and a future parallel candidate walk would silently
	// hand one caller a value that is not the frozen one. The resolve is a cache
	// read behind the Store's own lock, so serializing it costs nothing.
	return snap.resolveOnce(name, k, func() float64 { return e.resolveKnob(ctx, name, k) })
}

func (e *Engine) resolveKnob(ctx context.Context, name string, k knob) float64 {
	switch k {
	case knobMarkup:
		if e.flags != nil {
			return e.flags.MarkupPctFor(ctx, name)
		}
		return e.config.MarkupPctFor(name)
	case knobMinMargin:
		if e.flags != nil {
			return e.flags.MinMarginPctFor(ctx, name)
		}
		return e.config.MinRetailMarkupPct()
	default:
		if e.flags != nil {
			return e.flags.CostDiscountPctFor(ctx, name)
		}
		return e.config.CostDiscountPctFor(name)
	}
}

// listBasisCeiling converts a retail rate into the ceiling an ADAPTER can
// compare its own quotes against.
//
// Adapters see a provider's LIST prices (a marketplace adapter's offer walk
// compares req.MaxCostPerHour against the host's list hourly cost). Retail, since v85, is derived from TRUE
// cost, so on a discounted supplier the two are in different currencies: a
// straight retail ceiling is ~(1−discount) too low and rejects hosts we could
// serve profitably.
//
// What the guard actually means is "skip a host whose TRUE cost to us exceeds
// what we charge":
//
//	trueCost(host) > retail   ⟺   host × (1 − d) > retail   ⟺   host > retail / (1 − d)
//
// so the list-basis ceiling is retail grossed back up by the discount. At d = 0
// this is exactly retail, which is every marketplace supplier today — so this
// is a no-op on current config and a correctness fix the moment a partner
// discount is set on one. That is now a single click in the Suppliers panel,
// and getting it wrong fails EVERY launch on that supplier as "no capacity".
func (e *Engine) listBasisCeiling(ctx context.Context, retail float64, providerName string) float64 {
	d := e.costDiscountFor(ctx, providerName)
	if d <= 0 || d >= 100 {
		return retail
	}
	return retail / (1.0 - d/100.0)
}

// knob names which of a provider's three pricing numbers is being resolved.
type knob int

const (
	knobMarkup knob = iota
	knobCostDiscount
	knobMinMargin
)

// pricingSnapshotKey types the context key for the per-launch pricing freeze.
type pricingSnapshotKey struct{}

// pricingSnapshot memoizes each provider's resolved markup AND partner discount
// for the lifetime of ONE launch.
//
// This exists to keep a price CEILING and the price BILLED provably equal.
// Provision checks req.MaxPricePerHour against retailWithDisk early (before
// upstream placement) and computes the persisted BillingSession.PricePerHour
// from retailWithDisk again ~40s later, after the provider call returns. There
// is no cap re-check in between, and by then the instance exists upstream — so a
// late disagreement cannot be resolved by failing the request.
//
// Before these moved onto admin.Store both calls read the same immutable env
// map, so they could not disagree. The Store makes them two reads of a 30s-TTL
// cache that the availability poller refreshes underneath the request, and that
// fails OPEN on a transient DB error. Either an operator's panel edit mid-launch
// or a single failed read between the two calls would bill the customer above
// the cap they approved — the exact invariant retailWithDisk's own comment
// promises.
//
// BOTH knobs must be frozen, not just the markup. Since v85 retail is DERIVED
// from the discount, and the discount's fail-open is the more dangerous of the
// two: markup falls back to a global env default, but a discount set only in the
// panel has no env value to fall back to, so a failed read resolves it to 0 and
// re-prices the launch at full LIST. A launch quoted at 0.9 x list would bill at
// list — an 11% overcharge above an approved cap.
//
// Freezing at first resolve restores the old guarantee without a late abort:
// every retailWithDisk in a launch (fast-fail cap, per-fallback guard, candidate
// sort, persisted rate, boot-stall re-price) and the COGS record beside it see
// one number per provider per knob. An edit lands on the NEXT launch, which is
// the same semantics the catalog has always had.
type pricingSnapshot struct {
	mu  sync.Mutex
	pct map[string]float64 // key: "<knob>:<folded provider>"
}

func snapKey(name string, k knob) string {
	switch k {
	case knobMarkup:
		return "m:" + strings.ToLower(name)
	case knobMinMargin:
		return "f:" + strings.ToLower(name)
	default:
		return "d:" + strings.ToLower(name)
	}
}

// resolveOnce returns the frozen value for (name, knob), calling resolve exactly
// once per key for the lifetime of this snapshot.
func (p *pricingSnapshot) resolveOnce(name string, k knob, resolve func() float64) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := snapKey(name, k)
	if pct, ok := p.pct[key]; ok {
		return pct
	}
	pct := resolve()
	p.pct[key] = pct
	return pct
}

// withPricingSnapshot returns a ctx carrying a fresh per-launch pricing freeze.
// Called once at each launch entry point (Provision, and the boot-stall rescue).
// A ctx without one resolves live on every call, which is the correct behavior
// for non-launch readers.
func withPricingSnapshot(ctx context.Context) context.Context {
	return context.WithValue(ctx, pricingSnapshotKey{}, &pricingSnapshot{pct: make(map[string]float64, 8)})
}

func pricingSnapshotFrom(ctx context.Context) *pricingSnapshot {
	snap, _ := ctx.Value(pricingSnapshotKey{}).(*pricingSnapshot)
	return snap
}

// ErrNoProvider is returned when no suitable provider is found for the request.
var ErrNoProvider = errors.New("no suitable provider available")

// ErrOfferingUnavailable is returned when a pinned launch (req.SelectedOfferingID)
// names an exact offering that is no longer available at launch time (D-63-08). It
// is intentionally distinct from ErrNoProvider so the caller can surface an
// honest "pick another offering" message and NEVER silently substitute a cheaper
// live offering. The message carries no provider name, upstream body, or image
// digest (T-63-04 defense-by-omission).
var ErrOfferingUnavailable = errors.New("selected machine is no longer available — pick another offering")

// ErrCertifiedUnavailable is returned when a certified-environment launch lands
// on a GPU that has no server-controlled manifest entry (no pinned image ref for
// it). We NEVER launch a "certified" instance without a digest-pinned image (V5),
// so this is an explicit, honest error rather than a silent unpinned launch. The
// message names no provider, upstream body, or image digest (T-63-04).
var ErrCertifiedUnavailable = errors.New("selected GPU has no certified environment available — pick another offering")

// ErrFrameworkUnavailable is returned when a certified:<framework> launch names
// a framework the server-controlled manifest cannot resolve for the placed GPU
// — unknown id, no family for the GPU's CUDA generation, or a still-unpublished
// (placeholder) image (D-64-01). Like ErrCertifiedUnavailable we refuse rather
// than silently fall back to the bare certified image: the user asked for a
// specific environment and must get exactly it or an honest error. The message
// names no provider, upstream body, or image digest (T-63-04).
var ErrFrameworkUnavailable = errors.New("selected framework environment is not available for this GPU — pick another framework or the certified base image")

// ErrOSUnavailable is returned when a raw_vm:<os> launch names an OS the
// server-controlled vm_images catalog cannot resolve for the placed capacity —
// unknown os id, or no candidate provider carries a verified native image slug
// for it. Like ErrFrameworkUnavailable we refuse rather than silently boot the
// provider's default OS: the user asked for a specific OS and must get exactly
// it or an honest error. The message names no provider or native image slug
// (T-63-04 defense by omission).
var ErrOSUnavailable = errors.New("selected operating system is not available for this machine — pick another OS or the provider default")

// ErrDiskUnavailable is returned when a launch explicitly requested disk_gb but
// no candidate provider for the requested capacity can configure the instance
// disk. Like ErrOSUnavailable we refuse rather than silently provisioning the
// provider's fixed size: the customer asked for a specific amount of room, is
// quoted for it, and must get exactly it or an honest error — a silent 40 GB
// where 200 was requested is precisely the failure this feature exists to end.
//
// The message names no provider (T-63-04 defense by omission) and points at the
// action that actually resolves it, since disk configurability is a property of
// the capacity rather than of the account.
var ErrDiskUnavailable = errors.New("the selected machine cannot be configured with a custom disk size — pick another machine or launch with the default disk")

// ErrImageEntrypointUnavailable is returned when a customer-image launch
// matched no capacity because every otherwise-eligible candidate was dropped
// by the ENTRYPOINT steer (BootstrapRidesImageEntrypoint): the image's config
// defines its own ENTRYPOINT and the remaining capacity cannot inject our
// bootstrap around one. Distinct from ErrOfferingUnavailable for the same
// reason as ErrDiskExceedsOffering — a pinned offering dropped by this gate
// is right there and will fail identically on retry, so the message names
// the property the customer can actually change. Clearing `entrypoint` in
// the REQUEST does not help (the container runtime applies the image
// config's entrypoint regardless), so the message doesn't suggest it. Names
// no provider (T-63-04 defense by omission).
var ErrImageEntrypointUnavailable = errors.New("this capacity cannot run images that define their own ENTRYPOINT — launch without pinning, pick different capacity, or use an image whose ENTRYPOINT is not set")

// ErrCustomImageSpotUnsupported is returned when a CUSTOMER-image launch asks
// for the spot tier (GPU-92). A spot instance can be reclaimed by the upstream
// at any time, and the preemption re-route (RerouteSpot) rebuilds the
// replacement from the persisted instance row — which carries the image ref
// but NOT the customer env, entrypoint/cmd, ports, or the private-registry
// login. Until the full launch shape is persisted and re-threaded, a reroute
// of a custom-image instance would silently boot a stock box in its place, so
// the combination is refused at launch instead. The handler 422s the request
// shape up front; this is the engine's defense in depth for other callers.
var ErrCustomImageSpotUnsupported = errors.New("custom images cannot be launched on the spot tier yet — a preemption re-route cannot carry the image; use tier on_demand")

// ErrSpotRerouteCustomEnvironment is returned by RerouteSpot for a reclaimed
// instance whose environment cannot be reconstructed from its row — a
// custom-image launch (instances.image set) or a template deploy
// (instances.template_id set). Rerouting such an instance from GPU type/count
// alone would swap the customer's environment for the default image without
// telling them (GPU-92); failing loudly leaves the instance in the failed
// state the watcher already recorded, with billing closed.
var ErrSpotRerouteCustomEnvironment = errors.New("spot re-route refused: the reclaimed instance's custom environment cannot be reconstructed for a replacement")

// ErrDiskExceedsOffering is returned when an explicitly requested disk_gb is
// larger than the storage the offering's host can actually deliver
// (Offering.StorageGB, when the adapter reports one). It is deliberately
// distinct from both ErrDiskUnavailable (this capacity CAN configure disk —
// just not that much of it) and ErrOfferingUnavailable (the machine is right
// there; retrying the same pin can only fail the same way). Born from the
// 2026-08-11 prod battery, where a pinned marketplace launch with --disk 150 was
// accepted, billed at 150, and silently clamped to the host's 134GB — the
// exact silent-substitution the --disk contract promises to refuse. The
// message names no provider (T-63-04 defense by omission).
var ErrDiskExceedsOffering = errors.New("requested disk_gb exceeds the storage this machine can deliver — lower disk_gb or pick another machine")

// ParseEnvironmentSelection splits a wire `environment` value into its class
// and optional sub-choice: the certified framework ("certified:pytorch" →
// ("certified", "pytorch"), D-64-01) or the raw-VM OS id ("raw_vm:ubuntu-24.04"
// → ("raw_vm", "ubuntu-24.04"), the launch OS picker). Values without a colon
// pass through as (value, ""). The sub token's MEANING depends on the class —
// callers must switch on class before treating it as a framework or an os id.
// Shared by the engine and both create handlers so the wire grammar has exactly
// one definition.
func ParseEnvironmentSelection(sel string) (class, sub string) {
	if c, s, ok := strings.Cut(sel, ":"); ok {
		return c, s
	}
	return sel, ""
}

// ParseFrameworkVersion splits a certified sub-choice into the framework id and
// its optional version (the launch version picker's `certified:<fw>@<ver>`
// grammar): "pytorch@2.13" → ("pytorch", "2.13"); a bare "pytorch" → ("pytorch",
// "") meaning "the default build for this GPU's variant". Only meaningful on the
// certified class — callers must parse the class off first (ParseEnvironmentSelection).
func ParseFrameworkVersion(sub string) (framework, version string) {
	if f, v, ok := strings.Cut(sub, "@"); ok {
		return f, v
	}
	return sub, ""
}

// envResolution carries the SERVER-DERIVED Docker-normalization environment
// attributes for a placed launch (Phase 63, D-63-04/D-63-08). It is computed by
// resolveEnvironment from the placed provider's capability + the server-controlled
// manifest — the client's req.Environment is only a validated hint; the engine
// re-derives every field itself and NEVER trusts customer input for image refs.
type envResolution struct {
	certifiedImage bool   // BootstrapData.CertifiedImage (nested certified-VM docker block)
	deploymentType string // "vm" | "container" | "" (unknown/legacy)
	pinnedImageRef string // digest-pinned certified image for the certified-VM bootstrap
	driverFloor    string // minimum NVIDIA driver, enforced at boot (sort -V) + placement
	// dockerImageOverride is set ONLY for the container-native certified path: the
	// provider runs our image natively, so the adapter's DockerImage is pointed at
	// the pinned manifest ref and the bootstrap grows NO nested docker block.
	dockerImageOverride string
	// osID is set ONLY for a raw_vm:<os> launch: the manifest-validated vm_images
	// key. The provider-NATIVE image slug is resolved PER ATTEMPT in the provision
	// retry loop (candidates can span providers whose slugs for the same os id
	// differ), never stored here.
	osID string
}

// resolveEnvironment maps the server-validated environment class (req.Environment)
// + the PLACED provider's capability + the server-controlled manifest into the
// concrete env fields (D-63-04 defaults). req.Environment == "" keeps the legacy
// path byte-for-byte (empty resolution). A certified request for a GPU with no
// manifest entry is an explicit error — we never launch certified unpinned.
func (e *Engine) resolveEnvironment(req ProvisionRequest, providerName, gpuType string) (envResolution, error) {
	var res envResolution
	if req.Environment == "" {
		return res, nil // legacy path — no env fields, unchanged behavior
	}

	// A sub-choice rides two classes: "certified:<framework>" (D-64-01) and
	// "raw_vm:<os>" (the launch OS picker). Parse it off before classifying; a
	// sub token on any other class should have been rejected by the handlers'
	// format gates, so refuse conservatively here (never launch it unclassified).
	class, sub := ParseEnvironmentSelection(req.Environment)
	if sub != "" && class != "certified" && class != "raw_vm" {
		return res, ErrFrameworkUnavailable
	}
	framework, frameworkVersion := "", ""
	if class == "certified" {
		framework, frameworkVersion = ParseFrameworkVersion(sub)
	}

	// Bridge the GPUType enum (snake_case, e.g. "h100_sxm") to the manifest's
	// display keys ("H100 SXM"). Lookup is case/whitespace-insensitive but does
	// NOT map underscores, so translate them here (mirrors handlers_catalog.go).
	gpuKey := strings.ReplaceAll(gpuType, "_", " ")

	switch class {
	case "raw_vm":
		// Explicit root VM, no Certified Image. We still carry the driver floor so
		// the bootstrap enforces it at boot; a manifest miss just leaves it empty.
		res.deploymentType = "vm"
		if e.manifest != nil {
			if ge, ok := e.manifest.Lookup(gpuKey); ok {
				res.driverFloor = ge.DriverFloor
			}
		}
		// raw_vm:<os> — an explicit OS pick must resolve against the server-
		// controlled vm_images catalog for the PLACED provider or refuse (never
		// boot the default OS the user didn't ask for). candidates were already
		// narrowed to os-capable providers in SelectProviderCandidates; this
		// re-check is the per-placement trust boundary. Only the os id is kept —
		// the retry loop resolves the provider-native slug per attempt.
		if sub != "" {
			if e.manifest == nil || !e.manifest.HasVMImage(sub) {
				return res, ErrOSUnavailable
			}
			if _, ok := e.manifest.VMImageSlug(providerName, sub); !ok {
				return res, ErrOSUnavailable
			}
			res.osID = sub
		}
		return res, nil

	case "provider_template":
		// Fixed provider template — provider boots its own image; we inject
		// nothing and add no bootstrap docker block.
		return res, nil

	case "certified":
		if e.manifest == nil {
			return res, ErrCertifiedUnavailable
		}
		ge, ok := e.manifest.Lookup(gpuKey)
		if !ok {
			return res, ErrCertifiedUnavailable
		}
		// D-64-01: a framework sub-choice swaps the pinned image for the
		// framework family's ref matching this GPU's CUDA generation — same
		// server-controlled manifest, same digest-pin guarantee. Unknown or
		// still-unpublished (placeholder) frameworks refuse explicitly; every
		// other certified attribute (floors, bootstrap shape) is identical.
		imageRef := ge.ImageRef
		if framework != "" {
			// A `certified:<fw>@<ver>` version pick (the launch version picker)
			// resolves against the per-variant version catalog — refused if the
			// version can't run on this GPU's CUDA variant. A bare `certified:<fw>`
			// keeps the default per-variant family ref, byte-for-byte as before.
			var fwRef string
			var ok bool
			if frameworkVersion != "" {
				fwRef, ok = e.manifest.FrameworkImageRefForVersion(framework, ge.CUDAMin, frameworkVersion)
			} else {
				fwRef, ok = e.manifest.FrameworkImageRef(framework, ge.CUDAMin)
			}
			if !ok {
				return res, ErrFrameworkUnavailable
			}
			imageRef = fwRef
		}
		if SupportsCustomImage(providerName) {
			// Container-native: the provider runs our image natively. Point the
			// adapter image at the pinned ref; the bootstrap must NEVER nest Docker,
			// so CertifiedImage stays false and no PinnedImageRef flows to bootstrap.
			res.deploymentType = "container"
			res.dockerImageOverride = imageRef
			return res, nil
		}
		// Certified on a capable VM provider: we bootstrap the machine and run the
		// pinned image ourselves, gated by the driver floor.
		res.certifiedImage = true
		res.deploymentType = "vm"
		res.pinnedImageRef = imageRef
		res.driverFloor = ge.DriverFloor
		return res, nil

	default:
		// Unknown class should have been rejected by the handler's Validate(); be
		// conservative and refuse rather than launch an unclassified instance.
		return res, ErrCertifiedUnavailable
	}
}

// defaultEnvironmentFor returns the environment to apply to a launch that named
// none, or "" to keep the legacy path. Only plain launches qualify: an explicit
// environment, a template or customer image, and the reserved tier all keep
// what they asked for. The default must also resolve for this GPU in the
// manifest (base entry plus, for certified:<framework>, a published framework
// image for its CUDA variant), so a GPU the manifest does not cover keeps the
// legacy behavior rather than failing in resolveEnvironment. Where it applies
// is decided per placement (placementEnvironment): container-native only.
func (e *Engine) defaultEnvironmentFor(req ProvisionRequest) string {
	if e.defaultEnvironment == "" || req.Environment != "" || req.Image != "" || req.CustomerImage ||
		req.TemplateID != nil || req.Tier == provider.TierReserved || e.manifest == nil {
		return ""
	}
	class, sub := ParseEnvironmentSelection(e.defaultEnvironment)
	if class != "certified" {
		return ""
	}
	ge, ok := e.manifest.Lookup(strings.ReplaceAll(string(req.GPUType), "_", " "))
	if !ok {
		return ""
	}
	// The legacy adapter images are CUDA 12.4 builds, cached on most hosts
	// and fast to boot. Leave them alone where they work; replace them where
	// the GPU needs a newer CUDA than they carry (Blackwell: sm_100/sm_120).
	if !e.defaultEnvironmentAll && !cudaVersionAbove(ge.CUDAMin, legacyImageCUDA) {
		return ""
	}
	if sub != "" {
		framework, version := ParseFrameworkVersion(sub)
		var published bool
		if version != "" {
			_, published = e.manifest.FrameworkImageRefForVersion(framework, ge.CUDAMin, version)
		} else {
			_, published = e.manifest.FrameworkImageRef(framework, ge.CUDAMin)
		}
		if !published {
			return ""
		}
	}
	return e.defaultEnvironment
}

// legacyImageCUDA is the CUDA toolkit the adapters' hardcoded default images
// are built for (pytorch/pytorch:2.4.1-cuda12.4 and the CUDA 12.4 VM
// images).
const legacyImageCUDA = "12.4"

// cudaVersionAbove compares "major.minor" CUDA versions; unparseable input is
// treated as not above, which keeps the legacy behavior.
func cudaVersionAbove(v, than string) bool {
	parse := func(s string) (int, int, bool) {
		maj, min, ok := strings.Cut(s, ".")
		a, err1 := strconv.Atoi(maj)
		b, err2 := strconv.Atoi(min)
		return a, b, ok && err1 == nil && err2 == nil
	}
	a1, b1, ok1 := parse(v)
	a2, b2, ok2 := parse(than)
	if !ok1 || !ok2 {
		return false
	}
	return a1 > a2 || (a1 == a2 && b1 > b2)
}

// placementEnvironment is the environment one candidate launches with. An
// explicit choice applies on every placement. A default (the launch named
// none) applies only where the provider runs our image natively
// (SupportsCustomImage, the same test resolveEnvironment uses for its
// container branch). On a VM provider the default would take the certified-VM
// branch, which moves the customer's SSH from root on the VM into a container
// on :2222: a different machine from the one they get today by asking for
// nothing, so a VM placement keeps the legacy path. Deciding this per
// placement, not once per launch, keeps the candidate set and its order exactly
// the legacy ones (so the default can never turn a placeable launch into a
// refusal) and means a fallback across provider classes never carries a
// default onto a VM as if the customer had asked for it.
func placementEnvironment(explicit, defaultEnv, providerName string) string {
	if explicit != "" {
		return explicit
	}
	if defaultEnv != "" && SupportsCustomImage(providerName) {
		return defaultEnv
	}
	return ""
}

// expectedFramework is the framework the startup GPU check must find working:
// the one a certified:<framework> environment promises, else none.
func expectedFramework(environment string) string {
	if class, sub := ParseEnvironmentSelection(environment); class == "certified" {
		if fw, _ := ParseFrameworkVersion(sub); fw == "pytorch" {
			return "pytorch"
		}
	}
	return ""
}

// readinessFailureTaker is the optional seam through which the poll loop picks
// up a startup GPU compute check failure the ready handler recorded on the row
// (*db.Pool implements it; test fakes opt in).
type readinessFailureTaker interface {
	TakeInstanceReadinessFailure(ctx context.Context, instanceID string) (string, error)
}

var _ readinessFailureTaker = (*db.Pool)(nil)

// ErrPriceExceeded is returned when the current price exceeds the requested max.
var ErrPriceExceeded = errors.New("current price exceeds maximum price per hour")

// PriceExceededError carries the current cheapest RETAIL $/hr alongside the
// ErrPriceExceeded sentinel, so the API can hand the client the NEW price to
// re-quote against ("price changed to $X — confirm") instead of an opaque
// rejection. errors.Is(err, ErrPriceExceeded) still matches (see Is), so every
// existing sentinel check is unaffected.
type PriceExceededError struct {
	// CurrentBestPerHour is the cheapest available RETAIL $/hr for this request
	// right now — the price the client should display for re-confirmation.
	CurrentBestPerHour float64
	// MaxPricePerHour is the cap the caller sent that the current price exceeds.
	MaxPricePerHour float64
}

func (e *PriceExceededError) Error() string {
	return fmt.Sprintf("current price %.4f/hr exceeds maximum %.4f/hr",
		e.CurrentBestPerHour, e.MaxPricePerHour)
}

// Is reports true for ErrPriceExceeded so callers can errors.Is on the sentinel
// while errors.As on *PriceExceededError recovers the priced detail.
func (e *PriceExceededError) Is(target error) bool { return target == ErrPriceExceeded }

// viewedPriceCentSlack is how far a launch-time retail may sit above the card
// price the customer saw before we refuse the launch (GPU-78).
//
// The divergence it absorbs is structural, not sloppiness: the launch modal
// folds a raw sub-cent disk uplift into the price it renders while the backend
// ceils the whole retail to the cent, so the two can legitimately differ by up
// to a penny on an offering that never moved. Refusing on that would 409 a
// large share of ordinary launches — a worse outcome than the bug.
const viewedPriceCentSlack = 0.01

// viewedPriceExceeded reports whether a launch-time RETAIL rate has moved above
// the price the customer saw by more than the rounding slack.
//
// nil or non-positive means "absent" and always returns false — that is the
// byte-for-byte path for every caller that never sends the field, and it is
// what keeps a garbage value from becoming a new 4xx surface.
func viewedPriceExceeded(retail float64, viewed *float64) bool {
	if viewed == nil || *viewed <= 0 {
		return false
	}
	return retail-*viewed > viewedPriceCentSlack+1e-9
}

// ErrSSHKeysNotFound is returned when none of the requested SSH keys exist.
var ErrSSHKeysNotFound = errors.New("ssh keys not found")

// ErrSSHKeyOrgMismatch is returned when at least one SSH key referenced by
// ProvisionRequest.SSHKeyIDs belongs to an org other than req.OrgID. This is
// the defense-in-depth gate from 30-DESIGN §5.3.1 — API-key auth already
// scopes the request to one org, but a user-in-multiple-orgs could
// still hand the engine an ssh_key_id from the wrong org context. The
// assertion blocks that pathway BEFORE we embed any public key into a
// cloud-init / FRP-routed tunnel. Applies to ALL tiers (on_demand, spot,
// reserved) — wrong-org keys are a security gap everywhere; the
// requirement is registered against the reserved feature only because
// that is where the cross-org concern is most concrete (RES-18).
var ErrSSHKeyOrgMismatch = errors.New("ssh key does not belong to the requesting organization")

// ErrSSHKeyForbidden is returned when a non-admin (org member) passes an
// explicit ssh_key_id for a key they do not own (v33). Members may only attach
// their own keys; admins may attach any key in the org. Maps to HTTP 403.
var ErrSSHKeyForbidden = errors.New("ssh key does not belong to the requesting user")

// ErrReservedNotConfigured is returned when a request arrives with
// Tier == TierReserved. Reserved capacity contracts are not part of GPU.ai
// Open Source, so the request is refused before any upstream call.
var ErrReservedNotConfigured = errors.New("reserved tier is not available on this deployment")

// ErrProviderDisabled is returned when a provision request targets a provider
// that an operator has switched off sitewide (PROVIDERS_DISABLED). Only the
// reserved path surfaces it directly (the deferred guard releases the allocated
// slot first); the on-demand / spot path never reaches it — disabled providers
// are filtered out of candidate selection and an all-disabled match falls
// through to the indistinguishable ErrNoProvider, so the kill-switch stays
// invisible to customers.
var ErrProviderDisabled = errors.New("upstream provider is administratively disabled")

// Provision creates a new GPU instance.
//
// Steps:
//  1. Generate instance ID and internal token
//  2. Resolve SSH keys from DB
//  3. Refuse the reserved tier (not part of GPU.ai Open Source)
//  4. SelectProviderCandidates, which live-lists every provider
//     (ListAvailable per adapter) — NOT the availability cache. Launch
//     placement therefore never sees cached or retained catalog rows;
//     a provider that is down simply yields no candidates.
//  5. Build callback URL (uses GPUCTL_PUBLIC_URL if configured)
//  6. Optionally allocate FRP port and render bootstrap script
//  7. Call provider to create upstream instance (retry loop)
//  8. Persist instance record to DB
//  9. Kick off async status progression (creating -> provisioning -> booting)
//
// The named return (retErr) lets the deferred release-on-failure guard
// see whether anything between AllocateSlot and the final return failed.
// effectiveDiskGB resolves the instance disk size, in GB, that this launch is
// actually provisioned with and quoted for. It is the single place that answers
// "how much disk?" — narrowing, pricing, the adapter payload and the persisted
// instance row all read this one value, so they cannot disagree.
//
// Precedence:
//   - the customer's explicit disk_gb, else the server default
//     (LAUNCH_DEFAULT_DISK_GB);
//   - raised to a template's ContainerDiskGB when that is larger.
//
// The template acts as a FLOOR, never a ceiling. That direction is deliberate:
// a template declares the disk its image needs to boot, so landing under it
// ENOSPCs, while a customer asking for more room is always safe to honor. It
// also means no existing template deploy can shrink — every catalog template
// sits at or below the 100 GB default today, so none of their prices move.
func (e *Engine) effectiveDiskGB(req ProvisionRequest) int {
	disk := e.config.DefaultDiskGB()
	if req.RequestedDiskGB != nil {
		disk = *req.RequestedDiskGB
	}
	if req.ContainerDiskGB > disk {
		disk = req.ContainerDiskGB
	}
	return disk
}

// retailWithDisk is the ONE retail-price formula for a launch: the offering's
// wholesale GPU rate plus the wholesale cost of any disk beyond the included
// allowance, marked up and floored jointly by availability.RetailPrice.
//
// Every site that compares, quotes, or bills a launch price MUST go through
// here. There are FIVE — the fast-fail price cap, the per-fallback cap guard,
// the rate written to the billing session, the candidate sort, and the
// boot-stall rescue re-price (bootstall.go) — and the
// billing invariant (the rate shown at rent time equals the rate billed) only
// holds if they compute the same number. Inlining the formula at any one of
// them is how a cap and a bill drift apart.
//
// No billing schema change and no line item: the disk cost is folded into the
// quoted hourly rate, so the disk-inclusive number is both what the customer
// approves and what BillingSession.PricePerHour stores.
//
// With DISK_PRICE_PER_GB_HOUR at 0 the disk term is exactly 0 and this reduces
// to the previous formula, bit for bit.
//
// ctx is here for the markup lookup, which resolves DB-over-env through
// markupFor — the same precedence the availability poller applies to the catalog.
// It takes a ctx rather than a plain markup argument so no call site can supply
// its own number: the catalog and the bill must read the one resolver.
func (e *Engine) retailWithDisk(ctx context.Context, wholesalePerHour float64, providerName string, diskGB int) float64 {
	diskWholesale := availability.DiskWholesalePerHour(diskGB, e.config.IncludedDiskGB(), e.config.DiskPricePerGBHourFor(providerName))
	// The basis is what this launch actually costs us: the provider's rate after
	// their partner discount, plus the disk. The disk term is added AFTER
	// discounting because it is our own configured rate, invoiced at face value
	// — a GPU-only partner discount must not discount our disk.
	costBasis := availability.TrueCost(wholesalePerHour, e.costDiscountFor(ctx, providerName)) + diskWholesale
	return availability.RetailPrice(costBasis, e.markupFor(ctx, providerName), e.minMarginFor(ctx, providerName))
}

// billedRetail is the rate the instance is actually sold at: normally the
// quoted row's retail, raised to what we BOUGHT when the customer explicitly
// approved a higher price.
//
// The cost ceiling lets an accept land above the quoted wholesale in exactly
// two ways, and they must bill differently:
//
//   - inside the drift allowance, with no explicit cap: we EAT it. That is what
//     the allowance is for, and re-pricing here would bill above the number the
//     customer clicked — the billing invariant this file's price sites exist to
//     protect.
//   - after a price-changed re-quote, where MaxPricePerHour carries the price
//     the customer just confirmed. Then the confirmed price is what they agreed
//     to pay, and billing the stale row instead is not a courtesy — it
//     reproduces the exact margin loss the ceiling exists to stop, now with a
//     consent dialog in front of it. Without this the confirmed cap raised our
//     authority to BUY and never reached the invoice, so every re-confirmed
//     launch lost precisely what it lost before the ceiling existed.
//
// Clamped at the cap, so a launch can never bill above what was approved
// however far upstream moved between the confirm and the accept.
//
// And in the other direction (GPU-78): the customer is never billed above the
// retail derived from what we ACTUALLY paid. A cheaper-than-quoted accept
// lowers the bill. Under a 0%-passthrough policy the old behavior — keep the
// quoted rate and let the margin show up in the panel — was not a courtesy but
// a hidden markup: prod 2026-08-23 billed $0.41/hr on an ask that cost $0.337,
// a ~22% spread on a page that promises none. The quote is our worst case for
// the customer, not our floor. Note the asymmetry with the drift allowance
// above, and it is deliberate: drift UP inside the allowance we eat, drift DOWN
// we pass on. Both rules move the bill the same way, toward the customer.
//
// A non-positive actual cost is "unreported", not "free": some adapters
// echo no per-instance charge
// and the reserved tier quotes 0 by construction. Those keep the quoted retail.
func (e *Engine) billedRetail(ctx context.Context, quotedWholesale, actualCost float64, providerName string, diskGB int, approvedCap *float64) float64 {
	retail := e.retailWithDisk(ctx, quotedWholesale, providerName, diskGB)
	// The buy came in under the quote: re-derive from what we paid. Guarded on
	// "only if lower" so this can never RAISE a bill however the retail formula
	// is configured — the whole change is one-directional by construction.
	if actualCost > 0 && actualCost < quotedWholesale {
		if bought := e.retailWithDisk(ctx, actualCost, providerName, diskGB); bought < retail {
			retail = bought
		}
	}
	if approvedCap == nil || actualCost <= quotedWholesale {
		return retail
	}
	bought := e.retailWithDisk(ctx, actualCost, providerName, diskGB)
	if bought > *approvedCap {
		bought = *approvedCap
	}
	if bought > retail {
		return bought
	}
	return retail
}

// costCeilingFor returns the WHOLESALE $/hr ceiling to hand the adapter for a
// candidate (provider.ProvisionRequest.MaxCostPerHour) — the most this launch
// may cost us before the customer has to be re-quoted.
//
// The catalog row is a QUOTE, not a booking. Retail is fixed from the row at
// engine level, but a per-machine marketplace adapter re-searches at accept
// time, so what we actually buy can cost more than what we sold. Without a
// ceiling that difference is silent margin loss with nothing in the path to
// object to it — live on prod 2026-08-13, a 3090 quoted at $0.16 bought a
// $0.3348 ask (+109%).
//
// Two inputs raise it above bare wholesale:
//
//   - the drift allowance — min(pct, abs) so a big node cannot quietly eat a
//     big number (see config.PricingCostDriftTolerancePct);
//   - an explicit customer-approved retail cap, which is what a re-confirm
//     after a price-changed 409 carries. Without this term the re-confirm
//     would 409 again at the same ceiling and the customer could never
//     actually accept the new price.
//
// Non-positive wholesale (reserved tier, flat-fee off-platform) returns 0 —
// the adapter treats that as "no ceiling", preserving today's behavior.
func (e *Engine) costCeilingFor(ctx context.Context, wholesalePerHour float64, providerName string, diskGB int, approvedRetail *float64, rescue bool) float64 {
	if wholesalePerHour <= 0 {
		return 0
	}
	pct, abs := e.config.CostDriftTolerancePct(), e.config.CostDriftToleranceAbs()
	if rescue {
		// A rescue is a different trade — it excludes the stalled host, so the
		// replacement is pricier by construction rather than by drift, and the
		// customer keeps paying the original rate either way. See
		// config.PricingRescueCostAllowancePct.
		pct, abs = e.config.RescueCostAllowancePct(), e.config.RescueCostAllowanceAbs()
	}
	allowance := wholesalePerHour * pct / 100.0
	if allowance > abs {
		allowance = abs
	}
	ceiling := wholesalePerHour + allowance
	if approvedRetail != nil {
		if funded := e.wholesaleFundedBy(ctx, *approvedRetail, providerName, diskGB); funded > ceiling {
			ceiling = funded
		}
	}
	return ceiling
}

// wholesaleFundedBy inverts retailWithDisk: the largest wholesale GPU rate a
// given approved RETAIL $/hr can pay for, once the disk term is removed.
//
// It divides by the LARGER of the provider markup and the provider's retail
// floor, because retailWithDisk takes the max of those two branches.
//
// The half-cent numerator slack is load-bearing, not fudge. retailWithDisk's
// markup branch rounds to NEAREST cent, so a cost can round DOWN into the
// retail rate we quoted; inverting by exact division would then hand back less
// than that cost and refuse the very price the customer was just asked to
// confirm — an infinite 409 loop, with the customer unable to accept at any
// price. (Prod is insulated today only because markup=0/floor=0 makes the
// ceil-rounding floor branch dominate; a per-provider markup would ship the
// loop, and a per-supplier markup was under consideration when this shipped.) The
// slack is bounded by half a cent of wholesale, and cannot become an unbilled
// loss: the accept path clamps billed retail at the approved cap.
//
// Basis: the division below lands on the partner-discounted TRUE-cost basis
// (that is the basis retail is derived from since v85), while the adapter
// compares the ceiling against the provider's LIST rate — so the result is
// grossed back up through listBasisCeiling before returning. At discount 0
// (every marketplace supplier today) the gross-up is exactly identity; with a
// discount set, skipping it would refuse hosts the approved retail genuinely
// funds — the same latent failure #641 closed on the walk's own ceiling, and
// one click in the Suppliers panel away from firing.
//
// Returns 0 when the retail rate cannot fund any GPU cost at all (which the
// adapter reads as "no ceiling" — the caller only uses this value when it
// RAISES the ceiling, so 0 can never widen one).
func (e *Engine) wholesaleFundedBy(ctx context.Context, retailPerHour float64, providerName string, diskGB int) float64 {
	// Resolve markup through the SAME path retailWithDisk uses (admin store,
	// then env) — inverting through a different markup than the one that
	// produced the retail rate is how the round-trip stops closing.
	markup := e.markupFor(ctx, providerName)
	if floor := e.minMarginFor(ctx, providerName); floor > markup {
		markup = floor
	}
	// The effective percentage MAY be negative now (a per-supplier loss floor
	// with a matching negative markup): the division then RAISES the ceiling
	// above the approved retail, which is exactly right — a sanctioned 10% loss
	// means a $0.90 retail genuinely funds a $1.00 cost. The old `markup < 0 →
	// 0` clamp would under-fund the ceiling and 409 the very price the customer
	// just confirmed. At or below −100% no retail funds any cost: treat as
	// unfundable rather than divide by zero/negative (the admin handler and the
	// v101 CHECK both reject ≤ −100, so this is defense in depth).
	if markup <= -100 {
		return 0
	}
	diskWholesale := availability.DiskWholesalePerHour(diskGB, e.config.IncludedDiskGB(), e.config.DiskPricePerGBHourFor(providerName))
	funded := (retailPerHour+halfCentSlack)/(1.0+markup/100.0) - diskWholesale
	if funded <= 0 {
		return 0
	}
	return e.listBasisCeiling(ctx, funded, providerName)
}

// halfCentSlack is the most a round-to-nearest-cent retail rate can sit BELOW
// the exact marked-up cost it was derived from. See wholesaleFundedBy.
const halfCentSlack = 0.005

// provisionWalkParams carries the per-launch inputs the candidate walk needs
// beyond the provider request itself. Assembled once in Provision() (and by
// the boot-stall rescue path) from values that are fixed for the launch.
type provisionWalkParams struct {
	instanceID      string
	maxPricePerHour *float64
	// viewedPricePerHour is ProvisionRequest.ViewedPricePerHour — the card price
	// the customer saw (GPU-78). Enforced per attempt so a ghosted cheapest
	// candidate cannot fall through onto a pricier one at a price the customer
	// never laid eyes on. nil on the rescue walk (see attemptBootRescue).
	viewedPricePerHour *float64
	effectiveDisk      int
	autoSelectGPU      bool
	osID               string
	// Preserve the accepted environment, not the first candidate's concrete
	// image/bootstrap. A fallback can change provider deployment class.
	environment     string
	baseDockerImage string
	// defaultEnvironment is the default for a launch that named none (see
	// defaultEnvironmentFor). Unlike environment it is class-dependent: each
	// candidate gets it only on a container-native placement
	// (placementEnvironment), and the legacy path otherwise.
	defaultEnvironment string
	// rescue marks this walk as a boot-stall replacement, which spends against
	// the rescue allowance instead of the quote-drift one (see costCeilingFor).
	rescue bool
	// narrowsPlacement: the launch asks the provider for more than the
	// offering itself (a custom image, an explicit disk_gb, a template's CUDA
	// or disk floor, a raw_vm OS), so a capacity refusal may mean "no host
	// fits this request" rather than "this offering is gone", and placement
	// memory does not count it.
	narrowsPlacement bool
	// bootstrapData is the data the pre-walk bootstrap render used, kept so a
	// candidate needing a different transport or environment bootstrap can
	// be re-rendered mid-walk. nil when the launch has no FRP tunnel.
	bootstrapData *tunnel.BootstrapData
	// ledger carries the v112 attempt-ledger context. Its ZERO VALUE disables
	// recording entirely, which is deliberately what the unit tests that call
	// walkProvisionCandidates directly get — instrumenting a walk must not be a
	// precondition for running one.
	ledger attemptLedgerCtx
}

// attemptLedgerCtx is the per-launch context the v112 attempt ledger needs
// beyond what the walk already holds. Assembled in Provision() and carried
// through the rescue by bootRescue.walk.
//
// A nil seq disables recording. That is the only switch: every write below is
// guarded by it, so a caller that does not opt in pays nothing and behaves
// exactly as it did before the ledger existed.
type attemptLedgerCtx struct {
	orgID string
	// seq is a per-LAUNCH attempt counter, SHARED ACROSS PHASES via pointer.
	// bootRescue captures provisionWalkParams whole at the moment the original
	// walk succeeded, so the rescue walk inherits this same pointer and its
	// attempts continue the initial walk's numbering rather than restarting at
	// 1. That is what makes attempt_seq reconstruct a launch in order.
	seq *int
	// deadline is the launch's provisioning deadline, for budget_remaining_ms.
	// Zero means unmeasured (→ SQL NULL), never "no budget left" — see the
	// ProvisionAttempt struct comment for why that distinction is load-bearing.
	deadline time.Time
	// rescuesRemaining at the start of this walk. nil on the initial walk,
	// where no rescue has been spent and the allowance is not yet meaningful.
	rescuesRemaining *int
}

// next returns the next attempt sequence number for this launch, or 0 when the
// ledger is disabled.
func (l attemptLedgerCtx) next() int {
	if l.seq == nil {
		return 0
	}
	*l.seq++
	return *l.seq
}

// budgetRemainingMS reports the provisioning budget left right now, or nil when
// no deadline was supplied. A deadline already passed clamps to 0 rather than
// going negative: "began with no budget left" is the finding, and a negative
// number would just be a noisier way of saying it.
func (l attemptLedgerCtx) budgetRemainingMS(now time.Time) *int64 {
	if l.deadline.IsZero() {
		return nil
	}
	ms := l.deadline.Sub(now).Milliseconds()
	if ms < 0 {
		ms = 0
	}
	return &ms
}

// walkProvisionCandidates tries each candidate in order (bounded at
// maxProvisionAttempts), mutating provReq's per-attempt fields (region/DC,
// capacity class, native GPU variant, bid, OS image) from the candidate's
// offering before each provider call, and returns the winning provider,
// offering, and result. On exhaustion it returns the wrapped
// "all providers failed" error.
//
// Extracted verbatim from Provision() so the boot-stall rescue can re-run the
// same walk against freshly matched inventory; Provision()'s behavior is
// unchanged.
func (e *Engine) walkProvisionCandidates(ctx context.Context, provReq *provider.ProvisionRequest, candidates []ProviderCandidate, p provisionWalkParams) (prov provider.Provider, offering *provider.GPUOffering, provResult *provider.ProvisionResult, retErr error) {
	const maxProvisionAttempts = 3
	var lastErr error

	// v112 attempt ledger. One row per candidate the walk reaches, plus one
	// walk-level row when the walk ends without reaching another candidate.
	//
	// walkTerminal is set by every EARLY exit below and left empty when the
	// loop runs to completion. That is how the post-loop write distinguishes
	// "we ran out of attempts" from "we stopped on purpose" — the distinction
	// the caller's single "all providers failed" error throws away, and the one
	// this table exists to recover.
	phase := db.AttemptPhaseInitial
	if p.rescue {
		phase = db.AttemptPhaseRescue
	}
	walkTerminal := ""
	candidateCount := len(candidates)
	attemptsAllowed := maxProvisionAttempts

	// attemptRow builds one ledger row. c is nil for a row that names no
	// candidate. start is when the attempt began: the budget reading is taken
	// AT THAT MOMENT (what the attempt had to work with) while the duration
	// runs to now.
	attemptRow := func(seq int, c *ProviderCandidate, outcome, reason string, start time.Time) db.ProvisionAttempt {
		row := db.ProvisionAttempt{
			InstanceID:        p.instanceID,
			AttemptSeq:        seq,
			Phase:             phase,
			Outcome:           outcome,
			GPUType:           string(provReq.GPUType),
			GPUCount:          provReq.GPUCount,
			CandidatesMatched: &candidateCount,
			AttemptsAllowed:   &attemptsAllowed,
			BudgetRemainingMS: p.ledger.budgetRemainingMS(start),
			Reason:            reason,
		}
		if c != nil {
			row.Provider = c.Prov.Name()
			row.GPUType = string(c.Offering.GPUType)
			row.Region = c.Offering.Region
			row.CapacityClass = c.Offering.CapacityClass
		}
		ms := time.Since(start).Milliseconds()
		row.DurationMS = &ms
		return row
	}

	// The frpc transport the currently-rendered StartupScript carries, so a
	// cross-provider fail-over only re-renders when the transport actually
	// changes (see the per-candidate block below).
	var renderedTransport string
	if p.bootstrapData != nil {
		renderedTransport = p.bootstrapData.FRPTransport
	}

	// requote is the re-quote a cost-ceiling refusal produced while cheaper
	// candidates were left to try (see the refusal below). From then on the
	// walk tries only candidates that could place without it: within the
	// customer's caps and priced under requoteBelow, the highest retail
	// ceiling refused so far. If none of them places, the walk ends on the
	// re-quote, as it would have at the refusal.
	var requote *PriceExceededError
	var requoteBelow float64
	beatsRequote := func(c ProviderCandidate) bool {
		r := e.retailWithDisk(ctx, c.Offering.PricePerHour, c.Prov.Name(), p.effectiveDisk)
		return r < requoteBelow && (p.maxPricePerHour == nil || r <= *p.maxPricePerHour) &&
			!viewedPriceExceeded(r, p.viewedPricePerHour)
	}

	for attempt := 0; attempt < maxProvisionAttempts && attempt < len(candidates); attempt++ {
		c := candidates[attempt]
		if requote != nil && !beatsRequote(c) {
			continue
		}
		prov = c.Prov
		offering = &c.Offering

		attemptSeq := p.ledger.next()
		attemptStart := time.Now()

		// Per-attempt price cap: candidates are sorted by RETAIL price ascending,
		// so once one exceeds the caller's cap every later candidate does too.
		// Stop rather than place above what the customer approved — the cheapest
		// option ghosted and the next-cheapest is over the cap, which is exactly
		// the "price changed, re-confirm" case. Carry that price so the client can
		// re-quote. (candidates[0] already passed the fast-fail check above; this
		// only ever fires on a fallback attempt.)
		if p.maxPricePerHour != nil || p.viewedPricePerHour != nil {
			retail := e.retailWithDisk(ctx, c.Offering.PricePerHour, prov.Name(), p.effectiveDisk)
			if p.maxPricePerHour != nil && retail > *p.maxPricePerHour {
				lastErr = &PriceExceededError{CurrentBestPerHour: retail, MaxPricePerHour: *p.maxPricePerHour}
				walkTerminal = db.AttemptOutcomePriceCap
				e.recordAttempt(ctx, p.ledger, attemptRow(attemptSeq, &c, walkTerminal,
					fmt.Sprintf("retail %.4f over cap %.4f", retail, *p.maxPricePerHour), attemptStart))
				break
			}
			// The same stop against the card price the customer saw (GPU-78).
			// The pre-flight only vetted candidates[0]; without this a fallback
			// attempt is exactly the silent upcharge the pre-flight refuses, one
			// candidate later.
			if viewedPriceExceeded(retail, p.viewedPricePerHour) {
				lastErr = &PriceExceededError{CurrentBestPerHour: retail, MaxPricePerHour: *p.viewedPricePerHour}
				walkTerminal = db.AttemptOutcomeViewedPriceCap
				e.recordAttempt(ctx, p.ledger, attemptRow(attemptSeq, &c, walkTerminal,
					fmt.Sprintf("retail %.4f over viewed %.4f", retail, *p.viewedPricePerHour), attemptStart))
				break
			}
		}

		// Per-attempt COST ceiling. The retail cap above bounds what we may
		// SELL a candidate for, using the catalog row's price; this bounds what
		// the adapter may BUY once it re-searches live inventory, which is the
		// only place quote-to-accept drift can be caught before the accept
		// commits us (see costCeilingFor). Adapters that ignore the field are
		// unaffected — for them this is the same launch it was before.
		provReq.MaxCostPerHour = e.costCeilingFor(ctx, c.Offering.PricePerHour, prov.Name(), p.effectiveDisk, p.maxPricePerHour, p.rescue)

		// TMPL-04 auto-select: the matched offering IS the GPU choice when the
		// deploy omitted gpu_type. Forward its type per attempt because
		// auto-select candidates may span GPU types. (Raw + explicit-type
		// deploys leave provReq.GPUType as the caller-supplied value — for those
		// c.Offering.GPUType == req.GPUType anyway, so this is gated to keep the
		// existing paths visibly untouched.) GPUCount stays as requested.
		if p.autoSelectGPU {
			provReq.GPUType = c.Offering.GPUType
		}

		// Set bid price for spot instances (provider's raw price, pre-markup).
		// Offering.PricePerHour covers the whole GPUCount-GPU node; the bid is
		// per GPU, so divide back down. (Today only 1-GPU offerings reach the
		// spot path, making this a no-op — kept correct for multi-GPU rows.)
		// Shared derivation, not a local divide: an unreported count degrades
		// to the whole-node rate here exactly as it does on every priced
		// surface, and a bid is the one place a stray divide-by-zero would
		// reach an upstream as +Inf (D-27).
		provReq.BidPricePerGPU = availability.PerGPUHourly(c.Offering.PricePerHour, c.Offering.GPUCount)

		var candidateEnv envResolution
		// perCandidateEnv: the environment (explicit, or a class-dependent
		// default) is resolved for this candidate. env is "" when a default
		// does not apply here (a VM placement), which is the legacy path.
		perCandidateEnv := p.environment != "" || p.defaultEnvironment != ""
		env := placementEnvironment(p.environment, p.defaultEnvironment, prov.Name())
		if perCandidateEnv {
			if env != "" {
				var err error
				candidateEnv, err = e.resolveEnvironment(ProvisionRequest{Environment: env}, prov.Name(), string(c.Offering.GPUType))
				if err != nil {
					lastErr = fmt.Errorf("resolve environment for fallback candidate: %w", err)
					e.recordAttempt(ctx, p.ledger, attemptRow(attemptSeq, &c, db.AttemptOutcomeBootstrapRender,
						"environment: "+err.Error(), attemptStart))
					continue
				}
			}
			// Reset the previous candidate's override, including container -> VM.
			provReq.DockerImage = p.baseDockerImage
			if candidateEnv.dockerImageOverride != "" {
				provReq.DockerImage = candidateEnv.dockerImageOverride
			}
		}

		// Per-provider frpc transport (FRP_TRANSPORT_<NAME>). The bootstrap is
		// rendered ONCE before this walk, but fail-over can cross providers, so
		// a candidate whose transport differs from the rendered one must be
		// re-rendered here. Without this, an opted-in provider's websocket
		// config could ship to a fallback provider — or, worse, the opted-in
		// provider could inherit the default raw tunnel it cannot carry data
		// on, silently reproducing the bug this knob exists to work around.
		//
		// An explicit or default environment is also resolved and re-staged
		// per attempt: container and VM candidates need different image and
		// SSH bootstraps, and a default applies to one class only. Re-rendering
		// even the first candidate keeps rescue walks correct when their
		// incoming script belongs to an earlier winning candidate. Legacy
		// requests with no environment or transport override retain the
		// existing script unchanged.
		if p.bootstrapData != nil {
			if want := e.config.FRPTransportFor(c.Prov.Name()); want != renderedTransport || perCandidateEnv {
				data := *p.bootstrapData
				data.FRPTransport = want
				if perCandidateEnv {
					data.CertifiedImage = candidateEnv.certifiedImage
					data.DeploymentType = candidateEnv.deploymentType
					data.PinnedImageRef = candidateEnv.pinnedImageRef
					data.DriverFloor = candidateEnv.driverFloor
					data.ExpectFramework = expectedFramework(env)
				}
				script, err := tunnel.RenderBootstrap(data)
				if err != nil {
					// Skip rather than launch with the wrong transport: a box
					// whose tunnel can't carry data is worse than one fewer
					// candidate, and on a prepaid upstream it bills anyway.
					//
					// lastErr MUST be set before continuing: the loop's exit
					// path treats a nil lastErr as success and returns a nil
					// provResult, which the caller dereferences
					// (&provResult.UpstreamIP). A skip that left lastErr nil
					// would turn "every candidate failed to render" into a nil
					// pointer panic instead of a provisioning error.
					lastErr = fmt.Errorf("render bootstrap for %s transport %q: %w", c.Prov.Name(), want, err)
					e.recordAttempt(ctx, p.ledger, attemptRow(attemptSeq, &c, db.AttemptOutcomeBootstrapRender,
						"render: "+err.Error(), attemptStart))
					e.logger.Error("re-render bootstrap for provider transport failed; skipping candidate",
						slog.String("instance_id", p.instanceID),
						slog.String("provider", c.Prov.Name()),
						slog.String("frp_transport", want),
						slog.Any("error", err),
					)
					continue
				}
				// Re-stage as well as re-assign. provReq.StartupScript alone
				// only reaches providers that carry the full script in the
				// launch call. Container/stage-0-loader providers instead boot a tiny loader that FETCHES
				// GET /internal/instances/{id}/bootstrap, which serves the
				// row staged BEFORE this walk — i.e. the default-transport
				// render. Without this re-stage the override silently does
				// nothing for exactly the providers it exists for: the
				// instance downloads and runs the raw-TCP config while every
				// server-side signal says websocket.
				//
				// Measured on demo 2026-08-13 (instance gpu-052746bc): the
				// launch reported the override, the tunnel registered, and SSH
				// still died on the first client->container payload -- because
				// the box had fetched the staged TCP bootstrap. The upsert is
				// keyed by instance id, so re-staging per attempt is safe.
				if err := e.db.StageInstanceBootstrap(ctx, p.instanceID, script); err != nil {
					lastErr = fmt.Errorf("re-stage bootstrap for %s transport %q: %w", c.Prov.Name(), want, err)
					e.recordAttempt(ctx, p.ledger, attemptRow(attemptSeq, &c, db.AttemptOutcomeBootstrapRender,
						"stage: "+err.Error(), attemptStart))
					e.logger.Error("re-stage bootstrap for provider transport failed; skipping candidate",
						slog.String("instance_id", p.instanceID),
						slog.String("provider", c.Prov.Name()),
						slog.String("frp_transport", want),
						slog.Any("error", err),
					)
					continue
				}
				provReq.StartupScript = script
				renderedTransport = want
				// Positive instrumentation: without this the ONLY way to tell
				// which transport an instance actually booted with is to shell
				// into it -- impossible on the very provider whose SSH is
				// broken. This line is what makes the experiment falsifiable.
				if want != "" {
					e.logger.Info("provisioning with non-default frpc transport",
						slog.String("instance_id", p.instanceID),
						slog.String("provider", c.Prov.Name()),
						slog.String("frp_transport", want),
					)
				}
			}
		}

		// REGION-02 + REGION-03: forward the matched offering's NATIVE region
		// + datacenter id so the provider can pin the pod. Set per attempt
		// because each candidate may live in a different region/DC.
		provReq.Region = c.Offering.Region
		provReq.DatacenterLocation = c.Offering.DatacenterLocation

		// Forward the offering's capacity class so class-straddling adapters
		// launch in the capacity the customer priced (a marketplace adapter that
		// sells both secure and community capacity picks between them on it).
		// Adapters with a single class ignore it. Empty for adapters that don't
		// stamp the field — those ignore it, preserving legacy behavior. Set
		// per attempt because fallback candidates can differ in class.
		provReq.CapacityClass = c.Offering.CapacityClass

		// Forward the offering's provider-native GPU variant so the adapter
		// launches the exact SKU whose stock was advertised. Without this,
		// an adapter may reverse-map the canonical type to a preferred variant that
		// the pinned datacenter may not stock (A100 PCIe vs SXM4 — the
		// 2026-08-03 "Capacity changed while launching" loop). Empty for
		// adapters that don't stamp it; set per attempt because fallback
		// candidates can be different variants.
		provReq.UpstreamGPUID = c.Offering.UpstreamGPUID

		// Forward the matched offering's advertised CPU/RAM as a per-GPU FLOOR so
		// the adapter can constrain placement to a host that actually delivers the
		// specs on the card. Per GPU (like BidPricePerGPU) because Offering.RAMGB /
		// CPUCores describe the whole GPUCount-GPU node while the launch is sized in
		// GPUs.
		//
		// ONLY for offerings whose adapter vouches for the figures as live and
		// class-matched (SpecFloorGuaranteed). A displayed-but-unvouched spec —
		// an adapter's cross-region cache or curated flagship-spec guesses — would
		// filter out every host that actually exists and take an in-stock SKU dark;
		// a pinned launch (SelectedOfferingID) has exactly one candidate and no
		// fallback, so that failure would be total rather than degraded.
		//
		// Cleared FIRST on every iteration: provReq is reused across candidates, so
		// an unvouched candidate must not inherit the floor a previous vouched one
		// left behind. The vouch is per field — an adapter can stand behind its RAM
		// figure without standing behind its core count.
		provReq.MinRAMPerGPUGB, provReq.MinVCPUPerGPU = 0, 0
		if c.Offering.RAMFloorGuaranteed {
			provReq.MinRAMPerGPUGB = perGPUSpec(c.Offering.RAMGB, c.Offering.GPUCount)
		}
		if c.Offering.VCPUFloorGuaranteed {
			provReq.MinVCPUPerGPU = perGPUSpec(c.Offering.CPUCores, c.Offering.GPUCount)
		}

		// raw_vm:<os> — resolve THIS candidate provider's native image slug for
		// the picked os id (server-controlled vm_images map; candidates were
		// already narrowed to providers that carry one). Set per attempt because
		// fallback candidates can name the same OS differently. A resolution
		// miss here (manifest drift between selection and attempt) skips the
		// candidate rather than booting its default OS.
		if p.osID != "" {
			slug, ok := "", false
			if e.manifest != nil {
				slug, ok = e.manifest.VMImageSlug(prov.Name(), p.osID)
			}
			if !ok {
				lastErr = ErrOSUnavailable
				e.recordAttempt(ctx, p.ledger, attemptRow(attemptSeq, &c, db.AttemptOutcomeOSUnavailable,
					"no native image slug for os "+p.osID, attemptStart))
				continue
			}
			provReq.OSImage = slug
		}

		e.logger.Info("attempting provision with provider",
			slog.String("instance_id", p.instanceID),
			slog.String("provider", prov.Name()),
			slog.Float64("price_per_hour", offering.PricePerHour),
			slog.Int("attempt", attempt+1),
			// The floors actually asserted, so a "no capacity" on an in-stock SKU
			// is diagnosable as spec drift rather than a genuine stock-out. 0 = the
			// offering's specs were display-only (not SpecFloorGuaranteed).
			slog.Int("min_ram_per_gpu_gb", provReq.MinRAMPerGPUGB),
			slog.Int("min_vcpu_per_gpu", provReq.MinVCPUPerGPU),
		)

		provResult, lastErr = prov.Provision(ctx, *provReq)
		if lastErr == nil {
			// PLACED, not launched: the upstream accepted the create and the
			// box still has to boot. The post-placement outcomes (stall
			// verdicts, upstream-terminal) are recorded later, by the poll
			// path that learns them.
			e.recordAttempt(ctx, p.ledger, attemptRow(attemptSeq, &c, db.AttemptOutcomePlaced, "", attemptStart))
			if e.placement != nil {
				e.placement.RecordSuccess(candidateOfferingID(c))
			}
			break // Success
		}

		// The adapter declined to buy above the cost ceiling. Re-quote rather
		// than walk on into pricier candidates: this is not a failure to
		// place, it is a price the customer has not agreed to. Translating to
		// PriceExceededError here means the price-changed 409 and its
		// re-confirm flow work identically whether the drift was caught at the
		// catalog (the retail cap above) or at the accept.
		//
		// The remaining candidates are not all more expensive, though:
		// placement memory demotes a held offering behind fresh ones, so a
		// cheaper candidate can come after this one. One priced under this
		// candidate's ceiling could place without any re-quote, so when one
		// is left within the attempt budget the walk goes on to it (and
		// only to such candidates, see requote) instead of stopping here.
		var ceilErr *provider.CostCeilingError
		if errors.As(lastErr, &ceilErr) {
			approved := e.retailWithDisk(ctx, c.Offering.PricePerHour, prov.Name(), p.effectiveDisk)
			if p.maxPricePerHour != nil {
				approved = *p.maxPricePerHour
			}
			e.logger.Warn("declined launch above cost ceiling; re-quoting customer",
				slog.String("instance_id", p.instanceID),
				slog.String("provider", prov.Name()),
				slog.Float64("quoted_wholesale", c.Offering.PricePerHour),
				slog.Float64("cheapest_wholesale_now", ceilErr.CheapestCostPerHour),
				slog.Float64("ceiling_wholesale", ceilErr.CeilingCostPerHour),
			)
			quote := &PriceExceededError{
				CurrentBestPerHour: e.retailWithDisk(ctx, ceilErr.CheapestCostPerHour, prov.Name(), p.effectiveDisk),
				MaxPricePerHour:    approved,
			}
			if requote == nil || quote.CurrentBestPerHour < requote.CurrentBestPerHour {
				requote = quote
			}
			requoteBelow = max(requoteBelow, e.retailWithDisk(ctx, ceilErr.CeilingCostPerHour, prov.Name(), p.effectiveDisk))
			lastErr = requote
			reason := fmt.Sprintf("cheapest wholesale %.4f over ceiling %.4f",
				ceilErr.CheapestCostPerHour, ceilErr.CeilingCostPerHour)
			if slices.ContainsFunc(candidates[attempt+1:min(maxProvisionAttempts, len(candidates))], beatsRequote) {
				// Not the end of the walk, so not the terminal outcome.
				e.recordAttempt(ctx, p.ledger, attemptRow(attemptSeq, &c, db.AttemptOutcomeProviderRejected,
					reason, attemptStart))
				continue
			}
			walkTerminal = db.AttemptOutcomeCostCeiling
			e.recordAttempt(ctx, p.ledger, attemptRow(attemptSeq, &c, walkTerminal, reason, attemptStart))
			break
		}

		e.recordAttempt(ctx, p.ledger, attemptRow(attemptSeq, &c, db.AttemptOutcomeProviderRejected,
			lastErr.Error(), attemptStart))
		// The provider listed this offering and then could not place it.
		// Counted; from the second failure in a streak the offering stops
		// being sold for a while, so the next customer is not refused on the
		// same listing. Only ErrNoCapacity counts: a transport error or an
		// upstream 5xx (ErrProviderUnavailable) is the provider's outage, not
		// this offering. And only when the request asked for nothing beyond
		// the offering: with a custom image, an explicit disk, a template
		// floor, a raw_vm OS or a rescue's excluded host, the adapter
		// re-searches with this request's filters and "no capacity" can mean
		// only that no host fits this request, which must not hide the
		// offering from everyone else. The same holds for a request for
		// fewer GPUs than the offering has: selection admits any offering
		// with at least the requested count, but the adapter is asked for
		// the requested count (a marketplace adapter searches exactly that many
		// GPUs in the offering's datacenter), so the refusal is about a shape this
		// offering does not sell.
		if e.placement != nil && errors.Is(lastErr, provider.ErrNoCapacity) &&
			!p.narrowsPlacement && len(provReq.ExcludeUpstreamHostIDs) == 0 &&
			c.Offering.GPUCount == provReq.GPUCount {
			e.placement.RecordFailure(candidateOfferingID(c), "no capacity at create")
		}

		// Our account at this supplier is out of money: every remaining
		// candidate on it would be refused identically. Suspend the provider
		// sitewide (candidate selection + catalog, and a page) and drop its
		// remaining candidates from THIS walk so the bounded attempts go to
		// suppliers that can actually take the launch. This is the gap the
		// launch-outcome breaker cannot see — it only scores launches the
		// provider accepted (2026-09-21→23: ~3,200 refused launches).
		if errors.Is(lastErr, provider.ErrAccountUnfunded) {
			e.fundingGate.Trip(prov.Name(), "create_rejected", lastErr.Error(), true)
			candidates = withoutProviderAfter(candidates, attempt, prov.Name())
		}

		e.logger.Warn("provider provision failed, trying next candidate",
			slog.String("instance_id", p.instanceID),
			slog.String("provider", prov.Name()),
			slog.String("error", lastErr.Error()),
			slog.Int("attempt", attempt+1),
		)
	}

	if lastErr != nil && requote != nil {
		// A cost-ceiling refusal went on to cheaper candidates and none of
		// them placed: the walk ends on the re-quote, as it would have at the
		// refusal. Recorded as a walk-level cost_ceiling row, not as
		// exhausted candidates: the walk skipped the rest on purpose.
		lastErr = requote
		if walkTerminal == "" {
			walkTerminal = db.AttemptOutcomeCostCeiling
			e.recordAttempt(ctx, p.ledger, attemptRow(p.ledger.next(), nil,
				walkTerminal, "no cheaper candidate placed: "+requote.Error(), time.Now()))
		}
	}
	if lastErr != nil {
		// The loop ran to completion rather than breaking. WHICH limit it ran
		// into is the finding: a walk that spent its cap with candidates still
		// in the list is a cap set too low (config), while a walk that tried
		// every candidate it had is a supply problem that raising the cap
		// cannot touch. Typed here rather than left to be re-derived from the
		// two columns at read time.
		if walkTerminal == "" {
			exhausted := db.AttemptOutcomeCandidatesExhausted
			if candidateCount > attemptsAllowed {
				exhausted = db.AttemptOutcomeAttemptsExhausted
			}
			e.recordAttempt(ctx, p.ledger, attemptRow(p.ledger.next(), nil,
				exhausted, lastErr.Error(), time.Now()))
		}
		return nil, nil, nil, fmt.Errorf("provision: all providers failed (last: %s): %w", prov.Name(), lastErr)
	}
	return prov, offering, provResult, nil
}

// recordProvisionFailure appends a PRE-INSTANCE failure to the v82 ledger — a
// request that died before any instance row existed, and therefore leaves no
// trace in `instances` for the failures panel to count. This is the whole reason
// the ledger exists: "no upstream had capacity" is the strongest sourcing signal
// we get, and it used to be invisible.
//
// Called at exactly TWO sites, both terminal returns inside Provision, so one
// user-facing failure produces one event. It is deliberately NOT called inside
// SelectProviderCandidates or per walk attempt — internal retries are not
// separate failures and counting them would inflate the panel.
//
// Recording is best-effort and returns nothing: an insert error is logged and
// swallowed. A metrics write must never fail, slow, or change the outcome of a
// customer's launch. A nil DB records nothing.
func (e *Engine) recordProvisionFailure(ctx context.Context, req ProvisionRequest, stockRelated bool, reason string) {
	if e.db == nil {
		return
	}
	if err := e.db.RecordProvisionFailure(ctx, db.ProvisionFailureEvent{
		GPUType:  string(req.GPUType),
		GPUCount: req.GPUCount,
		OrgID:    req.OrgID,
		// InstanceID stays empty (→ SQL NULL): no instance row was ever created.
		StockRelated: stockRelated,
		Reason:       reason,
	}); err != nil {
		e.logger.Warn("provision failure ledger: record failed",
			slog.String("org_id", req.OrgID),
			slog.String("gpu_type", string(req.GPUType)),
			slog.String("error", err.Error()),
		)
	}
}

// ledgerWriteTimeout bounds a single v112 write.
//
// Deliberately tight. Detaching from the caller's context (see recordAttempt)
// also removed the short-circuit an already-expired caller used to give us: a
// write that would have returned instantly now waits on the pool. Against a
// wedged pool a launch writing four rows would add 4x this to the customer's
// wall clock BEFORE they even get their error — so the contract's "never slow"
// clause, not the insert's own needs, sets this number. The insert is a single
// unindexed append; if it cannot finish in two seconds the row is not worth the
// delay.
const ledgerWriteTimeout = 2 * time.Second

// preflightPriceRow builds the ledger row for a PRE-FLIGHT price refusal — the
// cheapest matched candidate costing more than the customer approved, caught
// before the walk ever runs.
//
// It names that candidate: which supplier priced us out is the finding, and the
// walk-level rows (where no candidate is implicated) are a different shape.
// CandidatesMatched is deliberately absent — selection succeeded, so the count
// is not what failed here, and populating it would invite reading this row as
// an exhaustion.
func preflightPriceRow(instanceID string, seq int, req ProvisionRequest, c ProviderCandidate, outcome, reason string) db.ProvisionAttempt {
	return db.ProvisionAttempt{
		InstanceID:    instanceID,
		AttemptSeq:    seq,
		Phase:         db.AttemptPhaseInitial,
		Outcome:       outcome,
		Provider:      c.Prov.Name(),
		GPUType:       string(c.Offering.GPUType),
		GPUCount:      req.GPUCount,
		Region:        c.Offering.Region,
		CapacityClass: c.Offering.CapacityClass,
		Reason:        reason,
	}
}

// recordAttempt appends one row to the v112 per-attempt ledger.
//
// Best-effort and returns nothing, exactly like recordProvisionFailure: this
// runs inside a customer's provisioning walk, so a telemetry write must never
// fail, slow, or change the outcome of a launch. A disabled ledger (nil seq)
// and a nil DB both record nothing.
//
// Unlike the v82 ledger this IS called per attempt — that is the whole point of
// the table. The two are not redundant: v82 counts user-facing failures for the
// admin panel, this reconstructs how the engine got there.
func (e *Engine) recordAttempt(ctx context.Context, l attemptLedgerCtx, a db.ProvisionAttempt) {
	if l.seq == nil || e.db == nil {
		return
	}
	a.OrgID = l.orgID
	a.RescuesRemaining = l.rescuesRemaining

	// DETACH from the caller's deadline, keeping its values. Ledger writes ride
	// contexts that are themselves the thing being measured: the rescue walk
	// runs under a 2-minute budget, and a walk that burns all of it is exactly
	// the row worth keeping — yet writing under that expired context drops it,
	// keeping only the fast failures and biasing the distribution toward the
	// outcomes we least need to see. WithoutCancel preserves the pricing
	// snapshot and anything else in the context while making the write immune
	// to the deadline it is reporting on.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ledgerWriteTimeout)
	defer cancel()
	if err := e.db.RecordProvisionAttempt(wctx, a); err != nil {
		e.logger.Warn("provision attempt ledger: record failed",
			slog.String("instance_id", a.InstanceID),
			slog.String("outcome", a.Outcome),
			slog.String("error", err.Error()),
		)
	}
}

func (e *Engine) Provision(ctx context.Context, req ProvisionRequest) (resp *ProvisionResponse, retErr error) {
	// Freeze this launch's markup resolution up front so the price cap checked
	// below and the rate persisted after placement cannot disagree — see
	// pricingSnapshot.
	ctx = withPricingSnapshot(ctx)

	// 1. Generate instance ID: "gpu-" + 4 random hex bytes.
	instanceID, err := generateInstanceID()
	if err != nil {
		return nil, fmt.Errorf("provision: generate instance ID: %w", err)
	}

	// v112 attempt-ledger context for this launch. The counter is a POINTER on
	// purpose: bootRescue captures the walk params whole, so every rescue this
	// launch later spends continues this same sequence instead of restarting at
	// 1, and attempt_seq reconstructs the launch in order across phases.
	//
	// No deadline here — the initial walk runs before any boot clock exists
	// (the budget is sized from the placement's EstimatedReadySeconds). The
	// rescue walk gets one, set in progressStatus, which is the phase where the
	// budget question actually bites.
	attemptSeqCounter := 0
	ledger := attemptLedgerCtx{orgID: req.OrgID, seq: &attemptSeqCounter}

	// Generate internal token: 16 random hex bytes for callback auth.
	internalToken, err := generateHexToken(16)
	if err != nil {
		return nil, fmt.Errorf("provision: generate internal token: %w", err)
	}

	// Per-instance hostname, set on the machine by tunnel bootstraps and
	// never shown to API callers. It has no domain: with the tunnel enabled,
	// GPUCTL_PUBLIC_URL must be set, and a bootstrap callback that falls back
	// to this name fails instead of calling a real domain.
	hostname := instanceID

	// GPU-92: a custom-image launch is on_demand only until the preemption
	// re-route can carry the full launch shape (see
	// ErrCustomImageSpotUnsupported). Checked before the "provisioning
	// instance" log and before any DB or spend work, so a refused request
	// leaves no trace of an instance id that will never exist.
	if req.CustomerImage && req.Tier == provider.TierSpot {
		return nil, ErrCustomImageSpotUnsupported
	}

	e.logger.Info("provisioning instance",
		slog.String("instance_id", instanceID),
		slog.String("gpu_type", string(req.GPUType)),
		slog.Int("gpu_count", req.GPUCount),
		slog.String("tier", string(req.Tier)),
	)

	// 2. Look up SSH keys from DB.
	// Smart default: if no explicit key IDs provided, auto-include all of the user's keys.
	var sshKeys []db.SSHKey
	if len(req.SSHKeyIDs) > 0 {
		// Explicit key IDs provided -- look them up.
		sshKeys, err = e.db.GetSSHKeysByIDs(ctx, req.SSHKeyIDs)
		if err != nil {
			return nil, fmt.Errorf("provision: look up ssh keys: %w", err)
		}
		// The keyless allowance below applies ONLY to the auto-include branch.
		// Explicit IDs that resolve to zero rows (deleted key, typo'd UUID in
		// a launch script) are a stated intent we cannot satisfy — keep the
		// pre-keyless 400 rather than silently billing for an instance whose
		// SSH doesn't work.
		if len(sshKeys) == 0 {
			return nil, ErrSSHKeysNotFound
		}
		// Defense-in-depth cross-org assertion (30-DESIGN §5.3.1, Plan
		// 30-08). GetSSHKeysByIDs does NOT filter by org_id — it
		// resolves a list of UUIDs verbatim. A user who is a member of
		// multiple orgs could submit an ssh_key_id from the wrong org
		// context (key auth scopes the request to one org,
		// but does not prevent wrong-org SSH keys from being attached
		// to the cloud-init authorized_keys). Reject the WHOLE request
		// on the first mismatch — no partial success, no silent skip,
		// because the customer's stated intent is "provision with
		// these keys" and we cannot satisfy that safely. Applies to
		// all tiers; the auto-include branch above is already
		// org-scoped via GetSSHKeysByOrgID so it bypasses this check.
		for _, k := range sshKeys {
			if k.OrgID != req.OrgID {
				return nil, ErrSSHKeyOrgMismatch
			}
			// v33 role scoping: an org member may only attach keys they OWN;
			// admins may attach any key in the org. Mirrors the auto-include
			// scoping below so the explicit path can't bypass it (e.g. a member
			// passing a teammate's ssh_key_id).
			if !req.IsAdmin && k.UserID != req.UserID {
				return nil, ErrSSHKeyForbidden
			}
		}
	} else {
		// No key IDs provided -- auto-include the per-key attach_on_provision
		// set, scoped by role (v33):
		//   - admin  -> ALL org keys flagged attach_on_provision
		//   - member -> their OWN keys flagged attach_on_provision (so a
		//               member's instance is not implicitly SSH-able by every
		//               other org member)
		// Filtering is in Go over the existing seam methods (org key counts are
		// tiny, capped at 50/org), so no new seam method is needed.
		// Admin OR an org-level system job (UserID empty, e.g. a managed
		// fine-tune) resolves ALL org keys; a member resolves only their own.
		var candidates []db.SSHKey
		if req.IsAdmin || req.UserID == "" {
			candidates, err = e.db.GetSSHKeysByOrgID(ctx, req.OrgID)
			if err != nil {
				return nil, fmt.Errorf("provision: look up org ssh keys: %w", err)
			}
		} else {
			candidates, err = e.db.ListSSHKeysByOrgAndUser(ctx, req.OrgID, req.UserID)
			if err != nil {
				return nil, fmt.Errorf("provision: look up user ssh keys: %w", err)
			}
		}
		for _, k := range candidates {
			if k.AttachOnProvision {
				sshKeys = append(sshKeys, k)
			}
		}
	}
	// Whether this provision gets the browser web console (v76). Computed
	// before the key gate below because it doubles as the keyless launch's
	// reachability guarantee.
	webTerminalOn := e.webConsoleAvailable(req.AppPort)

	if len(sshKeys) == 0 && !webTerminalOn {
		// Either the org/user has no keys, or every candidate is toggled off
		// (attach_on_provision = false). Both mean "nothing to attach". A
		// keyless instance is fine when the browser web console will be there
		// (the UI warns and the customer opted in) — but with no console and
		// no key the node would be flat-out unreachable, so refuse rather than
		// bill for a brick.
		return nil, ErrSSHKeysNotFound
	}

	// Collect public keys.
	sshPubKeys := make([]string, 0, len(sshKeys))
	for _, k := range sshKeys {
		sshPubKeys = append(sshPubKeys, k.PublicKey)
	}

	// Generate the per-instance app basic-auth credential whenever a
	// vhost-proxied surface exists: a template app (req.AppPort != 0), the web
	// console, or both — they share the one credential (D6). The password is
	// rendered into the bootstrap's http-proxy httpPwd, persisted on the row
	// (v74), and returned via the operation result. Generated BEFORE the
	// bootstrap build so it can be threaded into BootstrapData.
	var appUser, appPass string
	if req.AppPort != 0 || webTerminalOn {
		u, p, gerr := generateAppBasicAuth()
		if gerr != nil {
			return nil, fmt.Errorf("provision: %w", gerr)
		}
		appUser, appPass = u, p
	}

	// TMPL-04: whether this template deploy auto-selects its GPU (gpu_type
	// omitted). When true, the matched offering — not the empty req.GPUType — is
	// the authoritative GPU choice, forwarded to the provider per attempt and
	// recorded on the persisted instance row below.
	autoSelectGPU := req.autoSelectsGPU()

	// 4. Build candidate list.
	//
	// Reserved tier: bypass the availability cache. AllocateSlot is the
	// security gate (Plan 30-02) — it returns a Slot keyed to the org's
	// active contract or one of the typed errors that Plan 30-06 maps to
	// 4xx codes. The slot is held in 'allocated' state until either
	// CreateInstance succeeds (and the FK pins it) or the deferred
	// release-on-failure guard returns it to the pool.
	//
	// Non-reserved tiers (on_demand, spot) use the existing
	// availability-cache + price-sort path.
	// Resolve the instance disk once, here, before anything reads it. Candidate
	// narrowing, the price cap, the billed rate, the adapter payload and the
	// persisted row all consume this same value — recomputing it per site is
	// how the quoted price and the billed price drift apart.
	effectiveDisk := e.effectiveDiskGB(req)

	var candidates []ProviderCandidate
	// The reserved tier (operator-held capacity contracts) is not part of
	// GPU.ai Open Source; a reserved request is refused before any upstream
	// call.
	if req.Tier == provider.TierReserved {
		return nil, ErrReservedNotConfigured
	}
	{
		var selErr error
		var trace *selectionTrace
		if e.placement != nil && req.SelectedOfferingID != "" {
			trace = newSelectionTrace()
		}
		candidates, selErr = e.selectProviderCandidates(ctx, req, trace)
		if selErr != nil {
			// Ledger: selection found nothing to place on — a pre-instance
			// rejection, invisible to the failures panel before v82. Only the two
			// supply sentinels count. Every other selection error (price cap,
			// disk, OS, region) is a policy/config denial, not a stock-out, and
			// recording it would pollute the unmet-demand signal.
			if errors.Is(selErr, ErrNoProvider) || errors.Is(selErr, ErrOfferingUnavailable) {
				e.recordProvisionFailure(ctx, req, true, selErr.Error())
			}
			// A pinned offering the catalog showed is gone at placement.
			// Counted the same way: a listing that keeps vanishing between the
			// catalog read and the launch (a marketplace host rented in between,
			// or community stock that is listed on one poll and gone on the
			// next) leaves the catalog from its second failure instead of
			// being re-listed to the next customer at the next poll. Only when
			// the offering itself is gone (pinVanished): a pin the request
			// excluded, or one on a provider that was skipped or failed to
			// list, is not the offering's fault.
			if errors.Is(selErr, ErrOfferingUnavailable) && e.pinVanished(req, trace) {
				e.placement.RecordFailure(req.SelectedOfferingID, "gone at placement")
			}
			// The attempt ledger records EVERY selection failure, not just the
			// two supply sentinels the v82 ledger counts. The distinction that
			// matters there — unmet demand vs. a policy denial — is a property
			// of the panel, not of the walk: a config denial that quietly kills
			// launches is exactly the thing this table is for. The outcome says
			// selection produced nothing to try; the reason says why.
			e.recordAttempt(ctx, ledger, db.ProvisionAttempt{
				InstanceID: instanceID,
				AttemptSeq: ledger.next(),
				Phase:      db.AttemptPhaseInitial,
				Outcome:    db.AttemptOutcomeNoCandidates,
				GPUType:    string(req.GPUType),
				GPUCount:   req.GPUCount,
				Reason:     selErr.Error(),
			})
			return nil, selErr
		}
		// Check price cap against the cheapest candidate's RETAIL price — the
		// cap comes from the user, who only ever sees catalog (retail) prices.
		// Fail fast here (before env resolution / port allocation) and carry the
		// current best price so the client can re-quote. The per-attempt guard in
		// the provision loop below enforces the SAME cap on fallback candidates —
		// without it, a ghosted cheapest could silently place on a pricier next
		// candidate and bill ABOVE what the customer approved.
		if req.MaxPricePerHour != nil || req.ViewedPricePerHour != nil {
			best := e.retailWithDisk(ctx, candidates[0].Offering.PricePerHour, candidates[0].Prov.Name(), effectiveDisk)
			if req.MaxPricePerHour != nil && best > *req.MaxPricePerHour {
				// Ledger: the PRE-FLIGHT refusal, which the walk never sees.
				// Without this row the price_cap outcome could only ever be
				// written for a fallback candidate (the walk's own guard fires
				// only after candidates[0] already passed here), so a launch
				// priced out on its cheapest candidate — the common shape —
				// would record nothing at all and read as a rare cause.
				e.recordAttempt(ctx, ledger, preflightPriceRow(instanceID, ledger.next(), req,
					candidates[0], db.AttemptOutcomePriceCap,
					fmt.Sprintf("best retail %.4f over cap %.4f", best, *req.MaxPricePerHour)))
				return nil, &PriceExceededError{CurrentBestPerHour: best, MaxPricePerHour: *req.MaxPricePerHour}
			}
			// Same refusal against the price the customer actually LOOKED at
			// (GPU-78). A pinned launch resolves to exactly one candidate, so for
			// the dashboard this single check covers the whole card -> re-quote
			// leg — the leg that had no protection at all and silently billed the
			// new price. Deliberately a separate branch from the cap above:
			// MaxPricePerHour is consent, this is only recognition, and the two
			// must never be conflated (see ProvisionRequest.ViewedPricePerHour).
			if viewedPriceExceeded(best, req.ViewedPricePerHour) {
				e.recordAttempt(ctx, ledger, preflightPriceRow(instanceID, ledger.next(), req,
					candidates[0], db.AttemptOutcomeViewedPriceCap,
					fmt.Sprintf("best retail %.4f over viewed %.4f", best, *req.ViewedPricePerHour)))
				return nil, &PriceExceededError{CurrentBestPerHour: best, MaxPricePerHour: *req.ViewedPricePerHour}
			}
		}
	}

	// Docker-normalization environment resolution (Phase 63, D-63-04/D-63-08).
	// req.Environment is the SERVER-VALIDATED class from the handler; the engine
	// re-derives every concrete field itself against the PLACED provider's
	// capability + the server-controlled manifest (V5 — never customer input).
	// "" keeps the legacy path byte-for-byte. A certified request for a GPU with
	// no manifest entry errors here — we never launch an unpinned certified image.
	//
	// A launch that names no environment gets the default one on
	// container-native placements only (placementEnvironment). The candidate
	// set above is the legacy one either way, so the default never turns a
	// launch that would have placed into a refusal; the walk re-resolves it
	// per candidate.
	defaultEnv := e.defaultEnvironmentFor(req)
	placed := req
	placed.Environment = placementEnvironment(req.Environment, defaultEnv, candidates[0].Prov.Name())
	if defaultEnv != "" {
		e.logger.Info("provision: default environment for container-native placements",
			slog.String("instance_id", instanceID),
			slog.String("environment", defaultEnv),
			slog.String("gpu_type", string(req.GPUType)),
			slog.Bool("first_candidate_applies", placed.Environment != ""),
		)
	}
	envRes, envErr := e.resolveEnvironment(placed, candidates[0].Prov.Name(), string(candidates[0].Offering.GPUType))
	if envErr != nil {
		return nil, envErr
	}
	// Defense in depth (custom-images Phase 1): the handler 422s a request
	// combining a customer image with an environment selection, because
	// envRes.dockerImageOverride below would silently DISCARD the customer
	// image in favor of the certified ref. If a future caller reaches the
	// engine with both anyway, fail loudly rather than launch the wrong image.
	if req.CustomerImage && req.Environment != "" {
		return nil, fmt.Errorf("provision: custom image cannot be combined with environment %q", req.Environment)
	}

	// 5. Build callback URL using public URL if configured, falling back to branded hostname.
	callbackURL := buildCallbackURL(e.config.GpuctlPublicURL, hostname, instanceID)
	// TELEM-01: the in-pod sampler's ingest URL, built the same way.
	telemetryURL := buildTelemetryURL(e.config.GpuctlPublicURL, hostname, instanceID)

	// 6. Optionally allocate FRP port and render bootstrap script.
	// FRP setup happens once before the provider retry loop.
	var frpPort *int
	var startupScript string
	// Kept past the render block so walkProvisionCandidates can re-render for a
	// candidate whose provider overrides the frpc transport. nil => no tunnel.
	var walkBootstrapData *tunnel.BootstrapData

	if e.tunnelMgr != nil {
		// The allocator must walk the SAME span the frps gateway allows, so the
		// range comes from FRP_ALLOW_PORTS rather than the tunnel constants. A
		// malformed value cannot reach here — tunnel.NewManager parses the same
		// string at boot and fails startup — so a parse error means the manager
		// was built some other way; fall back to the default span rather than
		// failing a customer launch over it.
		portRange, prErr := tunnel.ParsePortRange(e.config.FRPAllowPorts)
		if prErr != nil {
			e.logger.Error("invalid FRP_ALLOW_PORTS, falling back to default port range",
				slog.String("configured", e.config.FRPAllowPorts),
				slog.String("fallback", tunnel.DefaultPortRange().String()),
				slog.String("error", prErr.Error()),
			)
			portRange = tunnel.DefaultPortRange()
		}

		// Allocate FRP remote port within a transaction (advisory lock prevents races).
		tx, err := e.db.PgxPool().Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("provision: begin tx for FRP port: %w", err)
		}
		remotePort, err := tunnel.AllocatePort(ctx, tx, portRange)
		if err != nil {
			_ = tx.Rollback(ctx)
			return nil, fmt.Errorf("provision: allocate FRP port: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("provision: commit FRP port tx: %w", err)
		}
		frpPort = &remotePort

		// D-05: Port↔row atomicity is satisfied by terminate-on-failure at
		// CreateInstance (see the persist branch below), NOT a single port+row
		// transaction — holding this tx open across prov.Provision() (the slow
		// upstream HTTP call) would exhaust the pgx pool under load (Pitfall 1).
		// AllocatePort writes no row (internal/tunnel/ports.go: it advances a
		// round-robin cursor under an advisory xact lock and the caller persists
		// the port via CreateInstance), so a CreateInstance failure leaks NO
		// active port row; the cursor simply skipped a value and that port is
		// returned by a later AllocatePort once the cursor cycles back to it. The
		// concurrent-allocator race (two provisions landing on the same cursor
		// value before either writes its row) is backstopped by the v7 partial
		// unique index instances_frp_remote_port_active, which makes the second
		// CreateInstance fail with a unique violation -> terminate-on-failure
		// fires. The reconciler (Plan 32-05) is the residual-leak backstop.

		e.logger.Info("allocated FRP remote port",
			slog.String("instance_id", instanceID),
			slog.Int("remote_port", remotePort),
		)

		// Resolve the frps public host for the pod's bootstrap serverAddr.
		// Prefer the explicit FRP_PUBLIC_HOST (frps on a dedicated gateway,
		// separate from the load balancer that fronts the API). Fall back to
		// extracting from GpuctlPublicURL, where one host fronts both HTTPS
		// and frps.
		proxyHost := e.config.FRPPublicHost
		if proxyHost == "" {
			proxyHost = extractHost(e.config.GpuctlPublicURL)
		}

		// Render FRP bootstrap script.
		bootstrapData := tunnel.BootstrapData{
			InstanceID:        instanceID,
			ProxyHost:         proxyHost,
			FRPServerPort:     e.config.FRPBindPort,
			FRPToken:          e.config.FRPToken,
			RemotePort:        remotePort,
			AllowedPorts:      portRange,
			SSHAuthorizedKeys: strings.Join(sshPubKeys, "\n"),
			InternalToken:     internalToken,
			Hostname:          hostname,
			CallbackURL:       callbackURL,
			TelemetryURL:      telemetryURL,
		}
		// Bootstrap-asset mirror (frpc tarball): presign a short-lived GET
		// URL so the instance can fetch frpc from our bucket before falling
		// back to GitHub — provider VM egress is flaky per-destination and a
		// boot-time github.com blackout otherwise kills the tunnel for good.
		// Presigning is local SigV4 math (no network call). Best-effort: any
		// error (or a non-https dev-stub URL) degrades to GitHub-only, never
		// fails the provision.
		if e.assetStore != nil {
			key := fmt.Sprintf("frp/frp_%s_linux_amd64.tar.gz", tunnel.FRPVersion)
			mirrorURL, err := e.assetStore.PresignGetObject(ctx, key, time.Hour)
			if err != nil || !strings.HasPrefix(mirrorURL, "https://") {
				e.logger.Warn("bootstrap asset mirror unavailable, falling back to GitHub-only frpc download",
					slog.String("instance_id", instanceID),
					slog.String("key", key),
					slog.Any("error", err),
				)
			} else {
				bootstrapData.FRPCMirrorURL = mirrorURL
			}
		}
		// Template deploys (AppPort != 0) add the http-vhost proxy + basic-auth
		// to the bootstrap. AppSubdomain = instanceID; frps prepends it to the
		// subDomainHost so the app is reachable at <id>.apps.example.com. The
		// generated password lands in httpPwd here (and ONLY here + the
		// operation result). Default the ready timeout to 300s when unset.
		if req.AppPort != 0 {
			readyTimeout := req.AppReadyTimeout
			if readyTimeout == 0 {
				readyTimeout = 300
			}
			bootstrapData.AppPort = req.AppPort
			bootstrapData.AppSubdomain = instanceID
			bootstrapData.AppBasicAuthUser = appUser
			bootstrapData.AppBasicAuthPass = appPass
			bootstrapData.AppReadyTimeout = readyTimeout
		}
		// Web console (v76): ttyd on the loopback, vhost-proxied at
		// <id>-term.<apps host> behind the same per-instance credential.
		// Mirror the ttyd binary like the frpc tarball above — same flaky
		// per-destination egress, same best-effort degradation to GitHub.
		if webTerminalOn {
			bootstrapData.TerminalPort = webTerminalPort
			bootstrapData.AppBasicAuthUser = appUser
			bootstrapData.AppBasicAuthPass = appPass
			if e.assetStore != nil {
				key := fmt.Sprintf("ttyd/ttyd_%s_x86_64", tunnel.TtydVersion)
				mirrorURL, err := e.assetStore.PresignGetObject(ctx, key, time.Hour)
				if err != nil || !strings.HasPrefix(mirrorURL, "https://") {
					e.logger.Warn("bootstrap asset mirror unavailable, falling back to GitHub-only ttyd download",
						slog.String("instance_id", instanceID),
						slog.String("key", key),
						slog.Any("error", err),
					)
				} else {
					bootstrapData.TtydMirrorURL = mirrorURL
				}
			}
		}
		// Phase 63: server-derived Docker-normalization fields. Empty for legacy
		// launches (envRes zero-valued when req.Environment == ""), so the raw
		// bootstrap renders byte-for-byte unchanged. For certified-VM these carry
		// the digest-pinned image + driver floor the bootstrap runs/enforces.
		bootstrapData.CertifiedImage = envRes.certifiedImage
		bootstrapData.DeploymentType = envRes.deploymentType
		bootstrapData.PinnedImageRef = envRes.pinnedImageRef
		bootstrapData.DriverFloor = envRes.driverFloor
		// Startup GPU compute check: the count the customer pays for and the
		// framework their environment promises.
		bootstrapData.GPUCheckMode = e.gpuCheckMode
		if req.FleetProbe {
			bootstrapData.ProbeURL = buildProbeURL(e.config.GpuctlPublicURL, hostname, instanceID)
		}
		bootstrapData.ExpectedGPUCount = req.GPUCount
		bootstrapData.ExpectFramework = expectedFramework(placed.Environment)
		// Custom image (Phases 1+3). Container-native placement: the bootstrap
		// runs INSIDE the customer container and launches the image's own
		// entrypoint/cmd after sshd + tunnel setup (CustomStartB64). VM
		// placement (Phase 3): the bootstrap owns the HOST — sshd + tunnel on
		// the VM, then docker-runs the digest-pinned ref beside them with the
		// pre-flight argv, env file, and declared ports; the manifest driver
		// floor gates the run exactly like the certified path. Every argv/env
		// byte travels b64-enveloped, never interpolated into shell source.
		// The candidate set is class-homogeneous (see SelectProviderCandidates),
		// so rendering against candidates[0]'s class holds for every fallback.
		if req.CustomerImage {
			if provider.DeploymentType(candidates[0].Prov.Name()) == "vm" {
				bootstrapData.DeploymentType = "vm"
				bootstrapData.CustomerImageRef = req.Image
				bootstrapData.CustomerEntrypointB64 = argvB64(req.ImageEntrypoint)
				bootstrapData.CustomerArgsB64 = argvB64(req.ImageCmd)
				bootstrapData.CustomerEnvB64 = envFileB64(req.Env)
				for _, p := range req.Ports {
					bootstrapData.CustomerPorts = append(bootstrapData.CustomerPorts, p.Port)
				}
				if e.manifest != nil {
					if ge, ok := e.manifest.Lookup(strings.ReplaceAll(string(req.GPUType), "_", " ")); ok {
						bootstrapData.DriverFloor = ge.DriverFloor
					} else {
						e.logger.Warn("customer image on VM without a manifest driver floor",
							slog.String("instance_id", instanceID),
							slog.String("gpu_type", string(req.GPUType)),
						)
					}
				}
			} else {
				bootstrapData.CustomStartB64 = customStartB64(req.ImageEntrypoint, req.ImageCmd)
			}
		}
		startupScript, err = tunnel.RenderBootstrap(bootstrapData)
		if err != nil {
			return nil, fmt.Errorf("provision: render bootstrap: %w", err)
		}
		walkBootstrapData = &bootstrapData
	}

	// 6b. Stage the rendered bootstrap so the instance can fetch it at boot
	// (GET /internal/instances/{id}/bootstrap, per-instance token auth).
	// Adapters whose provider can't carry the full script per launch (e.g. a
	// small per-account script quota) send only a static stage-0 loader plus
	// this fetch URL. Staged for every provider — one small upsert — so the
	// engine stays adapter-agnostic. The row is deleted on the running
	// transition and swept after 24h.
	if startupScript != "" {
		if err := e.db.StageInstanceBootstrap(ctx, instanceID, startupScript); err != nil {
			return nil, fmt.Errorf("provision: stage bootstrap: %w", err)
		}
	}

	// 7. Build provider-level request and try provisioning with fallback retry.
	// Only the provider.Provision call is retried with different providers (max 3 attempts).
	// FRP port allocation happened once above.
	//
	// REGION-03: Region/DatacenterLocation are populated PER ATTEMPT from the
	// matched offering, because req.Regions is a CANONICAL filter set whereas
	// providers expect their NATIVE region label. Each candidate carries its
	// native region forward via Offering.Region.
	provReq := provider.ProvisionRequest{
		InstanceID:    instanceID,
		GPUType:       req.GPUType,
		GPUCount:      req.GPUCount,
		Tier:          req.Tier,
		SSHPublicKeys: sshPubKeys,
		InternalToken: internalToken,
		CallbackURL:   callbackURL,
		StartupScript: startupScript,
		// Template fields (Plan 03 already added these to provider.ProvisionRequest;
		// here we only POPULATE them). Empty for raw instances -> the adapter
		// falls back to its default image and the SSH-only "22/tcp" port set, so
		// the raw path is byte-for-byte unchanged.
		//
		// ContainerDiskGB is no longer template-only: effectiveDisk is populated
		// on EVERY launch (customer request, else server default, floored by the
		// template). Adapters that can't configure disk ignore it, which is why
		// filling it does not narrow placement.
		DockerImage:     req.Image,
		RegistryAuth:    req.RegistryAuth,
		Ports:           req.Ports,
		Env:             req.Env,
		ContainerDiskGB: effectiveDisk,
	}
	// The disk floor travels ONLY for an explicit customer request — the same
	// requireDiskGB scoping candidate narrowing uses. Request-scoped, not
	// per-candidate: the requested size doesn't change across fallback attempts.
	// An adapter may additionally floor placement on its own create size
	// (GPU-73), so default launches do not land on short-disk hosts; MinDiskGB
	// stays explicit-only here.
	if req.RequestedDiskGB != nil {
		provReq.MinDiskGB = effectiveDisk
	}
	// The template's driver floor travels the same way — request-scoped, not
	// per-candidate, since the image does not change across fallback attempts.
	provReq.MinCUDA = req.MinCUDA
	// Phase 63 container-native certified: point the adapter image at the pinned
	// manifest ref so the provider runs the Certified Image natively (no nested
	// docker in bootstrap). Empty for every other path, so DockerImage keeps its
	// caller/template value and the raw + template paths stay byte-for-byte intact.
	if envRes.dockerImageOverride != "" {
		provReq.DockerImage = envRes.dockerImageOverride
	}
	// Carry the engine-allocated FRP remote port to the adapter (D-59-01). Only
	// the community adapter reads it; every other adapter ignores it. frpPort is
	// the surviving *int (remotePort is block-scoped to the tunnel setup above and
	// out of scope here) — nil-guarded, exactly as db.Instance{FRPRemotePort: frpPort}
	// below persists the same pointer.
	if frpPort != nil {
		provReq.FRPRemotePort = *frpPort
	}

	walkParams := provisionWalkParams{
		instanceID:         instanceID,
		maxPricePerHour:    req.MaxPricePerHour,
		viewedPricePerHour: req.ViewedPricePerHour,
		effectiveDisk:      effectiveDisk,
		autoSelectGPU:      autoSelectGPU,
		osID:               envRes.osID,
		environment:        req.Environment,
		defaultEnvironment: defaultEnv,
		baseDockerImage:    req.Image,
		bootstrapData:      walkBootstrapData,
		ledger:             ledger,
		// The request-specific placement filters the adapter applies on top
		// of the offering: the custom image (template or customer), the
		// explicit disk (MinDiskGB), the template's CUDA floor, a disk
		// above the default (a template floor; an adapter may place on the
		// create size), and a raw_vm OS pick (the adapter launches that OS image,
		// which not every host in the offering may carry).
		narrowsPlacement: req.Image != "" || req.RequestedDiskGB != nil || req.MinCUDA > 0 ||
			effectiveDisk > e.config.DefaultDiskGB() || envRes.osID != "",
	}
	prov, offering, provResult, walkErr := e.walkProvisionCandidates(ctx, &provReq, candidates, walkParams)
	if walkErr != nil {
		// All attempts failed. FRP needs no cleanup -- the port allocation committed
		// but the instance row was never created, so no active instance holds the
		// port; the round-robin cursor will hand it out again once it cycles back.
		// This is the same D-05 reasoning as the FRP-port tx above: AllocatePort
		// writes no row.
		// (Distinct from the persist-failure path below, where the upstream WAS
		// created and so is terminated — see the CreateInstance guard.)
		//
		// Ledger: every candidate was tried and every one refused, with no
		// instance row to carry the failure. stock_related tracks whether the
		// last upstream said "no capacity" — that case IS unmet demand; a
		// credential or API break is not.
		e.recordProvisionFailure(ctx, req, errors.Is(walkErr, provider.ErrNoCapacity), walkErr.Error())
		return nil, walkErr
	}

	// TMPL-04: auto-select resolved the GPU type via the matched (winning)
	// offering — reflect it on the request so the persisted instance row records
	// the real GPU type instead of the empty placeholder the caller sent.
	if autoSelectGPU {
		req.GPUType = offering.GPUType
	}

	// 8. Build and persist instance record.
	upstreamIP := &provResult.UpstreamIP
	if provResult.UpstreamIP == "" {
		upstreamIP = nil
	}

	// The customer is billed the RETAIL rate — the same RetailPrice formula
	// the availability poller applies to the catalog (retail floor included:
	// never below wholesale × (1 + config.MinRetailMarkupPct), rounded up to
	// the cent), so the price shown at rent time and the rate on the billing
	// session/usage table are identical — and a launch can no more undercut
	// upstream cost than the catalog can.
	// Offering.PricePerHour is the provider's wholesale whole-node rate;
	// UpstreamPricePerHour keeps the provider's actual charge for margin
	// tracking. Reserved tier carries 0 (flat-fee, off-platform) — unaffected
	// (RetailPrice never floors a non-positive wholesale).
	retailPricePerHour := e.billedRetail(ctx, offering.PricePerHour, provResult.CostPerHour, prov.Name(), effectiveDisk, req.MaxPricePerHour)

	// Upstream (COGS) basis for the margin panel. Prefer the provider's actual
	// reported per-instance charge (some adapters echo the live rate, which can
	// differ from the catalog e.g. spot); fall back to the offering's wholesale
	// whole-node rate — the same number RetailPrice marks up — when the adapter
	// does not report one. Without this fallback every provider that doesn't echo
	// a charge stored upstream=0,
	// which zeroed COGS and showed a false +100% margin in the admin panel.
	// Reserved tier intentionally keeps 0 (flat-fee, off-platform).
	upstreamPricePerHour := provResult.CostPerHour
	if upstreamPricePerHour <= 0 && req.Tier != provider.TierReserved {
		upstreamPricePerHour = offering.PricePerHour
	}

	// Partner cost discounts (PROVIDER_COST_DISCOUNT_PCT_<NAME>): some providers
	// invoice us below their listed/reported prices (e.g. a partner agreement that bills 10%
	// under list at invoicing while the supplier's API still reports list). Record the discounted rate as COGS so the margin
	// panel reflects what we actually pay, not the list price.
	if disc := e.costDiscountFor(ctx, prov.Name()); disc > 0 && upstreamPricePerHour > 0 {
		upstreamPricePerHour *= 1.0 - disc/100.0
	}

	inst := db.Instance{
		InstanceID:           instanceID,
		OrgID:                req.OrgID,
		UserID:               req.UserID,
		UpstreamProvider:     prov.Name(),
		UpstreamID:           provResult.UpstreamID,
		UpstreamIP:           upstreamIP,
		Hostname:             hostname,
		FRPRemotePort:        frpPort,
		Name:                 req.Name,
		GPUType:              string(req.GPUType),
		GPUCount:             req.GPUCount,
		Tier:                 string(req.Tier),
		Region:               regionFromResult(provResult, offering),
		PricePerHour:         retailPricePerHour,
		UpstreamPricePerHour: upstreamPricePerHour,
		Status:               StateCreating,
		InternalToken:        &internalToken,
		TemplateID:           req.TemplateID,
		// The customer's per-instance runtime bound (schema v92, GPU-60). Only
		// the REQUESTED hours are persisted here; the absolute deadline is
		// armed later, by SetInstanceRunning, so the bound counts usable
		// runtime rather than the provisioning window. nil for every launch
		// that didn't ask for one.
		AutoTerminateHours: req.AutoTerminateHours,
	}
	// Record the customer's canonical image ref (schema v89). Gated on
	// CustomerImage so template/certified images never land in the column —
	// those stay server-internal (TMPL-13).
	if req.CustomerImage && req.Image != "" {
		img := req.Image
		inst.Image = &img
		// Record the declared primary port (schema v90) so reads derive
		// app_url — template rows key off template_id instead and stay NULL.
		if req.AppPort != 0 {
			ap := req.AppPort
			inst.AppPort = &ap
		}
	}
	// Persist the app credential (schema v74, revising the D6 surface-once
	// rule) so the org-scoped instance reads can re-surface the app/console
	// login for the instance's lifetime — the "Open in browser" button is
	// useless if the credential dies with the deploy modal. Same trust
	// boundary as internal_token on the row above. Still never logged.
	if req.AppPort != 0 || webTerminalOn {
		inst.AppAuthUser = &appUser
		inst.AppAuthPass = &appPass
	}
	// Record the web-console port (schema v76): non-NULL is the signal that
	// this instance actually has ttyd on board, gating terminal_url on reads
	// so pre-v76 rows never surface a dead console link.
	if webTerminalOn {
		tp := webTerminalPort
		inst.TerminalPort = &tp
	}
	// Record the disk ONLY when the winning provider honors the size we send
	// (ConfigurableDisk) — then effectiveDisk is literally the filesystem the
	// customer received and the row is a fact. For everyone else the machine's
	// disk is the upstream's decision (a VM flavor brings its own volume), and
	// recording our number would fabricate a spec the customer can hold us to —
	// the same honesty rule the catalog's instance_disk_gb follows. NULL means
	// "provider-determined".
	if provider.ConfigurableDisk(prov.Name()) {
		d := effectiveDisk
		inst.DiskGB = &d
	}
	if err := e.db.CreateInstance(ctx, &inst); err != nil {
		// Upstream succeeded but we can't track it — terminate or we pay forever.
		// This guard covers BOTH BLOCK-02 (generic persist failure) AND BLOCK-03
		// (a v7 partial-unique-index violation on frp_remote_port from a concurrent
		// allocator race surfaces HERE as a CreateInstance error — see the D-05
		// note at the FRP-port tx above).
		// Detached ctx: the caller's ctx may already be cancelling. Best-effort:
		// the reconciler (internal/reconcile, Plan 32-05) is the backstop if this
		// Terminate fails.
		tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if termErr := prov.Terminate(tctx, provResult.UpstreamID, provReq.Region); termErr != nil {
			e.logger.Error("CRITICAL: orphaned upstream after persist failure",
				slog.String("instance_id", instanceID),
				slog.String("upstream_id", provResult.UpstreamID),
				slog.String("persist_error", err.Error()),
				slog.String("terminate_error", termErr.Error()),
			)
		}
		cancel()
		return nil, fmt.Errorf("provision: persist instance: %w", err)
	}

	// instance.creating webhook (GPU-45): the record now exists, the create
	// was accepted. Exactly-once by construction — this is the sole
	// CreateInstance site and each provision creates exactly one row. A
	// later boot-stall rescue re-points this SAME row (RebindInstanceUpstream)
	// and must NOT re-emit creating; see InstanceEventSink.
	e.emitInstanceCreating(ctx, &inst)

	// Billing does NOT start here. We deliberately eat the cost of the
	// creating/provisioning/booting window — the customer can't use the
	// instance until it reaches running, so the meter must not run before
	// then. The billing session is opened at the running transition, which
	// the ready callback (api.handleInstanceReady) owns as the sole authority.
	// A provision that fails/times out before running therefore never opens a
	// session and is never billed; closeBillingSessionForFailedProvision stays
	// a safe idempotent no-op for that path.

	// Capture the boot-rescue state (bootstall.go) while everything the rescue
	// needs is in hand: the engine request (fresh candidate matching), the
	// provider request as sent, and the walk params. Armed for EVERY
	// non-reserved launch (a reserved slot is pinned hardware — there is no
	// replacement inventory to re-match): the stall watchdog remains gated on
	// BootRescueCapability inside bootStallDetectorFor, but the
	// upstream-terminal failover in progressStatus needs the rescue state on
	// every provider — any upstream can accept a create and then kill the VM
	// before it runs (live 2026-09: a VM supplier ERROR'd ~25s post-create).
	// In-memory only: a restart mid-boot falls back to the durable reaper.
	var rescue *bootRescue
	if e.bootRescueAttempts > 0 && req.Tier != provider.TierReserved {
		rescue = &bootRescue{
			req:          req,
			provReq:      provReq,
			walk:         walkParams,
			attemptsLeft: e.bootRescueAttempts,
		}
		// Customer image: the bootstrap was rendered ONCE against the placed
		// provider's deployment class, and the rescue reuses that render — so
		// pin the rescue's candidate matching to the same class. Without this,
		// a stalled container-native host whose class has drained from
		// availability would rescue onto VM capacity carrying the
		// container-mode script: sshd + tunnel come up, the workload never
		// runs, and the instance publishes healthy and bills (review 759).
		if req.CustomerImage {
			rescue.req.LockedDeploymentClass = provider.DeploymentType(prov.Name())
		}
	}

	// 9. Kick off async status progression: creating -> provisioning -> (poll) -> booting.
	// Size the poll budget from the provider's create→ready estimate so a slow
	// boot class (bare-metal allocation ~13 min) is driven to running instead of being
	// reaped at the flat default.
	budget := e.launchBudget(req, provResult.EstimatedReadySeconds)
	if req.FleetProbe {
		e.fleetProbeLaunches.Store(instanceID, struct{}{})
	}
	go e.progressStatus(instanceID, budget, rescue)

	e.logger.Info("instance provisioned",
		slog.String("instance_id", instanceID),
		slog.String("upstream_id", provResult.UpstreamID),
		slog.String("provider", prov.Name()),
	)

	// PricePerHour is the RETAIL rate (same value persisted on the instance
	// row and billing session) — the response is customer-facing; the
	// wholesale offering price must never leave the engine.
	resp = &ProvisionResponse{
		InstanceID:         instanceID,
		Hostname:           hostname,
		PricePerHour:       retailPricePerHour,
		Status:             StateCreating,
		ProvisioningBudget: budget,
	}
	// Surface the per-instance credential + derived URLs. The create-FSM
	// (runCreateInstance) puts these on the operation result. app_url is
	// template-deploys-only; terminal_url whenever the console was
	// provisioned. Never logged.
	if req.AppPort != 0 || webTerminalOn {
		resp.AppBasicAuthUser = appUser
		resp.AppBasicAuthPass = appPass
	}
	if e.config.AppsHost != "" {
		if req.AppPort != 0 {
			resp.AppURL = "https://" + instanceID + "." + e.config.AppsHost
		}
		if webTerminalOn {
			resp.TerminalURL = "https://" + instanceID + "-term." + e.config.AppsHost
		}
	}
	return resp, nil
}

// Terminate destroys a running instance.
//
// Steps:
//  1. Get instance from DB
//  2. Check state machine (idempotent if already terminated)
//  3. Update status to stopping
//  4. Call provider to terminate upstream instance
//  5. Mark as terminated in DB
//  6. FRP cleanup is automatic (frpc dies with instance)
func (e *Engine) Terminate(ctx context.Context, instanceID string) error {
	// 1. Get instance from DB.
	inst, err := e.db.GetInstance(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("terminate: get instance: %w", err)
	}

	// 2. If already in terminal state, return nil (idempotent).
	if inst.Status == StateTerminated {
		return nil
	}

	// 3. Transition to stopping using optimistic locking — but not for an
	// `error` row; see shouldTransitionToStopping for why (SQLSTATE 23505 on the
	// v7 port index, which aborted 187 prod terminates).
	//
	// One behaviour change, deliberate: a terminate retried on a row already in
	// `error` that fails upstream again no longer re-emits instance.failed,
	// because the row never leaves `error` for SetInstanceError to flip. The
	// first failure already emitted, and event consumers dedupe on
	// (event, id, status), so the re-emit was noise.
	if shouldTransitionToStopping(inst.Status) {
		updated, err := e.db.UpdateInstanceStatus(ctx, instanceID, inst.Status, StateStopping)
		if err != nil {
			return fmt.Errorf("terminate: update status to stopping: %w", err)
		}
		if !updated {
			// Status changed concurrently. Re-read and check.
			inst, err = e.db.GetInstance(ctx, instanceID)
			if err != nil {
				return fmt.Errorf("terminate: re-read instance: %w", err)
			}
			if inst.Status == StateTerminated {
				return nil
			}
		}
	}

	// 4. Look up provider and terminate upstream.
	prov, ok := e.registry.Get(inst.UpstreamProvider)
	if !ok {
		e.logger.Warn("provider not found for termination, marking as terminated",
			slog.String("instance_id", instanceID),
			slog.String("provider", inst.UpstreamProvider),
		)
	} else {
		if err := prov.Terminate(ctx, inst.UpstreamID, inst.Region); err != nil {
			// If the pod no longer exists upstream, treat as already terminated.
			if strings.Contains(err.Error(), "not found") {
				e.logger.Warn("pod already gone upstream, proceeding with local cleanup",
					slog.String("instance_id", instanceID),
					slog.String("provider", inst.UpstreamProvider),
					slog.String("error", err.Error()),
				)
			} else {
				// Real provider error — set error state AND close billing.
				// The stored reason is CUSTOMER-VISIBLE (surfaced as
				// status_reason on the public API and error_reason on the
				// dashboard), so it must never embed the raw adapter error —
				// upstream error bodies routinely name the provider (INFPRIV).
				// The raw error goes to the log line below instead.
				e.logger.Error("provider terminate failed",
					slog.String("instance_id", instanceID),
					slog.String("error", err.Error()),
				)
				reason := "terminate failed: upstream teardown could not be confirmed; our reconciler will retry"
				errFlipped, setErr := e.db.SetInstanceError(ctx, instanceID, reason)
				if setErr != nil {
					e.logger.Error("failed to set error state after terminate failure",
						slog.String("instance_id", instanceID),
						slog.String("error", setErr.Error()),
					)
				}
				// instance.failed webhook (GPU-45), gated on the guarded flip
				// so a RETRIED terminate that keeps failing upstream while the
				// row already sits in error does not re-emit. (A terminate
				// retried after leaving error — error→stopping→error — is a
				// genuine re-entry and emits again; receivers dedupe on
				// (event, id, status).)
				if errFlipped {
					e.emitInstanceFailed(ctx, inst, reason)
				}
				return fmt.Errorf("terminate: provider %s: %w", inst.UpstreamProvider, err)
			}
		}

		// Self-healing verification — confirm the upstream call actually
		// terminated the instance. Catches the regional-routing class of
		// bug (Pitfall 10): Terminate hits a backend that doesn't own the
		// machine_id, returns a no-op success, and the instance keeps
		// billing. Poll GetStatus a few times; if still alive on the last
		// check, fire one more Terminate. If verification ultimately fails
		// we still mark the row terminated locally — the reconciler
		// (internal/reconcile) is the safety net for stuck-alive cases.
		e.verifyUpstreamTerminated(ctx, prov, inst.UpstreamID, inst.Region, instanceID, inst.UpstreamProvider)
	}

	// 5. Mark as terminated in DB (sets terminated_at, billing_end).
	terminatedNow, err := e.db.TerminateInstance(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("terminate: update DB: %w", err)
	}
	// instance.terminated webhook (GPU-45): emit only when THIS call performed
	// the flip (TerminateInstance is guarded on status NOT IN ('terminated')),
	// so a concurrent/repeated terminate cannot double-emit.
	if terminatedNow {
		e.emitInstanceTerminated(ctx, inst)
	}

	// Log terminated event to instance_events table.
	metadata, _ := json.Marshal(map[string]string{
		"gpu_type": inst.GPUType,
		"region":   inst.Region,
		"tier":     inst.Tier,
	})
	terminatedEvent := &db.InstanceEvent{
		InstanceID: instanceID,
		OrgID:      inst.OrgID,
		EventType:  "terminated",
		Metadata:   metadata,
	}
	if err := e.db.CreateInstanceEvent(ctx, terminatedEvent); err != nil {
		e.logger.Error("failed to log terminated event",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		// Non-fatal: instance is terminated, event logging failure doesn't block.
	}

	// 6. FRP cleanup is automatic: when the provider terminates the instance,
	// frpc dies, and frps drops the connection. The port is freed by the DB
	// status change to 'terminated' (the partial unique index only covers
	// non-terminated rows).

	e.logger.Info("instance terminated",
		slog.String("instance_id", instanceID),
	)

	return nil
}

// verifyUpstreamTerminated polls GetStatus to confirm the upstream call
// actually killed the instance, and re-issues Terminate once if not.
//
// Why: Pitfall 10-class bugs (Terminate hits a regional backend that
// doesn't own the machine_id, returns a no-op success) silently leak
// upstream resources while the user's billing keeps running. After
// Terminate returns nil we cannot trust that alone — verify with a
// status read.
//
// Bounded best-effort: at most ~6s of polling and one retry. We never
// fail the parent Terminate call from here — if the instance is still
// alive on the final check, the reconciler (internal/reconcile) is
// responsible for cleaning it up. Logged loudly for ops visibility.
func (e *Engine) verifyUpstreamTerminated(ctx context.Context, prov provider.Provider, upstreamID, region, instanceID, providerName string) {
	const (
		verifyAttempts = 3
		verifyDelay    = 2 * time.Second
	)
	for attempt := 0; attempt < verifyAttempts; attempt++ {
		// Use a fresh deadline for each verify call so a parent
		// context with a tight deadline doesn't make the first one fail.
		select {
		case <-ctx.Done():
			return
		case <-time.After(verifyDelay):
		}

		status, err := prov.GetStatus(ctx, upstreamID, region)
		if err != nil {
			// Common: 404 / "not found" → upstream gone, success.
			if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "HTTP 404") {
				return
			}
			// Other errors are noisy but don't necessarily indicate the instance
			// is alive. Log and try again.
			e.logger.Warn("verify-terminated: GetStatus failed",
				slog.String("instance_id", instanceID),
				slog.String("provider", providerName),
				slog.Int("attempt", attempt+1),
				slog.String("error", err.Error()),
			)
			continue
		}
		if status == nil {
			return
		}
		switch status.Status {
		case "terminated", "stopped", "error":
			return
		}

		// Still alive — re-issue Terminate once and continue polling.
		if attempt == verifyAttempts-2 {
			e.logger.Warn("verify-terminated: instance still alive after Terminate, retrying",
				slog.String("instance_id", instanceID),
				slog.String("upstream_id", upstreamID),
				slog.String("provider", providerName),
				slog.String("upstream_status", status.Status),
			)
			if retryErr := prov.Terminate(ctx, upstreamID, region); retryErr != nil &&
				!strings.Contains(retryErr.Error(), "not found") {
				e.logger.Error("verify-terminated: retry Terminate failed",
					slog.String("instance_id", instanceID),
					slog.String("provider", providerName),
					slog.String("error", retryErr.Error()),
				)
			}
		}
	}

	// If we reach here, the upstream still appears alive. Surface a loud log
	// so the reconciler (and ops) catches it.
	e.logger.Error("verify-terminated: upstream still alive after retries — handed off to reconciler",
		slog.String("instance_id", instanceID),
		slog.String("upstream_id", upstreamID),
		slog.String("provider", providerName),
	)
}

// SelectProviderCandidates returns all matching providers sorted by price ascending.
// Used by selectProvider (returns first) and by Provision (iterates for fallback retry).
func (e *Engine) SelectProviderCandidates(ctx context.Context, req ProvisionRequest) ([]ProviderCandidate, error) {
	return e.selectProviderCandidates(ctx, req, nil)
}

// selectProviderCandidates is SelectProviderCandidates, also filling trace
// (when non-nil) with what it consulted, for pinVanished.
func (e *Engine) selectProviderCandidates(ctx context.Context, req ProvisionRequest, trace *selectionTrace) ([]ProviderCandidate, error) {
	providers := e.registry.All()
	if len(providers) == 0 {
		return nil, ErrNoProvider
	}

	var candidates []ProviderCandidate

	// Custom-image gate (TMPL-05 / D2): a template deploy ships a custom Docker
	// image (req.Image != ""). Providers that ignore the image (VM OS images,
	// fixed provider templates) would silently boot the wrong image
	// (T-31-05a Elevation), so they are rejected up front. The eligibility
	// filter cannot do this — it runs on the provider-anonymized availability
	// set — so the gate lives HERE, where provider identity is known. Raw
	// instances (req.Image == "") are unaffected.
	requireCustomImage := req.Image != ""

	// Exact-offering pin gate (D-63-08): when the request names
	// a specific offering (req.SelectedOfferingID), placement must return ONLY the
	// offering whose deterministic id matches — never fall through to the
	// price-sorted set below and silently hand the user a different (often
	// pricier) machine. Mirrors requireCustomImage: a set field NARROWS the
	// candidate set. If the pinned offering has drained between catalog read and
	// launch, we return an explicit typed error (ErrOfferingUnavailable) BEFORE
	// the price sort, so no substitute can win. The error carries no provider
	// name, upstream body, or digest (T-63-04).
	requireExactOffering := req.SelectedOfferingID != ""

	// Raw-VM OS gate (the launch OS picker): a raw_vm:<os> launch may only land
	// on providers the server-controlled vm_images catalog maps that os id to —
	// a provider without a VERIFIED native slug would boot its default OS, which
	// is exactly the silent-substitution this grammar exists to prevent. Like
	// requireCustomImage this is candidate ELIGIBILITY only (narrowing, provider
	// identity known HERE); a nil manifest keeps the gate closed rather than
	// open — we can never prove support without the catalog.
	requireOSID := ""
	if class, sub := ParseEnvironmentSelection(req.Environment); class == "raw_vm" && sub != "" {
		requireOSID = sub
	}

	// Certified-environment gate: a certified launch (bare or with a framework
	// sub-choice) may only land on providers that can run the GPU.ai Certified
	// Image — provider.CertifiedImageCapable, the SAME per-provider attribute the
	// catalog stamps as certified_image. This matters most for UNPINNED placement
	// (offering pin dropped in favor of max_price capping): without it, the
	// cheapest candidate could be a non-certified provider that then hard-fails in
	// resolveEnvironment (ErrCertifiedUnavailable) instead of the launch simply
	// skipping it for the next certified-capable candidate. Eligibility only —
	// narrowing, provider identity known here; mirrors requireCustomImage.
	requireCertified := false
	if class, _ := ParseEnvironmentSelection(req.Environment); class == "certified" {
		requireCertified = true
	}

	// Instance-disk eligibility. This fires ONLY when the customer explicitly
	// set disk_gb — never on the server default, which is threaded as a plain
	// hint that capable adapters honor and the rest ignore. That scoping is the
	// single most important safety property of the disk feature: every launch
	// that omits the field produces a byte-identical candidate set to before it
	// existed, so fixed-disk providers keep serving all their current traffic.
	// TestSelectProviderCandidates_NoDiskRequestDoesNotNarrow pins it.
	//
	// Like requireCustomImage this is eligibility only — accepting a request we
	// cannot honor would boot the customer a silently different disk than they
	// asked for and were quoted.
	requireDiskGB := req.RequestedDiskGB != nil

	// The disk size the launch will actually provision (request/default/template
	// floor resolved once — the same value the sort, the quote, and the adapter
	// payload consume). For a PINNED explicit-disk launch it is also the
	// per-offering cap check's subject (see the loop below); droppedByDiskCap
	// distinguishes "your pin cannot fit this size" from "the pin drained" so
	// the empty-set error names the actual problem.
	effectiveDisk := e.effectiveDiskGB(req)
	droppedByDiskCap := 0
	// droppedByEntrypoint counts OFFERINGS (post type/tier/region/pin
	// matching) dropped by the customer-image ENTRYPOINT steer, so the
	// emptied-set branch can name the real reason instead of the misleading
	// "no longer available"/"no capacity" defaults.
	droppedByEntrypoint := 0

	// TMPL-04 auto-select: a template deploy that omits gpu_type picks the
	// cheapest GPU meeting the template's VRAM floor (req.MinVRAMGB) instead of
	// matching an exact gpu_type. The price sort below makes the cheapest
	// eligible offering candidates[0], and its GPUType becomes the selection
	// (Provision forwards it per attempt). The GPU-count floor rides on the
	// existing GPUCount filter (the handler floors req.GPUCount to the
	// template's min_gpu_count). Raw + explicit-type deploys keep the exact
	// gpu_type match unchanged.
	autoSelect := req.autoSelectsGPU()

	// GPU-87: the explicit-gpu_type counterpart of the auto-select floor above.
	// A template deploy that NAMES a GPU smaller than the template's own
	// min_vram_gb can never boot, so refuse it here rather than provisioning it
	// and letting it die 1-2 minutes later.
	//
	// Pre-loop and TYPE-level, both deliberate. Pre-loop, because the check must
	// fire even when zero offerings of that type are listed — otherwise the loop
	// falls through to ErrNoProvider and tells the customer to retry later,
	// advice that can never work. TYPE-level via TemplateVRAMShortfall, because
	// a per-offering filter here could disagree with the handler gate whenever
	// an upstream over-reports VRAM or ships a smaller variant under a shared
	// name; the type's nameplate is what the customer was shown and what both
	// layers must judge.
	//
	// The reserved-tier branch bypasses SelectProviderCandidates entirely and is
	// out of scope for this check.
	if !autoSelect && req.TemplateID != nil {
		if verr := TemplateVRAMShortfall(*req.TemplateID, req.MinVRAMGB, req.GPUType); verr != nil {
			return nil, verr
		}
	}

	for _, prov := range providers {
		// Staff dev-tools provider pin: when set, ONLY the named provider is
		// a candidate — narrowing-only, like the offering pin. No capacity on
		// the pinned provider means an explicit provision failure, never a
		// silent fallback to a different provider. Placed BEFORE the
		// kill-switch check purely as a fast skip; the PROVIDERS_DISABLED
		// check below still applies to the pinned provider itself, so the
		// kill-switch outranks the pin (staff cannot pin a disabled provider).
		if req.PinnedProvider != "" && !strings.EqualFold(prov.Name(), req.PinnedProvider) {
			continue
		}
		// Rescue-failover exclusion (see the ExcludeProviders doc): the
		// provider that just killed this launch upstream is not a candidate
		// for its own replacement. Narrowing-only, like the pin above.
		if providerExcluded(req.ExcludeProviders, prov.Name()) {
			e.logger.Debug("skipping provider: excluded by rescue failover",
				slog.String("provider", prov.Name()),
			)
			continue
		}
		// Sitewide kill-switch (PROVIDERS_DISABLED): a disabled supplier is
		// never a provisioning candidate. Its adapter stays registered so the
		// terminate / health / reconcile paths can still manage instances
		// already running on it — it just can't take new ones.
		if e.providerDisabled(ctx, prov.Name()) {
			e.logger.Debug("skipping provider: disabled via PROVIDERS_DISABLED",
				slog.String("provider", prov.Name()),
			)
			continue
		}
		// Account-unfunded suspension (availability.FundingGate): our account
		// at this supplier recently refused a create for lack of money, or
		// the balance poller reads its prepaid pot at <= 0. Every launch
		// there would be refused identically, so the provider is not a
		// candidate until the gate's cooldown lapses (the next launch after
		// that is the re-probe). A staff provider pin bypasses it, like the
		// launch-outcome breaker — the pin is how an operator checks a
		// top-up landed without waiting out the cooldown.
		if req.PinnedProvider == "" && e.fundingGate.Suspended(prov.Name()) {
			e.logger.Debug("skipping provider: supplier account unfunded",
				slog.String("provider", prov.Name()),
			)
			continue
		}
		// Availability-only capability gate (Phase 58 CR-01): an adapter that
		// declares itself non-provisionable (communitycloud pre-Phase-59 — its
		// Provision is a deterministic ErrNotImplemented stub) surfaces catalog
		// rows via the availability poller but must never enter the candidate
		// set, where each of its machines would burn one of the bounded
		// fallback attempts (≥3 cheap community machines would exhaust all 3
		// and hard-fail provisions despite same-SKU datacenter stock). Like
		// PROVIDERS_DISABLED above and the custom-image gate below, this is
		// candidate ELIGIBILITY only — the ranking/scoring/ordering of the
		// surviving candidates is untouched.
		if pc, ok := prov.(provider.ProvisionCapability); ok && !pc.Provisionable() {
			e.logger.Debug("skipping provider: availability-only (not provisionable yet)",
				slog.String("provider", prov.Name()),
			)
			continue
		}
		// Spot-eligibility gate: a TierSpot request may only land on providers
		// that opt into provider.SpotCapability AND currently report
		// SupportsSpot() — spotrouter.Eligible is the single source of truth.
		// Like the two gates above, this is candidate ELIGIBILITY only; the
		// surviving spot candidates flow through the same retail-price sort
		// below, which is exactly the cheapest-spot ranking (and the bounded
		// fallback loop then gives next-cheapest failover for free). On-demand
		// and reserved requests never enter this branch.
		if req.Tier == provider.TierSpot && !spotrouter.Eligible(prov) {
			e.logger.Debug("skipping provider: not spot-capable for tier=spot request",
				slog.String("provider", prov.Name()),
			)
			continue
		}
		// Template-deploy image gate. Scoped to !req.CustomerImage: template
		// images MUST land on a provider that honors the image natively, but a
		// CUSTOMER image has its own two-path gate right below (container
		// native OR the Phase-3 bootstrap docker-run on VM capacity) — left
		// unscoped, this gate silently dropped every VM provider from
		// customer-image placement, so a pinned VM offering always failed
		// "no longer available" (caught live in the Phase-3 demo E2E).
		if requireCustomImage && !req.CustomerImage && !SupportsCustomImage(prov.Name()) {
			e.logger.Debug("skipping provider: no custom-image support for template deploy",
				slog.String("provider", prov.Name()),
			)
			continue
		}
		// Customer-supplied image (Phases 1+3): container-native providers run
		// the image natively (SupportsCustomerImage); certified-capable VM
		// providers docker-run it from the bootstrap (CustomerImageVMCapable)
		// — but ONLY for public images: a private-registry login would have to
		// ride the staged bootstrap script to reach a VM's docker login, and
		// customer credentials must never persist there, so RegistryAuth
		// narrows back to container-native. Fixed-template providers fail both
		// predicates.
		if req.CustomerImage && !SupportsCustomerImage(prov.Name()) {
			if !CustomerImageVMCapable(prov.Name()) || req.RegistryAuth != nil {
				e.logger.Debug("skipping provider: no customer-image support",
					slog.String("provider", prov.Name()),
				)
				continue
			}
		}
		if requireCertified && !provider.CertifiedImageCapable(prov.Name()) {
			e.logger.Debug("skipping provider: not certified-image capable for certified launch",
				slog.String("provider", prov.Name()),
			)
			continue
		}
		if requireDiskGB && !provider.ConfigurableDisk(prov.Name()) {
			// Logged at Info, not Debug, and with the requested size: this is
			// the signal for tuning LAUNCH_MAX_DISK_GB and for deciding which
			// adapter to teach disk next. Without the size in the log the only
			// evidence of a too-tight cap is a customer complaint.
			e.logger.Info("skipping provider: cannot configure instance disk for explicit disk request",
				slog.String("provider", prov.Name()),
				slog.Int("requested_disk_gb", *req.RequestedDiskGB),
			)
			continue
		}
		if requireOSID != "" {
			slug := ""
			if e.manifest != nil {
				slug, _ = e.manifest.VMImageSlug(prov.Name(), requireOSID)
			}
			if slug == "" {
				e.logger.Debug("skipping provider: no verified image slug for requested raw-VM OS",
					slog.String("provider", prov.Name()),
					slog.String("os_id", requireOSID),
				)
				continue
			}
		}
		offerings, err := prov.ListAvailable(ctx)
		if err != nil {
			e.logger.Warn("provider availability check failed",
				slog.String("provider", prov.Name()),
				slog.String("error", err.Error()),
			)
			continue
		}
		// Every gate above passed and the listing succeeded, so a pin on
		// this provider that is missing from offerings really is gone.
		if trace != nil {
			trace.listed[prov.Name()] = true
		}

		for _, o := range offerings {
			if autoSelect {
				// Eligibility by VRAM floor; gpu_type is whatever the cheapest
				// survivor turns out to be.
				//
				// Normalized, because MinVRAMGB comes from template/base-model
				// floors written in nameplate terms ("min_vram_gb": 24) while a
				// raw offering carries the upstream's measured figure (an L4
				// reports 22). Filtering raw dropped every L4 from a 24GB
				// template — a "no capacity" for hardware the catalog says
				// qualifies.
				if provider.NormalizeVRAM(o.GPUType, o.VRAMPerGPUGB) < req.MinVRAMGB {
					continue
				}
			} else if o.GPUType != req.GPUType {
				continue
			}
			if o.GPUCount < req.GPUCount {
				continue
			}
			if o.Tier != req.Tier {
				continue
			}
			// REGION-03: provider adapters emit NATIVE region labels
			// ("del", "Delhi", "yto"). The user submits CANONICAL
			// codes ("india-south") sourced from the availability cache. Normalize
			// the offering's region through the same map the cache uses so both
			// sides speak canonical for the membership check. The native region
			// rides on through Offering.Region into the provision call.
			if len(req.Regions) > 0 {
				canonical := availability.Normalize(o.Region, o.DatacenterLocation, prov.Name())
				matched := false
				for _, r := range req.Regions {
					if canonical == r {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			if o.AvailableCount <= 0 {
				continue
			}
			// Launch-outcome breaker (availability.LaunchKey docs): an
			// offering whose (provider, gpu_type) recently kept dying at
			// boot is skipped even though the provider still advertises
			// stock — advertised availability proved to be a claim, not a
			// fact (GPU-108: a VM supplier's stock endpoint listed A100s its
			// scheduler could not place). Allow, not State, on purpose:
			// after the cooldown it admits half-open probe launches, whose
			// outcome then heals or re-trips the circuit. A staff provider
			// pin bypasses the breaker — that path exists precisely to
			// test a provider the operator knows is suspect.
			if e.launchBreaker != nil && req.PinnedProvider == "" &&
				!e.launchBreaker.Allow(availability.LaunchKey(prov.Name(), string(o.GPUType), o.GPUCount)) {
				if trace != nil {
					trace.breakerSkipped[availability.LaunchKey(prov.Name(), string(o.GPUType), o.GPUCount)] = true
				}
				e.logger.Debug("skipping offering: launch-outcome breaker tripped",
					slog.String("provider", prov.Name()),
					slog.String("gpu_type", string(o.GPUType)),
					slog.Int("gpu_count", o.GPUCount),
				)
				continue
			}
			// Fleet prober verdict (internal/fleetprobe): a probe launched
			// this exact offering and it did not work as listed. In enforce
			// mode it is skipped here as well as hidden from the catalog, so
			// a pin taken from a stale catalog page cannot land on it. Probe
			// launches are exempt (a delisted offering only comes back by
			// passing a probe), and so is a staff provider pin.
			if e.offeringVerdicts != nil && e.enforceOfferingVerdicts && !req.FleetProbe && req.PinnedProvider == "" {
				canonicalRegion := availability.Normalize(o.Region, o.DatacenterLocation, prov.Name())
				if e.offeringVerdicts.Delisted(availability.OfferingID(o, canonicalRegion)) {
					e.logger.Debug("skipping offering: delisted by fleet probe",
						slog.String("provider", prov.Name()),
						slog.String("gpu_type", string(o.GPUType)),
					)
					continue
				}
			}
			// D-63-08 exact-offering pin: keep only the offering whose
			// deterministic id equals the pinned selection. The canonical region
			// is the SAME value the catalog hashed into the id it handed the
			// client (availability.OfferingID over the Normalize'd region), so the
			// comparison is stable across polls.
			if requireExactOffering {
				canonicalRegion := availability.Normalize(o.Region, o.DatacenterLocation, prov.Name())
				if availability.OfferingID(o, canonicalRegion) != req.SelectedOfferingID {
					continue
				}
			}
			// ENTRYPOINT steer (live finding 2026-09-02, GPU-101 retires it):
			// a provider that starts the bootstrap as ENTRYPOINT arguments
			// cannot host an image whose CONFIG defines one — the bootstrap
			// never runs unless the entrypoint happens to exec "$@", which
			// the config cannot tell us. Keyed on the pre-flight-captured
			// config entrypoint, never the request override (the provider
			// applies the config entrypoint at container start regardless of
			// the rendered launch script). Checked per OFFERING — after the
			// type/tier/region/pin filters, like the disk cap — so the
			// counter only fires for capacity this request would otherwise
			// have used; counting at the provider level misattributed every
			// unrelated empty-set failure (a drained non-steered pin, a
			// zero-capacity GPU type) to the ENTRYPOINT.
			if req.CustomerImage && len(req.ImageConfigEntrypoint) > 0 && BootstrapRidesImageEntrypoint(prov.Name()) {
				droppedByEntrypoint++
				// Debug, not Info: this fires once per matching offering, and
				// a popular GPU type can match dozens — the emptied-set
				// branch logs the Info-level summary when it matters.
				e.logger.Debug("skipping offering: image ENTRYPOINT incompatible with bootstrap injection",
					slog.String("provider", prov.Name()),
					slog.String("image_entrypoint_0", req.ImageConfigEntrypoint[0]),
				)
				continue
			}
			// PINNED-only disk cap (the 2026-08-11 marketplace clamp defect): an
			// explicitly requested disk that exceeds the storage the pinned
			// offering advertised would not fail upstream — the marketplace
			// adapter's upstream accepts the create, bills the requested size,
			// and silently clamps the container to the host's capacity. The
			// customer pinned THIS card and asked for more than the card said
			// the machine has, so refusing at the displayed figure is the honest
			// answer. Deliberately NOT applied to unpinned placement: a
			// marketplace offering row is a GROUP whose StorageGB is one
			// representative host's figure, and sibling hosts in the group can
			// carry far more disk — the adapter-level MinDiskGB predicate (live
			// per-host data) is the honor boundary there, and rejecting on the
			// representative's figure would refuse capacity that can honor the
			// request. StorageGB == 0 means the adapter reports no figure —
			// unknown is never grounds to reject, so the check fails OPEN.
			if requireDiskGB && requireExactOffering && o.StorageGB > 0 && effectiveDisk > o.StorageGB {
				droppedByDiskCap++
				e.logger.Info("skipping offering: requested disk exceeds reported host storage",
					slog.String("provider", prov.Name()),
					slog.Int("requested_disk_gb", effectiveDisk),
					slog.Int("storage_gb", o.StorageGB),
				)
				continue
			}
			candidates = append(candidates, ProviderCandidate{Prov: prov, Offering: o})
		}
	}

	if len(candidates) == 0 {
		// Size before existence: a pinned offering that was dropped by the disk
		// cap is NOT "no longer available" — it is right there and cannot fit
		// the request. ErrOfferingUnavailable would read as transient churn and
		// invite a retry that can only fail the same way; the disk-specific
		// error names the one thing the customer can actually change.
		// droppedByDiskCap can only be non-zero in pinned mode (the cap check
		// is gated on requireExactOffering).
		if requireDiskGB && droppedByDiskCap > 0 {
			e.logger.Info("no candidate offering can fit the explicitly requested disk",
				slog.Int("requested_disk_gb", effectiveDisk),
				slog.String("gpu_type", string(req.GPUType)),
				slog.Int("dropped_by_disk_cap", droppedByDiskCap),
			)
			return nil, ErrDiskExceedsOffering
		}
		// Same "size before existence" honesty as the disk cap: capacity
		// dropped by the ENTRYPOINT steer is not gone and not transient — a
		// retry fails identically — so name the real constraint. The counter
		// increments per OFFERING that survived type/tier/region/pin
		// matching, so non-zero here means this request's actual capacity
		// (the pinned offering itself, or every live match) was excluded for
		// the entrypoint — never that an unrelated drain or empty catalog
		// happened to coexist with a steered provider.
		if req.CustomerImage && droppedByEntrypoint > 0 {
			e.logger.Info("no candidates: remaining capacity cannot host an ENTRYPOINT-bearing customer image",
				slog.String("gpu_type", string(req.GPUType)),
				slog.Int("dropped_by_entrypoint", droppedByEntrypoint),
			)
			return nil, ErrImageEntrypointUnavailable
		}
		// A pinned launch that matched nothing means the exact offering drained
		// between catalog read and launch. Fail EXPLICITLY (D-63-08) — never let
		// the caller retry into the general placement path and get a silent
		// substitute. The message names no provider (T-63-04).
		if requireExactOffering {
			return nil, ErrOfferingUnavailable
		}
		// A raw_vm:<os> launch that matched nothing means no live capacity for
		// this request comes from a provider with a verified image for that OS.
		// Same honesty contract as the offering pin: explicit error, never a
		// silent boot of a different OS (T-63-04 — no provider named).
		if requireOSID != "" {
			return nil, ErrOSUnavailable
		}
		// An explicit disk_gb that matched nothing means every provider with
		// live capacity for this request has a fixed instance disk. Same honesty
		// contract: an explicit error, never a silent provision at the
		// provider's own size. Logged with the requested size; alongside
		// the per-provider skips above, this is the evidence for tuning
		// LAUNCH_MAX_DISK_GB or teaching another adapter to configure disk.
		if requireDiskGB {
			e.logger.Info("no candidates can configure instance disk for explicit disk request",
				slog.Int("requested_disk_gb", *req.RequestedDiskGB),
				slog.String("gpu_type", string(req.GPUType)),
				slog.Int("gpu_count", req.GPUCount),
			)
			return nil, ErrDiskUnavailable
		}
		return nil, ErrNoProvider
	}

	// Sort by RETAIL price ascending — the customer pays retail, so "cheapest
	// candidate" must mean cheapest after each provider's markup. With
	// per-provider markups, wholesale order and retail order can differ; the
	// retail order is also what the catalog shows, so the selection matches
	// what the user compared against. On equal retail price, registry
	// iteration order provides the implicit tiebreak (earlier-registered
	// provider = higher priority). Per CONTEXT.md: "higher-margin provider
	// wins (existing priority list handles this -- no new mechanism)".
	// sort.SliceStable preserves registry insertion order as the tiebreaker.
	//
	// The disk term rides along via retailWithDisk so "cheapest" means cheapest
	// ALL-IN, matching what the customer is quoted and billed. It is not merely
	// a constant added to every candidate: markup is per-provider, so a large
	// disk on differently-marked-up providers can genuinely reorder them. With
	// DISK_PRICE_PER_GB_HOUR at 0 the term is 0 and this ordering is identical
	// to before.
	//
	// Retail is computed ONCE per candidate up front rather than inside the
	// comparator. The markup now resolves through the admin Store, whose cache
	// can expire (or be invalidated by a panel edit) mid-sort — a comparator
	// that re-read it could return ri<rj and rj<ri for the same pair, which
	// breaks sort's strict weak ordering. Precomputing also drops the lookups
	// from O(n log n) to O(n).
	// Customer image (Phase 3): keep the candidate set HOMOGENEOUS by
	// deployment class. The bootstrap is rendered once against candidates[0]
	// (container mode = in-container launch script; vm mode = the docker-run
	// block), so a fail-over that crossed classes would ship the wrong render.
	// Container-native is preferred — the image runs natively, no nested
	// docker, faster boots; VM capacity serves only when no container-native
	// offering matched. A LOCKED class (the boot-stall rescue re-matching
	// against a script that already rendered) is a hard filter with NO
	// fallback: an empty result fails the rescue loudly rather than binding a
	// replacement that would run the wrong render.
	if req.CustomerImage {
		if req.LockedDeploymentClass != "" {
			var sameClass []ProviderCandidate
			for _, c := range candidates {
				if provider.DeploymentType(c.Prov.Name()) == req.LockedDeploymentClass {
					sameClass = append(sameClass, c)
				}
			}
			if len(sameClass) == 0 {
				return nil, ErrNoProvider
			}
			candidates = sameClass
		} else {
			var containerClass []ProviderCandidate
			for _, c := range candidates {
				if provider.DeploymentType(c.Prov.Name()) == "container" {
					containerClass = append(containerClass, c)
				}
			}
			if len(containerClass) > 0 {
				candidates = containerClass
			}
		}
	}

	retail := make([]float64, len(candidates))
	for i, c := range candidates {
		retail[i] = e.retailWithDisk(ctx, c.Offering.PricePerHour, c.Prov.Name(), effectiveDisk)
	}
	idx := make([]int, len(candidates))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return retail[idx[a]] < retail[idx[b]] })
	sorted := make([]ProviderCandidate, len(candidates))
	for i, j := range idx {
		sorted[i] = candidates[j]
	}
	candidates = sorted

	// Placement memory: an unpinned launch tries offerings that keep failing
	// to place (vanished or phantom stock) only after the ones that have not,
	// instead of re-picking the same cheapest dead listing first. Demoted,
	// never dropped: a held offering may have recovered, and when the others
	// refuse it is still worth an attempt the launch would otherwise lose.
	// Only within what the customer's price caps allow: the walk stops at the
	// first candidate over a cap (prices are ascending, so every later one is
	// over too), so a held offering moved behind one would never be tried.
	// (A cost-ceiling refusal on a fresh candidate does not end the walk
	// while a cheaper held one is left to try; see walkProvisionCandidates.)
	// Pins and staff provider pins are honored as asked.
	if e.placement != nil && !requireExactOffering && req.PinnedProvider == "" {
		withinCaps := 0
		for _, j := range idx {
			r := retail[j]
			if (req.MaxPricePerHour != nil && r > *req.MaxPricePerHour) || viewedPriceExceeded(r, req.ViewedPricePerHour) {
				break
			}
			withinCaps++
		}
		fresh := make([]ProviderCandidate, 0, len(candidates))
		var held []ProviderCandidate
		for _, c := range candidates[:withinCaps] {
			if e.placement.Hidden(candidateOfferingID(c)) {
				held = append(held, c)
			} else {
				fresh = append(fresh, c)
			}
		}
		candidates = append(append(fresh, held...), candidates[withinCaps:]...)
	}

	return candidates, nil
}

// RerouteSpot places a replacement spot instance for a preempted (warned or
// reclaimed) one. It is the re-route entry point the spot preemption watcher
// (internal/spotrouter) calls on the health cadence: it rebuilds an
// engine-level ProvisionRequest from the dying instance's row and runs it
// through the NORMAL Provision path, so the spot-eligibility gate, retail
// price sort, and bounded fallback in SelectProviderCandidates naturally land
// the customer on the current next-cheapest spot offering — no separate
// routing logic to drift.
//
// Deliberate shape of the rebuilt request:
//   - Tier is forced to TierSpot (a spot customer is rerouted within the spot
//     pool; upgrading to on-demand is a product decision, not a failover).
//   - Regions pins the CANONICAL region of the old instance (the row stores
//     the NATIVE upstream region; availability.Normalize maps it back), so a
//     reroute never silently moves the customer's workload to another
//     geography. Cross-region failover can be added later by widening this.
//   - SSHKeyIDs is left empty: Provision auto-includes the org/user
//     attach_on_provision keys, which is exactly the "give me my usual keys"
//     behavior a replacement instance needs.
//   - The old instance's user-facing constraints (GPUType, GPUCount) are
//     preserved verbatim; price is NOT pinned — spot prices move, and the
//     cheapest current offering is the product promise.
//
// The caller owns the old instance's teardown/billing closeout; RerouteSpot
// only creates the replacement. All of Provision's abuse/spend gates apply —
// a reroute blocked by a frozen org or spend cap fails like any other launch.
func (e *Engine) RerouteSpot(ctx context.Context, inst db.Instance) (*ProvisionResponse, error) {
	// GPU-92: the request below is rebuilt from the row's GPU shape only. A
	// custom-image launch (row.image) or a template deploy (row.template_id)
	// carries image/env/ports/entrypoint/registry state that the row does not
	// hold, so a reroute would boot a default-image replacement and present it
	// as the customer's instance — a silent environment swap. Refuse instead:
	// the watcher has already marked the reclaimed instance failed and closed
	// its billing session, which is the honest outcome. Launch-time gating
	// (ErrCustomImageSpotUnsupported) keeps new custom-image rows off spot;
	// this guard covers rows that predate it and templated spot deploys.
	hasImage := inst.Image != nil && *inst.Image != ""
	hasTemplate := inst.TemplateID != nil && *inst.TemplateID != ""
	if hasImage || hasTemplate {
		e.logger.Warn("spot reroute refused: instance carries a custom environment",
			slog.String("instance_id", inst.InstanceID),
			slog.Bool("custom_image", hasImage),
			slog.Bool("template", hasTemplate),
		)
		return nil, fmt.Errorf("reroute spot instance %s: %w", inst.InstanceID, ErrSpotRerouteCustomEnvironment)
	}
	req := ProvisionRequest{
		OrgID:    inst.OrgID,
		UserID:   inst.UserID,
		GPUType:  provider.GPUType(inst.GPUType),
		GPUCount: inst.GPUCount,
		Tier:     provider.TierSpot,
		Regions:  []string{availability.Normalize(inst.Region, "", inst.UpstreamProvider)},
		// The per-instance runtime bound follows the customer across the
		// reroute (GPU-60). Without this the platform itself would launch the
		// unbounded instance the bound exists to prevent: the customer asked
		// for a machine that stops on its own, a preemption they never saw
		// replaced it, and nothing would ever stop the replacement.
		//
		// Semantics: the replacement re-arms at its OWN SetInstanceRunning, so
		// the customer gets a fresh N hours on it rather than the remainder of
		// the original window. That is the deliberate reading of "N hours of
		// runtime" — a preempted instance's replacement is a new run, the
		// customer is not billed for the gap, and carrying a remainder would
		// mean a reroute late in the window hands back a machine that dies
		// minutes later. It also cannot be computed honestly here: the old
		// row's deadline is already gone by the time the watcher reroutes.
		AutoTerminateHours: inst.AutoTerminateHours,
	}
	e.logger.Info("rerouting preempted spot instance",
		slog.String("instance_id", inst.InstanceID),
		slog.String("from_provider", inst.UpstreamProvider),
		slog.String("gpu_type", inst.GPUType),
		slog.Int("gpu_count", inst.GPUCount),
		slog.String("region", req.Regions[0]),
	)
	resp, err := e.Provision(ctx, req)
	if err != nil {
		e.logger.Warn("spot reroute failed",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
		return nil, fmt.Errorf("reroute spot instance %s: %w", inst.InstanceID, err)
	}
	return resp, nil
}

// provisioningBudget sizes how long progressStatus polls a booting instance
// before giving up and reaping the upstream. It is derived from the provider's
// own create→ready estimate (ProvisionResult.EstimatedReadySeconds) with 2×
// headroom — a slow physical allocation (bare-metal allocation ~13 min) must never be
// reaped mid-build. It never drops below the engine default floor
// (e.provisionTimeout), and falls back to that floor when the provider reports
// no estimate.
func (e *Engine) provisioningBudget(estimatedReadySeconds int) time.Duration {
	if estimatedReadySeconds <= 0 {
		return e.provisionTimeout
	}
	budget := time.Duration(estimatedReadySeconds) * time.Second * 2
	if budget < e.provisionTimeout {
		return e.provisionTimeout
	}
	return budget
}

// FleetProbeBootAllowance is the time a fleet-probe launch's bootstrap spends
// on the on-host probe before its ready callback: download (curl, 3 retries,
// 30s each), the probe itself (capped at 180s, usually under 10s) and the
// report POST (same retries), 7.5 minutes at worst. A probe launch gets it on
// top of the provider's budget and the tunnel-stall threshold, so measuring an
// offering never makes it miss a deadline a customer's launch would meet.
const FleetProbeBootAllowance = 8 * time.Minute

// launchBudget is provisioningBudget for this launch, plus
// FleetProbeBootAllowance on a fleet-probe launch.
func (e *Engine) launchBudget(req ProvisionRequest, estimatedReadySeconds int) time.Duration {
	budget := e.provisioningBudget(estimatedReadySeconds)
	if req.FleetProbe {
		budget += FleetProbeBootAllowance
	}
	return budget
}

// IsBootTimeout reports whether an instance's error_reason is a time-based
// boot verdict: the poll loop's provisioning timeout, the durable reaper's, or
// a boot-stall verdict. Each means the launch did not reach running in time,
// where a slow host and a dead one look alike, unlike an upstream that
// reported the VM dead ("upstream instance failed", "provisioning failed").
// The prefixes are the customer-visible reasons and pinned by tests.
func IsBootTimeout(reason string) bool {
	return strings.HasPrefix(reason, "provisioning timeout") || strings.HasPrefix(reason, "provisioning stalled")
}

// progressStatus runs in a goroutine to drive the full provisioning lifecycle:
// creating -> provisioning -> (poll provider) -> booting.
// The ready callback from cloud-init handles the final booting -> running transition.
// budget caps how long it polls a booting instance before reaping the stuck
// upstream (sized per boot class by provisioningBudget — a baremetal physical
// allocation legitimately takes ~13 min, so a flat 10m would reap it mid-boot).
//
// rescue, when non-nil (every non-reserved launch with rescues enabled),
// serves two consumers: it arms the boot-stall watchdog (bootstall.go) for
// BootRescueCapability providers — a stall verdict terminates the stalled
// upstream early and relaunches on replacement capacity inside the SAME
// budget (the timeout timer is never reset) — and it powers the
// upstream-terminal failover (GPU-108) on EVERY provider: an upstream that
// reports terminated/error pre-running is relaunched on the next candidate
// with the failed provider excluded. nil — reserved tier or rescues
// disabled — keeps the historical poll behavior byte-for-byte.
func (e *Engine) progressStatus(instanceID string, budget time.Duration, rescue *bootRescue) {
	ctx := context.Background()

	// A fleet-probe launch never feeds the launch-outcome breaker. It
	// bypasses the breaker at placement (pinned to its provider), and the
	// breaker decides what customers are shown and placed on, so letting
	// probe outcomes trip or heal it would let shadow-mode measurement change
	// what customers see. Every breaker charge below goes through this local.
	launchBreaker := e.launchBreaker
	if _, probe := e.fleetProbeLaunches.LoadAndDelete(instanceID); probe {
		launchBreaker = nil
	}

	// Step 1: Transition creating -> provisioning (immediate, as before).
	updated, err := e.db.UpdateInstanceStatus(ctx, instanceID, StateCreating, StateProvisioning)
	if err != nil {
		e.logger.Error("failed to progress to provisioning",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		return
	}
	if !updated {
		e.logger.Warn("status already changed from creating",
			slog.String("instance_id", instanceID),
		)
		return
	}

	// Notify SSE subscribers of provisioning state.
	if e.onStatusChange != nil {
		e.onStatusChange(instanceID, StateProvisioning)
	}

	// Step 2: Look up instance to get upstream_provider and upstream_id.
	inst, err := e.db.GetInstance(ctx, instanceID)
	if err != nil {
		e.logger.Error("failed to get instance for status polling",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		return
	}

	// Step 3: Look up provider adapter from registry.
	prov, ok := e.registry.Get(inst.UpstreamProvider)
	if !ok {
		e.logger.Error("provider not found for status polling",
			slog.String("instance_id", instanceID),
			slog.String("provider", inst.UpstreamProvider),
		)
		return
	}

	// Step 4: Poll provider until running, terminal, or timeout.
	interval := e.pollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timeout := time.After(budget)

	// The same budget as an ABSOLUTE deadline, handed to the rescue walk's
	// ledger context so each rescue attempt records how much of the clock it
	// actually had. This is the measurement the table exists to take: the
	// rescue inherits the REMAINING budget and the timeout timer is never
	// reset, so a rescue handed a spent clock cannot succeed however good the
	// replacement capacity is — and today nothing anywhere records that.
	if rescue != nil {
		rescue.walk.ledger.deadline = time.Now().Add(budget)
	}

	// bootingStepDone latches the one-shot IP-persist/defer step so a raw
	// instance held in booting (waiting on the tunnel_up callback) doesn't
	// re-run it on every 5s tick.
	bootingStepDone := false

	// sawUpstreamRunning latches whether the PROVIDER'S status API ever said
	// "running" during this poll loop. It is the discriminator the timeout
	// branch uses to decide whether a timeout is the provider's fault or ours
	// — see the launch-breaker comment there. Scoped to this loop rather than
	// read from the DB at timeout, so that after a failover rebind it reflects
	// what THIS provider did, not a predecessor's.
	sawUpstreamRunning := false

	// Boot-stall watchdog (bootstall.go): nil unless this provider declares
	// the capability, the instance is non-template, and a stall leg is
	// enabled. Re-derived after a rescue because the replacement can land on
	// a non-capable provider.
	detector := e.bootStallDetectorFor(prov, inst, rescue)

	for {
		select {
		case <-timeout:
			e.logger.Error("provisioning timeout",
				slog.String("instance_id", instanceID),
			)
			// BLOCK-04 (SC-3): terminate the stuck upstream BEFORE marking error so it
			// stops costing money. Detached ctx (the poll ctx is context.Background but
			// give Terminate its own deadline). "not found" = already gone, not an error.
			// The reconciler (internal/reconcile) backstops a no-op/wrong-region terminate.
			e.terminateStuckUpstream(prov, inst, instanceID, "timeout: failed to terminate stuck upstream")

			// Launch-outcome breaker on the STALL path. The existing charge
			// below (case "terminated", "error") only fires when a provider
			// says out loud that the VM died. Some providers never say it:
			// Vultr has NO failure status at all — its support confirmed in
			// writing that "if a server is stuck or has failed installing we
			// see it as installing status too" — so a dead create sits in
			// creating until this timeout, and the SKU is never shed. Every
			// customer who picks it waits out the full budget and fails.
			//
			// Charge ONLY when the provider never reported running. That is
			// the honest boundary: the provider accepted a create, took the
			// capacity, and never produced a running box — the same evidence
			// of phantom stock the terminal-status charge acts on, arriving as
			// silence instead of an error.
			//
			// If it DID report running and we still timed out, the VM existed
			// and something on our side — cloud-init, the bootstrap, sshd, the
			// tunnel — failed to finish. Charging there would shed a healthy
			// SKU on our own bug, and would do it across every provider, so
			// the breaker must stay quiet on that path however tempting the
			// symmetry looks.
			//
			// Deliberately NOT a failover: the terminal-status path can
			// relaunch because no billing session exists for a VM that never
			// ran, but a stall can time out with a tunnel half-up and billing
			// semantics that are not obviously safe to duplicate. Shedding the
			// SKU protects the NEXT customer; rescuing this one is a separate
			// change with its own billing argument to make.
			if !sawUpstreamRunning && launchBreaker != nil {
				e.logger.Warn("charging launch breaker: provider never reported running before the provisioning timeout",
					slog.String("instance_id", instanceID),
					slog.String("provider", inst.UpstreamProvider),
					slog.String("gpu_type", inst.GPUType),
					slog.Duration("budget", budget),
				)
				launchBreaker.RecordFailure(
					availability.LaunchKey(inst.UpstreamProvider, inst.GPUType, inst.GPUCount),
					fmt.Errorf("upstream never reported running within %s", budget),
				)
			}

			timeoutReason := fmt.Sprintf("provisioning timeout: instance did not become SSH-reachable within %s", budget)
			if flipped, _ := e.db.SetInstanceError(ctx, instanceID, timeoutReason); flipped {
				// instance.failed webhook (GPU-45), gated on the guarded flip.
				e.emitInstanceFailed(ctx, inst, timeoutReason)
			}
			return
		case <-ticker.C:
			// Check if instance has been concurrently terminated.
			current, err := e.db.GetInstance(ctx, instanceID)
			if err != nil {
				e.logger.Warn("failed to check instance status during polling",
					slog.String("instance_id", instanceID),
					slog.String("error", err.Error()),
				)
				continue
			}
			if current.Status != StateProvisioning && current.Status != StateBooting {
				e.logger.Info("instance status changed during polling, exiting",
					slog.String("instance_id", instanceID),
					slog.String("status", current.Status),
				)
				return
			}

			// Poll upstream provider.
			status, err := prov.GetStatus(ctx, inst.UpstreamID, inst.Region)
			if err != nil {
				e.logger.Warn("status poll failed, will retry",
					slog.String("instance_id", instanceID),
					slog.String("error", err.Error()),
				)
				continue
			}

			e.logger.Debug("polled provider status",
				slog.String("instance_id", instanceID),
				slog.String("upstream_status", status.Status),
			)

			// Startup GPU compute check (enforce mode): the bootstrap refused to
			// publish this host and the ready handler recorded why. Release the
			// host and rescue elsewhere with it excluded, exactly like a stall.
			// Nothing has been billed: the session opens only at running. An
			// "environment:" reason is the image (the promised framework cannot
			// use the GPU), which no other host would change: no rescue, no host
			// exclusion, fail fast with an image-level reason.
			if taker, ok := e.db.(readinessFailureTaker); ok {
				if reason, terr := taker.TakeInstanceReadinessFailure(ctx, instanceID); terr != nil {
					e.logger.Warn("failed to read readiness verdict",
						slog.String("instance_id", instanceID),
						slog.String("error", terr.Error()),
					)
				} else if reason != "" {
					hostID := status.UpstreamHostID
					if hostID == "" && detector != nil {
						hostID = detector.lastHostID
					}
					verdict := verdictGPUCheckFailed
					if strings.HasPrefix(reason, "environment:") {
						verdict = verdictGPUCheckEnvironment
					}
					e.logger.Warn("startup GPU compute check failed; releasing host",
						slog.String("instance_id", instanceID),
						slog.String("provider", inst.UpstreamProvider),
						slog.String("upstream_host_id", hostID),
						slog.String("verdict", verdict),
						slog.String("reason", reason),
					)
					e.recordBootFailure(ctx, rescue, instanceID, inst, verdict, reason)
					if verdict == verdictGPUCheckFailed && rescue != nil && rescue.attemptsLeft > 0 {
						// Relax an exact-offering pin the way the upstream-
						// terminal branch does: the pinned offering is the
						// host that just failed, and with it excluded the
						// pin would match nothing, so a pinned launch could
						// never be rescued. The walk still holds the
						// replacement to the launch's GPU type, count and
						// region at or below the persisted retail price.
						rescue.req.SelectedOfferingID = ""
						outcome, newProv, newInst := e.attemptBootRescue(ctx, instanceID, inst, prov, rescue, hostID, verdictGPUCheckFailed)
						switch outcome {
						case rescueLaunched:
							prov, inst = newProv, newInst
							bootingStepDone = false
							sawUpstreamRunning = false
							detector = e.bootStallDetectorFor(prov, inst, rescue)
							continue
						case rescueAborted:
							return
						}
						failReason := stallErrorReason(verdictGPUCheckFailed, true)
						if flipped, _ := e.db.SetInstanceError(ctx, instanceID, failReason); flipped {
							e.emitInstanceFailed(ctx, inst, failReason)
						}
						return
					}
					e.terminateStuckUpstream(prov, inst, instanceID, "gpu check: failed to terminate upstream")
					failReason := stallErrorReason(verdict, verdict == verdictGPUCheckFailed && rescue != nil)
					if flipped, _ := e.db.SetInstanceError(ctx, instanceID, failReason); flipped {
						e.emitInstanceFailed(ctx, inst, failReason)
					}
					return
				}
			}

			// Boot-stall watchdog: judge progress BEFORE the status switch so a
			// stalled boot is rescued (or failed fast with a real reason)
			// instead of burning the rest of the budget. observe only ever
			// verdicts pre-running upstream states; terminal states fall
			// through to the existing handling below.
			if detector != nil {
				if verdict := detector.observe(time.Now(), status); verdict != "" {
					// Before the guard: a stalled boot is recorded whether or
					// not a rescue is available to follow it.
					e.recordBootFailure(ctx, rescue, instanceID, inst, verdict, "")
					e.noteStallVerdict(prov.Name(), detector.lastHostID, verdict, rescue != nil && rescue.req.CustomerImage)
					if rescue != nil && rescue.attemptsLeft > 0 {
						outcome, newProv, newInst := e.attemptBootRescue(ctx, instanceID, inst, prov, rescue, detector.lastHostID, verdict)
						switch outcome {
						case rescueLaunched:
							prov, inst = newProv, newInst
							bootingStepDone = false
							// The rebind points inst at a NEW provider, so the
							// predecessor's "it reported running" must not carry
							// across and suppress a charge the new one earns.
							sawUpstreamRunning = false
							detector = e.bootStallDetectorFor(prov, inst, rescue)
							continue
						case rescueAborted:
							return
						}
						// rescueFailed: the stalled upstream is already torn
						// down — fail the launch now with the stall reason.
						stallReason := stallErrorReason(verdict, true)
						if flipped, _ := e.db.SetInstanceError(ctx, instanceID, stallReason); flipped {
							e.emitInstanceFailed(ctx, inst, stallReason)
						}
						return
					}
					// Rescues exhausted (or disabled): fail fast rather than
					// letting a judged-dead boot burn the remaining budget.
					e.logger.Error("boot stalled with no rescue attempts remaining",
						slog.String("instance_id", instanceID),
						slog.String("verdict", verdict),
					)
					e.terminateStuckUpstream(prov, inst, instanceID, "boot-stall: failed to terminate stalled upstream")
					stallReason := stallErrorReason(verdict, rescue != nil)
					if flipped, _ := e.db.SetInstanceError(ctx, instanceID, stallReason); flipped {
						e.emitInstanceFailed(ctx, inst, stallReason)
					}
					return
				}
			}

			switch status.Status {
			case "running":
				// Latch before the transition guard: the provider said running,
				// which is the fact the timeout branch needs, whether or not
				// this tick is the one that flips us to booting.
				sawUpstreamRunning = true
				if current.Status == StateProvisioning {
					// Provider reports pod is running -> transition to booting.
					updated, err := e.db.UpdateInstanceStatus(ctx, instanceID, StateProvisioning, StateBooting)
					if err != nil {
						e.logger.Error("failed to transition to booting",
							slog.String("instance_id", instanceID),
							slog.String("error", err.Error()),
						)
						return
					}
					if updated {
						e.logger.Info("instance transitioned to booting",
							slog.String("instance_id", instanceID),
						)
						// Launch-outcome breaker: the upstream actually
						// booted, so this (provider, gpu_type) delivers real
						// capacity — heal the circuit. Recorded here (first
						// upstream-running) rather than at the ready
						// callback: the breaker scores whether the PROVIDER
						// delivers a VM, and a later tunnel/bootstrap failure
						// is our side of the fence.
						if launchBreaker != nil {
							launchBreaker.RecordSuccess(availability.LaunchKey(inst.UpstreamProvider, inst.GPUType, inst.GPUCount))
						}
						if e.onStatusChange != nil {
							e.onStatusChange(instanceID, StateBooting)
						}

						// The billing session is NOT opened here. Booting is still a
						// pre-usable state (no SSH tunnel / app yet), so the meter
						// must not run. The session opens only at the running
						// transition, owned by the ready callback
						// (api.handleInstanceReady) as the single session-open point.
					}
					// Continue polling to drive booting -> running.
					continue
				}

				if current.Status == StateBooting {
					// Wait until provider returns runtime networking info before marking running.
					if status.IP == "" {
						continue
					}
					// Booting->running is owned by the bootstrap's ready
					// callback for BOTH kinds: app_ready for a service
					// template (two-writer race, TMPL-10) and tunnel_up for a
					// raw instance (RAWREADY — "running" must imply the SSH
					// tunnel is registered, not just that the provider
					// attached an IP; a floating-IP race showed Running
					// with a permanently dead tunnel). The step below only
					// records the IP and logs the defer.
					// Direct connect repeats the step: it flips the row only
					// once the SSH endpoint answers, which can be minutes after
					// the supplier first attaches an address.
					if !bootingStepDone || e.directConnect {
						e.progressStatusStep(ctx, instanceID, *status)
						bootingStepDone = true
					}
					if current.TemplateID != nil {
						// Template: callback owns the flip; no poll-loop
						// timeout watchdog (app image pulls can legitimately
						// exceed it) — unchanged behavior.
						return
					}
					// Raw: keep polling so the provisioning timeout above
					// backstops a tunnel that never registers (frps down,
					// token mismatch, frpc download failure). The loop exits
					// via the status-changed branch once the callback flips
					// the row to running.
					continue
				}

			case "terminated", "error":
				e.logger.Error("upstream instance failed to start",
					slog.String("instance_id", instanceID),
					slog.String("upstream_status", status.Status),
				)
				// Launch-outcome breaker: the provider accepted the create
				// and the VM died before running — the strongest evidence
				// its advertised stock for this SKU is phantom. Recorded
				// before the failover below so the count accrues whether or
				// not a rescue lands (a rescued customer never sees the
				// failure, but the SKU is no less dead). After a failover
				// rebinds, inst names the NEW provider, so a chained
				// failure is charged to the circuit that actually failed.
				if launchBreaker != nil {
					launchBreaker.RecordFailure(
						availability.LaunchKey(inst.UpstreamProvider, inst.GPUType, inst.GPUCount),
						fmt.Errorf("upstream reported %s before running", status.Status),
					)
				}
				// Upstream-terminal failover (GPU-108): the provider accepted
				// the create and then killed the VM before it ever ran (live
				// 2026-09, a VM supplier: its stock endpoint advertised capacity the
				// scheduler didn't have, so creates died in ERROR ~25s in —
				// 55 failed launches across 14 orgs in a week). The instance
				// never reached running, so no billing session exists and a
				// relaunch is billing-safe by construction. The failed
				// provider is excluded from the re-match (its catalog still
				// advertises the phantom stock, so cheapest-first would
				// re-pick it immediately), and an exact-offering pin is
				// relaxed to the launch's constraints — same GPU type/count/
				// region at or below the persisted retail price, which the
				// walk enforces as a ceiling — because the pinned offering
				// just proved unfillable. A staff provider pin is honored:
				// no cross-provider failover behind a pin.
				// attemptBootRescue terminates the dead upstream FIRST
				// (BLOCK-04 ordering), so the pre-terminate below stays on
				// the no-rescue path only.
				e.recordBootFailure(ctx, rescue, instanceID, inst, verdictUpstreamTerminal, "")
				if rescue != nil && rescue.attemptsLeft > 0 && rescue.req.PinnedProvider == "" {
					rescue.req.SelectedOfferingID = ""
					rescue.req.ExcludeProviders = appendProviderExclusion(rescue.req.ExcludeProviders, prov.Name())
					outcome, newProv, newInst := e.attemptBootRescue(ctx, instanceID, inst, prov, rescue, "", verdictUpstreamTerminal)
					switch outcome {
					case rescueLaunched:
						prov, inst = newProv, newInst
						bootingStepDone = false
						// See the rebind above: the latch is per-binding, not per-loop,
						// or provider B inherits provider A's success.
						sawUpstreamRunning = false
						detector = e.bootStallDetectorFor(prov, inst, rescue)
						continue
					case rescueAborted:
						return
					}
					// rescueFailed: the dead upstream is already torn down —
					// fail the launch with the failover-tried reason.
					failReason := stallErrorReason(verdictUpstreamTerminal, true)
					if flipped, _ := e.db.SetInstanceError(ctx, instanceID, failReason); flipped {
						e.emitInstanceFailed(ctx, inst, failReason)
					}
					return
				}
				// BLOCK-04 (SC-3), second call site: the upstream reports terminal but
				// may still hold a billable shell — fire Terminate before marking error.
				// Idempotent; "not found" swallowed; reconciler backstops a no-op.
				e.terminateStuckUpstream(prov, inst, instanceID, "upstream-terminal: failed to terminate stuck upstream")
				upstreamReason := fmt.Sprintf("upstream instance failed: %s", status.Status)
				if flipped, _ := e.db.SetInstanceError(ctx, instanceID, upstreamReason); flipped {
					e.emitInstanceFailed(ctx, inst, upstreamReason)
				}
				return
			}
			// For "creating" or any other status, continue polling.
		}
	}
}

// progressStatusStep runs ONCE when the provider first reports running + a
// ready SSH IP during the booting phase. It used to be the poll-loop's WRITER
// #1 for the booting->running flip; it no longer flips ANY instance — the
// bootstrap's ready callback (handlers_internal.go) is the sole authority.
//
// Why (two gates, same shape):
//   - Service template (inst.TemplateID != nil): TMPL-10 / Pitfall 5 — the
//     SSH-IP signal is app-agnostic and would mark "running" while ComfyUI is
//     still loading, 502-ing the deploy URL for ~90s. The callback flips only
//     on app_ready=="true".
//   - Raw instance (TemplateID nil): RAWREADY — the provider attaching an IP
//     says nothing about the FRP tunnel. On some VM suppliers' GPU flavors the guest
//     can boot minutes before the floating IP, and pre-#55 bootstraps died
//     there forever: the dashboard said Running while `ssh -p <port>` was
//     connection-refused. The callback flips only on tunnel_up!="false",
//     which the bootstrap now derives from probing its own public SSH port
//     on the gateway.
//
// For a raw instance the step persists the provider-reported IP (ops
// visibility — SSH coords derive from frp_remote_port, never upstream_ip).
// For templates upstream_ip intentionally stays unset, as before.
func (e *Engine) progressStatusStep(ctx context.Context, instanceID string, status provider.InstanceStatus) {
	store := e.pollBackend()

	// Re-read the instance to learn whether it is a service template. The live
	// poll loop already holds a fresh row, but re-reading keeps this step
	// self-contained and honest against the DB seam (the unit test scripts
	// GetInstance). Cheap relative to the 5s poll cadence.
	inst, err := store.GetInstance(ctx, instanceID)
	if err != nil {
		e.logger.Error("failed to re-read instance for running transition",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		return
	}

	isTemplate := inst.TemplateID != nil
	if isTemplate {
		// TMPL-10 / Pitfall 5: defer to the app_ready callback. Do NOT flip to
		// running here. upstream_ip is intentionally not persisted (SSH coords
		// come from frp_remote_port, not upstream_ip).
		e.logger.Info("template instance: deferring booting->running to app_ready callback",
			slog.String("instance_id", instanceID),
		)
		return
	}

	// Direct-connect mode (no FRP tunnel configured): the engine rendered no
	// bootstrap, so nothing on the instance will ever call back. Readiness is
	// the supplier reporting running with an address AND that address answering
	// on SSH. Until it answers the instance stays booting and the next poll
	// tick checks again; the provisioning budget backstops one that never does.
	if e.directConnect {
		if e.sshReady != nil && !e.sshReady(ctx, status.IP, sshPortFromStatus(&status)) {
			e.logger.Debug("direct connect: waiting for SSH",
				slog.String("instance_id", instanceID),
				slog.String("upstream_ip", status.IP),
				slog.Int("ssh_port", sshPortFromStatus(&status)),
			)
			return
		}
		e.markDirectRunning(ctx, store, inst, status)
		return
	}

	// Raw instance: record the IP, defer the flip to the tunnel_up callback.
	if status.IP != "" && (inst.UpstreamIP == nil || *inst.UpstreamIP != status.IP) {
		if err := store.SetInstanceUpstreamIP(ctx, instanceID, status.IP); err != nil {
			e.logger.Error("failed to persist upstream IP",
				slog.String("instance_id", instanceID),
				slog.String("error", err.Error()),
			)
		}
	}
	e.logger.Info("raw instance: deferring booting->running to tunnel_up callback",
		slog.String("instance_id", instanceID),
		slog.String("upstream_ip", status.IP),
		slog.Int("ssh_port", sshPortFromStatus(&status)),
	)
}

// terminateStuckUpstream tears down the provider-side upstream for an instance
// that progressStatus is about to mark error (provisioning timeout or an
// upstream-terminal poll result). It is called BEFORE SetInstanceError on both
// failure paths so a stuck "creating" pod stops costing money (BLOCK-04 / SC-3).
//
// Detached context: progressStatus's poll ctx is context.Background(), but each
// Terminate gets its OWN 30s deadline via context.WithTimeout so a hung provider
// can't wedge the goroutine indefinitely (cancel is always called). It uses the
// instance's OWN persisted UpstreamID + Region (never a guessed id), so it can
// never tear down a different tenant's instance. A "not found" terminate error
// is swallowed — the pod is already gone, which is the desired end state, not an
// error worth logging. Any other terminate failure is logged with logLabel (which
// distinguishes the two call sites) and left for the reconciler
// (internal/reconcile, Plan 32-05) to backstop on the next tick.
func (e *Engine) terminateStuckUpstream(prov provider.Provider, inst *db.Instance, instanceID, logLabel string) {
	if prov == nil || inst == nil || inst.UpstreamID == "" {
		return
	}
	tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if termErr := prov.Terminate(tctx, inst.UpstreamID, inst.Region); termErr != nil &&
		!strings.Contains(termErr.Error(), "not found") {
		e.logger.Error(logLabel,
			slog.String("instance_id", instanceID),
			slog.String("error", termErr.Error()),
		)
	}
	cancel()
}

// reapStuckProvisionAfter is the conservative max age past which an instance
// still in a pre-ready state (creating/provisioning/booting) is treated as a
// failed provision and reaped. It MUST stay above the largest boot-class budget
// (provisioningBudget) so the durable reaper never preempts a legitimately slow
// boot — the slowest seen was a 20min estimate -> 40min budget;
// live-measured 2026-08-12: a healthy T4 Delhi launch was 19min allocating +
// still image-pulling when the old 30m backstop reaped it at minute 32), so
// 45m leaves headroom; raise this if a slower boot class is added.
// The live in-memory watchdog (progressStatus) reaps non-template instances at
// their exact per-instance budget; this flat backstop catches what no watchdog
// covers — template deploys (which get no watchdog) and any provision orphaned
// by a process restart (the watchdog is in-memory and dies with the process).
// AUDIT-05-001 (CRIT) / AUDIT-05-002 (HIGH).
const reapStuckProvisionAfter = 45 * time.Minute

// ReapStuckProvisions terminates and closes billing for every instance stuck in
// a pre-ready state past reapStuckProvisionAfter. It is the durable, DB-backed
// counterpart to the in-memory progressStatus watchdog: idempotent and safe to
// run every tick, it is the authority that stops a failed boot from billing
// forever when no live goroutine is watching it. Invoked by the BillingTicker.
func (e *Engine) ReapStuckProvisions(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-reapStuckProvisionAfter)
	stuck, err := e.db.ListStuckProvisions(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("list stuck provisions: %w", err)
	}
	for i := range stuck {
		inst := stuck[i]
		// Optimistic lock: only reap if we win the transition to error. A live
		// watchdog, a concurrent reap, or a user terminate that already moved the
		// row makes this a no-op — avoiding a redundant upstream Terminate. The
		// terminate + billing-close below are idempotent, so even a lost race
		// degrades to a harmless retry, never a double-charge.
		updated, uerr := e.db.UpdateInstanceStatus(ctx, inst.InstanceID, inst.Status, StateError)
		if uerr != nil {
			e.logger.Error("reaper: failed to mark stuck instance error",
				slog.String("instance_id", inst.InstanceID),
				slog.String("error", uerr.Error()))
			continue
		}
		if !updated {
			continue
		}
		e.logger.Warn("reaper: terminating stuck provision past boot budget",
			slog.String("instance_id", inst.InstanceID),
			slog.String("status", inst.Status),
			slog.String("provider", inst.UpstreamProvider))
		// Stop the money first: terminate the upstream, THEN mark error + close
		// billing (mirrors the progressStatus timeout path). A missing provider
		// adapter still proceeds to error + billing-close so the bill stops even
		// when we cannot reach the upstream (the reconciler backstops the leak).
		if prov, ok := e.registry.Get(inst.UpstreamProvider); ok {
			e.terminateStuckUpstream(prov, &inst, inst.InstanceID, "reaper: failed to terminate stuck upstream")
		}
		reapReason := fmt.Sprintf("provisioning timeout (reaper): stuck in %q for over %s", inst.Status, reapStuckProvisionAfter)
		_, _ = e.db.SetInstanceError(ctx, inst.InstanceID, reapReason)
		// instance.failed webhook (GPU-45), gated on the optimistic
		// UpdateInstanceStatus win above (updated==true on this path), so a
		// concurrent watchdog/terminate that already moved the row cannot
		// cause a double emission.
		e.emitInstanceFailed(ctx, &inst, reapReason)
		if e.onStatusChange != nil {
			e.onStatusChange(inst.InstanceID, StateError)
		}
	}
	return nil
}

// autoTerminateReason renders the customer-facing explanation stored on an
// instance the runtime-bound sweep stops. Same shape as the spend-cap stops'
// CustomerReason: it must read as a deliberate platform action the customer
// themselves configured, never as a failure. Contains no upstream identity
// (INFPRIV) — it names only the customer's own request.
func autoTerminateReason(hours int) string {
	return fmt.Sprintf(
		"Terminated: this instance reached the %d-hour runtime limit set at launch (auto_terminate_hours). Launch again without the limit, or with a longer one, to keep running.",
		hours,
	)
}

// autoTerminateSweepLimit caps how many instances one sweep terminates. 25 at
// ~2s of upstream-verify each is under a minute of tick time in the worst case,
// which keeps the sweep from pushing the balance deduction that follows it into
// the next tick. Deadlines are measured in hours, so deferring a remainder by
// 60s costs the customer nothing.
const autoTerminateSweepLimit = 25

// SweepAutoTerminate terminates every running instance whose per-instance
// runtime bound has elapsed (GPU-60). Called once per billing tick (60s) from
// the ticker's runTick, next to ReapStuckProvisions — the same leader-gated,
// durable cadence, chosen for the same reason: the failure this defends against
// is an unattended client that is no longer there, so enforcement has to live
// in a server loop that survives it.
//
// Exactly-once is the DB's job, not this loop's: ClaimInstanceAutoTerminate
// clears auto_terminate_at under the same predicate that made the row due, so a
// lost claim (a concurrent sweep, a user terminate that already moved the row)
// skips silently and no instance is ever terminated, billed-closed or audited
// twice.
//
// Termination goes through Engine.Terminate — the SAME path a user DELETE takes
// — deliberately: billing closeout, the instance.terminated webhook, the
// reservation-slot release and the upstream verify-and-retry all hang off that
// method, and a bespoke teardown here would silently miss them. That also makes
// the reserved tier safe to include: Terminate returns a reserved instance's
// slot to the pool, so a bounded reserved launch frees its capacity like any
// other terminate rather than needing a skip.
//
// Best-effort per instance: one failure is logged and the sweep continues, so a
// single unreachable provider cannot hold up everyone else's deadline. Returns
// an error only when the candidate read itself fails.
//
// Bounded to autoTerminateSweepLimit instances per call: this runs INSIDE the
// billing tick, ahead of balance deduction, and every terminate costs a couple
// of seconds of upstream verify. A large due set (a backlog after an outage, or
// a fleet launched at once on the same limit) would otherwise delay every org's
// deduction by seconds-per-instance. Oldest deadline first, remainder next tick.
func (e *Engine) SweepAutoTerminate(ctx context.Context) error {
	now := time.Now().UTC()
	due, err := e.db.ListInstancesDueForAutoTerminate(ctx, now, autoTerminateSweepLimit)
	if err != nil {
		return fmt.Errorf("list instances due for auto-terminate: %w", err)
	}
	if len(due) == autoTerminateSweepLimit {
		// The batch was full, so there is probably more behind it. Worth a log
		// line: a cap that is hit every tick for many ticks means deadlines are
		// being enforced minutes late, which is an ops signal, not a bug.
		e.logger.Warn("auto-terminate: sweep batch full; remainder deferred to the next tick",
			slog.Int("limit", autoTerminateSweepLimit))
	}
	for i := range due {
		inst := due[i]
		claimed, cerr := e.db.ClaimInstanceAutoTerminate(ctx, inst.InstanceID, now)
		if cerr != nil {
			e.logger.Error("auto-terminate: claim failed",
				slog.String("instance_id", inst.InstanceID),
				slog.String("error", cerr.Error()))
			continue
		}
		if !claimed {
			// Someone else got there first (another sweep, a user terminate,
			// a spend-cap stop). Nothing to do and nothing to report.
			continue
		}
		hours := 0
		if inst.AutoTerminateHours != nil {
			hours = *inst.AutoTerminateHours
		}
		// The deadline as it stood when this sweep claimed the row. Kept so a
		// failed terminate can put it back verbatim rather than pushing it
		// forward — the bound already elapsed, and now+hours would silently
		// extend a run the customer asked to have stopped. Non-nil for every
		// row the due query returns; the fallback keeps a hand-built row safe.
		deadline := now
		if inst.AutoTerminateAt != nil {
			deadline = *inst.AutoTerminateAt
		}
		e.logger.Info("auto-terminate: per-instance runtime limit reached",
			slog.String("instance_id", inst.InstanceID),
			slog.String("org_id", inst.OrgID),
			slog.Int("auto_terminate_hours", hours))

		// Record WHY before the teardown, so the row explains itself even if
		// the terminate fails halfway. recordStopReason is the same helper the
		// spend-cap stops use and is an idempotent overwrite, so it is safe to
		// repeat on a retried tick.
		reason := autoTerminateReason(hours)
		e.recordStopReason(ctx, inst.InstanceID, reason)

		terr := e.Terminate(ctx, inst.InstanceID)
		if terr != nil {
			// Loud but non-fatal for the sweep. Where Terminate got far enough
			// to touch the row it also stopped the money (its provider-error
			// branch flips the row to error and closes the billing session),
			// and the reconciler backstops a leaked upstream.
			//
			// But Terminate can also fail BEFORE changing anything — a
			// GetInstance blip, a lost status flip, a context cancelled by an
			// ECS rollout (the ticker runs on the serve context, with no
			// per-tick timeout). The claim is already spent, so without a
			// re-arm that row would sit there running, unbounded and never
			// retried: the platform quietly keeping alive the very instance
			// the customer asked to have stopped. Put the deadline back and let
			// the next tick try again.
			//
			// The re-arm is guarded on status = 'running' AND
			// auto_terminate_at IS NULL, so it is a no-op exactly when the
			// terminate DID land — exactly-once survives on the success path.
			e.logger.Error("auto-terminate: terminate failed",
				slog.String("instance_id", inst.InstanceID),
				slog.String("error", terr.Error()))
			if e.rearmAutoTerminate(inst.InstanceID, deadline) {
				// Nothing durable happened; the retry tick owns the audit row.
				continue
			}
		}

		// The 'auto_terminated' event is what distinguishes this from a
		// customer-initiated terminate, which the shared 'terminated' event
		// (emitted inside Terminate) cannot. It is written once the outcome is
		// known — after a landed terminate, or after a failed one that still
		// moved the row off running — and skipped only when the claim was
		// re-armed, so a retried tick cannot leave a duplicate audit row.
		e.emitAutoTerminatedEvent(ctx, &inst, hours)
	}
	return nil
}

// emitAutoTerminatedEvent writes the 'auto_terminated' instance_events row
// (schema v92) marking a termination as the platform enforcing the customer's
// own per-instance runtime bound.
//
// Customer-visible by design — unlike v87's admin-only 'rescued', the metadata
// carries only the instance's own request and the same gpu_type/region/tier
// trio the 'terminated' event already carries, so it flows through the
// org-scoped event read like every other lifecycle event.
//
// Best-effort: a failed audit write is logged and never changes the outcome
// (the same contract every other CreateInstanceEvent call site here follows).
func (e *Engine) emitAutoTerminatedEvent(ctx context.Context, inst *db.Instance, hours int) {
	metadata, _ := json.Marshal(map[string]any{
		"gpu_type":             inst.GPUType,
		"region":               inst.Region,
		"tier":                 inst.Tier,
		"auto_terminate_hours": hours,
	})
	if err := e.db.CreateInstanceEvent(ctx, &db.InstanceEvent{
		InstanceID: inst.InstanceID,
		OrgID:      inst.OrgID,
		EventType:  "auto_terminated",
		Metadata:   metadata,
	}); err != nil {
		e.logger.Error("auto-terminate: failed to log auto_terminated event",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()))
	}
}

// rearmAutoTerminate restores a spent claim after a terminate that left the row
// running, so the next tick retries it. Uses a detached context on purpose: the
// most likely reason the terminate failed is that the tick's context was
// cancelled mid-rollout, and re-arming under that same dead context would fail
// too — leaving exactly the unbounded row this is here to prevent.
//
// Best-effort: a failed re-arm is logged loudly (it is the one path that can
// silently strand an instance past its limit) but never fails the sweep.
//
// Reports whether the deadline was actually restored, i.e. the row was still
// running with no live deadline; false means the terminate moved the row (or
// the re-arm itself failed) and the sweep should treat this attempt as final.
func (e *Engine) rearmAutoTerminate(instanceID string, deadline time.Time) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rearmed, err := e.db.RearmInstanceAutoTerminate(ctx, instanceID, deadline)
	if err != nil {
		e.logger.Error("auto-terminate: FAILED to re-arm deadline after a failed terminate; "+
			"instance may run past its limit until an operator intervenes",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()))
		return false
	}
	if rearmed {
		e.logger.Warn("auto-terminate: deadline re-armed for retry on the next tick",
			slog.String("instance_id", instanceID))
	}
	return rearmed
}

// extractHost extracts the host portion from a URL string.
// e.g., "https://api.example.com" -> "api.example.com", "http://203.0.113.10:8080" -> "203.0.113.10".
// Falls back to the raw string if URL parsing fails.
func extractHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	// url.Parse includes port in Host. Extract hostname only.
	return u.Hostname()
}

// buildCallbackURL constructs the ready callback URL for an instance.
// Uses GpuctlPublicURL when configured, falling back to branded hostname.
func buildCallbackURL(publicURL, hostname, instanceID string) string {
	if publicURL != "" {
		return strings.TrimRight(publicURL, "/") + "/internal/instances/" + instanceID + "/ready"
	}
	return "https://" + hostname + "/internal/instances/" + instanceID + "/ready"
}

// buildTelemetryURL constructs the in-pod telemetry ingest URL for an instance
// (TELEM-01), mirroring buildCallbackURL: GpuctlPublicURL when configured,
// falling back to the branded hostname. The in-pod nvidia-smi sampler POSTs
// here on a recurring cadence under the same per-instance InternalToken.
func buildTelemetryURL(publicURL, hostname, instanceID string) string {
	if publicURL != "" {
		return strings.TrimRight(publicURL, "/") + "/internal/instances/" + instanceID + "/telemetry"
	}
	return "https://" + hostname + "/internal/instances/" + instanceID + "/telemetry"
}

// buildProbeURL is where a fleet-probe launch downloads the on-host probe and
// posts its report, built the same way as the telemetry URL.
func buildProbeURL(publicURL, hostname, instanceID string) string {
	if publicURL != "" {
		return strings.TrimRight(publicURL, "/") + "/internal/instances/" + instanceID + "/probe"
	}
	return "https://" + hostname + "/internal/instances/" + instanceID + "/probe"
}

// appBasicAuthUser is the fixed username for the per-instance app basic-auth.
// Only the password is secret (D6); the username is a constant so docs/CLI can
// hardcode "gpuai" in the example curl.
const appBasicAuthUser = "gpuai"

// webTerminalPort is the loopback port the bootstrap serves the ttyd web
// console on (v76). Persisted per-instance (instances.terminal_port) so the
// value can change across releases without breaking reads for old rows. 7681
// is ttyd's conventional port; no catalog template exposes it.
const webTerminalPort = 7681

// webConsoleAvailable reports whether a provision with the given template app
// port gets the browser web console (v76): requires the FRP bootstrap (that's
// where ttyd runs), the ops kill switch off, and no port collision with a
// template app (no catalog template uses the ttyd port today; the guard keeps
// a future one from breaking the render). Also the reachability guarantee that
// lets a keyless launch through the SSH-key gate — when this is false, zero
// attached keys means an unreachable node and Provision refuses.
func (e *Engine) webConsoleAvailable(appPort int) bool {
	return e.tunnelMgr != nil && !e.config.WebTerminalDisabled &&
		appPort != webTerminalPort
}

// generateAppBasicAuth produces the per-instance app/console credential (D6):
// a fixed user ("gpuai") and a cryptographically random URL-safe-base64
// password.
//
// 18 raw bytes -> 24 base64-url chars (RawURLEncoding, no padding). RawURL
// alphabet is [A-Za-z0-9_-] — no shell-meta characters, satisfying the Plan-04
// frpc.toml injection guard (the password is rendered into the bootstrap's
// httpPwd). The password is surfaced via the operation result, persisted on
// the instance row for org-scoped reads (v74), and never logged.
func generateAppBasicAuth() (user, pass string, err error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate app basic-auth: %w", err)
	}
	return appBasicAuthUser, base64.RawURLEncoding.EncodeToString(b), nil
}

// generateInstanceID produces a branded instance ID: "gpu-" + 4 random hex bytes.
// Result is 12 characters like "gpu-a1b2c3d4".
func generateInstanceID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "gpu-" + hex.EncodeToString(b), nil
}

// sshPortFromStatus extracts the SSH public port from provider status port mappings.
// Returns 22 as default if no SSH port mapping is found.
func sshPortFromStatus(status *provider.InstanceStatus) int {
	for _, p := range status.Ports {
		if p.PrivatePort == 22 {
			return p.PublicPort
		}
	}
	return 22
}

// regionFromResult returns the CANONICAL region for the instance row: the
// provision result's region if available, falling back to the offering's
// region, then to "unknown" as a last resort. The final "unknown" fallback
// prevents NOT NULL constraint violations on instances.region when a
// supplier returns an offering with no region info (Pitfall 6 from
// 15-RESEARCH.md).
//
// Both sources carry provider-NATIVE labels (e.g. "yto",
// "Delhi", ...), so the value is passed through
// availability.Normalize — the same D-05 single source of truth the catalog
// uses — before persisting. Without this, customers saw a mix of canonical
// and native region labels on /v1/instances (migration v28 backfills the
// rows written before this fix).
func regionFromResult(result *provider.ProvisionResult, offering *provider.GPUOffering) string {
	raw := result.Region
	if raw == "" {
		raw = offering.Region
	}
	if raw == "" {
		return "unknown"
	}
	return availability.Normalize(raw, offering.DatacenterLocation, offering.Provider)
}

// perGPUSpec divides a whole-node hardware figure (RAM GB, vCPU cores) by the
// offering's GPU count to get the per-GPU floor an adapter can re-scale to the
// launch's own GPU count. Unset (0) or an unstated GPU count yields 0, which
// every adapter reads as "assert no floor" — an offering that advertises no
// spec must not be turned into an unsatisfiable placement constraint.
func perGPUSpec(total, gpuCount int) int {
	if total <= 0 {
		return 0
	}
	if gpuCount <= 1 {
		return total
	}
	return total / gpuCount
}

// generateHexToken produces a random hex-encoded token of the specified byte length.
func generateHexToken(byteLen int) (string, error) {
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// withoutProviderAfter returns candidates with every entry AFTER index i whose
// provider is name removed; entries up to and including i are kept so the walk
// index stays valid. Always returns a fresh slice — the caller's candidate list
// is never mutated in place.
func withoutProviderAfter(candidates []ProviderCandidate, i int, name string) []ProviderCandidate {
	out := make([]ProviderCandidate, 0, len(candidates))
	out = append(out, candidates[:i+1]...)
	for _, c := range candidates[i+1:] {
		if strings.EqualFold(c.Prov.Name(), name) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// recordStopReason attaches a customer-facing explanation to a just-terminated
// row. Failures are logged, never fatal: the instance is already stopped.
func (e *Engine) recordStopReason(ctx context.Context, instanceID, reason string) {
	if reason == "" {
		return
	}
	if err := e.db.SetInstanceStatusReason(ctx, instanceID, reason); err != nil {
		e.logger.Warn("failed to record stop reason on instance",
			slog.String("instance_id", instanceID), slog.String("error", err.Error()))
	}
}

// markDirectRunning is the booting->running flip for direct-connect mode (no
// FRP tunnel). The SSH endpoint is the supplier-reported address: status.IP
// plus the public port mapped to container port 22 (22 when none is
// reported). SetInstanceRunning is guarded on a pre-running status, so a
// concurrent terminate wins and this becomes a no-op.
func (e *Engine) markDirectRunning(ctx context.Context, store progressStatusStore, inst *db.Instance, status provider.InstanceStatus) {
	instanceID := inst.InstanceID
	if err := store.SetInstanceSSHPort(ctx, instanceID, sshPortFromStatus(&status)); err != nil {
		e.logger.Error("failed to persist ssh port",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		return // retried next poll tick
	}
	updated, prev, err := store.SetInstanceRunning(ctx, instanceID, status.IP)
	if err != nil {
		e.logger.Error("failed to set instance running",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		return
	}
	if !updated {
		return
	}
	e.logger.Info("instance running (direct connect)",
		slog.String("instance_id", instanceID),
		slog.String("prev_status", prev),
		slog.String("upstream_ip", status.IP),
		slog.Int("ssh_port", sshPortFromStatus(&status)),
	)
	if e.onStatusChange != nil {
		e.onStatusChange(instanceID, StateRunning)
	}
	if e.db != nil {
		metadata, _ := json.Marshal(map[string]string{
			"gpu_type": inst.GPUType,
			"region":   inst.Region,
		})
		if err := e.db.CreateInstanceEvent(ctx, &db.InstanceEvent{
			InstanceID: instanceID,
			OrgID:      inst.OrgID,
			EventType:  "ready",
			Metadata:   metadata,
		}); err != nil {
			e.logger.Warn("failed to log ready event",
				slog.String("instance_id", instanceID),
				slog.String("error", err.Error()),
			)
		}
	}
}
