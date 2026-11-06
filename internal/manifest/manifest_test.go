// Phase 63 Wave 0 (Plan 01) RED tests for the pinned-environment manifest.
// These pin the contract that Plan 02 implements:
//   - (*Manifest).Lookup(gpuModel) (GPUEnv, bool)   — per-GPU env, digest-pinned image
//   - (*Manifest).DriverFloorForCUDA(gen) (string, bool) — toolkit→driver floor
//   - LoadEmbedded() (*Manifest, error)             — go:embed manifest.json
//
// They compile against the Wave 0 zero-value skeletons and FAIL on assertions
// (never panic, never compile-error). Plan 02 fills manifest.json + the lookup
// bodies, turning them green.
package manifest

import (
	"fmt"
	"strings"
	"testing"
)

// loadEmbeddedFixture returns the embedded manifest bytes, failing clearly if
// the go:embed broke. Mirrors the template-catalog test helper.
func loadEmbeddedFixture(t *testing.T) *Manifest {
	t.Helper()
	if len(EmbeddedManifestJSON) == 0 {
		t.Fatal("EmbeddedManifestJSON is empty — go:embed manifest.json broke")
	}
	m, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded() err = %v; want nil", err)
	}
	if m == nil {
		t.Fatal("LoadEmbedded() returned nil manifest")
	}
	return m
}

// TestManifest_Lookup pins the per-GPU environment contract: the flagship
// Blackwell SKU resolves to driver floor 570.26 / CUDA 12.8 with a DIGEST-PINNED
// certified image (must contain "@sha256:"), and an unknown GPU degrades to
// zero-value + false (never panic).
func TestManifest_Lookup(t *testing.T) {
	m := loadEmbeddedFixture(t)

	env, ok := m.Lookup("RTX PRO 6000")
	if !ok {
		t.Fatalf("Lookup(RTX PRO 6000) ok = false; want true")
	}
	if env.DriverFloor != "570.26" {
		t.Errorf("DriverFloor = %q; want \"570.26\"", env.DriverFloor)
	}
	if env.CUDAMin != "12.8" {
		t.Errorf("CUDAMin = %q; want \"12.8\"", env.CUDAMin)
	}
	if env.ImageRef == "" {
		t.Errorf("ImageRef is empty; want a digest-pinned certified image ref")
	}
	// Supply-chain (T-63-02): the certified image MUST be digest-pinned — a
	// mutable tag would let an upstream rebuild swap the image out from under us.
	if !strings.Contains(env.ImageRef, "@sha256:") {
		t.Errorf("ImageRef = %q; want a digest-pinned ref containing \"@sha256:\"", env.ImageRef)
	}

	if _, ok := m.Lookup("No Such GPU 9999"); ok {
		t.Errorf("Lookup(unknown GPU) ok = true; want false (zero-value + false)")
	}
	if env, _ := m.Lookup("No Such GPU 9999"); env != (GPUEnv{}) {
		t.Errorf("Lookup(unknown GPU) env = %+v; want zero value", env)
	}
}

// TestDriverFloor pins the toolkit-generation→minimum-driver table and asserts
// the Blackwell floor is sourced from the 12.8 row. Unknown generation → false.
func TestDriverFloor(t *testing.T) {
	m := loadEmbeddedFixture(t)

	rows := []struct {
		gen        string
		wantDriver string
	}{
		{"12.4", "550.54.14"},
		{"12.6", "560.28.03"},
		{"12.8", "570.26"},
		{"12.9", "575.51.03"},
		{"13.0", "580.65.06"},
	}
	for _, r := range rows {
		got, ok := m.DriverFloorForCUDA(r.gen)
		if !ok {
			t.Errorf("DriverFloorForCUDA(%q) ok = false; want true", r.gen)
			continue
		}
		if got != r.wantDriver {
			t.Errorf("DriverFloorForCUDA(%q) = %q; want %q", r.gen, got, r.wantDriver)
		}
	}

	// Blackwell's floor must come from the CUDA 12.8 row (the SKU's CUDAMin).
	blackwell, ok := m.Lookup("RTX PRO 6000")
	if ok {
		floor, ok := m.DriverFloorForCUDA(blackwell.CUDAMin)
		if !ok || floor != blackwell.DriverFloor {
			t.Errorf("Blackwell floor via CUDA %q = (%q,%v); want (%q,true)",
				blackwell.CUDAMin, floor, ok, blackwell.DriverFloor)
		}
	}

	if _, ok := m.DriverFloorForCUDA("99.9"); ok {
		t.Errorf("DriverFloorForCUDA(unknown gen) ok = true; want false")
	}
}

// TestFrameworkImages pins the D-64-01 framework-family contract on the embedded
// manifest: every shipped framework covers both CUDA base variants with a
// digest-pinned ref whose repo names the framework+variant, and the lookup path
// hides PLACEHOLDER (unpublished) refs — an unpublished image must never be
// resolvable for launch.
func TestFrameworkImages(t *testing.T) {
	m := loadEmbeddedFixture(t)

	for _, fw := range []string{"pytorch", "cuda-devel", "jupyter"} {
		family, ok := m.FrameworkImages[fw]
		if !ok {
			t.Errorf("framework_images missing %q", fw)
			continue
		}
		for _, cudaMin := range []string{"12.8", "12.4"} {
			variant := "cuda" + strings.ReplaceAll(cudaMin, ".", "")
			ref, ok := family[variant]
			if !ok {
				t.Errorf("framework %q missing variant %q", fw, variant)
				continue
			}
			if !strings.Contains(ref, "@sha256:") {
				t.Errorf("framework %q %s ref %q is not digest-pinned", fw, variant, ref)
			}
			if !strings.Contains(ref, fw+"-"+variant) {
				t.Errorf("framework %q %s ref %q does not name repo %s-%s", fw, variant, ref, fw, variant)
			}
			// The resolvable-for-launch contract: FrameworkImageRef returns the ref
			// exactly when it is published (non-placeholder), never a placeholder.
			got, resolvable := m.FrameworkImageRef(fw, cudaMin)
			if IsPlaceholderRef(ref) {
				if resolvable {
					t.Errorf("FrameworkImageRef(%q,%q) = %q, ok=true; want unresolvable while PLACEHOLDER", fw, cudaMin, got)
				}
			} else if !resolvable || got != ref {
				t.Errorf("FrameworkImageRef(%q,%q) = (%q,%v); want (%q,true)", fw, cudaMin, got, resolvable, ref)
			}
		}
	}

	if _, ok := m.FrameworkImageRef("no-such-framework", "12.8"); ok {
		t.Errorf("FrameworkImageRef(unknown framework) ok = true; want false")
	}
	if _, ok := m.FrameworkImageRef("pytorch", "99.9"); ok {
		t.Errorf("FrameworkImageRef(pytorch, unknown gen) ok = true; want false")
	}
}

// TestFrameworks pins the published-roster contract: Frameworks() lists exactly
// the framework ids with at least one non-PLACEHOLDER ref, sorted.
func TestFrameworks(t *testing.T) {
	m := loadEmbeddedFixture(t)

	want := map[string]bool{}
	for fw, family := range m.FrameworkImages {
		for _, ref := range family {
			if !IsPlaceholderRef(ref) {
				want[fw] = true
				break
			}
		}
	}
	got := m.Frameworks()
	if len(got) != len(want) {
		t.Fatalf("Frameworks() = %v; want the %d published frameworks %v", got, len(want), want)
	}
	for i, fw := range got {
		if !want[fw] {
			t.Errorf("Frameworks()[%d] = %q; not a published framework", i, fw)
		}
		if i > 0 && got[i-1] >= fw {
			t.Errorf("Frameworks() not sorted: %v", got)
		}
	}
}

// TestLoad_FrameworkValidation pins the boot-time strictness for framework
// families: an un-pinned ref and a family missing a variant the gpus table uses
// must both crash Load (fail at boot, not at provision time).
func TestLoad_FrameworkValidation(t *testing.T) {
	base := `{
		"schema_version": 1,
		"toolkit_driver_floors": {"12.8": "570.26"},
		"gpus": {
			"B200": {"gpu_model": "B200", "driver_floor": "570.26", "cuda_min": "12.8",
				"image_ref": "ghcr.io/gpuai-dev/base-cuda128:t@sha256:` + strings.Repeat("a", 64) + `"}
		},
		"framework_images": {"pytorch": %s}
	}`

	if _, err := Load([]byte(fmt.Sprintf(base, `{"cuda128": "ghcr.io/gpuai-dev/pytorch-cuda128:latest"}`))); err == nil {
		t.Errorf("Load() with un-pinned framework ref: err = nil; want digest-pin error")
	}
	if _, err := Load([]byte(fmt.Sprintf(base, `{"cuda124": "ghcr.io/gpuai-dev/pytorch-cuda124:t@sha256:`+strings.Repeat("b", 64)+`"}`))); err == nil {
		t.Errorf("Load() with framework missing gpus-used variant cuda128: err = nil; want coverage error")
	}
	if _, err := Load([]byte(fmt.Sprintf(base, `{"cuda128": "ghcr.io/gpuai-dev/pytorch-cuda128:t@sha256:`+strings.Repeat("c", 64)+`"}`))); err != nil {
		t.Errorf("Load() with valid framework family: err = %v; want nil", err)
	}
}

// TestFrameworkIncludes pins the "What's included" version lists (launch-modal
// polish, 2026-07-15): every published framework family plus "base" carries a
// list for BOTH CUDA variants, and every item has a non-empty name. Exact
// versions are deliberately NOT pinned here — they change at every monthly
// re-pin; the shape is the contract.
func TestFrameworkIncludes(t *testing.T) {
	m := loadEmbeddedFixture(t)

	wantFrameworks := append(m.Frameworks(), "base")
	for _, fw := range wantFrameworks {
		variants, ok := m.FrameworkIncludes[fw]
		if !ok {
			t.Errorf("FrameworkIncludes[%q] missing; every published framework + base needs a version list", fw)
			continue
		}
		for _, variant := range []string{"cuda128", "cuda124"} {
			items := variants[variant]
			if len(items) == 0 {
				t.Errorf("FrameworkIncludes[%q][%q] is empty", fw, variant)
			}
			for i, item := range items {
				if strings.TrimSpace(item.Name) == "" {
					t.Errorf("FrameworkIncludes[%q][%q] item %d has empty name", fw, variant, i)
				}
			}
		}
	}
}

// TestLoad_IncludesValidation pins the shape validation: an includes item with
// an empty name fails Load.
func TestLoad_IncludesValidation(t *testing.T) {
	raw := []byte(`{
		"schema_version": 1,
		"toolkit_driver_floors": {},
		"gpus": {},
		"framework_images": {},
		"framework_includes": {"pytorch": {"cuda128": [{"name": "", "version": "1.0"}]}}
	}`)
	if _, err := Load(raw); err == nil {
		t.Fatal("Load() accepted an includes item with an empty name; want error")
	}
}

// TestVMImages pins the raw-VM OS catalog (the launch OS picker) contract:
// the per-provider option lists come back sorted, slug resolution returns the
// provider-NATIVE name, and the fail-safe holds (unknown os / unmapped provider
// resolves to nothing, never a guess). The catalog is supplied inline with
// fictional providers because it is operator data, not a fixed adapter list.
func TestVMImages(t *testing.T) {
	raw := []byte(`{"schema_version":1,"toolkit_driver_floors":{},"gpus":{},"framework_images":{},"vm_images":{
		"ubuntu-22.04": {"label": "Ubuntu 22.04 LTS", "family": "ubuntu", "version": "22.04",
			"providers": {"vmco": "ubuntu_22_04_x64_lts", "metalco": "ubuntu:22.04"}},
		"ubuntu-24.04": {"label": "Ubuntu 24.04 LTS", "family": "ubuntu", "version": "24.04",
			"providers": {"vmco": "ubuntu_24_04_x64_lts", "metalco": "ubuntu:24.04"}},
		"ubuntu-24.04-cuda": {"label": "Ubuntu 24.04 LTS CUDA-ready", "family": "ubuntu", "version": "24.04",
			"driver_preinstalled": true, "providers": {"vmco": "ubuntu24_ml_in_a_box"}}
	}}`)
	m, err := Load(raw)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Every mapped VM provider gets its options, sorted.
	wantOptions := map[string][]string{
		"vmco":    {"ubuntu-22.04", "ubuntu-24.04", "ubuntu-24.04-cuda"},
		"metalco": {"ubuntu-22.04", "ubuntu-24.04"},
	}
	for prov, want := range wantOptions {
		got := m.VMOSOptionsFor(prov)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("VMOSOptionsFor(%q) = %v; want %v", prov, got, want)
		}
	}
	// Unmapped providers (container-native or simply absent) are never mapped.
	for _, prov := range []string{"podco", "community", "no-such"} {
		if got := m.VMOSOptionsFor(prov); got != nil {
			t.Errorf("VMOSOptionsFor(%q) = %v; want nil", prov, got)
		}
	}

	// Slug resolution returns the provider-NATIVE name for a mapped pair and
	// refuses everything else.
	if slug, ok := m.VMImageSlug("metalco", "ubuntu-22.04"); !ok || slug != "ubuntu:22.04" {
		t.Errorf("VMImageSlug(metalco, ubuntu-22.04) = (%q,%v); want (ubuntu:22.04,true)", slug, ok)
	}
	if slug, ok := m.VMImageSlug("vmco", "ubuntu-24.04-cuda"); !ok || slug != "ubuntu24_ml_in_a_box" {
		t.Errorf("VMImageSlug(vmco, ubuntu-24.04-cuda) = (%q,%v); want (ubuntu24_ml_in_a_box,true)", slug, ok)
	}
	if _, ok := m.VMImageSlug("metalco", "ubuntu-24.04-cuda"); ok {
		t.Error("VMImageSlug(metalco, ubuntu-24.04-cuda) resolved; metalco has no driver-baked image")
	}
	if _, ok := m.VMImageSlug("metalco", "no-such-os"); ok {
		t.Error("VMImageSlug with unknown os id resolved; want refusal")
	}
	if !m.HasVMImage("ubuntu-24.04") || m.HasVMImage("no-such-os") {
		t.Error("HasVMImage: want true for ubuntu-24.04, false for no-such-os")
	}
}

// TestEmbeddedVMImagesValid checks that whatever vm_images the embedded
// manifest ships satisfies the wire grammar's id shape and carries the display
// fields the frameworks endpoint serves.
func TestEmbeddedVMImagesValid(t *testing.T) {
	m, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	for osID, img := range m.VMImages {
		if !ValidVMOSID(osID) {
			t.Errorf("vm_images id %q fails ValidVMOSID", osID)
		}
		if img.Label == "" || img.Family == "" || img.Version == "" {
			t.Errorf("vm_images[%q] missing display fields: %+v", osID, img)
		}
	}
}

// TestLoad_VMImagesValidation pins the boot-time shape checks: a bad os id, a
// missing label, and an empty provider slug each fail Load.
func TestLoad_VMImagesValidation(t *testing.T) {
	base := `{"schema_version":1,"toolkit_driver_floors":{},"gpus":{},"framework_images":{},"vm_images":%s}`
	for name, vmImages := range map[string]string{
		"bad id (uppercase)":  `{"Ubuntu-24.04": {"label": "x", "family": "ubuntu", "version": "24.04", "providers": {"metalco": "ubuntu:24.04"}}}`,
		"bad id (colon)":      `{"ubuntu:24.04": {"label": "x", "family": "ubuntu", "version": "24.04", "providers": {"metalco": "ubuntu:24.04"}}}`,
		"empty label":         `{"ubuntu-24.04": {"label": " ", "family": "ubuntu", "version": "24.04", "providers": {"metalco": "ubuntu:24.04"}}}`,
		"missing family":      `{"ubuntu-24.04": {"label": "x", "version": "24.04", "providers": {"metalco": "ubuntu:24.04"}}}`,
		"no providers":        `{"ubuntu-24.04": {"label": "x", "family": "ubuntu", "version": "24.04", "providers": {}}}`,
		"empty provider slug": `{"ubuntu-24.04": {"label": "x", "family": "ubuntu", "version": "24.04", "providers": {"metalco": " "}}}`,
	} {
		raw := []byte(strings.Replace(base, "%s", vmImages, 1))
		if _, err := Load(raw); err == nil {
			t.Errorf("Load() accepted vm_images with %s; want error", name)
		}
	}
}

// TestValidVMOSID pins the id alphabet the wire grammar shares.
func TestValidVMOSID(t *testing.T) {
	for _, ok := range []string{"ubuntu-24.04", "ubuntu-22.04-cuda", "debian-12", "a"} {
		if !ValidVMOSID(ok) {
			t.Errorf("ValidVMOSID(%q) = false; want true", ok)
		}
	}
	for _, bad := range []string{"", "Ubuntu", "-ubuntu", "ubuntu-", ".ubuntu", "ubuntu.", "ubuntu_24", "ubuntu:24.04", strings.Repeat("a", 33)} {
		if ValidVMOSID(bad) {
			t.Errorf("ValidVMOSID(%q) = true; want false", bad)
		}
	}
}

// TestFrameworkVersions pins the embedded version catalog (the launch version
// picker): pytorch exposes its two per-variant versions, each resolving to the
// published family image for its own CUDA variant and refusing on the other.
func TestFrameworkVersions(t *testing.T) {
	m := loadEmbeddedFixture(t)

	vs := m.FrameworkVersionsFor("pytorch")
	if len(vs) < 2 {
		t.Fatalf("FrameworkVersionsFor(pytorch) = %d versions; want >= 2", len(vs))
	}
	byID := map[string]FrameworkVersion{}
	for _, v := range vs {
		if !ValidFrameworkVersionID(v.ID) {
			t.Errorf("version id %q is not a valid id", v.ID)
		}
		byID[v.ID] = v
	}
	// 2.11 targets cuda128, 2.13 targets cuda124 — each resolves on its own
	// variant (the cuda_min that maps there) and is unavailable on the other.
	cases := []struct {
		id, okCUDA, badCUDA string
	}{
		{"2.11", "12.8", "12.4"},
		{"2.13", "12.4", "12.8"},
	}
	for _, c := range cases {
		if _, ok := byID[c.id]; !ok {
			t.Errorf("pytorch version %q missing from catalog", c.id)
			continue
		}
		if ref, ok := m.FrameworkImageRefForVersion("pytorch", c.okCUDA, c.id); !ok || !strings.Contains(ref, "@sha256:") {
			t.Errorf("FrameworkImageRefForVersion(pytorch,%s,%s) = (%q,%v); want a pinned ref", c.okCUDA, c.id, ref, ok)
		}
		if _, ok := m.FrameworkImageRefForVersion("pytorch", c.badCUDA, c.id); ok {
			t.Errorf("FrameworkImageRefForVersion(pytorch,%s,%s) resolved; want refusal (wrong variant)", c.badCUDA, c.id)
		}
	}
	// Unknown version id and a framework with no version catalog both refuse.
	if _, ok := m.FrameworkImageRefForVersion("pytorch", "12.8", "9.9"); ok {
		t.Errorf("FrameworkImageRefForVersion(pytorch, unknown version) resolved; want refusal")
	}
	if _, ok := m.FrameworkImageRefForVersion("jupyter", "12.8", "1.0"); ok {
		t.Errorf("FrameworkImageRefForVersion(jupyter, any version) resolved; jupyter has no version catalog")
	}
}

// TestLoad_FrameworkVersionsValidation pins boot-time strictness for the version
// catalog: a version with no matching framework_images family, a duplicate id, a
// variant the family doesn't cover, and two defaults must all crash Load.
func TestLoad_FrameworkVersionsValidation(t *testing.T) {
	base := `{
		"schema_version": 1,
		"toolkit_driver_floors": {"12.8": "570.26"},
		"gpus": {"B200": {"gpu_model": "B200", "driver_floor": "570.26", "cuda_min": "12.8",
			"image_ref": "ghcr.io/gpuai-dev/base-cuda128:t@sha256:` + strings.Repeat("a", 64) + `"}},
		"framework_images": {"pytorch": {"cuda128": "ghcr.io/gpuai-dev/pytorch-cuda128:t@sha256:` + strings.Repeat("c", 64) + `"}},
		"framework_versions": %s
	}`
	bad := map[string]string{
		"no family":         `{"tensorflow": [{"id": "1.0", "label": "TF 1.0", "variant": "cuda128"}]}`,
		"duplicate id":      `{"pytorch": [{"id": "2.11", "label": "a", "variant": "cuda128"}, {"id": "2.11", "label": "b", "variant": "cuda128"}]}`,
		"uncovered variant": `{"pytorch": [{"id": "2.13", "label": "PyTorch 2.13", "variant": "cuda124"}]}`,
		"two defaults":      `{"pytorch": [{"id": "2.11", "label": "a", "variant": "cuda128", "default": true}, {"id": "2.12", "label": "b", "variant": "cuda128", "default": true}]}`,
		"bad id shape":      `{"pytorch": [{"id": "-2.11", "label": "a", "variant": "cuda128"}]}`,
	}
	for name, fv := range bad {
		if _, err := Load([]byte(fmt.Sprintf(base, fv))); err == nil {
			t.Errorf("Load() with %s: err = nil; want validation error", name)
		}
	}
	good := `{"pytorch": [{"id": "2.11", "label": "PyTorch 2.11", "variant": "cuda128", "default": true}]}`
	if _, err := Load([]byte(fmt.Sprintf(base, good))); err != nil {
		t.Errorf("Load() with a valid version catalog: err = %v; want nil", err)
	}
}
