package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// dueInstance builds a running instance whose per-instance runtime bound
// elapsed an hour ago. UpstreamProvider defaults to "ghost" (absent from the
// registry) so Terminate takes its local path — the DB flip, the billing
// closeout and the events, without the ~2s upstream verify poll. The one test
// that cares about the upstream call registers a real stub instead.
func dueInstance(id string, hours int) db.Instance {
	past := time.Now().UTC().Add(-time.Hour)
	return db.Instance{
		InstanceID:         id,
		OrgID:              "org-1",
		Status:             StateRunning,
		UpstreamProvider:   "ghost",
		UpstreamID:         "up-" + id,
		Region:             "us-west",
		GPUType:            "H100_SXM",
		Tier:               "on_demand",
		AutoTerminateHours: &hours,
		AutoTerminateAt:    &past,
	}
}

// TestSweepAutoTerminate_TerminatesDueInstance is the GPU-60 core guard: an
// instance past its per-instance runtime bound is claimed, explained, audited
// and terminated through Engine.Terminate — the SAME path a user DELETE takes,
// which is what makes the billing closeout and the terminated event fire.
//
// The ordering assertion is the point: the reason is recorded BEFORE the
// teardown (so the row explains itself even if the terminate fails halfway),
// and the 'auto_terminated' marker is written AFTER the outcome is known, so a
// tick that re-arms and retries cannot leave a duplicate audit row.
func TestSweepAutoTerminate_TerminatesDueInstance(t *testing.T) {
	const instanceID = "gpu-due0001"

	inst := dueInstance(instanceID, 6)
	var order []string
	var events []*db.InstanceEvent
	fake := &fakeEngineDB{
		listInstancesDueForAutoTerminate: func(_ time.Time, _ int) ([]db.Instance, error) {
			return []db.Instance{inst}, nil
		},
		getInstance: func(string) (*db.Instance, error) {
			row := inst
			return &row, nil
		},
		updateInstanceStatus: func(_, _, _ string) (bool, error) { return true, nil },
		terminateInstance: func(string) (bool, error) {
			order = append(order, "terminate_db")
			return true, nil
		},
		createInstanceEvent: func(ev *db.InstanceEvent) error {
			order = append(order, "event:"+ev.EventType)
			events = append(events, ev)
			return nil
		},
	}
	e := &Engine{
		registry: provider.NewRegistry(),
		db:       fake,
		config:   &config.Config{},
		logger:   slog.Default(),
	}

	if err := e.SweepAutoTerminate(context.Background()); err != nil {
		t.Fatalf("SweepAutoTerminate: %v", err)
	}

	// Claimed exactly once, before anything else happened.
	if len(fake.autoTerminateClaims) != 1 || fake.autoTerminateClaims[0] != instanceID {
		t.Fatalf("claims = %v, want exactly [%s]", fake.autoTerminateClaims, instanceID)
	}
	// The shared 'terminated' event (written inside Terminate) lands with the
	// teardown; the 'auto_terminated' audit marker follows once the outcome
	// is known.
	want := []string{"terminate_db", "event:terminated", "event:auto_terminated"}
	if len(order) != len(want) {
		t.Fatalf("call order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("call order = %v, want %v", order, want)
		}
	}

	// The customer-facing reason is recorded on the row (same helper the
	// spend-cap stops use) and names the customer's own request, nothing else.
	reason := fake.statusReasons[instanceID]
	if reason == "" {
		t.Fatal("no status reason recorded for the auto-terminated instance")
	}
	if want := autoTerminateReason(6); reason != want {
		t.Errorf("status reason = %q, want %q", reason, want)
	}

	// The 'auto_terminated' metadata carries the customer's own request and no
	// upstream identity (INFPRIV) — it is read through the org-scoped event API.
	var audit *db.InstanceEvent
	for _, ev := range events {
		if ev.EventType == "auto_terminated" {
			if audit != nil {
				t.Fatal("auto_terminated event written more than once")
			}
			audit = ev
		}
	}
	if audit == nil {
		t.Fatal("no auto_terminated event written")
	}
	var meta map[string]any
	if err := json.Unmarshal(audit.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal auto_terminated metadata: %v", err)
	}
	if got, ok := meta["auto_terminate_hours"].(float64); !ok || int(got) != 6 {
		t.Errorf("metadata auto_terminate_hours = %v, want 6", meta["auto_terminate_hours"])
	}
	for _, forbidden := range []string{"upstream_provider", "upstream_id", "upstream_price_per_hour"} {
		if _, present := meta[forbidden]; present {
			t.Errorf("auto_terminated metadata leaks %q to a customer-visible event", forbidden)
		}
	}
	if audit.OrgID != "org-1" {
		t.Errorf("auto_terminated event org = %q, want org-1", audit.OrgID)
	}
}

// TestSweepAutoTerminate_LostClaimTerminatesNothing pins the exactly-once gate:
// when the atomic claim is lost — a concurrent sweep, a user terminate, or a
// spend-cap stop got there first — the sweep must not terminate, must not close
// billing and must not write an audit row. This is the property that makes
// running the sweep every 60s safe.
func TestSweepAutoTerminate_LostClaimTerminatesNothing(t *testing.T) {
	inst := dueInstance("gpu-lost0001", 2)
	fake := &fakeEngineDB{
		listInstancesDueForAutoTerminate: func(_ time.Time, _ int) ([]db.Instance, error) {
			return []db.Instance{inst}, nil
		},
		claimInstanceAutoTerminate: func(string) (bool, error) { return false, nil },
		getInstance: func(string) (*db.Instance, error) {
			t.Fatal("must not read the instance after losing the claim")
			return nil, nil
		},
		terminateInstance: func(string) (bool, error) {
			t.Fatal("must not terminate after losing the claim")
			return false, nil
		},
		createInstanceEvent: func(*db.InstanceEvent) error {
			t.Fatal("must not write an audit row after losing the claim")
			return nil
		},
	}
	e := &Engine{registry: provider.NewRegistry(), db: fake, config: &config.Config{}, logger: slog.Default()}

	if err := e.SweepAutoTerminate(context.Background()); err != nil {
		t.Fatalf("SweepAutoTerminate: %v", err)
	}
	if len(fake.autoTerminateClaims) != 1 {
		t.Fatalf("claim attempts = %d, want 1", len(fake.autoTerminateClaims))
	}
	if len(fake.statusReasons) != 0 {
		t.Errorf("status reason written after a lost claim: %v", fake.statusReasons)
	}
}

// TestSweepAutoTerminate_NotDueUntouched proves the sweep only ever acts on the
// due set: an instance whose deadline is in the future is never selected, so it
// is never claimed and never terminated. The fake reproduces the DB's
// due-predicate (auto_terminate_at <= now) so the whole path is exercised
// rather than stubbed away.
func TestSweepAutoTerminate_NotDueUntouched(t *testing.T) {
	future := time.Now().UTC().Add(3 * time.Hour)
	hours := 4
	notDue := db.Instance{
		InstanceID:         "gpu-notdue01",
		OrgID:              "org-1",
		Status:             StateRunning,
		AutoTerminateHours: &hours,
		AutoTerminateAt:    &future,
	}
	unbounded := db.Instance{InstanceID: "gpu-nobound1", OrgID: "org-1", Status: StateRunning}

	var cutoff time.Time
	fake := &fakeEngineDB{
		listInstancesDueForAutoTerminate: func(now time.Time, _ int) ([]db.Instance, error) {
			cutoff = now
			var out []db.Instance
			for _, inst := range []db.Instance{notDue, unbounded} {
				if inst.AutoTerminateAt != nil && !inst.AutoTerminateAt.After(now) {
					out = append(out, inst)
				}
			}
			return out, nil
		},
		terminateInstance: func(string) (bool, error) {
			t.Fatal("terminated an instance that is not past its runtime bound")
			return false, nil
		},
	}
	e := &Engine{registry: provider.NewRegistry(), db: fake, config: &config.Config{}, logger: slog.Default()}

	if err := e.SweepAutoTerminate(context.Background()); err != nil {
		t.Fatalf("SweepAutoTerminate: %v", err)
	}
	if len(fake.autoTerminateClaims) != 0 {
		t.Fatalf("claimed %v; nothing was due", fake.autoTerminateClaims)
	}
	if cutoff.IsZero() {
		t.Fatal("sweep never queried the due set")
	}
}

// TestSweepAutoTerminate_TerminatesUpstream proves the sweep reaches the
// provider: a due instance whose provider IS in the registry has its upstream
// terminated with its own (upstream_id, region), not just its DB row flipped.
// Separated from the core test because Engine.Terminate's post-terminate verify
// poll costs a couple of seconds.
func TestSweepAutoTerminate_TerminatesUpstream(t *testing.T) {
	const instanceID = "gpu-up000001"
	inst := dueInstance(instanceID, 1)
	inst.UpstreamProvider = "stub"

	stub := &stubProvider{
		name:         "stub",
		statusResult: &provider.InstanceStatus{Status: "terminated"}, // verify sees it gone
	}
	reg := provider.NewRegistry()
	reg.Register(stub)

	fake := &fakeEngineDB{
		listInstancesDueForAutoTerminate: func(_ time.Time, _ int) ([]db.Instance, error) {
			return []db.Instance{inst}, nil
		},
		getInstance: func(string) (*db.Instance, error) {
			row := inst
			return &row, nil
		},
		updateInstanceStatus: func(_, _, _ string) (bool, error) { return true, nil },
		terminateInstance:    func(string) (bool, error) { return true, nil },
	}
	e := &Engine{registry: reg, db: fake, config: &config.Config{}, logger: slog.Default()}

	if err := e.SweepAutoTerminate(context.Background()); err != nil {
		t.Fatalf("SweepAutoTerminate: %v", err)
	}
	if len(stub.terminateCalls) != 1 {
		t.Fatalf("upstream Terminate calls = %d, want 1: %+v", len(stub.terminateCalls), stub.terminateCalls)
	}
	if got := stub.terminateCalls[0]; got.UpstreamID != "up-"+instanceID || got.Region != "us-west" {
		t.Fatalf("upstream Terminate target = %+v, want up-%s/us-west", got, instanceID)
	}
}

// TestAutoTerminateReason_IsCustomerSafe pins the wording contract: the reason
// stored on the row (and surfaced as status_reason on the public API) states
// the customer's own limit and never names an upstream provider (INFPRIV) —
// this string is read by customers, not machines.
func TestAutoTerminateReason_IsCustomerSafe(t *testing.T) {
	if got := autoTerminateReason(12); !strings.Contains(got, "12-hour runtime limit") {
		t.Errorf("reason = %q, want it to name the customer's 12-hour limit", got)
	}
	got := strings.ToLower(autoTerminateReason(3))
	for _, name := range []string{"podco", "marketco", "vmco", "fixedimageco", "metalco", "baremetalco", "startupvmco"} {
		if strings.Contains(got, name) {
			t.Errorf("reason names upstream provider %q: %s", name, got)
		}
	}
}

// TestSweepAutoTerminate_FailedTerminateRearmsAndRetries covers the window the
// claim opens: it is spent BEFORE any durable outcome, so a Terminate that
// fails early (a DB blip, a status flip lost to a concurrent writer, a context
// cancelled by a rollout — the ticker has no per-tick timeout) would otherwise
// leave the row running, unbounded and never re-selected. The platform would be
// quietly keeping alive the exact instance the customer asked to have stopped.
//
// The retry must restore the ORIGINAL deadline, not now+hours: the bound has
// already elapsed, and pushing it forward extends a run nobody asked to extend.
func TestSweepAutoTerminate_FailedTerminateRearmsAndRetries(t *testing.T) {
	const instanceID = "gpu-rearm001"
	inst := dueInstance(instanceID, 4)
	deadline := *inst.AutoTerminateAt

	// Tick 1: Terminate fails at its very first step (GetInstance), before it
	// touches the row — the row is still running when the claim is already gone.
	terminateAttempts := 0
	autoTerminatedEvents := 0
	countAutoTerminated := func(ev *db.InstanceEvent) error {
		if ev.EventType == "auto_terminated" {
			autoTerminatedEvents++
		}
		return nil
	}
	fake := &fakeEngineDB{
		listInstancesDueForAutoTerminate: func(_ time.Time, _ int) ([]db.Instance, error) {
			return []db.Instance{inst}, nil
		},
		getInstance: func(string) (*db.Instance, error) {
			terminateAttempts++
			return nil, errTest("connection reset")
		},
		createInstanceEvent: countAutoTerminated,
	}
	e := &Engine{registry: provider.NewRegistry(), db: fake, config: &config.Config{}, logger: slog.Default()}

	if err := e.SweepAutoTerminate(context.Background()); err != nil {
		t.Fatalf("SweepAutoTerminate: %v", err)
	}
	if terminateAttempts != 1 {
		t.Fatalf("terminate attempts = %d, want 1", terminateAttempts)
	}
	if len(fake.autoTerminateRearms) != 1 {
		t.Fatalf("re-arms = %d, want 1 — a failed terminate must put the deadline back, "+
			"or the instance runs past its limit forever", len(fake.autoTerminateRearms))
	}
	if got := fake.autoTerminateRearms[0]; got.instanceID != instanceID || !got.at.Equal(deadline) {
		t.Errorf("re-armed %s at %v, want %s at the original deadline %v",
			got.instanceID, got.at, instanceID, deadline)
	}
	if autoTerminatedEvents != 0 {
		t.Errorf("a re-armed tick wrote %d auto_terminated event(s); the retry tick owns the audit row",
			autoTerminatedEvents)
	}

	// Tick 2: the row is due again (the re-arm restored it) and the terminate
	// now succeeds — the deadline was deferred, not dropped.
	terminated := false
	fake2 := &fakeEngineDB{
		listInstancesDueForAutoTerminate: func(_ time.Time, _ int) ([]db.Instance, error) {
			return []db.Instance{inst}, nil
		},
		getInstance: func(string) (*db.Instance, error) {
			row := inst
			return &row, nil
		},
		updateInstanceStatus: func(_, _, _ string) (bool, error) { return true, nil },
		terminateInstance: func(string) (bool, error) {
			terminated = true
			return true, nil
		},
		createInstanceEvent: countAutoTerminated,
	}
	e2 := &Engine{registry: provider.NewRegistry(), db: fake2, config: &config.Config{}, logger: slog.Default()}
	if err := e2.SweepAutoTerminate(context.Background()); err != nil {
		t.Fatalf("second SweepAutoTerminate: %v", err)
	}
	if !terminated {
		t.Fatal("the retry tick did not terminate the re-armed instance")
	}
	if len(fake2.autoTerminateRearms) != 0 {
		t.Errorf("a successful terminate must not re-arm anything, got %d", len(fake2.autoTerminateRearms))
	}
	if autoTerminatedEvents != 1 {
		t.Errorf("auto_terminated events across both ticks = %d, want exactly 1", autoTerminatedEvents)
	}
}

// TestSweepAutoTerminate_RespectsPerTickCap proves the sweep asks the DB for a
// bounded batch. The sweep runs inside the billing tick ahead of balance
// deduction and each terminate costs seconds of upstream verify, so an
// unbounded due set (a post-outage backlog, or a fleet launched on one shared
// limit) would push every org's deduction late. The remainder is the next
// tick's problem — 60s is nothing against a limit measured in hours.
func TestSweepAutoTerminate_RespectsPerTickCap(t *testing.T) {
	// 30 due instances behind a fake that honors LIMIT the way the real query
	// does (oldest deadline first — already the order they are built in).
	all := make([]db.Instance, 0, 30)
	for i := 0; i < 30; i++ {
		inst := dueInstance(fmt.Sprintf("gpu-cap%05d", i), 1)
		at := inst.AutoTerminateAt.Add(time.Duration(i) * time.Second)
		inst.AutoTerminateAt = &at
		all = append(all, inst)
	}
	remaining := all
	var limitSeen int

	fake := &fakeEngineDB{
		listInstancesDueForAutoTerminate: func(_ time.Time, limit int) ([]db.Instance, error) {
			limitSeen = limit
			if limit > 0 && len(remaining) > limit {
				return remaining[:limit], nil
			}
			return remaining, nil
		},
		getInstance: func(id string) (*db.Instance, error) {
			return &db.Instance{InstanceID: id, OrgID: "org-1", Status: StateRunning, UpstreamProvider: "ghost"}, nil
		},
		updateInstanceStatus: func(_, _, _ string) (bool, error) { return true, nil },
		terminateInstance:    func(string) (bool, error) { return true, nil },
	}
	e := &Engine{registry: provider.NewRegistry(), db: fake, config: &config.Config{}, logger: slog.Default()}

	// Tick 1 takes the cap.
	if err := e.SweepAutoTerminate(context.Background()); err != nil {
		t.Fatalf("SweepAutoTerminate tick 1: %v", err)
	}
	if limitSeen != autoTerminateSweepLimit {
		t.Fatalf("sweep asked for limit %d, want %d — an unbounded read would let one tick "+
			"delay every org's balance deduction", limitSeen, autoTerminateSweepLimit)
	}
	if len(fake.autoTerminateClaims) != autoTerminateSweepLimit {
		t.Fatalf("tick 1 claimed %d instances, want %d", len(fake.autoTerminateClaims), autoTerminateSweepLimit)
	}

	// Tick 2 picks up the remainder.
	remaining = all[autoTerminateSweepLimit:]
	if err := e.SweepAutoTerminate(context.Background()); err != nil {
		t.Fatalf("SweepAutoTerminate tick 2: %v", err)
	}
	if got := len(fake.autoTerminateClaims); got != len(all) {
		t.Errorf("claims across both ticks = %d, want all %d instances swept", got, len(all))
	}
}
