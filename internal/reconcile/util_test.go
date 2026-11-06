package reconcile

import "testing"

func TestParseOurInstanceID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Canonical "gpu-<8hex>" — the real format provision.generateInstanceID
		// mints and every adapter sets as the upstream Name. Returns the FULL
		// id (with prefix) so it matches instance.InstanceID. These are the real
		// IDs from the 2026-06-19 zombie incident.
		{"gpu-207db644", "gpu-207db644"},
		{"gpu-28318f2b", "gpu-28318f2b"},
		{"gpu-aab975c5", "gpu-aab975c5"},

		// All-hex suffix incl. a-f and digits, boundary values.
		{"gpu-deadbeef", "gpu-deadbeef"},
		{"gpu-00000000", "gpu-00000000"},

		// Uppercase normalizes to lowercase (a provider may upcase the Name).
		{"GPU-207DB644", "gpu-207db644"},

		// Surrounding whitespace tolerated.
		{"  gpu-207db644 \n", "gpu-207db644"},

		// Foreign / non-matching names — these MUST return "" so the reconciler
		// (in enforce mode) never terminates an instance launched outside
		// gpu-ai under a shared upstream API key (WR-04, tenant isolation).
		{"", ""},
		{"my-experiment", ""},
		{"team-pod-001", ""},
		{"gpu-1234567", ""},          // 7 hex — too short
		{"gpu-123456789", ""},        // 9 hex — too long
		{"gpu-deadbeeg", ""},         // 'g' is not hex
		{"207db644", ""},             // missing "gpu-" prefix
		{"gpu-207db644-extra", ""},   // trailing junk
		{"job-gpu-207db644", ""},     // embedded, not a clean prefix
		{"gpu-207db644 gpu-aab", ""}, // two tokens / wrong length

		// The old UUID forms are now REJECTED — we never minted UUIDs, and
		// accepting them let a foreign UUID-named VM parse as ours (WR-04).
		{"0529c75e-1234-5678-9abc-def012345678", ""},
		{"gpu-0529c75e-1234-5678-9abc-def012345678", ""},
	}

	for _, c := range cases {
		got := parseOurInstanceID(c.in)
		if got != c.want {
			t.Errorf("parseOurInstanceID(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestIsNotFoundErr(t *testing.T) {
	if isNotFoundErr(nil) {
		t.Error("nil error should not be not-found")
	}
	cases := []struct {
		err  error
		want bool
	}{
		{errString("instance not found"), true},
		{errString("HTTP 404 from upstream"), true},
		{errString("connection refused"), false},
		{errString(""), false},
	}
	for _, c := range cases {
		if got := isNotFoundErr(c.err); got != c.want {
			t.Errorf("isNotFoundErr(%q) = %v; want %v", c.err.Error(), got, c.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
