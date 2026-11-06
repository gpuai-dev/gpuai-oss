package public

import (
	"encoding/json"
	"strings"
	"testing"
)

// skipExplicitWrite reports whether (resource, level) is exempt from the
// "shortcut covers everything" expectation because the resource's WRITE level
// is explicit-grant-only (scopes.go's explicitWriteGrantResources).
//
// Every shortcut-coverage test below consults this rather than hard-coding
// "autolaunch", so adding a second carve-out resource does not require editing
// four tests — and, more importantly, so the exemption in each test is visibly
// the SAME exemption rather than four independent opinions. The positive half
// of the contract (that the carve-out actually denies) lives in
// TestExplicitGrantOnlyResources, which is what stops these skips from making
// the suite pass vacuously.
func skipExplicitWrite(resource, level string) bool {
	return explicitWriteGrantResources[resource] && level == string(ScopeWrite)
}

func TestScopes_FullAccess(t *testing.T) {
	s := Scopes{FullAccess: true}
	for _, r := range KnownResources {
		for _, lvl := range []string{"read", "write"} {
			if skipExplicitWrite(r, lvl) {
				continue
			}
			if !s.Allows(r, lvl) {
				t.Errorf("FullAccess.Allows(%q, %q) = false, want true", r, lvl)
			}
		}
	}
}

func TestScopes_ReadOnly(t *testing.T) {
	s := Scopes{ReadOnly: true}
	for _, r := range KnownResources {
		if !s.Allows(r, "read") {
			t.Errorf("ReadOnly.Allows(%q, read) = false", r)
		}
		if s.Allows(r, "write") {
			t.Errorf("ReadOnly.Allows(%q, write) = true (should be false)", r)
		}
	}
}

func TestScopes_Standard(t *testing.T) {
	s := Scopes{Standard: true}
	for _, r := range KnownResources {
		for _, lvl := range []string{"read", "write"} {
			if skipExplicitWrite(r, lvl) {
				continue
			}
			if !s.Allows(r, lvl) {
				t.Errorf("Standard.Allows(%q, %q) = false, want true (standard is full capability at the scope layer)", r, lvl)
			}
		}
	}
}

func TestScopes_PerResource(t *testing.T) {
	s := Scopes{Resources: map[string]ScopeLevel{
		"instances": ScopeWrite,
		"ssh_keys":  ScopeRead,
		"billing":   ScopeNone,
	}}
	if !s.Allows("instances", "write") {
		t.Error("instances:write should allow write")
	}
	if !s.Allows("instances", "read") {
		t.Error("write level should also allow read (write subsumes read)")
	}
	if s.Allows("ssh_keys", "write") {
		t.Error("ssh_keys:read should NOT allow write")
	}
	if !s.Allows("ssh_keys", "read") {
		t.Error("ssh_keys:read should allow read")
	}
	if s.Allows("billing", "read") {
		t.Error("billing:none should NOT allow read")
	}
	if s.Allows("webhooks", "read") {
		t.Error("missing resource entry should default to deny")
	}
}

func TestParseScopes(t *testing.T) {
	// Full-access shortcut.
	s1, err := ParseScopes(json.RawMessage(`{"full_access":true}`))
	if err != nil || !s1.FullAccess {
		t.Errorf("parse full_access: %+v err=%v", s1, err)
	}
	// Read-only shortcut.
	s2, err := ParseScopes(json.RawMessage(`{"read_only":true}`))
	if err != nil || !s2.ReadOnly {
		t.Errorf("parse read_only: %+v err=%v", s2, err)
	}
	// Standard shortcut.
	s4, err := ParseScopes(json.RawMessage(`{"standard":true}`))
	if err != nil || !s4.Standard {
		t.Errorf("parse standard: %+v err=%v", s4, err)
	}
	// Per-resource.
	s3, err := ParseScopes(json.RawMessage(`{"instances":"write","ssh_keys":"read"}`))
	if err != nil {
		t.Fatalf("parse per-resource: %v", err)
	}
	if s3.Resources["instances"] != ScopeWrite {
		t.Errorf("expected instances=write, got %q", s3.Resources["instances"])
	}
}

// TestFullAccessCoversAllResources is the Pitfall 7 guard.
// Ensures the FullAccess short-circuit covers every KnownResources entry
// for both read and write — protecting against drift when a future plan
// adds a new resource family.
//
// Phase 86 Plan 11 (SEC-03) narrowed it: entries in explicitWriteGrantResources
// are exempt at WRITE, because for those the short-circuit is DELIBERATELY not
// reached. Skipping them here is not a weakening of this guard — the property
// they need is the opposite one, and it is asserted positively in
// TestExplicitGrantOnlyResources.
func TestFullAccessCoversAllResources(t *testing.T) {
	s := Scopes{FullAccess: true}
	for _, r := range KnownResources {
		for _, lvl := range []string{string(ScopeRead), string(ScopeWrite)} {
			if skipExplicitWrite(r, lvl) {
				continue
			}
			if !s.Allows(r, lvl) {
				t.Errorf("FullAccess fails on resource %q level %q — drift detected, fix Allows() short-circuit", r, lvl)
			}
		}
	}
}

// TestStandardCoversAllResources is the Pitfall 7 guard for the standard
// shortcut (Phase 39, D-02). At the SCOPE layer standard grants full
// capability on every KnownResources entry — the admin/key-management
// restriction is the request-time DB-role check, not the scope, because the
// public surface has no key-management resource. Protects against drift when
// a future plan adds a new resource family.
// (Phase 86 Plan 11: explicit-grant-only resources are exempt at WRITE — see
// TestFullAccessCoversAllResources' note and TestExplicitGrantOnlyResources.)
func TestStandardCoversAllResources(t *testing.T) {
	s := Scopes{Standard: true}
	for _, r := range KnownResources {
		for _, lvl := range []string{string(ScopeRead), string(ScopeWrite)} {
			if skipExplicitWrite(r, lvl) {
				continue
			}
			if !s.Allows(r, lvl) {
				t.Errorf("Standard fails on resource %q level %q — drift detected, fix Allows() short-circuit", r, lvl)
			}
		}
	}
}

// TestExplicitGrantOnlyResources is the POSITIVE half of the SEC-03 carve-out,
// and the reason the three skips above are safe.
//
// Without it, a future "cleanup" that deleted explicitWriteGrantResources and
// its branch in Allows would make the whole suite green again — the skips would
// simply stop firing — while silently handing every key ever issued standing,
// unattended spend authority. This test fails loudly in that world, because it
// asserts the DENIALS directly rather than deriving them from the map.
func TestExplicitGrantOnlyResources(t *testing.T) {
	if len(explicitWriteGrantResources) == 0 {
		t.Fatal("explicitWriteGrantResources is empty — if the carve-out was removed, that is a SEC-03 decision to make deliberately, not by deleting a map")
	}
	if !explicitWriteGrantResources["autolaunch"] {
		t.Fatal("autolaunch left explicitWriteGrantResources: autolaunch:write authorizes UNATTENDED spending days after consent, and must never be covered by full_access/standard/read_only (Phase 86 Plan 11, T-86-44)")
	}

	// No shortcut reaches autolaunch:write. These are the three shapes every
	// key in the database has.
	for name, s := range map[string]Scopes{
		"full_access": {FullAccess: true},
		"standard":    {Standard: true},
		"read_only":   {ReadOnly: true},
	} {
		if s.Allows("autolaunch", string(ScopeWrite)) {
			t.Errorf("%s grants autolaunch:write — every key issued before Phase 86 would have acquired standing spend authority on deploy", name)
		}
	}

	// An EXPLICIT per-resource grant is the only thing that works.
	granular := Scopes{Resources: map[string]ScopeLevel{"autolaunch": ScopeWrite}}
	if !granular.Allows("autolaunch", string(ScopeWrite)) {
		t.Error(`{"autolaunch":"write"} must grant autolaunch:write — otherwise the resource is unreachable and the feature cannot ship`)
	}
	// ...and a granular READ grant must NOT be silently promoted.
	readOnlyGranular := Scopes{Resources: map[string]ScopeLevel{"autolaunch": ScopeRead}}
	if readOnlyGranular.Allows("autolaunch", string(ScopeWrite)) {
		t.Error(`{"autolaunch":"read"} must not grant autolaunch:write`)
	}

	// READ is deliberately NOT carved out: listing and inspecting rules is not
	// spend, and cancelling must never be harder than holding (T-86-84).
	for name, s := range map[string]Scopes{
		"full_access": {FullAccess: true},
		"standard":    {Standard: true},
		"read_only":   {ReadOnly: true},
	} {
		if !s.Allows("autolaunch", string(ScopeRead)) {
			t.Errorf("%s must still grant autolaunch:read — DELETE and the disarm PATCH are gated on read so a narrowed key can still cancel the rules it created", name)
		}
	}
}

// TestScopesStillCover pins the fire-time comparison plan 19's firer runs
// before replaying a frozen spec: the consenting key must still allow
// everything it allowed at consent.
func TestScopesStillCover(t *testing.T) {
	cases := []struct {
		name  string
		live  Scopes
		froze Scopes
		want  bool
	}{
		{
			name:  "identical granular",
			live:  Scopes{Resources: map[string]ScopeLevel{"instances": ScopeWrite, "autolaunch": ScopeWrite}},
			froze: Scopes{Resources: map[string]ScopeLevel{"instances": ScopeWrite, "autolaunch": ScopeWrite}},
			want:  true,
		},
		{
			name:  "live broader still covers",
			live:  Scopes{Resources: map[string]ScopeLevel{"instances": ScopeWrite, "ssh_keys": ScopeWrite, "autolaunch": ScopeWrite}},
			froze: Scopes{Resources: map[string]ScopeLevel{"instances": ScopeWrite}},
			want:  true,
		},
		{
			name:  "live dropped instances:write",
			live:  Scopes{Resources: map[string]ScopeLevel{"instances": ScopeRead, "autolaunch": ScopeWrite}},
			froze: Scopes{Resources: map[string]ScopeLevel{"instances": ScopeWrite, "autolaunch": ScopeWrite}},
			want:  false,
		},
		{
			name:  "live dropped autolaunch:write",
			live:  Scopes{Resources: map[string]ScopeLevel{"instances": ScopeWrite}},
			froze: Scopes{Resources: map[string]ScopeLevel{"instances": ScopeWrite, "autolaunch": ScopeWrite}},
			want:  false,
		},
		{
			name:  "narrowed to read_only",
			live:  Scopes{ReadOnly: true},
			froze: Scopes{Resources: map[string]ScopeLevel{"instances": ScopeWrite}},
			want:  false,
		},
		{
			// A standard key covers everything EXCEPT the carved-out write, so
			// it cannot cover a frozen blob that held autolaunch:write.
			name:  "standard does not cover a frozen autolaunch:write",
			live:  Scopes{Standard: true},
			froze: Scopes{Resources: map[string]ScopeLevel{"autolaunch": ScopeWrite}},
			want:  false,
		},
		{
			name:  "full_access covers an ordinary frozen blob",
			live:  Scopes{FullAccess: true},
			froze: Scopes{Resources: map[string]ScopeLevel{"instances": ScopeWrite, "billing": ScopeRead}},
			want:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScopesStillCover(tc.live, tc.froze); got != tc.want {
				t.Errorf("ScopesStillCover = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestKnownResourcesPinned pins the exact KnownResources set. Both the
// full_access AND standard shortcuts auto-grant write on every entry in this
// list (see Allows), so adding a resource silently extends every
// member-minted standard key issued to date. That is fine for ordinary
// resource families, but would defeat SEC-03 if the new resource is
// privileged (key management, org administration, billing mutation beyond
// today's surface). This test forces that decision to be explicit: when you
// add a resource, either confirm the standard auto-grant is safe for it, or
// stop standard short-circuiting in Allows and enumerate its grants — then
// update this pin.
//
// "watch" (Phase 86 Plan 05, MKT-05): auto-grant CONFIRMED SAFE. Watch rules
// are zero-spend notification preferences; no field on the resource reaches the
// provisioning path, and auto-launch — the resource that CAN spend — is a
// structurally separate one behind its own consent gate (D-18). So a
// standard-scoped key gaining watch:write gains no new ability to spend money.
//
// "market" (Phase 86 Plan 07, MKT-03): auto-grant CONFIRMED SAFE. The family is
// READ-ONLY — its only route is GET /v1/market-history. A market:write grant
// therefore reaches no handler at all: there is nothing to mutate and nothing
// to spend, so a standard key gaining it gains the ability to read a chart.
// If a future plan adds a WRITE route under this family, that safety argument
// stops holding and this pin has to be re-decided rather than re-typed.
//
// "autolaunch" (Phase 86 Plan 11, MKT-09): auto-grant REFUSED — the first
// entry in this list for which the answer is no, and the case the "or stop
// standard short-circuiting in Allows and enumerate its grants" clause above
// was written for. `autolaunch:read` is ordinary. `autolaunch:write` is
// EXPLICIT-GRANT-ONLY via explicitWriteGrantResources, checked in Allows ahead
// of every shortcut.
//
// The reasoning, recorded here because an earlier draft argued the opposite and
// was rejected on review — do not restore it. `instances:write` is REAL-TIME
// authority: money moves when the key holder makes a request, with someone
// present, and abuse appears in billing at that moment. `autolaunch:write` is a
// STANDING, FROZEN, UNATTENDED purchase order that fires days or weeks later
// with nobody watching. Different authority CLASS, not the same authority on a
// delay. A blanket short-circuit would grant it to every key already issued —
// including keys minted long before this phase existed, whose holders were
// never told — the moment this deploys. The per-rule ceiling, max runtime, org
// burn cap and spending limit bound the SIZE of an unattended purchase; none of
// them is consent to unattended purchasing at all.
//
// Consequences to keep in mind when adding the NEXT resource: the three
// shortcut-coverage tests above skip explicit-grant-only entries at write, so
// the coverage they assert is narrower than it looks. The teeth are in
// TestExplicitGrantOnlyResources.
func TestKnownResourcesPinned(t *testing.T) {
	want := []string{"instances", "ssh_keys", "billing", "webhooks", "serverless", "community", "watch", "market", "autolaunch"}
	if len(KnownResources) != len(want) {
		t.Fatalf("KnownResources changed (got %v, pinned %v) — decide whether the standard-scope auto-grant is safe for the new resource (SEC-03), then update this pin", KnownResources, want)
	}
	for i, r := range want {
		if KnownResources[i] != r {
			t.Fatalf("KnownResources[%d] = %q, pinned %q — decide whether the standard-scope auto-grant is safe for the change (SEC-03), then update this pin", i, KnownResources[i], r)
		}
	}
}

// ---------------------------------------------------------------------------
// The mint boundary (Phase 86 Plan 23, A CR-03 / A WR-06)
// ---------------------------------------------------------------------------

// TestExplicitWriteGrantResourcesIsReadOnlyToCallers: the accessor exists so
// the API-key mint gate can iterate the carve-out without being able to edit
// it. A caller that could write to the map could delete the carve-out at
// runtime and nothing downstream would notice.
func TestExplicitWriteGrantResourcesIsReadOnlyToCallers(t *testing.T) {
	got := ExplicitWriteGrantResources()
	if len(got) != len(explicitWriteGrantResources) {
		t.Fatalf("accessor returned %v, map holds %v", got, explicitWriteGrantResources)
	}
	for _, resource := range got {
		if !explicitWriteGrantResources[resource] {
			t.Errorf("accessor returned %q, which is not in the carve-out", resource)
		}
	}
	// Mutating the returned slice must not reach the map.
	got[0] = "instances"
	if explicitWriteGrantResources["instances"] {
		t.Fatal("writing to the returned slice reached the carve-out map")
	}
	if again := ExplicitWriteGrantResources(); again[0] == "instances" {
		t.Fatal("the accessor handed out an aliased slice — a caller can edit the carve-out")
	}
}

// TestValidateMintScopes_RefusesTheMixedShape is A WR-06. A shortcut key plus
// per-resource entries is stored as written and parsed as the shortcut ALONE,
// so a customer who adds {"autolaunch":"write"} to their existing full_access
// body gets a 201 and a key that still cannot create rules.
func TestValidateMintScopes_RefusesTheMixedShape(t *testing.T) {
	for _, raw := range []string{
		`{"full_access":true,"autolaunch":"write"}`,
		`{"standard":true,"instances":"write"}`,
		`{"read_only":true,"serverless":"write"}`,
		`{"full_access":true,"unknown_family":"write"}`,
	} {
		err := ValidateMintScopes(json.RawMessage(raw))
		if err == nil {
			t.Fatalf("%s was accepted at mint time — the per-resource half would be silently discarded", raw)
		}
		// The detail must name the AMBIGUITY, not merely report malformity.
		msg := err.Error()
		for _, want := range []string{"shortcut", "discarded"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: the refusal does not name the ambiguity (missing %q): %s", raw, want, msg)
			}
		}
	}
}

// TestValidateMintScopes_AcceptsTheTwoHonestShapes: everything customers mint
// today keeps minting. This is the blast radius check.
func TestValidateMintScopes_AcceptsTheTwoHonestShapes(t *testing.T) {
	for _, raw := range []string{
		`{"full_access":true}`,
		`{"standard":true}`,
		`{"read_only":true}`,
		`{"autolaunch":"write","instances":"write"}`,
		`{"instances":"write","ssh_keys":"read","billing":"none","webhooks":"none"}`,
		`{"autolaunch":"read"}`,
		// Not an object: ParseScopes owns that refusal, so this one stays
		// silent rather than giving the customer two different stories.
		`"not-an-object"`,
	} {
		if err := ValidateMintScopes(json.RawMessage(raw)); err != nil {
			t.Errorf("%s was refused at mint time: %v", raw, err)
		}
	}
}

// TestParseScopesStillAcceptsAStoredMixedBlob is the fleet-safety property, and
// it is the reason the mixed-shape refusal is NOT in ParseScopes.
//
// ParseScopes runs on STORED blobs on every authenticated request and on the
// auto-launch firer's frozen scopes. injectServerlessScopes (internal/api)
// appends a granular entry AFTER ParseScopes on the mint path, so mixed blobs
// are produced by our own code and are already in the fleet. If this test ever
// starts failing because someone "simplified" the two functions into one, the
// deploy that ships it fails authentication for those keys and stops their
// armed rules from firing.
func TestParseScopesStillAcceptsAStoredMixedBlob(t *testing.T) {
	raw := json.RawMessage(`{"full_access":true,"serverless":"write","autolaunch":"write"}`)

	// The stored blob still authenticates, and still resolves to the shortcut.
	sc, err := ParseScopes(raw)
	if err != nil {
		t.Fatalf("a stored mixed blob must keep parsing — every full_access key minted through injectServerlessScopes is one: %v", err)
	}
	if !sc.FullAccess {
		t.Errorf("a stored mixed blob must still resolve to the shortcut: %+v", sc)
	}
	// And the per-resource half is still discarded, which is exactly why the
	// MINT boundary refuses the shape rather than the read boundary.
	if sc.Allows("autolaunch", string(ScopeWrite)) {
		t.Error("the discarded per-resource half must not grant autolaunch:write — the carve-out is the whole point")
	}
	// The mint boundary, on the same bytes, refuses.
	if ValidateMintScopes(raw) == nil {
		t.Error("the mint boundary accepted the shape the read boundary tolerates")
	}
}
