package public

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ScopeLevel is the per-resource access tier.
type ScopeLevel string

const (
	ScopeNone  ScopeLevel = "none"
	ScopeRead  ScopeLevel = "read"
	ScopeWrite ScopeLevel = "write"
)

// KnownResources is the canonical resource family list (D-16). Adding a new
// resource requires updating this list AND the OpenAPI spec (Plan 18-10).
// The TestFullAccessCoversAllResources guard test (Pitfall 7) iterates this
// list to ensure shortcut handling stays in sync.
//
// "serverless" added by Phase 20 Plan 07 (SERV-05). Existing keys carrying
// {"full_access": true} auto-grant serverless:write via the FullAccess
// short-circuit in Allows below; no migration required.
//
// "community" added by Phase 55 Plan 03 (ONBD-01). Same story: existing
// {"full_access": true} keys auto-grant community:write via the FullAccess
// short-circuit — no key migration — while per-resource keys fail closed
// (absent from the map => deny). Per the D-16 comment above, the OpenAPI spec
// is updated in the SAME change (Plan 55-03 Task 2).
// "watch" added by Phase 86 Plan 05 (MKT-05). SEC-03 decision, recorded
// explicitly because the standard/full_access short-circuit auto-grants
// watch:write to every key already issued: that auto-grant is SAFE for this
// resource because nothing on it can spend. A watch rule is a notification
// preference — no field on it reaches the launch path, and escalating a watch
// into unattended spending is structurally impossible rather than merely
// disallowed, because auto-launch is a SEPARATE resource with its own
// endpoints and its own spend-consent gate (D-18). An agent holding a standard
// key can therefore create all the watch rules it likes and still cannot buy a
// GPU with them.
//
// "market" added by Phase 86 Plan 07 (MKT-03). SEC-03 decision, recorded
// explicitly because the standard/full_access short-circuit auto-grants
// market:write to every key already issued: that auto-grant is SAFE because
// `market` is a READ-ONLY family. It has exactly one route, a GET over the
// price/stock history series; there is no write handler for a market:write
// grant to reach, nothing on it mutates state, and nothing on it can spend. A
// standard-scoped key gaining `market` gains the ability to read a chart and
// nothing else.
//
// "autolaunch" added by Phase 86 Plan 11 (MKT-09). SEC-03 decision, and the
// FIRST resource where the answer is NO: see explicitWriteGrantResources below.
// `autolaunch:read` behaves like every other resource (listing and inspecting
// standing rules is not spend), but `autolaunch:write` is EXPLICIT-GRANT-ONLY —
// the full_access, standard and read_only short-circuits do not reach it.
var KnownResources = []string{"instances", "ssh_keys", "billing", "webhooks", "serverless", "community", "watch", "market", "autolaunch"}

// explicitWriteGrantResources lists resources whose WRITE level is never
// covered by the full_access / standard / read_only short-circuits. Only an
// explicit per-resource map entry ({"autolaunch":"write"}) grants them.
//
// WHY autolaunch IS HERE and instances is not. `instances:write` is REAL-TIME
// authority: spend happens only when the key holder issues a request, with
// someone present, and abuse shows up in billing at that moment.
// `autolaunch:write` is a STANDING, FROZEN, UNATTENDED purchase order that
// fires days or weeks later with nobody watching. That is a different authority
// CLASS, not the same authority on a delay. A blanket short-circuit would hand
// every key ever issued — including keys minted long before this phase existed,
// whose holders were never notified — standing spend authority the moment this
// ships. That is exactly D-18's "one field value flips a free notification into
// unattended spending" risk, delivered via scope instead of via field.
//
// The per-rule ceiling, the max runtime, the org burn cap and the spending
// limit all still apply. They bound the SIZE of an unattended purchase; they do
// not substitute for consenting to unattended purchasing at all.
//
// An earlier draft argued the opposite (that instances:write already implies
// this) and was rejected on review. Do not restore it. If a future reviewer
// concludes the carve-out is wrong, the correct move is to re-decide it here
// with the reasoning written down, not to delete the map and let
// TestExplicitGrantOnlyResources go green by accident.
//
// READ is deliberately NOT carved out, and neither is DELETE-or-disarm, which
// the autolaunch routes gate on `autolaunch:read` precisely so that CANCELLING
// standing spend authority is never harder than holding it (T-86-84).
var explicitWriteGrantResources = map[string]bool{"autolaunch": true}

// Scopes models the api_keys.scopes JSONB column.
// JSON shapes accepted by ParseScopes:
//
//	{"full_access": true}
//	{"standard": true}
//	{"read_only": true}
//	{"instances":"write","ssh_keys":"read","billing":"none","webhooks":"none"}
//
// "standard" (Phase 39, D-02) is full capability minus admin/key-management
// surfaces. At the SCOPE layer it short-circuits true exactly like
// full_access, because the public surface has no key-management resource —
// the admin restriction is enforced by the request-time DB-role check
// ("scopes bound what the key may do; DB role bounds what the user may do").
//
// Unknown resources in the map are silently ignored (forward-compat for v2).
type Scopes struct {
	FullAccess bool                  `json:"full_access,omitempty"`
	Standard   bool                  `json:"standard,omitempty"`
	ReadOnly   bool                  `json:"read_only,omitempty"`
	Resources  map[string]ScopeLevel `json:"resources,omitempty"`
}

// shortcutScopeKeys is the shortcut vocabulary, in the order ParseScopes
// resolves them. Shared by ValidateMintScopes so the mint-time check and the
// read-time parse cannot disagree about what counts as a shortcut.
var shortcutScopeKeys = []string{"full_access", "standard", "read_only"}

// ParseScopes decodes the api_keys.scopes JSONB column into a Scopes value.
// Accepts both the shortcut shapes and the full per-resource shape.
//
// DELIBERATELY PERMISSIVE, AND IT MUST STAY THAT WAY. This is not a mint-time
// validator. It runs on STORED blobs on every authenticated request
// (auth_middleware.go, middleware_scope.go) and on both the live key and the
// FROZEN rule scopes in the auto-launch firer. Tightening it — for instance to
// reject a mixed {"full_access":true,"instances":"write"} shape — would fail
// authentication for every key already stored that way and would stop every
// armed rule whose frozen blob has that shape from firing.
//
// And mixed blobs are not hypothetical: injectServerlessScopes in
// internal/api/handlers_api_keys.go runs AFTER this function and adds a
// granular entry to the blob that gets STORED. Our own mint path produces mixed
// shapes. A tightening here would break authentication fleet-wide on deploy,
// not merely reject an unusual key.
//
// The mint-time refusal lives in ValidateMintScopes above, called only from
// handleCreateAPIKey. Keep them separate. If a future plan wants this function
// tightened, the prerequisite is an audit of api_keys.scopes AND
// autolaunch_rules.creator_api_key_scopes on demo AND prod showing zero mixed
// shapes — not a code review.
func ParseScopes(raw json.RawMessage) (Scopes, error) {
	if len(raw) == 0 {
		return Scopes{}, errors.New("empty scopes")
	}
	// Try shortcut shape first.
	var sc struct {
		FullAccess bool `json:"full_access"`
		Standard   bool `json:"standard"`
		ReadOnly   bool `json:"read_only"`
	}
	if err := json.Unmarshal(raw, &sc); err == nil && (sc.FullAccess || sc.ReadOnly || sc.Standard) {
		return Scopes{FullAccess: sc.FullAccess, Standard: sc.Standard, ReadOnly: sc.ReadOnly}, nil
	}
	// Per-resource shape.
	var perResource map[string]ScopeLevel
	if err := json.Unmarshal(raw, &perResource); err != nil {
		return Scopes{}, err
	}
	return Scopes{Resources: perResource}, nil
}

// Allows reports whether the holding key may perform the given action.
// resource is one of KnownResources; level is "read" or "write".
// (Pass ScopeRead or ScopeWrite — the function tolerates string args for
// ergonomic use at handler call sites.)
func (s Scopes) Allows(resource, level string) bool {
	// EXPLICIT-GRANT-ONLY carve-out, evaluated BEFORE every shortcut below.
	// Order is the whole mechanism: full_access returns true unconditionally,
	// so a carve-out placed after it would be dead code that reads like a
	// guarantee. For these resources at WRITE, the per-resource map is the only
	// thing consulted — no shortcut, no read_only expansion, nothing.
	if explicitWriteGrantResources[resource] && level == string(ScopeWrite) {
		return s.Resources[resource] == ScopeWrite
	}
	// Shortcut: full_access wins over everything (Pitfall 7 short-circuit
	// BEFORE consulting the per-resource map; new resources auto-allowed).
	if s.FullAccess {
		return true
	}
	// DECISION (Phase 39, D-02): standard grants full capability at the scope
	// layer — same short-circuit as full_access. The admin/key-management
	// restriction lives in the request-time DB-role check, not here, because
	// KnownResources has no key-management resource on the public surface.
	if s.Standard {
		return true
	}
	if s.ReadOnly {
		return level == string(ScopeRead)
	}
	got, ok := s.Resources[resource]
	if !ok {
		return false
	}
	switch ScopeLevel(level) {
	case ScopeRead:
		return got == ScopeRead || got == ScopeWrite
	case ScopeWrite:
		return got == ScopeWrite
	default:
		return false
	}
}

// ScopesStillCover reports whether `live` still allows everything `frozen`
// allowed, across every KnownResources entry at both read and write.
//
// The plan-19 firer calls this before replaying a rule's frozen launch spec.
// Consent was given by a specific key at a specific breadth; if that key has
// since been re-scoped NARROWER — the customer noticed an over-broad key and
// tightened it, or an admin pulled instances:write from a service account — the
// standing purchase order it signed must stop firing. Checking only that the
// key still exists and is unrevoked would miss that entirely: a key downgraded
// to read_only is still a live key.
//
// The comparison is deliberately "live covers frozen", not "live equals
// frozen". A key BROADENED since consent still covers what it authorized, and
// refusing to fire in that case would be a false alarm rather than a safety
// property — the rule's own ceiling, runtime and frozen spec are unchanged, so
// the extra breadth buys the rule nothing.
//
// It iterates KnownResources rather than frozen.Resources so a shortcut-shaped
// frozen blob ({"standard":true}) is expanded through Allows and compared
// resource by resource. A resource added to KnownResources after a rule was
// created is therefore evaluated on both sides by the same Allows, so a new
// family cannot make an old rule spuriously uncovered.
func ScopesStillCover(live, frozen Scopes) bool {
	for _, resource := range KnownResources {
		for _, level := range []string{string(ScopeRead), string(ScopeWrite)} {
			if frozen.Allows(resource, level) && !live.Allows(resource, level) {
				return false
			}
		}
	}
	return true
}

// ExplicitWriteGrantResources returns the carve-out list, sorted, for callers
// outside this package.
//
// It returns a fresh slice rather than the map because the map is the ONLY
// thing standing between a shortcut-scoped key and standing spend authority: a
// caller that could write to it could delete the carve-out at runtime, and
// nothing downstream would notice. Read-only by construction, not by
// convention.
//
// The one consumer today is the API-key mint gate in internal/api
// (handleCreateAPIKey). That gate exists because the two halves of this
// carve-out have to agree: the /v1 surface refuses `autolaunch:write` to every
// non-explicit key, and the dashboard surface refuses rule creation to
// non-admins — but neither helps if any member can mint themselves a key
// carrying the explicit grant. Whoever adds a resource here gets that gate for
// free, which is why the mint gate iterates this rather than naming autolaunch.
func ExplicitWriteGrantResources() []string {
	out := make([]string, 0, len(explicitWriteGrantResources))
	for resource := range explicitWriteGrantResources {
		out = append(out, resource)
	}
	sort.Strings(out)
	return out
}

// ValidateMintScopes is the MINT-TIME shape check. It is NOT part of ParseScopes
// and must never be folded into it — see the deliberate split documented on
// ParseScopes below.
//
// It refuses one thing: a shortcut key alongside per-resource entries, e.g.
// {"full_access":true,"autolaunch":"write"}. ParseScopes stores that as written
// and then reads it as the SHORTCUT ALONE, discarding the map. That is A WR-06:
// a customer who follows the 403's own advice ("mint a key with
// {"autolaunch":"write"}") by adding it to their existing full_access body gets
// a 201 and a key that still cannot create rules, with nothing anywhere saying
// why. Refusing the ambiguous body is the only outcome that is not a lie.
//
// The returned error names the ambiguity — the caller has to be told that the
// two halves cannot be combined, not merely that something was malformed.
func ValidateMintScopes(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("scopes is required")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		// Not an object at all. ParseScopes owns that refusal; saying it twice
		// in two different words would just give the customer two stories.
		return nil
	}
	var shortcuts []string
	for _, key := range shortcutScopeKeys {
		if _, ok := members[key]; ok {
			shortcuts = append(shortcuts, key)
		}
	}
	if len(shortcuts) == 0 || len(shortcuts) == len(members) {
		return nil
	}
	// Name the offending per-resource entries, but only ones from the known
	// vocabulary: the rest of the object's keys are unvalidated client input
	// and do not belong in a message we render back.
	var resources []string
	for _, resource := range KnownResources {
		if _, ok := members[resource]; ok {
			resources = append(resources, resource)
		}
	}
	named := "per-resource entries"
	if len(resources) > 0 {
		named = "the per-resource entries " + strings.Join(resources, ", ")
	}
	return fmt.Errorf(
		"scopes may not combine the shortcut key %s with %s: a shortcut already grants every "+
			"resource, so the per-resource entries would be silently discarded and the key would "+
			"not have the access you asked for. Send either the shortcut on its own or a "+
			"per-resource map on its own",
		strings.Join(shortcuts, ", "), named)
}
