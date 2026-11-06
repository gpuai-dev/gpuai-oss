package provision

import (
	"context"
	"errors"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// TestProvision_ReservedTierRefused: reserved capacity contracts are not part
// of GPU.ai Open Source, so a reserved-tier request is refused with
// ErrReservedNotConfigured before any provider is touched.
func TestProvision_ReservedTierRefused(t *testing.T) {
	stub := onDemandStub("up-res", "us-east")
	fake := fakeWithOneKey()
	e := newProvisionUnitEngine(stub, fake)

	req := onDemandRequest()
	req.Tier = provider.TierReserved
	_, err := e.Provision(context.Background(), req)
	if !errors.Is(err, ErrReservedNotConfigured) {
		t.Fatalf("expected ErrReservedNotConfigured, got %v", err)
	}
	if len(stub.provisionReqs) != 0 {
		t.Fatalf("no provider call expected for a refused reserved request, got %d", len(stub.provisionReqs))
	}
	if len(fake.createdInstances) != 0 {
		t.Fatalf("no instance row expected, got %d", len(fake.createdInstances))
	}
}
