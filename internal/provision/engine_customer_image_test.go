package provision

// Placement regression tests for customer images (Phases 1+3). THE bug these
// pin: the template-deploy image gate (requireCustomImage &&
// !SupportsCustomImage) silently dropped every VM provider from
// customer-image placement, so a pinned VM offering always failed "no longer
// available" — caught live in the Phase-3 demo E2E, not by review or tests.

import (
	"context"
	"errors"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

func customerImageReq() ProvisionRequest {
	return ProvisionRequest{
		GPUType:       provider.GPUTypeH100SXM,
		GPUCount:      1,
		Tier:          provider.TierOnDemand,
		Image:         "ubuntu@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CustomerImage: true,
	}
}

// A customer image must be placeable on a VM-class certified-capable provider
// when no container-native capacity exists (Phase 3's whole point).
func TestSelectProviderCandidates_CustomerImageVMOnly(t *testing.T) {
	hyper := &stubProvider{
		name:      "vmco",
		offerings: []provider.GPUOffering{diskOffering("vmco", provider.GPUTypeH100SXM, "us-east", 2.40)},
	}
	e := newTestEngineWithProviders(hyper)

	candidates, err := e.SelectProviderCandidates(context.Background(), customerImageReq())
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Prov.Name() != "vmco" {
		t.Fatalf("customer image on VM-only capacity: got %d candidates, want vmco to qualify", len(candidates))
	}
}

// Container-native capacity is preferred when both classes match (the
// homogeneity rule: one class per launch, container first).
func TestSelectProviderCandidates_CustomerImagePrefersContainer(t *testing.T) {
	hyper := &stubProvider{
		name:      "vmco",
		offerings: []provider.GPUOffering{diskOffering("vmco", provider.GPUTypeH100SXM, "us-east", 2.40)},
	}
	rp := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{diskOffering("podco", provider.GPUTypeH100SXM, "us-west", 2.80)},
	}
	e := newTestEngineWithProviders(hyper, rp)

	candidates, err := e.SelectProviderCandidates(context.Background(), customerImageReq())
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	// vmco is cheaper, but the class filter must keep only podco.
	if len(candidates) != 1 || candidates[0].Prov.Name() != "podco" {
		t.Fatalf("mixed classes: got %d candidates (first %s), want container-native podco only",
			len(candidates), candidates[0].Prov.Name())
	}
}

// A private image (RegistryAuth set) must NEVER place on VM capacity — the
// login would have to ride the staged bootstrap.
func TestSelectProviderCandidates_PrivateImageNeverVM(t *testing.T) {
	hyper := &stubProvider{
		name:      "vmco",
		offerings: []provider.GPUOffering{diskOffering("vmco", provider.GPUTypeH100SXM, "us-east", 2.40)},
	}
	e := newTestEngineWithProviders(hyper)

	req := customerImageReq()
	req.RegistryAuth = &provider.RegistryAuth{Registry: "ghcr.io", Username: "u", Password: "p"}
	_, err := e.SelectProviderCandidates(context.Background(), req)
	if err == nil {
		t.Fatal("private customer image placed on VM-only capacity")
	}
}

// A rescue's locked class is a hard cut: locked "container" with only VM
// capacity left must fail loudly, never rebind cross-class.
func TestSelectProviderCandidates_LockedClassHardCut(t *testing.T) {
	hyper := &stubProvider{
		name:      "vmco",
		offerings: []provider.GPUOffering{diskOffering("vmco", provider.GPUTypeH100SXM, "us-east", 2.40)},
	}
	e := newTestEngineWithProviders(hyper)

	req := customerImageReq()
	req.LockedDeploymentClass = "container"
	_, err := e.SelectProviderCandidates(context.Background(), req)
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("locked container class with VM-only capacity: got %v, want ErrNoProvider", err)
	}
}

// The ENTRYPOINT steer (2026-09-02 live finding): an image whose CONFIG
// defines an ENTRYPOINT must not place on a provider whose bootstrap rides
// that entrypoint (podco) while entrypoint-safe container capacity exists.
func TestSelectProviderCandidates_EntrypointSteersOffEntrypointArgProvider(t *testing.T) {
	rp := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{diskOffering("podco", provider.GPUTypeH100SXM, "us-west", 2.40)},
	}
	marketco := &stubProvider{
		name:      "marketco",
		offerings: []provider.GPUOffering{diskOffering("marketco", provider.GPUTypeH100SXM, "us-east", 2.80)},
	}
	e := newTestEngineWithProviders(rp, marketco)

	req := customerImageReq()
	req.ImageConfigEntrypoint = []string{"docker-entrypoint.sh"}
	candidates, err := e.SelectProviderCandidates(context.Background(), req)
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	// podco is cheaper but must be steered away; marketco carries the launch.
	if len(candidates) != 1 || candidates[0].Prov.Name() != "marketco" {
		t.Fatalf("entrypoint-bearing image: got %d candidates (first %s), want marketco only",
			len(candidates), candidates[0].Prov.Name())
	}
}

// When the steer empties the set (e.g. a pinned podco offering), the error
// must be the entrypoint-specific one — not "no longer available", which
// reads as transient churn and invites a retry that fails identically.
func TestSelectProviderCandidates_EntrypointOnlyArgProviderTypedError(t *testing.T) {
	rp := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{diskOffering("podco", provider.GPUTypeH100SXM, "us-west", 2.40)},
	}
	e := newTestEngineWithProviders(rp)

	req := customerImageReq()
	req.ImageConfigEntrypoint = []string{"/opt/nvidia/nvidia_entrypoint.sh"}
	_, err := e.SelectProviderCandidates(context.Background(), req)
	if !errors.Is(err, ErrImageEntrypointUnavailable) {
		t.Fatalf("entrypoint image with podco-only capacity: got %v, want ErrImageEntrypointUnavailable", err)
	}
}

// An entrypoint-less image keeps its podco capacity — the steer must not
// shrink placement for the common case.
func TestSelectProviderCandidates_NoEntrypointKeepsArgProvider(t *testing.T) {
	rp := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{diskOffering("podco", provider.GPUTypeH100SXM, "us-west", 2.40)},
	}
	e := newTestEngineWithProviders(rp)

	candidates, err := e.SelectProviderCandidates(context.Background(), customerImageReq())
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Prov.Name() != "podco" {
		t.Fatalf("entrypoint-less image: got %d candidates, want podco to qualify", len(candidates))
	}
}

// An API-level entrypoint override must NOT bypass the steer: the container
// runtime applies the image CONFIG's entrypoint regardless of what the
// rendered launch script execs, so the steer keys on ImageConfigEntrypoint
// even when the effective (overridden) entrypoint is empty.
func TestSelectProviderCandidates_OverrideDoesNotBypassSteer(t *testing.T) {
	rp := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{diskOffering("podco", provider.GPUTypeH100SXM, "us-west", 2.40)},
	}
	e := newTestEngineWithProviders(rp)

	req := customerImageReq()
	req.ImageConfigEntrypoint = []string{"docker-entrypoint.sh"}
	req.ImageEntrypoint = nil // customer cleared it via entrypoint: []
	req.ImageCmd = []string{"sleep", "infinity"}
	_, err := e.SelectProviderCandidates(context.Background(), req)
	if !errors.Is(err, ErrImageEntrypointUnavailable) {
		t.Fatalf("cleared override must not re-admit podco: got %v, want ErrImageEntrypointUnavailable", err)
	}
}

// The steer is scoped to customer images: a template deploy (CustomerImage
// false) never carries ImageConfigEntrypoint, and even if one leaked in it
// must not narrow template placement.
func TestSelectProviderCandidates_EntrypointSteerScopedToCustomerImage(t *testing.T) {
	rp := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{diskOffering("podco", provider.GPUTypeH100SXM, "us-west", 2.40)},
	}
	e := newTestEngineWithProviders(rp)

	req := customerImageReq()
	req.CustomerImage = false
	req.ImageConfigEntrypoint = []string{"docker-entrypoint.sh"}
	candidates, err := e.SelectProviderCandidates(context.Background(), req)
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Prov.Name() != "podco" {
		t.Fatalf("non-customer-image request was steered: got %d candidates, want podco", len(candidates))
	}
}

// Review-790 regression: the steer counts per OFFERING (post pin-filter), so
// a pin on entrypoint-SAFE capacity that drains reports the honest
// offering-drained error — never the entrypoint one (which would tell the
// customer to rebuild an image that is fine on the capacity they picked).
func TestSelectProviderCandidates_PinDrainOnSafeCapacityNotBlamedOnEntrypoint(t *testing.T) {
	rp := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{diskOffering("podco", provider.GPUTypeH100SXM, "us-west", 2.40)},
	}
	marketco := &stubProvider{
		name:      "marketco",
		offerings: []provider.GPUOffering{diskOffering("marketco", provider.GPUTypeH100SXM, "us-east", 2.80)},
	}
	e := newTestEngineWithProviders(rp, marketco)

	req := customerImageReq()
	req.ImageConfigEntrypoint = []string{"docker-entrypoint.sh"}
	req.SelectedOfferingID = "drained-marketco-pin" // matches nothing this poll
	_, err := e.SelectProviderCandidates(context.Background(), req)
	if !errors.Is(err, ErrOfferingUnavailable) {
		t.Fatalf("drained pin on safe capacity: got %v, want ErrOfferingUnavailable", err)
	}
}

// A pin that lands ON steered capacity gets the entrypoint error — the
// offering is right there and a retry fails identically.
func TestSelectProviderCandidates_PinOnSteeredCapacityTypedError(t *testing.T) {
	rp := &stubProvider{
		name:      "podco",
		offerings: []provider.GPUOffering{diskOffering("podco", provider.GPUTypeH100SXM, "us-west", 2.40)},
	}
	e := newTestEngineWithProviders(rp)

	req := customerImageReq()
	req.ImageConfigEntrypoint = []string{"docker-entrypoint.sh"}
	req.SelectedOfferingID = pinFor(rp.offerings[0], "podco")
	_, err := e.SelectProviderCandidates(context.Background(), req)
	if !errors.Is(err, ErrImageEntrypointUnavailable) {
		t.Fatalf("pin on steered capacity: got %v, want ErrImageEntrypointUnavailable", err)
	}
}
