package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/cliauth"
)

// TestMain keeps this package hermetic. Every test here drives the real CLI
// against an httptest.Server, so two things from the developer's shell must
// never leak in:
//
//   - A non-local API base. A test that forgets --api-base would otherwise
//     inherit GPUAI_API_BASE and talk to a real server, which can provision
//     real instances. Non-loopback values are refused, loudly.
//   - A real credential. cliauth.ResolveToken falls back to
//     ~/.config/gpu/credentials.json, so XDG_CONFIG_HOME and HOME are pointed
//     at an empty temp dir and GPUAI_API_KEY is unset.
//
// Tests that want a key set one with t.Setenv; tests that want a base pass
// --api-base or set GPUAI_API_BASE themselves.
//
// Set GPUAI_ALLOW_LIVE_API_IN_TESTS=1 to skip both protections when you
// deliberately want to run against a live server.
func TestMain(m *testing.M) {
	if allowLive() {
		os.Exit(m.Run())
	}
	if err := refuseLiveEnvironment("GPUAI_API_BASE", os.Getenv("GPUAI_API_BASE")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "gpu-cli-test-config-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cmd/gpu tests: could not create a scratch config dir:", err)
		os.Exit(1)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	os.Setenv("HOME", dir)
	os.Unsetenv("GPUAI_API_KEY")

	// The api-base default is a local gpuctl on :8080, which may really be
	// running on a developer machine. Point the default at a port nothing
	// listens on so a test that forgets --api-base fails as connection-refused.
	os.Setenv("GPUAI_API_BASE", deadLoopbackBase)

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// deadLoopbackBase is a syntactically valid base that can never connect: port 1
// is reserved (tcpmux) and nothing in this repo binds it.
const deadLoopbackBase = "http://127.0.0.1:1/v1"

// allowLiveEnvVar is the deliberate escape hatch for running against a live
// server.
const allowLiveEnvVar = "GPUAI_ALLOW_LIVE_API_IN_TESTS"

// allowLive reads the escape hatch strictly: "0", "false" and anything
// unparseable mean off.
func allowLive() bool {
	v, err := strconv.ParseBool(os.Getenv(allowLiveEnvVar))
	return err == nil && v
}

// refuseLiveEnvironment fails the run when the ambient value points anywhere
// other than the local machine. Loopback is an allowlist: an httptest.Server
// is always 127.0.0.1.
func refuseLiveEnvironment(name, base string) error {
	base = strings.TrimSpace(base)
	if base == "" || isLoopbackBase(base) {
		return nil
	}
	return fmt.Errorf(`cmd/gpu tests refused to run: %s points at %q.

These tests drive the real CLI against an httptest.Server. With a live base
inherited from your shell, a test that forgets to pass --api-base would talk to
that server for real and could provision instances.

Run them without it:

    env -u %s go test ./cmd/gpu/...

or set %s=1 if you deliberately want a live run.`, name, base, name, allowLiveEnvVar)
}

// isLoopbackBase reports whether base names the local machine. A bare host:port
// with no scheme is accepted too, since url.Parse would otherwise read the host
// as a path and report no host at all.
func isLoopbackBase(base string) bool {
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func TestRefuseLiveEnvironment(t *testing.T) {
	allowed := []string{
		"",
		"   ",
		"http://127.0.0.1:53211/v1",
		"http://localhost:9090/v1",
		"https://localhost:9090/v1",
		"http://[::1]:9090/v1",
		"127.0.0.1:9090",
		"http://127.0.0.2:9090/v1", // the whole 127/8 block is loopback
	}
	for _, base := range allowed {
		if err := refuseLiveEnvironment("GPUAI_API_BASE", base); err != nil {
			t.Errorf("refuseLiveEnvironment(%q) refused a local base: %v", base, err)
		}
	}

	refused := []string{
		"https://gpu.example.com/v1",
		"  https://gpu.example.com/v1  ",
		"gpu.example.com",
		"http://192.168.1.10:9090/v1", // a LAN gpuctl is still not this process's
		"https://localhost.evil.example.com/v1",
	}
	for _, base := range refused {
		if err := refuseLiveEnvironment("GPUAI_API_BASE", base); err == nil {
			t.Errorf("refuseLiveEnvironment(%q) allowed a non-local base", base)
		}
	}
}

// TestCredentialFallbackIsSevered asserts the outcome of the credential half
// of the gate: with no GPUAI_API_KEY, no token resolves.
func TestCredentialFallbackIsSevered(t *testing.T) {
	if allowLive() {
		t.Skip("live mode: the credential is deliberately left intact")
	}
	if v := os.Getenv("GPUAI_API_KEY"); v != "" {
		t.Fatalf("GPUAI_API_KEY is set in-package (%d chars), TestMain should have unset it", len(v))
	}
	tok, src, err := cliauth.ResolveToken()
	if err == nil {
		t.Fatalf("ResolveToken succeeded from source %q with a %d-char token", src, len(tok))
	}
}

// setupCLITestEnv points the CLI at a stub server with a resolvable token and
// an isolated config dir.
func setupCLITestEnv(t *testing.T, baseURL string) {
	t.Helper()
	t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
	t.Setenv("GPUAI_API_BASE", baseURL)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}
