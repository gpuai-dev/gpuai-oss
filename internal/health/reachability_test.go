package health

import (
	"errors"
	"net"
	"testing"
	"time"
)

// The probe's whole reason to exist is that "the provider says running" and
// "a customer can actually SSH in" are different questions. These tests pin
// the two real-world failure shapes that motivated GPU-49, emulated with local
// listeners so no DB, no provider, and no network egress is involved:
//
//	frpc dead    -> nothing bound on the gateway's remote port -> dial fails
//	sshd dead    -> frps accepts the TCP connection, then the connection dies
//	                without a banner because frpc's local dial to :22 failed
//
// The second case is why a bare TCP connect check would have shipped a probe
// that reports every broken tunnel as healthy.

const (
	testDialTimeout = 2 * time.Second
	testReadTimeout = 2 * time.Second
)

// probeTime is the fixed "now" the tracker tests stamp failures with; tests
// that exercise the escalation clock pass explicit offsets from it instead.
var probeTime = time.Date(2026, 8, 20, 18, 45, 0, 0, time.UTC)

// failConnectN records n consecutive connect_failed probes for id, one per
// minute starting at start — the shape of a real dead-tunnel episode at the
// monitor's 60s cadence.
func failConnectN(tr *reachabilityTracker, id string, n int, start time.Time) {
	for i := 0; i < n; i++ {
		tr.recordFailure(id, ReasonConnectFailed, start.Add(time.Duration(i)*time.Minute))
	}
}

// listenerWritingBanner emulates a healthy tunnel: accept, write the SSH
// identification string, hold the connection briefly, close.
func listenerWritingBanner(t *testing.T, banner string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte(banner))
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// listenerAcceptingThenClosing emulates frps with a dead sshd behind it: the
// TCP handshake succeeds against frps, then frps tears the connection down
// when frpc's local dial to :22 fails. No bytes are ever written.
func listenerAcceptingThenClosing(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// closedPortAddr returns an address nothing is listening on — a port that was
// bound and immediately released. Emulates a dead frpc: frps never registered
// the proxy, so the remote port is unbound.
func closedPortAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

func TestProbeSSHBannerReachable(t *testing.T) {
	ln := listenerWritingBanner(t, "SSH-2.0-OpenSSH_9.6\r\n")

	if err := probeSSHBanner(ln.Addr().String(), testDialTimeout, testReadTimeout); err != nil {
		t.Fatalf("expected reachable, got error: %v", err)
	}
}

// The frps relay can split the banner across TCP segments, so the prefix may
// not arrive in a single Read. A probe that judged the first Read alone would
// flag this healthy tunnel unreachable — and two such ticks page a human.
func TestProbeSSHBannerSplitAcrossSegmentsIsReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("SS"))
			time.Sleep(50 * time.Millisecond)
			_, _ = conn.Write([]byte("H-2.0-OpenSSH_9.6\r\n"))
			_ = conn.Close()
		}
	}()

	if err := probeSSHBanner(ln.Addr().String(), testDialTimeout, testReadTimeout); err != nil {
		t.Fatalf("expected reachable despite split banner, got error: %v", err)
	}
}

func TestProbeSSHBannerDeadSSHDIsUnreachable(t *testing.T) {
	ln := listenerAcceptingThenClosing(t)

	err := probeSSHBanner(ln.Addr().String(), testDialTimeout, testReadTimeout)
	if err == nil {
		t.Fatal("expected an error when the tunnel accepts but no SSH banner follows " +
			"(a bare TCP-connect probe would wrongly report this healthy)")
	}
	if got := classifyProbeError(err); got != ReasonNoSSHBanner {
		t.Fatalf("classifyProbeError = %q, want %q (err: %v)", got, ReasonNoSSHBanner, err)
	}
}

func TestProbeSSHBannerDeadFrpcIsUnreachable(t *testing.T) {
	addr := closedPortAddr(t)

	err := probeSSHBanner(addr, testDialTimeout, testReadTimeout)
	if err == nil {
		t.Fatal("expected an error dialing an unbound remote port")
	}
	if got := classifyProbeError(err); got != ReasonConnectFailed {
		t.Fatalf("classifyProbeError = %q, want %q (err: %v)", got, ReasonConnectFailed, err)
	}
}

// A listener that answers with something that is not an SSH identification
// string (e.g. an HTTP server accidentally bound on the tunnel port) must not
// count as reachable — the probe validates the protocol, not just liveness.
func TestProbeSSHBannerNonSSHResponseIsUnreachable(t *testing.T) {
	ln := listenerWritingBanner(t, "HTTP/1.1 200 OK\r\n")

	err := probeSSHBanner(ln.Addr().String(), testDialTimeout, testReadTimeout)
	if err == nil {
		t.Fatal("expected an error for a non-SSH banner")
	}
	if got := classifyProbeError(err); got != ReasonNoSSHBanner {
		t.Fatalf("classifyProbeError = %q, want %q (err: %v)", got, ReasonNoSSHBanner, err)
	}
}

// The reason strings ride along on customer-visible instance_events metadata,
// so they must stay a closed set of net-level classifications — never a raw
// dial error (which leaks the gateway host/port) and never a provider name
// (INFPRIV-06). Guard the enum so a future refactor can't widen it silently.
func TestClassifyProbeErrorReturnsFixedEnum(t *testing.T) {
	cases := []error{
		errors.New("dial tcp 203.0.113.9:10042: connect: connection refused"),
		errNoSSHBanner,
		errors.New("something entirely unexpected"),
	}
	for _, err := range cases {
		got := classifyProbeError(err)
		if got != ReasonConnectFailed && got != ReasonNoSSHBanner {
			t.Fatalf("classifyProbeError(%v) = %q, which is outside the allowed enum", err, got)
		}
	}
}

// --- consecutive-failure tracker -------------------------------------------
//
// A tunnel blips for all sorts of benign reasons (frpc reconnect, gateway
// restart, a single dropped SYN). Alerting on the first failed probe would
// page on noise; alerting on every failed probe while an instance stays down
// would flood alerting for the whole life of the outage. The tracker exists to
// make "unreachable" a transition, emitted exactly once per down-episode.

func TestTrackerEmitsOnlyOnSecondConsecutiveFailure(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	if d := tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime); d != decisionNone {
		t.Fatalf("1st failure: got %v, want decisionNone (a single blip must not page)", d)
	}
	if d := tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime); d != decisionEmit {
		t.Fatalf("2nd failure: got %v, want decisionEmit", d)
	}
}

func TestTrackerDoesNotReEmitWhileStillDown(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime)
	if d := tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime); d != decisionEmit {
		t.Fatalf("2nd failure: got %v, want decisionEmit", d)
	}
	for i := 3; i <= 10; i++ {
		if d := tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime); d != decisionNone {
			t.Fatalf("failure #%d: got %v, want decisionNone (one alert per down-episode)", i, d)
		}
	}
}

func TestTrackerSignalsRecoveryThenResets(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime)
	tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime) // down-episode begins

	if d, _ := tr.recordSuccess("gpu-1"); d != decisionRecovered {
		t.Fatalf("success after down: got %v, want decisionRecovered", d)
	}
	// Recovery must reset the counter, so the NEXT episode also needs two
	// consecutive failures before it pages.
	if d := tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime); d != decisionNone {
		t.Fatalf("1st failure of a new episode: got %v, want decisionNone", d)
	}
	if d := tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime); d != decisionEmit {
		t.Fatalf("2nd failure of a new episode: got %v, want decisionEmit", d)
	}
}

func TestTrackerSuccessOnHealthyInstanceIsSilent(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	if d, _ := tr.recordSuccess("gpu-1"); d != decisionNone {
		t.Fatalf("success with no prior failure: got %v, want decisionNone", d)
	}
	tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime) // one blip, below threshold
	if d, _ := tr.recordSuccess("gpu-1"); d != decisionNone {
		t.Fatalf("success after a sub-threshold blip: got %v, want decisionNone "+
			"(nothing was ever announced as down, so there is nothing to recover from)", d)
	}
}

func TestTrackerIsolatesInstances(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime)
	if d := tr.recordFailure("gpu-2", ReasonConnectFailed, probeTime); d != decisionNone {
		t.Fatalf("gpu-2 1st failure: got %v, want decisionNone (counters must not be shared)", d)
	}
	if d := tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime); d != decisionEmit {
		t.Fatalf("gpu-1 2nd failure: got %v, want decisionEmit", d)
	}
}

// Instances are terminated constantly; without eviction the tracker's maps
// would grow for the process's lifetime.
func TestTrackerRetainEvictsInstancesNoLongerProbed(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	tr.recordFailure("gpu-gone", ReasonConnectFailed, probeTime)
	tr.recordFailure("gpu-gone", ReasonConnectFailed, probeTime) // now in the down set
	tr.recordFailure("gpu-live", ReasonConnectFailed, probeTime)

	tr.retain(map[string]struct{}{"gpu-live": {}})

	if n := tr.size(); n != 1 {
		t.Fatalf("tracker size after retain = %d, want 1", n)
	}
	// A recycled id must start from a clean slate, not inherit the evicted
	// instance's down-state.
	if d := tr.recordFailure("gpu-gone", ReasonConnectFailed, probeTime); d != decisionNone {
		t.Fatalf("failure after eviction: got %v, want decisionNone", d)
	}
}

func TestTrackerIsConcurrencySafe(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			id := "gpu-" + string(rune('a'+n))
			for j := 0; j < 200; j++ {
				tr.recordFailure(id, ReasonConnectFailed, probeTime)
				tr.recordSuccess(id)
				tr.retain(map[string]struct{}{id: {}})
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// --- escalation clock -------------------------------------------------------
//
// An announced outage that stays exclusively connect_failed past
// unreachableEscalateAfter qualifies for failing the instance. These tests pin
// the qualification rules; the DB/provider writes themselves live in
// monitor.go behind the optimistic status flip and are guarded there.

func TestEscalationDueOnlyAfterGracePeriod(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	failConnectN(tr, "gpu-1", unreachableEscalateMinFails, probeTime)

	if _, due := tr.escalationDue("gpu-1", probeTime.Add(unreachableEscalateAfter-time.Second)); due {
		t.Fatal("escalation due before the grace period elapsed")
	}
	downSince, due := tr.escalationDue("gpu-1", probeTime.Add(unreachableEscalateAfter))
	if !due {
		t.Fatal("escalation not due after the grace period elapsed")
	}
	if !downSince.Equal(probeTime) {
		t.Fatalf("downSince = %v, want the episode's first failure %v", downSince, probeTime)
	}
}

func TestEscalationRequiresObservedFailures(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	// An announced episode whose wall-clock is ancient but which was only
	// OBSERVED failing twice (e.g. the provider poll was down in between, so
	// no probes ran at all) must not escalate: unobserved time is not
	// evidence the tunnel stayed dead.
	tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime)
	tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime.Add(time.Minute))
	if _, due := tr.escalationDue("gpu-1", probeTime.Add(3*unreachableEscalateAfter)); due {
		t.Fatal("escalation due with only 2 observed failures")
	}

	// The same episode escalates once enough failures are actually observed.
	failConnectN(tr, "gpu-1", unreachableEscalateMinFails-2, probeTime.Add(2*time.Minute))
	if _, due := tr.escalationDue("gpu-1", probeTime.Add(3*unreachableEscalateAfter)); !due {
		t.Fatalf("escalation not due after %d observed failures past the grace period", unreachableEscalateMinFails)
	}
}

func TestEscalationRequiresAnnouncedOutage(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	// One failure is below the announce threshold — however old it gets, a
	// sub-threshold blip must never fail an instance.
	tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime)
	if _, due := tr.escalationDue("gpu-1", probeTime.Add(2*unreachableEscalateAfter)); due {
		t.Fatal("escalation due for an outage that was never announced")
	}
}

func TestNoSSHBannerPoisonsEscalation(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	// frps accepting the connection means frpc — and so probably the host —
	// is alive with only sshd dead; that episode must alert but never
	// auto-terminate, even if later probes classify connect_failed. Record
	// enough failures that ONLY the poison stands between it and escalating.
	tr.recordFailure("gpu-1", ReasonNoSSHBanner, probeTime)
	failConnectN(tr, "gpu-1", unreachableEscalateMinFails, probeTime.Add(time.Minute))

	if _, due := tr.escalationDue("gpu-1", probeTime.Add(2*unreachableEscalateAfter)); due {
		t.Fatal("escalation due for an episode containing a no_ssh_banner probe")
	}
}

func TestRecoveryClearsEscalation(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	failConnectN(tr, "gpu-1", unreachableEscalateMinFails, probeTime)
	tr.recordSuccess("gpu-1")

	if _, due := tr.escalationDue("gpu-1", probeTime.Add(2*unreachableEscalateAfter)); due {
		t.Fatal("escalation due after the tunnel recovered")
	}

	// A new episode's clock starts from ITS first failure, not the old one's.
	restart := probeTime.Add(time.Hour)
	failConnectN(tr, "gpu-1", unreachableEscalateMinFails, restart)
	downSince, due := tr.escalationDue("gpu-1", restart.Add(unreachableEscalateAfter))
	if !due {
		t.Fatal("escalation not due for the new episode after its own grace period")
	}
	if !downSince.Equal(restart) {
		t.Fatalf("downSince = %v, want the new episode's start %v", downSince, restart)
	}
}

func TestEscalationDueIsRepeatable(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	// A candidate deferred by the storm or telemetry veto must come back on
	// the next tick — the exactly-once guard is the status flip, not this.
	failConnectN(tr, "gpu-1", unreachableEscalateMinFails, probeTime)

	at := probeTime.Add(unreachableEscalateAfter)
	for i := 0; i < 3; i++ {
		if _, due := tr.escalationDue("gpu-1", at.Add(time.Duration(i)*time.Minute)); !due {
			t.Fatalf("escalationDue call #%d = false, want repeatably true", i+1)
		}
	}
}

// The alert line is a wire contract: log-based alerting matches this exact
// string, and that match is the only thing that turns a dead tunnel into a page.
func TestTunnelUnreachableAlertMessageIsStable(t *testing.T) {
	if TunnelUnreachableAlertMessage != "tunnel unreachable" {
		t.Fatalf("TunnelUnreachableAlertMessage = %q; changing it silently breaks the "+
			"log-based alert that matches it",
			TunnelUnreachableAlertMessage)
	}
}

func TestTunnelStormAlertMessageIsStable(t *testing.T) {
	if TunnelStormAlertMessage != "tunnel unreachable storm: escalation suppressed" {
		t.Fatalf("TunnelStormAlertMessage = %q; changing it silently breaks the "+
			"log-based alert that matches it",
			TunnelStormAlertMessage)
	}
}

// --- creditable windows, the escalation guard, and the knobs (GPU-77) --------
//
// The tracker is now the source of truth for "does this ended outage owe the
// customer money for the dead window?". Three shapes end an episode and only
// two of them are creditable:
//
//	recovery of an ANNOUNCED episode  -> creditable (T0 -> now)
//	eviction of an ANNOUNCED episode  -> creditable (the instance left the
//	                                    active set: terminated mid-outage)
//	anything ESCALATED                -> NEVER creditable; escalateUnreachable
//	                                    already closed billing backdated to T0,
//	                                    so a ledger credit on top would pay the
//	                                    same dead window twice.
//
// A never-announced episode is a blip: nothing was claimed to be down, so
// nothing is owed. That is the majority of production observations (58
// unreachable events in 10 days, most of them a single probe).

func TestRecordSuccessReportsCreditableWindow(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	failConnectN(tr, "gpu-1", 3, probeTime)

	decision, window := tr.recordSuccess("gpu-1")
	if decision != decisionRecovered {
		t.Fatalf("decision = %v, want decisionRecovered", decision)
	}
	if window == nil {
		t.Fatal("recovery of an announced episode returned no creditable window")
	}
	if !window.firstFailAt.Equal(probeTime) {
		t.Errorf("window.firstFailAt = %v, want the episode's first failed probe %v", window.firstFailAt, probeTime)
	}
	if window.fails != 3 {
		t.Errorf("window.fails = %d, want 3 (the confirmation strength the credit guard reads)", window.fails)
	}
}

func TestRecordSuccessOnBlipIsNotCreditable(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	// One failure — below the announce threshold.
	tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime)

	if decision, window := tr.recordSuccess("gpu-1"); window != nil {
		t.Fatalf("sub-threshold blip returned a creditable window (%+v, decision %v)", window, decision)
	}
	if _, window := tr.recordSuccess("gpu-never-failed"); window != nil {
		t.Fatalf("success with no episode at all returned a creditable window %+v", window)
	}
}

func TestRetainReturnsEvictedAnnouncedEpisodes(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	failConnectN(tr, "gpu-terminated", 3, probeTime) // announced, then dropped out
	tr.recordFailure("gpu-blip", ReasonConnectFailed, probeTime)
	failConnectN(tr, "gpu-live", 3, probeTime)

	evicted := tr.retain(map[string]struct{}{"gpu-live": {}})

	if len(evicted) != 1 {
		t.Fatalf("evicted = %+v, want exactly the announced gpu-terminated episode", evicted)
	}
	if evicted[0].instanceID != "gpu-terminated" {
		t.Fatalf("evicted[0].instanceID = %q, want gpu-terminated (a never-announced blip owes nothing)", evicted[0].instanceID)
	}
	if !evicted[0].window.firstFailAt.Equal(probeTime) {
		t.Errorf("evicted window firstFailAt = %v, want %v", evicted[0].window.firstFailAt, probeTime)
	}
	if evicted[0].window.fails != 3 {
		t.Errorf("evicted window fails = %d, want 3", evicted[0].window.fails)
	}
	if n := tr.size(); n != 1 {
		t.Fatalf("tracker size after retain = %d, want 1 (eviction must still drop state)", n)
	}
}

func TestMarkEscalatedSuppressesCreditOnEveryExit(t *testing.T) {
	// Escalation closes billing BACKDATED to T0 (escalateUnreachable), which is
	// the fail path's compensation. A credit racing in behind it — a late
	// successful probe, or the eviction that follows the running->error flip —
	// would pay the same dead window a second time.
	t.Run("recovery", func(t *testing.T) {
		tr := newReachabilityTracker(0, 0)
		failConnectN(tr, "gpu-1", unreachableEscalateMinFails+1, probeTime)
		tr.markEscalated("gpu-1")

		if _, window := tr.recordSuccess("gpu-1"); window != nil {
			t.Fatalf("escalated episode returned a creditable window on recovery: %+v", window)
		}
	})

	t.Run("eviction", func(t *testing.T) {
		tr := newReachabilityTracker(0, 0)
		failConnectN(tr, "gpu-1", unreachableEscalateMinFails+1, probeTime)
		tr.markEscalated("gpu-1")

		if evicted := tr.retain(map[string]struct{}{}); len(evicted) != 0 {
			t.Fatalf("escalated episode reported as creditable on eviction: %+v", evicted)
		}
	})
}

func TestAnnouncedCountIsTheBreakerNumerator(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	tr.recordFailure("gpu-blip", ReasonConnectFailed, probeTime) // below threshold
	failConnectN(tr, "gpu-1", 2, probeTime)
	failConnectN(tr, "gpu-2", 2, probeTime)

	if n := tr.announcedCount(); n != 2 {
		t.Fatalf("announcedCount = %d, want 2 (only ANNOUNCED episodes count toward the breaker)", n)
	}

	tr.recordSuccess("gpu-1")
	if n := tr.announcedCount(); n != 1 {
		t.Fatalf("announcedCount after one recovery = %d, want 1", n)
	}
}

func TestEpisodeStartReportsT0(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	if _, ok := tr.episodeStart("gpu-1"); ok {
		t.Fatal("episodeStart reported a start for an instance with no episode")
	}
	failConnectN(tr, "gpu-1", 2, probeTime)
	start, ok := tr.episodeStart("gpu-1")
	if !ok {
		t.Fatal("episodeStart reported no start for a live episode")
	}
	if !start.Equal(probeTime) {
		t.Fatalf("episodeStart = %v, want the episode's first failed probe %v", start, probeTime)
	}
}

// The confirmation threshold and the escalation grace are env-tunable (GPU-77):
// production defaults are derived from measured data, but an incident must be
// able to move them without a code change.

func TestTrackerHonorsConfiguredThresholdAndGrace(t *testing.T) {
	tr := newReachabilityTracker(4, 30*time.Minute)

	for i := 1; i <= 3; i++ {
		if d := tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime); d != decisionNone {
			t.Fatalf("failure #%d with threshold 4: got %v, want decisionNone", i, d)
		}
	}
	if d := tr.recordFailure("gpu-1", ReasonConnectFailed, probeTime); d != decisionEmit {
		t.Fatalf("failure #4 with threshold 4: got %v, want decisionEmit", d)
	}

	failConnectN(tr, "gpu-1", unreachableEscalateMinFails, probeTime)
	if _, due := tr.escalationDue("gpu-1", probeTime.Add(29*time.Minute)); due {
		t.Fatal("escalation due before the CONFIGURED 30m grace elapsed")
	}
	if _, due := tr.escalationDue("gpu-1", probeTime.Add(30*time.Minute)); !due {
		t.Fatal("escalation not due after the configured 30m grace elapsed")
	}
}

func TestTrackerZeroKnobsFallBackToShippedDefaults(t *testing.T) {
	tr := newReachabilityTracker(0, 0)

	if tr.failThreshold != reachabilityFailThreshold {
		t.Errorf("failThreshold = %d, want the shipped default %d", tr.failThreshold, reachabilityFailThreshold)
	}
	if tr.escalateAfter != unreachableEscalateAfter {
		t.Errorf("escalateAfter = %v, want the shipped default %v", tr.escalateAfter, unreachableEscalateAfter)
	}
}
