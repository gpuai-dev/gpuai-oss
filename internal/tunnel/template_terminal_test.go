// Web-console (ttyd browser terminal, schema v76) bootstrap tests.
//
// TerminalPort > 0 renders: the ttyd download (mirror-then-GitHub), the
// per-connection console entry script, a supervised ttyd bound to the
// loopback, and a THIRD [[proxies]] block at subdomain <InstanceID>-term
// guarded by the same basic-auth credential as the template app. Terminal
// and app are independent: either, both, or neither may render.

package tunnel

import (
	"strings"
	"testing"
)

// TestRenderBootstrapTerminal asserts a console-bearing BootstrapData renders
// the ttyd install + supervision and the -term http proxy with the creds.
func TestRenderBootstrapTerminal(t *testing.T) {
	data := baseBootstrap()
	data.TerminalPort = 7681
	data.AppBasicAuthUser = "u"
	data.AppBasicAuthPass = "p"

	script, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap err = %v", err)
	}
	wants := []string{
		`name = "gpu-abcd1234-term"`,
		`subdomain = "gpu-abcd1234-term"`,
		`localPort = 7681`,
		`httpUser = "u"`,
		`httpPassword = "p"`,
		// Pinned release in the GitHub fallback URL (TtydVersion defaulted by
		// RenderBootstrap).
		"https://github.com/tsl0922/ttyd/releases/download/${TTYD_VERSION}/ttyd.x86_64",
		`TTYD_VERSION="` + TtydVersion + `"`,
		// Loopback-only bind: the authenticated frps vhost is the only way in.
		"--interface 127.0.0.1",
		"/usr/local/bin/gpuai-console.sh",
		"gpuai-ttyd.service",
	}
	for _, want := range wants {
		if !strings.Contains(script, want) {
			t.Errorf("rendered script missing %q", want)
		}
	}
}

// TestRenderBootstrapTerminalWithApp asserts app + console coexist: both http
// proxy blocks render, each on its own subdomain, sharing one credential.
func TestRenderBootstrapTerminalWithApp(t *testing.T) {
	data := baseBootstrap()
	data.AppPort = 8888
	data.AppSubdomain = "gpu-abcd1234"
	data.TerminalPort = 7681
	data.AppBasicAuthUser = "u"
	data.AppBasicAuthPass = "p"

	script, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap err = %v", err)
	}
	for _, want := range []string{
		`name = "gpu-abcd1234-app"`,
		`name = "gpu-abcd1234-term"`,
		`subdomain = "gpu-abcd1234"`,
		`subdomain = "gpu-abcd1234-term"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("rendered script missing %q", want)
		}
	}
}

// TestRenderBootstrapNoTerminal asserts TerminalPort=0 renders no trace of the
// console (legacy render unchanged — the kill-switch/pre-v76 path).
func TestRenderBootstrapNoTerminal(t *testing.T) {
	data := baseBootstrap()

	script, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap err = %v", err)
	}
	for _, banned := range []string{"ttyd", "-term", "gpuai-console.sh"} {
		if strings.Contains(script, banned) {
			t.Errorf("terminal-less bootstrap must not contain %q", banned)
		}
	}
}

// TestRenderBootstrapTerminalMirror asserts the presigned mirror URL is tried
// (rendered into the source array) when provided.
func TestRenderBootstrapTerminalMirror(t *testing.T) {
	data := baseBootstrap()
	data.TerminalPort = 7681
	data.AppBasicAuthUser = "u"
	data.AppBasicAuthPass = "p"
	data.TtydMirrorURL = "https://assets.example.com/ttyd/ttyd_1.7.7_x86_64?X-Amz-Signature=abc"

	script, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap err = %v", err)
	}
	if !strings.Contains(script, data.TtydMirrorURL) {
		t.Errorf("rendered script missing ttyd mirror URL")
	}
}

// TestValidateTerminal asserts the render-boundary guards for the console
// fields: MD-04 credential requirement (frp treats an empty httpPassword as
// "no auth" — an unauthenticated root shell), port range, the app-port
// collision, and the mirror-URL / version character guards.
func TestValidateTerminal(t *testing.T) {
	mk := func(mut func(*BootstrapData)) BootstrapData {
		d := baseBootstrap()
		d.TerminalPort = 7681
		d.AppBasicAuthUser = "u"
		d.AppBasicAuthPass = "p"
		if mut != nil {
			mut(&d)
		}
		return d
	}

	if err := ValidateBootstrapData(mk(nil)); err != nil {
		t.Errorf("valid terminal bootstrap rejected: %v", err)
	}
	if err := ValidateBootstrapData(mk(func(d *BootstrapData) { d.AppBasicAuthPass = "" })); err == nil {
		t.Error("terminal without basic-auth password accepted; want error (MD-04)")
	}
	if err := ValidateBootstrapData(mk(func(d *BootstrapData) { d.AppBasicAuthUser = "" })); err == nil {
		t.Error("terminal without basic-auth user accepted; want error (MD-04)")
	}
	if err := ValidateBootstrapData(mk(func(d *BootstrapData) { d.TerminalPort = 70000 })); err == nil {
		t.Error("out-of-range TerminalPort accepted; want error")
	}
	if err := ValidateBootstrapData(mk(func(d *BootstrapData) {
		d.AppPort = 7681
		d.AppSubdomain = "gpu-abcd1234"
	})); err == nil {
		t.Error("AppPort == TerminalPort accepted; want error")
	}
	if err := ValidateBootstrapData(mk(func(d *BootstrapData) { d.TtydVersion = "1.7.7`id`" })); err == nil {
		t.Error("shell-meta TtydVersion accepted; want error")
	}
	if err := ValidateBootstrapData(mk(func(d *BootstrapData) { d.TtydMirrorURL = "https://a.example.com/x'y" })); err == nil {
		t.Error("quoted TtydMirrorURL accepted; want error")
	}
}
