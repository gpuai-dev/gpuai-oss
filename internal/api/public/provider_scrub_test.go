package public

import (
	"sort"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/privacy"
)

// canonicalProviderBrands is the set of compiled-in supplier brands that MUST
// be neutralised on customer-facing error text. It mirrors the registry
// adapter names in internal/provider/*/ minus the internal "generic" name.
// When you add an adapter, add its brand to BOTH this list and
// privacy.ProviderBrandTokens; TestProviderBrandTokensCoverCanonicalSet catches
// drift between the two. Operator-configured generic suppliers are covered at
// runtime by privacy.RegisterBrandTokens.
var canonicalProviderBrands = []string{"vultr"}

func TestScrubProviderNames(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		mustNot string // substring that must be absent after scrub
	}{
		{
			name:    "capacity failure with domain suffix",
			in:      "no matching offers for a100_40gb x8 on vultr.com: no GPU capacity available",
			want:    "no matching offers for a100_40gb x8 on the provider: no GPU capacity available",
			mustNot: "vultr",
		},
		{
			name:    "case-insensitive",
			in:      "Vultr timeout after 10s",
			want:    "the provider timeout after 10s",
			mustNot: "ultr",
		},
		{
			name: "no brand present is unchanged",
			in:   "disk size exceeds the maximum for this offering",
			want: "disk size exceeds the maximum for this offering",
		},
		{
			name: "empty stays empty",
			in:   "",
			want: "",
		},
		{
			name:    "generic infra words are NOT redacted (inference ScrubRe over-redacts these)",
			in:      "worker node datacenter disk full",
			want:    "worker node datacenter disk full",
			mustNot: "the provider",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scrubProviderNames(c.in)
			if got != c.want {
				t.Fatalf("scrubProviderNames(%q) = %q, want %q", c.in, got, c.want)
			}
			if c.mustNot != "" && strings.Contains(strings.ToLower(got), strings.ToLower(c.mustNot)) {
				t.Fatalf("scrubbed output %q still contains forbidden %q", got, c.mustNot)
			}
		})
	}
}

// TestEveryCanonicalBrandIsScrubbed proves each known provider brand is
// neutralised — the property that actually protects customers.
func TestEveryCanonicalBrandIsScrubbed(t *testing.T) {
	for _, brand := range canonicalProviderBrands {
		detail := "upstream error from " + brand + " while provisioning"
		got := scrubProviderNames(detail)
		if strings.Contains(strings.ToLower(got), brand) {
			t.Errorf("brand %q survives scrub: %q — add it to privacy.ProviderBrandTokens", brand, got)
		}
	}
}

// TestProviderBrandTokensCoverCanonicalSet pins privacy.ProviderBrandTokens to
// the canonical set so drift in either direction is caught at test time.
func TestProviderBrandTokensCoverCanonicalSet(t *testing.T) {
	got := append([]string(nil), privacy.ProviderBrandTokens...)
	want := append([]string(nil), canonicalProviderBrands...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("privacy.ProviderBrandTokens %v != canonical set %v — keep them in sync", got, want)
	}
}
