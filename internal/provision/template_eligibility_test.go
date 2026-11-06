// Plan 31-01 scaffold (TMPL-05 provider custom-image gate). Plan 06 landed
// SupportsCustomImage and removed the wave-gating build tag, so this runs as
// part of `go test ./...`.
//
// The template GPU-eligibility / auto-select path (TMPL-04) is exercised
// end-to-end against the real engine in engine_template_autoselect_test.go —
// SelectProviderCandidates filters the per-provider provider.GPUOffering set by
// the template's VRAM floor and composes with this custom-image gate. (The
// earlier filterEligibleOfferings helper, which filtered the provider-anonymized
// availability set, was never wireable into that loop and has been removed.)

package provision

import "testing"

// TestSupportsCustomImage pins the provider allow/deny table.
func TestSupportsCustomImage(t *testing.T) {
	cases := []struct {
		provider string
		want     bool
	}{
		{"podco", true},
		{"generic", true},
		// marketco honors DockerImage natively (adapter default-images otherwise) —
		// leaving it out of the allow-list made certified/framework launches on
		// marketco silently boot the stock pytorch 2.4.1 default (live-caught
		// 2026-07-16 on demo: certified:pytorch@2.13 → torch 2.4.1+cu124).
		{"marketco", true},
		{"vmimageco", false},
		{"fixedimageco", false},
	}
	for _, tc := range cases {
		if got := SupportsCustomImage(tc.provider); got != tc.want {
			t.Errorf("SupportsCustomImage(%q) = %v; want %v", tc.provider, got, tc.want)
		}
	}
}
