package tunnel

import (
	"os"
	"os/exec"
	"testing"
)

// TestRenderBootstrapShellSyntax renders both bootstrap variants (raw SSH-only
// and app-bearing) and runs `bash -n` over the output. The template mixes Go
// template directives with non-trivial shell (probe functions, backgrounded
// subshell re-POST loops); a stray quote or unbalanced paren would otherwise
// only surface on a live instance, as a dead tunnel.
func TestRenderBootstrapShellSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	app := baseBootstrap()
	app.AppPort = 8188
	app.AppSubdomain = "gpu-abcd1234"
	app.AppBasicAuthUser = "u"
	app.AppBasicAuthPass = "p"

	ssh := baseBootstrap()
	ssh.AppPort = 0

	// Mirror variant: a presigned-URL-shaped mirror (query string with & and %)
	// rendered into the FRPC_URLS array must not break shell parsing.
	mirror := baseBootstrap()
	mirror.FRPCMirrorURL = "https://mirror.example.com/frp/frp_0.67.0_linux_amd64.tar.gz?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=3600&X-Amz-Signature=ab%2Fc"

	// Console variant: app + web terminal in one render (v76) — the ttyd install,
	// console-entry heredoc, and supervised launch are all new shell.
	term := baseBootstrap()
	term.AppPort = 8188
	term.AppSubdomain = "gpu-abcd1234"
	term.TerminalPort = 7681
	term.AppBasicAuthUser = "u"
	term.AppBasicAuthPass = "p"
	term.TtydMirrorURL = "https://mirror.example.com/ttyd/ttyd_1.7.7_x86_64?X-Amz-Expires=3600&X-Amz-Signature=ab%2Fc"

	// Certified-VM variant (GPU-57): section 7b's docker-run block, the
	// liveness poll, and the conditional frpc SSH proxy are all new shell.
	certified := baseBootstrap()
	certified.CertifiedImage = true
	certified.DeploymentType = "vm"
	certified.PinnedImageRef = "registry.example.com/certified@sha256:6422f5ac0000000000000000000000000000000000000000000000000000dead"
	certified.DriverFloor = "550"

	// Customer-image-on-VM variant (Phase 3): section 7c's docker-run block,
	// the b64 argv/env plumbing, and the inspect brace assembly are all new
	// shell.
	customerVM := baseBootstrap()
	customerVM.DeploymentType = "vm"
	customerVM.DriverFloor = "550"
	customerVM.CustomerImageRef = "ghcr.io/acme/trainer:v1@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	customerVM.CustomerEntrypointB64 = "cHl0aG9u"
	customerVM.CustomerArgsB64 = "LS1wb3J0"
	customerVM.CustomerEnvB64 = "TU9ERUw9YQo="
	customerVM.CustomerPorts = []int{8000}
	customerVM.AppPort = 8000
	customerVM.AppSubdomain = "gpu-abcd1234"
	customerVM.AppBasicAuthUser = "u"
	customerVM.AppBasicAuthPass = "p"

	// Startup GPU compute check variants: the embedded check script, the
	// retry loop and the enforce-mode deferred callback are all new shell.
	gpuCheck := baseBootstrap()
	gpuCheck.GPUCheckMode = "enforce"
	gpuCheck.ExpectedGPUCount = 8
	gpuCheck.ExpectFramework = "pytorch"
	gpuCheckCertified := certified
	gpuCheckCertified.GPUCheckMode = "enforce"
	gpuCheckCertified.ExpectedGPUCount = 1
	gpuCheckCustomer := customerVM
	gpuCheckCustomer.GPUCheckMode = "shadow"
	// Fleet-probe launches: the probe download, run and report post.
	probe := gpuCheckCertified
	probe.ProbeURL = "https://api.example.com/internal/instances/gpu-abcd1234/probe"

	for name, data := range map[string]BootstrapData{"app": app, "ssh-only": ssh, "mirror": mirror, "terminal": term, "certified": certified, "customer-vm": customerVM,
		"gpu-check": gpuCheck, "gpu-check-certified": gpuCheckCertified, "gpu-check-customer-vm": gpuCheckCustomer, "fleet-probe": probe} {
		script, err := RenderBootstrap(data)
		if err != nil {
			t.Fatalf("RenderBootstrap(%s) err = %v", name, err)
		}
		f, err := os.CreateTemp(t.TempDir(), "bootstrap-*.sh")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(script); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if out, err := exec.Command("bash", "-n", f.Name()).CombinedOutput(); err != nil {
			t.Errorf("bash -n rejected the %s bootstrap: %v\n%s", name, err, out)
		}
	}
}
