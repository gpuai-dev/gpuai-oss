package privacy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// internalOnlyProviderDirs are adapter package directories whose names are
// registry-internal plumbing rather than an upstream brand. "generic" is the
// supplier multiplexer; its operator-configured supplier names are covered at
// runtime by RegisterBrandTokens.
var internalOnlyProviderDirs = map[string]bool{
	"generic": true,
}

func providerDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "provider")
}

// TestProviderBrandTokensCoverEveryAdapterDir walks internal/provider/*/ for
// adapter packages and asserts each one is covered by ProviderBrandTokens, so
// adding an adapter without a token is a red test, not a silent leak.
func TestProviderBrandTokensCoverEveryAdapterDir(t *testing.T) {
	dir := providerDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	covered := map[string]bool{}
	for _, tok := range ProviderBrandTokens {
		covered[strings.ToLower(tok)] = true
	}
	adapterDirs := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, statErr := os.Stat(filepath.Join(dir, name, "adapter.go")); statErr != nil {
			continue
		}
		adapterDirs++
		if internalOnlyProviderDirs[name] {
			continue
		}
		if !covered[strings.ToLower(name)] {
			t.Errorf("provider adapter %q has no entry in ProviderBrandTokens", name)
			continue
		}
		got := ScrubProviderNames("upstream error from " + name + " while provisioning")
		if strings.Contains(strings.ToLower(got), strings.ToLower(name)) {
			t.Errorf("provider adapter %q is listed but survives ScrubProviderNames: %q", name, got)
		}
	}
	if adapterDirs == 0 {
		t.Fatalf("found no provider adapter directories under %s", dir)
	}
}

func TestScrubProviderNamesEveryToken(t *testing.T) {
	for _, brand := range ProviderBrandTokens {
		t.Run(brand, func(t *testing.T) {
			variants := []string{
				"provision: all providers failed (last: " + brand + ": no GPU capacity available)",
				"upstream " + brand + ".com returned 503",
				"error from " + strings.ToUpper(brand) + " while provisioning",
			}
			for _, in := range variants {
				got := ScrubProviderNames(in)
				if strings.Contains(strings.ToLower(got), brand) {
					t.Errorf("brand %q survives scrub of %q: %q", brand, in, got)
				}
				if !strings.Contains(got, "the provider") {
					t.Errorf("scrub of %q produced no placeholder: %q", in, got)
				}
			}
			once := ScrubProviderNames("upstream " + brand + ".com returned 503")
			if twice := ScrubProviderNames(once); twice != once {
				t.Errorf("ScrubProviderNames is not idempotent: %q -> %q", once, twice)
			}
		})
	}
}

func TestScrubProviderNamesTable(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		mustNot string
	}{
		{
			name:    "bare brand token",
			in:      "vultr returned 500",
			want:    "the provider returned 500",
			mustNot: "vultr",
		},
		{
			name:    "case-insensitive with domain suffix",
			in:      "timeout calling Vultr.com after 10s",
			want:    "timeout calling the provider after 10s",
			mustNot: "ultr",
		},
		{
			name: "no brand present is unchanged",
			in:   "disk size exceeds the maximum for this offering",
			want: "disk size exceeds the maximum for this offering",
		},
		{name: "empty stays empty", in: "", want: ""},
		{
			name:    "internal registry name generic is deliberately not redacted",
			in:      "generic supplier adapter rejected the request",
			want:    "generic supplier adapter rejected the request",
			mustNot: "the provider",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ScrubProviderNames(c.in)
			if got != c.want {
				t.Fatalf("ScrubProviderNames(%q) = %q, want %q", c.in, got, c.want)
			}
			if c.mustNot != "" && strings.Contains(strings.ToLower(got), strings.ToLower(c.mustNot)) {
				t.Fatalf("scrubbed output %q still contains forbidden %q", got, c.mustNot)
			}
		})
	}
}

// TestRegisterBrandTokens covers operator-configured generic suppliers: their
// slugs and names are only known at startup.
func TestRegisterBrandTokens(t *testing.T) {
	t.Cleanup(resetRuntimeTokens)
	in := "provision failed on acme-gpu: HTTP 409 from Acme GPU Cloud"
	if got := ScrubProviderNames(in); got != in {
		t.Fatalf("unregistered names must pass through unchanged, got %q", got)
	}
	RegisterBrandTokens("acme-gpu", "Acme GPU Cloud", "", "ab", "generic")
	got := ScrubProviderNames(in)
	if strings.Contains(strings.ToLower(got), "acme") {
		t.Fatalf("registered supplier survives scrub: %q", got)
	}
	if want := "provision failed on the provider: HTTP 409 from the provider"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Short and internal tokens were ignored.
	if got := ScrubProviderNames("generic ab"); got != "generic ab" {
		t.Fatalf("short/internal tokens must be ignored, got %q", got)
	}
}
