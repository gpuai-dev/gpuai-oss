package public_test

// Custom-image launch tests (custom-images Phase 1): the `image` field's
// exclusivity gates, the imageref 400 format gate, the registry pre-flight's
// error mapping (422 / 502), and the exact threading contract into the
// provision request (digest-pinned ref, CustomerImage flag, Entrypoint/Cmd,
// sorted env, NO ports). The registry is an in-process httptest server wired
// through registrycheck.WithTestTransport, so nothing here touches the
// network.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/api/public"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
	"github.com/gpuai-dev/gpuai-oss/internal/registrycheck"
)

// imageTestRegistry serves acme/good (amd64, entrypoint+cmd), acme/armonly,
// acme/private, and 404s everything else. Anonymous (no token challenge).
// Returns the server plus the sha256 digest of the "good" manifest bytes —
// the checker computes digests from content, so the expectation must too.
func imageTestRegistry(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	marshal := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	goodManifest := marshal(map[string]any{
		"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
		"config":    map[string]string{"digest": "sha256:cfg"},
	})
	goodSum := sha256.Sum256(goodManifest)
	goodDigest := "sha256:" + hex.EncodeToString(goodSum[:])
	armManifest := marshal(map[string]any{
		"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
		"config":    map[string]string{"digest": "sha256:cfgarm"},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/{$}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /v2/acme/good/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", goodDigest)
		w.Write(goodManifest)
	})
	mux.HandleFunc("GET /v2/acme/good/blobs/{d}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"architecture": "amd64", "os": "linux",
			"config": map[string]any{"Entrypoint": []string{"/serve.sh"}, "Cmd": []string{"--port", "8000"}},
		})
	})
	mux.HandleFunc("GET /v2/acme/armonly/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		w.Write(armManifest)
	})
	mux.HandleFunc("GET /v2/acme/armonly/blobs/{d}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"architecture": "arm64", "os": "linux", "config": map[string]any{}})
	})
	mux.HandleFunc("GET /v2/acme/private/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, goodDigest
}

// imageTestDeps2 is imageTestDeps for tests that don't need the digest.
func imageTestDeps2(t *testing.T, engine *stubEngine) public.PublicDeps {
	t.Helper()
	srv, _ := imageTestRegistry(t)
	return imageTestDeps(t, engine, srv)
}

func imageTestDeps(t *testing.T, engine *stubEngine, srv *httptest.Server) public.PublicDeps {
	t.Helper()
	deps := instanceTestDeps(newStubOperationStore(), newStubInstanceStore(), engine)
	host := strings.TrimPrefix(srv.URL, "http://")
	deps.RegistryChecker = registrycheck.New(registrycheck.WithTestTransport(func(string) string { return host }))
	return deps
}

func imageBody(extra map[string]any) map[string]any {
	body := map[string]any{"gpu_type": "h100_sxm", "gpu_count": 1, "tier": "on_demand"}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func TestHandleCreateInstance_ImageExclusivityAndFormat(t *testing.T) {
	deps := imageTestDeps2(t, &stubEngine{})
	cases := []struct {
		name     string
		extra    map[string]any
		wantCode int
	}{
		{"image with template_id", map[string]any{"image": "reg.example.com/acme/good:v1", "template_id": "comfyui"}, http.StatusUnprocessableEntity},
		{"image with environment", map[string]any{"image": "reg.example.com/acme/good:v1", "environment": "certified"}, http.StatusUnprocessableEntity},
		{"empty image", map[string]any{"image": ""}, http.StatusUnprocessableEntity},
		{"uppercase ref", map[string]any{"image": "ACME/Bad:v1"}, http.StatusBadRequest},
		{"shell metacharacters", map[string]any{"image": "acme/bad:v1;rm -rf /"}, http.StatusBadRequest},
		{"localhost registry", map[string]any{"image": "localhost:5000/acme/bad:v1"}, http.StatusBadRequest},
		{"private-ip registry", map[string]any{"image": "10.0.0.5/acme/bad:v1"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", imageBody(c.extra))
			if w.Code != c.wantCode {
				t.Fatalf("status: got %d, want %d; body=%s", w.Code, c.wantCode, w.Body.String())
			}
		})
	}
}

func TestHandleCreateInstance_ImagePreflightErrors(t *testing.T) {
	deps := imageTestDeps2(t, &stubEngine{})
	cases := []struct {
		name     string
		image    string
		wantCode int
		wantErr  string
	}{
		{"not found", "reg.example.com/acme/missing:v1", http.StatusUnprocessableEntity, "image_unavailable"},
		{"private", "reg.example.com/acme/private:v1", http.StatusUnprocessableEntity, "image_unavailable"},
		{"arm only", "reg.example.com/acme/armonly:v1", http.StatusUnprocessableEntity, "image_unavailable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1", imageBody(map[string]any{"image": c.image}))
			if w.Code != c.wantCode {
				t.Fatalf("status: got %d, want %d; body=%s", w.Code, c.wantCode, w.Body.String())
			}
			var p public.Problem
			_ = json.NewDecoder(w.Body).Decode(&p)
			if p.Code != c.wantErr {
				t.Errorf("code: got %q, want %q", p.Code, c.wantErr)
			}
		})
	}
}

func TestHandleCreateInstance_ImageRegistryUnreachable(t *testing.T) {
	srv, _ := imageTestRegistry(t)
	deps := imageTestDeps(t, &stubEngine{}, srv)
	srv.Close()
	w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1",
		imageBody(map[string]any{"image": "reg.example.com/acme/good:v1"}))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleCreateInstance_ImageEnvGate(t *testing.T) {
	deps := imageTestDeps2(t, &stubEngine{})
	for name, env := range map[string]map[string]string{
		"reserved GPUAI_ key": {"GPUAI_CALLBACK": "x"},
		"reserved PUBLIC_KEY": {"PUBLIC_KEY": "x"},
		"malformed key":       {"9BAD": "x"},
		"newline in value":    {"K": "a\nINJECTED=b"},
		"control byte":        {"K": "a\x07b"},
	} {
		t.Run(name, func(t *testing.T) {
			w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1",
				imageBody(map[string]any{"image": "reg.example.com/acme/good:v1", "env": env}))
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status: got %d, want 422; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleCreateInstance_ImageHappyPathThreading(t *testing.T) {
	engine := &stubEngine{provisionResp: &provision.ProvisionResponse{InstanceID: "gpu-img1", Hostname: "gpu-img1.example.com"}}
	srv, goodDigest := imageTestRegistry(t)
	deps := imageTestDeps(t, engine, srv)

	w := invokeInstance(t, deps, http.MethodPost, "/v1/instances", "org-A", "user-A", "ak-1",
		imageBody(map[string]any{
			"image": "reg.example.com/acme/good:v1",
			"env":   map[string]string{"MODEL": "acme/m", "HF_TOKEN": "tok"},
		}))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202; body=%s", w.Code, w.Body.String())
	}

	var got provision.ProvisionRequest
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if req, ok := engine.lastRequest(); ok {
			got = req
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !got.CustomerImage {
		t.Fatalf("CustomerImage not set on provision request: %+v", got)
	}
	wantImage := "reg.example.com/acme/good:v1@" + goodDigest
	if got.Image != wantImage {
		t.Errorf("Image: got %q, want digest-pinned %q", got.Image, wantImage)
	}
	if len(got.ImageEntrypoint) != 1 || got.ImageEntrypoint[0] != "/serve.sh" {
		t.Errorf("ImageEntrypoint: got %v", got.ImageEntrypoint)
	}
	if len(got.ImageCmd) != 2 || got.ImageCmd[0] != "--port" {
		t.Errorf("ImageCmd: got %v", got.ImageCmd)
	}
	// Env is sorted by key: HF_TOKEN before MODEL.
	if len(got.Env) != 2 || got.Env[0].Key != "HF_TOKEN" || got.Env[1].Key != "MODEL" {
		t.Errorf("Env: got %+v, want sorted [HF_TOKEN MODEL]", got.Env)
	}
	// Phase 1 contract: no ports, no template, no app port.
	if len(got.Ports) != 0 || got.TemplateID != nil || got.AppPort != 0 {
		t.Errorf("unexpected template-shaped fields: ports=%v template=%v appPort=%d", got.Ports, got.TemplateID, got.AppPort)
	}
}
