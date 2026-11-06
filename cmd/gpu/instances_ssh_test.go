package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// TestInstancesSSH_PrintEmitsCommand asserts --print prints a correct ssh
// command line including all the option flags and copy-paste-safe quoting.
func TestInstancesSSH_PrintEmitsCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gpu-abc", "status": "running", "gpu_type": "h100_sxm",
			"gpu_count": 1, "tier": "on_demand", "region": "us-east",
			"price_per_hour": 2.5, "created_at": "2026-05-14T00:00:00Z",
			"connection": map[string]any{
				"hostname":    "ssh.example.com",
				"port":        10042,
				"ssh_command": "ssh -p 10042 root@ssh.example.com",
			},
		})
	}))
	defer srv.Close()
	t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
	t.Setenv("GPUAI_API_BASE", srv.URL)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := newRootCmd()
	root.SetArgs([]string{"instances", "ssh", "--print", "gpu-abc"})
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	wants := []string{
		"ssh",
		"-p 10042",
		"-o StrictHostKeyChecking=no",
		"-o UserKnownHostsFile=/dev/null",
		"-o LogLevel=ERROR",
		"-o ServerAliveInterval=60",
		"root@ssh.example.com",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q in: %s", w, out)
		}
	}
}

// TestInstancesSSH_ExecArgv replaces execSSH with a capturing fake to
// verify the EXACT argv passed to syscall.Exec. This is the only way to
// observe syscall.Exec behavior — the real call never returns.
func TestInstancesSSH_ExecArgv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gpu-abc", "status": "running", "gpu_type": "h100_sxm",
			"gpu_count": 1, "tier": "on_demand", "region": "us-east",
			"price_per_hour": 2.5, "created_at": "2026-05-14T00:00:00Z",
			"connection": map[string]any{
				"hostname": "ssh.example.com", "port": 10042,
				"ssh_command": "ssh -p 10042 root@ssh.example.com",
			},
		})
	}))
	defer srv.Close()
	t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
	t.Setenv("GPUAI_API_BASE", srv.URL)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var captured struct {
		host  string
		port  int
		extra []string
	}
	oldExec := execSSH
	execSSH = func(host string, port int, extraArgs []string) error {
		captured.host = host
		captured.port = port
		captured.extra = extraArgs
		return nil // pretend exec succeeded
	}
	defer func() { execSSH = oldExec }()

	root := newRootCmd()
	// Extra args after `--` should be forwarded to ssh.
	root.SetArgs([]string{"instances", "ssh", "gpu-abc", "--", "-L", "8888:localhost:8888"})
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if captured.host != "ssh.example.com" {
		t.Errorf("host: got %q, want ssh.example.com", captured.host)
	}
	if captured.port != 10042 {
		t.Errorf("port: got %d, want 10042", captured.port)
	}
	if !reflect.DeepEqual(captured.extra, []string{"-L", "8888:localhost:8888"}) {
		t.Errorf("extra args: got %v, want [-L 8888:localhost:8888]", captured.extra)
	}

	// Verify the buildSSHArgv helper produces the expected argv shape.
	argv := buildSSHArgv(captured.host, captured.port, captured.extra)
	wantArgv := []string{
		"ssh", "-p", "10042",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ServerAliveInterval=60",
		"root@ssh.example.com",
		"-L", "8888:localhost:8888",
	}
	if !reflect.DeepEqual(argv, wantArgv) {
		t.Errorf("argv mismatch:\ngot:  %v\nwant: %v", argv, wantArgv)
	}
}

// TestInstancesSSH_WaitsForRunning asserts the polling loop fires when
// status starts non-running and transitions to running. Counts API calls
// to verify polling happened.
func TestInstancesSSH_WaitsForRunning(t *testing.T) {
	var getCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&getCount, 1)
		status := "provisioning"
		var connection map[string]any
		if n >= 2 {
			status = "running"
			connection = map[string]any{
				"hostname": "ssh.example.com", "port": 10042,
				"ssh_command": "ssh -p 10042 root@ssh.example.com",
			}
		}
		resp := map[string]any{
			"id": "gpu-abc", "status": status, "gpu_type": "h100_sxm",
			"gpu_count": 1, "tier": "on_demand", "region": "us-east",
			"price_per_hour": 2.5, "created_at": "2026-05-14T00:00:00Z",
		}
		if connection != nil {
			resp["connection"] = connection
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
	t.Setenv("GPUAI_API_BASE", srv.URL)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	oldExec := execSSH
	execSSH = func(host string, port int, extraArgs []string) error { return nil }
	defer func() { execSSH = oldExec }()

	root := newRootCmd()
	// Short wait timeout so the test stays fast even if 5s sleep applies.
	root.SetArgs([]string{"instances", "ssh", "gpu-abc", "--wait-timeout=30s"})
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if atomic.LoadInt32(&getCount) < 2 {
		t.Errorf("expected ≥2 GetInstance calls (poll loop), got %d", getCount)
	}
}

// TestInstancesSSH_NoConnectionInfo asserts the friendly error when an
// instance is running but Connection is nil (no connection details from the supplier).
func TestInstancesSSH_NoConnectionInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gpu-abc", "status": "running", "gpu_type": "h100_sxm",
			"gpu_count": 1, "tier": "on_demand", "region": "us-east",
			"price_per_hour": 2.5, "created_at": "2026-05-14T00:00:00Z",
			// no "connection" field
		})
	}))
	defer srv.Close()
	t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
	t.Setenv("GPUAI_API_BASE", srv.URL)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := newRootCmd()
	root.SetArgs([]string{"instances", "ssh", "gpu-abc"})
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing connection info")
	}
	if !strings.Contains(err.Error(), "no connection info") {
		t.Errorf("expected a no-connection-info error, got: %v", err)
	}
}

// TestInstancesSSH_TerminatedInstanceFailsFast asserts that --wait does
// NOT block forever on terminated instances; it exits with a descriptive
// error immediately.
func TestInstancesSSH_TerminatedInstanceFailsFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gpu-abc", "status": "terminated", "gpu_type": "h100_sxm",
			"gpu_count": 1, "tier": "on_demand", "region": "us-east",
			"price_per_hour": 2.5, "created_at": "2026-05-14T00:00:00Z",
		})
	}))
	defer srv.Close()
	t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
	t.Setenv("GPUAI_API_BASE", srv.URL)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := newRootCmd()
	// The first GET returns "terminated"; --wait should fail fast.
	root.SetArgs([]string{"instances", "ssh", "gpu-abc", "--wait-timeout=30s"})
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for terminated instance")
	}
	if !strings.Contains(err.Error(), "terminated") || !strings.Contains(err.Error(), "cannot SSH") {
		t.Errorf("expected terminated error, got: %v", err)
	}
}
