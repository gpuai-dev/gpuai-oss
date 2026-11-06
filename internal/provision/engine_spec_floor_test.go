package provision

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// The engine half of RAM-01. The adapters have their own coverage; what these
// tests lock down is the GATE — the decision about whether an advertised spec is
// sound enough to become a hard placement constraint.
//
// This was found untested by review: mutating the gate to always forward, or
// deleting the per-iteration clear, left the entire suite green. Both mutations
// reintroduce a defect that takes an in-stock SKU dark, so the boolean deserves
// direct coverage rather than inheriting it from adapter tests.

func floorTestEngine() *Engine {
	return &Engine{logger: slog.Default()}
}

func floorTestCandidate(prov provider.Provider, o provider.GPUOffering) ProviderCandidate {
	return ProviderCandidate{Prov: prov, Offering: o}
}

// baseFloorOffering is an offering advertising 258GB / 32 cores on a single GPU.
func baseFloorOffering() provider.GPUOffering {
	return provider.GPUOffering{
		Provider:     "stub",
		GPUType:      provider.GPUTypeRTX4090,
		GPUCount:     1,
		RAMGB:        258,
		CPUCores:     32,
		PricePerHour: 1.0,
		Tier:         provider.TierOnDemand,
		Region:       "us-east",
	}
}

// TestWalkForwardsFloorOnlyWhenVouched — an adapter that does not vouch for its
// advertised figures must produce NO floor. Some adapters' specs come from a
// cross-cloud cache or a hand-written guess table; enforcing those would filter
// out every host that actually exists.
func TestWalkForwardsFloorOnlyWhenVouched(t *testing.T) {
	cases := []struct {
		name              string
		ramVouched        bool
		vcpuVouched       bool
		wantRAM, wantVCPU int
	}{
		{"both vouched", true, true, 258, 32},
		{"neither vouched", false, false, 0, 0},
		// The marketco shape: both figures are live and fragment-scoped, but only RAM
		// has an enforcing predicate in pickOffers, so only RAM is vouched. One
		// flag could not express this, which is why the vouch is per field.
		{"ram only", true, false, 258, 0},
		{"vcpu only", false, true, 0, 32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := baseFloorOffering()
			o.RAMFloorGuaranteed = tc.ramVouched
			o.VCPUFloorGuaranteed = tc.vcpuVouched

			stub := &stubProvider{
				name:            "stub",
				provisionResult: &provider.ProvisionResult{UpstreamID: "up-1", Provider: "stub"},
			}
			provReq := &provider.ProvisionRequest{InstanceID: "gpu-gate", GPUType: o.GPUType, GPUCount: 1, Tier: o.Tier}

			_, _, _, err := floorTestEngine().walkProvisionCandidates(
				context.Background(), provReq,
				[]ProviderCandidate{floorTestCandidate(stub, o)},
				provisionWalkParams{instanceID: "gpu-gate"},
			)
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			if len(stub.provisionReqs) != 1 {
				t.Fatalf("want 1 Provision call, got %d", len(stub.provisionReqs))
			}
			got := stub.provisionReqs[0]
			if got.MinRAMPerGPUGB != tc.wantRAM {
				t.Errorf("MinRAMPerGPUGB = %d, want %d", got.MinRAMPerGPUGB, tc.wantRAM)
			}
			if got.MinVCPUPerGPU != tc.wantVCPU {
				t.Errorf("MinVCPUPerGPU = %d, want %d", got.MinVCPUPerGPU, tc.wantVCPU)
			}
		})
	}
}

// TestWalkClearsFloorBetweenCandidates — provReq is a POINTER reused across
// candidate attempts, so a vouched candidate that fails must not leave its floor
// behind for an unvouched one. Without the per-iteration clear the second
// provider is asked for 258GB it never advertised, and fails for a reason that
// exists nowhere in its own catalog.
func TestWalkClearsFloorBetweenCandidates(t *testing.T) {
	vouched := baseFloorOffering()
	vouched.RAMFloorGuaranteed = true
	vouched.VCPUFloorGuaranteed = true

	unvouched := baseFloorOffering()
	unvouched.Provider = "stub2"
	unvouched.RAMGB = 64
	unvouched.CPUCores = 8
	unvouched.PricePerHour = 2.0
	// Deliberately NOT vouched.

	first := &stubProvider{name: "stub", provisionErr: provider.ErrNoCapacity}
	second := &stubProvider{
		name:            "stub2",
		provisionResult: &provider.ProvisionResult{UpstreamID: "up-2", Provider: "stub2"},
	}

	provReq := &provider.ProvisionRequest{InstanceID: "gpu-stale", GPUType: vouched.GPUType, GPUCount: 1, Tier: vouched.Tier}
	_, _, _, err := floorTestEngine().walkProvisionCandidates(
		context.Background(), provReq,
		[]ProviderCandidate{floorTestCandidate(first, vouched), floorTestCandidate(second, unvouched)},
		provisionWalkParams{instanceID: "gpu-stale"},
	)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(first.provisionReqs) != 1 || first.provisionReqs[0].MinRAMPerGPUGB != 258 {
		t.Fatalf("first (vouched) candidate should carry the 258GB floor, got %+v", first.provisionReqs)
	}
	if len(second.provisionReqs) != 1 {
		t.Fatalf("want 1 Provision call on the second candidate, got %d", len(second.provisionReqs))
	}
	if got := second.provisionReqs[0]; got.MinRAMPerGPUGB != 0 || got.MinVCPUPerGPU != 0 {
		t.Errorf("unvouched candidate inherited a stale floor: ram=%d vcpu=%d, want 0/0",
			got.MinRAMPerGPUGB, got.MinVCPUPerGPU)
	}
}

// TestWalkFloorIsPerGPU — Offering.RAMGB describes the whole GPUCount-GPU node
// while the floor travels per GPU, so a multi-GPU offering must be divided down
// (the adapter re-scales it to the pod it actually launches). Truncation must
// only ever relax the floor, never raise it above what was advertised.
func TestWalkFloorIsPerGPU(t *testing.T) {
	o := baseFloorOffering()
	o.GPUCount = 4
	o.RAMGB = 251 // deliberately not divisible by 4
	o.CPUCores = 32
	o.RAMFloorGuaranteed = true
	o.VCPUFloorGuaranteed = true

	stub := &stubProvider{
		name:            "stub",
		provisionResult: &provider.ProvisionResult{UpstreamID: "up-3", Provider: "stub"},
	}
	provReq := &provider.ProvisionRequest{InstanceID: "gpu-pergpu", GPUType: o.GPUType, GPUCount: 4, Tier: o.Tier}
	_, _, _, err := floorTestEngine().walkProvisionCandidates(
		context.Background(), provReq,
		[]ProviderCandidate{floorTestCandidate(stub, o)},
		provisionWalkParams{instanceID: "gpu-pergpu"},
	)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	got := stub.provisionReqs[0]
	if got.MinRAMPerGPUGB != 62 { // 251/4 = 62.75 -> 62
		t.Errorf("MinRAMPerGPUGB = %d, want 62 (251/4 truncated)", got.MinRAMPerGPUGB)
	}
	if got.MinRAMPerGPUGB*o.GPUCount > o.RAMGB {
		t.Errorf("re-scaled floor %d exceeds the advertised %dGB — unsatisfiable by construction",
			got.MinRAMPerGPUGB*o.GPUCount, o.RAMGB)
	}
	if got.MinVCPUPerGPU != 8 {
		t.Errorf("MinVCPUPerGPU = %d, want 8", got.MinVCPUPerGPU)
	}
}

// TestPerGPUSpec covers the helper's edges directly: an unstated spec asserts no
// floor, and truncation never rounds up.
func TestPerGPUSpec(t *testing.T) {
	cases := []struct{ total, count, want int }{
		{0, 1, 0},     // no advertised spec -> no floor
		{-5, 1, 0},    // defensive
		{258, 0, 258}, // unstated GPU count -> treat as whole-node
		{258, 1, 258},
		{251, 4, 62}, // truncates down
		{256, 8, 32},
	}
	for _, c := range cases {
		if got := perGPUSpec(c.total, c.count); got != c.want {
			t.Errorf("perGPUSpec(%d, %d) = %d, want %d", c.total, c.count, got, c.want)
		}
	}
}

// Guard against the walk silently swallowing a provider error, which would make
// the floor tests above pass vacuously.
func TestWalkPropagatesFailure(t *testing.T) {
	o := baseFloorOffering()
	stub := &stubProvider{name: "stub", provisionErr: errors.New("boom")}
	provReq := &provider.ProvisionRequest{InstanceID: "gpu-fail", GPUType: o.GPUType, GPUCount: 1, Tier: o.Tier}
	if _, _, _, err := floorTestEngine().walkProvisionCandidates(
		context.Background(), provReq,
		[]ProviderCandidate{floorTestCandidate(stub, o)},
		provisionWalkParams{instanceID: "gpu-fail"},
	); err == nil {
		t.Fatal("want an error when every candidate fails")
	}
}
