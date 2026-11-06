package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a database query yields no matching rows.
var ErrNotFound = errors.New("not found")

// Instance represents the full internal instance record from the database.
// Maps to all columns across v0+v1+v2+v3 schema versions.
type Instance struct {
	InstanceID       string
	OrgID            string
	UserID           string
	UpstreamProvider string
	UpstreamID       string
	UpstreamIP       *string
	Hostname         string
	WGPublicKey      *string
	WGPrivateKeyEnc  *string
	WGAddress        *string
	FRPRemotePort    *int
	// SSHPort is the instance's public SSH port in direct-connect mode (no
	// FRP tunnel): the port the supplier reported for container port 22,
	// reachable at UpstreamIP. NULL when the FRP tunnel carries SSH.
	SSHPort  *int
	Name     *string
	GPUType  string
	GPUCount int
	Tier     string
	Region   string
	// PricePerHour is the customer-facing WHOLE-INSTANCE hourly rate covering
	// all GPUCount GPUs (sourced from the matched GPUOffering, which prices the
	// full node). Billing never multiplies it by GPUCount.
	PricePerHour         float64
	UpstreamPricePerHour float64
	BillingStart         *time.Time
	BillingEnd           *time.Time
	Status               string
	ErrorReason          *string
	InternalToken        *string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	ReadyAt              *time.Time
	TerminatedAt         *time.Time
	// ReservationCapacityID is the FK to reservation_capacity(capacity_id) for
	// reserved-tier instances; NULL for on-demand / spot. Schema v18 (Plan
	// 30-01). Reserved capacity is not part of GPU.ai Open Source, so this is
	// always NULL; the column is kept for schema compatibility.
	ReservationCapacityID *string

	// ReservationContractID is the parent contract_id resolved via JOIN
	// through reservation_capacity at read time (Plan 30-06). NOT a column
	// on the instances table itself — populated only by the SELECT paths
	// that include the JOIN (ListInstances + ListInstancesWithOwner). The
	// single-row reads (GetInstance / GetInstanceForOrg) leave this nil
	// because their callers (engine, internal lookups) don't need the
	// customer-facing contract reference. NULL for on-demand / spot.
	ReservationContractID *string

	// CleanupStatus tracks the post-terminate SSH teardown outcome for
	// supplier-bound instances (Phase 30.1, migration v19). NULL for
	// on-demand and Phase-30 reserved-auto instances — they leave this
	// column unset forever. For supplier-bound instances:
	//   'done'    -- SSH teardown of frpc-<id>.service + authorized_keys.d/<id>
	//                succeeded; reservation_capacity slot has been released.
	//   'pending' -- SSH teardown failed; the slot is HELD until the cleanup
	//                retry succeeds (Plan 30.1-05 `gpuctl supplier-node
	//                cleanup <instance_id>`).
	CleanupStatus *string

	// TemplateID is the deployable-template id for template deploys (Phase 31,
	// schema v20); NULL for on-demand/spot. Plain TEXT — straight *string scan,
	// no ::TEXT cast. Set by the template deploy path (Plan 07); the app-ready
	// gate (Plan 08) keys off non-NULL to defer the booting->running flip.
	TemplateID *string

	// DiskGB is the instance filesystem size, in GB, that the platform actually
	// configured for this launch (schema v70). Written by the engine ONLY when
	// the winning provider honors the size we send (provider.ConfigurableDisk);
	// NULL means "provider-determined" — a VM upstream's flavor brings its own
	// disk, and recording our number for it would fabricate a spec the customer
	// can hold us to. NULL for every row that predates the column.
	DiskGB *int
	// AppAuthUser / AppAuthPass are the per-deploy app basic-auth credential
	// (schema v74, revising D6 surface-once): persisted so the org-scoped
	// instance reads can re-surface the app URL login for the instance's
	// lifetime — same trust boundary as internal_token above. NULL for raw
	// (no-app-port) instances and for template deploys created before v74.
	// AppAuthPass must never be logged.
	AppAuthUser *string
	AppAuthPass *string

	// TerminalPort is the local port the bootstrap serves the ttyd web console
	// on (schema v76). Non-NULL means the instance was provisioned with the
	// browser terminal — reads gate terminal_url on it so pre-v76 instances
	// (no ttyd on board) never surface a dead console link. The console shares
	// the v74 app basic-auth credential above.
	TerminalPort *int

	// LastReachableAt is the last time the platform confirmed this instance's
	// SSH path end-to-end — TCP connect to the FRP gateway's remote port plus a
	// valid SSH banner (schema v80, GPU-49). Written ONLY by the health
	// monitor's tunnel probe; nothing else may set it, because its whole value
	// is that it means "verified", not "we assume so". NULL = never probed:
	// every row predating v80, plus instances with no FRP tunnel.
	//
	// A frozen value on a still-"running" instance is the signature of the
	// incident this column exists for — sshd or frpc died while the provider's
	// status API kept reporting healthy.
	LastReachableAt *time.Time

	// UnreachableSince is T0 of the instance's CURRENT confirmed tunnel outage
	// — the first failed probe of the run that crossed the confirmation
	// threshold (schema v88, GPU-77). NULL means reachable, never probed, or a
	// row predating v88; NULL is the overwhelmingly common case.
	//
	// Two things read it, and both are why it is durable rather than living
	// only in the monitor's in-memory episode tracker:
	//   - the API read paths map a RUNNING instance with a non-NULL value to
	//     the customer-facing status "unreachable" (the DB status column stays
	//     "running" — see the mapping in internal/api/public/handlers_instances.go);
	//   - the credit writer anchors the dead-window credit and its idempotency
	//     reference on this exact timestamp, so a gpuctl restart mid-outage
	//     neither shortens the window nor mints a second reference for it.
	//
	// Written ONLY by the health monitor's tunnel probe: set at announce
	// (first write wins, see SetInstanceUnreachableSince) and cleared on the
	// first successful probe. A stale value on a non-running row is inert —
	// the status mapping only ever overrides "running".
	UnreachableSince *time.Time

	// Image is the CANONICAL customer-supplied image reference this instance
	// was launched with (schema v89, custom-images Phase 1) — normalized and
	// digest-pinned by the create handler's pre-flight, e.g.
	// "ghcr.io/acme/trainer:v1@sha256:…". NULL for every other launch class
	// (raw, template, certified, raw_vm); template/certified images stay
	// server-internal (TMPL-13), this column only ever holds a string the
	// customer themselves sent.
	Image *string

	// AppPort is the PRIMARY http port a CUSTOM-IMAGE launch declared
	// (schema v90, ports[].primary) — the read path derives app_url from it
	// the way template rows derive it from template_id. NULL for every other
	// launch class, including template deploys (whose app port lives in the
	// catalog spec, not the row).
	AppPort *int

	// AutoTerminateHours is the customer's per-instance runtime bound, in
	// hours, exactly as sent at launch (schema v92, GPU-60). NULL = never,
	// which is every row predating the column and every launch that omits the
	// field. Purely a record of the REQUEST — enforcement reads
	// AutoTerminateAt below — so it survives the sweep and keeps answering
	// "what did this launch ask for?" on a read after the fact.
	AutoTerminateHours *int

	// AutoTerminateAt is the absolute DEADLINE the sweep enforces (schema v92,
	// GPU-60): set once, at the running transition (SetInstanceRunning), to
	// ready_at + AutoTerminateHours. Computed there rather than at create time
	// because "N hours" means N hours of USABLE runtime — a slow-boot
	// provisioning window the customer is not billed for must not eat into it.
	//
	// NULL means one of: no bound requested, not running yet, or the sweep has
	// already claimed this row. That last case is load-bearing:
	// ClaimInstanceAutoTerminate clears the column as its atomic claim, so the
	// column doubles as the exactly-once gate and no second marker is needed.
	AutoTerminateAt *time.Time

	// AdminTerminatingAt is the admin force-terminate "claim" marker (schema
	// v40, RECON-03). When an admin force-terminate calls engine.Terminate it
	// first stamps this column so a concurrent reconciler tick skips the
	// in-flight instance instead of misclassifying it (Pitfall 2). NULL for
	// every instance not under an active force-terminate.
	//
	// This field is NOT part of the shared instanceColumns positional scan —
	// to keep the diff small it is populated ONLY by ListReconcilableInstances
	// (the sole reader that needs the in-loop defense-in-depth guard). All
	// other SELECT paths leave it nil; the DB predicate in that one query is
	// the primary guard.
	AdminTerminatingAt *time.Time
}

// instanceColumns is the ordered list of columns for SELECT queries.
// reservation_capacity_id (schema v18), cleanup_status (schema v19),
// template_id (schema v20), disk_gb (schema v70), last_reachable_at
// (schema v80), unreachable_since (schema v88) and the auto_terminate pair
// (schema v92) are appended at the END so
// existing positional scans don't shift; scanInstance + every callsite scan
// must include them as the trailing fields (before any query-specific JOIN
// columns). template_id is plain TEXT and disk_gb plain INTEGER (no casts).
//
// WARNING: a positional mismatch between this list and any of the ~11 Scan
// callsites does NOT fail the compile — it fails at runtime, or worse, silently
// lands a value in the wrong field. Append here and to EVERY scan in the same
// change, including instanceColumnsListWithContract.
const instanceColumns = `instance_id, org_id, user_id, upstream_provider, upstream_id,
	host(upstream_ip) AS upstream_ip, hostname, wg_public_key, wg_private_key_enc, wg_address::TEXT,
	frp_remote_port, ssh_port, name, gpu_type, gpu_count, tier, region,
	price_per_hour, upstream_price_per_hour, billing_start, billing_end,
	status, error_reason, internal_token,
	created_at, updated_at, ready_at, terminated_at,
	reservation_capacity_id::TEXT, cleanup_status, template_id, disk_gb, app_auth_user, app_auth_pass, terminal_port,
	last_reachable_at, unreachable_since, image, app_port,
	auto_terminate_hours, auto_terminate_at`

// scanInstance scans a single row into an Instance struct.
func scanInstance(row pgx.Row) (*Instance, error) {
	var i Instance
	err := row.Scan(
		&i.InstanceID, &i.OrgID, &i.UserID, &i.UpstreamProvider, &i.UpstreamID,
		&i.UpstreamIP, &i.Hostname, &i.WGPublicKey, &i.WGPrivateKeyEnc, &i.WGAddress,
		&i.FRPRemotePort, &i.SSHPort, &i.Name, &i.GPUType, &i.GPUCount, &i.Tier, &i.Region,
		&i.PricePerHour, &i.UpstreamPricePerHour, &i.BillingStart, &i.BillingEnd,
		&i.Status, &i.ErrorReason, &i.InternalToken,
		&i.CreatedAt, &i.UpdatedAt, &i.ReadyAt, &i.TerminatedAt,
		&i.ReservationCapacityID, &i.CleanupStatus, &i.TemplateID, &i.DiskGB, &i.AppAuthUser, &i.AppAuthPass, &i.TerminalPort,
		&i.LastReachableAt, &i.UnreachableSince, &i.Image, &i.AppPort,
		&i.AutoTerminateHours, &i.AutoTerminateAt,
	)
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// CreateInstance inserts a new instance record into the database.
// reservation_capacity_id ($26) is nil for on-demand / spot tiers; the engine
// (Plan 30-03) populates it for the reserved branch from the AllocateSlot
// result. cleanup_status ($27) is nil for on-demand and Phase-30 reserved
// instances; only the supplier-direct terminate path (Phase 30.1-05) writes
// it. template_id ($28) is nil for raw on-demand/spot; the template deploy
// path (Plan 31-07) sets it from the validated catalog id so reads can derive
// app_url and the app-ready gate (Plan 08) can defer the booting->running flip.
// disk_gb ($29) is nil unless the winning provider honors a configured disk
// size (schema v70) — see the Instance.DiskGB doc for the NULL semantics.
// auto_terminate_hours ($35) is nil unless the customer bounded this
// instance's runtime at launch (schema v92); the matching auto_terminate_at
// deadline is deliberately NOT inserted here — it is computed at the running
// transition (SetInstanceRunning), because the bound counts usable runtime,
// not the provisioning window.
func (p *Pool) CreateInstance(ctx context.Context, inst *Instance) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO instances (
			instance_id, org_id, user_id, upstream_provider, upstream_id,
			upstream_ip, hostname, wg_public_key, wg_private_key_enc, wg_address,
			frp_remote_port, name, gpu_type, gpu_count, tier, region,
			price_per_hour, upstream_price_per_hour, billing_start, billing_end,
			status, error_reason, internal_token,
			ready_at, terminated_at,
			reservation_capacity_id, cleanup_status, template_id, disk_gb,
			app_auth_user, app_auth_pass, terminal_port, image, app_port,
			auto_terminate_hours
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			$11, $12, $13, $14, $15, $16,
			$17, $18, $19, $20,
			$21, $22, $23,
			$24, $25,
			$26, $27, $28, $29,
			$30, $31, $32, $33, $34,
			$35
		)`,
		inst.InstanceID, inst.OrgID, inst.UserID, inst.UpstreamProvider, inst.UpstreamID,
		inst.UpstreamIP, inst.Hostname, inst.WGPublicKey, inst.WGPrivateKeyEnc, inst.WGAddress,
		inst.FRPRemotePort, inst.Name, inst.GPUType, inst.GPUCount, inst.Tier, inst.Region,
		inst.PricePerHour, inst.UpstreamPricePerHour, inst.BillingStart, inst.BillingEnd,
		inst.Status, inst.ErrorReason, inst.InternalToken,
		inst.ReadyAt, inst.TerminatedAt,
		inst.ReservationCapacityID, inst.CleanupStatus, inst.TemplateID, inst.DiskGB,
		inst.AppAuthUser, inst.AppAuthPass, inst.TerminalPort, inst.Image, inst.AppPort,
		inst.AutoTerminateHours,
	)
	return err
}

// GetInstance retrieves an instance by its ID. Returns ErrNotFound if no row exists.
func (p *Pool) GetInstance(ctx context.Context, instanceID string) (*Instance, error) {
	row := p.pool.QueryRow(ctx,
		`SELECT `+instanceColumns+` FROM instances WHERE instance_id = $1`,
		instanceID,
	)
	inst, err := scanInstance(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return inst, err
}

// GetInstanceForOrg retrieves an instance scoped to a specific organization.
// This enforces organization isolation at the query level (AUTH-03).
func (p *Pool) GetInstanceForOrg(ctx context.Context, instanceID, orgID string) (*Instance, error) {
	row := p.pool.QueryRow(ctx,
		`SELECT `+instanceColumns+` FROM instances WHERE instance_id = $1 AND org_id = $2`,
		instanceID, orgID,
	)
	inst, err := scanInstance(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return inst, err
}

// instanceColumnsListWithContract is instanceColumns prefixed with the `i.`
// alias plus a trailing reservation_contract_id. Reserved capacity contracts
// are not part of GPU.ai Open Source, so the column is always NULL; it is kept
// so the scan shape (and the public reservation_id field, always omitted)
// match the hosted API.
const instanceColumnsListWithContract = `i.instance_id, i.org_id, i.user_id, i.upstream_provider, i.upstream_id,
	host(i.upstream_ip) AS upstream_ip, i.hostname, i.wg_public_key, i.wg_private_key_enc, i.wg_address::TEXT,
	i.frp_remote_port, i.ssh_port, i.name, i.gpu_type, i.gpu_count, i.tier, i.region,
	i.price_per_hour, i.upstream_price_per_hour, i.billing_start, i.billing_end,
	i.status, i.error_reason, i.internal_token,
	i.created_at, i.updated_at, i.ready_at, i.terminated_at,
	i.reservation_capacity_id::TEXT, i.cleanup_status, i.template_id, i.disk_gb, i.app_auth_user, i.app_auth_pass, i.terminal_port,
	i.last_reachable_at, i.unreachable_since, i.image, i.app_port,
	i.auto_terminate_hours, i.auto_terminate_at,
	NULL::TEXT AS reservation_contract_id`

// ListInstances returns instances for an organization using keyset pagination.
// The cursor is a (created_at, instance_id) tuple. Pass nil cursor for the first page.
// Returns limit+1 rows so the caller can detect has_more by checking len > limit.
//
// statuses, when non-empty, restricts rows to the given internal states
// (i.status = ANY(...)); empty/nil means no status filter. The public list
// handler passes the non-terminated set by default so the unbounded
// terminated history is opt-in (?status=terminated / ?status=all).
func (p *Pool) ListInstances(ctx context.Context, orgID string, statuses []string, cursor *time.Time, cursorID string, limit int) ([]Instance, error) {
	args := []any{orgID}
	where := `i.org_id = $1`
	if len(statuses) > 0 {
		args = append(args, statuses)
		where += fmt.Sprintf(` AND i.status = ANY($%d)`, len(args))
	}
	if cursor != nil {
		args = append(args, *cursor, cursorID)
		where += fmt.Sprintf(` AND (i.created_at, i.instance_id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	args = append(args, limit+1)
	rows, err := p.pool.Query(ctx,
		`SELECT `+instanceColumnsListWithContract+`
		 FROM instances i
		 WHERE `+where+`
		 ORDER BY i.created_at DESC, i.instance_id DESC
		 LIMIT `+fmt.Sprintf("$%d", len(args)),
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Instance
	for rows.Next() {
		var inst Instance
		if err := rows.Scan(
			&inst.InstanceID, &inst.OrgID, &inst.UserID, &inst.UpstreamProvider, &inst.UpstreamID,
			&inst.UpstreamIP, &inst.Hostname, &inst.WGPublicKey, &inst.WGPrivateKeyEnc, &inst.WGAddress,
			&inst.FRPRemotePort, &inst.SSHPort, &inst.Name, &inst.GPUType, &inst.GPUCount, &inst.Tier, &inst.Region,
			&inst.PricePerHour, &inst.UpstreamPricePerHour, &inst.BillingStart, &inst.BillingEnd,
			&inst.Status, &inst.ErrorReason, &inst.InternalToken,
			&inst.CreatedAt, &inst.UpdatedAt, &inst.ReadyAt, &inst.TerminatedAt,
			&inst.ReservationCapacityID, &inst.CleanupStatus, &inst.TemplateID, &inst.DiskGB, &inst.AppAuthUser, &inst.AppAuthPass, &inst.TerminalPort,
			&inst.LastReachableAt, &inst.UnreachableSince, &inst.Image, &inst.AppPort,
			&inst.AutoTerminateHours, &inst.AutoTerminateAt,
			&inst.ReservationContractID,
		); err != nil {
			return nil, err
		}
		instances = append(instances, inst)
	}
	return instances, rows.Err()
}

// ListRunningInstancesByOrg returns all instances with status 'running' for an org.
// Used by the billing ticker to stop instances when a spending limit is reached.
func (p *Pool) ListRunningInstancesByOrg(ctx context.Context, orgID string) ([]Instance, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT `+instanceColumns+` FROM instances WHERE org_id = $1 AND status = 'running'`,
		orgID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Instance
	for rows.Next() {
		var inst Instance
		if err := rows.Scan(
			&inst.InstanceID, &inst.OrgID, &inst.UserID, &inst.UpstreamProvider, &inst.UpstreamID,
			&inst.UpstreamIP, &inst.Hostname, &inst.WGPublicKey, &inst.WGPrivateKeyEnc, &inst.WGAddress,
			&inst.FRPRemotePort, &inst.SSHPort, &inst.Name, &inst.GPUType, &inst.GPUCount, &inst.Tier, &inst.Region,
			&inst.PricePerHour, &inst.UpstreamPricePerHour, &inst.BillingStart, &inst.BillingEnd,
			&inst.Status, &inst.ErrorReason, &inst.InternalToken,
			&inst.CreatedAt, &inst.UpdatedAt, &inst.ReadyAt, &inst.TerminatedAt,
			&inst.ReservationCapacityID, &inst.CleanupStatus, &inst.TemplateID, &inst.DiskGB, &inst.AppAuthUser, &inst.AppAuthPass, &inst.TerminalPort,
			&inst.LastReachableAt, &inst.UnreachableSince, &inst.Image, &inst.AppPort,
			&inst.AutoTerminateHours, &inst.AutoTerminateAt,
		); err != nil {
			return nil, err
		}
		instances = append(instances, inst)
	}
	return instances, rows.Err()
}

// ListActiveInstances returns all instances with status 'running' or 'booting'.
// Used by the health monitor to check all active instances across all organizations.
func (p *Pool) ListActiveInstances(ctx context.Context) ([]Instance, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT `+instanceColumns+` FROM instances WHERE status IN ('running', 'booting')`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Instance
	for rows.Next() {
		var inst Instance
		// Scan fields must match instanceColumns order exactly.
		// Copy the scan list from ListRunningInstancesByOrg to stay in sync.
		if err := rows.Scan(
			&inst.InstanceID, &inst.OrgID, &inst.UserID, &inst.UpstreamProvider, &inst.UpstreamID,
			&inst.UpstreamIP, &inst.Hostname, &inst.WGPublicKey, &inst.WGPrivateKeyEnc, &inst.WGAddress,
			&inst.FRPRemotePort, &inst.SSHPort, &inst.Name, &inst.GPUType, &inst.GPUCount, &inst.Tier, &inst.Region,
			&inst.PricePerHour, &inst.UpstreamPricePerHour, &inst.BillingStart, &inst.BillingEnd,
			&inst.Status, &inst.ErrorReason, &inst.InternalToken,
			&inst.CreatedAt, &inst.UpdatedAt, &inst.ReadyAt, &inst.TerminatedAt,
			&inst.ReservationCapacityID, &inst.CleanupStatus, &inst.TemplateID, &inst.DiskGB, &inst.AppAuthUser, &inst.AppAuthPass, &inst.TerminalPort,
			&inst.LastReachableAt, &inst.UnreachableSince, &inst.Image, &inst.AppPort,
			&inst.AutoTerminateHours, &inst.AutoTerminateAt,
		); err != nil {
			return nil, err
		}
		instances = append(instances, inst)
	}
	return instances, rows.Err()
}

// AdminTerminatingClaimWindow is how long an admin force-terminate "claim"
// (instances.admin_terminating_at) suppresses the reconciler from acting on the
// in-flight instance (RECON-03 / D-04). It is deliberately longer than the
// 5-minute reconciler tick so a single in-flight force-terminate is reliably
// skipped across a tick boundary, yet short enough that a crashed force-
// terminate self-heals within a few ticks instead of pinning the instance out
// of reconciliation forever (T-48-12). Exported so the reconciler package
// shares the one window definition rather than duplicating a magic literal.
const AdminTerminatingClaimWindow = 15 * time.Minute

// ListReconcilableInstances returns every instance the reconciler needs to
// reason about: (a) every non-terminated row across all orgs, and (b) every
// row that was terminated within `recentWindow` (so the reconciler can
// verify the upstream actually died and re-issue Terminate if not).
//
// Rows under an active admin force-terminate claim are excluded: a row whose
// admin_terminating_at is within AdminTerminatingClaimWindow is filtered out so
// a concurrent force-terminate is not misclassified as drift (RECON-03). A
// claim older than the window IS returned (stale-claim self-heal).
//
// Returned rows are unordered. `recentWindow == 0` includes only
// non-terminated rows.
func (p *Pool) ListReconcilableInstances(ctx context.Context, recentWindow time.Duration) ([]Instance, error) {
	cutoff := time.Now().UTC().Add(-recentWindow)
	rows, err := p.pool.Query(ctx,
		`SELECT `+instanceColumns+`, admin_terminating_at
		   FROM instances
		  WHERE (status != 'terminated'
		     OR (status = 'terminated' AND terminated_at IS NOT NULL AND terminated_at >= $1))
		    AND (admin_terminating_at IS NULL
		     OR admin_terminating_at < NOW() - $2::interval)`,
		cutoff,
		AdminTerminatingClaimWindow.String(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Instance
	for rows.Next() {
		var inst Instance
		if err := rows.Scan(
			&inst.InstanceID, &inst.OrgID, &inst.UserID, &inst.UpstreamProvider, &inst.UpstreamID,
			&inst.UpstreamIP, &inst.Hostname, &inst.WGPublicKey, &inst.WGPrivateKeyEnc, &inst.WGAddress,
			&inst.FRPRemotePort, &inst.SSHPort, &inst.Name, &inst.GPUType, &inst.GPUCount, &inst.Tier, &inst.Region,
			&inst.PricePerHour, &inst.UpstreamPricePerHour, &inst.BillingStart, &inst.BillingEnd,
			&inst.Status, &inst.ErrorReason, &inst.InternalToken,
			&inst.CreatedAt, &inst.UpdatedAt, &inst.ReadyAt, &inst.TerminatedAt,
			&inst.ReservationCapacityID, &inst.CleanupStatus, &inst.TemplateID, &inst.DiskGB, &inst.AppAuthUser, &inst.AppAuthPass, &inst.TerminalPort,
			&inst.LastReachableAt, &inst.UnreachableSince, &inst.Image, &inst.AppPort,
			&inst.AutoTerminateHours, &inst.AutoTerminateAt,
			&inst.AdminTerminatingAt,
		); err != nil {
			return nil, err
		}
		instances = append(instances, inst)
	}
	return instances, rows.Err()
}

// ListStuckProvisions returns instances stuck in a pre-ready state
// (creating/provisioning/booting) whose created_at is older than cutoff —
// candidates for the durable reaper (AUDIT-05-001/002). The in-memory
// progressStatus watchdog reaps non-template instances at their own boot
// budget; this backstops the cases no watchdog covers: template deploys (which
// get no watchdog) and any provision orphaned by a process restart.
func (p *Pool) ListStuckProvisions(ctx context.Context, cutoff time.Time) ([]Instance, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT `+instanceColumns+`
		   FROM instances
		  WHERE status IN ('creating', 'provisioning', 'booting')
		    AND created_at < $1`,
		cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Instance
	for rows.Next() {
		var inst Instance
		if err := rows.Scan(
			&inst.InstanceID, &inst.OrgID, &inst.UserID, &inst.UpstreamProvider, &inst.UpstreamID,
			&inst.UpstreamIP, &inst.Hostname, &inst.WGPublicKey, &inst.WGPrivateKeyEnc, &inst.WGAddress,
			&inst.FRPRemotePort, &inst.SSHPort, &inst.Name, &inst.GPUType, &inst.GPUCount, &inst.Tier, &inst.Region,
			&inst.PricePerHour, &inst.UpstreamPricePerHour, &inst.BillingStart, &inst.BillingEnd,
			&inst.Status, &inst.ErrorReason, &inst.InternalToken,
			&inst.CreatedAt, &inst.UpdatedAt, &inst.ReadyAt, &inst.TerminatedAt,
			&inst.ReservationCapacityID, &inst.CleanupStatus, &inst.TemplateID, &inst.DiskGB, &inst.AppAuthUser, &inst.AppAuthPass, &inst.TerminalPort,
			&inst.LastReachableAt, &inst.UnreachableSince, &inst.Image, &inst.AppPort,
			&inst.AutoTerminateHours, &inst.AutoTerminateAt,
		); err != nil {
			return nil, err
		}
		instances = append(instances, inst)
	}
	return instances, rows.Err()
}

// ListInstancesDueForAutoTerminate returns every RUNNING instance whose
// per-instance runtime bound has elapsed (schema v92, GPU-60) — the candidate
// set for the engine's auto-terminate sweep.
//
// Deliberately narrow, in three ways:
//
//   - status = 'running' only. auto_terminate_at is armed at the running
//     transition, so a pre-running row cannot be due; and a row already
//     stopping/terminated/errored has either been handled or is being handled
//     by somebody else. This is also why an "unreachable" instance is included:
//     its lifecycle status column still reads 'running' (v88) and the customer
//     is still holding the machine, so its bound must still fire.
//   - `now` is passed in rather than read as NOW() so the sweep's cutoff is the
//     caller's clock — the same shape as ListStuckProvisions, and testable.
//   - It selects, it does not claim. The caller must win
//     ClaimInstanceAutoTerminate before acting on a row.
//
// `limit` bounds one sweep's work. The sweep runs INSIDE the billing tick,
// ahead of balance deduction, and each terminate costs a couple of seconds of
// upstream verify — so an unbounded due set (a backlog after an outage, or many
// instances sharing a deadline) would push every org's deduction late. Rows are
// returned oldest-deadline-first so the most overdue instance is always in the
// batch, and the remainder is picked up by the next tick 60s later. limit <= 0
// means no bound.
func (p *Pool) ListInstancesDueForAutoTerminate(ctx context.Context, now time.Time, limit int) ([]Instance, error) {
	query := `SELECT ` + instanceColumns + `
		   FROM instances
		  WHERE status = 'running'
		    AND auto_terminate_at IS NOT NULL
		    AND auto_terminate_at <= $1
		  ORDER BY auto_terminate_at ASC`
	args := []any{now}
	if limit > 0 {
		query += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Instance
	for rows.Next() {
		var inst Instance
		if err := rows.Scan(
			&inst.InstanceID, &inst.OrgID, &inst.UserID, &inst.UpstreamProvider, &inst.UpstreamID,
			&inst.UpstreamIP, &inst.Hostname, &inst.WGPublicKey, &inst.WGPrivateKeyEnc, &inst.WGAddress,
			&inst.FRPRemotePort, &inst.SSHPort, &inst.Name, &inst.GPUType, &inst.GPUCount, &inst.Tier, &inst.Region,
			&inst.PricePerHour, &inst.UpstreamPricePerHour, &inst.BillingStart, &inst.BillingEnd,
			&inst.Status, &inst.ErrorReason, &inst.InternalToken,
			&inst.CreatedAt, &inst.UpdatedAt, &inst.ReadyAt, &inst.TerminatedAt,
			&inst.ReservationCapacityID, &inst.CleanupStatus, &inst.TemplateID, &inst.DiskGB, &inst.AppAuthUser, &inst.AppAuthPass, &inst.TerminalPort,
			&inst.LastReachableAt, &inst.UnreachableSince, &inst.Image, &inst.AppPort,
			&inst.AutoTerminateHours, &inst.AutoTerminateAt,
		); err != nil {
			return nil, err
		}
		instances = append(instances, inst)
	}
	return instances, rows.Err()
}

// ClaimInstanceAutoTerminate atomically claims a due instance for the
// auto-terminate sweep, returning whether THIS caller won (schema v92, GPU-60).
//
// The claim IS the clearing of auto_terminate_at: the same predicate that made
// the row due is re-asserted inside the UPDATE, so exactly one caller can ever
// observe RowsAffected() == 1 for a given armed deadline. Every subsequent
// sweep — this process's next tick, or another process racing it — reads NULL
// and skips. That is what keeps the terminate, the billing closeout and the
// 'auto_terminated' audit row strictly once-per-instance without a second
// marker column or an advisory lock.
//
// Clearing rather than stamping is also the honest read: an instance that is
// being torn down no longer has a future deadline to advertise. The requested
// auto_terminate_hours stays on the row, so a read after the fact still shows
// what the launch asked for.
//
// A lost claim is NOT an error — the sweep skips the row.
func (p *Pool) ClaimInstanceAutoTerminate(ctx context.Context, instanceID string, now time.Time) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE instances
		    SET auto_terminate_at = NULL, updated_at = NOW()
		  WHERE instance_id = $1
		    AND status = 'running'
		    AND auto_terminate_at IS NOT NULL
		    AND auto_terminate_at <= $2`,
		instanceID, now,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RearmInstanceAutoTerminate puts a spent auto-terminate claim back (schema
// v92, GPU-60): it restores the deadline the sweep cleared, so the next tick
// re-selects the instance and tries again.
//
// It exists because the claim is spent BEFORE any durable outcome. The sweep
// clears auto_terminate_at, then calls Engine.Terminate — and Terminate can
// fail before it changes anything (a GetInstance blip, a lost status flip, a
// cancelled context during an ECS rollout: the ticker runs on the serve context
// with no per-tick timeout). Without this the row would sit there running,
// unbounded and never retried — silently the exact unbounded instance the
// feature exists to stop.
//
// Guarded on status = 'running' AND auto_terminate_at IS NULL, which is what
// keeps exactly-once intact:
//   - a terminate that DID land moved the row off 'running', so the re-arm is a
//     no-op and a terminated instance can never acquire a live deadline;
//   - a concurrent sweep that already re-armed leaves the column non-NULL, so
//     this cannot overwrite a fresher deadline.
//
// The restored value is the ORIGINAL deadline, not now+hours: the bound already
// elapsed, and pushing it forward would silently extend a run the customer
// asked to have stopped.
func (p *Pool) RearmInstanceAutoTerminate(ctx context.Context, instanceID string, at time.Time) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE instances
		    SET auto_terminate_at = $2, updated_at = NOW()
		  WHERE instance_id = $1
		    AND status = 'running'
		    AND auto_terminate_at IS NULL`,
		instanceID, at,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateInstanceLastReachable stamps instances.last_reachable_at after the
// health monitor's tunnel probe confirms the SSH path end-to-end (schema v80,
// GPU-49). Called on every successful probe, so this is a hot-ish write — one
// UPDATE per running instance per monitor tick.
//
// Deliberately does NOT touch updated_at: that column drives the customer-facing
// "last changed" reading, and a probe confirming nothing changed is not a change.
// Deliberately does NOT guard on status either — the probe only ever runs for
// running instances, and a concurrent terminate racing the stamp is harmless
// (the column is an observation, not a lifecycle claim).
//
// Silently affects zero rows if the instance was deleted; the caller logs and
// continues rather than treating that as a failure.
func (p *Pool) UpdateInstanceLastReachable(ctx context.Context, instanceID string, t time.Time) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE instances SET last_reachable_at = $2 WHERE instance_id = $1`,
		instanceID, t,
	)
	return err
}

// SetInstanceUnreachableSince stamps instances.unreachable_since with T0 of a
// newly CONFIRMED tunnel outage (schema v88, GPU-77) — the first failed probe
// of the run that crossed the confirmation threshold, not the probe that
// crossed it.
//
// FIRST ANNOUNCE WINS, by COALESCE rather than by a WHERE guard, so the write
// stays a single unconditional statement with no read-modify-write race. That
// matters because T0 is load-bearing twice over: it bounds the credited window
// AND it is half of the credit's idempotency reference. A gpuctl restart
// mid-outage re-confirms the same dead tunnel and would otherwise move T0
// forward — shortening what the customer is refunded and minting a SECOND
// reference for one outage, which the unique index could no longer collapse.
//
// Deliberately does NOT touch updated_at (same rationale as
// UpdateInstanceLastReachable) and does not guard on status: only the probe
// calls it, and the probe only runs for running instances. A row that races
// into terminated keeps an inert flag — the status mapping only overrides
// "running".
func (p *Pool) SetInstanceUnreachableSince(ctx context.Context, instanceID string, t time.Time) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE instances SET unreachable_since = COALESCE(unreachable_since, $2) WHERE instance_id = $1`,
		instanceID, t,
	)
	return err
}

// ClearInstanceUnreachableSince returns an instance to "reachable" after a
// successful probe (schema v88, GPU-77): the customer-facing status reverts
// from "unreachable" to "running", and the next outage gets a fresh T0.
//
// Callers must credit the closing window BEFORE clearing — the flag is the
// restart-durable record of what is owed, so clearing first and crashing
// second would lose it.
func (p *Pool) ClearInstanceUnreachableSince(ctx context.Context, instanceID string) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE instances SET unreachable_since = NULL WHERE instance_id = $1`,
		instanceID,
	)
	return err
}

// UpdateInstanceStatus atomically updates an instance's status using optimistic locking.
// It only updates the row if the current status matches fromStatus, preventing race conditions.
// Returns true if the row was updated, false if the status was concurrently changed.
func (p *Pool) UpdateInstanceStatus(ctx context.Context, instanceID, fromStatus, toStatus string) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE instances SET status = $1, updated_at = NOW() WHERE instance_id = $2 AND status = $3`,
		toStatus, instanceID, fromStatus,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetInstanceRunning atomically transitions an instance to running from any
// non-terminal pre-running state (creating, provisioning, or booting),
// recording the ready_at timestamp and upstream IP. Returns (updated,
// prevStatus, err): updated=false when the instance was already running /
// stopped / terminated / errored / stopping, in which case prevStatus is "".
//
// prevStatus tells the caller whether the engine's status-poll path
// already created a billing session at the booting transition (prev=
// "booting") or whether the callback skipped booting and the caller
// must start the billing session itself (prev="creating"|"provisioning").
//
// Why accept creating/provisioning, not just booting: fast-booting
// providers can fire the ready callback within seconds of
// provision — before the engine's status-poll loop has had a chance to
// observe the upstream pod and advance through booting. Pre-fix, those
// callbacks landed while status was still "provisioning", were rejected,
// and the instance got stuck in "starting" forever even though SSH was
// already working.
//
// The per-instance runtime bound (schema v92, GPU-60) is armed HERE and only
// here: auto_terminate_at becomes NOW() + auto_terminate_hours for a row that
// asked for one, and stays NULL for every row that did not. This is the one
// statement in the codebase that both knows the instance just became usable and
// is guarded to fire exactly once per launch (the status IN (...) predicate),
// which is what makes "N hours of RUNTIME" mean runtime rather than "N hours
// from the create call, minus however long provisioning took".
//
// A pending startup GPU check verdict (v115 readiness_failure) is cleared in
// the same statement: it described a pre-running placement, and a running row
// must never carry one the poll loop could later act on.
func (p *Pool) SetInstanceRunning(ctx context.Context, instanceID, upstreamIP string) (updated bool, prevStatus string, err error) {
	// CTE-based UPDATE that returns the row's status BEFORE the update,
	// captured atomically at statement start. Postgres CTE snapshot
	// semantics ensure prev.status reflects the state the WHERE clause
	// matched against, even under concurrent updates.
	err = p.pool.QueryRow(ctx,
		`WITH prev AS (
			SELECT status FROM instances WHERE instance_id = $1
		)
		UPDATE instances SET status = 'running', ready_at = NOW(),
			auto_terminate_at = CASE
				WHEN auto_terminate_hours IS NOT NULL
				THEN NOW() + (auto_terminate_hours * INTERVAL '1 hour')
			END,
			upstream_ip = COALESCE($2::INET, upstream_ip),
			readiness_failure = NULL, readiness_failed_at = NULL, updated_at = NOW()
		FROM prev
		WHERE instances.instance_id = $1
		  AND instances.status IN ('creating', 'provisioning', 'booting')
		RETURNING prev.status`,
		instanceID, nilIfEmpty(upstreamIP),
	).Scan(&prevStatus)

	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, prevStatus, nil
}

// nilIfEmpty returns nil for empty strings, allowing nullable DB columns.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// RebindInstanceUpstream re-points a mid-provision instance row at a freshly
// provisioned replacement upstream (the engine's boot-stall rescue): new
// provider / upstream id / region / prices, cleared upstream_ip, disk_gb
// re-stamped for the new provider's honesty rule (nil = provider-determined),
// and status reset to 'provisioning' so the poll loop drives the new boot
// from the top. Guarded on the row still being in a mid-provision state — a
// concurrent terminate/error wins the race and this returns false, in which
// case the CALLER must tear down the replacement upstream it just created
// (the row now points at nothing we track otherwise). A pending startup GPU
// check verdict (v115 readiness_failure) is cleared too: it was about the old
// host, and acting on it would release the healthy replacement.
func (p *Pool) RebindInstanceUpstream(ctx context.Context, instanceID, upstreamProvider, upstreamID, region string, pricePerHour, upstreamPricePerHour float64, diskGB *int) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE instances
		 SET upstream_provider = $2, upstream_id = $3, upstream_ip = NULL,
		     region = $4, price_per_hour = $5, upstream_price_per_hour = $6,
		     disk_gb = $7, status = 'provisioning',
		     readiness_failure = NULL, readiness_failed_at = NULL, updated_at = NOW()
		 WHERE instance_id = $1 AND status IN ('provisioning', 'booting')`,
		instanceID, upstreamProvider, upstreamID, region, pricePerHour, upstreamPricePerHour, diskGB,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetInstanceUpstreamIP records the provider-reported public IP without
// touching status. The poll loop calls this when the IP first appears: the
// booting->running flip itself is owned by the tunnel_up ready callback, but
// the IP is ops-visibility data known minutes earlier (SSH coords derive from
// frp_remote_port, never from upstream_ip). No-op on empty IP.
func (p *Pool) SetInstanceUpstreamIP(ctx context.Context, instanceID, upstreamIP string) error {
	if upstreamIP == "" {
		return nil
	}
	_, err := p.pool.Exec(ctx,
		`UPDATE instances SET upstream_ip = $2::INET, updated_at = NOW()
		 WHERE instance_id = $1`,
		instanceID, upstreamIP,
	)
	return err
}

// SetInstanceSSHPort records the public SSH port of a direct-connect instance
// (no FRP tunnel), the port the supplier mapped to container port 22 at
// upstream_ip. A port outside 1..65535 is ignored.
func (p *Pool) SetInstanceSSHPort(ctx context.Context, instanceID string, port int) error {
	if port <= 0 || port > 65535 {
		return nil
	}
	_, err := p.pool.Exec(ctx,
		`UPDATE instances SET ssh_port = $2, updated_at = NOW()
		 WHERE instance_id = $1`,
		instanceID, port,
	)
	return err
}

// TerminateInstance atomically terminates an instance, setting billing_end and
// terminated_at timestamps. Returns false if the instance is already in a terminal
// state (idempotent). Uses NOT IN for idempotent termination (INST-06).
func (p *Pool) TerminateInstance(ctx context.Context, instanceID string) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE instances SET status = 'terminated', terminated_at = NOW(), billing_end = NOW(), updated_at = NOW()
		 WHERE instance_id = $1 AND status NOT IN ('terminated')`,
		instanceID,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateInstanceName updates the display name of an instance scoped to an organization.
func (p *Pool) UpdateInstanceName(ctx context.Context, instanceID, orgID string, name *string) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE instances SET name = $1, updated_at = NOW() WHERE instance_id = $2 AND org_id = $3`,
		name, instanceID, orgID)
	return err
}

// SetInstanceError sets an instance to error state with a human-readable reason.
//
// Guarded for idempotency (GPU-45): the update only lands when the row is not
// already closed out —
//
//   - a live (non-error, non-terminated) row flips to error and records the
//     reason (the classic engine failure paths);
//   - an error row with NO reason yet gets its reason written (the health
//     monitor / spot watcher / reaper flows flip status via
//     UpdateInstanceStatus first, then write the reason here);
//   - an error row that already carries a reason is left untouched (first
//     error wins), and a terminated row is never flipped back to error.
//
// Returns updated=true iff the row changed. Callers use this as the
// exactly-once guard for instance.failed webhook emission — a retried
// failure path (e.g. a re-run Terminate that keeps failing upstream while
// the row already sits in error) must not re-emit.
//
// That same exactly-once flip is what the v82 provision-failure ledger write
// rides: recording happens ONLY on the transition, so a failure path that runs
// three times against an already-error row contributes one event, not three.
// Hooking here rather than at the ~10 call sites (engine, health monitor, spot
// watcher) is deliberate — one hook, no call-site churn, no way to add a caller
// that forgets to record. The ledger write is BEST-EFFORT: it is logged and
// dropped on failure, because a metrics insert must never turn a recorded
// instance failure into a reported error for the caller.
func (p *Pool) SetInstanceError(ctx context.Context, instanceID, reason string) (bool, error) {
	// RETURNING gives the ledger gpu_type/gpu_count/org_id off the same
	// statement — no second read on a failure path.
	var gpuType string
	var gpuCount int
	var orgID string
	err := p.pool.QueryRow(ctx,
		`UPDATE instances SET status = 'error', error_reason = $1, updated_at = NOW()
		 WHERE instance_id = $2
		   AND (status NOT IN ('error', 'terminated')
		        OR (status = 'error' AND error_reason IS NULL))
		 RETURNING gpu_type, gpu_count, COALESCE(org_id::TEXT, '')`,
		reason, instanceID,
	).Scan(&gpuType, &gpuCount, &orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Guard held: already error-with-reason, or terminated. Not an error.
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if recErr := p.RecordProvisionFailure(ctx, ProvisionFailureEvent{
		GPUType:      gpuType,
		GPUCount:     gpuCount,
		OrgID:        orgID,
		InstanceID:   instanceID,
		StockRelated: StockRelatedReason(reason),
		Reason:       reason,
	}); recErr != nil {
		slog.Warn("provision failure ledger: record on instance error failed",
			"instance_id", instanceID, "error", recErr)
	}
	return true, nil
}

// SetInstanceStatusReason records a customer-facing explanation on a row
// WITHOUT touching its status. This is the deliberate-stop counterpart to
// SetInstanceError: a spending-cap termination is a policy action, not a
// failure, so the row must still land in 'terminated' while carrying a reason
// the dashboard and the public API (status_reason) can show. Without it a
// capped org's instances simply disappear with no explanation.
//
// Only writes when the row has no reason yet, so a genuine earlier error
// (recorded by SetInstanceError) is never overwritten by a later stop.
func (p *Pool) SetInstanceStatusReason(ctx context.Context, instanceID, reason string) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE instances SET error_reason = $1, updated_at = NOW()
		 WHERE instance_id = $2 AND error_reason IS NULL`,
		reason, instanceID,
	)
	return err
}
