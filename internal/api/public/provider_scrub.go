package public

import "github.com/gpuai-dev/gpuai-oss/internal/privacy"

// provider_scrub.go — GPU-61. Upstream provider identities must never reach a
// customer-facing byte (INFPRIV-01..06 extend to the provisioning surface, not
// just inference). The one field on the public API that carries a raw adapter
// error string is Operation.Error.Detail (see handlers_operations.go
// operationToResponse, and the "may contain upstream provider hints" note in
// handlers_instances.go): a capacity failure names the broker on the exact path
// customers scrutinise hardest — failures.
//
// GPU-90 moved the implementation (the brand token list and the whole-word
// regex) into internal/privacy so the fine-tuning error paths, which
// live outside package public, share ONE list instead of forking a second copy
// that would drift. This file is now a thin package-local wrapper: the callers
// below and provider_scrub_test.go keep their existing shape, and there is a
// single place to add a new provider brand.
//
// The inference privacy.ScrubRe is still deliberately NOT reused on its own
// here: its token set includes generic infrastructure nouns that legitimately
// appear in provisioning error text (a compute-node disk-full message, say), so
// it would over-redact. See privacy/provider_scrub.go for the full rationale on
// why the provider brands live in their own error-message-only scrubber.
//
// MAINTENANCE: add new provider brands to privacy.ProviderBrandTokens.
// TestProviderBrandTokensCoverEveryAdapterDir (privacy package) walks
// internal/provider/*/ and fails when an adapter is not covered.

// scrubProviderNames replaces every upstream provider brand in a
// customer-facing error detail with a neutral placeholder, preserving the rest
// of the message. Idempotent and allocation-free when nothing matches.
func scrubProviderNames(s string) string {
	return privacy.ScrubProviderNames(s)
}
