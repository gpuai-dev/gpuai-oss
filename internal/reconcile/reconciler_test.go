package reconcile

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// ourInstanceID is a canonical "gpu-<8hex>" instance ID — the real format
// provision.generateInstanceID mints and every adapter sets as the upstream
// Name — so the reconciler's parseOurInstanceID safety guard (util.go)
// recognizes the instance as ours and the guarded action path runs. A name
// that isn't this exact format is skipped entirely (tenant-isolation backstop,
// Anti-Pattern in 32-RESEARCH).
const ourInstanceID = "gpu-207db644"

const testProviderName = "fake"

// fakeReconcilerDB satisfies the private reconcilerDB read seam with a static
// row set, letting RunOnce be exercised without a live PostgreSQL pool.
type fakeReconcilerDB struct {
	rows []db.Instance
	err  error

	// fresh models the POST-snapshot point read (GetInstance), keyed by
	// instance_id. A miss returns db.ErrNotFound (= genuine orphan) so existing
	// orphan tests — which expect termination — keep working unchanged. Populate
	// an entry to simulate the TOCTOU race: a live row that appeared AFTER the
	// ListReconcilableInstances snapshot the orphan pass indexed.
	fresh    map[string]db.Instance
	freshErr error
}

func (f *fakeReconcilerDB) ListReconcilableInstances(_ context.Context, _ time.Duration) ([]db.Instance, error) {
	return f.rows, f.err
}

func (f *fakeReconcilerDB) GetInstance(_ context.Context, instanceID string) (*db.Instance, error) {
	if f.freshErr != nil {
		return nil, f.freshErr
	}
	if inst, ok := f.fresh[instanceID]; ok {
		return &inst, nil
	}
	return nil, db.ErrNotFound
}

// fakeProvider implements provider.Provider. Only GetStatus / Terminate /
// ListProvisioned matter to the reconciler; the rest are inert. Terminate
// calls are recorded so the tests can assert enforce-vs-dry-run behavior.
type fakeProvider struct {
	mu sync.Mutex

	provisioned []provider.ProvisionedInstance
	// statusByUpstream maps upstream_id -> (status, err) returned by GetStatus.
	statusByUpstream map[string]fakeStatus
	terminateCalls   []string // upstream_ids passed to Terminate
}

type fakeStatus struct {
	status *provider.InstanceStatus
	err    error
}

func (p *fakeProvider) Name() string { return testProviderName }

func (p *fakeProvider) ListAvailable(_ context.Context) ([]provider.GPUOffering, error) {
	return nil, nil
}

func (p *fakeProvider) Provision(_ context.Context, _ provider.ProvisionRequest) (*provider.ProvisionResult, error) {
	return nil, errors.New("not used in reconciler tests")
}

func (p *fakeProvider) GetStatus(_ context.Context, upstreamID, _ string) (*provider.InstanceStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.statusByUpstream[upstreamID]
	if !ok {
		// Default: report running so a row that isn't explicitly a ghost
		// is treated as live.
		return &provider.InstanceStatus{UpstreamID: upstreamID, Status: "running"}, nil
	}
	return s.status, s.err
}

func (p *fakeProvider) Terminate(_ context.Context, upstreamID, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terminateCalls = append(p.terminateCalls, upstreamID)
	return nil
}

func (p *fakeProvider) ListProvisioned(_ context.Context) ([]provider.ProvisionedInstance, error) {
	return p.provisioned, nil
}

func (p *fakeProvider) terminatedUpstreams() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.terminateCalls))
	copy(out, p.terminateCalls)
	return out
}

// fakeTerminator records instance_ids passed to Terminate (the GHOST action
// path goes through the Terminator, NOT the provider).
type fakeTerminator struct {
	mu    sync.Mutex
	calls []string
}

func (t *fakeTerminator) Terminate(_ context.Context, instanceID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, instanceID)
	return nil
}

func (t *fakeTerminator) called() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.calls))
	copy(out, t.calls)
	return out
}

// newTestReconciler builds a Reconciler wired to fakes via the package-private
// fields (this test lives in package reconcile, so it can set them directly).
func newTestReconciler(fdb reconcilerDB, prov provider.Provider, term Terminator, dryRun, ghostDryRun bool) *Reconciler {
	reg := provider.NewRegistry()
	if prov != nil {
		reg.Register(prov)
	}
	r := New(Deps{
		Registry:         reg,
		Terminator:       term,
		Interval:         time.Minute,
		RecentTermWindow: time.Hour,
		DryRun:           dryRun,
		GhostDryRun:      ghostDryRun,
	})
	// Swap the concrete *db.Pool (nil here) for the fake read seam.
	r.db = fdb
	return r
}

// TestRunOnce_Orphan_EnforceTerminates: an upstream named with our UUID but
// with no matching DB row is a pure ORPHAN; in enforce mode the reconciler
// terminates it directly via the provider.
func TestRunOnce_Orphan_EnforceTerminates(t *testing.T) {
	fdb := &fakeReconcilerDB{rows: nil} // no DB rows -> the live upstream is orphaned
	prov := &fakeProvider{
		provisioned: []provider.ProvisionedInstance{
			{Name: ourInstanceID, UpstreamID: "up-1", Region: "us", Status: "running"},
		},
	}
	r := newTestReconciler(fdb, prov, &fakeTerminator{}, false /* dryRun */, false /* ghostDryRun */)

	r.RunOnce(context.Background())

	got := prov.terminatedUpstreams()
	if len(got) != 1 || got[0] != "up-1" {
		t.Fatalf("enforce ORPHAN: provider.Terminate calls = %v; want [up-1]", got)
	}
}

// TestRunOnce_Orphan_DryRunLogsOnly: same orphan, but dry-run -> no Terminate.
func TestRunOnce_Orphan_DryRunLogsOnly(t *testing.T) {
	fdb := &fakeReconcilerDB{rows: nil}
	prov := &fakeProvider{
		provisioned: []provider.ProvisionedInstance{
			{Name: ourInstanceID, UpstreamID: "up-1", Region: "us", Status: "running"},
		},
	}
	r := newTestReconciler(fdb, prov, &fakeTerminator{}, true /* dryRun */, false)

	r.RunOnce(context.Background())

	if got := prov.terminatedUpstreams(); len(got) != 0 {
		t.Fatalf("dry-run ORPHAN: provider.Terminate calls = %v; want none", got)
	}
}

// TestRunOnce_Orphan_TOCTOU exercises the orphan-pass freshness re-check that
// closes the 2026-06-26 prod incident: ListReconcilableInstances snapshots the
// DB at tick start, but ListProvisioned runs seconds later. A provision that
// created its upstream resource (and row) in that gap looks orphaned against the
// stale snapshot. The reaper must re-read fresh and skip a live row — never
// terminate a just-provisioned customer instance.
func TestRunOnce_Orphan_TOCTOU(t *testing.T) {
	mkProv := func() *fakeProvider {
		return &fakeProvider{
			provisioned: []provider.ProvisionedInstance{
				{Name: ourInstanceID, UpstreamID: "up-1", Region: "us", Status: "running"},
			},
		}
	}

	// The race itself: snapshot missed the row, fresh read shows it live -> SKIP.
	t.Run("skips_when_fresh_live_row_appears", func(t *testing.T) {
		fdb := &fakeReconcilerDB{
			rows: nil, // tick-start snapshot missed it (provision committed after)
			fresh: map[string]db.Instance{
				ourInstanceID: {InstanceID: ourInstanceID, UpstreamProvider: testProviderName, UpstreamID: "up-1", Status: "creating"},
			},
		}
		prov := mkProv()
		r := newTestReconciler(fdb, prov, &fakeTerminator{}, false /* dryRun */, false)
		r.RunOnce(context.Background())

		if got := prov.terminatedUpstreams(); len(got) != 0 {
			t.Fatalf("TOCTOU: Terminate calls = %v; want none (fresh live row must be skipped)", got)
		}
		if snap := r.Snapshot(); snap.Orphans != 0 || snap.OrphansActed != 0 {
			t.Fatalf("TOCTOU: snapshot Orphans=%d OrphansActed=%d; want 0/0 (not an orphan)", snap.Orphans, snap.OrphansActed)
		}
	})

	// Re-check itself errors -> termination is destructive, so we must NOT act.
	t.Run("skips_when_recheck_errors", func(t *testing.T) {
		fdb := &fakeReconcilerDB{rows: nil, freshErr: errors.New("db timeout")}
		prov := mkProv()
		r := newTestReconciler(fdb, prov, &fakeTerminator{}, false /* dryRun */, false)
		r.RunOnce(context.Background())

		if got := prov.terminatedUpstreams(); len(got) != 0 {
			t.Fatalf("recheck error: Terminate calls = %v; want none (unconfirmable state must not terminate)", got)
		}
	})

	// Genuine orphan (fresh read also finds no row) -> still terminates.
	t.Run("terminates_when_fresh_read_absent", func(t *testing.T) {
		fdb := &fakeReconcilerDB{rows: nil} // fresh map empty -> GetInstance returns ErrNotFound
		prov := mkProv()
		r := newTestReconciler(fdb, prov, &fakeTerminator{}, false /* dryRun */, false)
		r.RunOnce(context.Background())

		if got := prov.terminatedUpstreams(); len(got) != 1 || got[0] != "up-1" {
			t.Fatalf("genuine orphan: Terminate calls = %v; want [up-1]", got)
		}
	})

	// Row exists but is terminated -> upstream outlived it; kill it.
	t.Run("terminates_when_fresh_row_terminated", func(t *testing.T) {
		fdb := &fakeReconcilerDB{
			rows: nil,
			fresh: map[string]db.Instance{
				ourInstanceID: {InstanceID: ourInstanceID, UpstreamProvider: testProviderName, UpstreamID: "up-1", Status: "terminated"},
			},
		}
		prov := mkProv()
		r := newTestReconciler(fdb, prov, &fakeTerminator{}, false /* dryRun */, false)
		r.RunOnce(context.Background())

		if got := prov.terminatedUpstreams(); len(got) != 1 || got[0] != "up-1" {
			t.Fatalf("terminated-row orphan: Terminate calls = %v; want [up-1]", got)
		}
	})
}

// TestRunOnce_Ghost_StaysDryRunInEnforce: an active DB row whose upstream
// returns not-found is a GHOST. With ghostDryRun=true the GHOST action is
// suppressed EVEN when the global dryRun is false (D-04 / Pitfall 2). Flipping
// ghostDryRun=false (dryRun still false) proves the override is what gates it.
func TestRunOnce_Ghost_StaysDryRunInEnforce(t *testing.T) {
	ghostRow := db.Instance{
		InstanceID:       ourInstanceID,
		UpstreamProvider: testProviderName,
		UpstreamID:       "up-ghost",
		Status:           "running",
		Region:           "us",
		CreatedAt:        time.Now().Add(-10 * time.Minute), // older than minInstanceAge (90s)
		UpdatedAt:        time.Now().Add(-10 * time.Minute),
	}
	makeProv := func() *fakeProvider {
		return &fakeProvider{
			statusByUpstream: map[string]fakeStatus{
				"up-ghost": {status: nil, err: errors.New("instance not found")},
			},
		}
	}

	// Enforce globally, but GHOST suppressed: terminator must NOT be called.
	t.Run("ghost_suppressed_when_ghostDryRun_true", func(t *testing.T) {
		term := &fakeTerminator{}
		r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{ghostRow}}, makeProv(), term, false /* dryRun */, true /* ghostDryRun */)
		r.RunOnce(context.Background())
		if got := term.called(); len(got) != 0 {
			t.Fatalf("ghostDryRun=true: terminator called %v; want none (GHOST must defer to health monitor)", got)
		}
	})

	// Flip ghostDryRun off (still enforce): GHOST now fires through the Terminator.
	t.Run("ghost_fires_when_ghostDryRun_false", func(t *testing.T) {
		term := &fakeTerminator{}
		r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{ghostRow}}, makeProv(), term, false /* dryRun */, false /* ghostDryRun */)
		r.RunOnce(context.Background())
		got := term.called()
		if len(got) != 1 || got[0] != ourInstanceID {
			t.Fatalf("ghostDryRun=false: terminator calls = %v; want [%s]", got, ourInstanceID)
		}
	})
}

// TestStatsSnapshot: RunOnce populates the read-only StatsSnapshot (RECON-01),
// and a Snapshot() taken before any RunOnce is the zero value. The snapshot
// mirrors the same orphan/ghost/stuck counts the local reconcileStats computes
// and reports rows_examined == the seeded DB row count.
func TestStatsSnapshot(t *testing.T) {
	// A GHOST: an active DB row whose upstream reports not-found. Older than
	// minInstanceAge so it isn't skipped as too-fresh.
	ghostRow := db.Instance{
		InstanceID:       ourInstanceID,
		UpstreamProvider: testProviderName,
		UpstreamID:       "up-ghost",
		Status:           "running",
		Region:           "us",
		CreatedAt:        time.Now().Add(-10 * time.Minute),
		UpdatedAt:        time.Now().Add(-10 * time.Minute),
	}
	prov := &fakeProvider{
		statusByUpstream: map[string]fakeStatus{
			"up-ghost": {status: nil, err: errors.New("instance not found")},
		},
	}
	// Enforce mode (dryRun=false) with ghostDryRun=false so the ghost is acted on,
	// giving a non-zero GhostsActed to assert against.
	r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{ghostRow}}, prov, &fakeTerminator{}, false /* dryRun */, false /* ghostDryRun */)

	// Pre-run: Snapshot() must be the zero value (no pass has run yet).
	if pre := r.Snapshot(); !pre.LastRunAt.IsZero() {
		t.Fatalf("pre-run Snapshot().LastRunAt = %v; want zero value", pre.LastRunAt)
	}

	r.RunOnce(context.Background())

	snap := r.Snapshot()
	if snap.LastRunAt.IsZero() {
		t.Errorf("post-run Snapshot().LastRunAt is zero; want a populated timestamp")
	}
	if snap.RowsExamined != 1 {
		t.Errorf("RowsExamined = %d; want 1 (the one seeded DB row)", snap.RowsExamined)
	}
	if snap.Ghosts != 1 {
		t.Errorf("Ghosts = %d; want 1", snap.Ghosts)
	}
	if snap.GhostsActed != 1 {
		t.Errorf("GhostsActed = %d; want 1 (enforce mode acted on the ghost)", snap.GhostsActed)
	}
	// No orphan/stuck were seeded.
	if snap.Orphans != 0 || snap.Stuck != 0 {
		t.Errorf("Orphans=%d Stuck=%d; want both 0", snap.Orphans, snap.Stuck)
	}
	// dryRun was false on this reconciler — the snapshot mirrors that.
	if snap.DryRun {
		t.Errorf("snap.DryRun = true; want false (reconciler was in enforce mode)")
	}
}

// TestStatsSnapshot_OrphanCounts cross-checks that a pure ORPHAN (no DB rows)
// surfaces in the snapshot's orphan counters and that rows_examined is 0 when
// the DB returns no rows.
func TestStatsSnapshot_OrphanCounts(t *testing.T) {
	prov := &fakeProvider{
		provisioned: []provider.ProvisionedInstance{
			{Name: ourInstanceID, UpstreamID: "up-1", Region: "us", Status: "running"},
		},
	}
	r := newTestReconciler(&fakeReconcilerDB{rows: nil}, prov, &fakeTerminator{}, false /* dryRun */, false)

	r.RunOnce(context.Background())

	snap := r.Snapshot()
	if snap.RowsExamined != 0 {
		t.Errorf("RowsExamined = %d; want 0 (no DB rows seeded)", snap.RowsExamined)
	}
	if snap.Orphans != 1 {
		t.Errorf("Orphans = %d; want 1", snap.Orphans)
	}
	if snap.OrphansActed != 1 {
		t.Errorf("OrphansActed = %d; want 1 (enforce mode terminated the orphan)", snap.OrphansActed)
	}
}

// TestRunOnce_StuckTerminate_Refires: a DB row marked terminated whose upstream
// is still alive (matched via ListProvisioned by upstream_id) is STUCK-TERMINATE;
// in enforce mode the reconciler re-fires the upstream Terminate.
func TestRunOnce_StuckTerminate_Refires(t *testing.T) {
	stuckRow := db.Instance{
		InstanceID:       ourInstanceID,
		UpstreamProvider: testProviderName,
		UpstreamID:       "up-stuck",
		Status:           "terminated",
		Region:           "us",
		CreatedAt:        time.Now().Add(-30 * time.Minute),
		UpdatedAt:        time.Now().Add(-30 * time.Minute),
	}
	prov := &fakeProvider{
		// Upstream still alive despite the DB row being terminated.
		provisioned: []provider.ProvisionedInstance{
			{Name: ourInstanceID, UpstreamID: "up-stuck", Region: "us", Status: "running"},
		},
	}
	r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{stuckRow}}, prov, &fakeTerminator{}, false /* dryRun */, true /* ghostDryRun: irrelevant for STUCK */)

	r.RunOnce(context.Background())

	got := prov.terminatedUpstreams()
	if len(got) != 1 || got[0] != "up-stuck" {
		t.Fatalf("enforce STUCK-TERMINATE: provider.Terminate calls = %v; want [up-stuck]", got)
	}
}

// stuckErrorRow builds a DB row parked in `error` (the engine's failed-terminate
// state) whose upstream is still alive by default (fakeProvider.GetStatus
// returns "running" for unknown upstream ids).
func stuckErrorRow() db.Instance {
	reason := "provider terminate failed: terminate: delete cluster: HTTP 409"
	return db.Instance{
		InstanceID:       ourInstanceID,
		UpstreamProvider: testProviderName,
		UpstreamID:       "up-err",
		Status:           "error",
		ErrorReason:      &reason,
		Region:           "us",
		CreatedAt:        time.Now().Add(-30 * time.Minute),
		UpdatedAt:        time.Now().Add(-30 * time.Minute),
	}
}

// TestRunOnce_StuckError_RetriesViaEngine: a DB row in `error` whose upstream is
// still alive is STUCK-ERROR — the state of a 13h production leak. In
// enforce mode the reconciler re-drives the FULL engine terminate path
// (Terminator, error→stopping is legal) so a success closes the row + billing,
// NOT a bare upstream re-fire that would leave the row parked in error.
func TestRunOnce_StuckError_RetriesViaEngine(t *testing.T) {
	term := &fakeTerminator{}
	prov := &fakeProvider{}
	r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{stuckErrorRow()}}, prov, term, false /* dryRun */, true /* ghostDryRun */)

	r.RunOnce(context.Background())

	if got := term.called(); len(got) != 1 || got[0] != ourInstanceID {
		t.Fatalf("enforce STUCK-ERROR: terminator calls = %v; want [%s] (full engine path)", got, ourInstanceID)
	}
	if got := prov.terminatedUpstreams(); len(got) != 0 {
		t.Errorf("STUCK-ERROR must not bare-fire provider.Terminate (row would stay in error); got %v", got)
	}
	if snap := r.Snapshot(); snap.Stuck != 1 {
		t.Errorf("snapshot Stuck = %d; want 1", snap.Stuck)
	}
}

// TestRunOnce_StuckError_DryRunObservesOnly: dry-run mode logs the STUCK-ERROR
// but never calls the Terminator.
func TestRunOnce_StuckError_DryRunObservesOnly(t *testing.T) {
	term := &fakeTerminator{}
	r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{stuckErrorRow()}}, &fakeProvider{}, term, true /* dryRun */, true)

	r.RunOnce(context.Background())

	if got := term.called(); len(got) != 0 {
		t.Fatalf("dry-run STUCK-ERROR must not terminate; terminator calls = %v", got)
	}
	if snap := r.Snapshot(); snap.Stuck != 1 {
		t.Errorf("dry-run must still COUNT the stuck row; snapshot Stuck = %d, want 1", snap.Stuck)
	}
}

// TestRunOnce_StuckError_GoneUpstreamIsGhostNotStuck: an `error` row whose
// upstream is already gone flows to the GHOST class, not STUCK. Inside the
// errorGhostFinalizeGrace window (this row entered error 30 min ago) it is
// still log-only — the customer's error card stays up (GPU-41).
func TestRunOnce_StuckError_GoneUpstreamIsGhostNotStuck(t *testing.T) {
	term := &fakeTerminator{}
	prov := &fakeProvider{statusByUpstream: map[string]fakeStatus{
		"up-err": {status: &provider.InstanceStatus{UpstreamID: "up-err", Status: "terminated"}},
	}}
	r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{stuckErrorRow()}}, prov, term, false, true /* ghostDryRun */)

	r.RunOnce(context.Background())

	if got := term.called(); len(got) != 0 {
		t.Fatalf("error row dead <grace is a GHOST inside its grace window, not STUCK; terminator calls = %v", got)
	}
	snap := r.Snapshot()
	if snap.Ghosts != 1 || snap.Stuck != 0 {
		t.Errorf("snapshot Ghosts=%d Stuck=%d; want 1/0", snap.Ghosts, snap.Stuck)
	}
	if snap.GhostsActed != 0 {
		t.Errorf("snapshot GhostsActed=%d; want 0 (grace window held the action back)", snap.GhostsActed)
	}
}

// agedErrorGhostRow is stuckErrorRow past the errorGhostFinalizeGrace window:
// the reaper flipped it to `error` two hours ago and its upstream is gone —
// the exact dead hand-off from the 2026-07-30 gpu-c49fe16d ticket (GPU-41).
func agedErrorGhostRow() db.Instance {
	row := stuckErrorRow()
	row.CreatedAt = time.Now().Add(-3 * time.Hour)
	row.UpdatedAt = time.Now().Add(-2 * time.Hour)
	return row
}

// TestRunOnce_ErrorGhost_FinalizedAfterGrace: past the grace window, an
// `error` row with a dead upstream is finalized through the FULL engine path
// EVEN under GhostDryRun (GPU-41) — the health monitor never scans error rows,
// so no other component can double-act, and without this nothing ever
// finalizes a reaped deploy.
func TestRunOnce_ErrorGhost_FinalizedAfterGrace(t *testing.T) {
	term := &fakeTerminator{}
	prov := &fakeProvider{statusByUpstream: map[string]fakeStatus{
		"up-err": {status: nil, err: errors.New("instance not found")},
	}}
	r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{agedErrorGhostRow()}}, prov, term, false /* dryRun */, true /* ghostDryRun */)

	r.RunOnce(context.Background())

	if got := term.called(); len(got) != 1 || got[0] != ourInstanceID {
		t.Fatalf("aged error GHOST must finalize via Terminator despite ghostDryRun; calls = %v; want [%s]", got, ourInstanceID)
	}
	if got := prov.terminatedUpstreams(); len(got) != 0 {
		t.Errorf("error GHOST must not bare-fire provider.Terminate; got %v", got)
	}
	snap := r.Snapshot()
	if snap.Ghosts != 1 || snap.GhostsActed != 1 {
		t.Errorf("snapshot Ghosts=%d GhostsActed=%d; want 1/1", snap.Ghosts, snap.GhostsActed)
	}
}

// TestRunOnce_ErrorGhost_GlobalDryRunStillObserves: the global dry-run gate
// outranks the GPU-41 error-row exemption — an aged error ghost is still
// log-only when DryRun is on.
func TestRunOnce_ErrorGhost_GlobalDryRunStillObserves(t *testing.T) {
	term := &fakeTerminator{}
	prov := &fakeProvider{statusByUpstream: map[string]fakeStatus{
		"up-err": {status: nil, err: errors.New("instance not found")},
	}}
	r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{agedErrorGhostRow()}}, prov, term, true /* dryRun */, true /* ghostDryRun */)

	r.RunOnce(context.Background())

	if got := term.called(); len(got) != 0 {
		t.Fatalf("global dry-run must suppress even aged error GHOSTs; terminator calls = %v", got)
	}
	if snap := r.Snapshot(); snap.Ghosts != 1 || snap.GhostsActed != 0 {
		t.Errorf("snapshot Ghosts=%d GhostsActed=%d; want 1/0", snap.Ghosts, snap.GhostsActed)
	}
}

// TestRunOnce_ErrorGhost_ZeroUpdatedAtStaysLogOnly: an error ghost whose
// UpdatedAt is the zero value can't be dated, so the grace window can never be
// proven elapsed — it stays log-only (conservative default).
func TestRunOnce_ErrorGhost_ZeroUpdatedAtStaysLogOnly(t *testing.T) {
	term := &fakeTerminator{}
	row := agedErrorGhostRow()
	row.UpdatedAt = time.Time{}
	prov := &fakeProvider{statusByUpstream: map[string]fakeStatus{
		"up-err": {status: nil, err: errors.New("instance not found")},
	}}
	r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{row}}, prov, term, false /* dryRun */, true /* ghostDryRun */)

	r.RunOnce(context.Background())

	if got := term.called(); len(got) != 0 {
		t.Fatalf("zero UpdatedAt must stay log-only; terminator calls = %v", got)
	}
}

// TestRunOnce_StuckStopping_UsesEnginePath: a row parked in `stopping` past the
// grace window with a live upstream re-drives the FULL engine path (Terminator)
// so the DB row reaches terminated when the upstream finally dies — the health
// monitor only scans running/booting and GHOST is log-only, so a bare upstream
// re-fire would strand the row in stopping forever.
func TestRunOnce_StuckStopping_UsesEnginePath(t *testing.T) {
	term := &fakeTerminator{}
	prov := &fakeProvider{}
	row := db.Instance{
		InstanceID:       ourInstanceID,
		UpstreamProvider: testProviderName,
		UpstreamID:       "up-stop",
		Status:           "stopping",
		Region:           "us",
		CreatedAt:        time.Now().Add(-30 * time.Minute),
		UpdatedAt:        time.Now().Add(-10 * time.Minute), // past stuckTerminationGrace
	}
	r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{row}}, prov, term, false, true)

	r.RunOnce(context.Background())

	if got := term.called(); len(got) != 1 || got[0] != ourInstanceID {
		t.Fatalf("STUCK-STOPPING: terminator calls = %v; want [%s] (full engine path)", got, ourInstanceID)
	}
	if got := prov.terminatedUpstreams(); len(got) != 0 {
		t.Errorf("STUCK-STOPPING with a wired Terminator must not also bare-fire provider.Terminate; got %v", got)
	}
}

// TestRunOnce_StuckRepeat_EscalatesAfterConsecutivePasses: the same instance
// stuck for stuckRepeatTicks consecutive passes surfaces in stuck_repeat (the
// StuckRepeat alarm feed); a pass without it resets the streak to zero.
func TestRunOnce_StuckRepeat_EscalatesAfterConsecutivePasses(t *testing.T) {
	fdb := &fakeReconcilerDB{rows: []db.Instance{stuckErrorRow()}}
	r := newTestReconciler(fdb, &fakeProvider{}, &fakeTerminator{}, false, true)

	for i := 1; i < stuckRepeatTicks; i++ {
		r.RunOnce(context.Background())
		if snap := r.Snapshot(); snap.StuckRepeat != 0 {
			t.Fatalf("pass %d: StuckRepeat = %d; want 0 (below the %d-pass threshold)", i, snap.StuckRepeat, stuckRepeatTicks)
		}
	}
	r.RunOnce(context.Background())
	if snap := r.Snapshot(); snap.StuckRepeat != 1 {
		t.Fatalf("pass %d: StuckRepeat = %d; want 1 (streak reached threshold)", stuckRepeatTicks, snap.StuckRepeat)
	}

	// Instance recovers (no rows) → streak resets, escalation clears.
	fdb.rows = nil
	r.RunOnce(context.Background())
	if snap := r.Snapshot(); snap.StuckRepeat != 0 {
		t.Fatalf("after recovery: StuckRepeat = %d; want 0", snap.StuckRepeat)
	}
}

// stoppingGhostRow is the shape that billed a real customer for 14.7 hours on
// 2026-09-09 (gpu-7d1f1488): the customer hit terminate, the row moved to
// `stopping`, the upstream actually died — and then nothing ever finalized it,
// so its billing session stayed open. Aged past stoppingGhostFinalizeGrace.
func stoppingGhostRow() db.Instance {
	return db.Instance{
		InstanceID:       ourInstanceID,
		UpstreamProvider: testProviderName,
		UpstreamID:       "up-err",
		Status:           "stopping",
		Region:           "us",
		CreatedAt:        time.Now().Add(-3 * time.Hour),
		UpdatedAt:        time.Now().Add(-2 * time.Hour),
	}
}

// TestRunOnce_StoppingGhost_FinalizedAfterGrace is the regression test for the
// billing leak. A `stopping` row whose upstream is GONE was classified GHOST,
// suppressed by ghostDryRun, and then never finalized by anything: the health
// monitor scans running/booting only, and STUCK-STOPPING covers the opposite
// case (upstream still alive). The row sat in `stopping` with an OPEN billing
// session, charging for a machine that no longer existed.
//
// It must finalize through the FULL engine path — that is what closes the
// billing session; a bare upstream re-fire would leave the row parked.
func TestRunOnce_StoppingGhost_FinalizedAfterGrace(t *testing.T) {
	term := &fakeTerminator{}
	prov := &fakeProvider{statusByUpstream: map[string]fakeStatus{
		"up-err": {status: nil, err: errors.New("instance not found")},
	}}
	r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{stoppingGhostRow()}}, prov, term, false /* dryRun */, true /* ghostDryRun */)

	r.RunOnce(context.Background())

	if got := term.called(); len(got) != 1 || got[0] != ourInstanceID {
		t.Fatalf("aged stopping GHOST must finalize via the engine Terminator despite ghostDryRun "+
			"(this is the billing leak); calls = %v; want [%s]", got, ourInstanceID)
	}
	if got := prov.terminatedUpstreams(); len(got) != 0 {
		t.Errorf("stopping GHOST must not bare-fire provider.Terminate — only the engine path closes billing; got %v", got)
	}
	if snap := r.Snapshot(); snap.Ghosts != 1 || snap.GhostsActed != 1 {
		t.Errorf("snapshot Ghosts=%d GhostsActed=%d; want 1/1", snap.Ghosts, snap.GhostsActed)
	}
}

// TestRunOnce_StoppingGhost_WithinGraceStaysLogOnly: a teardown that is merely
// in progress must not be preempted. Inside the grace the row is observed only,
// so the normal termination path keeps ownership.
func TestRunOnce_StoppingGhost_WithinGraceStaysLogOnly(t *testing.T) {
	row := stoppingGhostRow()
	row.UpdatedAt = time.Now().Add(-1 * time.Minute) // well inside the grace
	term := &fakeTerminator{}
	prov := &fakeProvider{statusByUpstream: map[string]fakeStatus{
		"up-err": {status: nil, err: errors.New("instance not found")},
	}}
	r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{row}}, prov, term, false /* dryRun */, true /* ghostDryRun */)

	r.RunOnce(context.Background())

	if got := term.called(); len(got) != 0 {
		t.Fatalf("a stopping GHOST inside the grace must stay log-only; terminator calls = %v", got)
	}
	if snap := r.Snapshot(); snap.Ghosts != 1 || snap.GhostsActed != 0 {
		t.Errorf("snapshot Ghosts=%d GhostsActed=%d; want 1/0", snap.Ghosts, snap.GhostsActed)
	}
}

// TestRunOnce_StoppingGhost_GlobalDryRunStillObserves: the global dry-run gate
// outranks this carve-out, exactly as it does for error rows.
func TestRunOnce_StoppingGhost_GlobalDryRunStillObserves(t *testing.T) {
	term := &fakeTerminator{}
	prov := &fakeProvider{statusByUpstream: map[string]fakeStatus{
		"up-err": {status: nil, err: errors.New("instance not found")},
	}}
	r := newTestReconciler(&fakeReconcilerDB{rows: []db.Instance{stoppingGhostRow()}}, prov, term, true /* dryRun */, true /* ghostDryRun */)

	r.RunOnce(context.Background())

	if got := term.called(); len(got) != 0 {
		t.Fatalf("global dry-run must suppress even an aged stopping GHOST; terminator calls = %v", got)
	}
}

// TestGhostFinalizeGrace_MatchesTheMonitorScanSet pins the invariant the whole
// carve-out rests on: a status is enforced here IF AND ONLY IF the health
// monitor never scans it. db.ListActiveInstances scans running/booting, so
// those two must defer (or the two components double-act on one upstream);
// error and stopping have no other finalizer, so they must not.
func TestGhostFinalizeGrace_MatchesTheMonitorScanSet(t *testing.T) {
	for _, s := range []string{"running", "booting"} {
		if _, enforced := ghostFinalizeGrace(s); enforced {
			t.Errorf("%q is scanned by the health monitor — enforcing it here risks a double-act (D-04)", s)
		}
	}
	for _, s := range []string{"error", "stopping"} {
		grace, enforced := ghostFinalizeGrace(s)
		if !enforced {
			t.Errorf("%q has no other finalizer — leaving it deferred parks the row and its billing session forever", s)
		}
		if grace <= 0 {
			t.Errorf("%q enforced with a non-positive grace (%v)", s, grace)
		}
	}
}
