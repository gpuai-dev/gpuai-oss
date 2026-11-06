package health

// Tunnel veto: the machine's word against the provider's.
//
// handleNonSpotFailure fails a running instance once the provider's status API
// reports it terminated/exited/error. Provider status APIs lag and flap: on
// 2026-10-06 an RTX 3090 was failed 130s into a session while SSH through the
// tunnel answered with an unchanged boot ID. So while a running instance's
// sshd answers through our own FRP gateway, the monitor keeps it (the veto).
//
// The veto is bounded. sshd answering proves the host is up, not that the GPU
// works, and several provider states that map to error leave sshd up: a
// host reported "offline" or "degraded", and statuses an adapter does not recognize.
// Unbounded, a provider that keeps reporting such a state would keep a dead
// GPU running and billing forever. Past tunnelVetoMaxDuration the provider
// wins again.

import (
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// tunnelVetoMaxDuration is how long the tunnel veto may keep a running
// instance the provider keeps reporting gone. A lagging or flapping status
// clears within seconds to minutes; the same answer for half an hour is not
// lag but a state the provider means (a reclaimed machine, a degraded host).
// Past it the instance is failed as it was before the veto existed, and an
// ERROR line (tunnelVetoExpiredMessage) asks a human to look at the
// disagreement. It does not stretch on fresh GPU telemetry the way the
// unreachable escalation does: that would make the veto unbounded again for
// any host whose agent keeps posting, and whether a machine its provider calls
// gone should keep billing is the call that ERROR line hands to a human.
const tunnelVetoMaxDuration = 30 * time.Minute

// tunnelVetoStartMessage is the WARN line logged once when a veto starts, and
// tunnelVetoExpiredMessage the ERROR line logged when a veto outlives
// tunnelVetoMaxDuration and the instance goes back to the normal failure path.
const (
	tunnelVetoStartMessage   = "health check: provider reports the instance gone but it answers through the tunnel; not failing it"
	tunnelVetoExpiredMessage = "tunnel veto expired: failing an instance the provider reports gone although it still answers through the tunnel"
)

// tunnelVetoTracker remembers when the veto first kept each instance, so the
// bound runs from the first veto rather than restarting every tick. In memory
// like reachabilityTracker: a gpuctl restart starts the bound over. Accessed
// from the monitor's concurrent checks, so every method takes the mutex. The
// zero value is ready to use, so a Monitor built without NewMonitor (tests)
// still works.
type tunnelVetoTracker struct {
	mu    sync.Mutex
	since map[string]time.Time
}

// begin returns when instanceID's current veto started, recording now when
// this is its first veto (reported by first).
func (t *tunnelVetoTracker) begin(instanceID string, now time.Time) (since time.Time, first bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if s, ok := t.since[instanceID]; ok {
		return s, false
	}
	if t.since == nil {
		t.since = make(map[string]time.Time)
	}
	t.since[instanceID] = now
	return now, true
}

// end forgets instanceID's veto and reports when it had started (ok is false
// when there was none).
func (t *tunnelVetoTracker) end(instanceID string) (since time.Time, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	since, ok = t.since[instanceID]
	delete(t.since, instanceID)
	return since, ok
}

// retain drops vetoes for instances that left the active set (failed,
// terminated), so the map does not grow for the lifetime of the process.
func (t *tunnelVetoTracker) retain(live map[string]struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for id := range t.since {
		if _, ok := live[id]; !ok {
			delete(t.since, id)
		}
	}
}

// size reports how many instances currently hold a veto.
func (t *tunnelVetoTracker) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.since)
}

// answersThroughTunnel reports whether the instance's sshd answers through the
// FRP gateway right now. False when there is nothing to probe (no gateway
// configured, no allocated port): no evidence keeps the old behavior.
func (m *Monitor) answersThroughTunnel(inst *db.Instance) bool {
	if m.tunnelProbeHost == "" || inst.FRPRemotePort == nil {
		return false
	}
	addr := net.JoinHostPort(m.tunnelProbeHost, strconv.Itoa(*inst.FRPRemotePort))
	probe := m.probeTunnel
	if probe == nil {
		probe = func(a string) error { return probeSSHBanner(a, probeDialTimeout, probeReadTimeout) }
	}
	return probe(addr) == nil
}

// tunnelVeto reports whether to keep an instance the provider reports
// terminated/exited/error because its sshd still answers through the tunnel.
// handleNonSpotFailure asks it BEFORE its retry loop, so a kept instance costs
// one probe per tick instead of 30s of retry sleeps in one of the monitor's
// maxConcurrentChecks slots (a dozen vetoed rows would fill the whole tick).
//
// Only running instances qualify: a booting one is the engine's poll loop's
// to judge, and the veto exists to protect a machine a customer is using.
func (m *Monitor) tunnelVeto(inst *db.Instance, now time.Time) bool {
	if inst.Status != "running" || !m.answersThroughTunnel(inst) {
		return false
	}
	since, first := m.tunnelVetoes.begin(inst.InstanceID, now)
	vetoedFor := now.Sub(since)
	if vetoedFor > tunnelVetoMaxDuration {
		// The provider has said "gone" for longer than any flap. Let the
		// normal failure path run, and page: a machine that answers SSH while
		// its provider calls it dead needs a human to say which one is right.
		m.logger.Error(tunnelVetoExpiredMessage,
			slog.String("instance_id", inst.InstanceID),
			slog.String("provider", inst.UpstreamProvider),
			slog.String("org_id", inst.OrgID),
			slog.Time("vetoed_since", since),
			slog.Duration("vetoed_for", vetoedFor),
		)
		return false
	}
	if first {
		m.logger.Warn(tunnelVetoStartMessage,
			slog.String("instance_id", inst.InstanceID),
			slog.String("provider", inst.UpstreamProvider),
			slog.String("org_id", inst.OrgID),
			slog.Duration("veto_bound", tunnelVetoMaxDuration),
		)
	} else {
		m.logger.Debug("health check: tunnel veto continues",
			slog.String("instance_id", inst.InstanceID),
			slog.Duration("vetoed_for", vetoedFor),
		)
	}
	return true
}

// endTunnelVeto ends an instance's veto once the provider reports it running
// again: provider and tunnel agree, so a later disagreement gets a fresh
// bound.
func (m *Monitor) endTunnelVeto(inst *db.Instance) {
	if since, ok := m.tunnelVetoes.end(inst.InstanceID); ok {
		m.logger.Info("health check: provider reports the instance running again; tunnel veto over",
			slog.String("instance_id", inst.InstanceID),
			slog.String("provider", inst.UpstreamProvider),
			slog.Duration("vetoed_for", time.Since(since)),
		)
	}
}
