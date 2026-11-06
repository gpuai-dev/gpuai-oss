package provider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// Every declared GPUType must resolve to itself. This is the property the watch
// gate depends on: a model the catalog can emit must never be refused.
func TestCanonicalizeGPUTypeAcceptsEveryDeclaredType(t *testing.T) {
	for _, gt := range gpuTypeConstValues(t) {
		got, ok := CanonicalizeGPUType(string(gt))
		if !ok || got != gt {
			t.Errorf("CanonicalizeGPUType(%q) = (%q, %v); want (%q, true) — add it to vramByGPUType in vram.go", gt, got, ok, gt)
		}
	}
}

// Two GPU types must not collapse to the same comparison form, or one of them
// becomes unreachable: `rtx_6000_ada` and `rtx6000ada` would be the same key,
// and whichever lost the map write would silently be un-watchable.
func TestGPUTypeComparisonFormsAreUnique(t *testing.T) {
	seen := map[string]GPUType{}
	for _, gt := range gpuTypeConstValues(t) {
		key := normalizeGPUTypeKey(string(gt))
		if prev, dup := seen[key]; dup {
			t.Errorf("GPU types %q and %q share the comparison form %q — one of them cannot be resolved", prev, gt, key)
		}
		seen[key] = gt
	}
}

func TestCanonicalizeGPUType(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   GPUType
		wantOK bool
	}{
		{"exact", "h100_sxm", GPUTypeH100SXM, true},
		{"uppercase", "H100_SXM", GPUTypeH100SXM, true},
		// The spelling phase 86's own docs shipped. Refusing it would make the
		// documentation wrong rather than the input.
		{"hyphenated", "rtx-a4500", GPUTypeRTXA4500, true},
		{"spaced", "RTX A4500", GPUTypeRTXA4500, true},
		{"surrounding space", "  l40s  ", GPUTypeL40S, true},
		{"digits and units", "A100-80GB", GPUTypeA10080GB, true},

		// A GENERATION is not a model. Three H100 SKUs exist at three prices;
		// "H100" names none of them, and a rule keyed on it can never match.
		{"generation only", "H100", "", false},
		{"made up", "totally_not_a_gpu", "", false},
		{"empty", "", "", false},
		{"whitespace", "   ", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CanonicalizeGPUType(tc.in)
			if ok != tc.wantOK || (tc.wantOK && got != tc.want) {
				t.Errorf("CanonicalizeGPUType(%q) = (%q, %v); want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestIsKnownGPUType(t *testing.T) {
	if !IsKnownGPUType(GPUTypeB200) {
		t.Error("B200 must be known")
	}
	if IsKnownGPUType(GPUType("not_a_gpu")) {
		t.Error("a made-up id must not be known")
	}
}

// A refusal has to be able to name the way forward — see the doc comment on
// GPUTypesMatchingPrefix.
func TestGPUTypesMatchingPrefix(t *testing.T) {
	got := GPUTypesMatchingPrefix("H100", 5)
	want := []GPUType{GPUTypeH100NVL, GPUTypeH100PCIE, GPUTypeH100SXM}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GPUTypesMatchingPrefix(\"H100\") = %v; want %v", got, want)
	}

	if got := GPUTypesMatchingPrefix("a100", 5); len(got) != 2 {
		t.Errorf("a100 should suggest both capacities; got %v", got)
	}
	if got := GPUTypesMatchingPrefix("H100", 2); len(got) != 2 {
		t.Errorf("max must cap the suggestion list; got %v", got)
	}
	if got := GPUTypesMatchingPrefix("", 5); got != nil {
		t.Errorf("empty input has no better guess than silence; got %v", got)
	}
	if got := GPUTypesMatchingPrefix("zzz", 5); got != nil {
		t.Errorf("nothing matches; got %v", got)
	}
}

// TestGPUTypeSuggestionsFor_BacksOffToTheFamily pins the behaviour that makes an
// unknown-model refusal actionable rather than a dead end.
//
// The first two cases are what plain prefix completion already did. The third
// is the one it could not do and the reason this function exists: `h100_80gb`
// completes to nothing (no canonical id starts with "h10080gb"), so before the
// backoff a customer naming a GPU we plainly stock got a bare "no".
func TestGPUTypeSuggestionsFor_BacksOffToTheFamily(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []GPUType
	}{
		{"exact id suggests itself", "h100_sxm", []GPUType{"h100_sxm"}},
		{"generation completes to its SKUs", "h100", []GPUType{"h100_nvl", "h100_pcie", "h100_sxm"}},
		{"over-specified id backs off to the family", "h100_80gb", []GPUType{"h100_nvl", "h100_pcie", "h100_sxm"}},
		{"marketing spelling backs off too", "H100 80GB SXM5", []GPUType{"h100_nvl", "h100_pcie", "h100_sxm"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := GPUTypeSuggestionsFor(tc.input, 3)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestGPUTypeSuggestionsFor_StaysSilentWhenItHasNothingToSay — a suggestion has
// to be defensible as "you named this family". Backing off far enough to match
// anything at all would offer a essentially random GPU to someone who typed a
// word, and on a surface about spend that is worse than saying nothing.
func TestGPUTypeSuggestionsFor_StaysSilentWhenItHasNothingToSay(t *testing.T) {
	// `t400` is the case that set the stem floor at three. At two it backed off
	// to `t4` — a real but materially weaker card — and named it as if that were
	// what the caller meant. A refusal that guesses wrong about which GPU you
	// want is worse on a spend surface than one that just says no.
	for _, input := range []string{"", "   ", "banana", "not_a_real_gpu", "x", "t400", "gh200", "mi300x"} {
		if got := GPUTypeSuggestionsFor(input, 3); len(got) != 0 {
			t.Errorf("GPUTypeSuggestionsFor(%q) = %v, want none", input, got)
		}
	}
}

// TestGPUTypeSuggestionsFor_RespectsTheCap keeps a refusal readable: the point
// is to name a way forward, not to paste the catalog into an error string.
func TestGPUTypeSuggestionsFor_RespectsTheCap(t *testing.T) {
	if got := GPUTypeSuggestionsFor("rtx", 3); len(got) > 3 {
		t.Errorf("cap ignored: got %d suggestions %v", len(got), got)
	}
}

// TestGPUTypeSuggestionsFor_BoundsItsInput — the backoff is O(len × catalog)
// with a sort per step and runs on a path any caller-supplied string reaches,
// so the input is truncated before the loop starts. Nothing longer than the cap
// could name a GPU, so this costs no real suggestion.
func TestGPUTypeSuggestionsFor_BoundsItsInput(t *testing.T) {
	huge := "h100" + strings.Repeat("z", 64*1024)
	done := make(chan []GPUType, 1)
	go func() { done <- GPUTypeSuggestionsFor(huge, 3) }()
	select {
	case got := <-done:
		// The cap bounds the WORK, not the answer. "h100"+64KB truncates to 24
		// characters, which still carries the h100 stem, so finding the family
		// is the right result — arrived at in ~20 steps instead of ~65,000.
		if len(got) != 3 {
			t.Errorf("want the h100 family from the truncated stem, got %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GPUTypeSuggestionsFor did not bound its input: still running after 2s")
	}
}

// TestGPUTypeSuggestionsFor_HandlesNonASCII — the loop walks RUNES. Slicing the
// byte string could cut a multi-byte character in half, which cannot match
// anything but makes the function's own "walk the input's prefixes" contract
// untrue. Correct behaviour here is simply not to crash and not to guess.
func TestGPUTypeSuggestionsFor_HandlesNonASCII(t *testing.T) {
	for _, input := range []string{"h100—80gb", "Ｈ100", "гпу", "🎮🎮🎮"} {
		_ = GPUTypeSuggestionsFor(input, 3) // must not panic
	}
}
