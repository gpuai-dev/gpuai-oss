package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeysCreate_FromFile(t *testing.T) {
	tmp := t.TempDir()
	pubPath := filepath.Join(tmp, "id_ed25519.pub")
	pubBody := "ssh-ed25519 AAAA... laptop"
	if err := os.WriteFile(pubPath, []byte(pubBody), 0o600); err != nil {
		t.Fatal(err)
	}

	var receivedBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "key-xyz",
			"name":        receivedBody["name"],
			"public_key":  receivedBody["public_key"],
			"fingerprint": "SHA256:abc",
			"created_at":  "2026-05-14T00:00:00Z",
		})
	}))
	defer srv.Close()
	t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
	t.Setenv("GPUAI_API_BASE", srv.URL)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := newRootCmd()
	root.SetArgs([]string{"keys", "create", "--name", "laptop", "--public-key-file", pubPath, "--output", "json"})
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if receivedBody["name"] != "laptop" {
		t.Errorf("name: got %q, want laptop", receivedBody["name"])
	}
	if !strings.Contains(receivedBody["public_key"], "ssh-ed25519") {
		t.Errorf("public_key not forwarded: %q", receivedBody["public_key"])
	}
	if !strings.Contains(buf.String(), "key-xyz") {
		t.Errorf("response not printed: %s", buf.String())
	}
}
