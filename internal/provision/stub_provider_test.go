package provision

import (
	"context"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// terminateCall records a single Provider.Terminate invocation so tests can
// assert exactly which upstream id (and region) the engine asked to tear down
// — e.g. the BLOCK-02 persist-failure path and BLOCK-04 timeout path both must
// terminate the just-created upstream instance.
type terminateCall struct {
	UpstreamID string
	Region     string
}

// stubProvider is a test double implementing provider.Provider.
//
// Candidate-selection tests construct it with just name/offerings/listErr and
// never call the mutation methods. Lifecycle tests (Plans 02/03/06) set the
// configurable result/error fields below and inspect terminateCalls afterwards.
// All result fields default to nil/zero, so the original selection tests are
// unaffected. Placed in a _test.go file so it ships zero code to production
// binaries.
type stubProvider struct {
	name      string
	offerings []provider.GPUOffering
	listErr   error

	// Provision returns provisionResult (or provisionErr if non-nil).
	provisionResult *provider.ProvisionResult
	provisionErr    error
	// provisionReqs records every provider.ProvisionRequest passed to Provision
	// so SSH-resolution tests can assert exactly which public keys the engine
	// embedded (req.SSHPublicKeys). Defaults nil, so existing tests are unaffected.
	provisionReqs []provider.ProvisionRequest

	// GetStatus returns statusResult (or statusErr if non-nil).
	statusResult *provider.InstanceStatus
	statusErr    error

	// Terminate appends a terminateCall to terminateCalls BEFORE returning
	// terminateErr, so a test can assert the exact (upstreamID, region) the
	// engine terminated even when terminateErr is non-nil.
	terminateErr   error
	terminateCalls []terminateCall
	// onTerminate, if non-nil, is invoked inside Terminate right after the call
	// is recorded. Plan 03's timeout test uses it to append to a shared ordered
	// event log so the test can assert Terminate fires BEFORE SetInstanceError.
	onTerminate func(upstreamID, region string)

	// ListProvisioned returns listProvisioned (reconciler-orphan tests).
	listProvisioned []provider.ProvisionedInstance
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) ListAvailable(ctx context.Context) ([]provider.GPUOffering, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	// Return a copy so the engine cannot mutate our fixture.
	out := make([]provider.GPUOffering, len(s.offerings))
	copy(out, s.offerings)
	return out, nil
}

func (s *stubProvider) Provision(ctx context.Context, req provider.ProvisionRequest) (*provider.ProvisionResult, error) {
	s.provisionReqs = append(s.provisionReqs, req)
	if s.provisionErr != nil {
		return nil, s.provisionErr
	}
	return s.provisionResult, nil
}

func (s *stubProvider) GetStatus(ctx context.Context, upstreamID, region string) (*provider.InstanceStatus, error) {
	if s.statusErr != nil {
		return nil, s.statusErr
	}
	return s.statusResult, nil
}

func (s *stubProvider) Terminate(ctx context.Context, upstreamID, region string) error {
	s.terminateCalls = append(s.terminateCalls, terminateCall{UpstreamID: upstreamID, Region: region})
	if s.onTerminate != nil {
		s.onTerminate(upstreamID, region)
	}
	return s.terminateErr
}

func (s *stubProvider) ListProvisioned(ctx context.Context) ([]provider.ProvisionedInstance, error) {
	return s.listProvisioned, nil
}
