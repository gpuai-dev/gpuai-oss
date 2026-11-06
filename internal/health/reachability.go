package health

// Tunnel reachability probe (GPU-49).
//
// WHY IT EXISTS: the health monitor asks the upstream provider "is this node
// running?" and the provider answers about the VM, not about the customer's
// path to it. Twice in ~10 days a pod's sshd (or its frpc client) died while
// the provider kept reporting "running" — so the monitor was satisfied, the
// dashboard was green, billing kept accruing, and the first anyone heard of it
// was a support ticket. This probe asks the question the customer actually
// cares about: can something reach sshd through the FRP gateway right now?
//
// SCOPE. Originally strictly observability; the product decision it deferred
// was made on 2026-08-20 (support ticket: a marketplace host died at 18:44 UTC, the
// customer's instance stayed status=running and kept billing until they
// terminated it by hand at 18:55). The decision: a tunnel that stays
// connect_failed for unreachableEscalateAfter IS a lifecycle failure — the
// monitor terminates the upstream node, fails the instance, and closes billing
// backdated to the outage start so the customer never pays for the dead
// window. See escalateUnreachable in monitor.go for the action and its three
// guards (connect_failed-only, live-telemetry veto, gateway-storm veto), which
// exist because the two wrong guesses this file's original scope note feared —
// terminating healthy work over a blip, or mass-terminating the fleet when the
// gateway itself dies — are still wrong.
//
// GPU-77 lifted the scope the rest of the way, deliberately: a confirmed
// outage now WRITES STATUS (instances.unreachable_since, which the API read
// paths map to the customer-facing status "unreachable") and, when the outage
// ENDS without escalating, WRITES MONEY (an unreachable_credit ledger row for
// the dead window — see credit.go). The escalation path was the only one the
// 2026-08-20 decision covered; the two commoner endings, recovery inside the
// grace and the customer terminating mid-outage, left the dead window fully
// billed and were being settled by hand ($5 of goodwill for $1.35 of harm).
// The tracker below still only *decides* — it reports which ended episodes are
// creditable and which are not; every DB/provider/ledger write stays in
// monitor.go and credit.go with the other closeout paths.
//
// ALERT CONTRACT (ALRT-02): a level=ERROR structured slog line IS the alert.
// Route it with whatever log-based alerting the deployment uses (a metric
// filter on the exact message). There is no notification client in this
// package, on purpose.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

const (
	// TunnelUnreachableAlertMessage is the EXACT slog message a log-based
	// alert should match. Changing it breaks such an alert route silently (no
	// compile error, just a dead pager). Treat it as a wire contract.
	//
	// A dedicated filter+alarm is required rather than relying on the generic
	// app_error alarm, which only fires above 5 ERRORs in 5 minutes: a single
	// dead tunnel would never clear that bar.
	TunnelUnreachableAlertMessage = "tunnel unreachable"

	// Probe failure classifications. These ride on customer-visible
	// instance_events metadata, so they are a CLOSED set of net-level facts —
	// never a raw dial error (which would leak the gateway host/port) and never
	// an upstream provider name (INFPRIV-06).
	//
	//	connect_failed — nothing accepted the TCP connection on the gateway's
	//	  remote port. The proxy mapping is gone: frpc died, lost its control
	//	  connection, or never registered.
	//	no_ssh_banner  — the connection was accepted but no SSH identification
	//	  string came back. frpc is alive and the mapping exists, but its local
	//	  dial to :22 fails — i.e. sshd is dead inside the instance.
	ReasonConnectFailed = "connect_failed"
	ReasonNoSSHBanner   = "no_ssh_banner"

	// reachabilityFailThreshold is the DEFAULT number of CONSECUTIVE failed
	// probes that make a tunnel "unreachable" (overridable per-deployment via
	// TUNNEL_FAIL_THRESHOLD → MonitorDeps.FailThreshold). One is too jumpy —
	// frpc reconnects, gateway restarts and single dropped SYNs all produce a
	// one-tick failure. At the 60s monitor cadence two consecutive failures
	// means ~1-2 minutes down, which is past every benign blip and still well
	// inside the window where telling ops beats telling a customer.
	reachabilityFailThreshold = 2

	// unreachableEscalateAfter is the DEFAULT time a tunnel must stay
	// continuously connect_failed before the monitor escalates from alerting to
	// failing the instance (terminate upstream, close billing back to the
	// outage start); overridable via TUNNEL_ESCALATE_AFTER.
	// At the 60s monitor cadence this is ~10 consecutive failed probes — far
	// past any frpc reconnect, gateway restart, or transient network event
	// observed in production, and short enough that a customer discovers the
	// failure from the dashboard rather than from a support ticket.
	unreachableEscalateAfter = 10 * time.Minute

	// unreachableEscalateMinFails is the minimum OBSERVED failed probes an
	// episode needs before it can escalate, on top of the wall-clock grace.
	// Wall-clock alone is not evidence: when the provider status poll degrades,
	// no probes run at all — the episode is neither extended nor reset — so
	// "first failure was 20 minutes ago" can describe a tunnel that was probed
	// three times and recovered in between. At the 60s cadence a genuinely
	// dead tunnel accumulates ~10 fails over the grace window; requiring 8
	// tolerates a couple of missed ticks without letting unobserved wall-time
	// terminate anything.
	unreachableEscalateMinFails = 8

	// telemetryAliveWindow: a GPU-telemetry heartbeat newer than this vetoes
	// escalation. Telemetry reaches the API outside the customer SSH tunnel,
	// so a fresh sample proves the host is alive and only the tunnel is broken
	// — terminating would destroy running customer work over a connectivity
	// problem that ops can fix. The unreachable alert has already paged.
	telemetryAliveWindow = 5 * time.Minute

	// probeDialTimeout bounds the TCP connect. A gateway near gpuctl answers a
	// healthy connect in single-digit milliseconds; 5s only has to cover a
	// loaded gateway, not a slow network.
	probeDialTimeout = 5 * time.Second

	// probeReadTimeout bounds the banner read. sshd sends its identification
	// string immediately on accept, so this is a hard deadline against a
	// tunnel that connects and then hangs — the exact shape frps produces when
	// frpc's local dial to :22 is failing. Without it the probe would block a
	// monitor worker indefinitely.
	probeReadTimeout = 5 * time.Second

	// sshBannerPrefix is the RFC 4253 §4.2 identification-string prefix. Any
	// SSH server begins its first line with it. The probe reads exactly this
	// many bytes and no more — the tunnel port is internet-facing, so the
	// bytes arriving are attacker-influenced, and the fixed-size read means a
	// hostile or wedged peer cannot make the probe allocate or spin.
	sshBannerPrefix = "SSH-"
)

// errNoSSHBanner marks "connected, but the peer is not speaking SSH" — the
// dead-sshd-behind-a-live-frpc case that a bare TCP-connect probe misses.
var errNoSSHBanner = errors.New("no SSH banner on tunnel port")

// probeSSHBanner reports whether an SSH server is reachable at addr.
//
// It is NOT enough to complete the TCP handshake. When frpc is up but sshd is
// dead, frps ACCEPTS the connection on the remote port and only then tears it
// down, because the failure happens on frpc's local dial to :22 — after the
// handshake the probe already observed. So a connect-only probe reports every
// dead-sshd tunnel as healthy, which is precisely the incident this exists to
// catch. Reading the banner is what distinguishes "the port answers" from "a
// customer can log in".
//
// The returned error is for classification only (see classifyProbeError); it
// is never surfaced verbatim to customers.
func probeSSHBanner(addr string, dialTimeout, readTimeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		return err
	}

	// The banner prefix may straddle TCP segments through the frps relay, so a
	// single Read can come up short on a healthy tunnel; keep reading until the
	// prefix length is in hand or the deadline/EOF says it never will be.
	buf := make([]byte, len(sshBannerPrefix))
	n, _ := io.ReadFull(conn, buf)
	if n >= len(sshBannerPrefix) && string(buf[:len(sshBannerPrefix)]) == sshBannerPrefix {
		return nil
	}
	// Nothing (or not a banner) before error/deadline. Connected-then-nothing
	// is the dead-sshd signature; report it as such rather than leaking the
	// raw net error.
	return errNoSSHBanner
}

// classifyProbeError maps a probe error to the closed reason enum that goes
// into instance_events metadata and the alert line. Anything that is not
// explicitly "connected but not SSH" is treated as a connect failure, so the
// enum can never widen by accident.
func classifyProbeError(err error) string {
	if errors.Is(err, errNoSSHBanner) {
		return ReasonNoSSHBanner
	}
	return ReasonConnectFailed
}

// probeDecision is what the tracker tells the caller to do about one probe
// result. Making this an explicit decision (rather than the caller reading
// counters) is what keeps "emit exactly once per down-episode" testable
// without a database.
type probeDecision int

const (
	// decisionNone: nothing to announce — healthy, or a failure still under
	// the threshold, or a repeat failure on an already-announced outage.
	decisionNone probeDecision = iota
	// decisionEmit: this probe crossed the threshold. Emit the unreachable
	// event and the alert line — once for this outage.
	decisionEmit
	// decisionRecovered: an announced-down tunnel answered again.
	decisionRecovered
)

func (d probeDecision) String() string {
	switch d {
	case decisionEmit:
		return "emit"
	case decisionRecovered:
		return "recovered"
	default:
		return "none"
	}
}

// downEpisode is one instance's current run of consecutive failed probes.
type downEpisode struct {
	// fails counts failures since the last success.
	fails int
	// firstFailAt is when this episode's first failed probe was observed —
	// the best available estimate of the outage start, and therefore the
	// timestamp billing is closed back to if the episode escalates and the
	// start of the window credited if it ends any other way.
	firstFailAt time.Time
	// connectOnly stays true while every probe in the episode classified as
	// connect_failed. A single no_ssh_banner result poisons the episode for
	// escalation: frpc answering means the host may be alive, and terminating
	// a live host is the mistake the escalation guards exist to prevent.
	connectOnly bool
	// announced marks that the unreachable alert/event fired for this episode,
	// so repeat failures stay silent and only the first success pages recovery.
	announced bool
	// escalated marks that escalateUnreachable WON the running->error flip for
	// this episode and closed billing backdated to firstFailAt. That close is
	// the fail path's compensation — it refunds every metered cent of the dead
	// window — so this episode must never also produce a ledger credit. The
	// flag is the in-memory half of that guard (the eviction path's
	// status != "terminated" check is the other half, covering a process that
	// restarts between the escalation and the eviction).
	escalated bool
}

// creditableWindow is what the tracker reports about an ended episode that may
// owe the customer money: the outage start and how many failed probes actually
// confirmed it. The caller (credit.go) decides whether that is enough evidence
// and whether the amount is within the guardrails; the tracker never prices
// anything.
type creditableWindow struct {
	firstFailAt time.Time
	fails       int
}

// evictedEpisode is one announced, non-escalated episode that ended because
// the instance left the active set — overwhelmingly, the customer terminated
// it mid-outage (the 2026-08-20 incident shape). There is no recovery probe to
// hang the credit off, so retain reports it instead.
type evictedEpisode struct {
	instanceID string
	window     creditableWindow
}

// reachabilityTracker turns a stream of per-instance probe results into
// transitions. It exists so alerting gets one line when a tunnel dies and one
// when it comes back — not one per monitor tick for the entire outage, which
// for a multi-hour incident would be hundreds of pages for a single fact.
// It also answers escalationDue: which announced outages have been down long
// enough that the monitor should fail the instance (see monitor.go).
//
// Accessed concurrently: the monitor probes instances from bounded-concurrency
// goroutines, so every method takes the mutex.
type reachabilityTracker struct {
	// failThreshold and escalateAfter are the resolved (env-tunable) knobs the
	// decisions below read, rather than the package consts directly, so an
	// incident can retune confirmation and grace without a deploy. Resolved
	// once in the constructor: zero means "use the shipped default".
	failThreshold int
	escalateAfter time.Duration

	mu       sync.Mutex
	episodes map[string]*downEpisode
}

// newReachabilityTracker builds a tracker with the given knobs; a zero or
// negative value for either falls back to the shipped default, so an unset
// environment behaves exactly like the code that predates the knobs.
func newReachabilityTracker(failThreshold int, escalateAfter time.Duration) *reachabilityTracker {
	if failThreshold <= 0 {
		failThreshold = reachabilityFailThreshold
	}
	if escalateAfter <= 0 {
		escalateAfter = unreachableEscalateAfter
	}
	return &reachabilityTracker{
		failThreshold: failThreshold,
		escalateAfter: escalateAfter,
		episodes:      make(map[string]*downEpisode),
	}
}

// recordFailure registers one failed probe and returns whether this is the
// moment the outage becomes announceable.
func (t *reachabilityTracker) recordFailure(instanceID, reason string, now time.Time) probeDecision {
	t.mu.Lock()
	defer t.mu.Unlock()

	ep := t.episodes[instanceID]
	if ep == nil {
		ep = &downEpisode{firstFailAt: now, connectOnly: true}
		t.episodes[instanceID] = ep
	}
	ep.fails++
	ep.connectOnly = ep.connectOnly && reason == ReasonConnectFailed
	if ep.announced {
		return decisionNone // stay quiet for the rest of the outage
	}
	if ep.fails >= t.failThreshold {
		ep.announced = true
		return decisionEmit
	}
	return decisionNone
}

// recordSuccess registers one successful probe and returns whether it ends an
// announced outage, plus the window that outage owes the customer (nil when it
// owes nothing). A success on a tunnel that never crossed the threshold is
// silent — nothing was ever claimed to be down, so there is nothing to undo and
// nothing to refund; that is the majority of production observations.
//
// The window comes back from the SAME locked read that deletes the episode, so
// a caller can never observe a recovery whose window has already been dropped.
func (t *reachabilityTracker) recordSuccess(instanceID string) (probeDecision, *creditableWindow) {
	t.mu.Lock()
	defer t.mu.Unlock()

	ep := t.episodes[instanceID]
	delete(t.episodes, instanceID)
	if ep == nil || !ep.announced {
		return decisionNone, nil
	}
	if ep.escalated {
		// Billing was already closed backdated to firstFailAt; a credit on top
		// would pay the same dead window twice.
		return decisionRecovered, nil
	}
	return decisionRecovered, &creditableWindow{firstFailAt: ep.firstFailAt, fails: ep.fails}
}

// episodeStart reports the current episode's first failed probe — T0, the
// timestamp the durable instances.unreachable_since flag is stamped with at
// announce. Separate from recordFailure's decision so the announce path can
// read it without widening the hot per-probe return.
func (t *reachabilityTracker) episodeStart(instanceID string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	ep := t.episodes[instanceID]
	if ep == nil {
		return time.Time{}, false
	}
	return ep.firstFailAt, true
}

// markEscalated records that escalateUnreachable won the running->error flip
// for this instance, permanently disqualifying its episode from crediting. The
// caller MUST call this immediately after winning the flip and before closing
// billing, so no concurrent probe result can slip a credit in behind it.
//
// A no-op when the episode is already gone: with no episode there is nothing
// left that could report a creditable window.
func (t *reachabilityTracker) markEscalated(instanceID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if ep := t.episodes[instanceID]; ep != nil {
		ep.escalated = true
	}
}

// announcedCount reports how many instances are currently in an ANNOUNCED
// outage — the numerator of the credit circuit-breaker's fraction. Announced,
// not merely failing, because a sub-threshold blip is not evidence of anything
// and must not push the breaker toward tripping.
func (t *reachabilityTracker) announcedCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	n := 0
	for _, ep := range t.episodes {
		if ep.announced {
			n++
		}
	}
	return n
}

// escalationDue reports whether instanceID's current episode qualifies for
// failing the instance: announced, exclusively connect_failed, at least
// unreachableEscalateMinFails observed failures, and down for at least
// unreachableEscalateAfter. It also returns the episode start (the
// billing close-back timestamp).
//
// Deliberately NOT once-only: the caller's optimistic status flip is the
// exactly-once guard, and a candidate suppressed by the storm veto or the
// telemetry veto must come back on the next tick, not vanish.
func (t *reachabilityTracker) escalationDue(instanceID string, now time.Time) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	ep := t.episodes[instanceID]
	if ep == nil || !ep.announced || !ep.connectOnly {
		return time.Time{}, false
	}
	if ep.fails < unreachableEscalateMinFails {
		return time.Time{}, false
	}
	if now.Sub(ep.firstFailAt) < t.escalateAfter {
		return time.Time{}, false
	}
	return ep.firstFailAt, true
}

// retain drops state for instances no longer being probed (terminated, or no
// longer carrying a tunnel) and returns the ANNOUNCED, non-escalated episodes
// that died with them. Without the eviction the map would grow for the lifetime
// of the process, since instance IDs are never reused but are created
// constantly; without the return value the commonest real incident shape —
// customer sees SSH die, terminates the instance himself — would silently drop
// the window he is owed, because there is no recovery probe to notice it.
//
// Never-announced evictions report nothing: a blip owes nothing. Escalated
// evictions report nothing either: escalateUnreachable already closed billing
// backdated to T0.
func (t *reachabilityTracker) retain(live map[string]struct{}) []evictedEpisode {
	t.mu.Lock()
	defer t.mu.Unlock()

	var evicted []evictedEpisode
	for id, ep := range t.episodes {
		if _, ok := live[id]; ok {
			continue
		}
		delete(t.episodes, id)
		if ep.announced && !ep.escalated {
			evicted = append(evicted, evictedEpisode{
				instanceID: id,
				window:     creditableWindow{firstFailAt: ep.firstFailAt, fails: ep.fails},
			})
		}
	}
	return evicted
}

// size reports how many instances the tracker is holding state for.
func (t *reachabilityTracker) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.episodes)
}

// checkReachability probes one instance's SSH tunnel and records the result.
//
// Runs inside checkInstance, i.e. inside the monitor's existing
// maxConcurrentChecks semaphore, so the worst case (both timeouts elapsing) is
// ~10s of one bounded worker against a 60s tick — acceptable, and bounded by
// construction rather than by hope.
//
// Skips silently when there is nothing meaningful to probe: no configured
// gateway host (dev without FRP), a non-running instance (booting instances
// have no tunnel yet and terminating ones are expected to stop answering), or
// no allocated remote port.
func (m *Monitor) checkReachability(ctx context.Context, inst *db.Instance) {
	if m.tunnelProbeHost == "" || inst.Status != "running" || inst.FRPRemotePort == nil {
		return
	}

	port := *inst.FRPRemotePort
	addr := net.JoinHostPort(m.tunnelProbeHost, strconv.Itoa(port))

	m.tick.probed.Add(1)
	if err := probeSSHBanner(addr, probeDialTimeout, probeReadTimeout); err != nil {
		m.tick.failed.Add(1)
		m.handleUnreachable(ctx, inst, port, classifyProbeError(err))
		return
	}

	// Reachable. Stamp the column FIRST so the freshest confirmed-good
	// timestamp is durable even if the recovery log below is lost.
	if err := m.db.UpdateInstanceLastReachable(ctx, inst.InstanceID, time.Now().UTC()); err != nil {
		// Log-and-continue: failing to record a good probe is a visibility
		// gap, not an instance problem, and must not abort the tick.
		m.logger.Warn("tunnel probe: failed to record last_reachable_at",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
	}

	decision, window := m.reachability.recordSuccess(inst.InstanceID)

	// Resolve the owed window BEFORE clearing the flag (unreachable_since is
	// the restart-durable record of what the customer is owed) and QUEUE it
	// for end-of-tick settlement rather than crediting inline: the circuit
	// breaker must judge this recovery against the full tick's probe count,
	// not the partial count of whichever probes happened to finish first —
	// see queueRecoveryCredit. The queued record carries the resolved window,
	// so the clear below no longer races the money.
	//
	// Two ways to arrive here with something to pay:
	//   - the tracker has the episode (the normal case), or
	//   - the tracker does not, but the DB row is flagged — this process
	//     started mid-outage. The flag is only ever written by a CONFIRMED
	//     announce, so the confirmation already happened in the previous
	//     process; credit at exactly the confirmation floor rather than
	//     inventing an evidence count.
	switch {
	case window != nil:
		m.queueRecoveryCredit(inst,
			creditWindowStart(inst, window.firstFailAt), time.Now().UTC(), window.fails)
	case inst.UnreachableSince != nil:
		m.queueRecoveryCredit(inst,
			inst.UnreachableSince.UTC(), time.Now().UTC(), 0)
	}

	// Clear the durable outage flag off the DB ROW, not off the tracker: a
	// gpuctl restart mid-outage loses the in-memory episode but not the column,
	// and keying the clear off the row is what stops that restart from
	// stranding an "unreachable" badge on an instance that is answering again.
	if inst.UnreachableSince != nil {
		if err := m.db.ClearInstanceUnreachableSince(ctx, inst.InstanceID); err != nil {
			// Log-and-continue, matching the last_reachable_at stamp above: a
			// stale badge is a visibility bug, and the next successful probe
			// retries the clear anyway.
			m.logger.Warn("tunnel probe: failed to clear unreachable_since",
				slog.String("instance_id", inst.InstanceID),
				slog.String("error", err.Error()),
			)
		}
	}

	if decision == decisionRecovered {
		// Info, not Error: recovery must not trip the alert filter. Ops reads
		// it as the closing bracket on an outage they were already paged for.
		m.logger.Info("tunnel reachable again",
			slog.String("instance_id", inst.InstanceID),
			slog.String("org_id", inst.OrgID),
			slog.Int("remote_port", port),
		)
	}
}

// handleUnreachable records a failed probe and, on the transition into an
// outage, writes the instance_event and emits the alert line. When the
// episode has been down past unreachableEscalateAfter it also queues the
// instance for escalation — the actual lifecycle write happens after the
// tick's probes finish, in processUnreachableEscalations (monitor.go), so the
// gateway-storm veto can see the whole tick before anything is terminated.
func (m *Monitor) handleUnreachable(ctx context.Context, inst *db.Instance, port int, reason string) {
	now := time.Now().UTC()
	decision := m.reachability.recordFailure(inst.InstanceID, reason, now)

	if downSince, due := m.reachability.escalationDue(inst.InstanceID, now); due {
		m.tick.mu.Lock()
		m.tick.escalations = append(m.tick.escalations, escalationCandidate{inst: inst, downSince: downSince})
		m.tick.mu.Unlock()
	}

	if decision != decisionEmit {
		return // below threshold, or this outage was already announced
	}

	// Persist T0 — the FIRST failed probe of this confirmed run, not the probe
	// that crossed the threshold. Everything downstream keys off it: the
	// customer-facing "unreachable" status, the credited window's start, and
	// the credit's idempotency reference. COALESCE in the writer makes the
	// first announce win, so a monitor restart re-confirming the same dead
	// tunnel cannot move it.
	if t0, ok := m.reachability.episodeStart(inst.InstanceID); ok {
		if err := m.db.SetInstanceUnreachableSince(ctx, inst.InstanceID, t0); err != nil {
			// Log-and-continue: the alert below is the part a human acts on,
			// and the next failed probe of this episode re-announces nothing —
			// but a subsequent confirmed run will retry the stamp.
			m.logger.Error("tunnel probe: failed to record unreachable_since",
				slog.String("instance_id", inst.InstanceID),
				slog.String("error", err.Error()),
			)
		}
	}

	metadata, _ := json.Marshal(map[string]string{
		"reason":      reason,
		"remote_port": strconv.Itoa(port),
		"gpu_type":    inst.GPUType,
		"region":      inst.Region,
		"tier":        inst.Tier,
	})
	event := &db.InstanceEvent{
		InstanceID: inst.InstanceID,
		OrgID:      inst.OrgID,
		EventType:  "unreachable",
		Metadata:   metadata,
	}
	if err := m.db.CreateInstanceEvent(ctx, event); err != nil {
		// Non-fatal, matching the other event-logging paths in this package:
		// losing the audit row must not cost us the alert below, which is the
		// part a human acts on.
		m.logger.Error("tunnel probe: failed to log unreachable event",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
	}

	// Notify SSE subscribers so an open dashboard reflects it without waiting
	// for the next poll.
	if m.onEvent != nil {
		m.onEvent(*event)
	}

	// THE ALERT. Exact message string = wire contract (see the constant).
	m.logger.Error(TunnelUnreachableAlertMessage,
		slog.String("instance_id", inst.InstanceID),
		slog.String("org_id", inst.OrgID),
		slog.Int("remote_port", port),
		slog.String("reason", reason),
	)
}
