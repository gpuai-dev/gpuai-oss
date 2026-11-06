package instanceid

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in     string
		wantID string
		wantOK bool
	}{
		// Canonical "gpu-<8hex>" — the real format provision.generateInstanceID
		// mints and every adapter sets as the upstream Name. Returns the FULL id
		// (with prefix). These are the real IDs from the 2026-06-19 zombie
		// incident.
		{"gpu-207db644", "gpu-207db644", true},
		{"gpu-28318f2b", "gpu-28318f2b", true},
		{"gpu-aab975c5", "gpu-aab975c5", true},

		// Hex boundary values.
		{"gpu-deadbeef", "gpu-deadbeef", true},
		{"gpu-00000000", "gpu-00000000", true},
		{"gpu-ffffffff", "gpu-ffffffff", true},

		// Uppercase normalizes to lowercase (a provider may upcase the Name).
		{"GPU-207DB644", "gpu-207db644", true},

		// Surrounding whitespace tolerated.
		{"  gpu-207db644 \n", "gpu-207db644", true},

		// Foreign / malformed names — MUST NOT parse as ours (WR-04).
		{"", "", false},
		{"my-experiment", "", false},
		{"team-pod-001", "", false},
		{"ops-manual-cluster", "", false},
		{"gpu-1234567", "", false},          // 7 hex — too short
		{"gpu-123456789", "", false},        // 9 hex — too long
		{"gpu-deadbeeg", "", false},         // 'g' is not hex
		{"gpu-207db64", "", false},          // 11 chars total
		{"207db644", "", false},             // missing "gpu-" prefix
		{"gpu-207db644-extra", "", false},   // trailing junk
		{"job-gpu-207db644", "", false},     // embedded, not a clean prefix
		{"gpu-207db644 gpu-aab", "", false}, // wrong length / two tokens

		// The old UUID forms — never minted; accepting them let a foreign
		// UUID-named VM parse as ours. Now rejected.
		{"0529c75e-1234-5678-9abc-def012345678", "", false},
		{"gpu-0529c75e-1234-5678-9abc-def012345678", "", false},
	}

	for _, c := range cases {
		id, ok := Parse(c.in)
		if id != c.wantID || ok != c.wantOK {
			t.Errorf("Parse(%q) = (%q, %v); want (%q, %v)", c.in, id, ok, c.wantID, c.wantOK)
		}
		if got := IsOurs(c.in); got != c.wantOK {
			t.Errorf("IsOurs(%q) = %v; want %v", c.in, got, c.wantOK)
		}
	}
}

func TestTagFor(t *testing.T) {
	cases := map[string]string{
		"production": "prod", "PRODUCTION": "prod", "prod": "prod",
		"demo": "demo", " Demo ": "demo",
		"development": "", "": "", "staging": "",
	}
	for in, want := range cases {
		if got := tagFor(in); got != want {
			t.Errorf("tagFor(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestFormatUpstreamNameAndOwnedHere(t *testing.T) {
	defer SetLocalEnv("") // restore the package default for other tests

	const id = "gpu-207db644"

	// Development (untagged): names are bare, only bare names are owned, and a
	// foreign env's tagged name is NOT ours.
	SetLocalEnv("development")
	if got := FormatUpstreamName(id); got != id {
		t.Errorf("dev FormatUpstreamName = %q; want %q", got, id)
	}
	if gotID, ok := OwnedHere(id); !ok || gotID != id {
		t.Errorf("dev OwnedHere(%q) = (%q,%v); want (%q,true)", id, gotID, ok, id)
	}
	if _, ok := OwnedHere("prod-" + id); ok {
		t.Errorf("dev OwnedHere(prod-%s) = true; want false", id)
	}

	// Production: names are "prod-"-tagged; we own ONLY our own tag — never demo's
	// and never a bare/legacy name (so we can't reap another env's pods).
	SetLocalEnv("production")
	want := "prod-" + id
	if got := FormatUpstreamName(id); got != want {
		t.Errorf("prod FormatUpstreamName = %q; want %q", got, want)
	}
	if gotID, ok := OwnedHere(want); !ok || gotID != id {
		t.Errorf("prod OwnedHere(%q) = (%q,%v); want (%q,true)", want, gotID, ok, id)
	}
	if !IsOurs(want) {
		t.Errorf("prod IsOurs(%q) = false; want true", want)
	}
	for _, foreign := range []string{"demo-" + id, id, "prod-gpu-zzzzzzzz", "prod-" + id + "-boot"} {
		if _, ok := OwnedHere(foreign); ok {
			t.Errorf("prod OwnedHere(%q) = true; want false (not ours)", foreign)
		}
	}
}
