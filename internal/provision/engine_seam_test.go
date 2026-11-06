package provision

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeEngineDB is an in-memory test double satisfying the engineDB interface
// (engine.go). It exists so engine-side failure modes that would otherwise
// require a live Postgres become unit-testable by injecting it into the
// Engine.db field:
//
//   - createInstanceErr lets a test force CreateInstance to fail (BLOCK-02:
//     the engine must terminate the just-provisioned upstream when the DB
//     insert fails).
//   - getInstance is a scriptable hook for GetInstance so the timeout poll
//     loop (BLOCK-04) can be driven without real rows.
//
// Every other method returns a zero value (and nil error) — downstream plans
// add hooks here as they need them. PgxPool returns nil: the FRP port-tx is
// exercised by the real-DB harness, not this fake.
type fakeEngineDB struct {
	// createInstanceErr, if non-nil, is returned by CreateInstance.
	createInstanceErr error
	// createdInstances records every instance passed to CreateInstance
	// (recorded even when createInstanceErr is returned).
	createdInstances []*db.Instance
	// stagedBootstraps records every script passed to StageInstanceBootstrap,
	// in call order.
	stagedBootstraps []string
	// getInstance, if non-nil, backs GetInstance; otherwise GetInstance
	// returns (nil, nil).
	getInstance func(id string) (*db.Instance, error)
	// sshKeys, if set, is returned by both GetSSHKeysByOrgID and GetSSHKeysByIDs
	// so a Provision driven through the fake passes the "at least one SSH key"
	// gate. Added in Plan 02.
	sshKeys []db.SSHKey
	// updateInstanceStatus, if non-nil, backs UpdateInstanceStatus; otherwise it
	// returns (false, nil). The progressStatus timeout test (Plan 03) needs the
	// creating->provisioning transition to report updated=true so the poll loop
	// is actually entered (the default false makes progressStatus early-exit).
	updateInstanceStatus func(id, fromStatus, toStatus string) (bool, error)
	// onSetInstanceError, if non-nil, is invoked by SetInstanceError. Plan 03's
	// timeout test uses it to record that the error state was set AND its order
	// relative to the provider Terminate (proving Terminate fires first).
	onSetInstanceError func(instanceID, reason string)
	// setInstanceError, if non-nil, fully backs SetInstanceError (return
	// values included). The GPU-45 webhook tests use it to script
	// updated=false (already-error/terminated row) and prove instance.failed
	// is NOT emitted. When nil, SetInstanceError reports updated=true (the
	// live-row default) after invoking onSetInstanceError.
	setInstanceError func(instanceID, reason string) (bool, error)
	// terminateInstance, if non-nil, backs TerminateInstance; otherwise it
	// returns (false, nil). The GPU-45 webhook tests script the guarded flip
	// (true on the first terminate, false on a repeat) to prove
	// instance.terminated emits exactly once.
	terminateInstance func(instanceID string) (bool, error)
	// statusReasons captures SetInstanceStatusReason writes, keyed by instance
	// ID (first write wins, mirroring the production "error_reason IS NULL"
	// guard). setStatusReasonErr forces the write to fail.
	statusReasons      map[string]string
	setStatusReasonErr error
	// listStuckProvisions, if non-nil, backs ListStuckProvisions; otherwise it
	// returns nil. The reaper test (AUDIT-05-001/002) sets it to a stuck row.
	listStuckProvisions func(cutoff time.Time) ([]db.Instance, error)
	// recordedFailures captures every v82 provision-failure ledger write the
	// engine makes, in order. The ledger tests assert BOTH that a stock-out is
	// recorded once and that a policy rejection records nothing.
	recordedFailures []db.ProvisionFailureEvent
	// recordProvisionFailureErr, if non-nil, is returned by
	// RecordProvisionFailure (the event is still captured) so a test can prove
	// recording failures never break the provisioning path.
	recordProvisionFailureErr error
	// recordedAttempts captures every v112 per-attempt ledger write, in order.
	// The attempt-ledger tests assert the OUTCOME on each row: the whole value
	// of the table is that the four ways a walk can end early are four distinct
	// outcomes rather than one "all providers failed" string.
	recordedAttempts []db.ProvisionAttempt
	// recordProvisionAttemptErr, if non-nil, is returned by
	// RecordProvisionAttempt (the row is still captured) so a test can prove a
	// failing telemetry write never changes a launch.
	recordProvisionAttemptErr error
	// rebindInstanceUpstream, if non-nil, backs RebindInstanceUpstream
	// (boot-stall rescue); otherwise it returns (false, nil) — the "lost the
	// race" outcome, which is the conservative default (the rescue tears its
	// replacement upstream down rather than leaving an untracked orphan).
	rebindInstanceUpstream func(instanceID, upstreamProvider, upstreamID, region string, pricePerHour, upstreamPricePerHour float64, diskGB *int) (bool, error)
	// createInstanceEvent, if non-nil, backs CreateInstanceEvent; otherwise the
	// write is a silent success. The GPU-89 rescue-audit tests use it both to
	// capture the emitted 'rescued' row and to script an insert failure, proving
	// the emit is best-effort (a failed audit write never changes an outcome).
	createInstanceEvent   func(event *db.InstanceEvent) error
	runningInstancesByOrg func(orgID string) ([]db.Instance, error)

	// Per-instance runtime bound hooks (schema v92, GPU-60).
	// listInstancesDueForAutoTerminate scripts the due set (default: none, so
	// no existing test's sweep does any work); claimInstanceAutoTerminate
	// scripts the atomic claim (default: won) and autoTerminateClaims records
	// every claim attempt in order.
	listInstancesDueForAutoTerminate func(now time.Time, limit int) ([]db.Instance, error)
	claimInstanceAutoTerminate       func(instanceID string) (bool, error)
	rearmInstanceAutoTerminate       func(instanceID string, at time.Time) (bool, error)
	autoTerminateClaims              []string
	// autoTerminateRearms records every RearmInstanceAutoTerminate call (id +
	// the deadline restored) so a test can prove a failed terminate puts the
	// bound back verbatim rather than dropping or extending it.
	autoTerminateRearms []rearmCall
}

// rearmCall is one RearmInstanceAutoTerminate invocation.
type rearmCall struct {
	instanceID string
	at         time.Time
}

// Compile-time guard: if fakeEngineDB drifts from the engineDB interface the
// build fails here, the same mechanism that guards *db.Pool.
var _ engineDB = (*fakeEngineDB)(nil)

// --- Instance lifecycle ---

func (f *fakeEngineDB) CreateInstance(ctx context.Context, inst *db.Instance) error {
	f.createdInstances = append(f.createdInstances, inst)
	return f.createInstanceErr
}

func (f *fakeEngineDB) StageInstanceBootstrap(ctx context.Context, instanceID, script string) error {
	// Recorded (not discarded): container/stage-0-loader providers FETCH this
	// staged copy rather than reading ProvisionRequest.StartupScript, so it is
	// the only thing that proves what such an instance actually boots with.
	f.stagedBootstraps = append(f.stagedBootstraps, script)
	return nil
}

func (f *fakeEngineDB) GetInstance(ctx context.Context, instanceID string) (*db.Instance, error) {
	if f.getInstance != nil {
		return f.getInstance(instanceID)
	}
	return nil, nil
}

func (f *fakeEngineDB) UpdateInstanceStatus(ctx context.Context, instanceID, fromStatus, toStatus string) (bool, error) {
	if f.updateInstanceStatus != nil {
		return f.updateInstanceStatus(instanceID, fromStatus, toStatus)
	}
	return false, nil
}

func (f *fakeEngineDB) SetInstanceRunning(ctx context.Context, instanceID, upstreamIP string) (updated bool, prevStatus string, err error) {
	return false, "", nil
}

func (f *fakeEngineDB) SetInstanceUpstreamIP(ctx context.Context, instanceID, upstreamIP string) error {
	return nil
}

func (f *fakeEngineDB) SetInstanceSSHPort(ctx context.Context, instanceID string, port int) error {
	return nil
}

func (f *fakeEngineDB) SetInstanceError(ctx context.Context, instanceID, reason string) (bool, error) {
	if f.onSetInstanceError != nil {
		f.onSetInstanceError(instanceID, reason)
	}
	if f.setInstanceError != nil {
		return f.setInstanceError(instanceID, reason)
	}
	// updated=true mirrors the production guard's behavior for a live row —
	// the common case every failure-path test drives.
	return true, nil
}

func (f *fakeEngineDB) RebindInstanceUpstream(ctx context.Context, instanceID, upstreamProvider, upstreamID, region string, pricePerHour, upstreamPricePerHour float64, diskGB *int) (bool, error) {
	if f.rebindInstanceUpstream != nil {
		return f.rebindInstanceUpstream(instanceID, upstreamProvider, upstreamID, region, pricePerHour, upstreamPricePerHour, diskGB)
	}
	return false, nil
}

func (f *fakeEngineDB) TerminateInstance(ctx context.Context, instanceID string) (bool, error) {
	if f.terminateInstance != nil {
		return f.terminateInstance(instanceID)
	}
	return false, nil
}

func (f *fakeEngineDB) ListRunningInstancesByOrg(ctx context.Context, orgID string) ([]db.Instance, error) {
	if f.runningInstancesByOrg != nil {
		return f.runningInstancesByOrg(orgID)
	}
	return nil, nil
}

// --- Abuse-control reads (Plan 48-02) ---

// --- Phase-53 fraud-control config reads (Plan 53-02) ---
//
// Each delegates to the matching *OrDefault scaffold accessor (Plan 53-01) so
// the fail-safe tightest default is shared with TestGraduatedTierScaffold.

func (f *fakeEngineDB) ListStuckProvisions(ctx context.Context, cutoff time.Time) ([]db.Instance, error) {
	if f.listStuckProvisions != nil {
		return f.listStuckProvisions(cutoff)
	}
	return nil, nil
}

// --- Per-instance runtime bound (schema v92, GPU-60) ---

// ListInstancesDueForAutoTerminate returns the scripted due set, or nothing —
// the default keeps every existing Provision/reaper test's tick free of
// auto-terminate work.
func (f *fakeEngineDB) ListInstancesDueForAutoTerminate(ctx context.Context, now time.Time, limit int) ([]db.Instance, error) {
	if f.listInstancesDueForAutoTerminate != nil {
		return f.listInstancesDueForAutoTerminate(now, limit)
	}
	return nil, nil
}

// RearmInstanceAutoTerminate mirrors the production guard closely enough to be
// useful: it records the call and reports rearmed=true, the "row was still
// running with no deadline" outcome the retry path depends on.
func (f *fakeEngineDB) RearmInstanceAutoTerminate(ctx context.Context, instanceID string, at time.Time) (bool, error) {
	f.autoTerminateRearms = append(f.autoTerminateRearms, rearmCall{instanceID: instanceID, at: at})
	if f.rearmInstanceAutoTerminate != nil {
		return f.rearmInstanceAutoTerminate(instanceID, at)
	}
	return true, nil
}

// ClaimInstanceAutoTerminate defaults to WINNING the claim (true) so a scripted
// due row actually gets swept; the exactly-once tests script it explicitly.
// Every call is recorded so a test can prove the sweep claims before acting and
// claims exactly once per instance.
func (f *fakeEngineDB) ClaimInstanceAutoTerminate(ctx context.Context, instanceID string, now time.Time) (bool, error) {
	f.autoTerminateClaims = append(f.autoTerminateClaims, instanceID)
	if f.claimInstanceAutoTerminate != nil {
		return f.claimInstanceAutoTerminate(instanceID)
	}
	return true, nil
}

// --- Instance event log ---

func (f *fakeEngineDB) CreateInstanceEvent(ctx context.Context, event *db.InstanceEvent) error {
	if f.createInstanceEvent != nil {
		return f.createInstanceEvent(event)
	}
	return nil
}

// --- Provision-failure ledger (v82) ---

func (f *fakeEngineDB) RecordProvisionFailure(ctx context.Context, ev db.ProvisionFailureEvent) error {
	// Captured BEFORE the scripted error so a test can assert both that the
	// event was attempted and that the failing write was harmless.
	f.recordedFailures = append(f.recordedFailures, ev)
	return f.recordProvisionFailureErr
}

// --- Per-attempt ledger (v112) ---

func (f *fakeEngineDB) RecordProvisionAttempt(ctx context.Context, a db.ProvisionAttempt) error {
	// Captured BEFORE the scripted error, same discipline as the v82 fake: a
	// test must be able to assert both that the row was attempted and that the
	// failing write was harmless.
	f.recordedAttempts = append(f.recordedAttempts, a)
	return f.recordProvisionAttemptErr
}

// --- Billing sessions ---

// --- Spending-limit reads ---

// SetInstanceStatusReason records the customer-facing reason a deliberate stop
// wrote, so tests can assert the explanation attached to each stopped row.
func (f *fakeEngineDB) SetInstanceStatusReason(ctx context.Context, instanceID, reason string) error {
	if f.statusReasons == nil {
		f.statusReasons = map[string]string{}
	}
	if _, exists := f.statusReasons[instanceID]; !exists {
		f.statusReasons[instanceID] = reason
	}
	return f.setStatusReasonErr
}

// --- SSH key resolution ---

func (f *fakeEngineDB) GetSSHKeysByIDs(ctx context.Context, ids []string) ([]db.SSHKey, error) {
	return f.sshKeys, nil
}

func (f *fakeEngineDB) GetSSHKeysByOrgID(ctx context.Context, orgID string) ([]db.SSHKey, error) {
	return f.sshKeys, nil
}

func (f *fakeEngineDB) ListSSHKeysByOrgAndUser(ctx context.Context, orgID, userID string) ([]db.SSHKey, error) {
	// Mirror the real query: only keys the user owns. Fixture keys without a
	// UserID exercise the engine's org-wide fallback path.
	var out []db.SSHKey
	for _, k := range f.sshKeys {
		if k.UserID == userID {
			out = append(out, k)
		}
	}
	return out, nil
}

// --- Raw pool access (FRP port-allocation tx) ---

func (f *fakeEngineDB) PgxPool() *pgxpool.Pool {
	return nil
}

// TestEngineDB_FakeInjection proves the engineDB seam accepts an in-memory
// fake: an Engine can be constructed with fakeEngineDB in its db field, the
// compile-time guard holds, and the configurable hooks are wired through. Depth
// (asserting terminate-on-failure, synchronous billing session, fast timeout)
// lands in Plans 02/03/06 — this is the smoke test that the seam itself works.
func TestEngineDB_FakeInjection(t *testing.T) {
	sentinel := errors.New("create instance boom")
	fake := &fakeEngineDB{createInstanceErr: sentinel}

	e := &Engine{db: fake}

	// The seam accepts the fake (this would not compile otherwise).
	if e.db == nil {
		t.Fatal("expected Engine.db to be the injected fakeEngineDB, got nil")
	}

	// The CreateInstance hook is wired through the interface.
	if err := e.db.CreateInstance(context.Background(), &db.Instance{}); !errors.Is(err, sentinel) {
		t.Fatalf("expected injected createInstanceErr to surface, got %v", err)
	}

}
