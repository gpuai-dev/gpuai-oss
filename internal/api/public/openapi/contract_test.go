package openapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gpuai-dev/gpuai-oss/internal/api/public/openapi"
)

// TestSpecValidatesAsOpenAPI31 loads the embedded spec via kin-openapi and
// calls Validate to catch any structural drift from the OpenAPI 3.1 schema.
//
// This is the CI gate: any malformed $ref, missing required field, or schema
// mismatch in openapi/v1.yaml fails this test, which fails CI.
func TestSpecValidatesAsOpenAPI31(t *testing.T) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false

	// Convert YAML→JSON for the loader (kin-openapi accepts JSON natively).
	b, err := openapi.SpecJSON()
	if err != nil {
		t.Fatalf("SpecJSON: %v", err)
	}

	spec, err := loader.LoadFromData(b)
	if err != nil {
		t.Fatalf("LoadFromData: %v", err)
	}

	if err := spec.Validate(context.Background()); err != nil {
		t.Fatalf("spec.Validate: %v", err)
	}

	// The spec documents exactly the GPU.ai Open Source surface: a path added
	// to (or dropped from) the router without the spec, or vice versa, trips
	// this check before downstream tooling silently produces a wrong SDK.
	want := []string{
		"/health", "/openapi.json", "/gpu-types", "/pricing", "/environments",
		"/instances", "/instances/{id}", "/operations/{id}",
		"/registry-credentials", "/registry-credentials/{id}",
		"/ssh-keys", "/ssh-keys/{id}",
	}
	got := spec.Paths.Map()
	for _, p := range want {
		if _, ok := got[p]; !ok {
			t.Errorf("spec is missing path %s", p)
		}
	}
	if len(got) != len(want) {
		t.Errorf("paths count: got %d, want exactly %d", len(got), len(want))
	}
}

// loadSpec is a shared helper for tests that need a loaded *openapi3.T.
func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	b, err := openapi.SpecJSON()
	if err != nil {
		t.Fatalf("SpecJSON: %v", err)
	}
	spec, err := loader.LoadFromData(b)
	if err != nil {
		t.Fatalf("LoadFromData: %v", err)
	}
	return spec
}

// TestEmbeddedYAMLMatchesRoot guards against the served spec drifting from
// openapi/v1.yaml on disk (the file the SDKs are generated from). The server
// embeds that file directly, so this only fails if the embedding is rewired.
//
// Best-effort: when the test is run from a stripped GOPATH cache (e.g.
// `go test ./...` from outside the repo) the root path may not be
// reachable; in that case we Skip rather than fail.
func TestEmbeddedYAMLMatchesRoot(t *testing.T) {
	embedded := openapi.SpecYAML()

	// Walk up four levels: internal/api/public/openapi/ → repo root.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	rootPath := filepath.Join(cwd, "..", "..", "..", "..", "openapi", "v1.yaml")

	f, err := os.Open(rootPath)
	if err != nil {
		t.Skipf("root openapi/v1.yaml not reachable from %q: %v", cwd, err)
	}
	defer f.Close()

	rootBytes, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}

	embeddedSum := sha256.Sum256(embedded)
	rootSum := sha256.Sum256(rootBytes)
	if !bytes.Equal(embeddedSum[:], rootSum[:]) {
		t.Fatalf("openapi/v1.yaml drift detected:\n"+
			"  root  sha256 = %s\n"+
			"  embed sha256 = %s\n"+
			"  The server must embed openapi/v1.yaml via the root openapi package.",
			hex.EncodeToString(rootSum[:]),
			hex.EncodeToString(embeddedSum[:]))
	}
}
