// Package db's operations.go implements the public-API operations FSM
// (Phase 18, migration v13). Operations track long-running async writes —
// instance.create / instance.delete — exposed at GET /v1/operations/{id}.
//
// FSM contract:
//
//	pending -> in_progress -> {succeeded, failed, cancelled}
//
// The operations.version column is the optimistic-lock guard. Every UPDATE
// is gated by `WHERE version = $prev` and increments via `version + 1`.
// `RETURNING version` surfaces the new value back to the caller; callers
// MUST reassign the local copy of `op.Version` after each successful
// update so the next UPDATE uses the correct prevVersion. Failure to do so
// silently corrupts the FSM (MAJOR 13 in plan-check).
//
// Issuer scoping: GetOperation requires (operation_id, api_key_id) so an
// operation issued by key A is invisible to key B even within the same org.
// This matches D-08's "operations are issuer-scoped" contract — admins
// rotating an API key do not lose visibility (the row stays in the DB),
// they just have to look it up via the dashboard.
//
// Keyless operations (v100): api_key_id is NULLABLE, because a launch a
// customer authorized by their org ROLE on the dashboard has no key to record.
// SQL's NULL never equals a bound value, so those rows are invisible to the
// issuer-scoped read above — which is the intended reading, not a gap: they
// were issued to nobody's key. The org-scoped GetAutolaunchOperationForOrg
// exists precisely to read them.

package db

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrOperationStateChanged signals an optimistic-lock conflict on
// operations.version — i.e. another goroutine bumped the version between
// the caller's read and write. The instance-handler goroutine treats this
// as a fatal "FSM corrupted" signal and aborts (rather than continuing
// with a stale version, which would write past the real terminal state).
var ErrOperationStateChanged = errors.New("operation state changed concurrently")

// Operation maps a single row of the operations table.
//
// State is one of pending|in_progress|succeeded|failed|cancelled (D-09).
// Kind is one of "instance.create" | "instance.delete" in v1; future
// kinds (e.g. "instance.resume") slot in without schema changes.
//
// ResourceID is the instance_id once the engine has created the row;
// nil for pending/in_progress operations whose engine call has not yet
// returned. ErrorCode + ErrorDetail are populated only on failure.
type Operation struct {
	OperationID string
	// APIKeyID is the key that issued this operation, or "" when the launch was
	// authorized by an ORG ROLE rather than by a credential — an auto-launch
	// rule consented to on the dashboard, whose principal holds no key at all
	// (review A CR-02). The column is NULLABLE since v100 and "" round-trips
	// through NULL: apiKeyIDArg binds nil, scanOperation maps NULL back to "".
	//
	// It stays a plain string rather than a *string so no caller has to learn
	// the difference; the only place the distinction is visible is the
	// issuer-scoped GetOperation, which cannot match a NULL and therefore never
	// returns a keyless row. That is deliberate — those rows belong to the org,
	// not to a key, and are read through GetAutolaunchOperationForOrg.
	APIKeyID    string
	OrgID       string
	Kind        string // "instance.create" | "instance.delete"
	State       string // pending | in_progress | succeeded | failed | cancelled
	ResourceID  *string
	ErrorCode   *string
	ErrorDetail *string
	Metadata    json.RawMessage
	// Result is the terminal-success payload (schema v21). For a template
	// deploy it carries the surface-once app credential
	// (app_basic_auth_user/app_basic_auth_pass/app_url) — set by the create-FSM
	// success branch and returned by GET /v1/operations/{id} exactly once. NULL
	// for non-template operations. NEVER surfaced on any GET /v1/instances read.
	Result      json.RawMessage
	Version     int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

const operationColumns = `operation_id, api_key_id, org_id, kind, state, resource_id,
        error_code, error_detail, metadata, result, version, created_at, updated_at, completed_at`

func scanOperation(row pgx.Row) (*Operation, error) {
	var o Operation
	// api_key_id is scanned through a pointer because the column is NULL for a
	// keyless (org-role-authorized) operation since v100; scanning a NULL
	// straight into a string is a driver error, not a zero value. NULL maps
	// back to "" — the same value the caller bound — so the round trip is
	// lossless. This is the ONLY column on this table with that treatment.
	var apiKeyID *string
	err := row.Scan(
		&o.OperationID, &apiKeyID, &o.OrgID, &o.Kind, &o.State, &o.ResourceID,
		&o.ErrorCode, &o.ErrorDetail, &o.Metadata, &o.Result, &o.Version,
		&o.CreatedAt, &o.UpdatedAt, &o.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	if apiKeyID != nil {
		o.APIKeyID = *apiKeyID
	}
	return &o, nil
}

// operationInsertSQL creates a pending operation, letting the CALLER supply the
// primary key when it has one.
//
// COALESCE($1::uuid, gen_random_uuid()) is the whole of the seam: bind NULL and
// Postgres assigns the id exactly as before, bind an id and that is the row's
// id. It exists for ONE caller — the auto-launch firer, which must write a
// durable rule-to-operation link BEFORE the provider call can begin and
// therefore has to know the operation id before the row exists. Reading the id
// back out afterwards is too late: public.CreateInstance kicks the provisioning
// engine on a detached goroutine, so by the time it returns the money may
// already be moving (review B CR-01).
//
// It is NOT a customer-facing field, and there is deliberately no path from a
// request body to it: a client that could choose operation ids could collide
// with, or guess at, another tenant's operation id space.
const operationInsertSQL = `INSERT INTO operations (operation_id, api_key_id, org_id, kind, state, metadata)
                 VALUES (COALESCE($1::uuid, gen_random_uuid()), $2, $3, $4, 'pending', $5)
                 RETURNING operation_id, created_at, updated_at, version, state`

// operationIDArg renders a caller-supplied primary key for the wire: NULL when
// absent (so the column default applies), the value otherwise. An empty string
// bound directly would fail the ::uuid cast rather than meaning "no preference".
func operationIDArg(o *Operation) any {
	if o.OperationID == "" {
		return nil
	}
	return o.OperationID
}

// apiKeyIDArg renders operations.api_key_id for the wire: NULL when the
// operation has no issuing key, the value otherwise.
//
// The empty string is not a substitute for NULL here, in either direction.
// Bound directly it fails the ::uuid cast with SQLSTATE 22P02 — which is
// exactly how a dashboard-consented auto-launch rule used to die at the FK
// (review A CR-02) — and stored as a literal it would claim a key that does not
// exist. NULL is the column's own way of saying "authorized by an org role, not
// by a credential", matching autolaunch_rules.creator_api_key_id.
func apiKeyIDArg(o *Operation) any {
	if o.APIKeyID == "" {
		return nil
	}
	return o.APIKeyID
}

// CreateOperation inserts a new operation in 'pending' state. The fields
// OrgID, Kind, and Metadata MUST be set on the input struct; CreatedAt,
// UpdatedAt, Version, and State are populated via RETURNING so the caller
// observes the version=1 baseline that subsequent UPDATEs lock against.
//
// APIKeyID is OPTIONAL since v100: leave it empty for a launch authorized by an
// org role rather than by a credential, and the column is bound NULL. It is not
// a shortcut for "I did not bother" — every HTTP caller has a key and passes
// it; the empty case exists for exactly one principal, the auto-launch firer's
// dashboard rule, which provably has none.
//
// OperationID is populated via RETURNING as well, but it is an INPUT when
// non-empty: a caller that already committed to an id elsewhere (the auto-launch
// firer's pre-provision checkpoint) passes it in and gets the same value back.
// Leave it zero and Postgres assigns one, which is what every other caller does.
func (p *Pool) CreateOperation(ctx context.Context, o *Operation) error {
	return p.pool.QueryRow(ctx, operationInsertSQL,
		operationIDArg(o), apiKeyIDArg(o), o.OrgID, o.Kind, o.Metadata,
	).Scan(&o.OperationID, &o.CreatedAt, &o.UpdatedAt, &o.Version, &o.State)
}

// GetOperation returns the row matching (operation_id, api_key_id) — the
// scope is intentionally narrower than (operation_id, org_id) so a key
// rotation does not let key B observe operations issued by key A. Cross-key
// access returns ErrNotFound (handler maps to 404, matching the SSH-key
// idempotent-destroy semantics in PRIV-08 / D-07).
func (p *Pool) GetOperation(ctx context.Context, operationID, apiKeyID string) (*Operation, error) {
	row := p.pool.QueryRow(ctx,
		`SELECT `+operationColumns+`
                 FROM operations
                 WHERE operation_id = $1 AND api_key_id = $2`,
		operationID, apiKeyID,
	)
	o, err := scanOperation(row)
	if isNoRowsOrInvalidID(err) {
		return nil, ErrNotFound
	}
	return o, err
}

// terminalStates is the set of states for which completed_at is recorded.
// In-line CHECK in the schema also enforces the membership.
func isTerminalOperationState(s string) bool {
	switch s {
	case "succeeded", "failed", "cancelled":
		return true
	}
	return false
}

// UpdateOperationState transitions state with optimistic lock on version.
// Returns the post-update version on success; (0, ErrOperationStateChanged)
// when the WHERE version=$prev clause matches no row (another goroutine
// already advanced past prevVersion).
//
// CALLERS MUST reassign `op.Version = newVersion` so subsequent UPDATEs
// use the correct prevVersion — failing to do so silently corrupts the
// FSM. The contract is enforced at code-review time via grep:
// `! grep -q '_ = deps.DB.UpdateOperationState'` in handler files.
//
// completed_at is set to NOW() on transitions to terminal states
// (succeeded, failed, cancelled) and left NULL on transitions to
// in_progress. The schema's CHECK constraint ensures an out-of-FSM state
// like 'unknown' would be rejected at the DB layer, so we don't validate
// the input here.
func (p *Pool) UpdateOperationState(ctx context.Context, operationID string, prevVersion int, newState string) (int, error) {
	completedClause := "NULL"
	if isTerminalOperationState(newState) {
		completedClause = "NOW()"
	}
	var newVersion int
	err := p.pool.QueryRow(ctx,
		`UPDATE operations
                 SET state = $1,
                     version = version + 1,
                     updated_at = NOW(),
                     completed_at = `+completedClause+`
                 WHERE operation_id = $2 AND version = $3
                 RETURNING version`,
		newState, operationID, prevVersion,
	).Scan(&newVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrOperationStateChanged
	}
	return newVersion, err
}

// SetOperationResource attaches a resource_id (instance_id) to an
// operation and bumps the version. Optimistic-locked exactly like
// UpdateOperationState — same return contract: (newVersion, nil) on
// success, (0, ErrOperationStateChanged) on conflict. State is left
// untouched; callers typically follow up with UpdateOperationState to
// transition pending/in_progress → succeeded.
func (p *Pool) SetOperationResource(ctx context.Context, operationID string, prevVersion int, resourceID string) (int, error) {
	var newVersion int
	err := p.pool.QueryRow(ctx,
		`UPDATE operations
                 SET resource_id = $1,
                     version = version + 1,
                     updated_at = NOW()
                 WHERE operation_id = $2 AND version = $3
                 RETURNING version`,
		resourceID, operationID, prevVersion,
	).Scan(&newVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrOperationStateChanged
	}
	return newVersion, err
}

// SetOperationResult attaches the terminal-success result payload (schema v21)
// and bumps the version. Optimistic-locked exactly like SetOperationResource —
// same return contract: (newVersion, nil) on success, (0,
// ErrOperationStateChanged) on conflict. State is left untouched; the caller
// follows up with UpdateOperationState(...,"succeeded").
//
// Used by the template-deploy create-FSM to deliver the surface-once app
// credential (app_basic_auth_user/app_basic_auth_pass/app_url) through
// GET /v1/operations/{id}. result is opaque JSON the handler round-trips
// verbatim; it MUST NOT contain any value that the redacted metadata snapshot
// scrubs — the credential is generated by the engine, not echoed from the
// request, so there is no secret-from-request leak vector here.
func (p *Pool) SetOperationResult(ctx context.Context, operationID string, prevVersion int, result json.RawMessage) (int, error) {
	var newVersion int
	err := p.pool.QueryRow(ctx,
		`UPDATE operations
                 SET result = $1,
                     version = version + 1,
                     updated_at = NOW()
                 WHERE operation_id = $2 AND version = $3
                 RETURNING version`,
		result, operationID, prevVersion,
	).Scan(&newVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrOperationStateChanged
	}
	return newVersion, err
}

// SetOperationFailure transitions to 'failed' with structured error
// metadata and bumps the version. Optimistic-locked. completed_at is
// stamped at NOW() since 'failed' is terminal.
//
// code is one of the stable codes from the public.codeRegistry
// (e.g. "operation_failed", "insufficient_balance"). detail is the raw
// engine error string — surfaced verbatim only via Operation.Error.Detail
// so SDKs can switch on Code while still rendering a human-readable
// reason.
func (p *Pool) SetOperationFailure(ctx context.Context, operationID string, prevVersion int, code, detail string) (int, error) {
	var newVersion int
	err := p.pool.QueryRow(ctx,
		`UPDATE operations
                 SET state = 'failed',
                     version = version + 1,
                     updated_at = NOW(),
                     completed_at = NOW(),
                     error_code = $1,
                     error_detail = $2
                 WHERE operation_id = $3 AND version = $4
                 RETURNING version`,
		code, detail, operationID, prevVersion,
	).Scan(&newVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrOperationStateChanged
	}
	return newVersion, err
}
