package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := &server{
		apiKey:    "test-key-not-secret",
		offerings: defaultOfferings,
		runtime:   &simulatedRuntime{publicIP: "127.0.0.1", readyDelay: 0},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		instances: map[string]*instance{},
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return ts
}

func call(t *testing.T, ts *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestLifecycle walks the Generic Supplier API contract the generic adapter
// relies on: list, create, status until running, delete, idempotent delete.
func TestLifecycle(t *testing.T) {
	ts := newTestServer(t)
	const key = "test-key-not-secret"

	if code, _ := call(t, ts, "GET", "/v1/gpu/available", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list = %d, want 401", code)
	}
	code, avail := call(t, ts, "GET", "/v1/gpu/available", key, nil)
	if code != http.StatusOK || len(avail["offerings"].([]any)) != len(defaultOfferings) {
		t.Fatalf("list = %d %v", code, avail)
	}

	code, created := call(t, ts, "POST", "/v1/instances", key, map[string]any{
		"instance_id": "gpu-test1", "gpu_type": "rtx_4090", "gpu_count": 1, "tier": "on_demand", "region": "us-east",
	})
	if code != http.StatusCreated || created["status"] != "creating" {
		t.Fatalf("create = %d %v", code, created)
	}
	id := created["upstream_id"].(string)

	var status map[string]any
	for i := 0; i < 10; i++ {
		_, status = call(t, ts, "GET", "/v1/instances/"+id, key, nil)
		if status["status"] == "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status["status"] != "running" || status["ip"] != "127.0.0.1" || status["ssh_port"].(float64) != 22 {
		t.Fatalf("status = %v, want running at 127.0.0.1:22", status)
	}

	// Stock is consumed while the instance lives.
	_, avail = call(t, ts, "GET", "/v1/gpu/available", key, nil)
	for _, o := range avail["offerings"].([]any) {
		m := o.(map[string]any)
		if m["gpu_type"] == "rtx_4090" && m["available_count"].(float64) != 3 {
			t.Fatalf("rtx_4090 available_count = %v, want 3 while one instance runs", m["available_count"])
		}
	}

	for i := 0; i < 2; i++ { // delete is idempotent
		if code, del := call(t, ts, "DELETE", "/v1/instances/"+id, key, nil); code != http.StatusOK || del["status"] != "terminated" {
			t.Fatalf("delete #%d = %d %v", i+1, code, del)
		}
	}
	if _, status = call(t, ts, "GET", "/v1/instances/"+id, key, nil); status["status"] != "terminated" {
		t.Fatalf("status after delete = %v", status)
	}
	if code, _ := call(t, ts, "GET", "/v1/instances/nope", key, nil); code != http.StatusNotFound {
		t.Fatalf("unknown instance = %d, want 404", code)
	}
}

// TestCreateNoCapacity: an unmatched request answers 409 with a body the
// generic adapter classifies as "no capacity" (so the engine falls through).
func TestCreateNoCapacity(t *testing.T) {
	ts := newTestServer(t)
	code, body := call(t, ts, "POST", "/v1/instances", "test-key-not-secret", map[string]any{
		"gpu_type": "b200", "gpu_count": 8, "tier": "on_demand",
	})
	if code != http.StatusConflict || body["error"] != "no_capacity" {
		t.Fatalf("create = %d %v, want 409 no_capacity", code, body)
	}
}
