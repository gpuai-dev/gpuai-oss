package provision

import (
	"context"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// Startup GPU compute check, engine side. In enforce mode the bootstrap refuses
// to publish a host whose GPU cannot run a kernel (UVM/cuInit faults, missing
// GPUs, a framework without kernels for the architecture) and the ready
// handler records the reason on the row. The poll loop must take it, release
// the host, and rescue elsewhere with that host excluded, never billing the
// failed one. Measured live on 2026-09-20: one L40S with a dead UVM layer was
// sold to the same customer nine times in a row, each billed as ready.

// readinessFakeDB adds the optional readiness seam to the shared fake.
type readinessFakeDB struct {
	*fakeEngineDB
	pending []string // returned one per Take call, then empty
	takes   int
}

func (f *readinessFakeDB) TakeInstanceReadinessFailure(_ context.Context, _ string) (string, error) {
	f.takes++
	if len(f.pending) == 0 {
		return "", nil
	}
	r := f.pending[0]
	f.pending = f.pending[1:]
	return r, nil
}

const uvmFault = "host: cuInit returned 999, nvidia-uvm open: Input/output error"

// gpuCheckFixture: the placed upstream is up and reports its host; a backup
// provider has capacity.
func gpuCheckFixture() (bad, backup *stubProvider, rescue *bootRescue, inst *db.Instance) {
	bad, backup, rescue, inst = failoverFixture()
	bad.statusResult = &provider.InstanceStatus{Status: "running", UpstreamHostID: "host-42"}
	// The same provider can place a replacement on a different host.
	bad.provisionResult = &provider.ProvisionResult{
		UpstreamID: "up-replacement", Provider: "dying", Status: "creating", CostPerHour: 1.0, Region: "us-west",
	}
	seq := 0
	rescue.walk.ledger = attemptLedgerCtx{orgID: "org-1", seq: &seq}
	return bad, backup, rescue, inst
}

func TestProgressStatus_GPUCheckFailureRescuesWithHostExcluded(t *testing.T) {
	bad, backup, rescue, inst := gpuCheckFixture()
	var rebinds, errored []string
	rebound := false
	base := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) { return true, nil },
		rebindInstanceUpstream: func(id, prov, upID, region string, price, upPrice float64, diskGB *int) (bool, error) {
			rebinds = append(rebinds, prov+"/"+upID)
			rebound = true
			return true, nil
		},
		getInstance: func(id string) (*db.Instance, error) {
			if rebound {
				return &db.Instance{InstanceID: "inst-f", UpstreamProvider: "dying", UpstreamID: "up-replacement", Status: StateTerminated}, nil
			}
			return inst, nil
		},
		onSetInstanceError: func(id, reason string) { errored = append(errored, reason) },
	}
	fake := &readinessFakeDB{fakeEngineDB: base, pending: []string{uvmFault}}
	e := failoverEngine(base, bad, backup)
	e.db = fake

	e.progressStatus("inst-f", e.provisionTimeout, rescue)

	if len(errored) != 0 {
		t.Fatalf("a rescued launch must not be errored, got %v", errored)
	}
	if len(rebinds) != 1 {
		t.Fatalf("row must rebind to a replacement, got %v", rebinds)
	}
	if len(bad.terminateCalls) == 0 || bad.terminateCalls[0].UpstreamID != "up-dying" {
		t.Errorf("the failed host's upstream must be terminated first, got %+v", bad.terminateCalls)
	}
	excluded := false
	for _, h := range rescue.provReq.ExcludeUpstreamHostIDs {
		excluded = excluded || h == "host-42"
	}
	if !excluded {
		t.Errorf("the failed host must be excluded from the rescue, got %v", rescue.provReq.ExcludeUpstreamHostIDs)
	}
	var ledger *db.ProvisionAttempt
	for i := range base.recordedAttempts {
		if base.recordedAttempts[i].Outcome == db.AttemptOutcomeGPUCheckFailed {
			ledger = &base.recordedAttempts[i]
		}
	}
	if ledger == nil || ledger.Reason != uvmFault {
		t.Errorf("attempt ledger must record gpu_check_failed with the reason, got %+v", base.recordedAttempts)
	}
}

// A pinned launch (dashboard card) used to be unrescuable: the rescue kept
// the exact-offering pin, which named the failed host, so with that host
// excluded it matched nothing. The pin is relaxed like the upstream-terminal
// rescue relaxes it.
func TestProgressStatus_GPUCheckFailureRescuesAPinnedLaunch(t *testing.T) {
	bad, backup, rescue, inst := gpuCheckFixture()
	rescue.req.SelectedOfferingID = "pinned-offering-on-the-failed-host"
	var rebinds, errored []string
	rebound := false
	base := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) { return true, nil },
		rebindInstanceUpstream: func(id, prov, upID, region string, price, upPrice float64, diskGB *int) (bool, error) {
			rebinds = append(rebinds, prov+"/"+upID)
			rebound = true
			return true, nil
		},
		getInstance: func(id string) (*db.Instance, error) {
			if rebound {
				return &db.Instance{InstanceID: "inst-f", UpstreamProvider: "dying", UpstreamID: "up-replacement", Status: StateTerminated}, nil
			}
			return inst, nil
		},
		onSetInstanceError: func(id, reason string) { errored = append(errored, reason) },
	}
	e := failoverEngine(base, bad, backup)
	e.db = &readinessFakeDB{fakeEngineDB: base, pending: []string{uvmFault}}

	e.progressStatus("inst-f", e.provisionTimeout, rescue)

	if len(errored) != 0 || len(rebinds) != 1 {
		t.Fatalf("pinned launch was not rescued: errored=%v rebinds=%v", errored, rebinds)
	}
	if rescue.req.SelectedOfferingID != "" {
		t.Errorf("rescue kept the pin %q", rescue.req.SelectedOfferingID)
	}
}

func TestProgressStatus_GPUCheckFailureNoReplacementFailsWithoutBilling(t *testing.T) {
	bad, _, rescue, inst := gpuCheckFixture()
	var errored []string
	base := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) { return true, nil },
		getInstance:          func(id string) (*db.Instance, error) { return inst, nil },
		onSetInstanceError:   func(id, reason string) { errored = append(errored, reason) },
	}
	e := failoverEngine(base, bad) // nothing else to rescue onto... except the same provider
	bad.provisionErr = provider.ErrNoCapacity
	e.db = &readinessFakeDB{fakeEngineDB: base, pending: []string{uvmFault}}

	e.progressStatus("inst-f", e.provisionTimeout, rescue)

	want := stallErrorReason(verdictGPUCheckFailed, true)
	if len(errored) != 1 || errored[0] != want {
		t.Fatalf("errored = %v; want [%q]", errored, want)
	}
}

func TestProgressStatus_GPUCheckFailureWithoutRescueTerminatesAndFails(t *testing.T) {
	bad, _, _, inst := gpuCheckFixture()
	var errored []string
	base := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) { return true, nil },
		getInstance:          func(id string) (*db.Instance, error) { return inst, nil },
		onSetInstanceError:   func(id, reason string) { errored = append(errored, reason) },
	}
	e := failoverEngine(base, bad)
	e.db = &readinessFakeDB{fakeEngineDB: base, pending: []string{uvmFault}}

	e.progressStatus("inst-f", e.provisionTimeout, nil)

	want := stallErrorReason(verdictGPUCheckFailed, false)
	if len(errored) != 1 || errored[0] != want {
		t.Fatalf("errored = %v; want [%q]", errored, want)
	}
	if len(bad.terminateCalls) == 0 {
		t.Error("the failed host's upstream must be terminated")
	}
}

// No pending verdict: the loop behaves exactly as before (here it times out,
// because the fake never flips the row to running).
func TestProgressStatus_NoGPUCheckVerdictChangesNothing(t *testing.T) {
	bad, _, rescue, inst := gpuCheckFixture()
	var errored []string
	base := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) { return true, nil },
		getInstance:          func(id string) (*db.Instance, error) { return inst, nil },
		onSetInstanceError:   func(id, reason string) { errored = append(errored, reason) },
	}
	e := failoverEngine(base, bad)
	fake := &readinessFakeDB{fakeEngineDB: base}
	e.db = fake

	e.progressStatus("inst-f", e.provisionTimeout, rescue)

	if fake.takes == 0 {
		t.Error("the poll loop never checked for a readiness verdict")
	}
	for _, r := range errored {
		if r == stallErrorReason(verdictGPUCheckFailed, true) || r == stallErrorReason(verdictGPUCheckFailed, false) {
			t.Errorf("no verdict was pending but the launch failed with %q", r)
		}
	}
	if len(bad.terminateCalls) > 1 {
		t.Errorf("unexpected extra terminations: %+v", bad.terminateCalls)
	}
}

// An "environment:" verdict is the image (the promised framework cannot use
// the GPU), not the host: rescuing would spend the allowance on a host that
// fails the same way and exclude a healthy one. Terminate and fail fast with an
// image-level reason, never the "assigned GPU failed" host reason.
func TestProgressStatus_GPUCheckEnvironmentFailureFailsFastWithoutRescue(t *testing.T) {
	bad, backup, rescue, inst := gpuCheckFixture()
	var errored, rebinds []string
	base := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) { return true, nil },
		rebindInstanceUpstream: func(id, prov, upID, region string, price, upPrice float64, diskGB *int) (bool, error) {
			rebinds = append(rebinds, prov+"/"+upID)
			return true, nil
		},
		getInstance:        func(id string) (*db.Instance, error) { return inst, nil },
		onSetInstanceError: func(id, reason string) { errored = append(errored, reason) },
	}
	e := failoverEngine(base, bad, backup)
	const envFault = "environment: torch 2.4.1+cu124 cuda 12.4 on sm_120: no kernel image is available for execution on the device"
	e.db = &readinessFakeDB{fakeEngineDB: base, pending: []string{envFault}}

	e.progressStatus("inst-f", e.provisionTimeout, rescue)

	if len(rebinds) != 0 || len(backup.provisionReqs) != 0 || len(bad.provisionReqs) != 0 {
		t.Fatalf("an environment failure must not be rescued: rebinds=%v backup=%d same-provider=%d",
			rebinds, len(backup.provisionReqs), len(bad.provisionReqs))
	}
	if rescue.attemptsLeft != 1 || len(rescue.provReq.ExcludeUpstreamHostIDs) != 0 {
		t.Errorf("an environment failure must neither spend a rescue nor exclude the host: left=%d excluded=%v",
			rescue.attemptsLeft, rescue.provReq.ExcludeUpstreamHostIDs)
	}
	if len(bad.terminateCalls) == 0 || bad.terminateCalls[0].UpstreamID != "up-dying" {
		t.Errorf("the upstream must be terminated, got %+v", bad.terminateCalls)
	}
	want := stallErrorReason(verdictGPUCheckEnvironment, false)
	if len(errored) != 1 || errored[0] != want || errored[0] == stallErrorReason(verdictGPUCheckFailed, false) {
		t.Fatalf("errored = %v; want [%q]", errored, want)
	}
	var row *db.ProvisionAttempt
	for i := range base.recordedAttempts {
		if base.recordedAttempts[i].Outcome == db.AttemptOutcomeGPUCheckFailed {
			row = &base.recordedAttempts[i]
		}
	}
	if row == nil || row.Reason != envFault {
		t.Errorf("attempt ledger must record gpu_check_failed with the environment reason, got %+v", base.recordedAttempts)
	}
}

func TestStallErrorReason_GPUCheckFailedNamesNoSupplier(t *testing.T) {
	for _, verdict := range []string{verdictGPUCheckFailed, verdictGPUCheckEnvironment} {
		for _, tried := range []bool{false, true} {
			got := stallErrorReason(verdict, tried)
			if got == "" || got == stallErrorReason("unknown", tried) {
				t.Errorf("%s verdict has no reason of its own: %q", verdict, got)
			}
		}
	}
}
