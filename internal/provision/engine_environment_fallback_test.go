package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/tunnel"
)

// A certified request must keep its framework when fallback changes the
// provider's deployment class. Fake providers only; no cloud calls.
func TestProvision_CertifiedEnvironmentFollowsFallback(t *testing.T) {
	for _, tc := range []struct{ first, second, wantImage string }{
		{"vmco", "marketco", "ghcr.io/gpuai-dev/pytorch-cuda124:t@sha256:" + strings.Repeat("d", 64)},
		{"marketco", "vmco", ""},
	} {
		t.Run(tc.first+"_to_"+tc.second, func(t *testing.T) {
			first := fallbackStub(tc.first, 1, false, "unused")
			first.provisionErr = provider.ErrNoCapacity
			second := fallbackStub(tc.second, 2, false, "owned-test-upstream")
			first.offerings[0].GPUType = provider.GPUTypeH100SXM
			second.offerings[0].GPUType = provider.GPUTypeH100SXM
			engine := newFallbackEngine(fakeWithOneKey(), first, second)
			engine.manifest = envTestManifest(t)
			req := onDemandRequest()
			req.GPUType = provider.GPUTypeH100SXM
			req.Environment = "certified:pytorch"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if _, err := engine.Provision(ctx, req); err != nil {
				t.Fatalf("provision: %v", err)
			}
			if len(first.provisionReqs) != 1 || len(second.provisionReqs) != 1 {
				t.Fatal("expected one failed first candidate and one successful fallback")
			}
			if got := second.provisionReqs[0].DockerImage; got != tc.wantImage {
				t.Errorf("fallback DockerImage = %q; want %q for %s", got, tc.wantImage, tc.second)
			}
		})
	}
}

type unavailableEnvironmentStage struct{ *fakeEngineDB }

func (unavailableEnvironmentStage) StageInstanceBootstrap(context.Context, string, string) error {
	return errors.New("test staging unavailable")
}

func TestWalk_CertifiedEnvironmentStageFailureDoesNotLaunch(t *testing.T) {
	stub := fallbackStub("vmco", 1, false, "must-not-be-created")
	stub.offerings[0].GPUType = provider.GPUTypeH100SXM
	fake := fakeWithOneKey()
	engine := newFallbackEngine(fake, stub)
	engine.db = unavailableEnvironmentStage{fake}
	engine.manifest = envTestManifest(t)
	request := &provider.ProvisionRequest{GPUType: provider.GPUTypeH100SXM, GPUCount: 1, Tier: provider.TierOnDemand}
	_, _, _, err := engine.walkProvisionCandidates(context.Background(), request,
		[]ProviderCandidate{floorTestCandidate(stub, stub.offerings[0])},
		provisionWalkParams{environment: "certified:pytorch", bootstrapData: transportTestBootstrapData()})
	if err == nil {
		t.Fatal("expected staging failure")
	}
	if len(stub.provisionReqs) != 0 {
		t.Fatal("launched a paid allocation despite failed environment staging")
	}
}

func TestWalk_CertifiedFallbackRestagesMatchingBootstrap(t *testing.T) {
	for _, names := range [][2]string{{"marketco", "vmco"}, {"vmco", "marketco"}} {
		t.Run(names[0]+"_to_"+names[1], func(t *testing.T) {
			fake := fakeWithOneKey()
			first := fallbackStub(names[0], 1, false, "unused")
			first.provisionErr = provider.ErrNoCapacity
			second := fallbackStub(names[1], 2, false, "owned-test-upstream")
			first.offerings[0].GPUType = provider.GPUTypeH100SXM
			second.offerings[0].GPUType = provider.GPUTypeH100SXM
			engine := newFallbackEngine(fake, first, second)
			engine.manifest = envTestManifest(t)
			selection := ProvisionRequest{Environment: "certified:pytorch"}
			initial, err := engine.resolveEnvironment(selection, names[0], "h100_sxm")
			if err != nil {
				t.Fatal(err)
			}
			data := transportTestBootstrapData()
			data.CertifiedImage = initial.certifiedImage
			data.DeploymentType = initial.deploymentType
			data.PinnedImageRef = initial.pinnedImageRef
			data.DriverFloor = initial.driverFloor
			script, err := tunnel.RenderBootstrap(*data)
			if err != nil {
				t.Fatal(err)
			}
			request := &provider.ProvisionRequest{
				InstanceID: data.InstanceID, GPUType: provider.GPUTypeH100SXM,
				GPUCount: 1, Tier: provider.TierOnDemand,
				DockerImage: initial.dockerImageOverride, StartupScript: script,
			}
			_, _, _, err = engine.walkProvisionCandidates(context.Background(), request,
				[]ProviderCandidate{floorTestCandidate(first, first.offerings[0]), floorTestCandidate(second, second.offerings[0])},
				provisionWalkParams{instanceID: data.InstanceID, environment: selection.Environment, bootstrapData: data})
			if err != nil {
				t.Fatal(err)
			}
			if len(second.provisionReqs) != 1 || len(fake.stagedBootstraps) != 2 {
				t.Fatalf("want one fallback and two environment stages; got %d and %d", len(second.provisionReqs), len(fake.stagedBootstraps))
			}
			delivered := second.provisionReqs[0].StartupScript
			if delivered != fake.stagedBootstraps[len(fake.stagedBootstraps)-1] {
				t.Fatal("provider and staged bootstrap differ")
			}
			hasVMImage := strings.Contains(delivered, "PINNED_IMAGE=\"ghcr.io/gpuai-dev/pytorch-cuda124:")
			hasVMFloor := strings.Contains(delivered, "DRIVER_FLOOR=\"550.54.14\"")
			if wantVM := names[1] == "vmco"; hasVMImage != wantVM || hasVMFloor != wantVM {
				t.Fatalf("fallback VM image/floor = %v/%v; want %v", hasVMImage, hasVMFloor, wantVM)
			}
			// The reusable initial data must not be mutated by a fallback.
			if data.DeploymentType != initial.deploymentType || data.CertifiedImage != initial.certifiedImage {
				t.Fatal("walk mutated the initial bootstrap data")
			}
		})
	}
}
