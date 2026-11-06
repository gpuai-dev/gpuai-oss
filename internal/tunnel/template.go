package tunnel

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

//go:embed templates/bootstrap.sh.tmpl
var bootstrapTmplStr string

var bootstrapTmpl = template.Must(
	template.New("bootstrap").Funcs(template.FuncMap{
		// gpuCheckSeconds renders the startup GPU check budget for a mode
		// from the Go constants, so the engine's tunnel-stall widening and
		// the bootstrap's own bound cannot drift apart.
		"gpuCheckSeconds": func(mode string) int {
			if mode == "enforce" {
				return GPUCheckEnforceSeconds
			}
			return GPUCheckShadowSeconds
		},
	}).Parse(bootstrapTmplStr),
)

var (
	// instanceIDRegex allows only alphanumeric characters and hyphens.
	instanceIDRegex = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

	// hostnameRegex allows only alphanumeric characters, hyphens, and dots.
	hostnameRegex = regexp.MustCompile(`^[a-zA-Z0-9.-]+$`)

	// shellInjectionChars are characters that could enable shell injection.
	shellInjectionChars = []string{"$(", "`", ";", "|", "&"}

	// pinnedImageRefRegex constrains the certified-VM PinnedImageRef to a
	// digest-pinned reference (T-63-02): a registry/repo path followed by a
	// mandatory "@sha256:<64 hex>" digest. By construction it admits no shell
	// metacharacters, so a manifest ref can never break out of the rendered
	// `docker run` invocation. There is NO customer-supplied image on this path.
	pinnedImageRefRegex = regexp.MustCompile(`^[a-z0-9./:@-]+@sha256:[a-f0-9]{64}$`)

	// customerImageRefRegex is pinnedImageRefRegex widened to the imageref
	// grammar's full charset (customer repos/tags may carry uppercase tags
	// and underscores) — still digest-pinned, still shell-meta-free by
	// construction (Phase 3).
	customerImageRefRegex = regexp.MustCompile(`^[a-zA-Z0-9._/:-]+@sha256:[a-f0-9]{64}$`)

	// driverFloorRegex constrains DriverFloor to a dotted numeric version like
	// "570.26" — it flows into a `sort -V` comparison in the rendered shell.
	driverFloorRegex = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

	// frpVersionRegex constrains FRPVersion to a dotted numeric version like
	// "0.67.0" — it is interpolated into shell variable assignments and the
	// GitHub release URL.
	frpVersionRegex = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

	// mirrorURLRegex constrains FRPCMirrorURL to an https URL built from
	// RFC 3986 characters MINUS single quote, backtick, backslash, and
	// whitespace. The URL renders inside a single-quoted shell word, where
	// the only escape hatch is a single quote — presigned object-store URLs (the only
	// producer) percent-encode everything outside this set anyway, so a
	// mismatch means a bug or tampering, not a legitimate URL.
	mirrorURLRegex = regexp.MustCompile(`^https://[A-Za-z0-9._~:/?#@!$&*+,;=%()\[\]-]+$`)
)

// ValidateBootstrapData validates all fields of BootstrapData, returning a
// combined error listing all validation failures.
func ValidateBootstrapData(data BootstrapData) error {
	var errs []string

	if data.InstanceID == "" {
		errs = append(errs, "InstanceID is required")
	} else if !instanceIDRegex.MatchString(data.InstanceID) {
		errs = append(errs, "InstanceID contains invalid characters")
	}

	if data.ProxyHost == "" {
		errs = append(errs, "ProxyHost is required")
	}

	if data.FRPServerPort <= 0 {
		errs = append(errs, "FRPServerPort must be positive")
	}

	if data.FRPToken == "" {
		errs = append(errs, "FRPToken is required")
	}

	// Checked against the CONFIGURED span, not the default constants: an
	// environment running a widened range would otherwise render a perfectly
	// good port and then reject its own bootstrap here.
	if allowed := data.AllowedPorts.OrDefault(); !allowed.Contains(data.RemotePort) {
		errs = append(errs, fmt.Sprintf("RemotePort must be in range [%d, %d]", allowed.Min, allowed.Max))
	}

	// FRPTransport is interpolated into the instance's frpc.toml, so it is
	// checked against frp's closed set rather than trusted. Config already
	// rejects a bad FRP_TRANSPORT_<NAME> at boot; this is the second gate on
	// the value's way into a file, so no future caller can route around it.
	switch data.FRPTransport {
	case "", "websocket", "wss", "kcp", "quic":
	default:
		errs = append(errs, fmt.Sprintf("FRPTransport %q is not a supported frp transport", data.FRPTransport))
	}

	// The compute-check fields are interpolated into shell, so they are checked
	// against closed sets too.
	switch data.GPUCheckMode {
	case "", "shadow", "enforce":
	default:
		errs = append(errs, fmt.Sprintf("GPUCheckMode %q is not one of shadow, enforce", data.GPUCheckMode))
	}
	switch data.ExpectFramework {
	case "", "pytorch":
	default:
		errs = append(errs, fmt.Sprintf("ExpectFramework %q is not a checkable framework", data.ExpectFramework))
	}
	if data.ExpectedGPUCount < 0 || data.ExpectedGPUCount > 64 {
		errs = append(errs, "ExpectedGPUCount must be between 0 and 64")
	}

	// Fleet-probe URL: a curl target inside single quotes, so it is held to an
	// https URL without shell metacharacters.
	if data.ProbeURL != "" {
		if !strings.HasPrefix(data.ProbeURL, "https://") {
			errs = append(errs, "ProbeURL must be an https URL")
		}
		for _, ch := range shellInjectionChars {
			if strings.Contains(data.ProbeURL, ch) {
				errs = append(errs, fmt.Sprintf("ProbeURL contains forbidden character %q", ch))
				break
			}
		}
	}

	// Empty is legal: a keyless launch (web-console-only access, the engine
	// gates it on webTerminalOn) renders an empty authorized_keys — sshd stays
	// up with nothing accepted, which is exactly the customer's choice. The
	// per-line injection scan below still guards every non-empty value.
	if data.SSHAuthorizedKeys != "" {
		lines := strings.Split(data.SSHAuthorizedKeys, "\n")
		for i, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			for _, ch := range shellInjectionChars {
				if strings.Contains(line, ch) {
					errs = append(errs, fmt.Sprintf("SSHAuthorizedKeys line %d contains forbidden character %q", i+1, ch))
					break
				}
			}
		}
	}

	if data.InternalToken == "" {
		errs = append(errs, "InternalToken is required")
	}

	if data.Hostname == "" {
		errs = append(errs, "Hostname is required")
	} else if !hostnameRegex.MatchString(data.Hostname) {
		errs = append(errs, "Hostname contains invalid characters")
	}

	if data.CallbackURL == "" {
		errs = append(errs, "CallbackURL is required")
	}

	// TelemetryURL (TELEM-01) is OPTIONAL: empty => the recurring nvidia-smi
	// sampler block renders nothing (raw bootstrap unchanged), so an absent URL
	// must never fail validation. When present it is rendered into the bootstrap
	// shell (a curl target), so scan it for shell-injection metacharacters the
	// same way the SSH-key / app-cred fields are guarded — a forged URL must not
	// be able to break out of the curl invocation.
	if data.TelemetryURL != "" {
		for _, ch := range shellInjectionChars {
			if strings.Contains(data.TelemetryURL, ch) {
				errs = append(errs, fmt.Sprintf("TelemetryURL contains forbidden character %q", ch))
				break
			}
		}
	}

	// CustomStartB64 (custom-images Phase 1) is echoed into `base64 -d`, so
	// the only thing that must hold is that it IS pure base64 — decode it
	// rather than pattern-match it. The decoded script was rendered
	// server-side (provision.customStartB64) with per-argument quoting; the
	// b64 envelope is what keeps customer-derived argv bytes out of the
	// template's shell source.
	if data.CustomStartB64 != "" {
		if _, err := base64.StdEncoding.DecodeString(data.CustomStartB64); err != nil {
			errs = append(errs, "CustomStartB64 is not valid base64")
		}
	}

	// Customer-image-on-VM fields (Phase 3). The ref is interpolated into the
	// bootstrap (a shell context), so it must satisfy the imageref grammar's
	// charset AND carry the digest pin — the same posture PinnedImageRef gets,
	// widened only to allow the registry/tag prefix a customer ref carries.
	if data.CustomerImageRef != "" {
		if !customerImageRefRegex.MatchString(data.CustomerImageRef) {
			errs = append(errs, "CustomerImageRef must be a digest-pinned image reference")
		}
		if data.DeploymentType != "vm" {
			errs = append(errs, "CustomerImageRef requires DeploymentType \"vm\"")
		}
	}
	for name, v := range map[string]string{
		"CustomerEntrypointB64": data.CustomerEntrypointB64,
		"CustomerArgsB64":       data.CustomerArgsB64,
		"CustomerEnvB64":        data.CustomerEnvB64,
	} {
		if v == "" {
			continue
		}
		if data.CustomerImageRef == "" {
			errs = append(errs, name+" requires CustomerImageRef")
			continue
		}
		if _, err := base64.StdEncoding.DecodeString(v); err != nil {
			errs = append(errs, name+" is not valid base64")
		}
	}
	for _, p := range data.CustomerPorts {
		if p < 1 || p > 65535 || p == 22 {
			errs = append(errs, fmt.Sprintf("CustomerPorts entry %d is invalid", p))
		}
	}

	// Phase 31 app-vhost fields. Only validated when an app port is requested;
	// AppPort==0 means no template app, nothing app-specific to validate.
	if data.AppPort != 0 {
		if data.AppPort < 1 || data.AppPort > 65535 {
			errs = append(errs, "AppPort must be in range [1, 65535]")
		}
		if data.AppSubdomain == "" {
			errs = append(errs, "AppSubdomain is required when AppPort is set")
		} else if !instanceIDRegex.MatchString(data.AppSubdomain) {
			errs = append(errs, "AppSubdomain contains invalid characters")
		}
	}

	// Web console (v76). Port range only; the shared-credential requirement is
	// enforced below for app and terminal alike.
	if data.TerminalPort != 0 && (data.TerminalPort < 1 || data.TerminalPort > 65535) {
		errs = append(errs, "TerminalPort must be in range [1, 65535]")
	}
	if data.TerminalPort != 0 && data.AppPort == data.TerminalPort {
		errs = append(errs, "TerminalPort must differ from AppPort")
	}

	// Basic-auth creds (D6) guard BOTH vhost proxies (template app + web
	// console) and are rendered into frpc.toml -- scan for shell injection
	// (defense-in-depth; the Plan 06 generator emits URL-safe base64).
	if data.AppPort != 0 || data.TerminalPort != 0 {
		for _, f := range []struct{ name, val string }{
			{"AppBasicAuthUser", data.AppBasicAuthUser},
			{"AppBasicAuthPass", data.AppBasicAuthPass},
		} {
			// MD-04: a vhost-proxied surface is NEVER exposed unauthenticated
			// (D6). Require both creds non-empty at the render boundary -- not
			// just by upstream caller convention -- because frp treats an empty
			// httpPassword as "no auth required", which would publicly expose
			// the app or a root shell (the exact S1 "RCE-as-a-service" surface
			// D6 was added to close).
			if f.val == "" {
				errs = append(errs, f.name+" is required when AppPort or TerminalPort is set")
				continue
			}
			for _, ch := range shellInjectionChars {
				if strings.Contains(f.val, ch) {
					errs = append(errs, fmt.Sprintf("%s contains forbidden character %q", f.name, ch))
					break
				}
			}
		}
	}

	// Phase 63 Docker-normalization fields. These are server-derived class
	// attributes / server-controlled manifest values (never customer input), but
	// they render into the bootstrap shell, so guard them at the render boundary.
	//
	// A certified VM MUST carry a valid digest-pinned image ref (T-63-02) — the
	// docker-run block would otherwise render an empty/mutable image. The regex
	// admits no shell metacharacters, so an injected "$(...)" ref is rejected
	// here rather than reaching `docker run` (T-63-06).
	if data.CertifiedImage {
		if data.PinnedImageRef == "" {
			errs = append(errs, "PinnedImageRef is required when CertifiedImage is set")
		} else if !pinnedImageRefRegex.MatchString(data.PinnedImageRef) {
			errs = append(errs, "PinnedImageRef must be a digest-pinned reference (…@sha256:<64 hex>) with no shell metacharacters")
		}
	}
	// DriverFloor flows into a `sort -V` comparison; when present it must be a
	// dotted numeric version (again, no shell metacharacters).
	if data.DriverFloor != "" && !driverFloorRegex.MatchString(data.DriverFloor) {
		errs = append(errs, "DriverFloor must be a dotted numeric version (e.g. 570.26)")
	}

	// FRPVersion is optional (RenderBootstrap defaults it to the package pin)
	// but when set it renders into shell assignments and a URL path.
	if data.FRPVersion != "" && !frpVersionRegex.MatchString(data.FRPVersion) {
		errs = append(errs, "FRPVersion must be a dotted numeric version (e.g. 0.67.0)")
	}

	// TtydVersion mirrors FRPVersion: optional (defaulted by RenderBootstrap),
	// rendered into a shell assignment and the GitHub release URL.
	if data.TtydVersion != "" && !frpVersionRegex.MatchString(data.TtydVersion) {
		errs = append(errs, "TtydVersion must be a dotted numeric version (e.g. 1.7.7)")
	}

	// FRPCMirrorURL is optional (empty => GitHub-only download). When present
	// it renders inside a single-quoted shell word in the bootstrap's source
	// array, so it must be an https URL with no quote/backtick/backslash/
	// whitespace characters. The engine only ever supplies presigned object-store URLs
	// here; this guard is the render-boundary backstop (same posture as
	// TelemetryURL above).
	if data.FRPCMirrorURL != "" && !mirrorURLRegex.MatchString(data.FRPCMirrorURL) {
		errs = append(errs, "FRPCMirrorURL must be an https URL without quote or whitespace characters")
	}

	// TtydMirrorURL: same producer (a presigned object-store URL) and render context as
	// FRPCMirrorURL, same guard.
	if data.TtydMirrorURL != "" && !mirrorURLRegex.MatchString(data.TtydMirrorURL) {
		errs = append(errs, "TtydMirrorURL must be an https URL without quote or whitespace characters")
	}

	if len(errs) > 0 {
		return fmt.Errorf("bootstrap data validation failed: %s", strings.Join(errs, "; "))
	}

	return nil
}

// RenderBootstrap renders the instance bootstrap script with the provided data.
// It validates inputs before rendering and returns the rendered script as a string.
func RenderBootstrap(data BootstrapData) (string, error) {
	// Default the frp release to the package pin so every caller downloads
	// the version the frps gateway speaks (types.go FRPVersion).
	if data.FRPVersion == "" {
		data.FRPVersion = FRPVersion
	}
	// Same for the web-console ttyd release.
	if data.TtydVersion == "" {
		data.TtydVersion = TtydVersion
	}
	if err := ValidateBootstrapData(data); err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := bootstrapTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("tunnel: render bootstrap template: %w", err)
	}

	return buf.String(), nil
}
