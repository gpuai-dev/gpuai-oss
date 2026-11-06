package db

import (
	"context"
	"log/slog"
	"time"
)

// provision_attempts.go owns the write side of the append-only PER-ATTEMPT
// provisioning ledger (v112).
//
// It sits one level below provision_failures.go. That ledger records one row
// per USER-FACING failure and says so explicitly — it is deliberately not
// written per walk attempt, because the admin Key Metrics panel reads it on a
// one-failure-one-row contract that per-attempt rows would inflate. This table
// is the finer grain that contract excludes: every candidate the engine tried,
// every rescue it spent, and the reason each one resolved the way it did.
//
// The two tables answer different questions and neither replaces the other:
// v82 answers "how many launches failed, for what GPU, for whom"; v112 answers
// "which of the three retry paths ran out, and did it run out of attempts, of
// budget, or of candidates it was allowed to pay for".
//
// SCOPE — this is a WALK census, not a launch census. Rows cover candidate
// selection and everything downstream of it: the walk itself, the rescues, and
// the post-placement boot verdicts. They do NOT cover the pre-walk stages of
// Provision that can refuse a launch on their own — environment resolution,
// FRP port allocation, CreateInstance, and the spend/burn-cap denials — nor,
// when rescues are disabled entirely (reserved tier, PROVISION_BOOT_RESCUES=0),
// the post-placement half. Computing "failures by outcome" off this table alone
// therefore has a denominator that silently excludes those classes; the v82
// ledger remains the count of user-facing failures.
//
// PRIVACY (INFPRIV-06): Provider names an upstream supplier by definition and
// Reason carries raw adapter error text that can name one too. Both are
// BACKEND-ONLY. Nothing that reads this table may select either into a customer
// browser, and v1 has no API, admin endpoint or frontend at all — it is read
// with SQL via the in-VPC task.

// provisionAttemptReasonMaxLen bounds the stored reason, matching the v82
// ledger. Adapter errors can carry an entire upstream response body; the column
// is for operator debugging, not archival.
const provisionAttemptReasonMaxLen = 500

// provisionAttemptRetention is how long attempt rows are kept.
//
// Deliberately shorter than the v82 ledger's 365 days. That ledger is sourcing
// history — "what did we fail to supply last year" is a real question against
// it. This one is a diagnostic instrument for tuning the retry paths, and its
// row count is a multiple of the launch rate rather than of the failure rate, so
// it trades depth for staying small.
const provisionAttemptRetention = 90 * 24 * time.Hour

// Provisioning phases. A launch's initial candidate walk and every rescue that
// follows it share one instance_id; Phase says which produced a given row.
const (
	// AttemptPhaseInitial is the candidate walk Provision runs itself.
	AttemptPhaseInitial = "initial"
	// AttemptPhaseRescue is a replacement walk run by attemptBootRescue, after
	// a stall verdict or an upstream-terminal report killed a placed upstream.
	AttemptPhaseRescue = "rescue"
)

// Attempt outcomes. TYPED at the write site rather than parsed out of Reason at
// read time, for the same purpose the v82 ledger types StockRelated: the
// classification is pinned to what the engine actually decided, not to whatever
// error text happens to look like months later.
//
// The split matters more here than the values do. Today every early exit from
// the candidate walk reaches the caller as one string — "provision: all
// providers failed" — and the four ways a walk can end early want four
// different fixes: a cap that is too low is a config change, a walk that broke
// on the customer's price ceiling is a product decision about what we will
// spend to rescue a launch, and a placed-then-died upstream is supplier work.
const (
	// --- attempt-level: a candidate was reached and resolved ---

	// AttemptOutcomePlaced: the upstream accepted the create call. Not yet a
	// successful launch — the box still has to boot, and the post-placement
	// outcomes below are what happens when it does not.
	AttemptOutcomePlaced = "placed"
	// AttemptOutcomeProviderRejected: the adapter's Provision returned an
	// error. The walk continues to the next candidate.
	AttemptOutcomeProviderRejected = "provider_rejected"
	// AttemptOutcomeBootstrapRender: the candidate needed a different frpc
	// transport and re-rendering or re-staging the bootstrap failed, so the
	// candidate was skipped rather than launched with the wrong config.
	AttemptOutcomeBootstrapRender = "bootstrap_render"
	// AttemptOutcomeOSUnavailable: a raw_vm launch could not resolve this
	// provider's native image slug for the requested OS — manifest drift
	// between selection and attempt — so the candidate was skipped rather than
	// booted on an OS the customer did not ask for. Typed rather than folded
	// into provider_rejected because the provider never heard from us.
	AttemptOutcomeOSUnavailable = "os_unavailable"

	// --- attempt-level: placed, then died before running ---
	// These reuse the engine's existing stall-verdict vocabulary verbatim
	// (verdictTunnelStall / verdictDeadPull / verdictUpstreamTerminal) so the
	// ledger and the customer-visible error reason never drift apart.

	// AttemptOutcomeTunnelStall: upstream reached running but the bootstrap
	// never phoned home.
	AttemptOutcomeTunnelStall = "tunnel_stall"
	// AttemptOutcomeDeadPull: upstream sat pre-running with image-pull progress
	// frozen.
	AttemptOutcomeDeadPull = "dead_pull"
	// AttemptOutcomeUpstreamTerminal: the provider itself reported the
	// pre-running upstream terminated or errored. No judgment involved.
	AttemptOutcomeUpstreamTerminal = "upstream_terminal"
	// AttemptOutcomeGPUCheckFailed: the upstream booted and the bootstrap ran,
	// but the startup GPU compute check failed in enforce mode. Reason carries
	// the bootstrap's sanitized explanation.
	AttemptOutcomeGPUCheckFailed = "gpu_check_failed"

	// --- walk-level: the walk ended without reaching another candidate ---
	// Provider is NULL on these rows.

	// AttemptOutcomeNoCandidates: matching produced nothing to try.
	AttemptOutcomeNoCandidates = "no_candidates"
	// AttemptOutcomeAttemptsExhausted: the walk spent every attempt its CAP
	// allowed while candidates were still in the list. This is a cap set too
	// low — a config fix, and the single cheapest thing this table can find.
	AttemptOutcomeAttemptsExhausted = "attempts_exhausted"
	// AttemptOutcomeCandidatesExhausted: the walk tried every candidate it HAD
	// and they all refused. Deliberately a different outcome from the cap case
	// above: raising the cap cannot help here, and conflating the two would
	// make a supply problem look like a tuning problem. The two columns do
	// distinguish them, but re-deriving that at read time is exactly what
	// typing an outcome at the write site exists to avoid.
	AttemptOutcomeCandidatesExhausted = "candidates_exhausted"
	// AttemptOutcomePriceCap: the next candidate's retail price exceeded the
	// customer's max_price_per_hour. Candidates are sorted ascending, so no
	// later one could have rescued it either.
	AttemptOutcomePriceCap = "price_cap"
	// AttemptOutcomeViewedPriceCap: the next candidate exceeded the card price
	// the customer actually saw (GPU-78) — the silent-upcharge guard.
	AttemptOutcomeViewedPriceCap = "viewed_price_cap"
	// AttemptOutcomeCostCeiling: the adapter declined to buy above the cost
	// ceiling once it re-searched live inventory.
	AttemptOutcomeCostCeiling = "cost_ceiling"
	// AttemptOutcomeRescueFailed: a rescue walk placed nothing. The v87
	// 'rescued' instance event records only rescues that SUCCEED, so without
	// this row a failed rescue is invisible — which is the half worth seeing.
	AttemptOutcomeRescueFailed = "rescue_failed"
)

// ProvisionAttempt is one provisioning attempt, or one walk that ended without
// reaching a candidate.
//
// Every pointer field is genuinely OPTIONAL and inserts as SQL NULL when nil,
// rather than as a zero that reads back as a measurement. The distinction is
// load-bearing for BudgetRemainingMS in particular: 0 means "the attempt began
// with no budget left", which is a finding, and NULL means "not measured here",
// which is not.
//
// Provider is empty on walk-level rows — no candidate was reached, so naming one
// would be a fiction.
type ProvisionAttempt struct {
	InstanceID string
	OrgID      string
	AttemptSeq int
	Phase      string
	Outcome    string

	Provider      string
	GPUType       string
	GPUCount      int
	Region        string
	CapacityClass string

	CandidatesMatched *int
	AttemptsAllowed   *int
	RescuesRemaining  *int
	BudgetRemainingMS *int64
	DurationMS        *int64

	Reason string
}

// Terminal reports whether this outcome ends the launch rather than continuing
// to another candidate. Used to decide when to run the retention prune, so a
// 3-attempt launch costs one range delete instead of three.
//
// AttemptOutcomePlaced is NOT terminal: the create succeeded but the box still
// has to boot, and a stall verdict can reopen the launch into a rescue walk.
func (a ProvisionAttempt) Terminal() bool {
	switch a.Outcome {
	case AttemptOutcomeNoCandidates,
		AttemptOutcomeAttemptsExhausted,
		AttemptOutcomeCandidatesExhausted,
		AttemptOutcomePriceCap,
		AttemptOutcomeViewedPriceCap,
		AttemptOutcomeCostCeiling,
		AttemptOutcomeRescueFailed:
		return true
	default:
		return false
	}
}

// RecordProvisionAttempt appends one attempt row, then — on a terminal outcome
// only — opportunistically prunes past the retention horizon.
//
// BEST-EFFORT, exactly like RecordProvisionFailure: this rides the customer's
// provisioning hot path, so callers log the returned error and drop it. It must
// never fail, slow, or change the outcome of a launch. A nil Pool records
// nothing instead of panicking, so a DB-less engine (tests, degraded boot)
// behaves as it did before this table existed.
//
// The prune's error is swallowed rather than returned: retention is
// housekeeping, and failing to trim must never make a recorded attempt look
// unrecorded.
func (p *Pool) RecordProvisionAttempt(ctx context.Context, a ProvisionAttempt) error {
	if p == nil || p.pool == nil {
		return nil
	}

	gpuCount := a.GPUCount
	if gpuCount < 1 {
		gpuCount = 1
	}

	// NULLIF maps the zero value to SQL NULL for the optional text columns; the
	// pointer columns carry their own nil. See the struct comment for why the
	// difference matters on BudgetRemainingMS.
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO provision_attempts
			(instance_id, org_id, attempt_seq, phase, outcome, provider,
			 gpu_type, gpu_count, region, capacity_class,
			 candidates_matched, attempts_allowed, rescues_remaining,
			 budget_remaining_ms, duration_ms, reason)
		VALUES
			(NULLIF($1, ''), NULLIF($2, ''), $3, $4, $5, NULLIF($6, ''),
			 $7, $8, NULLIF($9, ''), NULLIF($10, ''),
			 $11, $12, $13,
			 $14, $15, NULLIF($16, ''))`,
		a.InstanceID, a.OrgID, a.AttemptSeq, a.Phase, a.Outcome, a.Provider,
		a.GPUType, gpuCount, a.Region, a.CapacityClass,
		a.CandidatesMatched, a.AttemptsAllowed, a.RescuesRemaining,
		a.BudgetRemainingMS, a.DurationMS, truncateAttemptReason(a.Reason),
	); err != nil {
		return err
	}

	if !a.Terminal() {
		return nil
	}
	if _, err := p.pool.Exec(ctx,
		`DELETE FROM provision_attempts WHERE occurred_at < $1`,
		time.Now().Add(-provisionAttemptRetention),
	); err != nil {
		slog.Warn("provision attempt ledger: retention prune failed", "error", err)
	}
	return nil
}

// truncateAttemptReason clips a reason to provisionAttemptReasonMaxLen RUNES
// (not bytes), so a cut never lands mid-UTF-8 and stores a broken sequence.
//
// Separate from provision_failures.go's truncateReason despite the identical
// body: the two ledgers own their own column bounds, and sharing the helper
// would silently couple v112's limit to a future change in v82's.
func truncateAttemptReason(reason string) string {
	runes := []rune(reason)
	if len(runes) <= provisionAttemptReasonMaxLen {
		return reason
	}
	return string(runes[:provisionAttemptReasonMaxLen])
}
