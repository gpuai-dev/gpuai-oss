package provision

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/tunnel"
)

// rescueCapableStub is a stubProvider that additionally declares
// provider.BootRescueCapability, standing in for a marketplace adapter that reports boot progress in
// watchdog/rescue tests. Wrapped (not added to stubProvider) so every other
// stub-driven test keeps a non-capable provider and byte-identical behavior.
type rescueCapableStub struct{ *stubProvider }

func (s *rescueCapableStub) SupportsBootRescue() bool { return true }

// --- detector ---

// TestBootStallDetector_TunnelStall — Mode A of the 2026-08-04
// characterization: the container reaches "running" in seconds but the
// bootstrap never phones home. The verdict must fire only after the full
// tunnel-stall window measured from the FIRST running sighting.
func TestBootStallDetector_TunnelStall(t *testing.T) {
	t0 := time.Now()
	d := newBootStallDetector(8*time.Minute, 5*time.Minute, t0)

	running := &provider.InstanceStatus{Status: "running", UpstreamHostID: "39206"}
	if v := d.observe(t0.Add(46*time.Second), running); v != "" {
		t.Fatalf("no verdict on first running sighting, got %q", v)
	}
	if v := d.observe(t0.Add(7*time.Minute), running); v != "" {
		t.Fatalf("no verdict inside the tunnel-stall window, got %q", v)
	}
	if v := d.observe(t0.Add(46*time.Second+8*time.Minute), running); v != verdictTunnelStall {
		t.Fatalf("want %s after 8m of upstream-running with no callback, got %q", verdictTunnelStall, v)
	}
	if d.lastHostID != "39206" {
		t.Errorf("detector must remember the stalled host id, got %q", d.lastHostID)
	}
}

// TestBootStallDetector_DeadPull — Mode B: the upstream sits pre-running with
// literally unchanged progress output for the whole pull-stall window.
func TestBootStallDetector_DeadPull(t *testing.T) {
	t0 := time.Now()
	d := newBootStallDetector(8*time.Minute, 5*time.Minute, t0)

	stuck := &provider.InstanceStatus{Status: "creating", BootDetail: "Extracting 1.2GB/4.7GB", UpstreamHostID: "8197"}
	// First observation CHANGES the detail (empty -> text), resetting the clock.
	if v := d.observe(t0.Add(30*time.Second), stuck); v != "" {
		t.Fatalf("no verdict while detail is fresh, got %q", v)
	}
	if v := d.observe(t0.Add(3*time.Minute), stuck); v != "" {
		t.Fatalf("no verdict inside the pull-stall window, got %q", v)
	}
	if v := d.observe(t0.Add(30*time.Second+5*time.Minute), stuck); v != verdictDeadPull {
		t.Fatalf("want %s after 5m of unchanged pull output, got %q", verdictDeadPull, v)
	}
}

// TestBootStallDetector_ProgressingPullNotKilled pins the boundary case that
// shaped the whole policy: a Hong Kong host pulled for 12+ minutes with
// continuous progress and SUCCEEDED at 12:57 — a progressing pull must ride
// out the full provisioning budget, never a stall verdict.
func TestBootStallDetector_ProgressingPullNotKilled(t *testing.T) {
	t0 := time.Now()
	d := newBootStallDetector(8*time.Minute, 5*time.Minute, t0)

	// 13 minutes of polls, detail advancing every 45s (as observed live).
	for i := 0; i < 18; i++ {
		now := t0.Add(time.Duration(i) * 45 * time.Second)
		st := &provider.InstanceStatus{
			Status:     "creating",
			BootDetail: strings.Repeat("layer\n", i) + "progress",
		}
		if v := d.observe(now, st); v != "" {
			t.Fatalf("a progressing pull must never be judged stalled, got %q at t+%s", v, now.Sub(t0))
		}
	}
}

// TestBootStallDetector_SilentUnpackNotKilled replays the shape of a healthy
// marketplace boot measured on 2026-10-07 (RTX 4090, Vietnam): the last layer reports
// "Pull complete" at 37 s, the status message then stays the same for 329 s
// while docker unpacks the 6.3 GB certified image, and the container runs at
// 579 s. A 5-minute window killed it at 342 s; the default must not (the
// longest healthy silence in that sample of 40 boots was 417 s). A host whose
// message stays frozen past the default is still judged stalled.
func TestBootStallDetector_SilentUnpackNotKilled(t *testing.T) {
	t0 := time.Now()
	d := newBootStallDetector(8*time.Minute, config.DefaultProvisionPullStallAfter, t0)
	unpacking := &provider.InstanceStatus{Status: "creating", BootDetail: "ebe8b59b2e79: Pull complete", UpstreamHostID: "51178"}
	for s := 37; s <= 37+329; s += 5 {
		if v := d.observe(t0.Add(time.Duration(s)*time.Second), unpacking); v != "" {
			t.Fatalf("healthy unpack judged %q at t+%ds", v, s)
		}
	}
	building := &provider.InstanceStatus{Status: "creating", BootDetail: "#7 116.9 Setting up libxml2:amd64 ...", UpstreamHostID: "51178"}
	if v := d.observe(t0.Add(370*time.Second), building); v != "" {
		t.Fatalf("progress after the unpack judged %q", v)
	}

	frozen := newBootStallDetector(8*time.Minute, config.DefaultProvisionPullStallAfter, t0)
	stuck := &provider.InstanceStatus{Status: "creating", BootDetail: "sha256:661e: Pulling from gpuai-dev/pytorch-cuda128", UpstreamHostID: "116630"}
	frozen.observe(t0.Add(48*time.Second), stuck)
	if v := frozen.observe(t0.Add(48*time.Second+config.DefaultProvisionPullStallAfter), stuck); v != verdictDeadPull {
		t.Fatalf("a frozen pull must still be judged stalled at the default window, got %q", v)
	}
}

// TestBootStallDetector_DisabledLegs — a zero threshold disables that leg
// entirely (the config contract for the env knobs).
func TestBootStallDetector_DisabledLegs(t *testing.T) {
	t0 := time.Now()
	d := newBootStallDetector(0, 0, t0)
	if v := d.observe(t0.Add(24*time.Hour), &provider.InstanceStatus{Status: "running"}); v != "" {
		t.Errorf("tunnel leg disabled, got verdict %q", v)
	}
	if v := d.observe(t0.Add(48*time.Hour), &provider.InstanceStatus{Status: "creating"}); v != "" {
		t.Errorf("pull leg disabled, got verdict %q", v)
	}
}

// --- gating ---

func TestBootStallDetectorFor_Gating(t *testing.T) {
	e := &Engine{logger: slog.Default(), tunnelStallAfter: 8 * time.Minute, pullStallAfter: 5 * time.Minute}
	capable := &rescueCapableStub{&stubProvider{name: "capable"}}
	plain := &stubProvider{name: "plain"}
	raw := &db.Instance{InstanceID: "i"}
	tmplID := "tmpl-1"
	rescue := &bootRescue{attemptsLeft: 1}

	if e.bootStallDetectorFor(capable, raw, rescue) == nil {
		t.Error("capable provider + raw instance + armed rescue must get a detector")
	}
	if e.bootStallDetectorFor(plain, raw, rescue) != nil {
		t.Error("non-capable provider must never get a detector")
	}
	if e.bootStallDetectorFor(capable, &db.Instance{TemplateID: &tmplID}, rescue) != nil {
		t.Error("template instances must never get a detector (no watchdog by design)")
	}
	if e.bootStallDetectorFor(capable, raw, nil) != nil {
		t.Error("nil rescue state must never get a detector")
	}
	eOff := &Engine{logger: slog.Default()}
	if eOff.bootStallDetectorFor(capable, raw, rescue) != nil {
		t.Error("zeroed stall knobs must disable the watchdog entirely")
	}
}

// Enforcing the startup GPU check holds the ready callback for up to the
// check's budget, and the tunnel-stall clock runs until that callback lands.
// The window must widen by the check's maximum delay so a healthy boot that
// spends it checking is not rescued as a stall; shadow mode (check after the
// callback) and a disabled tunnel leg stay as configured.
func TestBootStallDetectorFor_EnforcedGPUCheckWidensTunnelWindow(t *testing.T) {
	capable := &rescueCapableStub{&stubProvider{name: "capable"}}
	raw := &db.Instance{InstanceID: "i"}
	rescue := &bootRescue{attemptsLeft: 1}
	for _, tc := range []struct {
		mode        string
		tunnelAfter time.Duration
		want        time.Duration
	}{
		{"", 8 * time.Minute, 8 * time.Minute},
		{"shadow", 8 * time.Minute, 8 * time.Minute},
		{"enforce", 8 * time.Minute, 8*time.Minute + tunnel.GPUCheckEnforceMaxDelay},
		{"enforce", 0, 0}, // a disabled tunnel leg stays disabled
	} {
		e := &Engine{logger: slog.Default(), tunnelStallAfter: tc.tunnelAfter, pullStallAfter: 5 * time.Minute, gpuCheckMode: tc.mode}
		d := e.bootStallDetectorFor(capable, raw, rescue)
		if d == nil || d.tunnelStallAfter != tc.want {
			t.Fatalf("mode %q, tunnel %v: detector %+v; want tunnel window %v", tc.mode, tc.tunnelAfter, d, tc.want)
		}
	}

	// End to end on the detector: an enforce-mode boot whose callback lands
	// just inside the configured window plus the check's whole budget is not
	// a stall.
	e := &Engine{logger: slog.Default(), tunnelStallAfter: 8 * time.Minute, gpuCheckMode: "enforce"}
	d := e.bootStallDetectorFor(capable, raw, rescue)
	t0 := time.Now()
	running := &provider.InstanceStatus{Status: "running"}
	d.observe(t0, running)
	if v := d.observe(t0.Add(8*time.Minute+tunnel.GPUCheckEnforceSeconds*time.Second), running); v != "" {
		t.Fatalf("enforced check time was judged a stall: %q", v)
	}
	if v := d.observe(t0.Add(8*time.Minute+tunnel.GPUCheckEnforceMaxDelay), running); v != verdictTunnelStall {
		t.Fatalf("past the widened window the stall must still fire, got %q", v)
	}
}

// --- error reason wording ---

// TestStallErrorReason — the reason lands in error_reason, which is surfaced
// to customers as status_reason: it must describe the failure, name the
// rescue exhaustion when one was tried, and stay upstream-name-free.
func TestStallErrorReason(t *testing.T) {
	plain := stallErrorReason(verdictTunnelStall, false)
	if !strings.Contains(plain, "never became reachable") {
		t.Errorf("tunnel-stall reason must describe the failure, got %q", plain)
	}
	exhausted := stallErrorReason(verdictDeadPull, true)
	if !strings.Contains(exhausted, "relaunch") {
		t.Errorf("rescue-exhaustion reason must mention the failed relaunch, got %q", exhausted)
	}
	for _, reason := range []string{plain, exhausted, stallErrorReason("other", false)} {
		lower := strings.ToLower(reason)
		for _, banned := range []string{"marketco", "podco", "upstream provider"} {
			if strings.Contains(lower, banned) {
				t.Errorf("customer-visible reason leaks %q: %q", banned, reason)
			}
		}
	}
}

// --- rescue execution ---

// a100Offering is a minimal on-demand offering both rescue stubs advertise.
func a100Offering(providerName string, price float64) provider.GPUOffering {
	return provider.GPUOffering{
		Provider:       providerName,
		GPUType:        provider.GPUTypeA10080GB,
		GPUCount:       1,
		VRAMPerGPUGB:   80,
		PricePerHour:   price,
		Tier:           provider.TierOnDemand,
		Region:         "us-west",
		AvailableCount: 1,
	}
}

// rescueFixture wires an engine with the stalled (capable) provider plus a
// backup provider, a scripted rebind, and a bootRescue mirroring what
// Provision captures.
func rescueFixture(t *testing.T, fake *fakeEngineDB) (*Engine, *rescueCapableStub, *stubProvider, *bootRescue, *db.Instance) {
	t.Helper()
	stalled := &rescueCapableStub{&stubProvider{
		name:      "stuck",
		offerings: []provider.GPUOffering{a100Offering("stuck", 1.0)},
		// The stalled provider's re-attempt fails (its only host is excluded)
		// so the walk falls through to the backup — the exclusion contract is
		// asserted on the request it received.
		provisionErr: provider.ErrNoCapacity,
	}}
	backup := &stubProvider{
		name:      "backup",
		offerings: []provider.GPUOffering{a100Offering("backup", 2.0)},
		provisionResult: &provider.ProvisionResult{
			UpstreamID:  "up-new",
			Provider:    "backup",
			Status:      "creating",
			CostPerHour: 2.0,
			Region:      "us-west",
		},
	}
	reg := provider.NewRegistry()
	reg.Register(stalled)
	reg.Register(backup)
	e := &Engine{registry: reg, db: fake, config: &config.Config{}, logger: slog.Default()}

	rescue := &bootRescue{
		req: ProvisionRequest{
			OrgID:    "org-1",
			UserID:   "user-1",
			GPUType:  provider.GPUTypeA10080GB,
			GPUCount: 1,
			Tier:     provider.TierOnDemand,
		},
		provReq: provider.ProvisionRequest{
			InstanceID: "inst-1",
			GPUType:    provider.GPUTypeA10080GB,
			GPUCount:   1,
			Tier:       provider.TierOnDemand,
		},
		walk:         provisionWalkParams{instanceID: "inst-1", effectiveDisk: 100},
		attemptsLeft: 1,
	}
	inst := &db.Instance{
		InstanceID:       "inst-1",
		UpstreamProvider: "stuck",
		UpstreamID:       "up-stalled",
		Region:           "us-west",
		Status:           StateBooting,
	}
	return e, stalled, backup, rescue, inst
}

// TestAttemptBootRescue_Success — full happy path: the stalled upstream is
// terminated FIRST, the re-walk carries the stalled host in
// ExcludeUpstreamHostIDs, the replacement lands on the backup provider, and
// the row is re-bound to it.
func TestAttemptBootRescue_Success(t *testing.T) {
	type rebindArgs struct {
		provider, upstreamID string
		price                float64
	}
	var rebinds []rebindArgs
	freshRow := &db.Instance{
		InstanceID:       "inst-1",
		UpstreamProvider: "backup",
		UpstreamID:       "up-new",
		Status:           StateProvisioning,
	}
	fake := &fakeEngineDB{
		rebindInstanceUpstream: func(id, prov, upID, region string, price, upPrice float64, diskGB *int) (bool, error) {
			rebinds = append(rebinds, rebindArgs{provider: prov, upstreamID: upID, price: price})
			return true, nil
		},
		getInstance: func(id string) (*db.Instance, error) { return freshRow, nil },
	}
	e, stalled, backup, rescue, inst := rescueFixture(t, fake)

	outcome, newProv, newInst := e.attemptBootRescue(context.Background(), "inst-1", inst, stalled, rescue, "8197", verdictDeadPull)
	if outcome != rescueLaunched {
		t.Fatalf("want rescueLaunched, got %v", outcome)
	}
	if newProv.Name() != "backup" || newInst.UpstreamID != "up-new" {
		t.Errorf("loop working set must switch to the replacement, got %s/%s", newProv.Name(), newInst.UpstreamID)
	}

	// The stalled upstream was torn down with its own id+region.
	if len(stalled.terminateCalls) != 1 || stalled.terminateCalls[0].UpstreamID != "up-stalled" {
		t.Errorf("stalled upstream must be terminated, got %+v", stalled.terminateCalls)
	}
	// Both re-attempts carried the stalled host exclusion.
	for _, stub := range []*stubProvider{stalled.stubProvider, backup} {
		if len(stub.provisionReqs) != 1 {
			t.Fatalf("%s: want exactly 1 provision attempt, got %d", stub.name, len(stub.provisionReqs))
		}
		got := stub.provisionReqs[0].ExcludeUpstreamHostIDs
		if len(got) != 1 || got[0] != "8197" {
			t.Errorf("%s: rescue request must exclude the stalled host, got %v", stub.name, got)
		}
	}
	// The row was re-bound to the replacement at the replacement's price.
	if len(rebinds) != 1 || rebinds[0].provider != "backup" || rebinds[0].upstreamID != "up-new" {
		t.Fatalf("rebind args wrong: %+v", rebinds)
	}
	if rebinds[0].price <= 0 {
		t.Errorf("re-bound retail price must be repriced for the replacement, got %g", rebinds[0].price)
	}
	if rescue.attemptsLeft != 0 {
		t.Errorf("the rescue attempt must be consumed, got %d left", rescue.attemptsLeft)
	}
}

// TestAttemptBootRescue_RebindRaceLost — a concurrent terminate wins the
// rebind guard: the freshly provisioned REPLACEMENT must be torn down (it is
// tracked by nothing) and the outcome is rescueAborted so the poll loop exits
// without touching the row.
func TestAttemptBootRescue_RebindRaceLost(t *testing.T) {
	fake := &fakeEngineDB{
		rebindInstanceUpstream: func(id, prov, upID, region string, price, upPrice float64, diskGB *int) (bool, error) {
			return false, nil // row already left its mid-provision state
		},
	}
	e, stalled, backup, rescue, inst := rescueFixture(t, fake)

	outcome, _, _ := e.attemptBootRescue(context.Background(), "inst-1", inst, stalled, rescue, "8197", verdictTunnelStall)
	if outcome != rescueAborted {
		t.Fatalf("want rescueAborted on a lost rebind race, got %v", outcome)
	}
	found := false
	for _, c := range backup.terminateCalls {
		if c.UpstreamID == "up-new" {
			found = true
		}
	}
	if !found {
		t.Errorf("the orphaned replacement upstream must be terminated, got %+v", backup.terminateCalls)
	}
}

// TestAttemptBootRescue_NoReplacementCapacity — the walk finds nothing (every
// candidate fails): outcome rescueFailed, and the stalled upstream was still
// torn down first (the money stops even when the rescue can't land).
func TestAttemptBootRescue_NoReplacementCapacity(t *testing.T) {
	fake := &fakeEngineDB{}
	e, stalled, backup, rescue, inst := rescueFixture(t, fake)
	backup.provisionResult = nil
	backup.provisionErr = provider.ErrNoCapacity

	outcome, _, _ := e.attemptBootRescue(context.Background(), "inst-1", inst, stalled, rescue, "8197", verdictDeadPull)
	if outcome != rescueFailed {
		t.Fatalf("want rescueFailed when no replacement places, got %v", outcome)
	}
	if len(stalled.terminateCalls) != 1 {
		t.Errorf("stalled upstream must be terminated before the rescue attempt, got %+v", stalled.terminateCalls)
	}
}

// TestAttemptBootRescue_PriceCeilingWithoutExplicitCap — the price-clarity
// contract: a launch that carried NO explicit max_price must never be
// re-bound above the retail already persisted on the row. The fixture's only
// replacement (backup, $2.00 retail with the empty-config zero markup) sits
// above the $1.50 persisted rate, so the rescue must fail rather than
// silently upcharge.
func TestAttemptBootRescue_PriceCeilingWithoutExplicitCap(t *testing.T) {
	rebinds := 0
	fake := &fakeEngineDB{
		rebindInstanceUpstream: func(id, prov, upID, region string, price, upPrice float64, diskGB *int) (bool, error) {
			rebinds++
			return true, nil
		},
	}
	e, stalled, _, rescue, inst := rescueFixture(t, fake)
	inst.PricePerHour = 1.50

	outcome, _, _ := e.attemptBootRescue(context.Background(), "inst-1", inst, stalled, rescue, "8197", verdictDeadPull)
	if outcome != rescueFailed {
		t.Fatalf("want rescueFailed when the only replacement is pricier than the placed rate, got %v", outcome)
	}
	if rebinds != 0 {
		t.Errorf("row must not be re-bound above the persisted retail, got %d rebinds", rebinds)
	}
	// The derived ceiling is per-attempt state and must not leak into the
	// stored rescue params (a later code path could mistake it for a
	// customer-chosen cap).
	if rescue.walk.maxPricePerHour != nil {
		t.Errorf("derived ceiling leaked into rescue.walk.maxPricePerHour: %v", *rescue.walk.maxPricePerHour)
	}
}

// TestAttemptBootRescue_ExplicitCapStillHonored — an explicit customer cap is
// the consented ceiling: with max_price $3.00 the $2.00 replacement places
// even though the originally persisted rate was lower.
func TestAttemptBootRescue_ExplicitCapStillHonored(t *testing.T) {
	var reboundPrice float64
	freshRow := &db.Instance{
		InstanceID:       "inst-1",
		UpstreamProvider: "backup",
		UpstreamID:       "up-new",
		Status:           StateProvisioning,
	}
	fake := &fakeEngineDB{
		rebindInstanceUpstream: func(id, prov, upID, region string, price, upPrice float64, diskGB *int) (bool, error) {
			reboundPrice = price
			return true, nil
		},
		getInstance: func(id string) (*db.Instance, error) { return freshRow, nil },
	}
	e, stalled, _, rescue, inst := rescueFixture(t, fake)
	inst.PricePerHour = 1.50
	cap := 3.00
	rescue.walk.maxPricePerHour = &cap

	outcome, _, _ := e.attemptBootRescue(context.Background(), "inst-1", inst, stalled, rescue, "8197", verdictDeadPull)
	if outcome != rescueLaunched {
		t.Fatalf("want rescueLaunched under the explicit cap, got %v", outcome)
	}
	if reboundPrice != 2.00 {
		t.Errorf("replacement must re-bind at its own retail under the explicit cap, got %g", reboundPrice)
	}
}

// TestAttemptBootRescue_CheaperBuyRebindsLower — GPU-78 at the rescue site.
// The replacement offering QUOTES $2.00 but the ask the adapter actually bought
// costs $1.20. The rebind must persist retail of what we paid, not retail of
// the stale quote — the same min() discipline Provision's persist site applies,
// or the overbilling simply survives via rescues.
func TestAttemptBootRescue_CheaperBuyRebindsLower(t *testing.T) {
	var reboundPrice, reboundUpstream float64
	freshRow := &db.Instance{
		InstanceID:       "inst-1",
		UpstreamProvider: "backup",
		UpstreamID:       "up-new",
		Status:           StateProvisioning,
	}
	fake := &fakeEngineDB{
		rebindInstanceUpstream: func(id, prov, upID, region string, price, upPrice float64, diskGB *int) (bool, error) {
			reboundPrice, reboundUpstream = price, upPrice
			return true, nil
		},
		getInstance: func(id string) (*db.Instance, error) { return freshRow, nil },
	}
	e, stalled, backup, rescue, inst := rescueFixture(t, fake)
	inst.PricePerHour = 1.50
	cap := 3.00
	rescue.walk.maxPricePerHour = &cap
	backup.provisionResult.CostPerHour = 1.20

	outcome, _, _ := e.attemptBootRescue(context.Background(), "inst-1", inst, stalled, rescue, "8197", verdictDeadPull)
	if outcome != rescueLaunched {
		t.Fatalf("want rescueLaunched, got %v", outcome)
	}
	if reboundPrice != 1.20 {
		t.Errorf("re-bound retail = %g, want 1.20 — the bill follows the buy, not the quote", reboundPrice)
	}
	if reboundUpstream != 1.20 {
		t.Errorf("COGS = %g, want the 1.20 actually paid", reboundUpstream)
	}
}

// TestAttemptBootRescue_UnreportedCostKeepsQuotedRetail — the other half of the
// GPU-78 rescue guard: an adapter that reports no per-instance charge must
// re-bind at the replacement's quoted retail exactly as before, never at zero.
func TestAttemptBootRescue_UnreportedCostKeepsQuotedRetail(t *testing.T) {
	var reboundPrice float64
	freshRow := &db.Instance{
		InstanceID:       "inst-1",
		UpstreamProvider: "backup",
		UpstreamID:       "up-new",
		Status:           StateProvisioning,
	}
	fake := &fakeEngineDB{
		rebindInstanceUpstream: func(id, prov, upID, region string, price, upPrice float64, diskGB *int) (bool, error) {
			reboundPrice = price
			return true, nil
		},
		getInstance: func(id string) (*db.Instance, error) { return freshRow, nil },
	}
	e, stalled, backup, rescue, inst := rescueFixture(t, fake)
	inst.PricePerHour = 1.50
	cap := 3.00
	rescue.walk.maxPricePerHour = &cap
	backup.provisionResult.CostPerHour = 0

	if outcome, _, _ := e.attemptBootRescue(context.Background(), "inst-1", inst, stalled, rescue, "8197", verdictDeadPull); outcome != rescueLaunched {
		t.Fatalf("want rescueLaunched, got %v", outcome)
	}
	if reboundPrice != 2.00 {
		t.Errorf("re-bound retail = %g, want the quoted 2.00 — an unreported cost is not a free instance", reboundPrice)
	}
}

// TestAttemptBootRescue_ViewedPriceDoesNotVetoRescue — the viewed card price
// (GPU-78) gates the LAUNCH, not the recovery. A rescue is already bounded by
// the persisted retail the customer is paying, so letting a stale card price
// veto it would trade a working instance for a dead one over a number that
// changes nothing about the bill.
func TestAttemptBootRescue_ViewedPriceDoesNotVetoRescue(t *testing.T) {
	freshRow := &db.Instance{
		InstanceID:       "inst-1",
		UpstreamProvider: "backup",
		UpstreamID:       "up-new",
		Status:           StateProvisioning,
	}
	fake := &fakeEngineDB{
		rebindInstanceUpstream: func(id, prov, upID, region string, price, upPrice float64, diskGB *int) (bool, error) {
			return true, nil
		},
		getInstance: func(id string) (*db.Instance, error) { return freshRow, nil },
	}
	e, stalled, _, rescue, inst := rescueFixture(t, fake)
	inst.PricePerHour = 1.50
	cap := 3.00
	rescue.walk.maxPricePerHour = &cap
	// The original launch saw a $0.50 card — far under the $2.00 replacement.
	viewed := 0.50
	rescue.walk.viewedPricePerHour = &viewed

	if outcome, _, _ := e.attemptBootRescue(context.Background(), "inst-1", inst, stalled, rescue, "8197", verdictDeadPull); outcome != rescueLaunched {
		t.Fatalf("want rescueLaunched — the launch-time card price must not veto a rescue, got %v", outcome)
	}
}

// TestProgressStatus_RescueArmed_NonCapableProvider — arming a rescue on a
// provider WITHOUT the capability changes nothing: the timeout path behaves
// exactly as the historical BLOCK-04 contract (terminate, then error).
func TestProgressStatus_RescueArmed_NonCapableProvider(t *testing.T) {
	var events []string
	stub := &stubProvider{
		name:         "stub",
		statusResult: &provider.InstanceStatus{Status: "creating"},
		onTerminate:  func(_, _ string) { events = append(events, "terminate") },
	}
	row := &db.Instance{
		InstanceID:       "inst-nc",
		UpstreamProvider: "stub",
		UpstreamID:       "up-nc",
		Region:           "us-west",
		Status:           StateProvisioning,
	}
	fake := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) {
			return from == StateCreating && to == StateProvisioning, nil
		},
		getInstance:        func(id string) (*db.Instance, error) { return row, nil },
		onSetInstanceError: func(_, _ string) { events = append(events, "set_error") },
	}
	reg := provider.NewRegistry()
	reg.Register(stub)
	e := &Engine{
		registry:           reg,
		db:                 fake,
		logger:             slog.Default(),
		provisionTimeout:   5 * time.Millisecond,
		tunnelStallAfter:   8 * time.Minute,
		pullStallAfter:     5 * time.Minute,
		bootRescueAttempts: 1,
	}

	e.progressStatus("inst-nc", e.provisionTimeout, &bootRescue{attemptsLeft: 1})

	want := []string{"terminate", "set_error"}
	if len(events) != len(want) || events[0] != want[0] || events[1] != want[1] {
		t.Fatalf("non-capable provider must keep the exact timeout behavior, got %v", events)
	}
}

// --- rescue audit event (GPU-89) ---

// TestRescueEventMetadata — the audit row's whole value is the BEFORE/AFTER
// pair: post-rebind the instances row describes only the replacement, so the
// original placement and the subsidy the rescue accepted are unreconstructable
// without these fields. Table-driven over the shapes the emit sees live.
func TestRescueEventMetadata(t *testing.T) {
	tests := []struct {
		name        string
		verdict     string
		old         *db.Instance
		newProvider string
		newUpstream string
		newPrice    float64
		retail      float64
		want        map[string]any
	}{
		{
			name:    "subsidised rescue carries both sides",
			verdict: verdictDeadPull,
			old: &db.Instance{
				UpstreamProvider:     "stuck",
				UpstreamID:           "up-stalled",
				UpstreamPricePerHour: 1.0,
			},
			newProvider: "backup",
			newUpstream: "up-new",
			newPrice:    2.0,
			retail:      1.5,
			want: map[string]any{
				"verdict":                     verdictDeadPull,
				"old_provider":                "stuck",
				"old_upstream_id":             "up-stalled",
				"old_upstream_price_per_hour": 1.0,
				"new_provider":                "backup",
				"new_upstream_id":             "up-new",
				"new_upstream_price_per_hour": 2.0,
				"retail_price_per_hour":       1.5,
			},
		},
		{
			name:    "nil old row still emits a usable record",
			verdict: verdictTunnelStall,
			old:     nil,
			// A nil pre-rebind row should never happen on the live path, but the
			// emit is best-effort: it must degrade to a partial record, not panic
			// inside the rescue.
			newProvider: "backup",
			newUpstream: "up-new",
			newPrice:    0.9,
			retail:      0.9,
			want: map[string]any{
				"verdict":                     verdictTunnelStall,
				"old_provider":                "",
				"old_upstream_id":             "",
				"old_upstream_price_per_hour": 0.0,
				"new_provider":                "backup",
				"new_upstream_id":             "up-new",
				"new_upstream_price_per_hour": 0.9,
				"retail_price_per_hour":       0.9,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := rescueEventMetadata(tc.verdict, tc.old, tc.newProvider, tc.newUpstream, tc.newPrice, tc.retail)
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("metadata must be valid JSON, got %q (%v)", raw, err)
			}
			for k, want := range tc.want {
				have, ok := got[k]
				if !ok {
					t.Errorf("metadata missing %q (got %v)", k, got)
					continue
				}
				switch w := want.(type) {
				case float64:
					f, ok := have.(float64)
					if !ok || f != w {
						t.Errorf("metadata[%q] = %v, want %v", k, have, w)
					}
				default:
					if have != want {
						t.Errorf("metadata[%q] = %v, want %v", k, have, want)
					}
				}
			}
		})
	}
}

// TestAttemptBootRescue_EmitsRescuedEvent — a successful rebind writes exactly
// one 'rescued' audit row, scoped to the instance's org, carrying the
// before/after metadata. Nothing is published to SSE/webhooks from here (the
// event is admin-only, T-q89-01).
func TestAttemptBootRescue_EmitsRescuedEvent(t *testing.T) {
	var emitted []*db.InstanceEvent
	fake := &fakeEngineDB{
		rebindInstanceUpstream: func(id, prov, upID, region string, price, upPrice float64, diskGB *int) (bool, error) {
			return true, nil
		},
		getInstance: func(id string) (*db.Instance, error) {
			return &db.Instance{InstanceID: "inst-1", UpstreamProvider: "backup", UpstreamID: "up-new"}, nil
		},
		createInstanceEvent: func(ev *db.InstanceEvent) error {
			emitted = append(emitted, ev)
			return nil
		},
	}
	e, stalled, _, rescue, inst := rescueFixture(t, fake)
	inst.OrgID = "org-1"
	inst.UpstreamPricePerHour = 1.0

	outcome, _, _ := e.attemptBootRescue(context.Background(), "inst-1", inst, stalled, rescue, "8197", verdictDeadPull)
	if outcome != rescueLaunched {
		t.Fatalf("want rescueLaunched, got %v", outcome)
	}
	if len(emitted) != 1 {
		t.Fatalf("a successful rescue must emit exactly one audit event, got %d", len(emitted))
	}
	ev := emitted[0]
	if ev.EventType != "rescued" {
		t.Errorf("event_type = %q, want \"rescued\"", ev.EventType)
	}
	if ev.InstanceID != "inst-1" || ev.OrgID != "org-1" {
		t.Errorf("event must be scoped to the instance+org, got %s/%s", ev.InstanceID, ev.OrgID)
	}
	var meta map[string]any
	if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
		t.Fatalf("metadata must be valid JSON: %v", err)
	}
	if meta["old_upstream_id"] != "up-stalled" || meta["new_upstream_id"] != "up-new" {
		t.Errorf("metadata must carry the upstream swap, got %v", meta)
	}
	if meta["verdict"] != verdictDeadPull {
		t.Errorf("metadata must carry the stall verdict, got %v", meta["verdict"])
	}
}

// TestAttemptBootRescue_EventWriteFailureIsNonFatal — the audit write is
// BEST-EFFORT: an insert failure must never downgrade a rescue that already
// re-bound the row (the replacement is live and billing-safe either way). Same
// rationale as the terminated-event write in engine.go.
func TestAttemptBootRescue_EventWriteFailureIsNonFatal(t *testing.T) {
	freshRow := &db.Instance{InstanceID: "inst-1", UpstreamProvider: "backup", UpstreamID: "up-new"}
	fake := &fakeEngineDB{
		rebindInstanceUpstream: func(id, prov, upID, region string, price, upPrice float64, diskGB *int) (bool, error) {
			return true, nil
		},
		getInstance:         func(id string) (*db.Instance, error) { return freshRow, nil },
		createInstanceEvent: func(ev *db.InstanceEvent) error { return errors.New("insert exploded") },
	}
	e, stalled, _, rescue, inst := rescueFixture(t, fake)

	outcome, newProv, newInst := e.attemptBootRescue(context.Background(), "inst-1", inst, stalled, rescue, "8197", verdictTunnelStall)
	if outcome != rescueLaunched {
		t.Fatalf("a failed audit write must not change the rescue outcome, got %v", outcome)
	}
	if newProv == nil || newProv.Name() != "backup" || newInst == nil || newInst.UpstreamID != "up-new" {
		t.Fatalf("the working set must still switch to the replacement, got %v/%v", newProv, newInst)
	}
}

// --- upstream-terminal failover (GPU-108) ---

// failoverFixture builds the working set for the poll-loop failover tests: a
// NON-rescue-capable provider whose upstream reports "error" (the live
// vmco accept-then-ERROR shape), a healthy backup, and the bootRescue
// Provision now arms for every non-reserved launch. The rescue request
// carries a stale exact-offering pin on purpose — the failover must relax it
// or the backup could never match.
func failoverFixture() (dying, backup *stubProvider, rescue *bootRescue, inst *db.Instance) {
	dying = &stubProvider{
		name:         "dying",
		offerings:    []provider.GPUOffering{a100Offering("dying", 1.0)},
		statusResult: &provider.InstanceStatus{Status: "error"},
	}
	backup = &stubProvider{
		name:         "backup",
		offerings:    []provider.GPUOffering{a100Offering("backup", 2.0)},
		statusResult: &provider.InstanceStatus{Status: "creating"},
		provisionResult: &provider.ProvisionResult{
			UpstreamID:  "up-new",
			Provider:    "backup",
			Status:      "creating",
			CostPerHour: 2.0,
			Region:      "us-west",
		},
	}
	rescue = &bootRescue{
		req: ProvisionRequest{
			OrgID:              "org-1",
			UserID:             "user-1",
			GPUType:            provider.GPUTypeA10080GB,
			GPUCount:           1,
			Tier:               provider.TierOnDemand,
			SelectedOfferingID: "stale-pin",
		},
		provReq: provider.ProvisionRequest{
			InstanceID: "inst-f",
			GPUType:    provider.GPUTypeA10080GB,
			GPUCount:   1,
			Tier:       provider.TierOnDemand,
		},
		walk:         provisionWalkParams{instanceID: "inst-f", effectiveDisk: 100},
		attemptsLeft: 1,
	}
	inst = &db.Instance{
		InstanceID:       "inst-f",
		UpstreamProvider: "dying",
		UpstreamID:       "up-dying",
		Region:           "us-west",
		Status:           StateProvisioning,
	}
	return dying, backup, rescue, inst
}

func failoverEngine(fake *fakeEngineDB, provs ...provider.Provider) *Engine {
	reg := provider.NewRegistry()
	for _, p := range provs {
		reg.Register(p)
	}
	return &Engine{
		registry:           reg,
		db:                 fake,
		config:             &config.Config{},
		logger:             slog.Default(),
		provisionTimeout:   2 * time.Second,
		bootRescueAttempts: 1,
		pollInterval:       time.Millisecond,
	}
}

// TestProgressStatus_UpstreamErrorFailsOverToBackup — the poll loop sees the
// upstream report "error" pre-running and, instead of condemning the launch,
// terminates the dead upstream, EXCLUDES its provider from the re-match,
// relaxes the exact-offering pin, and rebinds the row to the backup. The
// instance is never errored.
func TestProgressStatus_UpstreamErrorFailsOverToBackup(t *testing.T) {
	dying, backup, rescue, inst := failoverFixture()
	var rebinds []string
	var errored []string
	rebound := false
	fake := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) {
			return from == StateCreating && to == StateProvisioning, nil
		},
		rebindInstanceUpstream: func(id, prov, upID, region string, price, upPrice float64, diskGB *int) (bool, error) {
			rebinds = append(rebinds, prov+"/"+upID)
			rebound = true
			return true, nil
		},
		getInstance: func(id string) (*db.Instance, error) {
			if rebound {
				// Exit the poll loop via the concurrent-status-change branch.
				return &db.Instance{InstanceID: "inst-f", UpstreamProvider: "backup",
					UpstreamID: "up-new", Status: StateTerminated}, nil
			}
			return inst, nil
		},
		onSetInstanceError: func(id, reason string) { errored = append(errored, reason) },
	}
	e := failoverEngine(fake, dying, backup)

	e.progressStatus("inst-f", e.provisionTimeout, rescue)

	if len(rebinds) != 1 || rebinds[0] != "backup/up-new" {
		t.Fatalf("row must rebind to the backup replacement, got %v", rebinds)
	}
	if len(errored) != 0 {
		t.Errorf("a failed-over launch must not be errored, got %v", errored)
	}
	if len(dying.terminateCalls) == 0 || dying.terminateCalls[0].UpstreamID != "up-dying" {
		t.Errorf("the dead upstream must be terminated first, got %+v", dying.terminateCalls)
	}
	if len(dying.provisionReqs) != 0 {
		t.Errorf("the failed provider must be excluded from its own replacement, got %d attempts", len(dying.provisionReqs))
	}
	if len(backup.provisionReqs) != 1 {
		t.Errorf("backup must receive exactly one replacement attempt, got %d", len(backup.provisionReqs))
	}
	if rescue.req.SelectedOfferingID != "" {
		t.Errorf("the stale exact-offering pin must be relaxed, still %q", rescue.req.SelectedOfferingID)
	}
}

// TestProgressStatus_UpstreamErrorNoReplacementFails — no other provider has
// capacity: the launch fails with the upstream-terminal reason (naming both
// the start failure and the failed relaunch), and the dead upstream is still
// torn down.
func TestProgressStatus_UpstreamErrorNoReplacementFails(t *testing.T) {
	dying, _, rescue, inst := failoverFixture()
	var errored []string
	fake := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) {
			return from == StateCreating && to == StateProvisioning, nil
		},
		getInstance:        func(id string) (*db.Instance, error) { return inst, nil },
		onSetInstanceError: func(id, reason string) { errored = append(errored, reason) },
	}
	e := failoverEngine(fake, dying) // no backup registered

	e.progressStatus("inst-f", e.provisionTimeout, rescue)

	if len(errored) != 1 {
		t.Fatalf("want exactly one error write, got %v", errored)
	}
	want := stallErrorReason(verdictUpstreamTerminal, true)
	if errored[0] != want {
		t.Errorf("error reason = %q, want %q", errored[0], want)
	}
	if len(dying.terminateCalls) == 0 {
		t.Errorf("the dead upstream must still be terminated, got %+v", dying.terminateCalls)
	}
}

// TestProgressStatus_UpstreamErrorPinnedProviderKeepsLegacyPath — a staff
// provider pin means no cross-provider failover: the legacy terminate +
// error path runs byte-for-byte.
func TestProgressStatus_UpstreamErrorPinnedProviderKeepsLegacyPath(t *testing.T) {
	dying, backup, rescue, inst := failoverFixture()
	rescue.req.PinnedProvider = "dying"
	var errored []string
	fake := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) {
			return from == StateCreating && to == StateProvisioning, nil
		},
		getInstance:        func(id string) (*db.Instance, error) { return inst, nil },
		onSetInstanceError: func(id, reason string) { errored = append(errored, reason) },
	}
	e := failoverEngine(fake, dying, backup)

	e.progressStatus("inst-f", e.provisionTimeout, rescue)

	if len(errored) != 1 || errored[0] != "upstream instance failed: error" {
		t.Fatalf("pinned launch must keep the legacy error reason, got %v", errored)
	}
	if len(backup.provisionReqs) != 0 {
		t.Errorf("pinned launch must never fail over cross-provider, backup got %d attempts", len(backup.provisionReqs))
	}
}

// TestProgressStatus_UpstreamErrorRescueExhaustedKeepsLegacyPath — with no
// rescue attempts remaining the terminal branch behaves exactly as before
// the failover existed.
func TestProgressStatus_UpstreamErrorRescueExhaustedKeepsLegacyPath(t *testing.T) {
	dying, backup, rescue, inst := failoverFixture()
	rescue.attemptsLeft = 0
	var errored []string
	fake := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) {
			return from == StateCreating && to == StateProvisioning, nil
		},
		getInstance:        func(id string) (*db.Instance, error) { return inst, nil },
		onSetInstanceError: func(id, reason string) { errored = append(errored, reason) },
	}
	e := failoverEngine(fake, dying, backup)

	e.progressStatus("inst-f", e.provisionTimeout, rescue)

	if len(errored) != 1 || errored[0] != "upstream instance failed: error" {
		t.Fatalf("exhausted rescue must keep the legacy error reason, got %v", errored)
	}
	if len(backup.provisionReqs) != 0 {
		t.Errorf("exhausted rescue must not fail over, backup got %d attempts", len(backup.provisionReqs))
	}
}

// TestSelectProviderCandidates_ExcludeProviders — the exclusion narrows the
// candidate set (case-insensitively) and an empty exclusion changes nothing.
func TestSelectProviderCandidates_ExcludeProviders(t *testing.T) {
	dying, backup, rescue, _ := failoverFixture()
	e := failoverEngine(&fakeEngineDB{}, dying, backup)
	req := rescue.req
	req.SelectedOfferingID = ""

	req.ExcludeProviders = []string{"DYING"}
	candidates, err := e.SelectProviderCandidates(context.Background(), req)
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	for _, c := range candidates {
		if c.Prov.Name() == "dying" {
			t.Errorf("excluded provider must not be a candidate")
		}
	}
	if len(candidates) == 0 {
		t.Fatalf("backup must survive the exclusion")
	}

	req.ExcludeProviders = nil
	candidates, err = e.SelectProviderCandidates(context.Background(), req)
	if err != nil {
		t.Fatalf("SelectProviderCandidates without exclusions: %v", err)
	}
	if len(candidates) != 2 {
		t.Errorf("no exclusions must keep both providers, got %d", len(candidates))
	}
}

// TestProviderExclusionHelpers — dedupe and case-insensitive membership.
func TestProviderExclusionHelpers(t *testing.T) {
	list := appendProviderExclusion(nil, "vmco")
	list = appendProviderExclusion(list, "VmCo")
	if len(list) != 1 {
		t.Errorf("append must dedupe case-insensitively, got %v", list)
	}
	if !providerExcluded(list, "VMCO") || providerExcluded(list, "marketco") {
		t.Errorf("membership must be case-insensitive and exact, got %v", list)
	}
}

// TestStallErrorReason_UpstreamTerminal — customer-visible wording for the
// provider-killed-it verdict, with the same INFPRIV guarantees as the stall
// reasons.
func TestStallErrorReason_UpstreamTerminal(t *testing.T) {
	plain := stallErrorReason(verdictUpstreamTerminal, false)
	if plain != "provisioning failed: the instance could not be started" {
		t.Errorf("plain reason = %q", plain)
	}
	tried := stallErrorReason(verdictUpstreamTerminal, true)
	if !strings.Contains(tried, "relaunch") {
		t.Errorf("failover-tried reason must mention the failed relaunch, got %q", tried)
	}
	for _, reason := range []string{plain, tried} {
		lower := strings.ToLower(reason)
		for _, banned := range []string{"marketco", "podco", "vmco", "upstream provider"} {
			if strings.Contains(lower, banned) {
				t.Errorf("customer-visible reason leaks %q: %q", banned, reason)
			}
		}
	}
}
