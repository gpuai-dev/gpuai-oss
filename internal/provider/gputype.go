package provider

import (
	"sort"
	"strings"
)

// gputype.go answers one question: is this string a GPU model the platform can
// ever offer?
//
// WHY IT EXISTS (FE finding, 2026-09-09). A watch rule's gpu_model was accepted
// as free text on both the dashboard and the public API, and the alert
// evaluator matches it by EXACT string equality against the availability cache
// (internal/alerts/condition.go: `o.GPUModel != t.GPUModel`). So a rule saved
// for "h100-sxm", "H100" or anything else typed by hand sits in the watchlist
// looking perfectly healthy and can NEVER fire — the customer waits for an
// alert that is structurally incapable of arriving. That is a trust bug against
// the feature's whole promise, and it is worse than the chart's unknown-model
// confusion because it silently PERSISTS a dead rule rather than showing one
// confusing screen.
//
// The truth set is the GPUType constants: an AvailableOffering's GPUModel is
// `string(o.GPUType)` (availability/types.go), so a string outside this set
// cannot appear in the catalog, which means no condition keyed on it can ever
// be true.
//
// Punctuation and case carry no meaning between our own surfaces — the API
// emits `rtx_a4500`, the docs shipped `rtx-a4500`, a human types `RTX A4500` —
// so those spellings are ALIASES to be resolved, not errors to be refused. Only
// a string that resolves to nothing is refused.

// normalizeGPUTypeKey collapses a model id to its comparison form. It is the
// same normalisation frontend/src/lib/gpu-model-lookup.ts applies, deliberately:
// the dashboard's fast client-side feedback and this server-side gate must agree
// about what counts as the same GPU, or one of them is lying to the customer.
func normalizeGPUTypeKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if r == ' ' || r == '\t' || r == '_' || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// canonicalGPUTypeIndex maps every declared GPUType's comparison form to the
// type itself.
//
// Sourced from vramByGPUType rather than a second hand-written list: that map is
// locked to the GPUType constants by TestVRAMByGPUTypeCoversAllGPUTypes, which
// parses types.go, so a GPU type added to types.go and given its VRAM row is
// automatically watchable here. A separate list would be one more place to
// forget, and forgetting it would refuse a real GPU.
var canonicalGPUTypeIndex = func() map[string]GPUType {
	idx := make(map[string]GPUType, len(vramByGPUType))
	for gt := range vramByGPUType {
		idx[normalizeGPUTypeKey(string(gt))] = gt
	}
	return idx
}()

// CanonicalizeGPUType resolves a caller-supplied model id to the canonical
// GPUType it names, reporting false when nothing we list matches.
//
// A differently-punctuated or differently-cased spelling resolves to the
// canonical id; the caller should STORE what comes back rather than what was
// sent, so a rule saved as "RTX A4500" is stored as `rtx_a4500` and can
// actually match the catalog.
//
// This is stock-independent on purpose. The most valuable watch is on a GPU that
// has no stock right now — that is the entire point of asking to be told when it
// comes back — so the live catalog must NOT be the gate here. A sold-out model
// is a real model.
func CanonicalizeGPUType(s string) (GPUType, bool) {
	gt, ok := canonicalGPUTypeIndex[normalizeGPUTypeKey(s)]
	return gt, ok
}

// IsKnownGPUType reports whether t is a GPU model the platform lists, ignoring
// current stock.
func IsKnownGPUType(t GPUType) bool {
	_, ok := canonicalGPUTypeIndex[normalizeGPUTypeKey(string(t))]
	return ok
}

// GPUTypesMatchingPrefix returns the canonical types whose comparison form
// starts with s's, sorted, capped at max.
//
// It exists so a refusal can NAME the way forward. "H100" is not a model id we
// can act on — there are three H100 SKUs and they are different products at
// different prices — but a customer typing it has told us exactly what they
// want, and answering "no" without answering "did you mean one of these" turns
// a two-second correction into a hunt through the catalog. Same reason
// GPUTypesWithVRAMAtLeast exists on the template floor.
//
// Returns nil for empty input rather than the whole catalog: an empty box is
// not a guess we can improve on.
func GPUTypesMatchingPrefix(s string, max int) []GPUType {
	key := normalizeGPUTypeKey(s)
	if key == "" || max <= 0 {
		return nil
	}
	var out []GPUType
	for norm, gt := range canonicalGPUTypeIndex {
		if strings.HasPrefix(norm, key) {
			out = append(out, gt)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// GPUTypeSuggestionsFor is the suggestion entry point every "unknown GPU model"
// refusal should use. It answers "did you mean…" for a model id we do not list.
//
// It tries GPUTypesMatchingPrefix first — the caller typed a STEM and we
// complete it, which is the "H100" case: three SKUs, all named. When nothing
// completes the input, it falls back to walking the input's own prefixes from
// longest to shortest and returning the first that completes to anything.
//
// The fallback is what makes an OVER-specified id recoverable, and that is the
// common shape of a real mistake rather than a hypothetical one: `h100_80gb` is
// a perfectly reasonable thing to type — it is how NVIDIA's own marketing names
// the part, and how several competitors' APIs spell it — but our vocabulary
// splits that generation by interconnect (`h100_sxm`, `h100_pcie`, `h100_nvl`)
// because they are differently-priced products. Prefix-completion alone returns
// NOTHING for it (no canonical id starts with "h10080gb"), so the customer gets
// a bare "no" for a model we obviously stock. Backing off to "h100" answers the
// question they were actually asking.
//
// It is deliberately only prefix arithmetic — no edit distance, no fuzzy
// scoring. A suggestion has to be something we can defend as "you named this
// family"; a typo-corrector that offers `l40s` for `l4` would be guessing at
// spend.
//
// Shortening stops at three characters. Two was measurably too eager: it turned
// `t400` into `t4` and `v1000` into `v100`, naming a materially weaker card as
// if it were what the caller meant, which on a surface about spend is worse
// than saying nothing. At three those go silent and the cases worth answering
// (`h100…`, `a100…`, `rtx…`) still resolve.
const minGPUTypeSuggestionStem = 3

// maxGPUTypeSuggestionInput bounds the backoff before it starts. The loop is
// O(len(input) × catalog) with an allocation and a sort per step, and the
// refusal path is reachable with any caller-supplied string — a 64 KB
// gpu_model would otherwise buy ~64k scans of the registry. Nothing longer than
// this could name a GPU anyway, so truncating costs no real suggestion.
const maxGPUTypeSuggestionInput = 24

func GPUTypeSuggestionsFor(s string, max int) []GPUType {
	if out := GPUTypesMatchingPrefix(s, max); len(out) > 0 {
		return out
	}
	// Runes, not bytes: normalizeGPUTypeKey lowercases and strips " \t_-" but
	// preserves every other rune, so slicing the byte string could cut a
	// multi-byte character in half and walk prefixes that are not prefixes of
	// anything a human typed.
	key := []rune(normalizeGPUTypeKey(s))
	if len(key) > maxGPUTypeSuggestionInput {
		key = key[:maxGPUTypeSuggestionInput]
	}
	for n := len(key) - 1; n >= minGPUTypeSuggestionStem; n-- {
		if out := GPUTypesMatchingPrefix(string(key[:n]), max); len(out) > 0 {
			return out
		}
	}
	return nil
}
