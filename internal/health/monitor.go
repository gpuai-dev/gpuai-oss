package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// MonitorDeps holds the dependencies for the health monitor.
type MonitorDeps struct {
	DB       *db.Pool
	Registry *provider.Registry
	Logger   *slog.Logger
	Interval time.Duration // default 60s
	// OnEvent is called when a health event is detected (interruption, failure).
	// Used to push events to SSE broker. Nil means no notification.
	OnEvent func(event db.InstanceEvent)
	// InstanceEvents receives instance.failed webhook events (GPU-45) when a
	// closeout path wins its optimistic status flip. Nil disables webhook
	// publishing. public.InstanceEventPublisher satisfies it.
	InstanceEvents InstanceEventSink
	// TunnelProbeHost is the public host of the FRP gateway that terminates
	// customer SSH tunnels — the address the reachability probe dials on each
	// instance's allocated remote port (GPU-49). Empty DISABLES tunnel probing,
	// matching the "empty FRP_TOKEN disables tunneling" convention, so dev
	// setups without a gateway don't emit a storm of false unreachables.
	TunnelProbeHost string

	// Tunnel reachability knobs (GPU-77). Every one of
	// them is ZERO-MEANS-DEFAULT: a deployment that sets none behaves exactly
	// like the code that predates them. They exist because the numbers below
	// were derived from ten days of measured production data, and the next
	// incident may need them moved without waiting for a deploy — the config
	// layer parses TUNNEL_FAIL_THRESHOLD and TUNNEL_ESCALATE_AFTER into them.
	//
	// FailThreshold: consecutive failed probes before an outage is announced.
	// EscalateAfter: how long an announced connect_failed outage runs before
	//   the instance is failed outright.
	FailThreshold int
	EscalateAfter time.Duration
}

// InstanceEventSink is the narrow webhook-emission seam the monitor needs
// (GPU-45). Declared locally — mirroring the per-consumer seam convention —
// because the concrete publisher lives in internal/api/public, which this
// package must not import.
type InstanceEventSink interface {
	InstanceFailed(ctx context.Context, inst *db.Instance, reason string)
}

// Monitor polls provider APIs to detect instance health issues.
type Monitor struct {
	db              *db.Pool
	registry        *provider.Registry
	logger          *slog.Logger
	interval        time.Duration
	onEvent         func(event db.InstanceEvent)
	instanceEvents  InstanceEventSink
	tunnelProbeHost string
	// nonSpotRetryEvery overrides nonSpotRetryInterval (tests); zero keeps it.
	nonSpotRetryEvery time.Duration
	// probeTunnel overrides the SSH-banner probe (tests); nil uses
	// probeSSHBanner.
	probeTunnel func(addr string) error
	// tunnelVetoes bounds the tunnel veto in handleNonSpotFailure: when each
	// instance was first kept against its provider's word. See tunnel_veto.go.
	tunnelVetoes tunnelVetoTracker
	// reachability turns per-tick probe results into transitions so a dead
	// tunnel pages once, not once per tick (GPU-49). See reachability.go.
	reachability *reachabilityTracker
	// Resolved GPU-77 knobs (see MonitorDeps for what each one governs).
	// failThreshold/escalateAfter are ALSO held by the tracker, which is what
	// actually reads them per probe; the copies here exist so the resolution
	// happens exactly once, in NewMonitor, and is assertable.
	failThreshold int
	escalateAfter time.Duration
	// tick accumulates one checkAll pass's probe counts and escalation
	// candidates; reset at the top of each pass and read after its probes
	// finish, so the storm veto judges a whole tick at once.
	tick tickState
}

// tickState is the per-checkAll scratch the reachability escalation path
// needs: how many tunnels were probed, how many failed, and which announced
// outages are old enough to fail their instance.
type tickState struct {
	probed atomic.Int64
	failed atomic.Int64

	mu          sync.Mutex
	escalations []escalationCandidate
}

func (t *tickState) reset() {
	t.probed.Store(0)
	t.failed.Store(0)
	t.mu.Lock()
	t.escalations = nil
	t.mu.Unlock()
}

// escalationCandidate is one instance whose tunnel outage crossed
// unreachableEscalateAfter during this tick.
type escalationCandidate struct {
	inst *db.Instance
	// downSince is the episode's first failed probe — the timestamp billing
	// is closed back to, so the customer pays nothing for the dead window.
	downSince time.Time
}

// unreachableStormMinFailed and unreachableStormFraction define the gateway-
// storm veto: when at least this many probes fail in one tick AND they are at
// least this fraction of all probes, the broken thing is almost certainly the
// FRP gateway (or the path to it), not that many customer hosts dying in the
// same minute — so no instance is auto-terminated that tick. Escalation is
// re-evaluated every tick, so a real multi-host outage only waits until the
// fleet-wide picture clears.
const (
	unreachableStormMinFailed = 3
	unreachableStormFraction  = 0.5

	// unreachableStormExemptMultiple un-wedges the storm veto: an episode
	// this many escalation-graces old escalates even during a storm tick.
	// Without it, a small fleet where 3 of ≤6 tunnels genuinely die satisfies
	// the veto on every tick forever and the dead instances bill until a human
	// intervenes — the exact bug this feature exists to fix. The exemption is
	// safe against the veto's own scenario (gateway outage, hosts alive)
	// because a live host keeps posting telemetry outside the tunnel path, so
	// the telemetry veto in escalateUnreachable still blocks its termination.
	//
	// Expressed as a MULTIPLE rather than a fixed duration so that retuning
	// the grace (TUNNEL_ESCALATE_AFTER) keeps the exemption proportionally
	// past it; a fixed 30m against a configured 30m grace would exempt every
	// candidate the moment it qualified, silently disabling the veto.
	unreachableStormExemptMultiple = 3

	// TunnelStormAlertMessage is the EXACT slog message a log-based alert
	// should match — a wire contract, like TunnelUnreachableAlertMessage. It pages because a suppressed-escalation
	// storm is invisible otherwise: the per-instance unreachable alerts fired
	// long ago, and nothing else says "the gateway is probably down AND
	// auto-remediation is standing off".
	TunnelStormAlertMessage = "tunnel unreachable storm: escalation suppressed"
)

// NewMonitor creates a new health monitor. Every GPU-77 knob resolves here,
// once: a zero (unset) value takes the shipped default, so a deployment that
// configures nothing behaves exactly as it did before the knobs existed.
func NewMonitor(deps MonitorDeps) *Monitor {
	interval := deps.Interval
	if interval == 0 {
		interval = 60 * time.Second
	}
	failThreshold := deps.FailThreshold
	if failThreshold <= 0 {
		failThreshold = reachabilityFailThreshold
	}
	escalateAfter := deps.EscalateAfter
	if escalateAfter <= 0 {
		escalateAfter = unreachableEscalateAfter
	}
	return &Monitor{
		db:              deps.DB,
		registry:        deps.Registry,
		logger:          deps.Logger,
		interval:        interval,
		onEvent:         deps.OnEvent,
		instanceEvents:  deps.InstanceEvents,
		tunnelProbeHost: deps.TunnelProbeHost,
		reachability:    newReachabilityTracker(failThreshold, escalateAfter),
		failThreshold:   failThreshold,
		escalateAfter:   escalateAfter,
	}
}

// emitInstanceFailed publishes the instance.failed webhook event (GPU-45).
// Callers invoke it only after WINNING the optimistic UpdateInstanceStatus
// flip to error, which is the exactly-once guard — a concurrent terminator
// or a re-check of an already-closed-out instance never reaches this.
func (m *Monitor) emitInstanceFailed(ctx context.Context, inst *db.Instance, reason string) {
	if m.instanceEvents != nil {
		m.instanceEvents.InstanceFailed(ctx, inst, reason)
	}
}

// SetOnEvent sets the callback invoked when a health event is detected.
// Allows post-construction wiring (same pattern as Engine.SetOnStatusChange).
func (m *Monitor) SetOnEvent(fn func(event db.InstanceEvent)) {
	m.onEvent = fn
}

// Start runs the health monitor loop. Checks immediately on startup,
// then on each tick. Blocks until ctx is cancelled.
func (m *Monitor) Start(ctx context.Context) {
	m.checkAll(ctx)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.logger.Info("health monitor stopped")
			return
		case <-ticker.C:
			m.checkAll(ctx)
		}
	}
}

// maxConcurrentChecks limits parallel provider API calls.
const maxConcurrentChecks = 10

// checkAll queries all active instances and checks their health.
func (m *Monitor) checkAll(ctx context.Context) {
	instances, err := m.db.ListActiveInstances(ctx)
	if err != nil {
		m.logger.Error("health check: failed to list active instances",
			slog.String("error", err.Error()),
		)
		return
	}

	// Evict reachability state for instances that dropped out of the active set
	// (terminated, stopped, errored). Done BEFORE the short-circuit below so an
	// account that scales to zero instances doesn't strand its tracker entries
	// for the lifetime of the process (GPU-49).
	//
	// Announced outages that died with their instance come back here: those are
	// the customers who watched SSH die and terminated the instance themselves,
	// so no recovery probe will ever notice the outage ended. They are credited
	// AFTER this tick's probes (GPU-77) so the circuit breaker judges them
	// against a fresh fleet-wide picture rather than the previous tick's.
	live := make(map[string]struct{}, len(instances))
	for i := range instances {
		live[instances[i].InstanceID] = struct{}{}
	}
	evicted := m.reachability.retain(live)
	m.tunnelVetoes.retain(live)

	if len(instances) == 0 {
		// Fleet scaled to zero: no probes will run, so credit the evictions
		// now rather than dropping them. With nothing probed the breaker
		// cannot trip, and the per-credit guards still apply.
		m.tick.reset()
		m.creditEvictedEpisodes(ctx, evicted)
		return // Short-circuit: nothing to check.
	}

	// Bounded concurrency with semaphore channel.
	m.tick.reset()
	sem := make(chan struct{}, maxConcurrentChecks)
	var wg sync.WaitGroup

	for i := range instances {
		inst := instances[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release

			m.checkInstance(ctx, &inst)
		}()
	}
	wg.Wait()

	// Escalations run after every probe has reported, so the storm veto can
	// compare this tick's failure count against the whole probed set.
	m.processUnreachableEscalations(ctx)

	// Money settles only after the full tick has reported, so the credit
	// circuit breaker judges every window against the complete fleet-wide
	// picture: recoveries observed mid-tick first, then the episodes that
	// left the active set mid-outage.
	m.settlePendingRecoveryCredits(ctx)
	m.creditEvictedEpisodes(ctx, evicted)
}

// processUnreachableEscalations fails the instances whose tunnel outages
// crossed unreachableEscalateAfter this tick — unless the tick looks like a
// gateway storm, in which case terminating customer instances would compound
// our own outage into destroyed customer work.
func (m *Monitor) processUnreachableEscalations(ctx context.Context) {
	m.tick.mu.Lock()
	candidates := m.tick.escalations
	m.tick.escalations = nil
	m.tick.mu.Unlock()
	if len(candidates) == 0 {
		return
	}

	probed, failed := m.tick.probed.Load(), m.tick.failed.Load()
	if failed >= unreachableStormMinFailed && float64(failed) >= unreachableStormFraction*float64(probed) {
		// Storm tick: defer everything except episodes so old the veto itself
		// has become the outage (see unreachableStormExemptMultiple).
		now := time.Now().UTC()
		exemptAfter := time.Duration(unreachableStormExemptMultiple) * m.escalateAfter
		exempt := candidates[:0]
		for _, c := range candidates {
			if now.Sub(c.downSince) >= exemptAfter {
				exempt = append(exempt, c)
			}
		}
		// One line per tick, not per instance: THE ALERT for the gateway-storm
		// state (exact message = wire contract, see the constant); the
		// per-instance unreachable alerts already fired.
		m.logger.Error(TunnelStormAlertMessage,
			slog.Int64("failed_probes", failed),
			slog.Int64("probed", probed),
			slog.Int("escalations_deferred", len(candidates)-len(exempt)),
			slog.Int("escalations_exempt", len(exempt)),
		)
		for _, c := range exempt {
			m.escalateUnreachable(ctx, c.inst, c.downSince, true)
		}
		return
	}

	for _, c := range candidates {
		m.escalateUnreachable(ctx, c.inst, c.downSince, false)
	}
}

// escalateUnreachable closes out an instance whose SSH tunnel has been
// continuously connect_failed past unreachableEscalateAfter: terminate the
// upstream node, fail the instance, and close billing backdated to the outage
// start (the true-up in CloseBillingSession then refunds every cent metered
// during the dead window). Mirrors the handleGhostInstance closeout order.
//
// stormExempt marks a candidate escalating THROUGH an active storm tick via
// the age exemption. That path leans entirely on telemetry to tell "dead
// host" from "dead gateway" — so an instance with no telemetry history at
// all (no nvidia-smi in its image, or a sampler that never started) carries
// no such evidence, and terminating it during a storm would mass-kill
// healthy telemetry-less instances the moment a gateway outage crosses the
// exemption age. Those defer until the storm clears; a normal-tick
// escalation of the same instance proceeds, because there the fleet-wide
// picture already says the gateway is fine.
func (m *Monitor) escalateUnreachable(ctx context.Context, inst *db.Instance, downSince time.Time, stormExempt bool) {
	// Telemetry veto: a fresh GPU-telemetry heartbeat arrives outside the
	// customer tunnel, so it proves the host is alive and only the tunnel
	// path is broken. Ops fixes tunnels; terminating destroys work.
	ts, ok, err := m.db.LatestGPUTelemetryTS(ctx, inst.InstanceID)
	if err != nil {
		m.logger.Warn("unreachable escalation: telemetry check failed, deferring",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
		return // retried next tick
	}
	if stormExempt && !ok {
		m.logger.Warn("unreachable escalation deferred: storm active and instance has no telemetry history",
			slog.String("instance_id", inst.InstanceID),
			slog.String("org_id", inst.OrgID),
		)
		return // retried next tick; escalates as soon as the storm clears
	}
	if ok && time.Since(ts) < telemetryAliveWindow {
		m.logger.Warn("unreachable escalation vetoed: host telemetry still alive",
			slog.String("instance_id", inst.InstanceID),
			slog.String("org_id", inst.OrgID),
			slog.Time("last_telemetry", ts),
		)
		return
	}

	// Optimistic locking: only the process that wins the running→error flip
	// does the closeout, so a concurrent user termination stays authoritative.
	updated, err := m.db.UpdateInstanceStatus(ctx, inst.InstanceID, "running", "error")
	if err != nil {
		m.logger.Error("unreachable escalation: failed to set error state",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
		return
	}
	if !updated {
		return // Status already changed concurrently.
	}

	// This episode's money is now OURS to settle, via the backdated
	// CloseBillingSession below — which refunds every metered cent of the dead
	// window and is therefore money-equivalent to a credit. Disqualify the
	// episode from the ledger-credit path immediately, before that close, so a
	// probe result racing in behind this flip cannot pay the same window twice
	// (GPU-77, T-k00-04).
	m.reachability.markEscalated(inst.InstanceID)

	m.logger.Error("INSTANCE_UNREACHABLE: failing instance after sustained tunnel outage",
		slog.String("instance_id", inst.InstanceID),
		slog.String("org_id", inst.OrgID),
		slog.Time("down_since", downSince),
	)

	const unreachableReason = "instance unreachable: SSH tunnel down for over 10 minutes"
	_, _ = m.db.SetInstanceError(ctx, inst.InstanceID, unreachableReason)
	m.emitInstanceFailed(ctx, inst, unreachableReason)

	// Close billing back to the outage start — the customer could not use a
	// second of the dead window, so they don't pay for it. This runs BEFORE
	// the provider Terminate (the slow, failure-prone step): the flip above
	// fires exactly once, so a crash between it and this close would leave
	// the session open — and metering — until the reconciler closes it at
	// ITS now(), losing the backdate. Keep that window as small as possible.
	//
	// The backdate prefers the DURABLE flag over the tracker's downSince for
	// the same reason the credit path does (creditWindowStart): after a
	// restart mid-outage the tracker only knows the post-restart segment, and
	// closing to that would bill the customer for the pre-restart dead time.

	// The upstream node may still be "running" (and costing money) even
	// though nothing can reach it — unlike the ghost/terminal paths, we must
	// tear it down ourselves. A failed terminate is logged, not retried here:
	// the reconciler sweeps error-state instances with live upstreams.
	if prov, ok := m.registry.Get(inst.UpstreamProvider); ok {
		if err := prov.Terminate(ctx, inst.UpstreamID, inst.Region); err != nil {
			m.logger.Error("unreachable escalation: upstream terminate failed",
				slog.String("instance_id", inst.InstanceID),
				slog.String("provider", inst.UpstreamProvider),
				slog.String("error", err.Error()),
			)
		}
	}

	metadata, _ := json.Marshal(map[string]string{
		"reason":     "tunnel_unreachable",
		"down_since": downSince.Format(time.RFC3339),
		"gpu_type":   inst.GPUType,
		"region":     inst.Region,
		"tier":       inst.Tier,
	})
	event := &db.InstanceEvent{
		InstanceID: inst.InstanceID,
		OrgID:      inst.OrgID,
		EventType:  "failed",
		Metadata:   metadata,
	}
	if err := m.db.CreateInstanceEvent(ctx, event); err != nil {
		m.logger.Error("unreachable escalation: failed to log event",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
	}

	if m.onEvent != nil {
		m.onEvent(*event)
	}
}

// checkInstance checks a single instance's health via provider API.
func (m *Monitor) checkInstance(ctx context.Context, inst *db.Instance) {
	prov, ok := m.registry.Get(inst.UpstreamProvider)
	if !ok {
		m.logger.Warn("health check: provider not found",
			slog.String("instance_id", inst.InstanceID),
			slog.String("provider", inst.UpstreamProvider),
		)
		return
	}

	status, err := prov.GetStatus(ctx, inst.UpstreamID, inst.Region)
	if err != nil {
		// A not-found error means the upstream pod/VM is gone (hard-deleted or
		// GC'd out-of-band). Some adapters surface this as an
		// error rather than a synthetic "terminated" status, so the terminal
		// switch below never sees it. Treat it as a ghost: terminate the
		// instance and close its billing session so we stop billing a customer
		// for a node that no longer exists. Transient poll failures (network
		// blips, 5xx) must NOT close billing — fall through to log-and-return
		// so a flaky poll can't kill a live instance.
		if isNotFoundErr(err) {
			m.handleGhostInstance(ctx, inst)
			return
		}
		m.logger.Warn("health check: provider status poll failed",
			slog.String("instance_id", inst.InstanceID),
			slog.String("provider", inst.UpstreamProvider),
			slog.String("error", err.Error()),
		)
		return
	}

	switch status.Status {
	case "running":
		// Provider and tunnel agree again, so any tunnel veto is over and a
		// later disagreement gets a fresh bound.
		m.endTunnelVeto(inst)
		// The provider says the node is up. That is NOT the same as the
		// customer being able to reach it: a dead sshd or a dead frpc leaves
		// the upstream VM happily "running" while SSH is gone. Probe the
		// tunnel before calling this healthy (GPU-49).
		m.checkReachability(ctx, inst)
		return

	case "terminated", "exited":
		// Check if this is a spot interruption or unexpected failure.
		if inst.Tier == "spot" {
			m.handleSpotInterruption(ctx, inst)
		} else {
			// Non-spot: retry before declaring failure.
			m.handleNonSpotFailure(ctx, inst, prov)
		}

	case "error":
		// Provider reports error state.
		if inst.Tier == "spot" {
			m.handleSpotInterruption(ctx, inst)
		} else {
			m.handleNonSpotFailure(ctx, inst, prov)
		}
	}
}

// handleSpotInterruption handles a detected spot instance interruption.
// Immediately stops billing and logs interruption event.
func (m *Monitor) handleSpotInterruption(ctx context.Context, inst *db.Instance) {
	m.logger.Warn("SPOT_INTERRUPTION: detected spot instance interruption",
		slog.String("instance_id", inst.InstanceID),
		slog.String("org_id", inst.OrgID),
	)

	// Use optimistic locking: only act if we successfully transition the state.
	// This prevents duplicate events if the monitor runs while another process
	// is also transitioning (e.g., user-initiated termination).
	updated, err := m.db.UpdateInstanceStatus(ctx, inst.InstanceID, inst.Status, "error")
	if err != nil {
		m.logger.Error("health check: failed to set error state for spot interruption",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
		return
	}
	if !updated {
		// Status already changed concurrently -- skip event logging.
		m.logger.Debug("health check: instance status already changed, skipping event",
			slog.String("instance_id", inst.InstanceID),
		)
		return
	}

	// Set error reason.
	const interruptReason = "spot instance interrupted by provider"
	_, _ = m.db.SetInstanceError(ctx, inst.InstanceID, interruptReason)
	m.emitInstanceFailed(ctx, inst, interruptReason)

	// Close billing session immediately.

	// Log event to instance_events.
	metadata, _ := json.Marshal(map[string]string{
		"reason":   "spot_interruption",
		"gpu_type": inst.GPUType,
		"region":   inst.Region,
		"tier":     inst.Tier,
	})
	event := &db.InstanceEvent{
		InstanceID: inst.InstanceID,
		OrgID:      inst.OrgID,
		EventType:  "interrupted",
		Metadata:   metadata,
	}
	if err := m.db.CreateInstanceEvent(ctx, event); err != nil {
		m.logger.Error("health check: failed to log interruption event",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
	}

	// Notify SSE subscribers.
	if m.onEvent != nil {
		m.onEvent(*event)
	}
}

// handleGhostInstance terminates an instance whose upstream node is gone
// (provider GetStatus returned a not-found error) and closes its billing
// session. A hard-deleted pod is unambiguously gone, so — unlike
// handleNonSpotFailure — there is no retry window. The closeout mirrors the
// terminal-status branches (UpdateInstanceStatus → SetInstanceError →
// CloseBillingSession → CreateInstanceEvent → onEvent) so a ghost is handled
// consistently with a provider-reported termination.
func (m *Monitor) handleGhostInstance(ctx context.Context, inst *db.Instance) {
	m.logger.Warn("INSTANCE_GHOST: upstream node not found, closing out instance",
		slog.String("instance_id", inst.InstanceID),
		slog.String("org_id", inst.OrgID),
		slog.String("provider", inst.UpstreamProvider),
	)

	// Optimistic locking: only act if we successfully transition the state.
	// Prevents duplicate events if another process is concurrently
	// transitioning (e.g. user-initiated termination).
	updated, err := m.db.UpdateInstanceStatus(ctx, inst.InstanceID, inst.Status, "error")
	if err != nil {
		m.logger.Error("health check: failed to set error state for ghost instance",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
		return
	}
	if !updated {
		return // Status already changed concurrently.
	}

	const ghostReason = "instance failed: upstream node not found"
	_, _ = m.db.SetInstanceError(ctx, inst.InstanceID, ghostReason)
	m.emitInstanceFailed(ctx, inst, ghostReason)

	// Close billing session immediately so we stop billing for a gone node.

	// Log event.
	metadata, _ := json.Marshal(map[string]string{
		"reason":   "instance_ghost",
		"gpu_type": inst.GPUType,
		"region":   inst.Region,
		"tier":     inst.Tier,
	})
	event := &db.InstanceEvent{
		InstanceID: inst.InstanceID,
		OrgID:      inst.OrgID,
		EventType:  "failed",
		Metadata:   metadata,
	}
	if err := m.db.CreateInstanceEvent(ctx, event); err != nil {
		m.logger.Error("health check: failed to log ghost event",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
	}

	if m.onEvent != nil {
		m.onEvent(*event)
	}
}

// isNotFoundErr reports whether a provider error indicates the upstream
// resource is gone. Duck-typed on the error message to match adapters that
// surface a hard-deleted pod/VM as an error rather than a synthetic
// "terminated" status (e.g. "pod <id> not found"). Kept local to the
// health package and intentionally mirrors reconcile.isNotFoundErr (which is
// unexported) so this package doesn't depend on the reconcile package.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "HTTP 404")
}

// nonSpotRetryCount is the number of retries before declaring a non-spot failure.
const nonSpotRetryCount = 3

// nonSpotRetryInterval is the delay between non-spot failure retry checks.
const nonSpotRetryInterval = 10 * time.Second

// handleNonSpotFailure retries health checks for non-spot instances before declaring failure.
// Avoids false positives from transient network blips.
func (m *Monitor) handleNonSpotFailure(ctx context.Context, inst *db.Instance, prov provider.Provider) {
	// The provider says the instance is gone. Provider status APIs lag and
	// flap, so before tearing the customer's machine down, ask the machine
	// itself: if its sshd answers through our own tunnel, it is alive, and
	// failing it would close billing on and destroy a working instance. Seen
	// live on 2026-10-06: an RTX 3090 was failed 130s into a session while SSH
	// through the tunnel answered with an unchanged boot ID. Keep it and look
	// again on the next tick; a machine that is really gone stops answering.
	// Asked before the retries so a kept instance does not sleep through them
	// every tick, and bounded (tunnelVetoMaxDuration) so a provider that means
	// it still wins: see tunnel_veto.go.
	if m.tunnelVeto(inst, time.Now().UTC()) {
		return
	}

	every := nonSpotRetryInterval
	if m.nonSpotRetryEvery > 0 {
		every = m.nonSpotRetryEvery
	}
	// Retry 3 times over ~30 seconds.
	for retry := 0; retry < nonSpotRetryCount; retry++ {
		time.Sleep(every)

		// Re-check: instance may have been terminated by user during retry window.
		current, err := m.db.GetInstance(ctx, inst.InstanceID)
		if err != nil {
			m.logger.Warn("health check: failed to re-read instance during retry",
				slog.String("instance_id", inst.InstanceID),
				slog.String("error", err.Error()),
			)
			return
		}
		if current.Status != inst.Status {
			// Status changed (user terminated, etc.) -- stop retrying.
			return
		}

		status, err := prov.GetStatus(ctx, inst.UpstreamID, inst.Region)
		if err != nil {
			m.logger.Warn("health check: retry poll failed",
				slog.String("instance_id", inst.InstanceID),
				slog.Int("retry", retry+1),
				slog.String("error", err.Error()),
			)
			continue
		}

		if status.Status == "running" {
			// Instance recovered -- false alarm.
			m.endTunnelVeto(inst)
			m.logger.Info("health check: instance recovered after transient failure",
				slog.String("instance_id", inst.InstanceID),
				slog.Int("retries", retry+1),
			)
			return
		}
	}

	// All retries exhausted -- declare failure.
	m.logger.Error("INSTANCE_FAILURE: instance failed after retries",
		slog.String("instance_id", inst.InstanceID),
		slog.String("org_id", inst.OrgID),
	)

	updated, err := m.db.UpdateInstanceStatus(ctx, inst.InstanceID, inst.Status, "error")
	if err != nil {
		m.logger.Error("health check: failed to set error state",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
		return
	}
	if !updated {
		return // Status already changed concurrently.
	}

	const failureReason = "instance failed: provider reports terminated/error"
	_, _ = m.db.SetInstanceError(ctx, inst.InstanceID, failureReason)
	m.emitInstanceFailed(ctx, inst, failureReason)

	// Close billing session.

	// Log event.
	metadata, _ := json.Marshal(map[string]string{
		"reason":   "instance_failure",
		"gpu_type": inst.GPUType,
		"region":   inst.Region,
		"tier":     inst.Tier,
	})
	event := &db.InstanceEvent{
		InstanceID: inst.InstanceID,
		OrgID:      inst.OrgID,
		EventType:  "failed",
		Metadata:   metadata,
	}
	if err := m.db.CreateInstanceEvent(ctx, event); err != nil {
		m.logger.Error("health check: failed to log failure event",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
	}

	if m.onEvent != nil {
		m.onEvent(*event)
	}
}
