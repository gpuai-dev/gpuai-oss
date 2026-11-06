package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// openTestServer serves a single instance whose connection block is supplied
// by the caller (nil means no connection at all). Mirroring the server
// contract, app_password is stripped from the response UNLESS the request
// carries ?include=credentials — so the with-creds tests below only pass when
// the CLI sends the opt-in. The last request query is recorded in *gotQuery.
func openTestServer(t *testing.T, connection map[string]any, gotQuery *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotQuery != nil {
			*gotQuery = r.URL.RawQuery
		}
		body := map[string]any{
			"id": "gpu-abc", "status": "running", "gpu_type": "l40",
			"gpu_count": 1, "tier": "on_demand", "region": "us-east",
			"price_per_hour": 0.99, "created_at": "2026-07-31T00:00:00Z",
		}
		if connection != nil {
			conn := map[string]any{}
			for k, v := range connection {
				if k == "app_password" && r.URL.Query().Get("include") != "credentials" {
					continue // password only on the opt-in read
				}
				conn[k] = v
			}
			body["connection"] = conn
		} else {
			body["status"] = "booting"
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
	t.Setenv("GPUAI_API_BASE", srv.URL)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return srv
}

func runOpen(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(append([]string{"instances", "open"}, args...))
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	err := root.Execute()
	return buf.String(), err
}

// TestInstancesOpen_LaunchesBrowserWithCreds asserts the happy path: the app
// URL is opened in the browser and the persisted login is printed.
func TestInstancesOpen_LaunchesBrowserWithCreds(t *testing.T) {
	var gotQuery string
	openTestServer(t, map[string]any{
		"hostname": "ssh.example.com", "port": 10042,
		"ssh_command":  "ssh -p 10042 root@ssh.example.com",
		"app_url":      "https://gpu-abc.apps.example.com",
		"app_user":     "gpuai",
		"app_password": "s3cret",
	}, &gotQuery)
	var opened string
	orig := execOpenBrowser
	execOpenBrowser = func(url string) error { opened = url; return nil }
	t.Cleanup(func() { execOpenBrowser = orig })

	out, err := runOpen(t, "gpu-abc")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if opened != "https://gpu-abc.apps.example.com" {
		t.Errorf("browser opened %q; want the app URL", opened)
	}
	for _, w := range []string{"https://gpu-abc.apps.example.com", "gpuai / s3cret"} {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q in: %s", w, out)
		}
	}
	// the CLI must fetch with the explicit credential opt-in.
	if !strings.Contains(gotQuery, "include=credentials") {
		t.Errorf("request query %q missing include=credentials opt-in", gotQuery)
	}
}

// TestInstancesOpen_PrintOnly asserts --print never launches a browser.
func TestInstancesOpen_PrintOnly(t *testing.T) {
	openTestServer(t, map[string]any{
		"hostname": "ssh.example.com", "port": 10042,
		"ssh_command": "ssh -p 10042 root@ssh.example.com",
		"app_url":     "https://gpu-abc.apps.example.com",
	}, nil)
	orig := execOpenBrowser
	execOpenBrowser = func(url string) error {
		t.Errorf("browser launched with %q despite --print", url)
		return nil
	}
	t.Cleanup(func() { execOpenBrowser = orig })

	out, err := runOpen(t, "--print", "gpu-abc")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// No persisted creds: says so instead of printing an empty login.
	if !strings.Contains(out, "not available for this instance") {
		t.Errorf("expected missing-credential hint, got: %s", out)
	}
}

// TestInstancesOpen_RawInstanceErrors asserts a raw (no app_url) instance gets
// a helpful error steering to ssh.
func TestInstancesOpen_RawInstanceErrors(t *testing.T) {
	openTestServer(t, map[string]any{
		"hostname": "ssh.example.com", "port": 10042,
		"ssh_command": "ssh -p 10042 root@ssh.example.com",
	}, nil)
	_, err := runOpen(t, "gpu-abc")
	if err == nil || !strings.Contains(err.Error(), "no app URL") {
		t.Fatalf("expected a no-app-URL error, got: %v", err)
	}
}

// TestInstancesOpen_Terminal asserts --terminal opens the web-console URL
// (not the app URL) with the same printed login.
func TestInstancesOpen_Terminal(t *testing.T) {
	var gotQuery string
	openTestServer(t, map[string]any{
		"hostname": "ssh.example.com", "port": 10042,
		"ssh_command":  "ssh -p 10042 root@ssh.example.com",
		"app_url":      "https://gpu-abc.apps.example.com",
		"terminal_url": "https://gpu-abc-term.apps.example.com",
		"app_user":     "gpuai",
		"app_password": "s3cret",
	}, &gotQuery)
	var opened string
	orig := execOpenBrowser
	execOpenBrowser = func(url string) error { opened = url; return nil }
	t.Cleanup(func() { execOpenBrowser = orig })

	out, err := runOpen(t, "--terminal", "gpu-abc")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if opened != "https://gpu-abc-term.apps.example.com" {
		t.Errorf("browser opened %q; want the console URL", opened)
	}
	for _, w := range []string{"https://gpu-abc-term.apps.example.com", "gpuai / s3cret"} {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q in: %s", w, out)
		}
	}
	if !strings.Contains(gotQuery, "include=credentials") {
		t.Errorf("request query %q missing include=credentials opt-in", gotQuery)
	}
}

// TestInstancesOpen_TerminalAbsentErrors asserts --terminal on a pre-console
// instance (no terminal_url) errors with the ssh fallback hint.
func TestInstancesOpen_TerminalAbsentErrors(t *testing.T) {
	openTestServer(t, map[string]any{
		"hostname": "ssh.example.com", "port": 10042,
		"ssh_command": "ssh -p 10042 root@ssh.example.com",
		"app_url":     "https://gpu-abc.apps.example.com",
	}, nil)
	_, err := runOpen(t, "--terminal", "gpu-abc")
	if err == nil || !strings.Contains(err.Error(), "no web console") {
		t.Fatalf("expected a no-web-console error, got: %v", err)
	}
}

// TestInstancesOpen_NotRunningErrors asserts an instance without connection
// info reports its status instead of a nil-pointer surprise.
func TestInstancesOpen_NotRunningErrors(t *testing.T) {
	openTestServer(t, nil, nil)
	_, err := runOpen(t, "gpu-abc")
	if err == nil || !strings.Contains(err.Error(), "no connection info") {
		t.Fatalf("expected a no-connection error, got: %v", err)
	}
}
