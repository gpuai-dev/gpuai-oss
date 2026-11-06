package spotrouter

import (
	"context"
	"sync"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// fakeProvider is a test double implementing provider.Provider. It mirrors
// the stubProvider pattern in internal/provision/stub_provider_test.go.
type fakeProvider struct {
	name      string
	offerings []provider.GPUOffering
	listErr   error

	provisionResult *provider.ProvisionResult
	provisionErr    error
	provisionReqs   []provider.ProvisionRequest
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) ListAvailable(ctx context.Context) ([]provider.GPUOffering, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.offerings, nil
}

func (f *fakeProvider) Provision(ctx context.Context, req provider.ProvisionRequest) (*provider.ProvisionResult, error) {
	f.provisionReqs = append(f.provisionReqs, req)
	if f.provisionErr != nil {
		return nil, f.provisionErr
	}
	return f.provisionResult, nil
}

func (f *fakeProvider) GetStatus(ctx context.Context, upstreamID, region string) (*provider.InstanceStatus, error) {
	return &provider.InstanceStatus{UpstreamID: upstreamID, Status: "running"}, nil
}

func (f *fakeProvider) Terminate(ctx context.Context, upstreamID, region string) error {
	return nil
}

func (f *fakeProvider) ListProvisioned(ctx context.Context) ([]provider.ProvisionedInstance, error) {
	return nil, provider.ErrNotImplemented
}

// fakeSpotProvider additionally implements provider.SpotCapability with a
// scriptable preemption state per upstream instance.
type fakeSpotProvider struct {
	fakeProvider

	supportsSpot bool

	mu              sync.Mutex
	preemption      map[string]provider.PreemptionStatus // upstreamID -> state
	preemptionErr   error
	preemptionCalls []string // upstreamIDs queried, in order
}

func (f *fakeSpotProvider) SupportsSpot() bool { return f.supportsSpot }

func (f *fakeSpotProvider) PreemptionState(ctx context.Context, upstreamID, region string) (provider.PreemptionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.preemptionCalls = append(f.preemptionCalls, upstreamID)
	if f.preemptionErr != nil {
		return provider.PreemptionNone, f.preemptionErr
	}
	if s, ok := f.preemption[upstreamID]; ok {
		return s, nil
	}
	return provider.PreemptionNone, nil
}

func (f *fakeSpotProvider) setPreemption(upstreamID string, s provider.PreemptionStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.preemption == nil {
		f.preemption = map[string]provider.PreemptionStatus{}
	}
	f.preemption[upstreamID] = s
}

// staticSource is a ProviderSource over a fixed slice.
type staticSource struct{ provs []provider.Provider }

func (s staticSource) All() []provider.Provider { return s.provs }

// spotOffering builds a minimally-valid spot offering for tests.
func spotOffering(providerName string, gpuType provider.GPUType, count int, price float64, region string) provider.GPUOffering {
	return provider.GPUOffering{
		Provider:       providerName,
		GPUType:        gpuType,
		GPUCount:       count,
		VRAMPerGPUGB:   80,
		PricePerHour:   price,
		Tier:           provider.TierSpot,
		Region:         region,
		AvailableCount: 3,
	}
}
