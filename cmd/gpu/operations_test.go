package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperationsGet_FetchOnce(t *testing.T) {
	var hits int
	var receivedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		receivedPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"operation_id": "op-123",
			"kind":         "instance.create",
			"state":        "running",
			"created_at":   "2026-07-29T00:00:00Z",
			"updated_at":   "2026-07-29T00:00:05Z",
		})
	}))
	defer srv.Close()
	setupCLITestEnv(t, srv.URL)

	root := newRootCmd()
	root.SetArgs([]string{"operations", "get", "op-123", "--output", "table"})
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	// A bare fetch reports whatever state it finds and exits 0 — even a
	// non-terminal one.
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if hits != 1 {
		t.Errorf("expected exactly one fetch with --wait-timeout unset, got %d", hits)
	}
	if receivedPath != "/operations/op-123" {
		t.Errorf("path: got %q", receivedPath)
	}
	for _, want := range []string{"op-123", "running"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output missing %q: %s", want, buf.String())
		}
	}
}

func TestOperationsGet_PollToFailed(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		body := map[string]any{
			"operation_id": "op-456",
			"kind":         "instance.create",
			"state":        "running",
			"created_at":   "2026-07-29T00:00:00Z",
			"updated_at":   "2026-07-29T00:00:05Z",
		}
		// Second read settles the operation — proving the CLI polled rather
		// than returning after the first fetch.
		if hits > 1 {
			body["state"] = "failed"
			body["error"] = map[string]string{
				"code":   "provision_failed",
				"detail": "no capacity in us-east",
			}
			body["completed_at"] = "2026-07-29T00:00:20Z"
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	setupCLITestEnv(t, srv.URL)

	root := newRootCmd()
	// PollOperation's first re-poll fires at 2s, so this settles in ~2s well
	// inside the deadline. The interval is fixed by the client.
	root.SetArgs([]string{"operations", "get", "op-456", "--wait-timeout", "30s", "--output", "table"})
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(outBuf)
	root.SetErr(errBuf)
	err := root.Execute()
	if err == nil {
		t.Fatal("expected a non-nil error (non-zero exit) for a failed operation")
	}
	if !strings.Contains(err.Error(), "operation failed") {
		t.Errorf("error should name the terminal state, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no capacity in us-east") {
		t.Errorf("error should carry error.detail, got: %v", err)
	}
	if hits < 2 {
		t.Errorf("expected at least two polls, got %d", hits)
	}
	// The operation is rendered before the failure return.
	if !strings.Contains(outBuf.String(), "failed") {
		t.Errorf("stdout should show the failed operation: %s", outBuf.String())
	}
	if !strings.Contains(outBuf.String(), "op-456") {
		t.Errorf("stdout should show the operation id: %s", outBuf.String())
	}
}
