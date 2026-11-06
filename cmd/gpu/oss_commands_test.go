package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubAPI records the last request and answers every call with body.
func stubAPI(t *testing.T, status int, body any) (*httptest.Server, *http.Request, *map[string]any) {
	t.Helper()
	var got http.Request
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	setupCLITestEnv(t, srv.URL+"/v1")
	return srv, &got, &gotBody
}

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(args)
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	err := root.Execute()
	return buf.String(), err
}

func TestKeysGet(t *testing.T) {
	_, req, _ := stubAPI(t, http.StatusOK, map[string]any{
		"id": "key-1", "name": "laptop", "fingerprint": "SHA256:abc", "created_at": "2026-05-14T00:00:00Z",
	})
	out, err := runCLI(t, "keys", "get", "key-1", "-o", "json")
	if err != nil {
		t.Fatalf("execute: %v (%s)", err, out)
	}
	if req.Method != http.MethodGet || req.URL.Path != "/v1/ssh-keys/key-1" {
		t.Errorf("request: %s %s", req.Method, req.URL.Path)
	}
	if !strings.Contains(out, "laptop") {
		t.Errorf("key not printed: %s", out)
	}
}

func TestInstancesUpdate_Rename(t *testing.T) {
	_, req, body := stubAPI(t, http.StatusOK, map[string]any{
		"id": "gpu-abc", "name": "trainer", "status": "running", "gpu_type": "rtx_4090",
		"gpu_count": 1, "region": "us-east", "tier": "on_demand", "price_per_hour": 0.5,
		"created_at": "2026-05-14T00:00:00Z",
	})
	out, err := runCLI(t, "instances", "update", "gpu-abc", "--name", "trainer", "-o", "json")
	if err != nil {
		t.Fatalf("execute: %v (%s)", err, out)
	}
	if req.Method != http.MethodPatch || req.URL.Path != "/v1/instances/gpu-abc" {
		t.Errorf("request: %s %s", req.Method, req.URL.Path)
	}
	if (*body)["name"] != "trainer" {
		t.Errorf("body: %v", *body)
	}
}

func TestInstancesUpdate_RequiresName(t *testing.T) {
	stubAPI(t, http.StatusOK, map[string]any{})
	if _, err := runCLI(t, "instances", "update", "gpu-abc"); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("expected a --name error, got %v", err)
	}
}

func TestInstancesTerminateAlias(t *testing.T) {
	_, req, _ := stubAPI(t, http.StatusAccepted, map[string]any{
		"operation_id": "7b0c1f4e-0000-4000-8000-000000000001", "kind": "instance.delete",
		"state": "pending", "created_at": "2026-05-14T00:00:00Z",
	})
	out, err := runCLI(t, "instances", "terminate", "gpu-abc", "--wait-timeout", "0")
	if err != nil {
		t.Fatalf("execute: %v (%s)", err, out)
	}
	if req.Method != http.MethodDelete || req.URL.Path != "/v1/instances/gpu-abc" {
		t.Errorf("request: %s %s", req.Method, req.URL.Path)
	}
}
