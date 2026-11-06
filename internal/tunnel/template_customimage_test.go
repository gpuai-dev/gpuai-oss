package tunnel

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Custom-image launch block (custom-images Phase 1): CustomStartB64 renders
// the section-9c workload launcher in container mode; empty leaves the render
// byte-for-byte free of it; a non-base64 value fails validation before it can
// reach the template.

func TestRenderBootstrapCustomStart(t *testing.T) {
	start := base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\nexec '/serve.sh' '--port' '8000'\n"))

	data := validFRPBootstrapData()
	data.CustomStartB64 = start
	out, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	for _, want := range []string{
		"gpuai-image-entrypoint.sh",
		start,
		"/var/log/gpuai-entrypoint.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered bootstrap missing %q", want)
		}
	}

	// Absent => the block renders nothing custom-image related.
	plain, err := RenderBootstrap(validFRPBootstrapData())
	if err != nil {
		t.Fatalf("RenderBootstrap(plain): %v", err)
	}
	if strings.Contains(plain, "gpuai-image-entrypoint") {
		t.Error("plain render unexpectedly contains the custom-image block")
	}
}

// Phase 3: the customer-image-on-VM block (section 7c).
func TestRenderBootstrapCustomerVM(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	data := validFRPBootstrapData()
	data.DeploymentType = "vm"
	data.DriverFloor = "550"
	data.CustomerImageRef = "ghcr.io/acme/trainer:v1@" + digest
	data.CustomerEntrypointB64 = base64.StdEncoding.EncodeToString([]byte("python\x00-m\x00server"))
	data.CustomerArgsB64 = base64.StdEncoding.EncodeToString([]byte("--port\x008000"))
	data.CustomerEnvB64 = base64.StdEncoding.EncodeToString([]byte("MODEL=acme/m\n"))
	data.CustomerPorts = []int{8000, 9000}
	data.AppPort = 8000
	data.AppSubdomain = data.InstanceID
	data.AppBasicAuthUser = "u"
	data.AppBasicAuthPass = "p"

	out, err := RenderBootstrap(data)
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	for _, want := range []string{
		`CUSTOMER_IMAGE="ghcr.io/acme/trainer:v1@` + digest + `"`,
		"gpuai-workload",
		"exit 92",
		"exit 93",
		"-p 127.0.0.1:8000:8000",
		"-p 127.0.0.1:9000:9000",
		"--entrypoint",
		"/etc/gpuai-workload.env",
		data.CustomerEntrypointB64,
		"--restart on-failure:3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("customer-VM render missing %q", want)
		}
	}
	// The Go-template brace assembly must survive rendering as shell vars.
	if !strings.Contains(out, `_LB='{{'`) || !strings.Contains(out, ".State.StartedAt") {
		t.Error("docker inspect format assembly missing")
	}
	// Certified block must NOT render (CertifiedImage false).
	if strings.Contains(out, "gpuai-certified") {
		t.Error("certified block rendered without CertifiedImage")
	}

	// A plain VM render (no customer ref) has no workload block.
	plain := validFRPBootstrapData()
	plain.DeploymentType = "vm"
	plainOut, err := RenderBootstrap(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plainOut, "gpuai-workload") {
		t.Error("plain VM render unexpectedly contains the customer workload block")
	}
}

func TestValidateBootstrapDataCustomerVM(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	base := func() BootstrapData {
		d := validFRPBootstrapData()
		d.DeploymentType = "vm"
		d.CustomerImageRef = "ubuntu:22.04@" + digest
		return d
	}
	if err := ValidateBootstrapData(base()); err != nil {
		t.Fatalf("valid customer-VM data rejected: %v", err)
	}
	cases := map[string]func(*BootstrapData){
		"unpinned ref":        func(d *BootstrapData) { d.CustomerImageRef = "ubuntu:22.04" },
		"shell metas in ref":  func(d *BootstrapData) { d.CustomerImageRef = "ubuntu:22.04@" + digest + `";rm -rf /` },
		"ref outside vm mode": func(d *BootstrapData) { d.DeploymentType = "container" },
		"env without ref":     func(d *BootstrapData) { d.CustomerImageRef = ""; d.CustomerEnvB64 = "YWJj" },
		"bad argv b64":        func(d *BootstrapData) { d.CustomerArgsB64 = "!!not-b64!!" },
		"reserved port 22":    func(d *BootstrapData) { d.CustomerPorts = []int{22} },
		"port out of range":   func(d *BootstrapData) { d.CustomerPorts = []int{70000} },
	}
	for name, mutate := range cases {
		d := base()
		mutate(&d)
		if err := ValidateBootstrapData(d); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestValidateBootstrapDataCustomStartB64(t *testing.T) {
	data := validFRPBootstrapData()
	data.CustomStartB64 = "not$base64!!"
	if err := ValidateBootstrapData(data); err == nil || !strings.Contains(err.Error(), "CustomStartB64") {
		t.Fatalf("expected CustomStartB64 validation error, got %v", err)
	}
	data.CustomStartB64 = base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\nexec 'x'\n"))
	if err := ValidateBootstrapData(data); err != nil {
		t.Fatalf("valid b64 rejected: %v", err)
	}
}
