package provision

import (
	"context"
	"errors"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// v82 provision-failure ledger — the engine half.
//
// The ledger exists because the Key Metrics failures panel used to count rows in
// `instances` with status='error', which cannot see a launch that was rejected
// BEFORE any instance row existed. Those pre-instance rejections are exactly the
// "no upstream had capacity" case — the sharpest unmet-demand signal we have —
// so these tests pin three properties:
//
//  1. a supply rejection is recorded EXACTLY ONCE, with instance_id empty
//     (→ SQL NULL, the pre-instance marker) and stock_related true;
//  2. a POLICY rejection records nothing, so the panel stays a supply signal and
//     is not diluted by cap/key/config denials;
//  3. a failing ledger write never changes the provisioning outcome.

// TestProvision_RecordsPreInstanceStockFailure: a stub with zero offerings drives
// SelectProviderCandidates to ErrNoProvider — the classic "nothing to place on".
// Exactly one event must land, carrying the request's GPU type/count/org.
func TestProvision_RecordsPreInstanceStockFailure(t *testing.T) {
	// Zero offerings → no candidate matches → ErrNoProvider.
	stub := &stubProvider{name: "stub"}
	fake := fakeWithOneKey()
	e := newProvisionUnitEngine(stub, fake)

	req := onDemandRequest()
	req.GPUCount = 4

	_, err := e.Provision(context.Background(), req)
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("expected ErrNoProvider from a zero-offering registry, got: %v", err)
	}

	if len(fake.recordedFailures) != 1 {
		t.Fatalf("expected exactly 1 ledger event for one user-facing failure, got %d: %+v",
			len(fake.recordedFailures), fake.recordedFailures)
	}
	ev := fake.recordedFailures[0]
	if ev.GPUType != string(req.GPUType) {
		t.Errorf("event GPUType = %q, want %q", ev.GPUType, req.GPUType)
	}
	if ev.GPUCount != req.GPUCount {
		t.Errorf("event GPUCount = %d, want %d (a failed 4-GPU ask is a bigger miss than a 1-GPU ask)", ev.GPUCount, req.GPUCount)
	}
	if ev.OrgID != req.OrgID {
		t.Errorf("event OrgID = %q, want %q", ev.OrgID, req.OrgID)
	}
	if ev.InstanceID != "" {
		t.Errorf("event InstanceID = %q, want empty — a pre-instance rejection has no instance row (NULL is the marker)", ev.InstanceID)
	}
	if !ev.StockRelated {
		t.Error("event StockRelated = false, want true — ErrNoProvider means we had nothing to give")
	}
	if ev.Reason == "" {
		t.Error("event Reason is empty; want the engine's error text for operator debugging")
	}
}

// TestProvision_RecordsWalkExhaustionAsStockFailure: selection found candidates
// but every provider attempt refused with ErrNoCapacity. No instance row is ever
// created on that path, so without the ledger this failure is invisible — and it
// is the single most useful sourcing signal on the panel.
func TestProvision_RecordsWalkExhaustionAsStockFailure(t *testing.T) {
	stub := onDemandStub("up-1", "us-west")
	stub.provisionErr = provider.ErrNoCapacity
	fake := fakeWithOneKey()
	e := newProvisionUnitEngine(stub, fake)

	_, err := e.Provision(context.Background(), onDemandRequest())
	if err == nil {
		t.Fatal("expected Provision to fail when every candidate reports no capacity")
	}
	if !errors.Is(err, provider.ErrNoCapacity) {
		t.Fatalf("expected the walk error to wrap ErrNoCapacity, got: %v", err)
	}

	if len(fake.recordedFailures) != 1 {
		t.Fatalf("expected exactly 1 ledger event (one user-facing failure, not one per attempt), got %d",
			len(fake.recordedFailures))
	}
	ev := fake.recordedFailures[0]
	if !ev.StockRelated {
		t.Error("walk exhaustion on ErrNoCapacity must be StockRelated=true — that IS unmet demand")
	}
	if ev.InstanceID != "" {
		t.Errorf("event InstanceID = %q, want empty — the walk fails before CreateInstance", ev.InstanceID)
	}
}

// TestProvision_WalkExhaustionNonCapacityIsNotStockRelated: candidates existed
// and all refused, but for a NON-capacity reason (a broken adapter, bad
// credentials). The failure is still recorded — it is a real launch failure —
// but must NOT be counted as unmet demand, or a credential outage would read as
// a signal to go buy GPUs.
func TestProvision_WalkExhaustionNonCapacityIsNotStockRelated(t *testing.T) {
	stub := onDemandStub("up-1", "us-west")
	stub.provisionErr = errors.New("upstream returned 500: internal error")
	fake := fakeWithOneKey()
	e := newProvisionUnitEngine(stub, fake)

	if _, err := e.Provision(context.Background(), onDemandRequest()); err == nil {
		t.Fatal("expected Provision to fail when the only candidate errors")
	}

	if len(fake.recordedFailures) != 1 {
		t.Fatalf("expected exactly 1 ledger event, got %d", len(fake.recordedFailures))
	}
	if fake.recordedFailures[0].StockRelated {
		t.Error("a non-capacity provider error must be StockRelated=false — it is a break, not a stock-out")
	}
}

// TestProvision_PolicyRejectionRecordsNothing: an org with no SSH keys is
// refused with ErrSSHKeysNotFound. That is a policy/config denial, not a supply
// failure, and recording it would dilute the panel's whole purpose.
func TestProvision_PolicyRejectionRecordsNothing(t *testing.T) {
	stub := onDemandStub("up-1", "us-west")
	fake := &fakeEngineDB{} // no SSH keys at all
	e := newProvisionUnitEngine(stub, fake)

	_, err := e.Provision(context.Background(), onDemandRequest())
	if !errors.Is(err, ErrSSHKeysNotFound) {
		t.Fatalf("expected ErrSSHKeysNotFound with no keys seeded, got: %v", err)
	}

	if len(fake.recordedFailures) != 0 {
		t.Fatalf("policy rejection must record NO ledger event, got %d: %+v",
			len(fake.recordedFailures), fake.recordedFailures)
	}
}

// TestProvision_LedgerWriteFailureIsHarmless: the ledger insert fails. The
// customer-facing outcome must be byte-identical — recording is best-effort, and
// a metrics table must never be able to change what a launch returns.
func TestProvision_LedgerWriteFailureIsHarmless(t *testing.T) {
	stub := &stubProvider{name: "stub"} // zero offerings → ErrNoProvider
	fake := fakeWithOneKey()
	fake.recordProvisionFailureErr = errors.New("ledger table missing")
	e := newProvisionUnitEngine(stub, fake)

	_, err := e.Provision(context.Background(), onDemandRequest())
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("a failing ledger write changed the provisioning outcome: got %v, want ErrNoProvider", err)
	}
	if len(fake.recordedFailures) != 1 {
		t.Fatalf("expected the write to still be attempted once, got %d", len(fake.recordedFailures))
	}
}

// TestRecordProvisionFailure_NilDBIsSafe: the helper nil-guards e.db so ledger
// recording can never be the thing that panics a request path. (Tested on the
// helper directly — Provision itself needs a DB long before it reaches here.)
func TestRecordProvisionFailure_NilDBIsSafe(t *testing.T) {
	e := newProvisionUnitEngine(&stubProvider{name: "stub"}, nil)
	e.db = nil

	// Must not panic.
	e.recordProvisionFailure(context.Background(), onDemandRequest(), true, "no suitable provider available")
}

// TestStockRelatedReason pins the shared classifier every write path uses. It
// matches the engine's own two supply sentinels, case-insensitively, and nothing
// else — so a broken launch is never miscounted as unmet demand.
func TestStockRelatedReason(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{ErrNoProvider.Error(), true},
		{ErrOfferingUnavailable.Error(), true},
		{"No Suitable Provider Available", true},                          // case-insensitive
		{"provision: NO LONGER AVAILABLE at launch", true},                // case-insensitive, embedded
		{"provision: all providers failed (last: x): no capacity", false}, // real but differently worded — lower bound, by design
		{"upstream returned 500: internal error", false},
		{"ssh keys not found", false},
		{"", false},
	}
	for _, c := range cases {
		if got := db.StockRelatedReason(c.reason); got != c.want {
			t.Errorf("StockRelatedReason(%q) = %v, want %v", c.reason, got, c.want)
		}
	}
}
