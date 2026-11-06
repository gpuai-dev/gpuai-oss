// Package authz holds the single role-authorization rule for Phase 39
// (SEC-01/SEC-05/SEC-07, decision D-04).
//
// Every terminate / revoke-key / mint-full-access role decision — on BOTH the
// API-key public surface (internal/api/public) — flows through Authorize() below. This is
// the structural fix for research Pitfall 1 ("fixed three call sites, missed
// the fourth"): one rule, one table-driven test matrix, identical 403
// semantics everywhere.
//
// Why a standalone package: internal/api imports internal/api/public (route
// mounting), so the public package cannot import internal/api back (import
// cycle + the TestNoInternalAPIImports boundary). The rule therefore lives
// here, below both surfaces; internal/api/authorize.go re-exports it via
// type aliases so the session-surface call sites and the Plan 02 test matrix
// are unchanged.
//
// Shadow-mode contract (SEC-07): Authorize() always computes the real
// decision; the `enforce` argument is carried back verbatim on
// Decision.Enforce so call sites can branch:
//
//   - enforce == false (shadow, the AUTHZ_ENFORCE default): callers MUST emit
//     the structured "authz decision" diff line (DecisionLogMsg) and then
//     PROCEED (do not 403). The operator greps would_deny=true enforce=false
//     on demo to validate zero unintended denials before flipping.
//   - enforce == true: callers 403 ("forbidden") when !Allowed, byte-matching
//     the existing RequireAdmin semantics on the session surface.
//
// Fail-closed invariants: an unknown/empty role is member; an unknown action
// is DENY; an empty principal UserID never matches a resource owner.
package authz

// Actions understood by Authorize(). Anything else is denied (fail-closed).
const (
	// ActionTerminate is instance termination (SEC-05 / D-04): members may
	// terminate only instances they created; admin+ terminates org-wide.
	ActionTerminate = "terminate"
	// ActionRevokeKey is API-key revocation (SEC-04): members may revoke only
	// their own keys; admin+ revokes org-wide.
	ActionRevokeKey = "revoke_key"
	// ActionMintFullAccess is minting a full_access-scoped API key (SEC-03):
	// admin+ only — members can NEVER mint full_access, even "for themselves".
	ActionMintFullAccess = "mint_full_access"
	// ActionUpdate is instance mutation (rename) on the dashboard surface:
	// members may update only instances they created; admin+ org-wide. Added
	// in Plan 05 when handleUpdateInstance's inline owner check migrated
	// onto Authorize() (D-04: no inline role comparisons at call sites).
	ActionUpdate = "update"
	// ActionReadAudit is reading an instance's audit/activity feed: members
	// may read only their own instances' history; admin+ org-wide. Added in
	// Plan 05 when handleListInstanceActivity's inline owner check migrated
	// onto Authorize().
	ActionReadAudit = "read_audit"
)

// DecisionLogMsg is the fixed slog message for every Authorize() shadow/
// audit line. Plan 06's shadow-soak instructions and Plan 07's enforcement-
// flip runbook grep for this constant plus the would_deny/enforce fields.
const DecisionLogMsg = "authz decision"

// Principal is the authenticated actor an Authorize() decision is made about.
//
// UserID is the INTERNAL user UUID (users.user_id), never an external id —
// resource ownership columns (instances.user_id, api_keys.user_id) store the
// internal UUID, and ownership comparison must be apples-to-apples.
//
// Role is the normalized DB role string ('owner'/'admin'/'member') on the
// API-key surface, or derived from JWT claims on the session surface. IsAdmin
// is precomputed by the constructors so call sites cannot fumble the
// owner-or-admin ("admin+", D-01) rule.
type Principal struct {
	UserID  string
	Role    string
	IsAdmin bool
}

// Resource is the object of an Authorize() decision. OwnerUserID is the
// owning user's internal UUID (instance.user_id or api_key.user_id); empty
// means "no recorded owner", which never matches any principal (fail-closed).
type Resource struct {
	OwnerUserID string
}

// Decision is Authorize()'s result. Enforce mirrors the enforce argument
// so callers can branch shadow-vs-enforce without re-reading config. Reason
// is a stable machine-greppable token for the shadow diff line, never
// user-facing copy.
type Decision struct {
	Allowed bool
	Reason  string
	Enforce bool
}

// IsAdminPlus reports whether a normalized DB role carries admin-or-better
// power ("admin+", D-01: 'owner' is reserved but must already map to
// admin-equivalent power wherever it appears). Case-sensitive on purpose:
// DB roles are normalized lowercase (db.normalizeRole + the users_role_check
// constraint); any other casing is not a DB role and fails closed.
func IsAdminPlus(role string) bool {
	return role == "admin" || role == "owner"
}

// PrincipalFromAPIKey builds a Principal on the API-key public surface.
// dbRole is the role resolved from users.role (SEC-01: the DB — not any JWT —
// is authoritative here), attached to context by the auth middlewares. An
// empty/unrecognized role yields a member-power principal (fail-closed).
func PrincipalFromAPIKey(userID, dbRole string) Principal {
	return Principal{
		UserID:  userID,
		Role:    dbRole,
		IsAdmin: IsAdminPlus(dbRole),
	}
}

// Authorize is the single role-decision point for Phase 39 (D-04). All
// terminate/revoke/mint authorization on both surfaces MUST route through
// this function — do not add role comparisons at call sites.
//
// Rules:
//   - admin+ (Principal.IsAdmin): ALLOW for terminate / revoke_key /
//     mint_full_access — org-wide power.
//   - mint_full_access: admin+ only; members are denied unconditionally
//     (SEC-03).
//   - terminate / revoke_key / update / read_audit: members are allowed on
//     their OWN resources (p.UserID == res.OwnerUserID, both non-empty),
//     denied otherwise.
//   - any other action: DENY (fail-closed) — even for admin+. New actions
//     must be added to the switch and the test matrix in the same change.
//
// The enforce argument is carried back verbatim on the decision; it never
// changes Allowed/Reason (shadow mode must be a faithful preview).
func Authorize(p Principal, action string, res Resource, enforce bool) Decision {
	d := Decision{Enforce: enforce}

	switch action {
	case ActionMintFullAccess:
		if p.IsAdmin {
			d.Allowed = true
			d.Reason = "admin_plus"
		} else {
			d.Reason = "member_cannot_mint_full_access"
		}
	case ActionTerminate, ActionRevokeKey, ActionUpdate, ActionReadAudit:
		switch {
		case p.IsAdmin:
			d.Allowed = true
			d.Reason = "admin_plus"
		case p.UserID != "" && p.UserID == res.OwnerUserID:
			d.Allowed = true
			d.Reason = "resource_owner"
		default:
			d.Reason = "member_not_owner"
		}
	default:
		d.Reason = "unknown_action"
	}

	return d
}
