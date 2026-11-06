package provision

import (
	"context"
	"errors"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// v112 per-attempt ledger — the engine half.
//
// The table exists because ~17% of prod launches fail (GPU-115) and the number
// cannot be decomposed: the caller sees one string, "all providers failed", for
// four structurally different endings. These tests pin the property that makes
// the table worth having — that each ending writes its OWN outcome — plus the
// two safety properties every best-effort telemetry write has to hold.

// outcomesOf flattens recorded rows to their outcomes, which is what nearly
// every assertion below is really about.
func outcomesOf(rows []db.ProvisionAttempt) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Outcome
	}
	return out
}

// TestAttemptLedger_RecordsPlacedOnSuccess: a launch that places must leave a
// `placed` row naming the supplier it landed on.
//
// `placed` is deliberately NOT "launched" — the create was accepted and the box
// still has to boot. The post-placement outcomes are recorded by the poll path,
// which is why a launch can legitimately show `placed` followed by a stall.
func TestAttemptLedger_RecordsPlacedOnSuccess(t *testing.T) {
	stub := onDemandStub("up-1", "us-west")
	fake := fakeWithOneKey()
	e := newProvisionUnitEngine(stub, fake)

	if _, err := e.Provision(context.Background(), onDemandRequest()); err != nil {
		t.Fatalf("expected the launch to place, got: %v", err)
	}

	var placed []db.ProvisionAttempt
	for _, r := range fake.recordedAttempts {
		if r.Outcome == db.AttemptOutcomePlaced {
			placed = append(placed, r)
		}
	}
	if len(placed) != 1 {
		t.Fatalf("expected exactly 1 placed row, got %d (all: %v)",
			len(placed), outcomesOf(fake.recordedAttempts))
	}
	row := placed[0]
	if row.Provider != "stub" {
		t.Errorf("placed row Provider = %q, want %q — a placement names the supplier it landed on", row.Provider, "stub")
	}
	if row.InstanceID == "" {
		t.Error("placed row InstanceID is empty; rows must group by launch")
	}
	if row.Phase != db.AttemptPhaseInitial {
		t.Errorf("placed row Phase = %q, want %q", row.Phase, db.AttemptPhaseInitial)
	}
	if row.AttemptSeq != 1 {
		t.Errorf("placed row AttemptSeq = %d, want 1 — the first attempt of a launch", row.AttemptSeq)
	}
	if row.CandidatesMatched == nil || *row.CandidatesMatched != 1 {
		t.Errorf("placed row CandidatesMatched = %v, want 1", row.CandidatesMatched)
	}
	if row.AttemptsAllowed == nil || *row.AttemptsAllowed != 3 {
		t.Errorf("placed row AttemptsAllowed = %v, want 3 (the walk cap in force)", row.AttemptsAllowed)
	}
}

// TestAttemptLedger_NoCandidates: selection produced nothing to try.
//
// The v82 ledger records this too, but only for the two supply sentinels. The
// attempt ledger records every selection failure — a config denial that quietly
// kills launches is exactly what this table is for.
func TestAttemptLedger_NoCandidates(t *testing.T) {
	stub := &stubProvider{name: "stub"} // zero offerings → ErrNoProvider
	fake := fakeWithOneKey()
	e := newProvisionUnitEngine(stub, fake)

	if _, err := e.Provision(context.Background(), onDemandRequest()); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("expected ErrNoProvider, got: %v", err)
	}

	got := outcomesOf(fake.recordedAttempts)
	if len(got) != 1 || got[0] != db.AttemptOutcomeNoCandidates {
		t.Fatalf("outcomes = %v, want exactly [%s]", got, db.AttemptOutcomeNoCandidates)
	}
	if fake.recordedAttempts[0].Provider != "" {
		t.Errorf("no_candidates row Provider = %q, want empty — no candidate was reached, so naming one is a fiction",
			fake.recordedAttempts[0].Provider)
	}
}

// TestAttemptLedger_CandidatesExhausted: one candidate, and it refuses.
//
// This must NOT record attempts_exhausted. The walk had 1 candidate against a
// cap of 3, so it ran out of SUPPLY, not of attempts — and raising the cap
// cannot help. Conflating the two is precisely the misreading the split
// outcome exists to prevent.
func TestAttemptLedger_CandidatesExhausted(t *testing.T) {
	stub := onDemandStub("up-1", "us-west")
	stub.provisionErr = provider.ErrNoCapacity
	fake := fakeWithOneKey()
	e := newProvisionUnitEngine(stub, fake)

	if _, err := e.Provision(context.Background(), onDemandRequest()); err == nil {
		t.Fatal("expected the launch to fail when the only candidate has no capacity")
	}

	got := outcomesOf(fake.recordedAttempts)
	want := []string{db.AttemptOutcomeProviderRejected, db.AttemptOutcomeCandidatesExhausted}
	if len(got) != len(want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("outcomes = %v, want %v", got, want)
		}
	}

	rejected := fake.recordedAttempts[0]
	if rejected.Provider != "stub" {
		t.Errorf("provider_rejected row Provider = %q, want %q", rejected.Provider, "stub")
	}
	if rejected.Reason == "" {
		t.Error("provider_rejected row Reason is empty; want the adapter's error for operator debugging")
	}

	// Sequence is per-launch and monotonic across rows, which is what lets a
	// reader reconstruct the launch in order.
	if a, b := fake.recordedAttempts[0].AttemptSeq, fake.recordedAttempts[1].AttemptSeq; a != 1 || b != 2 {
		t.Errorf("AttemptSeq = (%d, %d), want (1, 2)", a, b)
	}
}

// TestAttemptLedger_AttemptsExhausted: MORE candidates than the walk is allowed
// to try, and every attempt refuses.
//
// This is the finding the whole table is built to surface — the walk gave up
// with usable supply still in the list — and it is the cheapest thing to fix,
// because the limit it hit is a constant rather than the market. It must be
// distinguishable from candidates_exhausted without re-reading two columns,
// which is why the outcomes are split at the write site.
func TestAttemptLedger_AttemptsExhausted(t *testing.T) {
	stub := onDemandStub("up-1", "us-west")
	stub.provisionErr = provider.ErrNoCapacity
	fake := fakeWithOneKey()
	e := newProvisionUnitEngine(stub, fake)

	// Four candidates against a cap of three.
	candidates := make([]ProviderCandidate, 4)
	for i := range candidates {
		candidates[i] = ProviderCandidate{Prov: stub, Offering: stub.offerings[0]}
	}
	seq := 0
	provReq := provider.ProvisionRequest{GPUType: provider.GPUTypeA10080GB, GPUCount: 1}

	if _, _, _, err := e.walkProvisionCandidates(
		context.Background(), &provReq, candidates,
		provisionWalkParams{
			instanceID: "gpu-test",
			ledger:     attemptLedgerCtx{orgID: "org-1", seq: &seq},
		},
	); err == nil {
		t.Fatal("expected the walk to fail when every candidate has no capacity")
	}

	got := outcomesOf(fake.recordedAttempts)
	if len(got) != 4 {
		t.Fatalf("outcomes = %v, want 3 rejections + 1 terminal row", got)
	}
	for i := 0; i < 3; i++ {
		if got[i] != db.AttemptOutcomeProviderRejected {
			t.Fatalf("outcomes = %v, want the first three to be %s", got, db.AttemptOutcomeProviderRejected)
		}
	}
	final := fake.recordedAttempts[3]
	if final.Outcome != db.AttemptOutcomeAttemptsExhausted {
		t.Errorf("terminal outcome = %q, want %q — 4 candidates against a cap of 3 is the CAP running out, not supply",
			final.Outcome, db.AttemptOutcomeAttemptsExhausted)
	}
	if final.CandidatesMatched == nil || *final.CandidatesMatched != 4 {
		t.Errorf("CandidatesMatched = %v, want 4", final.CandidatesMatched)
	}
	if final.AttemptsAllowed == nil || *final.AttemptsAllowed != 3 {
		t.Errorf("AttemptsAllowed = %v, want 3", final.AttemptsAllowed)
	}
	if final.Provider != "" {
		t.Errorf("terminal row Provider = %q, want empty — no single candidate is implicated", final.Provider)
	}
	if final.OrgID != "org-1" {
		t.Errorf("terminal row OrgID = %q, want org-1 — the ledger ctx attributes every row", final.OrgID)
	}
}

// TestAttemptLedger_PreflightPriceCap: the cheapest matched candidate costs
// more than the customer approved, so the PRE-FLIGHT check refuses before the
// walk ever runs.
//
// This row is the difference between the price_cap outcome being measurable and
// being decorative. The walk's own guard fires only on a FALLBACK candidate —
// candidates[0] has by then already passed pre-flight — so without a row here
// the common shape (priced out on the cheapest candidate) would record nothing
// and the analytics would read price caps as a rare cause when they may be the
// leading one.
func TestAttemptLedger_PreflightPriceCap(t *testing.T) {
	stub := onDemandStub("up-1", "us-west") // offering is $1.50/hr
	fake := fakeWithOneKey()
	e := newProvisionUnitEngine(stub, fake)

	req := onDemandRequest()
	maxPrice := 0.50
	req.MaxPricePerHour = &maxPrice

	var priceErr *PriceExceededError
	_, err := e.Provision(context.Background(), req)
	if !errors.As(err, &priceErr) {
		t.Fatalf("expected PriceExceededError below the cap, got: %v", err)
	}

	got := outcomesOf(fake.recordedAttempts)
	if len(got) != 1 || got[0] != db.AttemptOutcomePriceCap {
		t.Fatalf("outcomes = %v, want exactly [%s] — a pre-flight refusal must leave a row",
			got, db.AttemptOutcomePriceCap)
	}
	row := fake.recordedAttempts[0]
	if row.Provider != "stub" {
		t.Errorf("price_cap row Provider = %q, want %q — which supplier priced us out IS the finding",
			row.Provider, "stub")
	}
	if row.CandidatesMatched != nil {
		t.Errorf("price_cap row CandidatesMatched = %v, want nil — selection succeeded, so a count here invites reading this as an exhaustion",
			row.CandidatesMatched)
	}
	if row.Reason == "" {
		t.Error("price_cap row Reason is empty; want the two prices that were compared")
	}
}

// TestAttemptLedger_SurvivesExpiredCallerContext pins the detach.
//
// Ledger writes ride contexts that are themselves the measurement: the rescue
// walk runs under a two-minute budget, and a rescue that burns all of it is the
// row most worth keeping. Before the detach, pgx saw the dead context and
// dropped exactly those rows, keeping only the fast failures — a silent bias
// toward the outcomes we least need to see.
//
// Without this test the suite stays green if someone later unwraps the
// WithoutCancel, because every other test here runs under a live
// context.Background().
func TestAttemptLedger_SurvivesExpiredCallerContext(t *testing.T) {
	stub := onDemandStub("up-1", "us-west")
	fake := fakeWithOneKey()
	e := newProvisionUnitEngine(stub, fake)

	dead, cancel := context.WithCancel(context.Background())
	cancel() // the caller's budget is already gone

	seq := 0
	e.recordAttempt(dead, attemptLedgerCtx{orgID: "org-1", seq: &seq}, db.ProvisionAttempt{
		InstanceID: "gpu-test",
		AttemptSeq: 1,
		Phase:      db.AttemptPhaseRescue,
		Outcome:    db.AttemptOutcomeRescueFailed,
		GPUType:    string(provider.GPUTypeA10080GB),
		GPUCount:   1,
	})

	if len(fake.recordedAttempts) != 1 {
		t.Fatalf("expected the row to be written under a cancelled caller context, got %d rows — "+
			"a write that reports on a budget must not be killed by that budget",
			len(fake.recordedAttempts))
	}
}

// TestAttemptLedger_TerminalClassification guards the prune trigger: only an
// outcome that ENDS a launch may be terminal. `placed` in particular must not
// be — the create succeeded but the box still has to boot, and a stall verdict
// can reopen the launch into a rescue walk.
func TestAttemptLedger_TerminalClassification(t *testing.T) {
	terminal := []string{
		db.AttemptOutcomeNoCandidates,
		db.AttemptOutcomeAttemptsExhausted,
		db.AttemptOutcomeCandidatesExhausted,
		db.AttemptOutcomePriceCap,
		db.AttemptOutcomeViewedPriceCap,
		db.AttemptOutcomeCostCeiling,
		db.AttemptOutcomeRescueFailed,
	}
	notTerminal := []string{
		db.AttemptOutcomePlaced,
		db.AttemptOutcomeProviderRejected,
		db.AttemptOutcomeBootstrapRender,
		db.AttemptOutcomeOSUnavailable,
		db.AttemptOutcomeTunnelStall,
		db.AttemptOutcomeDeadPull,
		db.AttemptOutcomeUpstreamTerminal,
	}
	for _, o := range terminal {
		if !(db.ProvisionAttempt{Outcome: o}).Terminal() {
			t.Errorf("outcome %q: Terminal() = false, want true", o)
		}
	}
	for _, o := range notTerminal {
		if (db.ProvisionAttempt{Outcome: o}).Terminal() {
			t.Errorf("outcome %q: Terminal() = true, want false — it does not end the launch", o)
		}
	}
}

// TestAttemptLedger_WriteFailureIsHarmless: a failing ledger write must never
// change a launch. Telemetry that can break provisioning is worse than no
// telemetry, and this is the property that makes it safe to call on the hot
// path at all.
func TestAttemptLedger_WriteFailureIsHarmless(t *testing.T) {
	stub := onDemandStub("up-1", "us-west")
	fake := fakeWithOneKey()
	fake.recordProvisionAttemptErr = errors.New("ledger table is gone")
	e := newProvisionUnitEngine(stub, fake)

	if _, err := e.Provision(context.Background(), onDemandRequest()); err != nil {
		t.Fatalf("a failing ledger write must not fail the launch, got: %v", err)
	}
	if len(fake.recordedAttempts) == 0 {
		t.Error("expected the write to have been ATTEMPTED even though it errors")
	}
}

// TestAttemptLedger_DisabledByDefaultForDirectWalks: a walk run without a
// ledger context records nothing rather than panicking on the nil counter.
// Instrumenting a walk must not become a precondition for running one — every
// pre-existing unit test calls walkProvisionCandidates with a zero-value
// provisionWalkParams.
func TestAttemptLedger_DisabledByDefaultForDirectWalks(t *testing.T) {
	stub := onDemandStub("up-1", "us-west")
	fake := fakeWithOneKey()
	e := newProvisionUnitEngine(stub, fake)

	provReq := provider.ProvisionRequest{GPUType: provider.GPUTypeA10080GB, GPUCount: 1}
	candidates := []ProviderCandidate{{Prov: stub, Offering: stub.offerings[0]}}

	if _, _, _, err := e.walkProvisionCandidates(
		context.Background(), &provReq, candidates, provisionWalkParams{instanceID: "gpu-test"},
	); err != nil {
		t.Fatalf("walk failed: %v", err)
	}
	if len(fake.recordedAttempts) != 0 {
		t.Errorf("expected no ledger rows from an uninstrumented walk, got %v",
			outcomesOf(fake.recordedAttempts))
	}
}
