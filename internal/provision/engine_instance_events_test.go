package provision

// engine_instance_events_test.go — GPU-45: the engine must emit instance.*
// webhook events at lifecycle transitions, exactly once per genuine
// transition per instance. These tests drive the engine through the fake DB
// seam with a recording sink and assert (a) the right event fires with the
// right instance, and (b) the guarded-transition gates suppress re-emission
// on repeats/races.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// recordingEventSink is an in-memory InstanceEventSink capturing every
// emission. Mutex-guarded because progressStatus runs emission from the
// engine's own goroutine in some tests.
type recordingEventSink struct {
	mu         sync.Mutex
	creating   []string // instance ids
	terminated []string // instance ids
	failed     []string // "<instance id>|<reason>"
}

func (r *recordingEventSink) InstanceCreating(_ context.Context, inst *db.Instance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creating = append(r.creating, inst.InstanceID)
}

func (r *recordingEventSink) InstanceTerminated(_ context.Context, inst *db.Instance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.terminated = append(r.terminated, inst.InstanceID)
}

func (r *recordingEventSink) InstanceFailed(_ context.Context, inst *db.Instance, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, inst.InstanceID+"|"+reason)
}

func (r *recordingEventSink) snapshot() (creating, terminated, failed []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.creating...),
		append([]string(nil), r.terminated...),
		append([]string(nil), r.failed...)
}

// TestProvision_EmitsInstanceCreatingOnce proves the create-accepted event:
// a successful Provision (record persisted) emits instance.creating exactly
// once, for the instance id the response carries.
func TestProvision_EmitsInstanceCreatingOnce(t *testing.T) {
	stub := onDemandStub("up-create", "us-west")
	fake := fakeWithOneKey()
	sink := &recordingEventSink{}

	e := newProvisionUnitEngine(stub, fake)
	e.SetInstanceEventSink(sink)

	resp, err := e.Provision(context.Background(), onDemandRequest())
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	creating, terminated, failed := sink.snapshot()
	if len(creating) != 1 {
		t.Fatalf("expected exactly 1 instance.creating emission, got %d: %v", len(creating), creating)
	}
	if creating[0] != resp.InstanceID {
		t.Errorf("instance.creating emitted for %q, want %q", creating[0], resp.InstanceID)
	}
	if len(terminated) != 0 || len(failed) != 0 {
		t.Errorf("unexpected terminated/failed emissions on the happy path: %v / %v", terminated, failed)
	}
}

// TestProvision_PersistFailure_NoCreatingEmission proves the negative: when
// CreateInstance fails (no record exists), instance.creating must NOT fire —
// the customer never had a resource.
func TestProvision_PersistFailure_NoCreatingEmission(t *testing.T) {
	stub := onDemandStub("up-nope", "us-west")
	fake := fakeWithOneKey()
	fake.createInstanceErr = errTestPersist
	sink := &recordingEventSink{}

	e := newProvisionUnitEngine(stub, fake)
	e.SetInstanceEventSink(sink)

	if _, err := e.Provision(context.Background(), onDemandRequest()); err == nil {
		t.Fatal("expected Provision to fail on persist error")
	}
	if creating, _, _ := sink.snapshot(); len(creating) != 0 {
		t.Fatalf("instance.creating must not fire when the record was never created, got %v", creating)
	}
}

var errTestPersist = errTest("persist boom")

type errTest string

func (e errTest) Error() string { return string(e) }

// TestTerminate_EmitsInstanceTerminatedOnce proves the terminated event is
// gated on the guarded DB flip: the first Terminate (flip lands) emits once;
// a repeated Terminate on the already-terminated row emits nothing.
func TestTerminate_EmitsInstanceTerminatedOnce(t *testing.T) {
	const instanceID = "inst-term"

	status := StateRunning
	flips := 0
	fake := &fakeEngineDB{
		getInstance: func(id string) (*db.Instance, error) {
			return &db.Instance{
				InstanceID:       instanceID,
				OrgID:            "org-1",
				Status:           status,
				UpstreamProvider: "ghost", // not in registry -> local-terminate path
			}, nil
		},
		updateInstanceStatus: func(_, _, to string) (bool, error) {
			status = to
			return true, nil
		},
		terminateInstance: func(id string) (bool, error) {
			flips++
			if flips == 1 {
				status = StateTerminated
				return true, nil // the guarded flip landed
			}
			return false, nil // already terminated (NOT IN ('terminated') guard)
		},
	}
	sink := &recordingEventSink{}
	e := &Engine{
		registry: provider.NewRegistry(),
		db:       fake,
		config:   &config.Config{},
		logger:   slog.Default(),
	}
	e.SetInstanceEventSink(sink)

	if err := e.Terminate(context.Background(), instanceID); err != nil {
		t.Fatalf("first Terminate failed: %v", err)
	}
	// Second call: the row is terminated now — Terminate returns early
	// (idempotent), and even if it reached the DB the guard reports false.
	if err := e.Terminate(context.Background(), instanceID); err != nil {
		t.Fatalf("second Terminate failed: %v", err)
	}

	_, terminated, _ := sink.snapshot()
	if len(terminated) != 1 {
		t.Fatalf("expected exactly 1 instance.terminated emission across 2 Terminate calls, got %d: %v",
			len(terminated), terminated)
	}
	if terminated[0] != instanceID {
		t.Errorf("instance.terminated emitted for %q, want %q", terminated[0], instanceID)
	}
}

// TestProgressStatus_Timeout_EmitsInstanceFailedOnce proves the provisioning
// timeout path emits instance.failed with the customer-visible reason, gated
// on SetInstanceError's updated=true.
func TestProgressStatus_Timeout_EmitsInstanceFailedOnce(t *testing.T) {
	const instanceID = "inst-timeout-hook"

	provisioningRow := &db.Instance{
		InstanceID:       instanceID,
		OrgID:            "org-1",
		UpstreamProvider: "stub",
		UpstreamID:       "up-stuck",
		Region:           "us-west",
		Status:           StateProvisioning,
	}
	fake := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) {
			return from == StateCreating && to == StateProvisioning, nil
		},
		getInstance: func(id string) (*db.Instance, error) { return provisioningRow, nil },
	}
	stub := &stubProvider{
		name:         "stub",
		statusResult: &provider.InstanceStatus{Status: "creating"}, // never running
	}
	reg := provider.NewRegistry()
	reg.Register(stub)

	sink := &recordingEventSink{}
	e := &Engine{
		registry:         reg,
		db:               fake,
		logger:           slog.Default(),
		provisionTimeout: 5 * time.Millisecond,
	}
	e.SetInstanceEventSink(sink)

	e.progressStatus(instanceID, e.provisionTimeout, nil)

	_, _, failed := sink.snapshot()
	if len(failed) != 1 {
		t.Fatalf("expected exactly 1 instance.failed emission, got %d: %v", len(failed), failed)
	}
	if !strings.HasPrefix(failed[0], instanceID+"|provisioning timeout") {
		t.Errorf("instance.failed emission = %q, want instance %q with a 'provisioning timeout' reason",
			failed[0], instanceID)
	}
}

// TestProgressStatus_Timeout_NoEmissionWhenAlreadyClosedOut proves the
// exactly-once gate: when SetInstanceError reports updated=false (a
// concurrent writer already closed the row out — e.g. a user terminate or an
// earlier failure), instance.failed must NOT be emitted.
func TestProgressStatus_Timeout_NoEmissionWhenAlreadyClosedOut(t *testing.T) {
	const instanceID = "inst-timeout-raced"

	provisioningRow := &db.Instance{
		InstanceID:       instanceID,
		OrgID:            "org-1",
		UpstreamProvider: "stub",
		UpstreamID:       "up-raced",
		Region:           "us-west",
		Status:           StateProvisioning,
	}
	fake := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) {
			return from == StateCreating && to == StateProvisioning, nil
		},
		getInstance:      func(id string) (*db.Instance, error) { return provisioningRow, nil },
		setInstanceError: func(_, _ string) (bool, error) { return false, nil }, // lost the race
	}
	stub := &stubProvider{
		name:         "stub",
		statusResult: &provider.InstanceStatus{Status: "creating"},
	}
	reg := provider.NewRegistry()
	reg.Register(stub)

	sink := &recordingEventSink{}
	e := &Engine{
		registry:         reg,
		db:               fake,
		logger:           slog.Default(),
		provisionTimeout: 5 * time.Millisecond,
	}
	e.SetInstanceEventSink(sink)

	e.progressStatus(instanceID, e.provisionTimeout, nil)

	if _, _, failed := sink.snapshot(); len(failed) != 0 {
		t.Fatalf("instance.failed must not emit when SetInstanceError reports updated=false, got %v", failed)
	}
}

// TestReapStuckProvisions_EmitsInstanceFailed proves the durable reaper emits
// instance.failed when it WINS the optimistic flip, and stays silent when it
// loses it (a live watchdog / user terminate already moved the row).
func TestReapStuckProvisions_EmitsInstanceFailed(t *testing.T) {
	stuckRow := db.Instance{
		InstanceID:       "inst-stuck",
		OrgID:            "org-1",
		Status:           StateBooting,
		UpstreamProvider: "ghost", // not registered -> reaper skips upstream terminate
	}

	for _, tc := range []struct {
		name      string
		winFlip   bool
		wantCount int
	}{
		{name: "wins flip -> emits once", winFlip: true, wantCount: 1},
		{name: "loses flip -> no emission", winFlip: false, wantCount: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeEngineDB{
				listStuckProvisions: func(time.Time) ([]db.Instance, error) {
					return []db.Instance{stuckRow}, nil
				},
				updateInstanceStatus: func(_, _, _ string) (bool, error) { return tc.winFlip, nil },
			}
			sink := &recordingEventSink{}
			e := &Engine{
				registry: provider.NewRegistry(),
				db:       fake,
				logger:   slog.Default(),
			}
			e.SetInstanceEventSink(sink)

			if err := e.ReapStuckProvisions(context.Background()); err != nil {
				t.Fatalf("ReapStuckProvisions failed: %v", err)
			}
			_, _, failed := sink.snapshot()
			if len(failed) != tc.wantCount {
				t.Fatalf("expected %d instance.failed emissions, got %d: %v", tc.wantCount, len(failed), failed)
			}
			if tc.wantCount == 1 && !strings.HasPrefix(failed[0], "inst-stuck|provisioning timeout (reaper)") {
				t.Errorf("emission = %q, want reaper reason for inst-stuck", failed[0])
			}
		})
	}
}

// TestEngine_NilSinkIsSafe proves every emission site tolerates the unwired
// state (tests, partial deployments): no sink, no panic.
func TestEngine_NilSinkIsSafe(t *testing.T) {
	e := &Engine{logger: slog.Default()}
	e.emitInstanceCreating(context.Background(), &db.Instance{InstanceID: "x"})
	e.emitInstanceTerminated(context.Background(), &db.Instance{InstanceID: "x"})
	e.emitInstanceFailed(context.Background(), &db.Instance{InstanceID: "x"}, "r")
}
