package public

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/authz"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
	tmplcatalog "github.com/gpuai-dev/gpuai-oss/internal/template/catalog"
)

// Instance is the public-API instance response shape (PRIV-08).
//
// Privacy contract: this struct intentionally OMITS upstream_provider,
// upstream_id, upstream_ip, hostname, internal_token, wg_*, and
// frp_remote_port. The TestNoUpstreamLeakage_Catalog regression test in
// privacy_test.go pins this contract against struct-tag drift.
//
// Note this is NOT the dashboard's InstanceResponse (internal/api/handlers.go).
// The dashboard surface includes Owner + IsOwner; the public API does not
// (an API key is identity, not a person — owner attribution is a dashboard
// concern). Connection details are exposed via the same Hostname:Port:
// SSHCommand triple as the dashboard, sourced from the FRP tunnel coords
// when configured.
type Instance struct {
	ID           string          `json:"id"`
	Name         *string         `json:"name,omitempty"`
	Status       string          `json:"status"` // external state (starting, running, stopping, terminated, error)
	GPUType      string          `json:"gpu_type"`
	GPUCount     int             `json:"gpu_count"`
	Tier         string          `json:"tier"`
	Region       string          `json:"region"`
	PricePerHour float64         `json:"price_per_hour"`
	Connection   *ConnectionInfo `json:"connection,omitempty"`
	// TemplateID is the operator-curated deployable-template id (e.g. "comfyui");
	// present only for template deploys (migration v20). Non-sensitive — NOT a
	// provider/image identifier (TMPL-13 keeps image/start_command off the wire).
	TemplateID *string `json:"template_id,omitempty"`
	// Image is the CUSTOMER's own image reference for a custom-image launch
	// (schema v89), digest-pinned at pre-flight. Present only when the
	// customer supplied it — template/certified launches keep their images
	// server-internal (TMPL-13), so this echoes customer input, nothing more.
	Image *string `json:"image,omitempty"`
	// DiskGB is the instance filesystem size, in GB, the platform configured for
	// this launch (schema v70). Present only where WE set the disk (the winning
	// provider honors a configured size); absent means provider-determined — the
	// machine's own flavor disk — never fabricated. A plain integer carrying no
	// provider identity, so it survives the PRIV-08 strip.
	DiskGB *int `json:"disk_gb,omitempty"`
	// StatusReason explains WHY an instance is in the error state, in
	// platform-side wording ("provisioning stalled: …", "provisioning
	// timeout: …"). Absent for healthy instances. Every writer of the backing
	// column (db.SetInstanceError callers) keeps the text upstream-name-free
	// (INFPRIV) — raw adapter errors go to logs, never into this column.
	StatusReason *string    `json:"status_reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ReadyAt      *time.Time `json:"ready_at,omitempty"`
	TerminatedAt *time.Time `json:"terminated_at,omitempty"`
	// LastReachableAt is the last time the platform's tunnel probe confirmed
	// the SSH path to this instance end-to-end (GPU-49). Absent means not yet
	// probed — a fresh instance, or one with no tunnel — never "unreachable".
	//
	// Useful for the case a status field cannot express: an instance whose
	// status is still "running" but whose last_reachable_at has stopped
	// advancing is one whose SSH has died even though the machine is up.
	// Carries no provider identity, so it survives the PRIV-08 strip.
	LastReachableAt *time.Time `json:"last_reachable_at,omitempty"`
	// AutoTerminateHours is the per-instance runtime bound this instance was
	// launched with (schema v92, GPU-60) — the customer's own request, echoed
	// back. Absent means the instance runs until something stops it. It stays
	// present after the platform acts on it, so a terminated instance still
	// answers "why did this stop?" alongside status_reason.
	AutoTerminateHours *int `json:"auto_terminate_hours,omitempty"`
	// AutoTerminateAt is when that bound expires: computed when the instance
	// became running (ready_at + auto_terminate_hours), so the countdown covers
	// usable runtime, not the provisioning window. Absent when no bound was
	// requested, when the instance is not running yet, or once the platform has
	// begun acting on an elapsed bound — a deadline that is no longer in the
	// future is not advertised as one. A plain timestamp carrying no provider
	// identity, so it survives the PRIV-08 strip.
	AutoTerminateAt *time.Time `json:"auto_terminate_at,omitempty"`
	// UnreachableSince is the start of the CONFIRMED tunnel outage this
	// instance is currently in (GPU-77) — present exactly when Status is
	// "unreachable", absent otherwise. It is the timestamp the platform
	// auto-credits the dead window from, so a customer can reconcile the
	// ledger row against it.
	//
	// Distinct from LastReachableAt: that one is the last GOOD probe and is
	// present on healthy instances too. Carries no provider identity, so it
	// survives the PRIV-08 strip.
	UnreachableSince *time.Time `json:"unreachable_since,omitempty"`
}

// ConnectionInfo is the SSH coordinates for a running instance. With the FRP
// tunnel enabled, Hostname is the FRP proxy host derived from GPUCTL_PUBLIC_URL
// and Port is the per-instance FRP remote port (allocated at provision time).
// In direct-connect mode (the self-host default) Hostname is the
// supplier-reported address and Port its mapped SSH port. SSHCommand
// is the ready-to-paste shell command, including StrictHostKeyChecking
// flags so first-connect doesn't prompt.
type ConnectionInfo struct {
	Hostname   string `json:"hostname"`
	Port       int    `json:"port"`
	SSHCommand string `json:"ssh_command"`
	// AppURL is the HTTPS URL of a deployed application template, surfaced only
	// on running template instances (Phase 31, TMPL-11). Derived purely from the
	// instance id + Config.AppsHost (https://<id>.<AppsHost>) — never stored.
	// Empty/omitted for raw instances.
	AppURL string `json:"app_url,omitempty"`
	// AppUser / AppPassword are the app URL's HTTP Basic Auth login (schema
	// v74, revising D6 surface-once). AppUser is surfaced on every org-scoped
	// read for the instance's lifetime; AppPassword is OPT-IN (GPU-59) —
	// returned only on GET /v1/instances/{id}?include=credentials so bare
	// reads can be pasted into logs/transcripts without leaking the secret.
	// The list endpoint and operation results never return it. Both fields
	// are omitted for raw instances and for template deploys that predate
	// v74 (credential was never stored).
	AppUser     string `json:"app_user,omitempty"`
	AppPassword string `json:"app_password,omitempty"`
	// TerminalURL is the browser web console (ttyd behind the FRP gateway,
	// schema v76): https://<id>-term.<AppsHost>, guarded by the same
	// AppUser/AppPassword basic-auth login. Surfaced only on running
	// instances provisioned with the console (terminal_port non-NULL) —
	// pre-v76 instances have no ttyd on board and omit it.
	TerminalURL string `json:"terminal_url,omitempty"`
}

// maxLaunchGPUCount is the most GPUs ONE launch may request. Named so the
// auto-launch node-shape refusal can intersect against it instead of quoting a
// wider market list and sending a caller into a second 422.
const maxLaunchGPUCount = 8

// CreateInstanceRequest is the JSON body for POST /v1/instances.
//
// Mirrors openapi/v1.yaml + the dashboard's CreateInstanceRequest, but
// stripped to the fields the public API exposes. `Region` (singular,
// back-compat) is supported alongside `Regions` (preferred); when both
// are present, Regions wins. Empty/omitted means "any region".
type CreateInstanceRequest struct {
	GPUType         string   `json:"gpu_type"`
	GPUCount        int      `json:"gpu_count"`
	Region          string   `json:"region,omitempty"`
	Regions         []string `json:"regions,omitempty"`
	Tier            string   `json:"tier"`
	SSHKeyIDs       []string `json:"ssh_key_ids,omitempty"`
	Name            *string  `json:"name,omitempty"`
	MaxPricePerHour *float64 `json:"max_price_per_hour,omitempty"`

	// ViewedPricePerHour is the retail $/hr the caller DISPLAYED to its user
	// before launching (GPU-78). A catalog row is a quote, not a booking: by
	// launch time the marketplace may have repriced, and pinning an offering
	// fixes WHERE we place but carries no price. Send this and a re-quote more
	// than a cent above it fails with price_changed carrying the new price,
	// instead of proceeding at the higher price; omit it and the launch takes
	// the launch-time price exactly as before.
	//
	// Optional and 409-only. The engine never reads it as consent, so it can
	// never raise what you are charged or what we are authorized to buy — the
	// worst a wrong value can do is refuse your own launch.
	ViewedPricePerHour *float64 `json:"viewed_price_per_hour,omitempty"`

	// TemplateID + Env drive a deployable-template deploy (Phase 31, TMPL-07).
	// When TemplateID is set, gpu_type MAY be omitted — the engine auto-selects
	// the cheapest eligible GPU for the template's resource floor. Env carries
	// per-deploy overrides; only keys the template marks user_overridable are
	// accepted (others 422), and secret values are REDACTED from the operation
	// metadata snapshot (TMPL-13). Spec-allowlist validation lives in
	// handleCreateInstance (it needs the catalog), NOT in Validate().
	TemplateID *string           `json:"template_id,omitempty"`
	Env        map[string]string `json:"env,omitempty"`

	// Image launches the instance from a CUSTOMER-supplied container image
	// (custom-images Phase 1), e.g. "pytorch/pytorch:2.4.1-cuda12.4-cudnn9-devel"
	// or "ghcr.io/acme/trainer:v1". The image must be publicly pullable
	// (private registries come with Phase 2) and linux/amd64; both are
	// verified synchronously by a registry pre-flight before the operation is
	// created, and the launch is digest-pinned to exactly the manifest that
	// pre-flight saw. Mutually exclusive with template_id and environment
	// (422): a template carries its own image, and an environment selection
	// would silently override the custom image in the engine. Env carries
	// plain container env vars on this path (no template allowlist —
	// BuildCustomImageEnv gates reserved keys instead). Placement narrows to
	// providers that can run customer images (provision.SupportsCustomerImage).
	Image *string `json:"image,omitempty"`

	// RegistryCredentialID (Phase 2) references a stored registry login
	// (POST /v1/registry-credentials) to pull a PRIVATE image. Only valid with
	// Image. The credential is decrypted server-side for the pre-flight and
	// forwarded to the placed provider's native auth mechanism; the id is all
	// that ever appears in the operation snapshot.
	RegistryCredentialID *string `json:"registry_credential_id,omitempty"`

	// Entrypoint / Cmd (Phase 2) override the image config's process defaults
	// for a custom-image launch — docker semantics: setting entrypoint
	// replaces the image ENTRYPOINT and drops the image CMD (unless cmd is
	// also set); setting cmd alone replaces just the CMD. nil = use the
	// image's own; an explicit empty array clears it. Only valid with Image.
	Entrypoint *[]string `json:"entrypoint,omitempty"`
	Cmd        *[]string `json:"cmd,omitempty"`

	// Ports (Phase 2) declares the container ports a custom-image launch
	// exposes. At most one entry may be primary (and it must be http): the
	// primary port is vhost-proxied at https://<id>.<apps-host> behind the
	// per-instance basic-auth credential — the same plumbing template deploys
	// use — and recorded on the row (schema v90) so reads surface app_url.
	// Non-primary ports are forwarded to the provider's native port exposure.
	// Only valid with Image.
	Ports []InstancePortSpec `json:"ports,omitempty"`

	// EnvironmentSelection picks the environment class at launch (Phase 63):
	// "certified" (GPU.ai Certified Image), "raw_vm" (a VM we bootstrap but
	// cannot certify), or "provider_template" (provider runs our image natively).
	// nil => server default (D-63-04). A value the placed provider cannot honor
	// (e.g. "raw_vm" on a container-only provider, "certified" on a fixed-template
	// provider) is a 422 capability gate resolved in handleCreateInstance against
	// the catalog; an unrecognized value is a 400 in Validate().
	EnvironmentSelection *string `json:"environment,omitempty"`
	// OfferingID pins the launch to one exact offering (D-63-08). nil => legacy
	// price-sorted placement. Opaque server-issued token — carries no provider
	// identity; when the pinned offering is no longer available the launch fails
	// explicitly rather than silently substituting a pricier offering.
	OfferingID *string `json:"offering_id,omitempty"`

	// DiskGB requests a specific instance filesystem size, in GB. nil => the
	// platform default (LAUNCH_DEFAULT_DISK_GB), which every launch already
	// receives and which narrows placement not at all.
	//
	// Setting it explicitly is a HARD requirement, not a hint: placement narrows
	// to capacity that can honor it (the catalog's disk_configurable attribute),
	// and a launch that cannot get the disk it asked for fails with
	// provision.ErrDiskUnavailable rather than silently landing on the provider's
	// fixed size. That asymmetry — nil is a hint, a value is a requirement — is
	// the property that keeps every existing launch byte-identical.
	//
	// Bounds are split by what they need to know: the floor
	// (>= catalog.MinDiskGB) is catalog-independent and lives in Validate(); the
	// ceiling (<= LAUNCH_MAX_DISK_GB) needs config and the capability check needs
	// the availability catalog, so both live in handleCreateInstance — the same
	// split validateEnvironmentFormat and the environment capability gate use.
	DiskGB *int `json:"disk_gb,omitempty"`

	// AutoTerminateHours bounds THIS instance's runtime: the platform
	// terminates it that many hours after it becomes running, through the same
	// path a DELETE takes (billing closeout, webhook, reservation release).
	// nil (the default) means no bound — unchanged behavior for every existing
	// caller.
	//
	// The countdown starts at running, not at this call: provisioning can take
	// ~15 min on slow-boot capacity and is not billed, so charging it against
	// the bound would silently shorten the run. Read back as
	// auto_terminate_hours + the resolved auto_terminate_at deadline.
	//
	// This is unattended-run protection, which is why it lives on the server:
	// a timer in the client dies with the process that set it, and the case
	// this exists for is exactly an agent that crashed after launching.
	AutoTerminateHours *int `json:"auto_terminate_hours,omitempty"`
}

// autoTerminateMaxHours bounds the per-instance runtime limit at 30 days.
// Rationale is the shape of the feature, not a billing policy: the field is a
// SAFETY bound for unattended runs, and a value beyond a month is
// indistinguishable from a typo (a mistyped 720 for 72 costs a customer weeks
// of GPU time only if we accept it). Omitting the field remains the way to say
// "no limit" — the ceiling never turns an unbounded launch into a bounded one.
const autoTerminateMaxHours = 24 * 30

var sshKeyIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsSSHKeyID reports whether id has the form of an SSH key id (a UUID). The
// dashboard create handler shares it, so both surfaces accept the same ids.
func IsSSHKeyID(id string) bool { return sshKeyIDRe.MatchString(id) }

// validateSSHKeyIDs requires every ssh_key_ids entry to be a key id (a UUID),
// so a public key, a name or a malformed value is a clear 400, not a database
// error. The echoed value is truncated.
func validateSSHKeyIDs(ids []string) error {
	for _, id := range ids {
		if !IsSSHKeyID(id) {
			shown := id
			if len(shown) > 40 {
				shown = shown[:40] + "..."
			}
			return fmt.Errorf("ssh_key_ids must contain key ids from GET /v1/ssh-keys, not %q", shown)
		}
	}
	return nil
}

// Validate enforces the field-level invariants the public API contract
// pins. Returns the FIRST validation error so SDK consumers get a single
// actionable message — not a list. Matches the dashboard's Validate
// shape so the two surfaces stay aligned.
func (req *CreateInstanceRequest) Validate() error {
	// Template deploys MAY omit gpu_type AND gpu_count — the engine auto-selects
	// the cheapest eligible GPU for the template's resource floor (Plan 07) and
	// the handler defaults the count to the template's min_gpu_count (CR-01). A
	// raw instance still requires both. Tier is required on both paths.
	if req.GPUType == "" && req.TemplateID == nil {
		return errors.New("gpu_type is required")
	}
	if req.TemplateID == nil && req.GPUCount < 1 {
		return fmt.Errorf("gpu_count must be between 1 and %d", maxLaunchGPUCount)
	}
	if req.GPUCount > maxLaunchGPUCount {
		return fmt.Errorf("gpu_count must be between 1 and %d", maxLaunchGPUCount)
	}
	if req.Tier != "spot" && req.Tier != "on_demand" && req.Tier != "reserved" {
		return errors.New("tier must be 'spot', 'on_demand', or 'reserved'")
	}
	// ssh_key_ids are key ids from GET /v1/ssh-keys. Anything else used to
	// reach the key lookup and fail there with a raw SQL error (2026-09-27: 32
	// creates from one org failed with "invalid input syntax for type uuid").
	if err := validateSSHKeyIDs(req.SSHKeyIDs); err != nil {
		return err
	}
	// Custom image (Phase 1): field-level exclusivity only — the ref grammar
	// is a 400 format gate in the handler (imageref.Parse), and pullability is
	// the registry pre-flight. A template deploy carries its own image, and an
	// environment selection would silently override the custom image in the
	// engine (dockerImageOverride), so both combinations are refused up front.
	if req.Image != nil {
		if *req.Image == "" {
			return errors.New("image must not be empty")
		}
		if req.TemplateID != nil {
			return errors.New("image cannot be combined with template_id — a template already carries its image")
		}
		if req.EnvironmentSelection != nil {
			return errors.New("image cannot be combined with environment — a custom image is its own environment")
		}
		// GPU-92: a spot instance can be reclaimed and re-routed, and the
		// re-route cannot yet carry the custom image's env/entrypoint/ports/
		// registry login — it would boot a stock replacement in the image's
		// place. Refuse the shape up front (the engine repeats the check as
		// defense in depth: provision.ErrCustomImageSpotUnsupported).
		if req.Tier == "spot" {
			return errors.New("image cannot be launched on the spot tier yet — a preemption re-route cannot carry a custom image; use tier on_demand")
		}
	}
	// Phase 2 runtime-config fields require an image — they describe the
	// customer container, which only exists on the custom-image path.
	if req.Image == nil {
		switch {
		case req.RegistryCredentialID != nil:
			return errors.New("registry_credential_id requires image")
		case req.Entrypoint != nil || req.Cmd != nil:
			return errors.New("entrypoint/cmd require image")
		case len(req.Ports) > 0:
			return errors.New("ports requires image")
		}
	}
	if req.RegistryCredentialID != nil && *req.RegistryCredentialID == "" {
		return errors.New("registry_credential_id must not be empty")
	}
	if err := ValidateEntrypointOverride("entrypoint", req.Entrypoint); err != nil {
		return err
	}
	if err := ValidateEntrypointOverride("cmd", req.Cmd); err != nil {
		return err
	}
	if err := ValidatePortSpecs(req.Ports); err != nil {
		return err
	}
	// Disk floor only — the ceiling needs config, so it lives in the handler.
	// Anything under the floor ENOSPCs on boot before the workload starts, and
	// that is true regardless of catalog or configuration, so it belongs here
	// with the other field-level bounds. Shares catalog.MinDiskGB with the
	// template validator so the two cannot drift.
	if req.DiskGB != nil && *req.DiskGB < tmplcatalog.MinDiskGB {
		return fmt.Errorf("disk_gb must be at least %d", tmplcatalog.MinDiskGB)
	}
	// Per-instance runtime bound (GPU-60). Both bounds are catalog- and
	// config-independent, so unlike disk_gb the whole check lives here.
	//
	// Zero is rejected rather than read as "no limit": a client that computed
	// its way to 0 (an unset variable, an int division) means something is
	// wrong with the request, and treating it as "unbounded" would silently
	// launch the exact unattended instance the field exists to bound. Absent is
	// the only way to say "no limit"; that asymmetry is deliberate.
	if req.AutoTerminateHours != nil {
		h := *req.AutoTerminateHours
		if h < 1 {
			return errors.New("auto_terminate_hours must be at least 1 (omit the field for no limit)")
		}
		if h > autoTerminateMaxHours {
			return fmt.Errorf("auto_terminate_hours must be at most %d (30 days)", autoTerminateMaxHours)
		}
	}
	return nil
}

// InstancePortSpec is a customer-declared container port on a custom-image
// launch (Phase 2). Mirrors the template catalog's port shape.
type InstancePortSpec struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"` // "http" | "tcp"
	Primary  bool   `json:"primary,omitempty"`
}

// ValidateEntrypointOverride bounds an entrypoint/cmd override: the argv is
// rendered (quoted, b64-enveloped) into the bootstrap launch script, so the
// caps bound the script size; content needs no charset gate — arguments are
// data, never shell source (provision.customStartB64 quotes per argument).
// Exported: the dashboard create (internal/api) mirrors the custom-image
// contract and MUST judge the identical predicate, not a hand-copy of it.
func ValidateEntrypointOverride(field string, argv *[]string) error {
	if argv == nil {
		return nil
	}
	if len(*argv) > 32 {
		return fmt.Errorf("%s supports at most 32 arguments", field)
	}
	// The 4KB total keeps the b64-enveloped launch script (~4/3 expansion)
	// comfortably inside EVERY container-native provider's script budget —
	// a provider's startup-command field can cap at ~15.8KB including the whole bootstrap, so a
	// 16KB argv would 202 here and then fail at the adapter. Real
	// entrypoints are tiny; this cap fails the oversized ones at the 422
	// stage instead.
	total := 0
	for _, a := range *argv {
		// An empty element renders as `exec ''` in the launch script — the
		// workload dies with "Permission denied" while the instance reports
		// running (live-confirmed 2026-09-02). The docker-CLI-style clear
		// spelling [""] is therefore rejected with a pointer at the API's
		// actual clear idiom (an explicit empty array).
		if a == "" {
			return fmt.Errorf("%s arguments must not be empty strings — to clear the image's %s, pass an empty array", field, field)
		}
		if len(a) > 2048 {
			return fmt.Errorf("%s arguments must be at most 2048 characters each", field)
		}
		// NUL is the argv encoding's element terminator (provision.argvB64);
		// a NUL inside an element would silently re-split the argv. Other
		// control bytes (tabs, newlines in a -c script) are legitimate
		// argument content and pass through b64 untouched.
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("%s arguments must not contain NUL bytes", field)
		}
		total += len(a)
	}
	if total > 4096 {
		return fmt.Errorf("%s is too large (4KB total limit)", field)
	}
	return nil
}

// ValidatePortSpecs bounds the declared port set: at most one primary, and it
// must be http (the vhost proxy is an http reverse proxy); 22 is the
// platform's SSH port and can't be re-declared. Exported for the dashboard
// create's mirror (internal/api), like ValidateEntrypointOverride.
func ValidatePortSpecs(ports []InstancePortSpec) error {
	if len(ports) == 0 {
		return nil
	}
	if len(ports) > 10 {
		return errors.New("ports supports at most 10 entries")
	}
	seen := make(map[int]bool, len(ports))
	primaries := 0
	for _, p := range ports {
		if p.Port < 1 || p.Port > 65535 {
			return errors.New("ports entries must be between 1 and 65535")
		}
		if p.Port == 22 {
			return errors.New("port 22 is reserved for SSH")
		}
		if seen[p.Port] {
			return fmt.Errorf("port %d is declared more than once", p.Port)
		}
		seen[p.Port] = true
		if p.Protocol != "http" && p.Protocol != "tcp" {
			return errors.New("ports protocol must be 'http' or 'tcp'")
		}
		if p.Primary {
			primaries++
			if p.Protocol != "http" {
				return errors.New("the primary port must be http (it is served at your instance's app URL)")
			}
		}
	}
	if primaries > 1 {
		return errors.New("at most one port may be primary")
	}
	return nil
}

// validateEnvironmentFormat is the CHEAP, catalog-independent format gate for
// the Phase-63 environment-selection fields (T-63-01). It runs BEFORE the
// capability gate (which needs the availability catalog) and maps to a 400 —
// an unrecognized `environment` value or a malformed `offering_id` is a
// client-format error, distinct from the 422 the capability gate returns when
// a well-formed value simply can't be honored by the placed provider.
//
// Both fields are untrusted client input. `offering_id` is an opaque
// server-issued token — we only reject values that could not possibly be one
// (empty, over-long, or out of the [A-Za-z0-9._:-] charset), never decode or
// trust it here; the engine re-derives the exact offering (T-63-09).
func (req *CreateInstanceRequest) validateEnvironmentFormat() error {
	if req.EnvironmentSelection != nil {
		if err := validateEnvironmentValue(*req.EnvironmentSelection); err != nil {
			return err
		}
	}
	if req.OfferingID != nil {
		id := *req.OfferingID
		if len(id) == 0 || len(id) > 128 {
			return errors.New("offering_id is malformed")
		}
		for _, c := range id {
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') || c == '-' || c == '_' || c == ':' || c == '.'
			if !ok {
				return errors.New("offering_id is malformed")
			}
		}
	}
	return nil
}

// validateEnvironmentValue is the shape half of the environment grammar: the
// three Phase-63 classes, where "certified" may carry a framework sub-choice as
// "certified:<framework>" (D-64-01) — 1-32 chars of [a-z0-9-] — optionally with
// a version as "certified:<framework>@<version>" (the launch version picker) —
// 1-32 chars of [a-z0-9.-] — and "raw_vm" may carry an OS sub-choice as
// "raw_vm:<os>" (the launch OS picker) — 1-32 chars of [a-z0-9.-]; os ids and
// versions like "ubuntu-24.04"/"2.13" carry dots. A CHEAP
// catalog-independent charset check only. Whether the framework/OS actually
// exists/is published is the manifest-backed 422 gate in handleCreateInstance
// (well-formed-but-unavailable), not a 400 here.
func validateEnvironmentValue(sel string) error {
	switch sel {
	case "certified", "raw_vm", "provider_template":
		return nil
	}
	if sub, ok := strings.CutPrefix(sel, "certified:"); ok {
		// sub is "<framework>" or "<framework>@<version>" (the launch version picker).
		name, ver, hasVer := strings.Cut(sub, "@")
		if validFrameworkShape(name) && (!hasVer || validVersionShape(ver)) {
			return nil
		}
	}
	if os, ok := strings.CutPrefix(sel, "raw_vm:"); ok && validOSIDShape(os) {
		return nil
	}
	return errors.New("environment must be 'certified', 'certified:<framework>', 'certified:<framework>@<version>', 'raw_vm', 'raw_vm:<os>', or 'provider_template'")
}

// validFrameworkShape bounds an untrusted framework id to the roster alphabet
// ([a-z0-9-], 1-32 chars, no leading/trailing '-') without consulting the
// manifest — the format gate stays cheap and catalog-independent (T-63-01).
func validFrameworkShape(fw string) bool {
	if len(fw) == 0 || len(fw) > 32 || fw[0] == '-' || fw[len(fw)-1] == '-' {
		return false
	}
	for _, c := range fw {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// validOSIDShape bounds an untrusted raw-VM os id to the vm_images catalog
// alphabet ([a-z0-9.-], 1-32 chars, no leading/trailing '-' or '.') without
// consulting the manifest — the format gate stays cheap and catalog-independent
// (T-63-01). Same shape manifest.ValidVMOSID enforces at boot.
func validOSIDShape(os string) bool {
	if len(os) == 0 || len(os) > 32 {
		return false
	}
	if os[0] == '-' || os[0] == '.' || os[len(os)-1] == '-' || os[len(os)-1] == '.' {
		return false
	}
	for _, c := range os {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// validVersionShape bounds an untrusted framework version id (the
// `certified:<fw>@<version>` suffix, the launch version picker) to the same
// dotted alphabet os ids use — versions like "2.13" carry dots. Catalog-
// independent (T-63-01); whether the version is actually published for the GPU
// is the manifest-backed 422 gate. Mirrors manifest.ValidFrameworkVersionID.
func validVersionShape(ver string) bool {
	return validOSIDShape(ver)
}

// PatchInstanceRequest is the JSON body for PATCH /v1/instances/{id}.
// Only `name` is mutable in v1 — everything else is locked at create
// time so changes can be expressed as new instances.
type PatchInstanceRequest struct {
	Name *string `json:"name,omitempty"`
}

// operationResult is the terminal-success payload persisted on the operation
// row (operations.result, schema v21) and surfaced verbatim by
// GET /v1/operations/{id}. It carries the per-instance app/console credential
// (D6): app_url for template deploys, terminal_url for any instance
// provisioned with the web console (v76), and the shared basic-auth login
// whenever either is present. All fields omitempty. The same JSON keys are
// declared in the OpenAPI Operation.result schema and mirrored by the CLI's
// apiclient.OperationResult.
type operationResult struct {
	AppURL           string `json:"app_url,omitempty"`
	TerminalURL      string `json:"terminal_url,omitempty"`
	AppBasicAuthUser string `json:"app_basic_auth_user,omitempty"`
	AppBasicAuthPass string `json:"app_basic_auth_pass,omitempty"`
}

// instanceStore is the narrow seam handlers_instances.go consumes from
// the DB layer. *db.Pool satisfies it; tests substitute an in-memory
// stub via PublicDepsWithInstanceStore.
//
// Same package-private convention as sshKeyDeps / operationStore.
type instanceStore interface {
	ListInstances(ctx context.Context, orgID string, statuses []string, cursor *time.Time, cursorID string, limit int) ([]db.Instance, error)
	GetInstanceForOrg(ctx context.Context, instanceID, orgID string) (*db.Instance, error)
	UpdateInstanceName(ctx context.Context, instanceID, orgID string, name *string) error
}

// engineRunner is the narrow seam through which handlers reach the
// provisioning engine. *provision.Engine satisfies it. Tests provide an
// in-memory stub that returns a fixed InstanceID — sufficient to verify
// the FSM transitions of handleCreateInstance without spinning up
// providers or FRP.
//
// Method signatures match *provision.Engine exactly so the production
// path is a zero-allocation passthrough.
type engineRunner interface {
	Provision(ctx context.Context, req provision.ProvisionRequest) (*provision.ProvisionResponse, error)
	Terminate(ctx context.Context, instanceID string) error
}

// resolveInstanceStore returns the test override when present, otherwise
// the *db.Pool which already implements instanceStore.
func (deps PublicDeps) resolveInstanceStore() instanceStore {
	if deps.instStore != nil {
		return deps.instStore
	}
	return deps.DB
}

// auditStore is the narrow seam through which the public surface writes
// audit-log rows (Phase 39 Plan 05, SEC-09). *db.Pool satisfies it via
// CreateAuditEntry — the SAME writer internal/api's logAudit wraps, so the
// public-surface terminate audit lands in the same audit_log table with the
// same shape. Tests inject a capturing stub via PublicDepsWithAuditStore.
type auditStore interface {
	CreateAuditEntry(ctx context.Context, entry *db.AuditEntry) error
}

// resolveAuditStore returns the test override when present, otherwise the
// *db.Pool which already implements auditStore. Returns nil only when
// neither is wired (DB-less unit tests that don't assert auditing) — callers
// must treat a nil store as a non-fatal skip, mirroring logAudit's stance.
func (deps PublicDeps) resolveAuditStore() auditStore {
	if deps.auditStore != nil {
		return deps.auditStore
	}
	if deps.DB != nil {
		return deps.DB
	}
	return nil
}

// resolveEngineRunner returns the test override when present, otherwise
// the *provision.Engine which already implements engineRunner.
func (deps PublicDeps) resolveEngineRunner() engineRunner {
	if deps.engineRunner != nil {
		return deps.engineRunner
	}
	return deps.Engine
}

// RegisterInstanceRoutes wires the 5 instance routes + the 1 operations
// route onto mux. Wave 6+ router-serialization convention: each handler
// module owns its own Register*Routes helper so router.go remains a thin
// aggregator and parallel plans never collide on its body.
//
// /v1/operations/{id} reuses the instances:read scope because operations
// in v1 are 1:1 with instance lifecycle. A future plan that adds
// non-instance operations (e.g. ssh-key bulk import) can introduce an
// "operations" scope at that time without breaking existing tokens.
func RegisterInstanceRoutes(mux *http.ServeMux, deps PublicDeps) {
	mux.Handle("GET /v1/instances",
		chain(deps, "instances", "read")(http.HandlerFunc(deps.handleListInstances)))
	mux.Handle("GET /v1/instances/{id}",
		chain(deps, "instances", "read")(http.HandlerFunc(deps.handleGetInstance)))
	mux.Handle("POST /v1/instances",
		chainWrite(deps, "instances")(http.HandlerFunc(deps.handleCreateInstance)))
	mux.Handle("PATCH /v1/instances/{id}",
		chainWrite(deps, "instances")(http.HandlerFunc(deps.handlePatchInstance)))
	mux.Handle("DELETE /v1/instances/{id}",
		chainWrite(deps, "instances")(http.HandlerFunc(deps.handleDeleteInstance)))
	// Operations endpoint pinned to instances:read — operations in v1 are
	// 1:1 with instance lifecycle; granting operations:read separately
	// would require every existing key to be re-issued.
	mux.Handle("GET /v1/operations/{id}",
		chain(deps, "instances", "read")(http.HandlerFunc(deps.handleGetOperation)))
}

// mapEngineError translates known provider/engine error sentinels to
// public-API error codes from the Plan 18-05 registry. Unknown errors
// map to "operation_failed" — never expose upstream error messages
// verbatim through the code field (the err.Error() may contain upstream
// provider hints; we surface that ONLY via Operation.Error.Detail).
//
// The codes below MUST be present in errors.go's codeRegistry — the
// TestCodeRegistryCoherence test asserts no handler emits a code that
// isn't registered.
//
// Engine sentinels covered:
//   - provider.ErrNoCapacity        → operation_failed (try later / different region)
//   - provision.ErrPriceExceeded    → price_changed (re-confirm at the new price)
//   - provision.ErrSSHKeysNotFound  → validation_failed (caller-side fix)
//   - provision.ErrSpendingLimitReached / ErrMemberSpendingLimitReached → quota_exceeded
//
// "insufficient_balance" is not currently produced by any engine sentinel —
// the dashboard handler enforces it via EnsureOrgBalance BEFORE calling
// the engine. The public-API path doesn't pre-check balance (deferred to
// a future plan), so we do not map any sentinel to it today; the registry
// reservation in errors.go is forward-looking. Leaving it in mapEngineError
// would create a dead branch.
func mapEngineError(err error) string {
	var vramErr *provision.TemplateVRAMError
	switch {
	case errors.As(err, &vramErr):
		// NOT operation_failed: like price_changed below, this is a failure a
		// retry can never fix — the template's declared floor and the type's
		// VRAM are both static, so the identical request fails identically
		// forever. Unreachable via this handler today (the pre-flight gate
		// judges the same predicate), but the engine backstop exists exactly
		// for future callers that skip the gate, and for them the code must
		// not read "retryable".
		return "gpu_vram_too_small"
	case errors.Is(err, provider.ErrNoCapacity):
		// No upstream capacity is operationally a transient failure —
		// SDK retry logic should treat operation_failed as retryable
		// after a backoff.
		return "operation_failed"
	case errors.Is(err, provision.ErrNoProvider):
		return "operation_failed"
	case errors.Is(err, provision.ErrPriceExceeded):
		// NOT operation_failed: that code tells SDKs "retryable after a
		// backoff", and this is the one launch failure a retry can never fix.
		// The market moved above the approved price, so the identical request
		// fails identically forever — the caller has to re-price against the
		// new number, which is a fresh spend decision, not a backoff. This
		// became common the moment every launch carried an implicit ceiling
		// (previously it needed an explicit max_price_per_hour, which most
		// callers never sent).
		return "price_changed"
	case errors.Is(err, provision.ErrSSHKeysNotFound):
		return "validation_failed"
	case errors.Is(err, provision.ErrCustomImageSpotUnsupported):
		// GPU-92: request-shape refusal, never retryable — the handler's
		// Validate() 422s it first; this covers any other engine caller.
		return "validation_failed"
	case errors.Is(err, provision.ErrImageEntrypointUnavailable):
		// NOT operation_failed (retryable): the image's config ENTRYPOINT
		// and the capacity's bootstrap mechanism are both static, so the
		// identical request fails identically forever — the caller must
		// change the image or the placement, not back off.
		return "validation_failed"
	case errors.Is(err, provision.ErrSSHKeyForbidden):
		// Member passed an ssh_key_id for a key they don't own (v33).
		return "insufficient_scope"
	default:
		return "operation_failed"
	}
}

// instanceToPublic maps a db.Instance row to the public wire shape.
// Structurally drops every upstream/internal field. When the instance is
// running, ConnectionInfo is populated (FRP tunnel coordinates when the
// tunnel is configured, the supplier-reported address in direct-connect
// mode); otherwise it is nil so SDK consumers can detect "not yet
// connectable".
//
// includeCredentials gates connection.app_password (GPU-59): only the
// single-instance GET with ?include=credentials passes true — list reads,
// PATCH responses, and webhook payloads always redact. app_user and the
// app/terminal URLs are non-secret and stay regardless.
func instanceToPublic(inst *db.Instance, frpHost, appsHost string, includeCredentials bool) Instance {
	out := Instance{
		ID:       inst.InstanceID,
		Name:     inst.Name,
		Status:   provision.ExternalState(inst.Status),
		GPUType:  inst.GPUType,
		GPUCount: inst.GPUCount,
		Tier:     inst.Tier,
		Region:   inst.Region,
		// Round to whole cents at the wire: rows written before the
		// RetailPrice formula landed carry 3-decimal wholesale-derived rates
		// (e.g. 1.188) that would otherwise leak precision inconsistency to
		// customers. Stored values are untouched — display-only rounding.
		PricePerHour: math.Round(inst.PricePerHour*100) / 100,
		// Additive (Phase 31.1 Plan 02): surface the operator-curated catalog id
		// so the dashboard can render the template-name badge. omitempty → absent
		// for raw instances; an existing column (migration v20), zero migration.
		TemplateID: inst.TemplateID,
		// Additive (schema v70): the configured instance disk. omitempty → absent
		// for provider-determined disks and rows predating the column.
		DiskGB: inst.DiskGB,
		// Additive (schema v89): the customer's own digest-pinned image ref.
		// omitempty → absent for every non-custom-image launch. Only ever a
		// string the customer themselves sent, so echoing it leaks nothing
		// (template/certified images stay server-internal per TMPL-13).
		Image: inst.Image,
		// The why behind an error status (SetInstanceError writers). The
		// column carries platform-side wording only — see the field doc —
		// and the brand scrub is a backstop in case a future writer embeds
		// an upstream error (it also covers instance.failed webhooks, which
		// serialize through this function).
		StatusReason: scrubReasonPtr(inst.ErrorReason),
		CreatedAt:    inst.CreatedAt,
		ReadyAt:      inst.ReadyAt,
		TerminatedAt: inst.TerminatedAt,
		// Additive (schema v80, GPU-49): last verified SSH reachability.
		// omitempty → absent for never-probed instances.
		LastReachableAt: inst.LastReachableAt,
		// Additive (schema v92, GPU-60): the customer's own runtime bound and
		// the deadline it resolved to at the running transition. Echoes the
		// request back; carries no provider identity.
		AutoTerminateHours: inst.AutoTerminateHours,
		AutoTerminateAt:    inst.AutoTerminateAt,
	}

	// GPU-77 degraded status, applied HERE rather than in
	// provision.ExternalState: the mapping needs unreachable_since, which the
	// FSM helper has no access to (it maps a status string), and the DB's
	// lifecycle column must keep saying "running" — every optimistic
	// running->error flip, ListActiveInstances, and the probe's own
	// `Status != "running"` skip depend on it.
	//
	// Guarded on internal "running" so lifecycle truth always wins: the
	// escalation path leaves unreachable_since set on the instance it fails,
	// and a terminated instance that never got a final probe keeps its flag
	// too. Neither may read as "unreachable".
	if inst.Status == "running" && inst.UnreachableSince != nil {
		out.Status = "unreachable"
		out.UnreachableSince = inst.UnreachableSince
	}

	// Direct-connect mode (no FRP tunnel): SSH goes straight to the
	// supplier-reported address and the port it mapped to container port 22.
	if inst.FRPRemotePort == nil && inst.UpstreamIP != nil && *inst.UpstreamIP != "" && inst.Status == "running" {
		port := 22
		if inst.SSHPort != nil && *inst.SSHPort > 0 {
			port = *inst.SSHPort
		}
		host := *inst.UpstreamIP
		out.Connection = &ConnectionInfo{
			Hostname:   host,
			Port:       port,
			SSHCommand: fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p %d root@%s", port, host),
		}
	}

	if inst.FRPRemotePort != nil && frpHost != "" && inst.Status == "running" {
		port := *inst.FRPRemotePort
		out.Connection = &ConnectionInfo{
			Hostname:   frpHost,
			Port:       port,
			SSHCommand: fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p %d root@%s", port, frpHost),
		}
		// TMPL-11 (HG-01): surface the deterministic app URL for a RUNNING
		// template instance so a caller who didn't capture the one-time deploy
		// output can still find where the app lives. Derived purely from the
		// instance id + AppsHost (the URL is non-secret). Guarded on
		// appsHost != "" so a misconfigured deployment never emits a malformed
		// "https://<id>." host.
		// Custom-image launches (schema v90) record their declared primary
		// port on the row instead of carrying a template id — either signal
		// means "this instance serves an app at the deterministic URL".
		if (inst.TemplateID != nil || inst.AppPort != nil) && appsHost != "" {
			out.Connection.AppURL = "https://" + inst.InstanceID + "." + appsHost
		}
		// v76: the browser web console, for any instance provisioned with it
		// (terminal_port non-NULL) — template deploys and raw launches alike.
		if inst.TerminalPort != nil && appsHost != "" {
			out.Connection.TerminalURL = "https://" + inst.InstanceID + "-term." + appsHost
		}
		// v74 (revising D6): re-surface the persisted basic-auth credential on
		// this org-scoped read so the app/console URLs are actually usable
		// from the instance page. Nil for pre-v74 deploys → fields omitted.
		// GPU-59: the password is opt-in — only GET /v1/instances/{id}
		// ?include=credentials sets includeCredentials; every other read
		// (list, patch, webhooks) keeps app_user + URLs but omits the secret.
		if (out.Connection.AppURL != "" || out.Connection.TerminalURL != "") &&
			inst.AppAuthUser != nil && inst.AppAuthPass != nil {
			out.Connection.AppUser = *inst.AppAuthUser
			if includeCredentials {
				out.Connection.AppPassword = *inst.AppAuthPass
			}
		}
	}
	return out
}

// extractHost extracts the hostname from a URL string (e.g.
// "https://api.example.com" -> "api.example.com"). Falls back to the raw string
// on parse failure so a misconfigured GPUCTL_PUBLIC_URL doesn't blank
// out the SSH command — operators see the bad value and can fix it.
func extractHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Hostname()
}

// engineGoroutineTimeout bounds the lifetime of the async provision
// goroutine spawned by handleCreateInstance / handleDeleteInstance.
// 10 minutes matches the existing dashboard provisioning timeout —
// long enough for slow providers (some take 3-5 min to
// boot) and short enough that a stuck operation surfaces via FSM
// failure rather than hanging forever.
const engineGoroutineTimeout = 10 * time.Minute

// regionsFromRequest collapses the back-compat Region/Regions fields
// into a single canonical slice. Regions wins when both are present;
// otherwise singular Region is wrapped in a one-element slice; empty
// returns nil ("any region" — engine selects by price).
func regionsFromRequest(req CreateInstanceRequest) []string {
	if len(req.Regions) > 0 {
		return req.Regions
	}
	if strings.TrimSpace(req.Region) != "" {
		return []string{req.Region}
	}
	return nil
}

// buildTemplateEnv overlays user-supplied env overrides onto the template's
// declared defaults, returning the effective []provider.EnvVar to inject into
// the container. Each override key MUST match a spec env entry that is
// user_overridable — an unknown key OR a key the template does not expose for
// override is rejected (the caller surfaces 422). This is the trust boundary
// (T-31-13c): a caller cannot smuggle arbitrary env into the provisioned pod.
//
// Secret is copied from the spec entry so downstream redaction (the metadata
// snapshot) and the adapter's secret handling keep their classification. The
// result preserves the spec's declaration order with overrides applied
// in-place, so the effective env is deterministic.
// BuildCustomImageEnv validates and projects the request's env map for a
// CUSTOM-image launch (Phase 1). Unlike the template path there is no spec
// allowlist — the image is the customer's, so its env is too — but the
// platform's reserved namespace is still off limits: PUBLIC_KEY and GPUAI_*
// are how the bootstrap and adapters communicate with the instance, and a
// customer value there could redirect SSH-key injection or callback plumbing
// (the same reservation the template catalog enforces at load time). Keys
// must be well-formed POSIX names; values are bounded but otherwise free —
// they travel as JSON into provider APIs, never into shell source. Exported
// for the dashboard create's mirror (internal/api) — the reserved-namespace
// gate is a trust boundary and must be one function, not two copies.
func BuildCustomImageEnv(env map[string]string) ([]provider.EnvVar, error) {
	if len(env) == 0 {
		return nil, nil
	}
	if len(env) > 64 {
		return nil, errors.New("env supports at most 64 keys")
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic order for snapshots and adapters
	out := make([]provider.EnvVar, 0, len(keys))
	for _, k := range keys {
		if !envKeyRegex.MatchString(k) {
			return nil, fmt.Errorf("env key %q is invalid (letters, digits, and underscores; must not start with a digit)", k)
		}
		if k == "PUBLIC_KEY" || strings.HasPrefix(k, "GPUAI_") {
			return nil, fmt.Errorf("env key %q is reserved by the platform", k)
		}
		if len(env[k]) > 4096 {
			return nil, fmt.Errorf("env value for %q exceeds 4096 characters", k)
		}
		// No control bytes in values (Phase 3): on VM placements env travels
		// as a docker --env-file (KEY=VALUE per line), so a newline in a
		// value could smuggle extra lines into the file. Real env values
		// never carry control bytes; refuse rather than escape.
		for _, r := range env[k] {
			if r < 0x20 || r == 0x7f {
				return nil, fmt.Errorf("env value for %q contains control characters", k)
			}
		}
		out = append(out, provider.EnvVar{Key: k, Value: env[k]})
	}
	return out, nil
}

var envKeyRegex = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func buildTemplateEnv(spec *tmplcatalog.TemplateSpec, overrides map[string]string) ([]provider.EnvVar, error) {
	// Index spec env by key for the allowlist check.
	specByKey := make(map[string]tmplcatalog.EnvSpec, len(spec.Env))
	for _, e := range spec.Env {
		specByKey[e.Key] = e
	}
	for k := range overrides {
		entry, ok := specByKey[k]
		if !ok || !entry.UserOverridable {
			return nil, fmt.Errorf("env key %q is not overridable for this template", k)
		}
	}
	out := make([]provider.EnvVar, 0, len(spec.Env))
	for _, e := range spec.Env {
		val := e.Default
		if ov, ok := overrides[e.Key]; ok {
			val = ov
		}
		out = append(out, provider.EnvVar{Key: e.Key, Value: val, Secret: e.Secret})
	}
	return out, nil
}

// redactSecretEnv returns a shallow copy of req with every Env value whose
// matching spec entry is Secret==true replaced by the literal "[REDACTED]".
// Used to sanitize the operation.metadata snapshot (TMPL-13, T-31-13a /
// Pitfall 6) so a persisted/logged request body never carries an HF token or
// other secret. The raw secret env is threaded separately into the provision
// request — only the snapshot is redacted. Non-secret keys (and the rest of
// the request) pass through unchanged.
func redactSecretEnv(req CreateInstanceRequest, spec *tmplcatalog.TemplateSpec) CreateInstanceRequest {
	if len(req.Env) == 0 {
		return req
	}
	secretKeys := make(map[string]bool, len(spec.Env))
	for _, e := range spec.Env {
		if e.Secret {
			secretKeys[e.Key] = true
		}
	}
	sanitized := make(map[string]string, len(req.Env))
	for k, v := range req.Env {
		if secretKeys[k] {
			sanitized[k] = "[REDACTED]"
			continue
		}
		sanitized[k] = v
	}
	out := req
	out.Env = sanitized
	return out
}

// templatePortsToProvider projects the catalog's []catalog.Port to the
// provider adapter's []provider.Port shape (same fields, distinct package
// types). The adapter renders these into the upstream create call (Plan 03).
func templatePortsToProvider(ports []tmplcatalog.Port) []provider.Port {
	if len(ports) == 0 {
		return nil
	}
	out := make([]provider.Port, 0, len(ports))
	for _, p := range ports {
		out = append(out, provider.Port{Port: p.Port, Protocol: p.Protocol, Primary: p.Primary})
	}
	return out
}

// primaryPort returns the port number of the spec's Primary port (the app's
// main web port — surfaced as app_url and vhost-proxied). Returns 0 when no
// port is marked primary; the catalog Load guarantees exactly one primary for
// active (non-coming_soon) templates, so 0 only occurs for malformed specs.
func primaryPort(ports []tmplcatalog.Port) int {
	for _, p := range ports {
		if p.Primary {
			return p.Port
		}
	}
	return 0
}

// frpHost returns the public hostname of the frps daemon, preferring the
// explicit FRP_PUBLIC_HOST (frps on a dedicated gateway) and falling back to
// extractHost(GpuctlPublicURL) when unset (one host fronts both HTTPS and
// frps). Returns "" when
// Config is nil (test path) so ConnectionInfo defaults to nil — tests
// that verify the SSH command set a non-nil Config explicitly.
func (deps PublicDeps) frpHost() string {
	if deps.Config == nil {
		return ""
	}
	if deps.Config.FRPPublicHost != "" {
		return deps.Config.FRPPublicHost
	}
	return extractHost(deps.Config.GpuctlPublicURL)
}

// appsHost returns the wildcard apps host (Config.AppsHost) used to derive a
// running template instance's app_url. Empty when unset or Config is absent
// (e.g. handler unit tests that build PublicDeps without a Config) — callers
// already guard on "" so an empty value simply omits app_url.
func (deps PublicDeps) appsHost() string {
	if deps.Config == nil {
		return ""
	}
	return deps.Config.AppsHost
}

// handleCreateInstance serves POST /v1/instances. D-08 async write:
// returns 202 Accepted + Operation-Id header + Operation body; the
// engine work runs in a goroutine. Clients poll GET /v1/operations/{id}
// to observe transitions.
//
// This handler is deliberately thin: decode -> delegate -> serialize. Every
// validation gate, the operation row creation and the engine dispatch live in
// deps.CreateInstance (create_instance.go) so a background caller — the
// auto-launch firer replaying a customer's frozen launch spec — runs the SAME
// gates without an http.Request (D-28). Do NOT add a gate here: it would apply
// to typed launches and silently skip fired ones, which is exactly the drift
// TestCreateInstanceValidationParity exists to catch.
//
// FSM transitions (MAJOR 13 fix) happen in runCreateInstance, dispatched by
// CreateInstance:
//
//	pending  -> in_progress           (UpdateOperationState)
//	         -> resource_id attached  (SetOperationResource on success)
//	         -> succeeded             (UpdateOperationState)
//	  on err -> failed (ErrorCode + ErrorDetail)  (SetOperationFailure)
//
// Each UPDATE returns the new version via RETURNING; the goroutine MUST
// reassign op.Version = newVersion so the next UPDATE locks against the
// correct version. On any optimistic-lock conflict, the goroutine logs
// "FSM corrupted" with operation_id and aborts — does NOT continue with
// a stale version.
//
// Goroutine context: detached via context.Background() with a 10-min
// timeout. Request cancellation does not kill the engine — once the
// 202 is on the wire, the customer's HTTP client may disconnect at any
// time and the engine MUST keep running to completion.
func (deps PublicDeps) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	apiKey := APIKeyFromContext(r.Context())
	if apiKey == nil {
		WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	var req CreateInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "validation_failed", "Invalid JSON body")
		return
	}
	resp, err := deps.CreateInstance(r.Context(), apiKey, req)
	if err != nil {
		// A *CreateInstanceError carries the exact status/type/detail the
		// inline WriteProblem call used to pass, so the wire response is
		// unchanged by the extraction.
		var cerr *CreateInstanceError
		if errors.As(err, &cerr) {
			WriteProblem(w, r, cerr.Status, cerr.Type, cerr.Detail)
			return
		}
		// Unreachable today — every rejection inside CreateInstance is a
		// *CreateInstanceError. Kept so a future non-problem error cannot leak
		// an engine string to the customer or fall through to a 200.
		slog.ErrorContext(r.Context(), "instances: create failed with non-problem error", "error", err.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to start operation")
		return
	}
	w.Header().Set("Operation-Id", resp.OperationID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
}

// runCreateInstance drives the create-instance FSM in a goroutine. Split
// out from handleCreateInstance so unit tests can call it synchronously
// and observe each FSM transition end-to-end.
//
// Pattern (MAJOR 13): every UPDATE call follows
//
//	newV, err := store.X(ctx, opID, op.Version, ...)
//	if err != nil { log + return }
//	op.Version = newV
//
// NEVER swallow the error with `_ =` (loses conflict signal) and
// NEVER do a manual increment of op.Version (drifts from the
// DB-authoritative version). The acceptance grep in PLAN.md enforces
// both.
func runCreateInstance(store operationStore, engine engineRunner, op *db.Operation, req provision.ProvisionRequest) {
	bgCtx, cancel := context.WithTimeout(context.Background(), engineGoroutineTimeout)
	defer cancel()

	// pending -> in_progress.
	newV, err := store.UpdateOperationState(bgCtx, op.OperationID, op.Version, "in_progress")
	if err != nil {
		slog.ErrorContext(bgCtx, "instances: in_progress transition failed (FSM corrupted)",
			"operation_id", op.OperationID, "error", err.Error())
		return
	}
	op.Version = newV

	resp, perr := engine.Provision(bgCtx, req)
	if perr != nil {
		slog.ErrorContext(bgCtx, "instances: engine.Provision failed",
			"operation_id", op.OperationID, "error", perr.Error())
		newV, ferr := store.SetOperationFailure(bgCtx, op.OperationID, op.Version,
			mapEngineError(perr), perr.Error())
		if ferr != nil {
			slog.ErrorContext(bgCtx, "instances: SetOperationFailure conflict (FSM corrupted)",
				"operation_id", op.OperationID, "error", ferr.Error())
			return
		}
		op.Version = newV
		return
	}

	// Attach resource id; reassign version.
	newV, err = store.SetOperationResource(bgCtx, op.OperationID, op.Version, resp.InstanceID)
	if err != nil {
		slog.ErrorContext(bgCtx, "instances: SetOperationResource conflict (FSM corrupted)",
			"operation_id", op.OperationID, "error", err.Error())
		return
	}
	op.Version = newV

	// Surface the per-instance app/console credential on the operation RESULT
	// (D6 channel; since v74/v76 it is also re-readable on the org-scoped
	// instance reads). Any instance with a vhost surface carries a password —
	// template deploys AND web-console provisions; only a render with neither
	// (console kill switch on, raw launch) skips this write entirely. The
	// credential is NEVER written to the request metadata snapshot (redacted,
	// Task 1).
	if resp.AppBasicAuthPass != "" {
		result, mErr := json.Marshal(operationResult{
			AppURL:           resp.AppURL,
			TerminalURL:      resp.TerminalURL,
			AppBasicAuthUser: resp.AppBasicAuthUser,
			AppBasicAuthPass: resp.AppBasicAuthPass,
		})
		if mErr != nil {
			// Unreachable for a struct of strings; log and continue so the
			// deploy still completes (the FSM reaches succeeded). The owner can
			// re-derive the app_url from the instance; only the one-time
			// password is lost in this should-never-happen path.
			slog.ErrorContext(bgCtx, "instances: marshal operation result failed",
				"operation_id", op.OperationID, "error", mErr.Error())
		} else {
			newV, rErr := store.SetOperationResult(bgCtx, op.OperationID, op.Version, result)
			if rErr != nil {
				slog.ErrorContext(bgCtx, "instances: SetOperationResult conflict (FSM corrupted)",
					"operation_id", op.OperationID, "error", rErr.Error())
				return
			}
			op.Version = newV
		}
	}

	// Mark succeeded; reassign version.
	newV, err = store.UpdateOperationState(bgCtx, op.OperationID, op.Version, "succeeded")
	if err != nil {
		slog.ErrorContext(bgCtx, "instances: succeeded transition failed (FSM corrupted)",
			"operation_id", op.OperationID, "error", err.Error())
		return
	}
	op.Version = newV
}

// externalStatusFilter maps the customer-facing ?status= value (the same
// vocabulary ExternalState emits) to the internal instance states it covers.
// "all" is handled separately; absence of the param means "everything except
// terminated" — the default list is the org's live fleet, not its unbounded
// provision history.
var externalStatusFilter = map[string][]string{
	"allocating": {provision.StateCreating, provision.StateProvisioning},
	"starting":   {provision.StateBooting},
	"running":    {provision.StateRunning},
	"stopping":   {provision.StateStopping},
	"stopped":    {provision.StateStopped},
	"terminated": {provision.StateTerminated},
	"error":      {provision.StateError},
}

// nonTerminatedStates is the default GET /v1/instances filter: every internal
// state except terminated.
var nonTerminatedStates = []string{
	provision.StateCreating, provision.StateProvisioning, provision.StateBooting,
	provision.StateRunning, provision.StateStopped, provision.StateStopping,
	provision.StateError,
}

// handleListInstances serves GET /v1/instances. Cursor-paginated org-scoped
// list; defense-in-depth filters by api_key.OrgID. Same cursor scheme as
// SSH keys: (created_at, instance_id) tuple base64url-encoded.
//
// ?status= filters by customer-facing state (allocating | starting | running |
// stopping | stopped | terminated | error | all). Default is all non-terminated
// states.
func (deps PublicDeps) handleListInstances(w http.ResponseWriter, r *http.Request) {
	apiKey := APIKeyFromContext(r.Context())
	if apiKey == nil {
		WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	params := ParsePageParams(r)
	var statuses []string
	switch sp := r.URL.Query().Get("status"); sp {
	case "":
		statuses = nonTerminatedStates
	case "all":
		statuses = nil
	default:
		var ok bool
		statuses, ok = externalStatusFilter[sp]
		if !ok {
			WriteProblem(w, r, http.StatusBadRequest, "validation_failed",
				"Invalid status filter: must be one of allocating, starting, running, stopping, stopped, terminated, error, all")
			return
		}
	}
	var cursorTime *time.Time
	var cursorID string
	if params.Cursor != "" {
		ts, id, derr := DecodeCursor(params.Cursor)
		if derr != nil {
			WriteProblem(w, r, http.StatusBadRequest, "validation_failed", "Invalid cursor")
			return
		}
		cursorTime = &ts
		cursorID = id
	}
	store := deps.resolveInstanceStore()
	rows, err := store.ListInstances(r.Context(), apiKey.OrgID, statuses, cursorTime, cursorID, params.Limit)
	if err != nil {
		slog.ErrorContext(r.Context(), "instances: list failed", "error", err.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to list instances")
		return
	}
	frpHost := deps.frpHost()
	appsHost := deps.appsHost()
	items := make([]Instance, len(rows))
	for i := range rows {
		// GPU-59: the list NEVER returns app_password — ?include is not
		// parsed here on purpose (least surface; use the single GET).
		items[i] = instanceToPublic(&rows[i], frpHost, appsHost, false)
	}
	page := BuildPage(items, params.Limit, func(s Instance) (time.Time, string) {
		return s.CreatedAt, s.ID
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(page)
}

// handleGetInstance serves GET /v1/instances/{id}. Org-scoped via
// GetInstanceForOrg — cross-org access returns 404 (PRIV-08: must not
// leak instance existence to other tenants).
//
// ?include=credentials (GPU-59) opts in to connection.app_password on this
// single-instance read — the only public surface that returns it.
func (deps PublicDeps) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	apiKey := APIKeyFromContext(r.Context())
	if apiKey == nil {
		WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	var includeCredentials bool
	switch inc := r.URL.Query().Get("include"); inc {
	case "":
		// default: redacted
	case "credentials":
		includeCredentials = true
	default:
		WriteProblem(w, r, http.StatusBadRequest, "validation_failed",
			"Invalid include: must be credentials")
		return
	}
	id := r.PathValue("id")
	store := deps.resolveInstanceStore()
	inst, err := store.GetInstanceForOrg(r.Context(), id, apiKey.OrgID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			WriteProblem(w, r, http.StatusNotFound, "not_found", "Instance not found")
			return
		}
		slog.ErrorContext(r.Context(), "instances: get failed", "error", err.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to fetch instance")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(instanceToPublic(inst, deps.frpHost(), deps.appsHost(), includeCredentials))
}

// handlePatchInstance serves PATCH /v1/instances/{id}. Only the `name`
// field is mutable in v1. Org-scoped: missing-or-cross-org returns 404.
//
// Synchronous (no D-08 Operation-Id) — name updates are a single SQL
// UPDATE with no engine involvement.
func (deps PublicDeps) handlePatchInstance(w http.ResponseWriter, r *http.Request) {
	apiKey := APIKeyFromContext(r.Context())
	if apiKey == nil {
		WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	id := r.PathValue("id")
	var req PatchInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "validation_failed", "Invalid JSON body")
		return
	}
	store := deps.resolveInstanceStore()
	// Existence + org-scope check first so we return 404 on missing
	// instances rather than silently no-op'ing the UPDATE. The instance is
	// captured so the D-04 member-scoping rule below can compare its owner.
	existing, err := store.GetInstanceForOrg(r.Context(), id, apiKey.OrgID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			WriteProblem(w, r, http.StatusNotFound, "not_found", "Instance not found")
			return
		}
		slog.ErrorContext(r.Context(), "instances: patch lookup failed", "error", err.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to update instance")
		return
	}

	// D-04: members update only instances they created — same single
	// authorize() rule and shadow-diff line as handleDeleteInstance below,
	// keeping the public surface in lockstep with the dashboard's
	// handleUpdateInstance gate (surfaces must not diverge per action).
	enforce := deps.Config != nil && deps.Config.AuthzEnforce
	principal := authz.PrincipalFromAPIKey(apiKey.UserID, apiKey.Role)
	decision := authz.Authorize(principal, authz.ActionUpdate, authz.Resource{OwnerUserID: existing.UserID}, enforce)
	slog.InfoContext(r.Context(), authz.DecisionLogMsg,
		slog.String("user_id", apiKey.UserID),
		slog.String("role", apiKey.Role),
		slog.String("action", authz.ActionUpdate),
		slog.String("resource", "instance/"+id),
		slog.Bool("would_deny", !decision.Allowed),
		slog.Bool("enforce", decision.Enforce),
		slog.String("reason", decision.Reason),
		slog.String("org_id", apiKey.OrgID),
		slog.String("api_key_id", apiKey.APIKeyID),
	)
	if decision.Enforce && !decision.Allowed {
		WriteProblem(w, r, http.StatusForbidden, "forbidden", "You can only manage instances you created")
		return
	}

	if err := store.UpdateInstanceName(r.Context(), id, apiKey.OrgID, req.Name); err != nil {
		slog.ErrorContext(r.Context(), "instances: update name failed", "error", err.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to update instance")
		return
	}
	// Re-read so the response reflects the new name + post-update fields.
	inst, err := store.GetInstanceForOrg(r.Context(), id, apiKey.OrgID)
	if err != nil {
		slog.ErrorContext(r.Context(), "instances: re-read after patch failed", "error", err.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to update instance")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// GPU-59: PATCH responses never carry the password (opt-in is on GET only).
	_ = json.NewEncoder(w).Encode(instanceToPublic(inst, deps.frpHost(), deps.appsHost(), false))
}

// handleDeleteInstance serves DELETE /v1/instances/{id}. D-08 async write:
// returns 202 + Operation-Id (NOT 204) — the actual termination happens
// in a goroutine; clients poll the operation. D-07: returns 404 on
// already-gone (or cross-org) instances so clients can distinguish "did
// not exist" from "just deleted".
func (deps PublicDeps) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	apiKey := APIKeyFromContext(r.Context())
	if apiKey == nil {
		WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	id := r.PathValue("id")
	instStore := deps.resolveInstanceStore()
	// Existence + org-scope check (D-07) BEFORE creating the operation —
	// otherwise a 404 would leave a "pending instance.delete" row that
	// no one ever transitions. The instance is captured so the SEC-05
	// member-scoping rule below can compare its owner.
	inst, err := instStore.GetInstanceForOrg(r.Context(), id, apiKey.OrgID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			WriteProblem(w, r, http.StatusNotFound, "not_found", "Instance not found")
			return
		}
		slog.ErrorContext(r.Context(), "instances: delete lookup failed", "error", err.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to delete instance")
		return
	}

	// SEC-05 / D-04: route the terminate decision through the single
	// authorize() rule (internal/authz) — members terminate only instances
	// they created (instance.user_id); admin+ terminates org-wide. The
	// principal's role is the DB role the auth middleware attached to the
	// key context (SEC-01); fail-closed to member.
	enforce := deps.Config != nil && deps.Config.AuthzEnforce
	principal := authz.PrincipalFromAPIKey(apiKey.UserID, apiKey.Role)
	decision := authz.Authorize(principal, authz.ActionTerminate, authz.Resource{OwnerUserID: inst.UserID}, enforce)
	// Structured decision diff line (SEC-07). shadowLogAuthz is a *Server
	// method on the session surface and unreachable from PublicDeps, so the
	// same field set is emitted inline here; Plans 06/07 grep this exact
	// message + would_deny/enforce fields during the shadow soak.
	slog.InfoContext(r.Context(), authz.DecisionLogMsg,
		slog.String("user_id", apiKey.UserID),
		slog.String("role", apiKey.Role),
		slog.String("action", authz.ActionTerminate),
		slog.String("resource", "instance/"+id),
		slog.Bool("would_deny", !decision.Allowed),
		slog.Bool("enforce", decision.Enforce),
		slog.String("reason", decision.Reason),
		slog.String("org_id", apiKey.OrgID),
		slog.String("api_key_id", apiKey.APIKeyID),
	)
	if decision.Enforce && !decision.Allowed {
		WriteProblem(w, r, http.StatusForbidden, "forbidden", "You can only terminate instances you created")
		return
	}

	// Snapshot the resource_id into operation.metadata so future audit
	// queries can reconstruct "which instance was this?" even after the
	// instances row is hard-deleted.
	meta, mErr := json.Marshal(map[string]string{"instance_id": id})
	if mErr != nil {
		slog.ErrorContext(r.Context(), "instances: marshal delete snapshot failed", "error", mErr.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to delete instance")
		return
	}

	opStore := deps.resolveOperationStore()
	op := &db.Operation{
		APIKeyID: apiKey.APIKeyID,
		OrgID:    apiKey.OrgID,
		Kind:     "instance.delete",
		Metadata: meta,
	}
	if err := opStore.CreateOperation(r.Context(), op); err != nil {
		slog.ErrorContext(r.Context(), "instances: create delete operation failed", "error", err.Error())
		WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Failed to delete instance")
		return
	}

	// SEC-09: actor-attributed terminate audit on the public surface,
	// emitted SYNCHRONOUSLY at the point the terminate operation is created
	// (every allowed path — admin org-wide, member-on-own, and the
	// shadow-mode would-deny-but-proceeds case). source="user_api" keeps it
	// distinguishable from the dashboard emit (source="user") and the
	// system cap-kill (nil actor). The actual engine terminate runs async in
	// runDeleteInstance and can still fail — phase="requested" marks this row
	// as recording the authorized request, not a confirmed teardown (the
	// dashboard surface audits post-success and carries no phase marker).
	// Non-fatal: an audit write failure must not fail the terminate,
	// mirroring internal/api logAudit's stance.
	if store := deps.resolveAuditStore(); store != nil {
		actorID := apiKey.UserID
		resourceType := "instance"
		resourceID := id
		auditMeta, aErr := json.Marshal(map[string]any{"source": "user_api", "phase": "requested"})
		if aErr != nil {
			auditMeta = nil
		}
		entry := &db.AuditEntry{
			OrgID:        apiKey.OrgID,
			UserID:       &actorID,
			Action:       "instance.terminated",
			ResourceType: &resourceType,
			ResourceID:   &resourceID,
			Metadata:     auditMeta,
		}
		if aErr := store.CreateAuditEntry(r.Context(), entry); aErr != nil {
			slog.WarnContext(r.Context(), "instances: terminate audit write failed",
				"action", "instance.terminated", "error", aErr.Error())
		}
	} else {
		slog.WarnContext(r.Context(), "instances: no audit store wired; terminate audit skipped",
			"action", "instance.terminated")
	}

	// Pre-attach the resource id so /v1/operations/{id} surfaces it from
	// the moment the 202 lands — clients don't have to wait for the
	// engine to call back.
	if newV, err := opStore.SetOperationResource(r.Context(), op.OperationID, op.Version, id); err != nil {
		slog.ErrorContext(r.Context(), "instances: pre-attach resource_id conflict (FSM corrupted)",
			"operation_id", op.OperationID, "error", err.Error())
		// Continue anyway — the goroutine will populate state. The 202
		// has already been "earned" since the operation row exists.
	} else {
		op.Version = newV
		op.ResourceID = &id
	}

	engine := deps.resolveEngineRunner()
	go runDeleteInstance(opStore, engine, op, id)

	w.Header().Set("Operation-Id", op.OperationID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(operationToResponse(op))
}

// runDeleteInstance drives the delete-instance FSM in a goroutine.
// Same MAJOR 13 contract as runCreateInstance — every UPDATE call
// reassigns op.Version from the RETURNING-derived newV.
func runDeleteInstance(store operationStore, engine engineRunner, op *db.Operation, instanceID string) {
	bgCtx, cancel := context.WithTimeout(context.Background(), engineGoroutineTimeout)
	defer cancel()

	// pending -> in_progress.
	newV, err := store.UpdateOperationState(bgCtx, op.OperationID, op.Version, "in_progress")
	if err != nil {
		slog.ErrorContext(bgCtx, "instances: delete in_progress transition failed (FSM corrupted)",
			"operation_id", op.OperationID, "error", err.Error())
		return
	}
	op.Version = newV

	if terr := engine.Terminate(bgCtx, instanceID); terr != nil {
		slog.ErrorContext(bgCtx, "instances: engine.Terminate failed",
			"operation_id", op.OperationID, "instance_id", instanceID, "error", terr.Error())
		newV, ferr := store.SetOperationFailure(bgCtx, op.OperationID, op.Version,
			mapEngineError(terr), terr.Error())
		if ferr != nil {
			slog.ErrorContext(bgCtx, "instances: SetOperationFailure conflict (FSM corrupted)",
				"operation_id", op.OperationID, "error", ferr.Error())
			return
		}
		op.Version = newV
		return
	}

	newV, err = store.UpdateOperationState(bgCtx, op.OperationID, op.Version, "succeeded")
	if err != nil {
		slog.ErrorContext(bgCtx, "instances: delete succeeded transition failed (FSM corrupted)",
			"operation_id", op.OperationID, "error", err.Error())
		return
	}
	op.Version = newV
}

// scrubReasonPtr applies the provider-brand scrubber to an optional
// customer-visible reason, preserving nil.
func scrubReasonPtr(r *string) *string {
	if r == nil {
		return nil
	}
	s := scrubProviderNames(*r)
	return &s
}

// InstanceHandlersForTesting registers the 5 instance handlers + 1
// operations handler WITHOUT the auth/rate-limit/idempotency chain.
// Tests inject an APIKey on the request context directly (via
// WithAPIKey) and exercise handler logic in isolation. Same contract as
// HandlersForTesting in the SSH-key handlers file.
//
// Production code MUST NOT call this — it bypasses the very middleware
// that enforces tenancy and rate limits.
func InstanceHandlersForTesting(mux *http.ServeMux, deps PublicDeps) {
	mux.Handle("GET /v1/instances", http.HandlerFunc(deps.handleListInstances))
	mux.Handle("GET /v1/instances/{id}", http.HandlerFunc(deps.handleGetInstance))
	mux.Handle("POST /v1/instances", http.HandlerFunc(deps.handleCreateInstance))
	mux.Handle("PATCH /v1/instances/{id}", http.HandlerFunc(deps.handlePatchInstance))
	mux.Handle("DELETE /v1/instances/{id}", http.HandlerFunc(deps.handleDeleteInstance))
	mux.Handle("GET /v1/operations/{id}", http.HandlerFunc(deps.handleGetOperation))
}

// RunCreateInstanceForTesting is the test-only synchronous entry point
// for the create-instance goroutine driver. Tests call this in place of
// `go runCreateInstance(...)` so they can deterministically observe FSM
// transitions and conflict-abort behavior (see
// TestRunCreateInstance_FSMVersionDriftAborts). Production code MUST
// use handleCreateInstance, which fires the goroutine itself.
func RunCreateInstanceForTesting(store operationStore, engine engineRunner, op *db.Operation, req provision.ProvisionRequest) {
	runCreateInstance(store, engine, op, req)
}
