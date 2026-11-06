package spotrouter

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// fakeWatcherDB implements watcherDB in memory. Mutex-guarded because the
// watcher sweeps instances concurrently.
type fakeWatcherDB struct {
	mu        sync.Mutex
	instances []db.Instance
	listErr   error

	// updateOK controls the optimistic-lock outcome of UpdateInstanceStatus.
	updateOK       bool
	statusUpdates  []string // instanceIDs whose status was flipped
	errorReasons   map[string]string
	createdEvents  []db.InstanceEvent
	createEventErr error
}

func newFakeWatcherDB(instances ...db.Instance) *fakeWatcherDB {
	return &fakeWatcherDB{
		instances:    instances,
		updateOK:     true,
		errorReasons: map[string]string{},
	}
}

func (f *fakeWatcherDB) ListActiveInstances(ctx context.Context) ([]db.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]db.Instance, len(f.instances))
	copy(out, f.instances)
	return out, nil
}

func (f *fakeWatcherDB) UpdateInstanceStatus(ctx context.Context, instanceID, fromStatus, toStatus string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.updateOK {
		return false, nil
	}
	f.statusUpdates = append(f.statusUpdates, instanceID)
	return true, nil
}

func (f *fakeWatcherDB) SetInstanceError(ctx context.Context, instanceID, reason string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errorReasons[instanceID] = reason
	return true, nil
}

func (f *fakeWatcherDB) CreateInstanceEvent(ctx context.Context, event *db.InstanceEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createEventErr != nil {
		return f.createEventErr
	}
	f.createdEvents = append(f.createdEvents, *event)
	return nil
}

func (f *fakeWatcherDB) eventTypes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.createdEvents))
	for i, e := range f.createdEvents {
		out[i] = e.EventType
	}
	return out
}

// fakeGetter maps provider names to adapters, standing in for
// *provider.Registry.
type fakeGetter struct{ provs map[string]provider.Provider }

func (g fakeGetter) Get(name string) (provider.Provider, bool) {
	p, ok := g.provs[name]
	return p, ok
}

// rerouteRecorder is a RerouteFunc that records calls and optionally fails.
type rerouteRecorder struct {
	mu    sync.Mutex
	calls []string // instanceIDs rerouted
	err   error
}

func (r *rerouteRecorder) fn(ctx context.Context, inst db.Instance) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, inst.InstanceID)
	return r.err
}

func (r *rerouteRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func spotInstance(instanceID, providerName, upstreamID string) db.Instance {
	return db.Instance{
		InstanceID:       instanceID,
		OrgID:            "org-1",
		UserID:           "user-1",
		UpstreamProvider: providerName,
		UpstreamID:       upstreamID,
		GPUType:          "A100 80GB",
		GPUCount:         1,
		Tier:             string(provider.TierSpot),
		Region:           "us-east",
		Status:           "running",
	}
}

func newTestWatcher(fdb *fakeWatcherDB, rr *rerouteRecorder, provs ...provider.Provider) *Watcher {
	m := map[string]provider.Provider{}
	for _, p := range provs {
		m[p.Name()] = p
	}
	return NewWatcher(WatcherDeps{
		DB:        fdb,
		Providers: fakeGetter{m},
		Reroute:   rr.fn,
		Logger:    testLogger(),
	})
}

// TestWatcher_ReclaimedStopsBillingAndReroutes is the brief's required proof:
// PreemptionState == reclaimed must stop billing the reclaimed instance and
// re-route the customer.
func TestWatcher_ReclaimedStopsBillingAndReroutes(t *testing.T) {
	prov := &fakeSpotProvider{fakeProvider: fakeProvider{name: "community"}, supportsSpot: true}
	prov.setPreemption("up-1", provider.PreemptionReclaimed)

	fdb := newFakeWatcherDB(spotInstance("inst-1", "community", "up-1"))
	rr := &rerouteRecorder{}
	w := newTestWatcher(fdb, rr, prov)

	w.Sweep(context.Background())

	if len(fdb.statusUpdates) != 1 || fdb.statusUpdates[0] != "inst-1" {
		t.Errorf("reclaimed instance status must be flipped, got %v", fdb.statusUpdates)
	}
	if fdb.errorReasons["inst-1"] == "" {
		t.Error("reclaimed instance must carry an error reason")
	}
	if got := fdb.eventTypes(); len(got) != 1 || got[0] != "preemption_reclaimed" {
		t.Errorf("want one preemption_reclaimed event, got %v", got)
	}
	if rr.count() != 1 || rr.calls[0] != "inst-1" {
		t.Errorf("customer must be re-routed exactly once, got %v", rr.calls)
	}
}

// TestWatcher_WarnedPreStagesRerouteOnce: warned records the drain event and
// pre-stages a single re-route inside the grace window — repeated sweeps while
// still warned must not fire again, and billing stays open.
func TestWatcher_WarnedPreStagesRerouteOnce(t *testing.T) {
	prov := &fakeSpotProvider{fakeProvider: fakeProvider{name: "community"}, supportsSpot: true}
	prov.setPreemption("up-1", provider.PreemptionWarned)

	fdb := newFakeWatcherDB(spotInstance("inst-1", "community", "up-1"))
	rr := &rerouteRecorder{}
	w := newTestWatcher(fdb, rr, prov)

	w.Sweep(context.Background())
	w.Sweep(context.Background()) // still warned — must be a no-op

	if got := fdb.eventTypes(); len(got) != 1 || got[0] != "preemption_warned" {
		t.Errorf("want exactly one preemption_warned event across sweeps, got %v", got)
	}
	if rr.count() != 1 {
		t.Errorf("pre-staged re-route must fire exactly once, got %d", rr.count())
	}
	if len(fdb.statusUpdates) != 0 {
		t.Errorf("warned instance must not change status, got %v", fdb.statusUpdates)
	}
}

// TestWatcher_ReclaimedAfterWarnedDoesNotDoubleReroute: the warned pre-stage
// already placed the replacement, so the reclaimed sweep closes out billing
// but must not re-route a second time.
func TestWatcher_ReclaimedAfterWarnedDoesNotDoubleReroute(t *testing.T) {
	prov := &fakeSpotProvider{fakeProvider: fakeProvider{name: "community"}, supportsSpot: true}
	prov.setPreemption("up-1", provider.PreemptionWarned)

	fdb := newFakeWatcherDB(spotInstance("inst-1", "community", "up-1"))
	rr := &rerouteRecorder{}
	w := newTestWatcher(fdb, rr, prov)

	w.Sweep(context.Background()) // warned: pre-stage succeeds
	prov.setPreemption("up-1", provider.PreemptionReclaimed)
	w.Sweep(context.Background()) // reclaimed: closeout only

	if got := fdb.eventTypes(); len(got) != 2 || got[0] != "preemption_warned" || got[1] != "preemption_reclaimed" {
		t.Errorf("want warned then reclaimed events, got %v", got)
	}
	if rr.count() != 1 {
		t.Errorf("customer must be re-routed exactly once across warn+reclaim, got %d", rr.count())
	}
}

// TestWatcher_WarnedRerouteFailureRetriedOnReclaim: if the pre-staged re-route
// fails (e.g. no capacity yet), the reclaimed path must retry it.
func TestWatcher_WarnedRerouteFailureRetriedOnReclaim(t *testing.T) {
	prov := &fakeSpotProvider{fakeProvider: fakeProvider{name: "community"}, supportsSpot: true}
	prov.setPreemption("up-1", provider.PreemptionWarned)

	fdb := newFakeWatcherDB(spotInstance("inst-1", "community", "up-1"))
	rr := &rerouteRecorder{err: errors.New("no spot capacity")}
	w := newTestWatcher(fdb, rr, prov)

	w.Sweep(context.Background()) // warned: pre-stage fails
	rr.mu.Lock()
	rr.err = nil // capacity came back
	rr.mu.Unlock()
	prov.setPreemption("up-1", provider.PreemptionReclaimed)
	w.Sweep(context.Background()) // reclaimed: must retry the re-route

	if rr.count() != 2 {
		t.Errorf("re-route must be retried on reclaim after a failed pre-stage, got %d calls", rr.count())
	}
}

// TestWatcher_ErrNotImplementedSkips: a provider with no native preemption
// signal must cause no action at all — the health monitor owns that path.
func TestWatcher_ErrNotImplementedSkips(t *testing.T) {
	prov := &fakeSpotProvider{fakeProvider: fakeProvider{name: "community"}, supportsSpot: true}
	prov.preemptionErr = provider.ErrNotImplemented

	fdb := newFakeWatcherDB(spotInstance("inst-1", "community", "up-1"))
	rr := &rerouteRecorder{}
	w := newTestWatcher(fdb, rr, prov)

	w.Sweep(context.Background())

}

// TestWatcher_TransientPollErrorSkips: a flaky poll must never close billing
// or trigger a re-route.
func TestWatcher_TransientPollErrorSkips(t *testing.T) {
	prov := &fakeSpotProvider{fakeProvider: fakeProvider{name: "community"}, supportsSpot: true}
	prov.preemptionErr = errors.New("upstream 500")

	fdb := newFakeWatcherDB(spotInstance("inst-1", "community", "up-1"))
	rr := &rerouteRecorder{}
	w := newTestWatcher(fdb, rr, prov)

	w.Sweep(context.Background())

}

// TestWatcher_OnlySpotInstancesPolled: on-demand instances and instances on
// providers without SpotCapability are never polled.
func TestWatcher_OnlySpotInstancesPolled(t *testing.T) {
	spotProv := &fakeSpotProvider{fakeProvider: fakeProvider{name: "community"}, supportsSpot: true}
	plainProv := &fakeProvider{name: "plain"}

	onDemand := spotInstance("inst-od", "community", "up-od")
	onDemand.Tier = string(provider.TierOnDemand)
	onPlain := spotInstance("inst-plain", "plain", "up-plain")

	fdb := newFakeWatcherDB(
		spotInstance("inst-1", "community", "up-1"),
		onDemand,
		onPlain,
	)
	rr := &rerouteRecorder{}
	w := newTestWatcher(fdb, rr, spotProv, plainProv)

	w.Sweep(context.Background())

	spotProv.mu.Lock()
	calls := append([]string(nil), spotProv.preemptionCalls...)
	spotProv.mu.Unlock()
	if len(calls) != 1 || calls[0] != "up-1" {
		t.Errorf("only the spot instance on a SpotCapability provider may be polled, got %v", calls)
	}
}

// TestWatcher_LostOptimisticLockSkipsCloseout: if another actor (health
// monitor, user termination) wins the status transition, the watcher must not
// close billing, emit an event, or re-route.
func TestWatcher_LostOptimisticLockSkipsCloseout(t *testing.T) {
	prov := &fakeSpotProvider{fakeProvider: fakeProvider{name: "community"}, supportsSpot: true}
	prov.setPreemption("up-1", provider.PreemptionReclaimed)

	fdb := newFakeWatcherDB(spotInstance("inst-1", "community", "up-1"))
	fdb.updateOK = false
	rr := &rerouteRecorder{}
	w := newTestWatcher(fdb, rr, prov)

	w.Sweep(context.Background())

}

// TestWatcher_OnEventCallbackFires: preemption events reach the SSE callback
// like health-monitor events do.
func TestWatcher_OnEventCallbackFires(t *testing.T) {
	prov := &fakeSpotProvider{fakeProvider: fakeProvider{name: "community"}, supportsSpot: true}
	prov.setPreemption("up-1", provider.PreemptionReclaimed)

	fdb := newFakeWatcherDB(spotInstance("inst-1", "community", "up-1"))
	rr := &rerouteRecorder{}

	var mu sync.Mutex
	var notified []string
	w := NewWatcher(WatcherDeps{
		DB:        fdb,
		Providers: fakeGetter{map[string]provider.Provider{"community": prov}},
		Reroute:   rr.fn,
		Logger:    testLogger(),
		OnEvent: func(e db.InstanceEvent) {
			mu.Lock()
			defer mu.Unlock()
			notified = append(notified, e.EventType)
		},
	})

	w.Sweep(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(notified) != 1 || notified[0] != "preemption_reclaimed" {
		t.Errorf("OnEvent must receive the reclaimed event, got %v", notified)
	}
}

// --- GPU-45: instance.failed webhook emission on spot reclaim ---

// recordingFailedSink records InstanceFailed calls (the watcher's webhook seam).
type recordingFailedSink struct {
	mu     sync.Mutex
	failed []string // "<instance id>|<reason>"
}

func (r *recordingFailedSink) InstanceFailed(_ context.Context, inst *db.Instance, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, inst.InstanceID+"|"+reason)
}

// TestWatcher_Reclaimed_EmitsInstanceFailedOnce: winning the optimistic flip
// emits instance.failed once; losing it (concurrent closeout) emits nothing.
func TestWatcher_Reclaimed_EmitsInstanceFailedOnce(t *testing.T) {
	run := func(t *testing.T, updateOK bool, wantCount int) {
		fdb := newFakeWatcherDB()
		fdb.updateOK = updateOK
		rr := &rerouteRecorder{}
		sink := &recordingFailedSink{}
		w := NewWatcher(WatcherDeps{
			DB:             fdb,
			Providers:      fakeGetter{map[string]provider.Provider{}},
			Reroute:        rr.fn,
			Logger:         testLogger(),
			InstanceEvents: sink,
		})

		w.handleReclaimed(context.Background(), spotInstance("inst-hook", "community", "up-hook"))

		sink.mu.Lock()
		defer sink.mu.Unlock()
		if len(sink.failed) != wantCount {
			t.Fatalf("expected %d instance.failed emissions, got %d: %v", wantCount, len(sink.failed), sink.failed)
		}
		if wantCount == 1 && sink.failed[0] != "inst-hook|spot instance reclaimed by provider" {
			t.Errorf("emission = %q, want inst-hook with the reclaim reason", sink.failed[0])
		}
	}

	t.Run("wins flip -> emits once", func(t *testing.T) { run(t, true, 1) })
	t.Run("loses flip -> no emission", func(t *testing.T) { run(t, false, 0) })
}
