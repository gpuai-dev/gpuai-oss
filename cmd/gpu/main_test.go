package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
	"github.com/gpuai-dev/gpuai-oss/internal/cliauth"
)

// TestRootCmd_Subcommands verifies all declared subcommands resolve. Per
// test cobra in-process via SetArgs, NOT subprocess.
func TestRootCmd_Subcommands(t *testing.T) {
	wantCommands := []string{"instances", "keys", "gpu-types", "pricing", "login", "logout", "version", "operations", "environments", "registry-credentials"}
	root := newRootCmd()
	for _, name := range wantCommands {
		found := false
		for _, c := range root.Commands() {
			if c.Name() == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected subcommand %q not found", name)
		}
	}
}

func TestRootCmd_APIBaseFlag(t *testing.T) {
	// Override via --api-base flag. version is in commandsThatSkipAuth so
	// PersistentPreRunE will not attempt to resolve a token.
	root := newRootCmd()
	root.SetArgs([]string{"--api-base", "https://example.com/v1", "version"})
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "gpu version") {
		t.Errorf("expected version output, got: %s", buf.String())
	}
}

func TestRootCmd_APIBaseEnv(t *testing.T) {
	t.Setenv("GPUAI_API_BASE", "https://env.example.com/v1")
	root := newRootCmd()
	f := root.PersistentFlags().Lookup("api-base")
	if f == nil {
		t.Fatal("api-base flag not registered")
		return
	}
	if f.DefValue != "https://env.example.com/v1" {
		t.Errorf("env override: got %q, want https://env.example.com/v1", f.DefValue)
	}
}

func TestVersionCmd(t *testing.T) {
	// Override globals for the test.
	oldV, oldC, oldD := version, commit, date
	version, commit, date = "1.0.0-test", "abc123", "2026-05-14"
	defer func() { version, commit, date = oldV, oldC, oldD }()

	root := newRootCmd()
	root.SetArgs([]string{"version"})
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "1.0.0-test") {
		t.Errorf("version missing from output: %s", out)
	}
	if !strings.Contains(out, "abc123") {
		t.Errorf("commit missing from output: %s", out)
	}
}

func TestErrorMessages_401SuggestsLogin(t *testing.T) {
	// Construct a ProblemError manually; mapClientError should turn 401 into
	// the "run `gpu login`" message.
	pe := &apiclient.ProblemError{Status: 401, Code: "unauthenticated", Detail: "no key"}
	got := mapClientError(pe)
	if got == nil || !strings.Contains(got.Error(), "gpu login") {
		t.Errorf("expected hint to run `gpu login`, got: %v", got)
	}
	if !strings.Contains(got.Error(), "GPUAI_API_KEY") {
		t.Errorf("expected hint to mention GPUAI_API_KEY, got: %v", got)
	}
}

func TestRootCmd_APIBaseDefault(t *testing.T) {
	t.Setenv("GPUAI_API_BASE", "")
	f := newRootCmd().PersistentFlags().Lookup("api-base")
	if f.DefValue != "http://localhost:8080/v1" {
		t.Errorf("default api-base: got %q, want http://localhost:8080/v1", f.DefValue)
	}
}

// TestLogin_StoresKeyAndBase checks that `gpu login` writes the key and base,
// and that later commands pick the stored base up when no flag or env is set.
func TestLogin_StoresKeyAndBase(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GPUAI_API_BASE", "")
	t.Setenv("GPUAI_API_KEY", "")

	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"login", "--stdin", "--api-base", srv.URL + "/v1/"})
	root.SetIn(strings.NewReader("gpuai_test_stored\n"))
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	if err := root.Execute(); err != nil {
		t.Fatalf("login: %v (%s)", err, out.String())
	}
	c, err := cliauth.LoadCredentials()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.APIKey != "gpuai_test_stored" || c.APIBase != srv.URL+"/v1" {
		t.Fatalf("stored %+v", c)
	}

	root = newRootCmd()
	root.SetArgs([]string{"keys", "list", "-o", "json"})
	root.SetOut(out)
	root.SetErr(out)
	if err := root.Execute(); err != nil {
		t.Fatalf("keys list: %v (%s)", err, out.String())
	}
	if gotAuth != "Bearer gpuai_test_stored" || gotPath != "/v1/ssh-keys" {
		t.Errorf("request used auth=%q path=%q", gotAuth, gotPath)
	}
}
