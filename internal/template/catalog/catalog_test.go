// Catalog load/validate/reload tests (Plan 31-01 scaffold, enforcing as of
// Plan 31-02 which landed internal/template/catalog/catalog.go +
// embed.go). The wave-pending build tag was removed once the production code
// existed, turning these tests from dormant to enforcing.
//
// References (Wave 1 production symbols this file pins):
//   - catalog.Load([]byte) (*Catalog, error)   — mirrors inference/catalog.Load
//   - (*Catalog).Get(id) (*TemplateSpec, bool)
//   - (*Catalog).List() []*TemplateSpec
//   - (*Catalog).Reload([]byte) error
//   - testdata/templates.json                   — a representative catalog

package catalog

import (
	"os"
	"strings"
	"testing"
)

// loadEmbeddedFixture returns the representative catalog fixture.
func loadEmbeddedFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/templates.json")
	if err != nil || len(raw) == 0 {
		t.Fatalf("read testdata/templates.json: %v", err)
	}
	return raw
}

// TestLoadEmpty: the shipped catalog loads and holds no templates.
func TestLoadEmpty(t *testing.T) {
	c, err := LoadEmpty()
	if err != nil {
		t.Fatalf("LoadEmpty: %v", err)
	}
	if n := len(c.List()); n != 0 {
		t.Fatalf("shipped catalog has %d templates, want 0", n)
	}
	if _, ok := c.Get("jupyter"); ok {
		t.Fatal("shipped catalog must resolve no template ids")
	}
}

// TestLoad_Valid asserts the seed catalog loads, the flagship service templates
// resolve via Get, the Axolotl entry now loads as a live deployable "service"
// (a Jupyter Lab workbench — no longer a coming_soon placeholder), and List
// returns exactly fourteen entries.
func TestLoad_Valid(t *testing.T) {
	c, err := Load(loadEmbeddedFixture(t))
	if err != nil {
		t.Fatalf("Load err = %v; want nil", err)
	}
	if _, ok := c.Get("comfyui"); !ok {
		t.Errorf("Get(comfyui) = false; want true")
	}
	if _, ok := c.Get("vllm"); !ok {
		t.Errorf("Get(vllm) = false; want true")
	}
	ax, ok := c.Get("axolotl-finetuning")
	if !ok {
		t.Fatalf("Get(axolotl-finetuning) = false; want true")
	}
	// Axolotl is now a deployable service (kind=service, no coming_soon status)
	// — it must have passed the service-only port/image validation in Load.
	if ax.Kind != "service" || ax.Status != "" {
		t.Errorf("axolotl: kind=%q status=%q; want service/\"\"", ax.Kind, ax.Status)
	}
	if ax.Image == "" {
		t.Errorf("axolotl: image is empty; want a deployable image reference")
	}
	// Faster Whisper (speaches) — speech-to-text service; deliberately declares
	// NO env (models load dynamically per request), which Load must accept.
	fw, ok := c.Get("faster-whisper")
	if !ok {
		t.Fatalf("Get(faster-whisper) = false; want true")
	}
	if fw.Kind != "service" || fw.Status != "" {
		t.Errorf("faster-whisper: kind=%q status=%q; want service/\"\"", fw.Kind, fw.Status)
	}
	if len(fw.Env) != 0 {
		t.Errorf("faster-whisper: len(Env) = %d; want 0 (zero-config template)", len(fw.Env))
	}
	// Stable Diffusion WebUI (A1111) — image-generation service.
	a1, ok := c.Get("automatic1111")
	if !ok {
		t.Fatalf("Get(automatic1111) = false; want true")
	}
	if a1.Kind != "service" || a1.Status != "" {
		t.Errorf("automatic1111: kind=%q status=%q; want service/\"\"", a1.Kind, a1.Status)
	}
	// The 2026-06 hub expansion: every new entry is a live deployable service.
	for _, id := range []string{
		"ollama", "open-webui", "sglang", "text-generation-webui",
		"invokeai", "swarmui", "kohya-ss", "ai-toolkit", "llama-factory",
	} {
		s, ok := c.Get(id)
		if !ok {
			t.Errorf("Get(%s) = false; want true", id)
			continue
		}
		if s.Kind != "service" || s.Status != "" {
			t.Errorf("%s: kind=%q status=%q; want service/\"\"", id, s.Kind, s.Status)
		}
	}
	if got := len(c.List()); got != 15 {
		t.Fatalf("len(List()) = %d; want 15", got)
	}
	jp, ok := c.Get("jupyter")
	if !ok {
		t.Fatal("Get(jupyter) = false; want true")
	}
	if jp.Kind != "service" || jp.Status != "" {
		t.Errorf("jupyter: kind=%q status=%q; want service/\"\"", jp.Kind, jp.Status)
	}
	var jprimary *Port
	for i := range jp.Ports {
		if jp.Ports[i].Primary {
			jprimary = &jp.Ports[i]
		}
	}
	if jprimary == nil || jprimary.Port != 8888 || jprimary.Protocol != "http" {
		t.Errorf("jupyter: primary port = %+v; want 8888/http", jprimary)
	}
}

// TestLoad_RejectsDuplicateID feeds two templates with id "comfyui" and
// expects a load error mentioning the duplicate.
func TestLoad_RejectsDuplicateID(t *testing.T) {
	raw := []byte(`{
	  "version": 1,
	  "templates": [
	    {"id":"comfyui","display_name":"A","category":"image-generation","kind":"service","image":"ghcr.io/gpuai/a:1@sha256:0000000000000000000000000000000000000000000000000000000000000000","container_disk_gb":40,"min_vram_gb":16,"min_gpu_count":1,"ready_timeout_seconds":300,"ports":[{"port":8188,"protocol":"http","primary":true}],"env":[],"start_command":"","docs_url":"https://x"},
	    {"id":"comfyui","display_name":"B","category":"image-generation","kind":"service","image":"ghcr.io/gpuai/b:1@sha256:0000000000000000000000000000000000000000000000000000000000000000","container_disk_gb":40,"min_vram_gb":16,"min_gpu_count":1,"ready_timeout_seconds":300,"ports":[{"port":8189,"protocol":"http","primary":true}],"env":[],"start_command":"","docs_url":"https://y"}
	  ]
	}`)
	_, err := Load(raw)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v; want contains \"duplicate\"", err)
	}
}

// TestLoad_RejectsNonServiceKind asserts kind must be "service" — a "job"
// kind (no long-lived listening port) is rejected at load time.
func TestLoad_RejectsNonServiceKind(t *testing.T) {
	raw := []byte(`{
	  "version": 1,
	  "templates": [
	    {"id":"batch","display_name":"Batch","category":"misc","kind":"job","image":"ghcr.io/gpuai/batch:1@sha256:0000000000000000000000000000000000000000000000000000000000000000","container_disk_gb":40,"min_vram_gb":16,"min_gpu_count":1,"ready_timeout_seconds":300,"ports":[{"port":8188,"protocol":"http","primary":true}],"env":[],"start_command":"","docs_url":"https://x"}
	  ]
	}`)
	_, err := Load(raw)
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("err = %v; want contains \"kind\"", err)
	}
}

// TestLoad_AllowsComingSoonNonServiceKind asserts the coming_soon escape hatch
// permits a non-service kind (e.g. "finetuning") — such a placeholder advertises
// an upcoming, not-yet-deployable template in the Hub grid and never reaches the
// service-only deploy path. The same kind WITHOUT coming_soon is rejected
// (TestLoad_RejectsNonServiceKind), so the relaxation is strictly scoped.
func TestLoad_AllowsComingSoonNonServiceKind(t *testing.T) {
	raw := []byte(`{
	  "version": 1,
	  "templates": [
	    {"id":"axolotl","display_name":"Axolotl","category":"fine-tuning","kind":"finetuning","status":"coming_soon","ports":[],"env":[],"docs_url":"https://docs.axolotl.ai"}
	  ]
	}`)
	c, err := Load(raw)
	if err != nil {
		t.Fatalf("Load(coming_soon finetuning) err = %v; want nil", err)
	}
	s, ok := c.Get("axolotl")
	if !ok {
		t.Fatalf("Get(axolotl) = false; want true")
	}
	if s.Kind != "finetuning" {
		t.Errorf("kind = %q; want finetuning", s.Kind)
	}
}

// TestLoad_RejectsMultiplePrimaryPorts asserts exactly one port may carry
// primary:true — two primaries is an authoring error.
func TestLoad_RejectsMultiplePrimaryPorts(t *testing.T) {
	raw := []byte(`{
	  "version": 1,
	  "templates": [
	    {"id":"comfyui","display_name":"ComfyUI","category":"image-generation","kind":"service","image":"ghcr.io/gpuai/comfyui:1@sha256:0000000000000000000000000000000000000000000000000000000000000000","container_disk_gb":40,"min_vram_gb":16,"min_gpu_count":1,"ready_timeout_seconds":300,"ports":[{"port":8188,"protocol":"http","primary":true},{"port":7860,"protocol":"http","primary":true}],"env":[],"start_command":"","docs_url":"https://x"}
	  ]
	}`)
	_, err := Load(raw)
	if err == nil || !strings.Contains(err.Error(), "primary") {
		t.Fatalf("err = %v; want contains \"primary\"", err)
	}
}

// TestLoad_RejectsReservedEnvKey asserts a template declaring an env key
// reserved by the GPU.ai bootstrap (GPUAI_* / PUBLIC_KEY) is rejected at Load,
// rather than the colliding key being silently dropped at deploy time when the
// provider adapter merges the internal vars with "internal wins" (MD-05).
func TestLoad_RejectsReservedEnvKey(t *testing.T) {
	for _, key := range []string{"GPUAI_INSTANCE_ID", "GPUAI_CALLBACK_URL", "PUBLIC_KEY"} {
		raw := []byte(`{
		  "version": 1,
		  "templates": [
		    {"id":"comfyui","display_name":"ComfyUI","category":"image-generation","kind":"service","image":"ghcr.io/gpuai/comfyui:1@sha256:0000000000000000000000000000000000000000000000000000000000000000","container_disk_gb":40,"min_vram_gb":16,"min_gpu_count":1,"ready_timeout_seconds":300,"ports":[{"port":8188,"protocol":"http","primary":true}],"env":[{"key":"` + key + `"}],"start_command":"","docs_url":"https://x"}
		  ]
		}`)
		if _, err := Load(raw); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("key %q: err = %v; want contains \"reserved\"", key, err)
		}
	}
}

// TestLoad_RejectsWrongVersion asserts version must be 1 (forward-compat
// guard, mirrors inference catalog).
func TestLoad_RejectsWrongVersion(t *testing.T) {
	raw := []byte(`{"version": 2, "templates": []}`)
	_, err := Load(raw)
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("err = %v; want contains \"version\"", err)
	}
}

// TestReload_AtomicSwap asserts Reload swaps to a valid second payload and
// that a subsequent bad payload returns an error while leaving the catalog
// intact (in-flight readers keep the prior pointer). Mirrors the inference
// catalog's atomic-reload contract.
func TestReload_AtomicSwap(t *testing.T) {
	c, err := Load(loadEmbeddedFixture(t))
	if err != nil {
		t.Fatalf("Load err = %v", err)
	}
	// Swap to a valid single-template payload.
	next := []byte(`{
	  "version": 1,
	  "templates": [
	    {"id":"comfyui","display_name":"ComfyUI","category":"image-generation","kind":"service","image":"ghcr.io/gpuai/comfyui:1@sha256:0000000000000000000000000000000000000000000000000000000000000000","container_disk_gb":40,"min_vram_gb":16,"min_gpu_count":1,"ready_timeout_seconds":300,"ports":[{"port":8188,"protocol":"http","primary":true}],"env":[],"start_command":"","docs_url":"https://x"}
	  ]
	}`)
	if err := c.Reload(next); err != nil {
		t.Fatalf("Reload(valid) err = %v; want nil", err)
	}
	if _, ok := c.Get("vllm"); ok {
		t.Errorf("Get(vllm) = true after reload-without-vllm; want false")
	}
	if _, ok := c.Get("comfyui"); !ok {
		t.Errorf("Get(comfyui) = false after reload; want true")
	}
	// A bad payload must error and leave the catalog intact.
	if err := c.Reload([]byte(`{"version":2,"templates":[]}`)); err == nil {
		t.Errorf("Reload(bad version) err = nil; want error")
	}
	if _, ok := c.Get("comfyui"); !ok {
		t.Errorf("Get(comfyui) = false after failed reload; want true (catalog intact)")
	}
}

// TestLoad_RejectsMutableImageTag pins the digest-pinning rule. The freshness
// workflow computes DATE_TAG="$(date -u +%Y-%m)" and pushes to that SAME tag
// every weekly run, so a bare `:2026-09` resolves to whatever the latest
// September build produced — an image rebuild would change what production
// launches with no code change, no review, and no way to tell from the catalog.
// Mirrors the GPU_BURN_IMAGE rule in internal/config (which rejects floating
// tags for the same reason) and tools/bump_template_image.py's _IMAGE_RE, which
// enforces the same shape in CI.
func TestLoad_RejectsMutableImageTag(t *testing.T) {
	const digest = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	entry := func(image string) []byte {
		return []byte(`{"version":1,"templates":[
	    {"id":"comfyui","display_name":"ComfyUI","category":"image-generation","kind":"service","image":"` + image + `","container_disk_gb":40,"min_vram_gb":16,"min_gpu_count":1,"ready_timeout_seconds":300,"ports":[{"port":8188,"protocol":"http","primary":true}],"env":[],"start_command":"","docs_url":"https://x"}
	]}`)
	}

	for _, bad := range []string{
		"ghcr.io/gpuai-dev/comfyui:2026-09", // the live shape before this rule
		"ghcr.io/gpuai-dev/comfyui:latest",
		"ghcr.io/gpuai-dev/comfyui@sha256:abc", // digest present but not 64 hex
		"ghcr.io/gpuai-dev/comfyui",            // no tag at all
	} {
		if _, err := Load(entry(bad)); err == nil {
			t.Errorf("Load(%q) = nil error; a mutable image reference must be rejected", bad)
		}
	}

	if _, err := Load(entry("ghcr.io/gpuai-dev/comfyui:2026-09" + digest)); err != nil {
		t.Errorf("Load(digest-pinned) = %v; want nil", err)
	}

	// coming_soon templates carry no image and must stay exempt.
	comingSoon := []byte(`{"version":1,"templates":[
	    {"id":"soon","display_name":"Soon","category":"misc","kind":"service","status":"coming_soon","image":"","container_disk_gb":40,"min_vram_gb":16,"min_gpu_count":1,"ready_timeout_seconds":300,"ports":[],"env":[],"start_command":"","docs_url":"https://x"}
	]}`)
	if _, err := Load(comingSoon); err != nil {
		t.Errorf("Load(coming_soon without image) = %v; want nil", err)
	}
}
