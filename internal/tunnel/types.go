// Package tunnel implements FRP-based SSH tunneling for GPU instances.
// It replaces the internal/wireguard package with a userspace TCP reverse
// proxy that works in unprivileged containers.
package tunnel

import "time"

// FRPVersion is the pinned frp release the bootstrap installs on every
// instance. Lockstep with the frps version gpuctl embeds (go.mod's
// github.com/fatedier/frp) and with the mirror object key
// frp/frp_<version>_linux_amd64.tar.gz in the bootstrap-assets bucket —
// bump all three in lockstep, and upload the new tarball to the bucket
// BEFORE deploying (the bootstrap falls back to the GitHub release URL if
// the mirror object is missing, so a late upload degrades, not breaks).
const FRPVersion = "0.67.0"

// Startup GPU compute check budgets (bootstrap section 8b), in seconds of wall
// time for the whole check, retries and their spacing included.
//
// Shadow runs once, after the ready callback and in the background, so it
// never delays readiness; its budget only bounds how long an observe-only
// check can hold a context on the customer's GPUs. Healthy hosts finished in
// 0 to 5 s on 29 live instances (2026-10-06), so 60 s is over 10x that.
//
// Enforce gates the ready callback, so it runs before it. It gets more room
// because a timeout there releases the host and relaunches elsewhere, and a
// host that is merely slow (cold torch import, eight CUDA contexts) must not
// be called hung.
const (
	GPUCheckShadowSeconds  = 60
	GPUCheckEnforceSeconds = 150
)

// GPUCheckEnforceMaxDelay is the most the enforce-mode check can hold back the
// ready callback: its whole budget, plus up to 10 s to kill a timed-out check
// left running inside a container and 5 s for the rest of the section. The
// engine widens its tunnel-stall window by this much, because that clock runs
// until the ready callback lands.
const GPUCheckEnforceMaxDelay = (GPUCheckEnforceSeconds + 15) * time.Second

// TtydVersion is the pinned ttyd release the bootstrap installs for the web
// console (browser terminal). Single static binary, x86_64 only. Mirror object
// key ttyd/ttyd_<version>_x86_64 in the bootstrap-assets bucket — like frpc,
// upload the binary BEFORE deploying a bump (the bootstrap falls back to the
// GitHub release URL when the mirror object is missing, so a late upload
// degrades, not breaks).
const TtydVersion = "1.7.7"

// BootstrapData holds all parameters for rendering the instance bootstrap script.
// The bootstrap script downloads frpc, configures SSH, and establishes a
// reverse tunnel back to the FRP server on the proxy.
type BootstrapData struct {
	InstanceID    string // e.g., "gpu-4a7f"
	ProxyHost     string // e.g., "203.0.113.10" (proxy server public IP)
	FRPServerPort int    // e.g., 7000
	FRPToken      string // per-instance auth token
	RemotePort    int    // e.g., 10002 (unique port for this instance)
	// AllowedPorts is the configured span RemotePort must fall inside. Zero
	// means DefaultPortRange, so a caller that predates the configurable range
	// still validates against the historical bounds.
	AllowedPorts      PortRange
	SSHAuthorizedKeys string // newline-separated SSH public keys
	InternalToken     string // per-instance callback auth token
	Hostname          string // e.g., "gpu-4a7f.example.com"
	CallbackURL       string // full URL: "https://api.example.com/internal/instances/{id}/ready"
	// TelemetryURL is the full URL the in-pod nvidia-smi sampler POSTs to on a
	// recurring 1-5 min cadence (TELEM-01):
	// "https://api.example.com/internal/instances/{id}/telemetry". Empty => the
	// sampler block renders nothing (raw bootstrap unchanged); the loop must
	// never crash bootstrap when absent.
	TelemetryURL string

	// FRPTransport overrides the frpc transport protocol for this instance
	// ("websocket", "wss", "kcp", "quic"). Empty (the norm) => frp's default
	// raw TCP and NO transport line in the rendered frpc.toml, so every
	// provider that doesn't opt in renders byte-identically to before this
	// field existed. Server-derived from FRP_TRANSPORT_<PROVIDER> — never
	// customer input — and validated against the closed set by
	// ValidateBootstrapData, because it is interpolated into a TOML file.
	//
	// Motivating case: a container runtime that registers the tunnel and
	// passes container->client data fine, but resets ANY client->container
	// payload on a raw tunnel.
	// Note kcp/quic ALSO need frps-side bind ports the gateway does not
	// currently open; websocket/wss ride the existing bindPort with no
	// gateway change.
	FRPTransport string

	// GPUCheckMode turns on the startup compute check: "" (off, the bootstrap
	// renders byte-identically to before), "shadow" (run in the background
	// after the ready callback and report it, never delaying or blocking
	// readiness) or "enforce" (run just before the ready callback; a failed
	// check defers the callback and exits 94 so the engine can release the
	// host and rescue the launch before billing starts). Server-derived from
	// PROVISION_GPU_CHECK; validated against the closed set.
	GPUCheckMode string
	// ExpectedGPUCount is the GPU count the customer is paying for. The check
	// fails when fewer devices are visible. Zero skips the count comparison.
	ExpectedGPUCount int
	// ExpectFramework names the framework the selected environment promises
	// ("pytorch" for certified:pytorch). Only a promised framework can fail the
	// check. Empty means none is promised: one that is present is still
	// exercised and reported, but neither its absence nor its failure blocks
	// (a failure reports gpu_check=warn).
	ExpectFramework string

	// ProbeURL is set only on fleet-probe launches (the internal prober
	// measuring an offering). The bootstrap downloads the on-host probe from
	// it (GET), runs it where a customer's workload would run, and POSTs the
	// JSON report back to it, both with the instance's callback token. The
	// probe is fetched rather than embedded because it is about 20 KB: inline
	// it would push a size-capped provider startup script (15,800 characters
	// on one marketplace) past its cap.
	// Empty (every customer launch) renders nothing.
	ProbeURL string

	// FRPVersion is the frp release the bootstrap downloads. Optional:
	// RenderBootstrap defaults it to the package-level FRPVersion pin, so
	// callers only set it to override in tests.
	FRPVersion string
	// FRPCMirrorURL is a short-lived presigned GET URL for the pinned frpc
	// tarball on the platform's bootstrap-assets bucket. Optional: empty =>
	// the bootstrap downloads from the upstream GitHub release only. When
	// present the bootstrap tries the mirror before GitHub on every retry
	// round — provider VM egress can be dark per-destination, so the two
	// sources fail independently. Presigned by the provision engine at render
	// time (internal/storage); never customer input.
	FRPCMirrorURL string

	// App-vhost proxy (Phase 31, TMPL-07). Zero/empty => no template app.
	AppPort          int    // primary HTTP port to vhost-proxy (0 = none)
	AppSubdomain     string // = InstanceID; frps prepends to subDomainHost -> <sub>.apps.example.com
	AppBasicAuthUser string // D6: per-instance generated; httpUser on the http proxies
	AppBasicAuthPass string // D6: SECRET -- scrubbed from logs/metadata (TMPL-13)
	AppReadyTimeout  int    // A5: per-template ready_timeout_seconds (default ~300)

	// Web console (browser terminal, schema v76). TerminalPort != 0 makes the
	// bootstrap install ttyd, serve it on 127.0.0.1:<port>, and add a second
	// http proxy at subdomain <InstanceID>-term guarded by the SAME
	// AppBasicAuth* credential as the template app (which is why the creds
	// above are now generated for every bootstrapped instance, not only
	// template deploys). 0 = no web console (legacy render, byte-identical).
	TerminalPort int
	// TtydVersion is the ttyd release the bootstrap downloads. Optional:
	// RenderBootstrap defaults it to the package-level TtydVersion pin.
	TtydVersion string
	// TtydMirrorURL is a short-lived presigned GET URL for the pinned ttyd
	// binary on the bootstrap-assets bucket. Optional: empty => GitHub-only
	// download, same posture as FRPCMirrorURL.
	TtydMirrorURL string

	// Docker-normalization fields (Phase 63). All are server-derived class
	// attributes or server-controlled manifest values — never customer input.
	//
	// CertifiedImage marks that this instance runs the GPU.ai Certified Image
	// (normalized environment). Drives the driver-floor + toolkit + docker-run
	// block in the certified-VM bootstrap path.
	CertifiedImage bool
	// DeploymentType is "vm" (we bootstrap the machine) or "container" (the
	// provider runs our image natively — the bootstrap must NEVER nest Docker).
	// "" marks a pre-field / unknown instance.
	DeploymentType string
	// PinnedImageRef is the digest-pinned image reference the certified-VM
	// bootstrap `docker run`s. It comes from the server-controlled manifest for
	// the placed GPU (V5) — it is NEVER derived from customer input, and it is a
	// digest ref (contains "@sha256:"), never a mutable tag.
	PinnedImageRef string
	// DriverFloor is the minimum NVIDIA driver version the certified image
	// requires, e.g. "570.26". The bootstrap compares the host driver against it
	// (sort -V) before installing the toolkit and running the pinned image.
	DriverFloor string

	// CustomStartB64 (custom-images Phase 1) is the base64 of a
	// server-rendered "#!/bin/sh\nexec '<argv>'…" script that launches a
	// customer image's own Entrypoint+Cmd after sshd + tunnel setup, in
	// container mode only. The argv derives from CUSTOMER-chosen image
	// contents, which is exactly why it travels pre-rendered and b64-encoded:
	// the template only ever echoes the blob through base64 -d into a file,
	// so no customer byte is interpolated into shell source
	// (ValidateBootstrapData enforces the b64 charset). "" => no launch block
	// (raw/template/certified renders are byte-for-byte unchanged).
	CustomStartB64 string

	// Customer-image-on-VM fields (custom images Phase 3, DeploymentType
	// "vm"). The bootstrap owns the HOST (sshd + tunnel), then docker-runs
	// the customer's image beside them — the certified-VM machinery pointed
	// at a customer ref. All server-derived from the create handler's
	// validated + pre-flight-verified request:
	//
	// CustomerImageRef is the DIGEST-PINNED customer ref (imageref grammar,
	// must contain "@sha256:<64hex>" — ValidateBootstrapData enforces both).
	// "" => no customer-image block renders.
	CustomerImageRef string
	// CustomerEntrypointB64 / CustomerArgsB64 are the effective
	// entrypoint/args argv, NUL-delimited and base64-enveloped so arbitrary
	// argument bytes never enter shell source (the bootstrap readarray-splits
	// them). "" => keep the image's own defaults for that half.
	CustomerEntrypointB64 string
	CustomerArgsB64       string
	// CustomerEnvB64 is the docker --env-file content (KEY=VALUE lines),
	// base64-enveloped. Values are control-byte-free by API validation, so
	// the line format is unbreakable. "" => no env file.
	CustomerEnvB64 string
	// CustomerPorts are the DECLARED container ports, published on the VM's
	// loopback only (-p 127.0.0.1:p:p) — the frpc app vhost serves the
	// primary (AppPort) and SSH tunneling reaches the rest; nothing is
	// exposed on the VM's public interface.
	CustomerPorts []int
}

// Default FRP remote port range. These are the FALLBACK bounds used when
// nothing is configured — the live span comes from FRP_ALLOW_PORTS via
// ParsePortRange, so that the allocator and the frps gateway allow-list are
// always the same string. Do not read these directly to decide whether a port
// is allocatable; read the configured PortRange.
const (
	MinPort = 10000
	MaxPort = 10255
)

// portLockID is a constant used for the PostgreSQL advisory lock
// to serialize port allocations. Represents "FRPPT" as a numeric constant.
const portLockID int64 = 0x4650525054
