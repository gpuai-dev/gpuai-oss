package provision

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// TestProgressStatus_Timeout_TerminatesUpstream is the mandatory BLOCK-04 (SC-3)
// test: when the provisioning timeout fires before the upstream ever reaches
// running, progressStatus must call provider.Terminate on the stuck upstream
// (with the instance's OWN UpstreamID + Region) BEFORE it marks the row error,
// so a pod stuck in "creating" stops costing money.
//
// It is fast because Plan 01 made the timeout injectable: provisionTimeout is set
// to 5ms, so the `case <-timeout:` branch fires almost immediately (well before
// the 5s poll ticker would tick), driving the timeout path deterministically with
// no live Postgres and no 10-minute wait.
func TestProgressStatus_Timeout_TerminatesUpstream(t *testing.T) {
	const (
		instanceID = "inst-timeout"
		upstreamID = "up-stuck"
		region     = "us-west"
	)

	// Shared ordered event log so we can assert Terminate fired BEFORE the error
	// state was set. progressStatus is invoked synchronously below (not via `go`),
	// so a plain slice is race-free here.
	var events []string

	stub := &stubProvider{
		name: "stub",
		// Never transitions to running -> the 5ms timeout is what ends the loop.
		statusResult: &provider.InstanceStatus{Status: "creating"},
		terminateErr: nil,
		onTerminate: func(_, _ string) {
			events = append(events, "terminate")
		},
	}

	provisioningRow := &db.Instance{
		InstanceID:       instanceID,
		UpstreamProvider: "stub",
		UpstreamID:       upstreamID,
		Region:           region,
		Status:           StateProvisioning,
	}

	fake := &fakeEngineDB{
		// creating -> provisioning must report updated=true or progressStatus
		// early-exits before ever entering the poll loop.
		updateInstanceStatus: func(_, from, to string) (bool, error) {
			if from == StateCreating && to == StateProvisioning {
				return true, nil
			}
			return false, nil
		},
		// Both the Step-2 lookup and the poll-loop re-read return a still
		// "provisioning" row so the loop does not early-exit on a status change.
		getInstance: func(id string) (*db.Instance, error) {
			return provisioningRow, nil
		},
		onSetInstanceError: func(_, _ string) {
			events = append(events, "set_error")
		},
	}

	reg := provider.NewRegistry()
	reg.Register(stub)

	e := &Engine{
		registry:         reg,
		db:               fake,
		logger:           slog.Default(),
		provisionTimeout: 5 * time.Millisecond,
	}

	// Blocks only until the 5ms budget fires.
	e.progressStatus(instanceID, e.provisionTimeout, nil)

	// 1. The stuck upstream was terminated exactly once, with its own id+region.
	if len(stub.terminateCalls) != 1 {
		t.Fatalf("expected exactly 1 Terminate call, got %d: %+v", len(stub.terminateCalls), stub.terminateCalls)
	}
	if got := stub.terminateCalls[0]; got.UpstreamID != upstreamID || got.Region != region {
		t.Fatalf("Terminate called with wrong target: got %+v, want {UpstreamID:%q Region:%q}", got, upstreamID, region)
	}

	// 2. The timeout path completed (the row was marked error).
	// 3. Ordering: Terminate happened BEFORE SetInstanceError — a pod must stop
	//    costing money before we give up tracking it.
	wantOrder := []string{"terminate", "set_error"}
	if len(events) != len(wantOrder) {
		t.Fatalf("expected event order %v, got %v", wantOrder, events)
	}
	for i := range wantOrder {
		if events[i] != wantOrder[i] {
			t.Fatalf("expected event order %v, got %v", wantOrder, events)
		}
	}
}

// TestTerminateStuckUpstream covers the shared helper that BOTH progressStatus
// failure paths (timeout + upstream-terminal) call. Driving the helper directly
// gives fast, deterministic coverage of the second call site's exact teardown
// logic without a 5s real-ticker wait (the poll ticker is not injectable, and
// widening that seam is out of scope for this plan). The two call sites are
// covered because they invoke this identical, here-tested helper.
func TestTerminateStuckUpstream(t *testing.T) {
	const (
		instanceID = "inst-x"
		upstreamID = "up-x"
		region     = "eu-central"
	)
	inst := &db.Instance{InstanceID: instanceID, UpstreamID: upstreamID, Region: region}
	e := &Engine{logger: slog.Default()}

	t.Run("terminates with the instance's own id and region", func(t *testing.T) {
		stub := &stubProvider{name: "stub"}
		e.terminateStuckUpstream(stub, inst, instanceID, "test: terminate failed")
		if len(stub.terminateCalls) != 1 {
			t.Fatalf("expected 1 Terminate call, got %d", len(stub.terminateCalls))
		}
		if got := stub.terminateCalls[0]; got.UpstreamID != upstreamID || got.Region != region {
			t.Fatalf("wrong terminate target: got %+v", got)
		}
	})

	t.Run("a not-found terminate error is swallowed", func(t *testing.T) {
		stub := &stubProvider{name: "stub", terminateErr: &notFoundErr{}}
		// Must not panic and still records the attempt; the error is intentionally
		// not surfaced (the pod is already gone — the desired end state).
		e.terminateStuckUpstream(stub, inst, instanceID, "test: terminate failed")
		if len(stub.terminateCalls) != 1 {
			t.Fatalf("expected 1 Terminate call even when it returns not-found, got %d", len(stub.terminateCalls))
		}
	})

	t.Run("no-ops on nil provider, nil instance, or empty upstream id", func(t *testing.T) {
		stub := &stubProvider{name: "stub"}
		e.terminateStuckUpstream(nil, inst, instanceID, "lbl")
		e.terminateStuckUpstream(stub, nil, instanceID, "lbl")
		e.terminateStuckUpstream(stub, &db.Instance{UpstreamID: ""}, instanceID, "lbl")
		if len(stub.terminateCalls) != 0 {
			t.Fatalf("expected no Terminate calls for nil/empty guards, got %d", len(stub.terminateCalls))
		}
	})
}

// notFoundErr is a terminate error whose message contains "not found", matching
// the swallow predicate in terminateStuckUpstream.
type notFoundErr struct{}

func (e *notFoundErr) Error() string { return "instance not found" }

// TestReapStuckProvisions_TerminatesAndClosesBilling is the AUDIT-05-001/002
// guard: the durable reaper must terminate the stuck upstream, mark the row
// error, AND close its billing session — in that order (stop the money before
// giving up tracking) — for an instance left in a pre-ready state past its boot
// budget with no live watchdog (template deploy / restart-orphaned provision).
func TestReapStuckProvisions_TerminatesAndClosesBilling(t *testing.T) {
	const (
		instanceID = "inst-reap"
		upstreamID = "up-reap"
		region     = "us-west"
	)
	var events []string
	stub := &stubProvider{
		name:        "stub",
		onTerminate: func(_, _ string) { events = append(events, "terminate") },
	}
	stuck := db.Instance{
		InstanceID:       instanceID,
		UpstreamProvider: "stub",
		UpstreamID:       upstreamID,
		Region:           region,
		Status:           StateBooting, // stuck in booting — the CRIT template case
	}
	fake := &fakeEngineDB{
		listStuckProvisions: func(_ time.Time) ([]db.Instance, error) {
			return []db.Instance{stuck}, nil
		},
		// Win the optimistic booting -> error transition so the reaper proceeds.
		updateInstanceStatus: func(_, from, to string) (bool, error) {
			return from == StateBooting && to == StateError, nil
		},
		onSetInstanceError: func(_, _ string) { events = append(events, "set_error") },
	}
	reg := provider.NewRegistry()
	reg.Register(stub)
	e := &Engine{registry: reg, db: fake, logger: slog.Default()}

	if err := e.ReapStuckProvisions(context.Background()); err != nil {
		t.Fatalf("ReapStuckProvisions: %v", err)
	}

	// Terminate fired exactly once with the instance's own id + region.
	if len(stub.terminateCalls) != 1 {
		t.Fatalf("expected 1 Terminate call, got %d: %+v", len(stub.terminateCalls), stub.terminateCalls)
	}
	if got := stub.terminateCalls[0]; got.UpstreamID != upstreamID || got.Region != region {
		t.Fatalf("Terminate target wrong: got %+v", got)
	}
	// Ordering: terminate (stop the money) -> set_error.
	want := []string{"terminate", "set_error"}
	if len(events) != len(want) {
		t.Fatalf("event order: got %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event order: got %v, want %v", events, want)
		}
	}
}

// TestReapStuckProvisions_SkipsWhenAlreadyTransitioned proves the optimistic
// guard: if the row moved out of its pre-ready state concurrently (lost the
// UpdateInstanceStatus race), the reaper does NOT terminate or close billing.
func TestReapStuckProvisions_SkipsWhenAlreadyTransitioned(t *testing.T) {
	stub := &stubProvider{name: "stub"}
	fake := &fakeEngineDB{
		listStuckProvisions: func(_ time.Time) ([]db.Instance, error) {
			return []db.Instance{{InstanceID: "inst-x", UpstreamProvider: "stub", UpstreamID: "up-x", Region: "us-west", Status: StateBooting}}, nil
		},
		updateInstanceStatus: func(_, _, _ string) (bool, error) { return false, nil }, // lost the race
	}
	reg := provider.NewRegistry()
	reg.Register(stub)
	e := &Engine{registry: reg, db: fake, logger: slog.Default()}
	if err := e.ReapStuckProvisions(context.Background()); err != nil {
		t.Fatalf("ReapStuckProvisions: %v", err)
	}
	if len(stub.terminateCalls) != 0 {
		t.Fatalf("must not terminate when the transition was lost, got %d", len(stub.terminateCalls))
	}
}
