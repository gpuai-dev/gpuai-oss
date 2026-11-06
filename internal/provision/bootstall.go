package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/tunnel"
)

// Boot-stall detection + rescue reprovision (2026-08 marketplace stall fix).
//
// The 20-min marketplace-supplier boot stalls were characterized (2026-08-04) as host-specific
// with two distinct modes, neither of which the flat provisioning budget
// distinguishes from a healthy slow boot:
//
//   - Mode A "tunnel stall": the upstream container reaches "running" within
//     seconds (image cached) but the bootstrap never phones home — no tunnel
//     registration, no ready callback. Healthy hosts complete this in <=3 min;
//     the stalled ones burn the whole 20-min budget.
//   - Mode B "dead pull": the upstream sits pre-running while the host pulls
//     the image over a pipe too thin to ever finish inside the budget.
//
// The policy is PROGRESS-AWARE, not speed-based: a 13-min image pull with
// continuously advancing progress (InstanceStatus.BootDetail) succeeded live
// and must never be killed. Only "no callback despite running for N minutes"
// and "no pull progress for M minutes" are verdicts.
//
// On a verdict the engine terminates the stalled upstream and (budget and
// attempts permitting) RESCUES the launch: re-matches fresh inventory with the
// stalled host excluded, re-runs the normal candidate attempt walk, and
// re-points the instance row at the replacement upstream. The rescue inherits
// the REMAINING provisioning budget — worst-case wall clock is unchanged and
// the 30-min durable reaper never preempts a legitimate rescue. Billing is
// safe by construction: the session opens only at the running transition.
//
// The stall WATCHDOG is gated on provider.BootRescueCapability (only adapters
// that report boot progress declare it) and skipped for template deploys (their poll loop deliberately has no
// watchdog — app image pulls can legitimately be slow). The rescue STATE and
// attemptBootRescue itself are wider than the watchdog since GPU-108: they are
// armed for every non-reserved launch, because progressStatus's terminal
// branch reuses them to fail over to another provider when the upstream
// reports terminated/error pre-running (no judgment involved — the provider
// itself declared the VM dead). State is IN-MEMORY only: a gpuctl restart
// mid-boot falls back to the durable reaper path, exactly as today.

// Stall verdicts, also used to build the customer-visible error reason — keep
// the vocabulary upstream-name-free.
const (
	verdictTunnelStall = "tunnel-stall"
	verdictDeadPull    = "dead-pull"
	// verdictUpstreamTerminal is not a stall judgment at all: the provider
	// itself reported the pre-running upstream terminated/errored. Issued by
	// progressStatus's terminal branch, not the detector — no telemetry or
	// clocks involved, the upstream is dead on the provider's own authority.
	verdictUpstreamTerminal = "upstream-terminal"
	// verdictGPUCheckFailed: the bootstrap's startup GPU compute check failed
	// in enforce mode (CUDA could not run a kernel, fewer GPUs than paid for,
	// or the promised framework could not use the GPU). Not a stall, but the
	// same remedy: release the host, rescue elsewhere, never bill.
	verdictGPUCheckFailed = "gpu-check-failed"
	// verdictGPUCheckEnvironment: the same check failed because the framework
	// the environment promised could not use the GPU ("environment:" reason).
	// That is the image, not the host: every host fails it the same way, so it
	// is never rescued (a rescue would only spend the allowance and exclude a
	// healthy host); the launch fails fast with an image-level reason.
	verdictGPUCheckEnvironment = "gpu-check-environment"
)

// bootStallDetector accumulates per-poll boot telemetry and judges stalls.
// Pure state machine over injected clocks, so tests drive it directly without
// the (non-injectable) 5s poll ticker.
type bootStallDetector struct {
	tunnelStallAfter time.Duration // 0 disables the Mode A leg
	pullStallAfter   time.Duration // 0 disables the Mode B leg

	lastDetail        string
	lastDetailChange  time.Time
	upstreamRunningAt time.Time // zero until the upstream first reports running
	lastHostID        string    // most recent non-empty UpstreamHostID seen
}

func newBootStallDetector(tunnelStallAfter, pullStallAfter time.Duration, now time.Time) *bootStallDetector {
	return &bootStallDetector{
		tunnelStallAfter: tunnelStallAfter,
		pullStallAfter:   pullStallAfter,
		lastDetailChange: now,
	}
}

// observe folds one status poll into the detector and returns a non-empty
// verdict when the boot is judged stalled. Only the two boot-phase upstream
// states are ever judged — terminal states flow to the poll loop's existing
// handling untouched.
func (d *bootStallDetector) observe(now time.Time, status *provider.InstanceStatus) string {
	if status.UpstreamHostID != "" {
		d.lastHostID = status.UpstreamHostID
	}
	if status.BootDetail != d.lastDetail {
		d.lastDetail = status.BootDetail
		d.lastDetailChange = now
	}
	switch status.Status {
	case "running":
		// Mode A: the container is up but our FSM is still waiting on the
		// tunnel/ready callback (the poll loop only reaches here pre-running).
		if d.upstreamRunningAt.IsZero() {
			d.upstreamRunningAt = now
		}
		if d.tunnelStallAfter > 0 && now.Sub(d.upstreamRunningAt) >= d.tunnelStallAfter {
			return verdictTunnelStall
		}
	case "creating":
		// Mode B: pre-running with no boot progress. A CHANGING BootDetail is
		// proof of a live pull and resets the clock above — progressing pulls
		// ride out the full provisioning budget.
		d.upstreamRunningAt = time.Time{}
		if d.pullStallAfter > 0 && now.Sub(d.lastDetailChange) >= d.pullStallAfter {
			return verdictDeadPull
		}
	}
	return ""
}

// bootRescue carries everything a rescue reprovision needs to re-run the
// launch, captured in Provision() at the moment the original walk succeeded.
// In-memory only by design (see the package comment above).
type bootRescue struct {
	req          ProvisionRequest          // engine-level request, for fresh candidate matching
	provReq      provider.ProvisionRequest // provider request as sent; the walk re-stamps per-attempt fields
	walk         provisionWalkParams
	attemptsLeft int
}

// rescueOutcome is attemptBootRescue's tri-state result.
type rescueOutcome int

const (
	// rescueLaunched: a replacement upstream is provisioned and the row is
	// re-bound — keep polling against the returned provider/instance.
	rescueLaunched rescueOutcome = iota
	// rescueFailed: no replacement could be placed — fail the launch now
	// (the stalled upstream is already torn down).
	rescueFailed
	// rescueAborted: the row left its mid-provision state concurrently
	// (terminate/error won the race) — exit the poll loop without touching it.
	rescueAborted
)

// stallErrorReason renders the customer-visible error reason for a stall that
// could not be rescued. Wording is deliberately platform-side and
// upstream-name-free (INFPRIV): it lands in instances.error_reason, which is
// surfaced to customers as status_reason.
func stallErrorReason(verdict string, rescueTried bool) string {
	var what string
	switch verdict {
	case verdictTunnelStall:
		what = "provisioning stalled: the instance started but never became reachable"
	case verdictDeadPull:
		what = "provisioning stalled: environment setup stopped making progress"
	case verdictUpstreamTerminal:
		what = "provisioning failed: the instance could not be started"
	case verdictGPUCheckFailed:
		what = "provisioning failed: the assigned GPU failed its startup compute check and was released"
	case verdictGPUCheckEnvironment:
		what = "provisioning failed: the selected environment's framework cannot use this GPU"
	default:
		what = "provisioning stalled: the instance stopped making boot progress"
	}
	if rescueTried {
		return fmt.Sprintf("%s; relaunch on replacement capacity also failed", what)
	}
	return what
}

// appendProviderExclusion appends a provider name to an exclusion list unless
// it is already present (case-insensitive, matching the candidate gate).
func appendProviderExclusion(excluded []string, name string) []string {
	if providerExcluded(excluded, name) {
		return excluded
	}
	return append(excluded, name)
}

// providerExcluded reports whether name is on the exclusion list,
// case-insensitively (registry names are lowercase by convention, but the
// candidate gates around it — PinnedProvider — already match fold-safely).
func providerExcluded(excluded []string, name string) bool {
	for _, n := range excluded {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// rescueEventTypeRescued is the v87 instance_events type carrying the rescue
// audit row. ADMIN-ONLY: its metadata names upstream providers/ids and carries
// wholesale costs, so db.ListInstanceEventsByOrg (the customer catch-up feed)
// filters it out and nothing publishes it to SSE or the instance.* webhooks.
const rescueEventTypeRescued = "rescued"

// rescueEventMetadata builds the 'rescued' audit row's metadata: the BEFORE and
// AFTER of the upstream swap plus the retail rate held constant across it.
//
// This exists because the rebind is DESTRUCTIVE to the evidence — afterwards
// the instances row describes only the replacement, so neither the original
// placement nor the size of the subsidy the rescue accepted (retail vs. the new
// wholesale cost) can be reconstructed from the row. Pure function: no I/O, no
// engine state, so the field list is table-driven testable.
//
// old may be nil (never on the live path, but the caller's emit is best-effort
// and must not be able to panic inside a completed rescue) — the record then
// degrades to its new-side half rather than blowing up.
func rescueEventMetadata(verdict string, old *db.Instance, newProvider, newUpstreamID string, newUpstreamPrice, retailPrice float64) json.RawMessage {
	var oldProvider, oldUpstreamID string
	var oldUpstreamPrice float64
	if old != nil {
		oldProvider = old.UpstreamProvider
		oldUpstreamID = old.UpstreamID
		oldUpstreamPrice = old.UpstreamPricePerHour
	}
	raw, err := json.Marshal(map[string]any{
		"verdict":                     verdict,
		"old_provider":                oldProvider,
		"old_upstream_id":             oldUpstreamID,
		"old_upstream_price_per_hour": oldUpstreamPrice,
		"new_provider":                newProvider,
		"new_upstream_id":             newUpstreamID,
		"new_upstream_price_per_hour": newUpstreamPrice,
		"retail_price_per_hour":       retailPrice,
	})
	if err != nil {
		// Only reachable if a plain string/float map stops marshalling, i.e.
		// never. Return valid-but-empty JSON so the insert still records THAT a
		// rescue happened rather than failing the (best-effort) write.
		return json.RawMessage(`{}`)
	}
	return raw
}

// recordBootFailure appends the POST-PLACEMENT half of a launch to the v112
// attempt ledger: an upstream that was accepted by its provider and then died
// before it ever ran.
//
// Called at the VERDICT site, before the "do we still have a rescue?" guard, so
// a death is recorded whether or not a rescue followed it. RescuesRemaining on
// the row is what says which it was — a boot failure with zero rescues left is
// a different finding from one with a rescue in hand, and the guard would have
// hidden the first.
//
// KNOWN BLIND SPOT: silent for a nil rescue. The ledger context lives on
// bootRescue, so reserved-tier launches and any deployment that sets
// PROVISION_BOOT_RESCUES=0 record no post-placement rows at all — meaning the
// rescue kill-switch also turns off the telemetry that would explain why it was
// thrown. Prod defaults the knob to 1 so this is dark today, but it is a real
// edge and the reason the package header calls this a WALK census rather than a
// launch census. Closing it means threading a ledger context into
// progressStatus independently of the rescue, which is a wider change than this
// instrumentation pass.
func (e *Engine) recordBootFailure(ctx context.Context, rescue *bootRescue, instanceID string, inst *db.Instance, verdict, reason string) {
	if rescue == nil {
		return
	}
	var outcome string
	switch verdict {
	case verdictTunnelStall:
		outcome = db.AttemptOutcomeTunnelStall
	case verdictDeadPull:
		outcome = db.AttemptOutcomeDeadPull
	case verdictUpstreamTerminal:
		outcome = db.AttemptOutcomeUpstreamTerminal
	case verdictGPUCheckFailed, verdictGPUCheckEnvironment:
		outcome = db.AttemptOutcomeGPUCheckFailed
	default:
		// An unmapped verdict is a vocabulary drift, not a row worth inventing.
		return
	}

	// Which placement died. A rescue already spent means the upstream that just
	// died was itself a replacement, so the row belongs to the rescue phase —
	// otherwise a second stall would be indistinguishable from the first.
	l := rescue.walk.ledger
	phase := db.AttemptPhaseInitial
	if rescue.attemptsLeft < e.bootRescueAttempts {
		phase = db.AttemptPhaseRescue
	}
	remaining := rescue.attemptsLeft
	l.rescuesRemaining = &remaining

	row := db.ProvisionAttempt{
		InstanceID:        instanceID,
		AttemptSeq:        l.next(),
		Phase:             phase,
		Outcome:           outcome,
		GPUType:           string(rescue.req.GPUType),
		GPUCount:          rescue.req.GPUCount,
		BudgetRemainingMS: l.budgetRemainingMS(time.Now()),
		Reason:            reason,
	}
	if inst != nil {
		row.Provider = inst.UpstreamProvider
		row.Region = inst.Region
	}
	e.recordAttempt(ctx, l, row)
}

// attemptBootRescue executes one rescue: terminate the stalled upstream,
// re-match fresh inventory with the stalled host excluded (marketplace asks
// churn ~39%/3min — stale candidates are useless), re-run the candidate walk,
// and re-bind the instance row to the replacement. On rescueLaunched the
// returned provider + freshly re-read instance row replace the poll loop's
// working set; the loop's budget timer is deliberately untouched so the
// rescue inherits the remaining budget.
func (e *Engine) attemptBootRescue(ctx context.Context, instanceID string, inst *db.Instance, prov provider.Provider, rescue *bootRescue, stalledHostID, verdict string) (rescueOutcome, provider.Provider, *db.Instance) {
	rescue.attemptsLeft--
	e.logger.Warn("boot failed or stalled, attempting rescue reprovision",
		slog.String("instance_id", instanceID),
		slog.String("verdict", verdict),
		slog.String("provider", prov.Name()),
		slog.String("upstream_id", inst.UpstreamID),
		slog.String("stalled_host_id", stalledHostID),
		slog.Int("rescues_remaining", rescue.attemptsLeft),
	)

	// Stop the stalled upstream costing money BEFORE placing a replacement —
	// same ordering rule as the timeout path (BLOCK-04 / SC-3).
	e.terminateStuckUpstream(prov, inst, instanceID, "boot-stall: failed to terminate stalled upstream")

	// Never land the rescue back on the host that just proved itself broken.
	// Cheapest-first selection would otherwise re-pick it immediately (its
	// offers stay listed while it stalls). Dedupe for the multi-rescue future.
	if stalledHostID != "" {
		already := false
		for _, id := range rescue.provReq.ExcludeUpstreamHostIDs {
			if id == stalledHostID {
				already = true
				break
			}
		}
		if !already {
			rescue.provReq.ExcludeUpstreamHostIDs = append(rescue.provReq.ExcludeUpstreamHostIDs, stalledHostID)
		}
	}

	// The poll loop's ctx is context.Background(); the rescue's match + walk
	// do live upstream HTTP, so give them their own sane deadline.
	//
	// The markup freeze is installed on rctx for the same reason Provision
	// installs one (see pricingSnapshot): the walk enforces the rescue's price
	// ceiling per candidate, then the re-price below runs AFTER the walk's live
	// HTTP. Without the freeze those are two reads of a TTL cache — and this
	// path is worse than Provision's, because rctx's 2-minute deadline starts
	// before the walk, so a slow walk can leave the final read with an expired
	// ctx and silently fall back to the env markup.
	rctx, cancel := context.WithTimeout(withPricingSnapshot(ctx), 2*time.Minute)
	defer cancel()

	candidates, err := e.SelectProviderCandidates(rctx, rescue.req)
	if err != nil {
		e.logger.Warn("boot-stall rescue: no replacement candidates",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		return rescueFailed, nil, nil
	}

	// Price ceiling (price-clarity contract): a rescue must never re-bind the
	// row ABOVE the retail rate the customer already saw placed. When the
	// launch carried an explicit max_price the walk enforces it as-is (the
	// customer consented up to that cap); otherwise the persisted retail
	// becomes the cap, so a replacement can only be same-priced or cheaper.
	// A pricier-only market means the rescue fails — that is the pre-#484
	// outcome, never a silent upcharge.
	walk := rescue.walk
	if walk.maxPricePerHour == nil && inst.PricePerHour > 0 {
		ceiling := inst.PricePerHour
		walk.maxPricePerHour = &ceiling
	}
	// The card price the customer saw at LAUNCH (GPU-78) gates that launch, not
	// this recovery. The cap just installed already bounds what they pay, so a
	// stale card price could only veto a rescue without changing a cent of the
	// bill — trading a working instance for a dead one.
	walk.viewedPricePerHour = nil
	// Spend against the RESCUE allowance, not the quote-drift one. The cap
	// above is what the customer PAYS (unchanged, and never more than they
	// were quoted); this is how much we may pay to honor it. They differ
	// because a rescue excludes the stalled host — routinely the very machine
	// whose cheap ask set the row's price — so the replacement is pricier by
	// construction. Under the quote allowance (min(5%, $0.02)) nearly every
	// community rescue would price out and the customer would get a dead
	// instance instead of a working one, which is a worse outcome than eating
	// a bounded subsidy on capacity they are already waiting on.
	walk.rescue = true
	// Carry the allowance left AFTER this rescue's decrement, so every row the
	// replacement walk writes says how many more chances the launch has.
	rescuesLeft := rescue.attemptsLeft
	walk.ledger.rescuesRemaining = &rescuesLeft
	newProv, newOffering, newResult, err := e.walkProvisionCandidates(rctx, &rescue.provReq, candidates, walk)
	if err != nil {
		// The v87 'rescued' instance event records only rescues that SUCCEED,
		// so without this row a failed rescue leaves no trace at all — and the
		// failures are the half worth seeing. The walk's own rows already say
		// which candidates it tried; this one says the rescue as a whole lost.
		// rctx is safe to pass: recordAttempt detaches every ledger write from
		// the caller's deadline, so a rescue that burned its whole budget —
		// precisely the row worth keeping — still records.
		e.recordAttempt(rctx, walk.ledger, db.ProvisionAttempt{
			InstanceID:        instanceID,
			AttemptSeq:        walk.ledger.next(),
			Phase:             db.AttemptPhaseRescue,
			Outcome:           db.AttemptOutcomeRescueFailed,
			GPUType:           string(rescue.req.GPUType),
			GPUCount:          rescue.req.GPUCount,
			BudgetRemainingMS: walk.ledger.budgetRemainingMS(time.Now()),
			Reason:            err.Error(),
		})
		e.logger.Warn("boot-stall rescue: replacement walk failed",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		return rescueFailed, nil, nil
	}

	// Re-price for the replacement offering with the SAME formulas Provision
	// persists (retail incl. disk fold, under the min(quoted, actual) discipline;
	// COGS prefers the provider's reported charge, with the partner discount
	// applied) so the rate billed at running matches the placed capacity. The
	// walk already enforced the price ceiling above on every candidate.
	//
	// The nil approvedCap is deliberate, not an omission: a rescue must never
	// RAISE what the customer pays, and passing walk.maxPricePerHour here would
	// be handing billedRetail a consent cap the customer never gave for this
	// replacement. With nil, billedRetail can only keep the replacement's quoted
	// retail or lower it to retail of the ask actually bought (GPU-78) — without
	// which the overbilling this fixes would simply survive via rescues.
	retailPricePerHour := e.billedRetail(rctx, newOffering.PricePerHour, newResult.CostPerHour, newProv.Name(), walk.effectiveDisk, nil)
	upstreamPricePerHour := newResult.CostPerHour
	if upstreamPricePerHour <= 0 && rescue.req.Tier != provider.TierReserved {
		upstreamPricePerHour = newOffering.PricePerHour
	}
	if disc := e.costDiscountFor(rctx, newProv.Name()); disc > 0 && upstreamPricePerHour > 0 {
		upstreamPricePerHour *= 1.0 - disc/100.0
	}
	var diskGB *int
	if provider.ConfigurableDisk(newProv.Name()) {
		d := walk.effectiveDisk
		diskGB = &d
	}

	rebound, err := e.db.RebindInstanceUpstream(ctx, instanceID,
		newProv.Name(), newResult.UpstreamID, regionFromResult(newResult, newOffering),
		retailPricePerHour, upstreamPricePerHour, diskGB)
	if err != nil || !rebound {
		// The row moved on (concurrent terminate) or the write failed — the
		// replacement upstream is tracked by nothing, so tear it down NOW
		// (same orphan rule as the persist-failure guard in Provision).
		tctx, tcancel := context.WithTimeout(context.Background(), 30*time.Second)
		if termErr := newProv.Terminate(tctx, newResult.UpstreamID, rescue.provReq.Region); termErr != nil {
			e.logger.Error("CRITICAL: orphaned upstream after boot-stall rebind failure",
				slog.String("instance_id", instanceID),
				slog.String("upstream_id", newResult.UpstreamID),
				slog.Any("rebind_error", err),
				slog.String("terminate_error", termErr.Error()),
			)
		}
		tcancel()
		if err != nil {
			return rescueFailed, nil, nil
		}
		return rescueAborted, nil, nil
	}

	// Audit the swap while `inst` still holds the PRE-rebind placement — this is
	// the last moment the old provider/upstream id/wholesale cost exist anywhere
	// (the GetInstance below re-reads the row as the replacement). BEST-EFFORT
	// by construction: the rescue has already committed, the replacement is live
	// and billing-safe, so a failed audit insert is logged and stepped over
	// exactly like the terminated-event write in engine.go. Never published to
	// SSE or the instance.* webhooks — the metadata is admin-only.
	rescuedEvent := &db.InstanceEvent{
		InstanceID: instanceID,
		OrgID:      inst.OrgID,
		EventType:  rescueEventTypeRescued,
		Metadata: rescueEventMetadata(verdict, inst, newProv.Name(), newResult.UpstreamID,
			upstreamPricePerHour, retailPricePerHour),
	}
	// Short deadline so a wedged DB connection cannot stall the rescue loop at
	// a best-effort write (the surrounding poll ctx has no deadline).
	evCtx, evCancel := context.WithTimeout(ctx, 5*time.Second)
	if evErr := e.db.CreateInstanceEvent(evCtx, rescuedEvent); evErr != nil {
		e.logger.Warn("boot-stall rescue: audit event write failed",
			slog.String("instance_id", instanceID),
			slog.String("error", evErr.Error()),
		)
	}
	evCancel()

	fresh, err := e.db.GetInstance(ctx, instanceID)
	if err != nil {
		// The rebind committed; the loop just can't refresh its row copy.
		// Patch the in-memory copy so subsequent polls/terminates target the
		// replacement upstream, never the dead one.
		e.logger.Warn("boot-stall rescue: re-read after rebind failed, patching in-memory row",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		patched := *inst
		patched.UpstreamProvider = newProv.Name()
		patched.UpstreamID = newResult.UpstreamID
		patched.UpstreamIP = nil
		patched.Region = regionFromResult(newResult, newOffering)
		fresh = &patched
	}

	if e.onStatusChange != nil {
		// The rebind reset a booting row to provisioning; let SSE reflect it.
		e.onStatusChange(instanceID, StateProvisioning)
	}
	e.logger.Info("boot-stall rescue launched replacement upstream",
		slog.String("instance_id", instanceID),
		slog.String("provider", newProv.Name()),
		slog.String("upstream_id", newResult.UpstreamID),
		slog.String("verdict", verdict),
	)
	return rescueLaunched, newProv, fresh
}

// bootStallDetectorFor gates the watchdog for one poll session: only non-
// template instances on a provider that declares BootRescueCapability, and
// only when at least one stall leg is enabled. Returns nil when the watchdog
// must not run — existing behavior is then byte-for-byte unchanged.
//
// With the startup GPU check enforced, the bootstrap holds the ready callback
// until the check finishes, and the tunnel-stall clock (upstream running to
// callback accepted) keeps running meanwhile. The window is widened by the
// check's maximum delay so enforcing the check cannot by itself turn a healthy
// slow boot into a stall verdict and a rescue. Shadow mode runs the check
// after the callback and needs no widening.
func (e *Engine) bootStallDetectorFor(prov provider.Provider, inst *db.Instance, rescue *bootRescue) *bootStallDetector {
	if rescue == nil || inst.TemplateID != nil {
		return nil
	}
	br, ok := prov.(provider.BootRescueCapability)
	if !ok || !br.SupportsBootRescue() {
		return nil
	}
	if e.tunnelStallAfter <= 0 && e.pullStallAfter <= 0 {
		return nil
	}
	tunnelStallAfter := e.tunnelStallAfter
	if tunnelStallAfter > 0 && e.gpuCheckMode == "enforce" {
		tunnelStallAfter += tunnel.GPUCheckEnforceMaxDelay
	}
	// A fleet-probe launch runs the on-host probe between the container
	// coming up and the ready callback, so the "running but no callback" leg
	// gets the probe's time on top; otherwise a slow probe download would read
	// as a tunnel stall on a host a customer's launch would have finished on.
	if rescue.req.FleetProbe && tunnelStallAfter > 0 {
		tunnelStallAfter += FleetProbeBootAllowance
	}
	return newBootStallDetector(tunnelStallAfter, e.pullStallAfter, time.Now())
}
