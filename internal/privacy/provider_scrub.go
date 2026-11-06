// Package privacy keeps upstream supplier identities out of customer-facing
// error text.
//
// Two customer-facing surfaces can carry a raw adapter/engine error string:
// Operation.Error.Detail on the instances/operations API, and the
// provisioning failure message rendered by internal/provision. Both name the
// upstream supplier on the exact path customers scrutinise hardest (failures),
// so error text is passed through ScrubProviderNames before it leaves the
// server. The rest of the message ("no GPU capacity available") is preserved.
//
// The token set has two halves:
//
//   - ProviderBrandTokens: the compiled-in adapters (every directory under
//     internal/provider/ with an adapter.go, minus the internal "generic"
//     multiplexer). TestProviderBrandTokensCoverEveryAdapterDir fails when a
//     new adapter is added without a token here.
//   - Runtime tokens registered with RegisterBrandTokens: on a self-hosted
//     deployment the operator configures generic suppliers by slug and name
//     (GENERIC_SUPPLIERS), so those names are only known at startup.
//
// Internal logs may still name suppliers: on a self-hosted deployment the
// operator owns them. Only customer-facing bytes are scrubbed.
package privacy

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ProviderBrandTokens lists every compiled-in adapter name that is an upstream
// brand. "generic" is an internal registry name, far too common a word to
// redact safely, and is omitted by design.
var ProviderBrandTokens = []string{
	"vultr",
}

var (
	mu            sync.RWMutex
	runtimeTokens = map[string]bool{}
	scrubRe       = compile(nil)
)

// compile builds the whole-word, case-insensitive matcher for the static
// tokens plus extra. A brand may be followed by a domain suffix (example.cloud)
// so the scrub leaves no ".cloud" fragment behind.
func compile(extra []string) *regexp.Regexp {
	toks := make([]string, 0, len(ProviderBrandTokens)+len(extra))
	for _, t := range ProviderBrandTokens {
		toks = append(toks, regexp.QuoteMeta(strings.ToLower(t)))
	}
	for _, t := range extra {
		toks = append(toks, regexp.QuoteMeta(strings.ToLower(t)))
	}
	// Longest first so a token that is a prefix of another cannot win early.
	sort.Slice(toks, func(i, j int) bool { return len(toks[i]) > len(toks[j]) })
	return regexp.MustCompile(`(?i)\b(` + strings.Join(toks, "|") + `)(\.[a-z]{2,})?\b`)
}

// RegisterBrandTokens adds operator-configured supplier identities (generic
// supplier slugs and display names) to the scrubber. Empty values, values
// shorter than three characters, and the internal name "generic" are ignored:
// they would over-redact ordinary error text. Safe for concurrent use;
// typically called once at startup.
func RegisterBrandTokens(tokens ...string) {
	mu.Lock()
	defer mu.Unlock()
	changed := false
	for _, t := range tokens {
		t = strings.ToLower(strings.TrimSpace(t))
		if len(t) < 3 || t == "generic" || runtimeTokens[t] {
			continue
		}
		runtimeTokens[t] = true
		changed = true
	}
	if !changed {
		return
	}
	extra := make([]string, 0, len(runtimeTokens))
	for t := range runtimeTokens {
		extra = append(extra, t)
	}
	scrubRe = compile(extra)
}

// resetRuntimeTokens clears runtime registrations (tests only).
func resetRuntimeTokens() {
	mu.Lock()
	defer mu.Unlock()
	runtimeTokens = map[string]bool{}
	scrubRe = compile(nil)
}

// ScrubProviderNames replaces every supplier brand in a customer-facing error
// string with a neutral placeholder, preserving the rest of the message.
// Idempotent; returns the input unchanged when nothing matches.
func ScrubProviderNames(s string) string {
	if s == "" {
		return s
	}
	mu.RLock()
	re := scrubRe
	mu.RUnlock()
	return re.ReplaceAllString(s, "the provider")
}
