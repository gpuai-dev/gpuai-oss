// Multi-proxy bootstrap tests (Plan 31-01 scaffold, un-gated by Plan 31-04).
// Originally gated behind the template_wave_pending build tag until BootstrapData
// gained the app-proxy fields (Plan 04) and the bootstrap template grew a second
// [[proxies]] block. Plan 04 removed the build tag.
//
// References (Wave 1 production symbols this file pins):
//   - tunnel.BootstrapData.AppPort           int
//   - tunnel.BootstrapData.AppSubdomain      string
//   - tunnel.BootstrapData.AppBasicAuthUser  string
//   - tunnel.BootstrapData.AppBasicAuthPass  string
//
// The SSH proxy block always renders; the app (http) proxy block renders only
// when AppPort > 0. The env-injection guard extends ValidateBootstrapData to
// the new app fields.

package tunnel

import (
	"strings"
	"testing"
)

// baseBootstrap returns a BootstrapData with all pre-existing required fields
// populated, so ValidateBootstrapData passes once the app fields are added.
func baseBootstrap() BootstrapData {
	return BootstrapData{
		InstanceID:        "gpu-abcd1234",
		ProxyHost:         "203.0.113.10",
		FRPServerPort:     7000,
		FRPToken:          "frp-token",
		RemotePort:        10002,
		SSHAuthorizedKeys: "ssh-ed25519 AAAA test@example.com",
		InternalToken:     "internal-token",
		Hostname:          "gpu-abcd1234.example.com",
		CallbackURL:       "https://api.example.com/internal/instances/gpu-abcd1234/ready",
	}
}

// TestRenderBootstrapMultiProxy asserts an app-bearing BootstrapData renders
// BOTH the ssh proxy and the http app proxy blocks with the basic-auth creds.
func TestRenderBootstrapMultiProxy(t *testing.T) {
	data := baseBootstrap()
	data.AppPort = 8188
	data.AppSubdomain = "gpu-abcd1234"
	data.AppBasicAuthUser = "u"
	data.AppBasicAuthPass = "p"

	script, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap err = %v", err)
	}
	wants := []string{
		`name = "gpu-abcd1234-ssh"`,
		`name = "gpu-abcd1234-app"`,
		`type = "http"`,
		`subdomain = "gpu-abcd1234"`,
		`httpUser = "u"`,
		`httpPassword = "p"`,
	}
	for _, want := range wants {
		if !strings.Contains(script, want) {
			t.Errorf("rendered script missing %q\n---\n%s", want, script)
		}
	}
}

// TestRenderBootstrapSSHOnly asserts AppPort=0 renders only the ssh proxy (no
// http app block) — backward compatibility for raw (non-template) instances.
func TestRenderBootstrapSSHOnly(t *testing.T) {
	data := baseBootstrap()
	data.AppPort = 0

	script, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap err = %v", err)
	}
	if !strings.Contains(script, `name = "gpu-abcd1234-ssh"`) {
		t.Errorf("ssh proxy block missing: %s", script)
	}
	if strings.Contains(script, `type = "http"`) {
		t.Errorf("http app proxy block must NOT render when AppPort=0: %s", script)
	}
}

// TestRenderBootstrapReadyRecheck asserts the MD-03 late-readiness re-poll block
// renders for an app-bearing bootstrap (so a slow-booting app eventually flips
// booting->running via a re-POSTed app_ready=true callback) and is ABSENT for a
// raw SSH-only instance. The RAWREADY tunnel verify + re-probe blocks, by
// contrast, must render for BOTH kinds (every instance's running flip gates on
// tunnel_up).
func TestRenderBootstrapReadyRecheck(t *testing.T) {
	app := baseBootstrap()
	app.AppPort = 8188
	app.AppSubdomain = "gpu-abcd1234"
	app.AppBasicAuthUser = "u"
	app.AppBasicAuthPass = "p"
	script, err := RenderBootstrap(app)
	if err != nil {
		t.Fatalf("RenderBootstrap err = %v", err)
	}
	for _, want := range []string{"RECHECK_DEADLINE", "re-sending ready callback"} {
		if !strings.Contains(script, want) {
			t.Errorf("app bootstrap missing late-readiness re-poll marker %q", want)
		}
	}

	ssh := baseBootstrap()
	ssh.AppPort = 0
	sshScript, err := RenderBootstrap(ssh)
	if err != nil {
		t.Fatalf("RenderBootstrap(ssh) err = %v", err)
	}
	if strings.Contains(sshScript, "RECHECK_DEADLINE=") {
		t.Errorf("raw SSH-only bootstrap must NOT render the app re-poll block")
	}

	// RAWREADY: the tunnel verify probe and the late re-POST loop (late
	// tunnel OR undelivered initial callback -- READY_CB_OK) render for both
	// bootstrap kinds.
	for _, s := range []string{script, sshScript} {
		for _, want := range []string{"probe_tunnel", "TUNNEL_REPROBE_DEADLINE", "Tunnel reachable -- re-sending ready callback", "READY_CB_OK"} {
			if !strings.Contains(s, want) {
				t.Errorf("bootstrap missing tunnel verify/re-probe marker %q", want)
			}
		}
	}
}

// TestValidateRejectsEnvInjection asserts an app field carrying a shell
// command-substitution payload fails ValidateBootstrapData (the same blocklist
// already guarding SSHAuthorizedKeys must cover the new app fields).
func TestValidateRejectsEnvInjection(t *testing.T) {
	for _, payload := range []string{"abc`id`", "x$(whoami)"} {
		data := baseBootstrap()
		data.AppPort = 8188
		data.AppSubdomain = "gpu-abcd1234"
		data.AppBasicAuthUser = "u"
		data.AppBasicAuthPass = payload
		if err := ValidateBootstrapData(data); err == nil {
			t.Errorf("ValidateBootstrapData accepted injection payload %q; want error", payload)
		}
	}
}

// TestValidateRequiresBasicAuth asserts an app-bearing bootstrap with an empty
// basic-auth user or password is REJECTED at render time (MD-04). frp treats an
// empty httpPassword as "no auth required", so the guard must forbid it rather
// than trust the caller to populate it (D6: a template app is never exposed
// unauthenticated).
func TestValidateRequiresBasicAuth(t *testing.T) {
	mk := func(user, pass string) BootstrapData {
		d := baseBootstrap()
		d.AppPort = 8188
		d.AppSubdomain = "gpu-abcd1234"
		d.AppBasicAuthUser = user
		d.AppBasicAuthPass = pass
		return d
	}
	if err := ValidateBootstrapData(mk("", "p")); err == nil {
		t.Error("empty AppBasicAuthUser accepted; want error")
	}
	if err := ValidateBootstrapData(mk("u", "")); err == nil {
		t.Error("empty AppBasicAuthPass accepted; want error")
	}
	if err := ValidateBootstrapData(mk("u", "p")); err != nil {
		t.Errorf("valid creds rejected: %v", err)
	}
}
