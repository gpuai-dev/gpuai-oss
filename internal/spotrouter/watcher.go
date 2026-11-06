package spotrouter

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// watcherDB is the narrow slice of *db.Pool the preemption watcher needs.
// It reuses the exact closeout primitives the health monitor uses so a
// preemption closeout is indistinguishable from any other instance closeout.
type watcherDB interface {
	ListActiveInstances(ctx context.Context) ([]db.Instance, error)
	UpdateInstanceStatus(ctx context.Context, instanceID, fromStatus, toStatus string) (bool, error)
	SetInstanceError(ctx context.Context, instanceID, reason string) (bool, error)
	CreateInstanceEvent(ctx context.Context, event *db.InstanceEvent) error
}

// InstanceEventSink is the narrow webhook-emission seam the watcher needs
// (GPU-45): a reclaimed spot instance is a terminal error state the customer
// subscribed to as instance.failed. Declared locally (per-consumer seam
// convention) because the concrete publisher lives in internal/api/public.
type InstanceEventSink interface {
	InstanceFailed(ctx context.Context, inst *db.Instance, reason string)
}

// ProviderGetter resolves a provider adapter by name. *provider.Registry
// satisfies it; tests may inject a fake.
type ProviderGetter interface {
	Get(name string) (provider.Provider, bool)
}

// RerouteFunc re-places a preempted spot instance's customer on the current
// cheapest spot candidate. Production wires provision.Engine.RerouteSpot via a
// small closure (the watcher only needs success/failure); tests inject a fake.
// Keeping this a function seam avoids a spotrouter -> provision import.
type RerouteFunc func(ctx context.Context, inst db.Instance) error

// WatcherDeps holds the dependencies for the preemption watcher.
type WatcherDeps struct {
	DB        watcherDB
	Providers ProviderGetter
	Reroute   RerouteFunc
	Logger    *slog.Logger
	Interval  time.Duration // default 60s, the health-monitor cadence
	// OnEvent is called when a preemption event is recorded (warned,
	// reclaimed). Used to push events to the SSE broker. Nil means no
	// notification.
	OnEvent func(event db.InstanceEvent)
	// InstanceEvents receives instance.failed webhook events (GPU-45) when
	// the reclaimed-closeout wins its optimistic status flip. Nil disables
	// webhook publishing. public.InstanceEventPublisher satisfies it.
	InstanceEvents InstanceEventSink
}

// Watcher polls provider.SpotCapability.PreemptionState for every active spot
// instance on the health cadence and drives the cheapest-spot failover:
//
//   - warned: the provider signalled preemption ahead of reclaim. The watcher
//     records a "preemption_warned" event (the drain signal consumed by the
//     same event plumbing as health events) and pre-stages the re-route inside
//     the grace window so the replacement is already provisioning before the
//     node disappears.
//   - reclaimed: the node is gone. The watcher closes the old instance out
//     exactly like the health monitor (optimistic status flip -> error reason
//     -> billing session closed immediately) and re-routes the customer to the
//     next-cheapest spot candidate if the warned pre-stage didn't already.
//
// Providers with no native preemption signal return provider.ErrNotImplemented
// and are skipped — the health monitor's GetStatus polling already catches
// their instances disappearing, so the watcher never duplicates that path.
type Watcher struct {
	db             watcherDB
	providers      ProviderGetter
	reroute        RerouteFunc
	logger         *slog.Logger
	interval       time.Duration
	onEvent        func(event db.InstanceEvent)
	instanceEvents InstanceEventSink

	mu    sync.Mutex
	state map[string]*instanceState // instanceID -> per-instance dedupe
}

// instanceState dedupes per-instance actions across sweeps: the warned event
// fires once, and the customer is re-routed at most once per preemption.
type instanceState struct {
	warned   bool
	rerouted bool
}

// NewWatcher constructs a preemption watcher. DB, Providers, and Reroute must
// be non-nil; a nil Logger falls back to slog.Default(), a zero Interval to
// 60s (the health-monitor cadence).
func NewWatcher(deps WatcherDeps) *Watcher {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := deps.Interval
	if interval == 0 {
		interval = 60 * time.Second
	}
	return &Watcher{
		db:             deps.DB,
		providers:      deps.Providers,
		reroute:        deps.Reroute,
		logger:         logger,
		interval:       interval,
		onEvent:        deps.OnEvent,
		instanceEvents: deps.InstanceEvents,
		state:          make(map[string]*instanceState),
	}
}

// Start runs the watcher loop. Sweeps immediately on startup, then on each
// tick. Blocks until ctx is cancelled.
func (w *Watcher) Start(ctx context.Context) {
	w.Sweep(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("spot preemption watcher stopped")
			return
		case <-ticker.C:
			w.Sweep(ctx)
		}
	}
}

// maxConcurrentPolls bounds parallel PreemptionState calls per sweep,
// mirroring the health monitor's semaphore.
const maxConcurrentPolls = 10

// Sweep polls PreemptionState once for every active spot instance and acts on
// the result. Exported so tests (and operational tooling) can drive sweeps
// without the ticker.
func (w *Watcher) Sweep(ctx context.Context) {
	instances, err := w.db.ListActiveInstances(ctx)
	if err != nil {
		w.logger.Error("preemption sweep: failed to list active instances",
			slog.String("error", err.Error()),
		)
		return
	}

	var spot []db.Instance
	for _, inst := range instances {
		if inst.Tier == string(provider.TierSpot) {
			spot = append(spot, inst)
		}
	}
	w.pruneState(spot)
	if len(spot) == 0 {
		return
	}

	sem := make(chan struct{}, maxConcurrentPolls)
	var wg sync.WaitGroup
	for i := range spot {
		inst := spot[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release

			w.checkInstance(ctx, inst)
		}()
	}
	wg.Wait()
}

// pruneState drops dedupe entries for instances that are no longer active, so
// the map doesn't grow with instance churn.
func (w *Watcher) pruneState(active []db.Instance) {
	alive := make(map[string]bool, len(active))
	for _, inst := range active {
		alive[inst.InstanceID] = true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for id := range w.state {
		if !alive[id] {
			delete(w.state, id)
		}
	}
}

// checkInstance polls one spot instance's preemption state and acts on it.
func (w *Watcher) checkInstance(ctx context.Context, inst db.Instance) {
	prov, ok := w.providers.Get(inst.UpstreamProvider)
	if !ok {
		w.logger.Warn("preemption sweep: provider not found",
			slog.String("instance_id", inst.InstanceID),
			slog.String("provider", inst.UpstreamProvider),
		)
		return
	}
	sc, ok := prov.(provider.SpotCapability)
	if !ok {
		return // provider has no preemption signal; health monitor covers it
	}

	status, err := sc.PreemptionState(ctx, inst.UpstreamID, inst.Region)
	if err != nil {
		if errors.Is(err, provider.ErrNotImplemented) {
			return // no native signal; health monitor covers disappearance
		}
		// Transient poll failure — never close billing on a flaky poll.
		w.logger.Warn("preemption sweep: state poll failed",
			slog.String("instance_id", inst.InstanceID),
			slog.String("provider", inst.UpstreamProvider),
			slog.String("error", err.Error()),
		)
		return
	}

	switch status {
	case provider.PreemptionWarned:
		w.handleWarned(ctx, inst)
	case provider.PreemptionReclaimed:
		w.handleReclaimed(ctx, inst)
	}
}

// handleWarned records the drain signal and pre-stages a re-route inside the
// provider's grace window, once per instance. The warned instance keeps
// running (and billing) until the provider actually reclaims it — closeout
// happens on reclaimed.
func (w *Watcher) handleWarned(ctx context.Context, inst db.Instance) {
	w.mu.Lock()
	st := w.state[inst.InstanceID]
	if st == nil {
		st = &instanceState{}
		w.state[inst.InstanceID] = st
	}
	if st.warned {
		w.mu.Unlock()
		return // already draining + pre-staged
	}
	st.warned = true
	w.mu.Unlock()

	w.logger.Warn("SPOT_PREEMPTION_WARNED: provider signalled preemption, pre-staging re-route",
		slog.String("instance_id", inst.InstanceID),
		slog.String("org_id", inst.OrgID),
		slog.String("provider", inst.UpstreamProvider),
	)

	w.recordEvent(ctx, inst, "preemption_warned", "spot_preemption_warned")

	if err := w.reroute(ctx, inst); err != nil {
		// Leave rerouted=false so the reclaimed path retries the re-route.
		w.logger.Warn("preemption sweep: pre-staged re-route failed, will retry on reclaim",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
		return
	}
	w.mu.Lock()
	st.rerouted = true
	w.mu.Unlock()
}

// handleReclaimed closes out the reclaimed instance — optimistic status flip,
// error reason, immediate billing stop, event — and re-routes the customer to
// the next-cheapest spot candidate unless the warned pre-stage already did.
// The closeout mirrors health.Monitor's spot-interruption path so both signal
// sources (status disappearance vs. native preemption) behave identically.
func (w *Watcher) handleReclaimed(ctx context.Context, inst db.Instance) {
	w.logger.Warn("SPOT_PREEMPTION_RECLAIMED: provider reclaimed spot instance",
		slog.String("instance_id", inst.InstanceID),
		slog.String("org_id", inst.OrgID),
		slog.String("provider", inst.UpstreamProvider),
	)

	// Optimistic locking: only close out if we win the status transition.
	// Prevents duplicate closeouts when the health monitor or a user-initiated
	// termination races this sweep.
	updated, err := w.db.UpdateInstanceStatus(ctx, inst.InstanceID, inst.Status, "error")
	if err != nil {
		w.logger.Error("preemption sweep: failed to set error state for reclaimed instance",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
		return
	}
	if !updated {
		return // status already changed concurrently — the winner owns closeout
	}

	const reclaimReason = "spot instance reclaimed by provider"
	_, _ = w.db.SetInstanceError(ctx, inst.InstanceID, reclaimReason)
	// instance.failed webhook (GPU-45): emitted only after winning the
	// optimistic flip above, so a racing health-monitor closeout or user
	// terminate cannot double-emit.
	if w.instanceEvents != nil {
		w.instanceEvents.InstanceFailed(ctx, &inst, reclaimReason)
	}

	w.recordEvent(ctx, inst, "preemption_reclaimed", "spot_preemption_reclaimed")

	w.mu.Lock()
	st := w.state[inst.InstanceID]
	if st == nil {
		st = &instanceState{}
		w.state[inst.InstanceID] = st
	}
	alreadyRerouted := st.rerouted
	st.rerouted = true // claim the re-route before releasing the lock
	w.mu.Unlock()
	if alreadyRerouted {
		return // warned pre-stage already placed the replacement
	}

	if err := w.reroute(ctx, inst); err != nil {
		w.logger.Error("preemption sweep: re-route after reclaim failed",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
	}
}

// recordEvent writes an instance event and notifies the SSE callback,
// mirroring the health monitor's event shape.
func (w *Watcher) recordEvent(ctx context.Context, inst db.Instance, eventType, reason string) {
	metadata, _ := json.Marshal(map[string]string{
		"reason":   reason,
		"gpu_type": inst.GPUType,
		"region":   inst.Region,
		"tier":     inst.Tier,
	})
	event := &db.InstanceEvent{
		InstanceID: inst.InstanceID,
		OrgID:      inst.OrgID,
		EventType:  eventType,
		Metadata:   metadata,
	}
	if err := w.db.CreateInstanceEvent(ctx, event); err != nil {
		w.logger.Error("preemption sweep: failed to log event",
			slog.String("instance_id", inst.InstanceID),
			slog.String("event_type", eventType),
			slog.String("error", err.Error()),
		)
	}
	if w.onEvent != nil {
		w.onEvent(*event)
	}
}
