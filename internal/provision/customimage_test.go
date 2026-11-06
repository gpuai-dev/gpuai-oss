package provision

import (
	"encoding/base64"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

func TestSupportsCustomerImage(t *testing.T) {
	// Strict subset of SupportsCustomImage: every customer-image provider must
	// also pass the template gate (the reverse is deliberately not true).
	for _, name := range []string{"podco", "marketco", "generic"} {
		if !SupportsCustomerImage(name) {
			t.Errorf("SupportsCustomerImage(%q) = false, want true", name)
		}
		if !SupportsCustomImage(name) {
			t.Errorf("subset violated: %q supports customer images but not custom images", name)
		}
	}
	for _, name := range []string{"templateonlyco", "fixedimageco", "vmco", "metalco", "cloudvmco", "vmimageco", "baremetalco", "startupvmco", "vultr", "community", ""} {
		if SupportsCustomerImage(name) {
			t.Errorf("SupportsCustomerImage(%q) = true, want false", name)
		}
	}
}

func TestCustomerImageVMCapable(t *testing.T) {
	// vm-class AND certified-capable; derived from the shared classifiers so
	// this test pins the composition, not a third roster.
	// vultr is asserted DELIBERATELY: it is vm-class + certified-capable, so
	// it joins customer-image VM placement — roster drift here must be a
	// conscious choice, not a surprise (review 759).
	for _, name := range []string{"vmco", "metalco", "cloudvmco", "vmimageco", "baremetalco", "startupvmco", "vultr"} {
		if !CustomerImageVMCapable(name) {
			t.Errorf("CustomerImageVMCapable(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"podco", "marketco", "generic", "templateonlyco", "fixedimageco", ""} {
		if CustomerImageVMCapable(name) {
			t.Errorf("CustomerImageVMCapable(%q) = true, want false", name)
		}
	}
}

func TestArgvAndEnvB64(t *testing.T) {
	if argvB64(nil) != "" {
		t.Error("empty argv should encode to empty string")
	}
	b, err := base64.StdEncoding.DecodeString(argvB64([]string{"python", "-c", "print('hi mom')"}))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "python\x00-c\x00print('hi mom')\x00" {
		t.Errorf("argv encoding = %q", b)
	}
	// Docker's clear-entrypoint idiom: a single empty element must stay
	// distinguishable from an absent override (review 759).
	eb0, err := base64.StdEncoding.DecodeString(argvB64([]string{""}))
	if err != nil {
		t.Fatal(err)
	}
	if string(eb0) != "\x00" {
		t.Errorf("[\"\"] encoding = %q, want a single NUL", eb0)
	}

	if envFileB64(nil) != "" {
		t.Error("empty env should encode to empty string")
	}
	eb, err := base64.StdEncoding.DecodeString(envFileB64([]provider.EnvVar{
		{Key: "A", Value: "1"}, {Key: "B", Value: "two words"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if string(eb) != "A=1\nB=two words\n" {
		t.Errorf("env file = %q", eb)
	}
}

func TestCustomStartB64(t *testing.T) {
	decode := func(t *testing.T, s string) string {
		t.Helper()
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("output is not base64: %v", err)
		}
		return string(b)
	}

	got := decode(t, customStartB64([]string{"/entry.sh"}, []string{"serve", "--port", "8000"}))
	want := "#!/bin/sh\nexec '/entry.sh' 'serve' '--port' '8000'\n"
	if got != want {
		t.Errorf("script = %q, want %q", got, want)
	}

	// Quoting: a single quote inside an arg must not break out of the quoted
	// word (the '\'' idiom).
	got = decode(t, customStartB64(nil, []string{"sh", "-c", "echo 'hi'; $(whoami)"}))
	want = "#!/bin/sh\nexec 'sh' '-c' 'echo '\\''hi'\\''; $(whoami)'\n"
	if got != want {
		t.Errorf("quoted script = %q, want %q", got, want)
	}

	// No entrypoint and no cmd => no script at all (SSH-only instance).
	if s := customStartB64(nil, nil); s != "" {
		t.Errorf("empty argv should produce empty script, got %q", s)
	}
}
