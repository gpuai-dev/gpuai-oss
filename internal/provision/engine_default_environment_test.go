package provision

import (
	"context"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/tunnel"
)

// Launches that name no environment used to take each adapter's hardcoded
// image: CUDA 12.4 PyTorch on the container providers (no Blackwell kernels)
// and a bare OS on the VM providers. Measured on the live API on 2026-09-20 and
// again on 2026-10-06: every default-path RTX 5090/5080 launch failed with
// "no kernel image is available", and every default-path VM launch delivered
// no framework, all billed as ready. These tests pin the default environment
// and the cases where it must NOT apply. Fake providers only; no cloud calls.

var (
	testPytorch128 = "ghcr.io/gpuai-dev/pytorch-cuda128:t@sha256:" + strings.Repeat("c", 64)
	testPytorch124 = "ghcr.io/gpuai-dev/pytorch-cuda124:t@sha256:" + strings.Repeat("d", 64)
	testBase128    = "ghcr.io/gpuai-dev/base-cuda128:t@sha256:" + strings.Repeat("a", 64)
)

func defaultEnvEngine(t *testing.T, provs ...provider.Provider) *Engine {
	t.Helper()
	e := newFallbackEngine(fakeWithOneKey(), provs...)
	e.manifest = envTestManifest(t)
	e.defaultEnvironment = "certified:pytorch"
	return e
}

func defaultEnvStub(name string, gpu provider.GPUType, price float64) *stubProvider {
	s := fallbackStub(name, price, false, "up-"+name)
	s.offerings[0].GPUType = gpu
	return s
}

func defaultEnvRequest(gpu provider.GPUType) ProvisionRequest {
	req := onDemandRequest()
	req.GPUType = gpu
	return req
}

func provisionOK(t *testing.T, e *Engine, req ProvisionRequest) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := e.Provision(ctx, req); err != nil {
		t.Fatalf("provision: %v", err)
	}
}

func onlyImage(t *testing.T, s *stubProvider) string {
	t.Helper()
	if len(s.provisionReqs) != 1 {
		t.Fatalf("%s: %d provision calls; want 1", s.name, len(s.provisionReqs))
	}
	return s.provisionReqs[0].DockerImage
}

func TestProvision_DefaultEnvironment_PicksFrameworkForGPUGeneration(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prov      string
		gpu       provider.GPUType
		all       bool
		wantImage string
	}{
		{"blackwell container gets cuda 12.8 pytorch", "marketco", provider.GPUTypeB200, false, testPytorch128},
		// The legacy CUDA 12.4 images run Hopper, are cached on the host and
		// boot in seconds: by default they are left alone.
		{"hopper keeps the fast legacy image", "podco", provider.GPUTypeH100SXM, false, ""},
		{"hopper with scope all gets cuda 12.4 pytorch", "podco", provider.GPUTypeH100SXM, true, testPytorch124},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := defaultEnvStub(tc.prov, tc.gpu, 2)
			e := defaultEnvEngine(t, stub)
			e.defaultEnvironmentAll = tc.all
			provisionOK(t, e, defaultEnvRequest(tc.gpu))
			if got := onlyImage(t, stub); got != tc.wantImage {
				t.Errorf("DockerImage = %q; want %q", got, tc.wantImage)
			}
		})
	}
}

func TestCUDAVersionAbove(t *testing.T) {
	for _, tc := range []struct {
		v, than string
		want    bool
	}{
		{"12.8", "12.4", true}, {"13.0", "12.4", true}, {"12.4", "12.4", false},
		{"12.1", "12.4", false}, {"11.8", "12.4", false}, {"", "12.4", false}, {"12", "12.4", false},
	} {
		if got := cudaVersionAbove(tc.v, tc.than); got != tc.want {
			t.Errorf("cudaVersionAbove(%q, %q) = %v; want %v", tc.v, tc.than, got, tc.want)
		}
	}
}

func TestProvision_DefaultEnvironment_LeavesExplicitChoicesAlone(t *testing.T) {
	t.Run("explicit certified base", func(t *testing.T) {
		stub := defaultEnvStub("marketco", provider.GPUTypeB200, 2)
		req := defaultEnvRequest(provider.GPUTypeB200)
		req.Environment = "certified"
		provisionOK(t, defaultEnvEngine(t, stub), req)
		if got := onlyImage(t, stub); got != testBase128 {
			t.Errorf("DockerImage = %q; want the certified base %q", got, testBase128)
		}
	})
	t.Run("template image", func(t *testing.T) {
		stub := defaultEnvStub("marketco", provider.GPUTypeB200, 2)
		req := defaultEnvRequest(provider.GPUTypeB200)
		req.Image = "ghcr.io/gpuai-dev/vllm:t"
		provisionOK(t, defaultEnvEngine(t, stub), req)
		if got := onlyImage(t, stub); got != req.Image {
			t.Errorf("DockerImage = %q; want the template image %q", got, req.Image)
		}
	})
	t.Run("default turned off", func(t *testing.T) {
		stub := defaultEnvStub("marketco", provider.GPUTypeB200, 2)
		e := defaultEnvEngine(t, stub)
		e.defaultEnvironment = ""
		provisionOK(t, e, defaultEnvRequest(provider.GPUTypeB200))
		if got := onlyImage(t, stub); got != "" {
			t.Errorf("DockerImage = %q; want the adapter default (empty)", got)
		}
	})
}

// A GPU the manifest does not cover keeps the legacy path instead of failing in
// resolveEnvironment with ErrCertifiedUnavailable.
func TestProvision_DefaultEnvironment_UncoveredGPUKeepsLegacy(t *testing.T) {
	stub := defaultEnvStub("marketco", provider.GPUTypeA10080GB, 2)
	provisionOK(t, defaultEnvEngine(t, stub), defaultEnvRequest(provider.GPUTypeA10080GB))
	if got := onlyImage(t, stub); got != "" {
		t.Errorf("DockerImage = %q; want the adapter default (empty)", got)
	}
}

// The default never turns a launch that would have placed into a refusal: with
// only capacity that cannot run the certified image, or only certified-capable
// capacity above the customer's cap, the launch keeps the legacy candidate set.
func TestProvision_DefaultEnvironment_NeverNarrowsToRefusal(t *testing.T) {
	t.Run("no certified-capable capacity", func(t *testing.T) {
		fixed := defaultEnvStub("fixedimageco", provider.GPUTypeB200, 2)
		provisionOK(t, defaultEnvEngine(t, fixed), defaultEnvRequest(provider.GPUTypeB200))
		if got := onlyImage(t, fixed); got != "" {
			t.Errorf("DockerImage = %q; want the provider's own template (empty)", got)
		}
	})
	t.Run("certified capacity above the cap", func(t *testing.T) {
		fixed := defaultEnvStub("fixedimageco", provider.GPUTypeB200, 1)
		pricey := defaultEnvStub("marketco", provider.GPUTypeB200, 50)
		e := defaultEnvEngine(t, fixed, pricey)
		req := defaultEnvRequest(provider.GPUTypeB200)
		capPrice := e.retailWithDisk(context.Background(), 1, "fixedimageco", e.effectiveDiskGB(req)) + 0.5
		req.MaxPricePerHour = &capPrice
		provisionOK(t, e, req)
		if len(pricey.provisionReqs) != 0 {
			t.Fatal("placed on the over-cap certified candidate")
		}
		if got := onlyImage(t, fixed); got != "" {
			t.Errorf("DockerImage = %q; want the provider's own template (empty)", got)
		}
	})
}

// With the default applied, a container -> VM fallback must not carry the
// container's image override onto the VM (the environment-fallback fix), and
// the VM keeps the legacy bare-VM path (see the walk test below).
func TestProvision_DefaultEnvironment_FollowsFallbackAcrossDeploymentClass(t *testing.T) {
	container := defaultEnvStub("marketco", provider.GPUTypeB200, 1)
	container.provisionErr = provider.ErrNoCapacity
	vm := defaultEnvStub("vmco", provider.GPUTypeB200, 2)
	provisionOK(t, defaultEnvEngine(t, container, vm), defaultEnvRequest(provider.GPUTypeB200))
	if len(container.provisionReqs) != 1 {
		t.Fatalf("container candidate tried %d times; want 1", len(container.provisionReqs))
	}
	if got := container.provisionReqs[0].DockerImage; got != testPytorch128 {
		t.Errorf("container DockerImage = %q; want %q", got, testPytorch128)
	}
	if got := onlyImage(t, vm); got != "" {
		t.Errorf("VM fallback DockerImage = %q; want empty (the legacy bare VM)", got)
	}
}

// The default is container-native only. On a VM provider it would take the
// certified-VM branch and move the customer's SSH from root on the VM into a
// container on :2222, so a launch that named no environment keeps today's
// bare-VM path there. Every VM provider is listed: a forgotten one would
// silently change what those customers get.
func TestPlacementEnvironment_DefaultIsContainerNativeOnly(t *testing.T) {
	const def = "certified:pytorch"
	// Container providers get the default exactly where they run our image
	// natively (SupportsCustomImage). templateonlyco takes template images but
	// not customer images, and the test must hold for both kinds of provider.
	for _, name := range []string{"marketco", "podco", "generic", "templateonlyco"} {
		want := ""
		if SupportsCustomImage(name) {
			want = def
		}
		if got := placementEnvironment("", def, name); got != want {
			t.Errorf("%s (container): default = %q; want %q", name, got, want)
		}
	}
	for _, name := range []string{"marketco", "podco", "generic"} {
		if !SupportsCustomImage(name) {
			t.Fatalf("%s no longer runs custom images; this test's premise changed", name)
		}
	}
	for _, name := range []string{"vmco", "metalco", "baremetalco", "vultr", "startupvmco", "cloudvmco", "vmimageco", "fixedimageco"} {
		if got := placementEnvironment("", def, name); got != "" {
			t.Errorf("%s: default = %q; want none (keep the legacy path)", name, got)
		}
		// An explicit choice is the customer's and applies everywhere.
		if got := placementEnvironment("certified", def, name); got != "certified" {
			t.Errorf("%s: explicit environment = %q; want it kept", name, got)
		}
	}
}

// Across a fallback that changes provider class, each candidate gets the
// default only if it is container-native: the container gets the certified
// framework image and a check that expects PyTorch; the VM gets the legacy
// bootstrap (no pinned image, no driver floor, no framework promised), never
// the default as if the customer had asked for it.
func TestWalk_DefaultEnvironmentFollowsPlacementClass(t *testing.T) {
	for _, names := range [][2]string{{"marketco", "vmco"}, {"vmco", "marketco"}} {
		t.Run(names[0]+"_to_"+names[1], func(t *testing.T) {
			fake := fakeWithOneKey()
			first := defaultEnvStub(names[0], provider.GPUTypeB200, 1)
			first.provisionErr = provider.ErrNoCapacity
			second := defaultEnvStub(names[1], provider.GPUTypeB200, 2)
			e := defaultEnvEngine(t, first, second)
			e.db = fake
			data := transportTestBootstrapData()
			data.GPUCheckMode = "shadow"
			data.ExpectedGPUCount = 1
			script, err := tunnel.RenderBootstrap(*data)
			if err != nil {
				t.Fatal(err)
			}
			request := &provider.ProvisionRequest{
				InstanceID: data.InstanceID, GPUType: provider.GPUTypeB200,
				GPUCount: 1, Tier: provider.TierOnDemand, StartupScript: script,
			}
			_, _, _, err = e.walkProvisionCandidates(context.Background(), request,
				[]ProviderCandidate{floorTestCandidate(first, first.offerings[0]), floorTestCandidate(second, second.offerings[0])},
				provisionWalkParams{instanceID: data.InstanceID, defaultEnvironment: "certified:pytorch", bootstrapData: data})
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range []*stubProvider{first, second} {
				if len(s.provisionReqs) != 1 {
					t.Fatalf("%s: %d provision calls; want 1", s.name, len(s.provisionReqs))
				}
				got := s.provisionReqs[0]
				vmImage := strings.Contains(got.StartupScript, `PINNED_IMAGE="`) || strings.Contains(got.StartupScript, `DRIVER_FLOOR="`)
				expectsTorch := strings.Contains(got.StartupScript, "'pytorch' < /tmp/gpuai-gpucheck.py")
				if s.name == "marketco" {
					if got.DockerImage != testPytorch128 || vmImage || !expectsTorch {
						t.Errorf("marketco: image=%q vm-bootstrap=%v expects-pytorch=%v; want the certified framework image, container bootstrap, pytorch expected",
							got.DockerImage, vmImage, expectsTorch)
					}
					continue
				}
				if got.DockerImage != "" || vmImage || expectsTorch {
					t.Errorf("%s: image=%q vm-bootstrap=%v expects-pytorch=%v; want the legacy bare VM with no framework promised",
						s.name, got.DockerImage, vmImage, expectsTorch)
				}
			}
		})
	}
}
