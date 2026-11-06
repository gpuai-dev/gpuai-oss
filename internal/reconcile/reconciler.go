// Package reconcile sweeps for drift between gpu-ai's database and what
// upstream providers are actually running, then takes action to bring them
// back in sync. The goal is to prevent money leaks: every minute we delay
// catching a leaked upstream instance, the user is billed for it.
//
// Three classes of drift the reconciler watches for:
//
//  1. ORPHAN — upstream alive, no live DB row. Cause: provision succeeded
//     upstream but our DB write failed, or DB row was already marked
//     terminated even though Terminate hit the wrong region (Pitfall 10).
//     Action: terminate the upstream resource directly.
//
//  2. GHOST — DB says running/starting/stopping, upstream gone or errored.
//     Cause: spot reclamation we missed, provider-side outage, or a
//     terminate that succeeded upstream but failed locally. Action: mark
//     the DB row error/terminated and close billing so we stop charging
//     the user.
//
//  3. STUCK-TERMINATE — DB says terminated, upstream still alive. Cause:
//     the regional-routing class of bug; Terminate returned 200 but hit a
//     backend that doesn't own the machine_id. Action: re-issue Terminate.
//
// Safety guards:
//   - Only acts on upstream rows whose Name parses as a UUID (= our
//     InstanceID). Defends against operating on instances launched outside
//     gpu-ai (shared API keys with other tooling).
//   - Honors a dry-run mode that logs intended actions without taking them.
//     Recommended for the first deploy; flip via env once a tick or two
//     have been observed.
//   - Skips orphan detection for providers that return ErrNotImplemented
//     from ListProvisioned (generic adapter today). DB-side drift checks
//     still run for those providers.
package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// Terminator is the dependency reconciler uses to terminate live DB-backed
// instances (the GHOST and stuck-state cases). Implemented by *provision.Engine
// — calling Engine.Terminate runs the full local-lifecycle path (status
// transitions, billing close, audit log) without a circular import.
type Terminator interface {
	Terminate(ctx context.Context, instanceID string) error
}

// reconcilerDB is the narrow read seam the reconciler needs from *db.Pool.
// Mirrors the tickerDB pattern in internal/billing/ticker.go: a private
// interface so RunOnce can be unit-tested with a fake instead of a live
// PostgreSQL pool. Production wiring always passes a concrete *db.Pool
// (Deps.DB); the compile-time check below guards against drift.
type reconcilerDB interface {
	ListReconcilableInstances(ctx context.Context, recentWindow time.Duration) ([]db.Instance, error)
	// GetInstance is a fresh point read used as the orphan-pass TOCTOU guard:
	// ListReconcilableInstances snapshots the DB at tick start, but ListProvisioned
	// runs seconds later, so a provision that created its upstream in that gap looks
	// orphaned though its row now exists. Re-reading right before we terminate closes
	// the race. Returns db.ErrNotFound when no row exists (= a genuine orphan).
	GetInstance(ctx context.Context, instanceID string) (*db.Instance, error)
}

// Compile-time assertion that *db.Pool satisfies the read seam.
var _ reconcilerDB = (*db.Pool)(nil)

// Deps wires the reconciler. Interval defaults to 5 minutes; RecentTermWindow
// defaults to 4 hours.
type Deps struct {
	DB               *db.Pool
	Registry         *provider.Registry
	Terminator       Terminator
	Logger           *slog.Logger
	Interval         time.Duration
	RecentTermWindow time.Duration
	DryRun           bool
	// GhostDryRun keeps the GHOST pass log-only INDEPENDENTLY of the global
	// DryRun (D-04 / BLOCK-01b). When true, ORPHAN + STUCK-TERMINATE still
	// enforce (gated on DryRun alone) but GHOST never terminates — deferring
	// to the wired health monitor so the two cannot double-act (Pitfall 2).
	// Exception (GPU-41): `error`-status ghosts are enforced after
	// errorGhostFinalizeGrace even when this is true — the health monitor
	// never scans error rows, so the double-act concern cannot apply and
	// deferring would leave them un-finalized forever.
	GhostDryRun bool
}

// Reconciler runs periodic drift detection across providers.
type Reconciler struct {
	db               reconcilerDB
	registry         *provider.Registry
	terminator       Terminator
	logger           *slog.Logger
	interval         time.Duration
	recentTermWindow time.Duration
	dryRun           bool
	// ghostDryRun suppresses GHOST termination even when dryRun is false
	// (D-04), except for aged `error` rows (GPU-41). See Deps.GhostDryRun.
	ghostDryRun bool

	// snapMu guards snap — the last-pass StatsSnapshot read cross-goroutine by
	// the internal /admin reconcile panel (RECON-01). RunOnce runs on the
	// background ticker goroutine; Snapshot() is called from an HTTP handler
	// goroutine, so the read/write of snap must be synchronized (threat T-46-07).
	snapMu sync.RWMutex
	// snap is the most recent RunOnce result, exposed read-only via Snapshot().
	// Zero value until the first RunOnce completes.
	snap StatsSnapshot

	// stuckStreak counts CONSECUTIVE passes each instance_id has been classified
	// stuck (any stuck class). An id absent from a pass resets (deleted). Only
	// touched by RunOnce on the ticker goroutine — no lock needed. Feeds the
	// stuck_repeat escalation: retry-forever is the right action, but a stuck
	// that persists >= stuckRepeatTicks passes is a provider incident a human
	// must see (the alarm on the StuckRepeat metric), not just a silent loop.
	stuckStreak map[string]int
}

// StatsSnapshot is a read-only point-in-time view of the reconciler's last pass
// (RECON-01). It mirrors the local reconcileStats counters plus the tick-complete
// log line's elapsed / rows_examined / dry_run fields. Populated by RunOnce and
// served verbatim to the internal /admin reconcile panel via Snapshot().
//
// This is a READ-ONLY addition (L-03): nothing here changes the reconciler's
// action path (orphan/ghost/stuck termination). It is observability only.
type StatsSnapshot struct {
	LastRunAt    time.Time     `json:"last_run_at"`
	Elapsed      time.Duration `json:"elapsed_ms"`
	RowsExamined int           `json:"rows_examined"`
	Orphans      int           `json:"orphans"`
	OrphansActed int           `json:"orphans_acted"`
	Ghosts       int           `json:"ghosts"`
	GhostsActed  int           `json:"ghosts_acted"`
	Stuck        int           `json:"stuck"`
	StuckActed   int           `json:"stuck_acted"`
	// StuckRepeat is how many instances have been stuck for >= stuckRepeatTicks
	// consecutive passes — the not-converging escalation worth alerting on.
	StuckRepeat int  `json:"stuck_repeat"`
	DryRun      bool `json:"dry_run"`
}

const (
	defaultInterval         = 5 * time.Minute
	defaultRecentTermWindow = 4 * time.Hour

	// minInstanceAge protects newly-created rows from being misclassified
	// as ghosts: a freshly-provisioned instance hasn't propagated to the
	// upstream's listing endpoint yet, and its upstream_id may even be
	// empty for the first few seconds.
	minInstanceAge = 90 * time.Second

	// stuckTerminationGrace gives the upstream a few minutes to actually
	// kill the machine after we issued Terminate before we re-fire it.
	// Some providers tear down asynchronously.
	stuckTerminationGrace = 3 * time.Minute

	// errorGhostFinalizeGrace is how long a GHOST row already in `error` keeps
	// its customer-visible error card before the reconciler finalizes it to
	// terminated (GPU-41). Error rows are the one GHOST class no other
	// component ever finalizes — the health monitor scans running/booting only
	// — so without enforcement here a reaped deploy sits in `error` until a
	// human deletes it. The grace keeps the dashboard's red error card (with
	// error_reason) up long enough for the customer to see why their deploy
	// died; error_reason survives the transition either way.
	errorGhostFinalizeGrace = 60 * time.Minute

	// stoppingGhostFinalizeGrace is how long a GHOST row already in `stopping`
	// waits before the reconciler finalizes it. Like `error` rows, NOTHING else
	// ever finalizes them — the health monitor scans running/booting only, and
	// STUCK-STOPPING handles the opposite case (upstream still ALIVE past the
	// grace). A `stopping` row whose upstream is already GONE therefore fell
	// between the two and billed the customer forever.
	//
	// It is far shorter than the error grace because the tradeoffs invert. The
	// error grace exists to keep a customer-visible error card up long enough
	// to read; here the customer has already asked to terminate, there is no
	// card to preserve, the upstream is confirmed gone, and every extra minute
	// bills them for a machine that does not exist. The wait exists only so a
	// slow-but-working teardown finishes on its own path rather than being
	// preempted — comfortably longer than stuckTerminationGrace.
	stoppingGhostFinalizeGrace = 10 * time.Minute

	// adminTerminatingClaimWindow mirrors db.AdminTerminatingClaimWindow so the
	// in-loop defense-in-depth skip uses the SAME 15-minute window as the DB
	// predicate (RECON-03) — one source of truth, no duplicated magic literal.
	adminTerminatingClaimWindow = db.AdminTerminatingClaimWindow

	// stuckRepeatTicks is how many CONSECUTIVE passes an instance may be stuck
	// before it counts toward the stuck_repeat escalation metric (~15 min at
	// the 5-min interval). One or two stuck passes are normal (slow provider
	// teardown, a transient 409); three in a row means the retry loop is not
	// converging — a provider incident someone must look at.
	stuckRepeatTicks = 3
)

// isAdminClaimed reports whether an instance is under an active admin
// force-terminate claim (instances.admin_terminating_at set within `window`).
// A nil claim or a claim older than `window` returns false so a crashed
// force-terminate self-heals instead of pinning the instance out of
// reconciliation forever (RECON-03 / T-48-12).
func isAdminClaimed(inst *db.Instance, window time.Duration) bool {
	if inst.AdminTerminatingAt == nil {
		return false
	}
	return time.Since(*inst.AdminTerminatingAt) < window
}

// New constructs a Reconciler. Logger defaults to slog.Default if nil.
func New(d Deps) *Reconciler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := d.Interval
	if interval == 0 {
		interval = defaultInterval
	}
	window := d.RecentTermWindow
	if window == 0 {
		window = defaultRecentTermWindow
	}
	return &Reconciler{
		db:               d.DB,
		registry:         d.Registry,
		terminator:       d.Terminator,
		logger:           logger,
		interval:         interval,
		recentTermWindow: window,
		dryRun:           d.DryRun,
		ghostDryRun:      d.GhostDryRun,
	}
}

// Start runs the reconciler loop. First pass fires immediately, subsequent
// passes on each tick. Blocks until ctx is cancelled.
func (r *Reconciler) Start(ctx context.Context) {
	r.logger.Info("reconciler starting",
		slog.Duration("interval", r.interval),
		slog.Duration("recent_term_window", r.recentTermWindow),
		slog.Bool("dry_run", r.dryRun),
	)
	r.RunOnce(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("reconciler stopped")
			return
		case <-ticker.C:
			r.RunOnce(ctx)
		}
	}
}

// RunOnce performs a single reconciliation pass. Exported so tests and
// the eventual /admin/reconcile-now endpoint can trigger it.
func (r *Reconciler) RunOnce(ctx context.Context) {
	start := time.Now()
	rows, err := r.db.ListReconcilableInstances(ctx, r.recentTermWindow)
	if err != nil {
		r.logger.Error("reconciler: list instances failed",
			slog.String("error", err.Error()),
		)
		return
	}

	// Index DB state for fast lookup by (provider, upstream_id).
	byUpstream := make(map[string]map[string]dbRowRef) // provider → upstream_id → row
	for i := range rows {
		inst := rows[i]
		// Defense-in-depth (RECON-03): the DB predicate in
		// ListReconcilableInstances already filters freshly-claimed rows, but if
		// a claim leaks through (e.g. set between the query and now) skip it so a
		// concurrent admin force-terminate is never misclassified (Pitfall 2).
		if isAdminClaimed(&inst, adminTerminatingClaimWindow) {
			continue
		}
		if inst.UpstreamProvider == "" || inst.UpstreamID == "" {
			continue
		}
		m, ok := byUpstream[inst.UpstreamProvider]
		if !ok {
			m = make(map[string]dbRowRef)
			byUpstream[inst.UpstreamProvider] = m
		}
		m[inst.UpstreamID] = dbRowRef{instance: &rows[i]}
	}

	stats := reconcileStats{}

	// Pass 1: orphan detection per provider via ListProvisioned.
	r.detectOrphans(ctx, byUpstream, &stats)

	// Pass 2: ghost + stuck-terminate detection — DB-side scan in parallel.
	r.detectGhostsAndStuck(ctx, rows, &stats)

	stuckRepeat := r.updateStuckStreaks(stats.stuckIDs)

	elapsed := time.Since(start)
	r.logger.Info("reconciler tick complete",
		slog.Duration("elapsed", elapsed),
		slog.Int("rows_examined", len(rows)),
		slog.Int("orphans_found", stats.orphans),
		slog.Int("orphans_acted", stats.orphansActed),
		slog.Int("ghosts_found", stats.ghosts),
		slog.Int("ghosts_acted", stats.ghostsActed),
		slog.Int("stuck_found", stats.stuck),
		slog.Int("stuck_acted", stats.stuckActed),
		slog.Int("stuck_repeat", stuckRepeat),
		slog.Bool("dry_run", r.dryRun),
	)

	// Read-only snapshot for the /admin reconcile panel (RECON-01 / L-03). This
	// is the ONLY addition to RunOnce — it records what the pass already
	// computed and logged; it takes no action and is written AFTER all action
	// passes have completed.
	r.setSnapshot(StatsSnapshot{
		LastRunAt:    start,
		Elapsed:      elapsed,
		RowsExamined: len(rows),
		Orphans:      stats.orphans,
		OrphansActed: stats.orphansActed,
		Ghosts:       stats.ghosts,
		GhostsActed:  stats.ghostsActed,
		Stuck:        stats.stuck,
		StuckActed:   stats.stuckActed,
		StuckRepeat:  stuckRepeat,
		DryRun:       r.dryRun,
	})
}

// updateStuckStreaks advances the cross-tick consecutive-stuck counters from
// this pass's stuck instance_ids and returns how many instances have now been
// stuck for >= stuckRepeatTicks CONSECUTIVE passes. Ids not stuck this pass
// reset (dropped from the map), so a recovered instance never lingers. Called
// only from RunOnce (ticker goroutine) — no locking needed.
func (r *Reconciler) updateStuckStreaks(stuckIDs []string) int {
	next := make(map[string]int, len(stuckIDs))
	for _, id := range stuckIDs {
		next[id] = r.stuckStreak[id] + 1
	}
	r.stuckStreak = next
	repeat := 0
	for id, n := range next {
		if n >= stuckRepeatTicks {
			repeat++
			r.logger.Warn("reconciler: instance stuck across consecutive passes — retry loop not converging",
				slog.String("instance_id", id),
				slog.Int("consecutive_passes", n),
			)
		}
	}
	return repeat
}

// setSnapshot stores the latest pass result under snapMu (write lock).
func (r *Reconciler) setSnapshot(s StatsSnapshot) {
	r.snapMu.Lock()
	defer r.snapMu.Unlock()
	r.snap = s
}

// Snapshot returns the most recent RunOnce result for the internal /admin
// reconcile panel (RECON-01). Returns a zero-value StatsSnapshot (LastRunAt
// is the zero time) before the first pass completes. Read-only — it never
// triggers a pass or mutates any state (L-03).
func (r *Reconciler) Snapshot() StatsSnapshot {
	r.snapMu.RLock()
	defer r.snapMu.RUnlock()
	return r.snap
}

type reconcileStats struct {
	orphans, orphansActed int
	ghosts, ghostsActed   int
	stuck, stuckActed     int
	// stuckIDs collects the instance_ids counted stuck THIS pass (all three
	// stuck classes), consumed by RunOnce's cross-tick streak tracking so a
	// persistently-stuck instance (>= stuckRepeatTicks consecutive passes)
	// escalates via the StuckRepeat metric instead of retrying silently forever.
	stuckIDs []string

	mu sync.Mutex
}

func (s *reconcileStats) addOrphan(acted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orphans++
	if acted {
		s.orphansActed++
	}
}

func (s *reconcileStats) addGhost(acted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ghosts++
	if acted {
		s.ghostsActed++
	}
}

func (s *reconcileStats) addStuck(acted bool, instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stuck++
	if acted {
		s.stuckActed++
	}
	if instanceID != "" {
		s.stuckIDs = append(s.stuckIDs, instanceID)
	}
}

// dbRowRef is a small wrapper around a db.Instance pointer used as the value
// type in the per-provider upstream-id index (declared at package level so
// detectOrphans can take it as a parameter type).
type dbRowRef struct {
	instance *db.Instance
}

// detectOrphans iterates each registered provider, lists its live instances,
// and terminates any whose Name looks like one of our UUIDs but doesn't
// appear in our DB (or appears only as a terminated row).
func (r *Reconciler) detectOrphans(ctx context.Context, byUpstream map[string]map[string]dbRowRef, stats *reconcileStats) {
	for _, name := range r.registry.Names() {
		prov, ok := r.registry.Get(name)
		if !ok {
			continue
		}
		live, err := prov.ListProvisioned(ctx)
		if err != nil {
			if errors.Is(err, provider.ErrNotImplemented) {
				continue // expected for the generic adapter
			}
			r.logger.Warn("reconciler: ListProvisioned failed",
				slog.String("provider", name),
				slog.String("error", err.Error()),
			)
			continue
		}

		dbRows := byUpstream[name]
		for _, up := range live {
			// Safety guard: only operate on instances tagged with our UUID
			// format. This prevents accidentally terminating instances
			// launched outside gpu-ai under the same API key.
			ourInstanceID := parseOurInstanceID(up.Name)
			if ourInstanceID == "" {
				continue
			}

			// Match by upstream_id first (canonical), fall back to instance_id
			// (sometimes the upstream encodes our UUID in the name but reports a
			// different opaque upstream_id back, e.g. a provider-native machine id).
			row, hasRow := dbRows[up.UpstreamID]
			if !hasRow {
				// Try matching by our instance_id encoded in the Name
				// against the indexed map by walking — small N, fine.
				for _, candidate := range dbRows {
					if candidate.instance.InstanceID == ourInstanceID {
						row = candidate
						hasRow = true
						break
					}
				}
			}

			if !hasRow {
				// TOCTOU guard: byUpstream was built from a DB snapshot taken at the
				// START of this tick (ListReconcilableInstances), but ListProvisioned
				// ran seconds later. A provision whose upstream resource was created in
				// that gap — or whose row was snapshotted before its upstream_id was
				// written — appears here with no matching row even though a live row
				// now exists. Terminating it kills a just-provisioned customer instance
				// (seen in production: an instance reaped ~4s after provision).
				// Re-read the row fresh before acting; only a definitive "no row" is an
				// orphan. Any other state (live row, or an unconfirmable DB error) means
				// we must NOT terminate — a real leak costs minutes, a wrong terminate
				// costs a customer their instance.
				if !r.confirmOrphan(ctx, ourInstanceID, name, up) {
					continue
				}
				// Pure orphan — no DB row references this upstream resource at all.
				stats.addOrphan(true /* will act if not dry-run */)
				r.actOnOrphan(ctx, name, up, ourInstanceID, "no matching db row")
				continue
			}

			// We have a DB row. If it's terminated, this is also an orphan
			// (we thought we killed it but didn't). Re-fire Terminate.
			if row.instance.Status == "terminated" {
				stats.addStuck(true, row.instance.InstanceID)
				r.actOnStuckTerminate(ctx, name, up, row.instance, "db terminated, upstream alive")
				continue
			}
			// Otherwise the DB row is still live — health monitor / engine
			// owns this case. Reconciler intentionally does nothing here.
		}
	}
}

// detectGhostsAndStuck scans the DB-side rows to catch:
//   - GHOST: DB says active, upstream is gone or errored
//   - STUCK: DB says stopping for >grace period (caller never confirmed)
//
// We use bounded concurrency to keep upstream API load reasonable.
func (r *Reconciler) detectGhostsAndStuck(ctx context.Context, rows []db.Instance, stats *reconcileStats) {
	const maxConcurrent = 8
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	now := time.Now().UTC()

	for i := range rows {
		inst := rows[i]
		// Skip instances under an active admin force-terminate claim — the
		// force-terminate already owns this row; acting here would race it and
		// risk a ghost/double-terminate misclassification (RECON-03 / Pitfall 2).
		if isAdminClaimed(&inst, adminTerminatingClaimWindow) {
			continue
		}
		// Skip terminated rows here — orphan/stuck pass already handles them via ListProvisioned.
		if inst.Status == "terminated" {
			continue
		}
		// Skip too-fresh rows — upstream may not have surfaced them yet.
		if now.Sub(inst.CreatedAt) < minInstanceAge {
			continue
		}
		// Skip rows with no upstream_id (rare, but we have nothing to query).
		if inst.UpstreamID == "" {
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r.checkActiveRow(ctx, &inst, stats)
		}()
	}
	wg.Wait()
}

func (r *Reconciler) checkActiveRow(ctx context.Context, inst *db.Instance, stats *reconcileStats) {
	prov, ok := r.registry.Get(inst.UpstreamProvider)
	if !ok {
		return
	}

	status, err := prov.GetStatus(ctx, inst.UpstreamID, inst.Region)
	if err != nil {
		// 404 / not found while DB says active = ghost.
		if isNotFoundErr(err) {
			stats.addGhost(r.actOnGhost(ctx, inst, "upstream returned not-found"))
			return
		}
		// Other errors are noisy but not actionable here — health monitor handles transient glitches.
		return
	}
	if status == nil {
		return
	}

	switch status.Status {
	case "terminated", "exited":
		stats.addGhost(r.actOnGhost(ctx, inst, "upstream reports "+status.Status))
		return
	}

	// Stuck-error: DB says error but upstream is still alive — the state the
	// production leak (an upstream left running for 13h) sat in. The engine parks a
	// row in `error` when its provider Terminate ultimately fails (or a
	// provision dies after the upstream exists); before this class no loop
	// owned that state, so the upstream billed US indefinitely. Re-drive the
	// FULL engine terminate path (error→stopping is a legal transition,
	// state.go): on success the engine marks the row terminated + closes
	// billing; on provider failure it returns to `error` and we retry next
	// tick — indefinitely, until the upstream yields.
	if inst.Status == "error" {
		stats.addStuck(true, inst.InstanceID)
		r.actOnStuckError(ctx, inst, status.Status)
		return
	}

	// Stuck-stopping: DB says stopping but upstream still alive after grace.
	if inst.Status == "stopping" && inst.UpdatedAt != (time.Time{}) {
		if time.Since(inst.UpdatedAt) > stuckTerminationGrace {
			stats.addStuck(true, inst.InstanceID)
			r.actOnStuckLocal(ctx, prov, inst, status.Status)
		}
	}
}

// confirmOrphan re-reads the instance row fresh (point read, post-snapshot) to
// decide whether an orphan candidate is truly orphaned before we terminate it.
// It closes the orphan-pass TOCTOU race (see the call site): the byUpstream index
// is built from a tick-start snapshot, but a provision can create its upstream
// resource — and its row — between that snapshot and ListProvisioned.
//
// Returns true ONLY when the row is definitively absent (db.ErrNotFound) — a
// genuine leak the reaper should kill. Returns false (skip terminate) when a live
// row now exists, OR when the re-read fails for any other reason: termination is
// destructive and irreversible, so an unconfirmable state must never act. A row
// that exists but is already terminated is also treated as confirmed-orphan
// (true): we believed it dead, upstream says otherwise, so killing it is correct.
func (r *Reconciler) confirmOrphan(ctx context.Context, ourInstanceID, providerName string, up provider.ProvisionedInstance) bool {
	fresh, err := r.db.GetInstance(ctx, ourInstanceID)
	if errors.Is(err, db.ErrNotFound) {
		return true // no row — genuine orphan
	}
	if err != nil {
		r.logger.Warn("reconciler: orphan re-check failed, skipping terminate",
			slog.String("provider", providerName),
			slog.String("upstream_id", up.UpstreamID),
			slog.String("our_instance_id", ourInstanceID),
			slog.String("error", err.Error()),
		)
		return false // can't confirm — do not terminate
	}
	if fresh != nil && fresh.Status != "terminated" {
		r.logger.Info("reconciler: orphan candidate has a live db row on re-check, skipping (provision/snapshot race)",
			slog.String("provider", providerName),
			slog.String("upstream_id", up.UpstreamID),
			slog.String("our_instance_id", ourInstanceID),
			slog.String("db_status", fresh.Status),
		)
		return false // live row appeared after the snapshot — not an orphan
	}
	return true // row exists but is terminated — upstream outlived it, kill it
}

// --- Action helpers ---

// actOnOrphan terminates an upstream resource that has no live matching DB row.
func (r *Reconciler) actOnOrphan(ctx context.Context, providerName string, up provider.ProvisionedInstance, ourInstanceID, reason string) {
	r.logger.Warn("reconciler: ORPHAN detected",
		slog.String("provider", providerName),
		slog.String("upstream_id", up.UpstreamID),
		slog.String("upstream_name", up.Name),
		slog.String("our_instance_id", ourInstanceID),
		slog.String("upstream_status", up.Status),
		slog.String("reason", reason),
		slog.Bool("dry_run", r.dryRun),
	)
	if r.dryRun {
		return
	}
	prov, ok := r.registry.Get(providerName)
	if !ok {
		return
	}
	if err := prov.Terminate(ctx, up.UpstreamID, up.Region); err != nil && !isNotFoundErr(err) {
		r.logger.Error("reconciler: orphan terminate failed",
			slog.String("provider", providerName),
			slog.String("upstream_id", up.UpstreamID),
			slog.String("error", err.Error()),
		)
	}
}

// actOnStuckTerminate re-fires Terminate on an upstream that's still alive
// despite the DB row being marked terminated.
func (r *Reconciler) actOnStuckTerminate(ctx context.Context, providerName string, up provider.ProvisionedInstance, inst *db.Instance, reason string) {
	r.logger.Warn("reconciler: STUCK-TERMINATE detected",
		slog.String("provider", providerName),
		slog.String("instance_id", inst.InstanceID),
		slog.String("upstream_id", up.UpstreamID),
		slog.String("reason", reason),
		slog.Bool("dry_run", r.dryRun),
	)
	if r.dryRun {
		return
	}
	prov, ok := r.registry.Get(providerName)
	if !ok {
		return
	}
	if err := prov.Terminate(ctx, up.UpstreamID, up.Region); err != nil && !isNotFoundErr(err) {
		r.logger.Error("reconciler: stuck-terminate retry failed",
			slog.String("provider", providerName),
			slog.String("instance_id", inst.InstanceID),
			slog.String("upstream_id", up.UpstreamID),
			slog.String("error", err.Error()),
		)
	}
}

// actOnGhost marks a DB row terminated when its upstream has disappeared.
// Uses Terminator (provision.Engine) so billing close + audit log happen
// alongside the status transition. Returns true when it attempted the
// terminate (false when a dry-run gate or the grace window held it back),
// feeding the GhostsActed snapshot counter.
func (r *Reconciler) actOnGhost(ctx context.Context, inst *db.Instance, reason string) bool {
	r.logger.Warn("reconciler: GHOST detected",
		slog.String("provider", inst.UpstreamProvider),
		slog.String("instance_id", inst.InstanceID),
		slog.String("upstream_id", inst.UpstreamID),
		slog.String("db_status", inst.Status),
		slog.String("reason", reason),
		slog.Bool("dry_run", r.dryRun),
		slog.Bool("ghost_dry_run", r.ghostDryRun),
	)
	if r.dryRun {
		return false
	}
	if grace, enforced := ghostFinalizeGrace(inst.Status); enforced {
		// GPU-41 + the `stopping` extension: these statuses are enforced past
		// their grace window even under ghostDryRun. The D-04 deferral exists
		// so the reconciler and the health monitor cannot double-act on the
		// same disappeared upstream — but the monitor scans running/booting
		// ONLY (db.ListActiveInstances), so for these statuses there is no
		// second actor to defer to, and deferring leaves them un-finalized
		// forever. Zero UpdatedAt means we cannot date the transition, so stay
		// log-only (conservative).
		if inst.UpdatedAt.IsZero() || time.Since(inst.UpdatedAt) < grace {
			return false
		}
	} else if r.ghostDryRun {
		// D-04 / BLOCK-01b: non-error GHOSTs stay dry-run even in enforce
		// mode, deferring to the wired health monitor (health/monitor.go:
		// 129-150) so the two can't double-act on the same disappeared
		// upstream (Pitfall 2).
		return false
	}
	if r.terminator == nil {
		return false
	}
	if err := r.terminator.Terminate(ctx, inst.InstanceID); err != nil {
		r.logger.Error("reconciler: ghost terminate failed",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
	}
	return true
}

// ghostFinalizeGrace reports whether a GHOST in the given DB status must be
// finalized by the reconciler despite ghostDryRun, and how long to wait first.
//
// The rule is one question: does any OTHER component ever finalize this status?
// The health monitor scans `running`/`booting` only (db.ListActiveInstances),
// so those defer to it (D-04, no double-act). Every status below is one the
// monitor never sees and nothing else finalizes, so the reconciler is the only
// component that can end them — and if it declines, the row's billing session
// stays open indefinitely.
//
// Keep this in sync with db.ListActiveInstances: a status added THERE must be
// removed HERE, or the two will double-act on the same upstream.
func ghostFinalizeGrace(status string) (time.Duration, bool) {
	switch status {
	case "error":
		return errorGhostFinalizeGrace, true
	case "stopping":
		return stoppingGhostFinalizeGrace, true
	default:
		return 0, false
	}
}

// actOnStuckLocal re-drives termination when DB says stopping but upstream is
// still alive past the grace window. Prefers the FULL engine path (Terminator)
// so a success also transitions the row to terminated + closes billing —
// a bare upstream re-fire leaves the row parked in `stopping` forever, because
// the health monitor only scans running/booting rows and the GHOST class is
// log-only (D-04). Falls back to a bare upstream Terminate when no Terminator
// is wired (defensive; production always wires one).
func (r *Reconciler) actOnStuckLocal(ctx context.Context, prov provider.Provider, inst *db.Instance, upstreamStatus string) {
	r.logger.Warn("reconciler: STUCK-STOPPING detected",
		slog.String("provider", inst.UpstreamProvider),
		slog.String("instance_id", inst.InstanceID),
		slog.String("upstream_id", inst.UpstreamID),
		slog.String("upstream_status", upstreamStatus),
		slog.Bool("dry_run", r.dryRun),
	)
	if r.dryRun {
		return
	}
	if r.terminator != nil {
		if err := r.terminator.Terminate(ctx, inst.InstanceID); err != nil {
			r.logger.Error("reconciler: stuck-stopping terminate failed",
				slog.String("instance_id", inst.InstanceID),
				slog.String("error", err.Error()),
			)
		}
		return
	}
	if err := prov.Terminate(ctx, inst.UpstreamID, inst.Region); err != nil && !isNotFoundErr(err) {
		r.logger.Error("reconciler: stuck-stopping terminate failed",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
	}
}

// actOnStuckError re-drives termination for a row the engine parked in `error`
// after a failed provider Terminate (or a provision failure that left the
// upstream alive). Uses the FULL engine path: error→stopping is a legal
// transition (provision/state.go), so Engine.Terminate re-runs provider
// terminate + verification and, on success, marks the row terminated and
// closes billing; on failure the row returns to `error` and the next 5-min
// tick retries — indefinitely, until the upstream is confirmed gone. This is
// the continuous-retry loop a production leak (13h at
// ~$11/hr) was missing.
func (r *Reconciler) actOnStuckError(ctx context.Context, inst *db.Instance, upstreamStatus string) {
	errorReason := ""
	if inst.ErrorReason != nil {
		errorReason = *inst.ErrorReason
	}
	r.logger.Warn("reconciler: STUCK-ERROR detected (db error, upstream alive)",
		slog.String("provider", inst.UpstreamProvider),
		slog.String("instance_id", inst.InstanceID),
		slog.String("upstream_id", inst.UpstreamID),
		slog.String("upstream_status", upstreamStatus),
		slog.String("error_reason", errorReason),
		slog.Bool("dry_run", r.dryRun),
	)
	if r.dryRun || r.terminator == nil {
		return
	}
	if err := r.terminator.Terminate(ctx, inst.InstanceID); err != nil {
		r.logger.Error("reconciler: stuck-error terminate failed",
			slog.String("instance_id", inst.InstanceID),
			slog.String("error", err.Error()),
		)
	}
}
