package provision

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// v33 — role-aware, attach-flagged SSH key resolution in Engine.Provision.
//
// These are fake-DB unit tests (no live Postgres) covering the resolution rules
// added for the "attach on provision" feature (Bug 1):
//
//   - Auto-include (empty SSHKeyIDs) is role-aware:
//       admin (or org-level system job, UserID == "") -> ALL org keys flagged
//         attach_on_provision
//       member -> only THEIR OWN keys flagged attach_on_provision
//   - attach_on_provision == false excludes a key from the auto-include set.
//   - "everything toggled off" -> ErrSSHKeysNotFound (fail rather than provision
//     an unreachable node).
//   - Explicit SSHKeyIDs: a member may only attach keys they own; passing a
//     teammate's key -> ErrSSHKeyForbidden. An admin may attach any org key.
//
// Observability: the engine embeds the resolved public keys into
// provider.ProvisionRequest.SSHPublicKeys, which the stub records — so each test
// asserts the EXACT set of public keys that reached the provider.

// roleKey builds a db.SSHKey owned by userID in org-1 with the given
// attach_on_provision flag. The public key is unique per name so a test can
// assert which keys resolved.
func roleKey(id, userID string, attach bool) db.SSHKey {
	return db.SSHKey{
		SSHKeyID:          id,
		OrgID:             "org-1",
		UserID:            userID,
		PublicKey:         "ssh-ed25519 AAAA-" + id,
		AttachOnProvision: attach,
	}
}

// fakeWithKeys returns a fakeEngineDB seeded with the given keys. The fake's
// GetSSHKeysByOrgID / GetSSHKeysByIDs return them verbatim; ListSSHKeysByOrgAndUser
// filters by UserID (mirroring the real query), so the role branch is exercised.
func fakeWithKeys(keys ...db.SSHKey) *fakeEngineDB {
	return &fakeEngineDB{sshKeys: keys}
}

// resolvedPubKeys returns the sorted set of public keys the engine embedded into
// the single provider.Provision call.
func resolvedPubKeys(t *testing.T, stub *stubProvider) []string {
	t.Helper()
	if len(stub.provisionReqs) != 1 {
		t.Fatalf("expected exactly 1 provider.Provision call, got %d", len(stub.provisionReqs))
	}
	got := append([]string(nil), stub.provisionReqs[0].SSHPublicKeys...)
	sort.Strings(got)
	return got
}

func wantKeys(ids ...string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, "ssh-ed25519 AAAA-"+id)
	}
	sort.Strings(out)
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestProvision_AdminAutoInclude_AllOrgAttachFlagged: an admin with no explicit
// SSHKeyIDs gets every org key flagged attach_on_provision — including keys owned
// by other members — but NOT keys toggled off.
func TestProvision_AdminAutoInclude_AllOrgAttachFlagged(t *testing.T) {
	stub := onDemandStub("up-admin", "us-west")
	fake := fakeWithKeys(
		roleKey("admin-own", "admin-1", true),
		roleKey("member-on", "member-2", true),
		roleKey("member-off", "member-2", false), // toggled off -> excluded
		roleKey("other-on", "member-3", true),
	)
	e := newProvisionUnitEngine(stub, fake)

	req := onDemandRequest()
	req.UserID = "admin-1"
	req.IsAdmin = true

	if _, err := e.Provision(context.Background(), req); err != nil {
		t.Fatalf("Provision returned unexpected error: %v", err)
	}

	got := resolvedPubKeys(t, stub)
	want := wantKeys("admin-own", "member-on", "other-on")
	if !eqStrings(got, want) {
		t.Errorf("admin auto-include resolved keys = %v, want %v (all org attach-flagged, member-off excluded)", got, want)
	}
}

// TestProvision_MemberAutoInclude_OwnAttachFlaggedOnly: a member with no explicit
// SSHKeyIDs gets ONLY their own attach-flagged keys — never a teammate's, and not
// their own toggled-off key.
func TestProvision_MemberAutoInclude_OwnAttachFlaggedOnly(t *testing.T) {
	stub := onDemandStub("up-member", "us-west")
	fake := fakeWithKeys(
		roleKey("member-on", "member-2", true),
		roleKey("member-off", "member-2", false), // own but toggled off -> excluded
		roleKey("other-on", "member-3", true),    // teammate's -> excluded
		roleKey("admin-own", "admin-1", true),    // admin's -> excluded
	)
	e := newProvisionUnitEngine(stub, fake)

	req := onDemandRequest()
	req.UserID = "member-2"
	req.IsAdmin = false

	if _, err := e.Provision(context.Background(), req); err != nil {
		t.Fatalf("Provision returned unexpected error: %v", err)
	}

	got := resolvedPubKeys(t, stub)
	want := wantKeys("member-on")
	if !eqStrings(got, want) {
		t.Errorf("member auto-include resolved keys = %v, want %v (own attach-flagged only)", got, want)
	}
}

// TestProvision_OrgLevelJob_AutoIncludesAllOrgKeys: an org-level system job
// (UserID == "", e.g. a managed fine-tune) resolves ALL org attach-flagged keys,
// like an admin. Guards the managed-FT path that intentionally leaves UserID empty.
func TestProvision_OrgLevelJob_AutoIncludesAllOrgKeys(t *testing.T) {
	stub := onDemandStub("up-job", "us-west")
	fake := fakeWithKeys(
		roleKey("member-on", "member-2", true),
		roleKey("other-on", "member-3", true),
		roleKey("member-off", "member-2", false),
	)
	e := newProvisionUnitEngine(stub, fake)

	req := onDemandRequest()
	req.UserID = "" // org-level job
	req.IsAdmin = false

	if _, err := e.Provision(context.Background(), req); err != nil {
		t.Fatalf("Provision returned unexpected error: %v", err)
	}

	got := resolvedPubKeys(t, stub)
	want := wantKeys("member-on", "other-on")
	if !eqStrings(got, want) {
		t.Errorf("org-level job resolved keys = %v, want %v (all org attach-flagged)", got, want)
	}
}

// TestProvision_AllToggledOff_FailsClosed: when every candidate key is toggled
// off, Provision returns ErrSSHKeysNotFound rather than silently provisioning an
// unreachable node — and never calls the provider.
func TestProvision_AllToggledOff_FailsClosed(t *testing.T) {
	stub := onDemandStub("up-off", "us-west")
	fake := fakeWithKeys(
		roleKey("admin-own", "admin-1", false),
		roleKey("member-off", "member-2", false),
	)
	e := newProvisionUnitEngine(stub, fake)

	req := onDemandRequest()
	req.UserID = "admin-1"
	req.IsAdmin = true

	_, err := e.Provision(context.Background(), req)
	if !errors.Is(err, ErrSSHKeysNotFound) {
		t.Fatalf("expected ErrSSHKeysNotFound when all keys toggled off, got: %v", err)
	}
	if len(stub.provisionReqs) != 0 {
		t.Errorf("provider.Provision must not be called when no key resolves, got %d calls", len(stub.provisionReqs))
	}
}

// TestProvision_MemberExplicitTeammateKey_Forbidden: a member who passes an
// explicit ssh_key_id for a key owned by a teammate is rejected with
// ErrSSHKeyForbidden (maps to HTTP 403) — the explicit path can't bypass the
// member-own scoping. No provider call.
func TestProvision_MemberExplicitTeammateKey_Forbidden(t *testing.T) {
	stub := onDemandStub("up-forbidden", "us-west")
	// The fake's GetSSHKeysByIDs returns its seeded keys verbatim; seed it with
	// the teammate's key to model "member-2 passed other-on's id".
	fake := fakeWithKeys(roleKey("other-on", "member-3", true))
	e := newProvisionUnitEngine(stub, fake)

	req := onDemandRequest()
	req.UserID = "member-2"
	req.IsAdmin = false
	req.SSHKeyIDs = []string{"other-on"}

	_, err := e.Provision(context.Background(), req)
	if !errors.Is(err, ErrSSHKeyForbidden) {
		t.Fatalf("expected ErrSSHKeyForbidden for member passing a teammate's key, got: %v", err)
	}
	if len(stub.provisionReqs) != 0 {
		t.Errorf("provider.Provision must not be called on a forbidden key, got %d calls", len(stub.provisionReqs))
	}
}

// TestProvision_MemberExplicitOwnKey_Allowed: a member passing their OWN
// ssh_key_id is allowed, even when attach_on_provision is false — explicit intent
// overrides the auto-include toggle (the toggle only governs auto-include).
func TestProvision_MemberExplicitOwnKey_Allowed(t *testing.T) {
	stub := onDemandStub("up-own", "us-west")
	fake := fakeWithKeys(roleKey("member-off", "member-2", false))
	e := newProvisionUnitEngine(stub, fake)

	req := onDemandRequest()
	req.UserID = "member-2"
	req.IsAdmin = false
	req.SSHKeyIDs = []string{"member-off"}

	if _, err := e.Provision(context.Background(), req); err != nil {
		t.Fatalf("member attaching their own key should succeed, got: %v", err)
	}
	got := resolvedPubKeys(t, stub)
	want := wantKeys("member-off")
	if !eqStrings(got, want) {
		t.Errorf("explicit own-key resolved = %v, want %v", got, want)
	}
}

// TestProvision_AdminExplicitTeammateKey_Allowed: an admin may attach any org
// key explicitly, including a key owned by another member.
func TestProvision_AdminExplicitTeammateKey_Allowed(t *testing.T) {
	stub := onDemandStub("up-admin-explicit", "us-west")
	fake := fakeWithKeys(roleKey("other-on", "member-3", true))
	e := newProvisionUnitEngine(stub, fake)

	req := onDemandRequest()
	req.UserID = "admin-1"
	req.IsAdmin = true
	req.SSHKeyIDs = []string{"other-on"}

	if _, err := e.Provision(context.Background(), req); err != nil {
		t.Fatalf("admin attaching a teammate's key should succeed, got: %v", err)
	}
	got := resolvedPubKeys(t, stub)
	want := wantKeys("other-on")
	if !eqStrings(got, want) {
		t.Errorf("admin explicit teammate-key resolved = %v, want %v", got, want)
	}
}
