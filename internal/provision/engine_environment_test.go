// Phase 64 (D-64-01) tests for the certified:<framework> environment grammar
// and the engine's framework image resolution. resolveEnvironment is the trust
// boundary: the pinned ref always comes from the server-controlled manifest —
// a framework the manifest cannot resolve (unknown, uncovered variant, or
// still-PLACEHOLDER) must refuse explicitly, never fall back to the bare base
// or launch unpinned.
package provision

import (
	"errors"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/manifest"
)

// envTestManifest builds a manifest with one Blackwell-floor GPU and one
// cuda124 GPU, a published pytorch family, and an all-PLACEHOLDER jupyter
// family — enough to cover resolve, refuse-placeholder, and refuse-unknown.
func envTestManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	zero := strings.Repeat("0", 64)
	m, err := manifest.Load([]byte(`{
		"schema_version": 1,
		"toolkit_driver_floors": {"12.8": "570.26", "12.4": "550.54.14"},
		"gpus": {
			"B200": {"gpu_model": "B200", "driver_floor": "570.26", "cuda_min": "12.8",
				"image_ref": "ghcr.io/gpuai-dev/base-cuda128:t@sha256:` + strings.Repeat("a", 64) + `"},
			"H100 SXM": {"gpu_model": "H100 SXM", "driver_floor": "550.54.14", "cuda_min": "12.4",
				"image_ref": "ghcr.io/gpuai-dev/base-cuda124:t@sha256:` + strings.Repeat("b", 64) + `"}
		},
		"framework_images": {
			"pytorch": {
				"cuda128": "ghcr.io/gpuai-dev/pytorch-cuda128:t@sha256:` + strings.Repeat("c", 64) + `",
				"cuda124": "ghcr.io/gpuai-dev/pytorch-cuda124:t@sha256:` + strings.Repeat("d", 64) + `"
			},
			"jupyter": {
				"cuda128": "ghcr.io/gpuai-dev/jupyter-cuda128:PLACEHOLDER@sha256:` + zero + `",
				"cuda124": "ghcr.io/gpuai-dev/jupyter-cuda124:PLACEHOLDER@sha256:` + zero + `"
			}
		},
		"framework_versions": {
			"pytorch": [
				{"id": "2.11", "label": "PyTorch 2.11", "variant": "cuda128", "default": true},
				{"id": "2.13", "label": "PyTorch 2.13", "variant": "cuda124"}
			]
		},
		"vm_images": {
			"ubuntu-24.04": {
				"label": "Ubuntu 24.04 LTS", "family": "ubuntu", "version": "24.04",
				"providers": {"vmco": "Ubuntu Server 24.04 LTS (Noble Numbat)"}
			}
		}
	}`))
	if err != nil {
		t.Fatalf("test manifest: %v", err)
	}
	return m
}

func TestParseEnvironmentSelection(t *testing.T) {
	cases := []struct {
		in, class, framework string
	}{
		{"certified", "certified", ""},
		{"certified:pytorch", "certified", "pytorch"},
		{"certified:cuda-devel", "certified", "cuda-devel"},
		{"raw_vm", "raw_vm", ""},
		{"raw_vm:ubuntu-24.04", "raw_vm", "ubuntu-24.04"},
		{"provider_template", "provider_template", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		class, fw := ParseEnvironmentSelection(c.in)
		if class != c.class || fw != c.framework {
			t.Errorf("ParseEnvironmentSelection(%q) = (%q,%q); want (%q,%q)", c.in, class, fw, c.class, c.framework)
		}
	}
}

func TestResolveEnvironment_Framework(t *testing.T) {
	e := &Engine{manifest: envTestManifest(t)}

	// Published framework on a container-native provider: the adapter image is
	// the FRAMEWORK ref, not the base — and no nested docker block (mirrors the
	// bare-certified container path).
	res, err := e.resolveEnvironment(ProvisionRequest{Environment: "certified:pytorch"}, "podco", "b200")
	if err != nil {
		t.Fatalf("certified:pytorch on podco/b200: %v", err)
	}
	if want := "ghcr.io/gpuai-dev/pytorch-cuda128:t@sha256:" + strings.Repeat("c", 64); res.dockerImageOverride != want {
		t.Errorf("dockerImageOverride = %q; want %q", res.dockerImageOverride, want)
	}
	if res.certifiedImage || res.pinnedImageRef != "" {
		t.Errorf("container-native framework path must not set the VM bootstrap fields; got %+v", res)
	}

	// marketco is container-native too: certified frameworks must take the adapter
	// image override path, never the nested-docker VM path (which the marketco
	// adapter can't run — it silently boots its stock default image instead;
	// live-caught on demo 2026-07-16).
	res, err = e.resolveEnvironment(ProvisionRequest{Environment: "certified:pytorch"}, "marketco", "b200")
	if err != nil {
		t.Fatalf("certified:pytorch on marketco/b200: %v", err)
	}
	if want := "ghcr.io/gpuai-dev/pytorch-cuda128:t@sha256:" + strings.Repeat("c", 64); res.dockerImageOverride != want {
		t.Errorf("marketco dockerImageOverride = %q; want %q", res.dockerImageOverride, want)
	}
	if res.certifiedImage || res.pinnedImageRef != "" {
		t.Errorf("marketco framework path must not set the VM bootstrap fields; got %+v", res)
	}

	// Published framework on a certified VM provider: pinned ref is the
	// framework ref for the GPU's OWN CUDA family (cuda124 here), floors intact.
	res, err = e.resolveEnvironment(ProvisionRequest{Environment: "certified:pytorch"}, "vmco", "h100_sxm")
	if err != nil {
		t.Fatalf("certified:pytorch on vmco/h100_sxm: %v", err)
	}
	if want := "ghcr.io/gpuai-dev/pytorch-cuda124:t@sha256:" + strings.Repeat("d", 64); res.pinnedImageRef != want {
		t.Errorf("pinnedImageRef = %q; want %q (the cuda124 family ref)", res.pinnedImageRef, want)
	}
	if !res.certifiedImage || res.driverFloor != "550.54.14" {
		t.Errorf("VM framework path must keep certified bootstrap + driver floor; got %+v", res)
	}

	// Bare certified is untouched by the framework machinery.
	res, err = e.resolveEnvironment(ProvisionRequest{Environment: "certified"}, "vmco", "b200")
	if err != nil {
		t.Fatalf("bare certified: %v", err)
	}
	if want := "ghcr.io/gpuai-dev/base-cuda128:t@sha256:" + strings.Repeat("a", 64); res.pinnedImageRef != want {
		t.Errorf("bare certified pinnedImageRef = %q; want base ref %q", res.pinnedImageRef, want)
	}

	// Refusals: unknown framework, still-PLACEHOLDER framework, and a sub token
	// on a class that takes none. NEVER a silent fallback to the base image.
	for _, c := range []struct{ env, gpu string }{
		{"certified:tensorflow", "b200"},
		{"certified:jupyter", "b200"},
		{"provider_template:pytorch", "h100_sxm"},
	} {
		if _, err := e.resolveEnvironment(ProvisionRequest{Environment: c.env}, "vmco", c.gpu); !errors.Is(err, ErrFrameworkUnavailable) {
			t.Errorf("resolveEnvironment(%q) err = %v; want ErrFrameworkUnavailable", c.env, err)
		}
	}
}

func TestParseFrameworkVersion(t *testing.T) {
	cases := []struct{ in, fw, ver string }{
		{"pytorch", "pytorch", ""},
		{"pytorch@2.13", "pytorch", "2.13"},
		{"cuda-devel", "cuda-devel", ""},
	}
	for _, c := range cases {
		fw, ver := ParseFrameworkVersion(c.in)
		if fw != c.fw || ver != c.ver {
			t.Errorf("ParseFrameworkVersion(%q) = (%q,%q); want (%q,%q)", c.in, fw, ver, c.fw, c.ver)
		}
	}
}

// TestResolveEnvironment_FrameworkVersion pins the launch version picker's
// engine path: `certified:<fw>@<ver>` resolves the version's image on a GPU
// whose CUDA variant matches, and refuses (ErrFrameworkUnavailable) when the
// version targets a different variant than the placed GPU.
func TestResolveEnvironment_FrameworkVersion(t *testing.T) {
	e := &Engine{manifest: envTestManifest(t)}

	// pytorch@2.11 targets cuda128 → resolves on B200 (cuda_min 12.8), container
	// override is the cuda128 family ref.
	res, err := e.resolveEnvironment(ProvisionRequest{Environment: "certified:pytorch@2.11"}, "podco", "b200")
	if err != nil {
		t.Fatalf("certified:pytorch@2.11 on podco/b200: %v", err)
	}
	if want := "ghcr.io/gpuai-dev/pytorch-cuda128:t@sha256:" + strings.Repeat("c", 64); res.dockerImageOverride != want {
		t.Errorf("dockerImageOverride = %q; want cuda128 ref %q", res.dockerImageOverride, want)
	}

	// pytorch@2.13 targets cuda124 → resolves on H100 SXM (cuda_min 12.4).
	res, err = e.resolveEnvironment(ProvisionRequest{Environment: "certified:pytorch@2.13"}, "vmco", "h100_sxm")
	if err != nil {
		t.Fatalf("certified:pytorch@2.13 on vmco/h100_sxm: %v", err)
	}
	if want := "ghcr.io/gpuai-dev/pytorch-cuda124:t@sha256:" + strings.Repeat("d", 64); res.pinnedImageRef != want {
		t.Errorf("pinnedImageRef = %q; want cuda124 ref %q", res.pinnedImageRef, want)
	}

	// Cross-variant pick refuses: 2.11 (cuda128) on an H100 SXM (cuda124) GPU,
	// and an unknown version id both fail.
	for _, env := range []string{"certified:pytorch@2.11", "certified:pytorch@9.9"} {
		if _, err := e.resolveEnvironment(ProvisionRequest{Environment: env}, "vmco", "h100_sxm"); !errors.Is(err, ErrFrameworkUnavailable) {
			t.Errorf("resolveEnvironment(%q) on h100_sxm err = %v; want ErrFrameworkUnavailable", env, err)
		}
	}
}

func TestResolveEnvironment_RawVMOS(t *testing.T) {
	e := &Engine{manifest: envTestManifest(t)}

	// raw_vm:<os> on a provider the vm_images map covers: the os id is kept for
	// the per-attempt slug resolution, the VM shape + driver floor are the same
	// as bare raw_vm, and no certified/pinned fields leak in.
	res, err := e.resolveEnvironment(ProvisionRequest{Environment: "raw_vm:ubuntu-24.04"}, "vmco", "h100_sxm")
	if err != nil {
		t.Fatalf("raw_vm:ubuntu-24.04 on vmco: %v", err)
	}
	if res.osID != "ubuntu-24.04" || res.deploymentType != "vm" || res.driverFloor != "550.54.14" {
		t.Errorf("raw_vm:<os> resolution = %+v; want osID=ubuntu-24.04, vm shape, driver floor intact", res)
	}
	if res.certifiedImage || res.pinnedImageRef != "" || res.dockerImageOverride != "" {
		t.Errorf("raw_vm:<os> must not set certified/image fields; got %+v", res)
	}

	// Bare raw_vm keeps carrying no os id (provider default boot image).
	res, err = e.resolveEnvironment(ProvisionRequest{Environment: "raw_vm"}, "vmco", "h100_sxm")
	if err != nil || res.osID != "" {
		t.Errorf("bare raw_vm = (%+v, %v); want empty osID, nil error", res, err)
	}

	// Refusals — NEVER boot a different OS than asked: unknown os id, an os the
	// placed provider has no verified slug for, and a nil manifest.
	if _, err := e.resolveEnvironment(ProvisionRequest{Environment: "raw_vm:no-such-os"}, "vmco", "h100_sxm"); !errors.Is(err, ErrOSUnavailable) {
		t.Errorf("unknown os id err = %v; want ErrOSUnavailable", err)
	}
	if _, err := e.resolveEnvironment(ProvisionRequest{Environment: "raw_vm:ubuntu-24.04"}, "cloudvmco", "h100_sxm"); !errors.Is(err, ErrOSUnavailable) {
		t.Errorf("unmapped provider err = %v; want ErrOSUnavailable", err)
	}
	eNil := &Engine{}
	if _, err := eNil.resolveEnvironment(ProvisionRequest{Environment: "raw_vm:ubuntu-24.04"}, "vmco", "h100_sxm"); !errors.Is(err, ErrOSUnavailable) {
		t.Errorf("nil manifest err = %v; want ErrOSUnavailable", err)
	}
}
