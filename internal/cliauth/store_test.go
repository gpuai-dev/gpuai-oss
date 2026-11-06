package cliauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveCredentials_Mode0600(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	err := SaveCredentials(Credentials{APIKey: "gpuai_test_abc", APIBase: "http://localhost:8080/v1"})
	if err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	path := filepath.Join(tmp, "gpu", "credentials.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("mode: got %o, want %o", got, want)
	}
}

func TestSaveAndLoadCredentials_Roundtrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	in := Credentials{APIKey: "gpuai_test_xyz", APIBase: "https://api.example.com/v1"}
	if err := SaveCredentials(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadCredentials()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if out.APIKey != in.APIKey || out.APIBase != in.APIBase {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", out, in)
	}
}

func TestResolveAPIBase_Precedence(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("GPUAI_API_BASE", "")

	if got, src := ResolveAPIBase(); got != DefaultAPIBase || src != "default" {
		t.Errorf("default: got (%s, %s)", got, src)
	}
	if err := SaveCredentials(Credentials{APIKey: "k", APIBase: "http://gpu.example.test:9000/v1"}); err != nil {
		t.Fatal(err)
	}
	if got, src := ResolveAPIBase(); got != "http://gpu.example.test:9000/v1" || src != "config" {
		t.Errorf("config: got (%s, %s)", got, src)
	}
	t.Setenv("GPUAI_API_BASE", "http://env.example.test/v1")
	if got, src := ResolveAPIBase(); got != "http://env.example.test/v1" || src != "env" {
		t.Errorf("env: got (%s, %s)", got, src)
	}
}

func TestResolveToken_Precedence(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)

	// 1. Neither set → error.
	t.Setenv("GPUAI_API_KEY", "")
	_, _, err := ResolveToken()
	if err == nil {
		t.Error("expected error when neither source set")
	}

	// 2. Only credentials.json → source=config.
	if err := SaveCredentials(Credentials{APIKey: "config_key"}); err != nil {
		t.Fatal(err)
	}
	tok, src, err := ResolveToken()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tok != "config_key" || src != "config" {
		t.Errorf("got (%s, %s), want (config_key, config)", tok, src)
	}

	// 3. Env var wins over credentials.json → source=env.
	t.Setenv("GPUAI_API_KEY", "env_key")
	tok, src, err = ResolveToken()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tok != "env_key" || src != "env" {
		t.Errorf("got (%s, %s), want (env_key, env)", tok, src)
	}
}

func TestRunLogout_RemovesFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	// Save, confirm exists, remove, confirm gone.
	if err := SaveCredentials(Credentials{APIKey: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(CredentialsPath()); err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}
	if err := RemoveCredentials(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(CredentialsPath()); !strings.Contains(err.Error(), "no such file") {
		t.Errorf("expected file gone, got err=%v", err)
	}
	// Idempotent: second call returns nil.
	if err := RemoveCredentials(); err != nil {
		t.Errorf("second remove should be nil: %v", err)
	}
}
