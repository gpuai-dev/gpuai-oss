// Package instanceid is the single source of truth for the gpu-ai instance-ID
// grammar. An instance ID is "gpu-" followed by exactly 8 lowercase hex chars
// (e.g. "gpu-207db644"), 12 chars total — see provision.generateInstanceID,
// which mints it and which every provider adapter sets as the upstream resource
// Name during Provision.
//
// It exists because the ownership check used to live in three hand-maintained
// copies (the reconciler's parseOurInstanceID plus a looksLikeUUID in two
// adapters). Those copies drifted: they matched 36-char UUIDs, which
// we never mint, so the drift reconciler treated every real instance as foreign
// and stayed silently inert — a leaked A100 VM ran ~7 days undetected
// (2026-06-19). One definition, used everywhere, keeps that from recurring.
package instanceid

import "strings"

const (
	// prefix is the brand all gpu-ai instance IDs carry.
	prefix = "gpu-"
	// hexLen is the hex-character count after the prefix (4 random bytes).
	hexLen = 8
	// Len is the total length of a canonical instance ID: "gpu-" + 8 hex.
	Len = len(prefix) + hexLen
)

// Parse reports whether name is one we minted and, if so, returns the canonical
// instance ID. The returned id is the FULL "gpu-<8hex>" string (lowercased,
// trimmed) — byte-identical to what adapters set as the upstream Name and to
// what the DB stores in instance_id — so callers can compare it directly
// against a stored InstanceID.
//
// It matches ONLY the exact grammar: not a UUID, not a substring, not "anything
// containing hex". A third-party resource named "<their-job>-..." launched
// under a shared upstream API key must NOT parse as ours, or the reconciler's
// enforce path could terminate someone else's instance (WR-04). Tighten, never
// widen.
func Parse(name string) (id string, ok bool) {
	name = strings.TrimSpace(strings.ToLower(name))
	if len(name) != Len {
		return "", false
	}
	rest, found := strings.CutPrefix(name, prefix)
	if !found || len(rest) != hexLen {
		return "", false
	}
	for i := 0; i < len(rest); i++ {
		if !isHex(rest[i]) {
			return "", false
		}
	}
	return name, true
}

// IsOurs reports whether name was minted by THIS process's environment — the
// ownership gate for any destructive reconciler action. Convenience boolean
// wrapper around OwnedHere; see it for the env-scoping rationale. Used by the
// adapters' ListProvisioned ownership filters and the drift reconciler.
func IsOurs(name string) bool {
	_, ok := OwnedHere(name)
	return ok
}

// localTag is the short environment tag (e.g. "prod", "demo") this process
// stamps onto every upstream resource name it mints, and the ONLY tag whose
// instances its reconciler will treat as ours. Empty (development / unset) =
// untagged: names are minted bare ("gpu-<8hex>") and only bare names are owned —
// byte-identical to the pre-2026-06-20 behaviour, so single-env and dev/test
// setups are unaffected. Set once at startup via SetLocalEnv.
//
// Why it exists: demo and prod can share one provider account. With both
// reconcilers enforcing, each saw the OTHER env's freshly-minted "gpu-<8hex>" pods as
// orphans (no matching DB row) and terminated them within seconds, so no GPU
// instance could reach running (2026-06-20). Stamping the env into the upstream
// NAME and scoping ownership to the local tag isolates the environments on a
// shared provider account, without separate accounts per env.
var localTag string

// SetLocalEnv records this process's deployment environment. Call once at
// startup, before any provisioning or reconciliation runs. Accepts the raw
// GPUCTL_ENV value ("development"|"demo"|"production"); unknown/empty/dev map to
// the untagged tag, preserving legacy behaviour.
func SetLocalEnv(env string) {
	localTag = tagFor(env)
}

// tagFor maps a GPUCTL_ENV value to the short tag embedded in upstream resource
// names. Only "production"/"demo" are tagged; everything else (including
// "development" and unset) is untagged ("").
func tagFor(env string) string {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "production", "prod":
		return "prod"
	case "demo":
		return "demo"
	default:
		return ""
	}
}

// FormatUpstreamName returns the name to set on an upstream provider resource
// for instance id. In a tagged env it prefixes the local env tag (e.g.
// "prod-gpu-207db644"); in development (untagged) it returns id unchanged.
// Every adapter MUST set the upstream resource Name to this value so the
// reconciler can tell environments apart on a shared provider account. The id
// stored in the DB (instance_id) stays the bare "gpu-<8hex>".
func FormatUpstreamName(id string) string {
	if localTag == "" {
		return id
	}
	return localTag + "-" + id
}

// OwnedHere reports whether name was minted by THIS process's environment and,
// if so, returns the canonical bare instance ID ("gpu-<8hex>") for comparison
// against a stored InstanceID. It is the ownership gate for destructive
// reconciler actions on a possibly-SHARED provider account.
//
//   - Untagged process (development): only bare "gpu-<8hex>" names are ours —
//     identical to Parse, so legacy/single-env behaviour is unchanged.
//   - Tagged process (demo/prod): ONLY "<localtag>-gpu-<8hex>" is ours. A name
//     carrying a different env's tag, or an untagged legacy name, is NOT ours,
//     so we never terminate another environment's instance.
func OwnedHere(name string) (id string, ok bool) {
	name = strings.TrimSpace(strings.ToLower(name))
	if localTag == "" {
		return Parse(name)
	}
	rest, found := strings.CutPrefix(name, localTag+"-")
	if !found {
		return "", false
	}
	return Parse(rest)
}

func isHex(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'f':
		return true
	default:
		return false
	}
}
