package public

import "net/http"

// chain composes the standard authenticated read/write middleware stack:
//
//	RequestID → ApiKeyAuth → RateLimit → RequireScope → next
//
// Used by Wave 6+ Register*Routes helpers (Plans 18-13/14/15/16). The closure
// captures `deps` so per-call middleware (auth, rate, idempotency) get the
// live DB + Redis pointers from PublicDeps.
//
// resource is one of KnownResources ("instances", "ssh_keys", "billing",
// "webhooks"); level is "read" or "write".
//
// The helper exists at package level (rather than inline in router.go) so
// Wave 6+ Register*Routes helpers in subsequent plans can call it directly.
// This pattern is the linchpin of the plan's router-serialization
// convention: each plan adds ONE LINE to router.go (a Register*Routes call)
// instead of editing the body of RegisterPublicRoutes — which would
// otherwise produce parallel-merge conflicts.
func chain(deps PublicDeps, resource, level string) func(http.Handler) http.Handler {
	rl := NewRateLimiter(deps.Redis)
	// resolveAPIKeyLookup falls back to deps.DB in production (fail-closed,
	// byte-identical). The test-only PublicDepsWithAPIKeyLookup seam lets the
	// CLI<->API integration harness drive the REAL auth chain with a fixed
	// test key (BLOCK-08) without a live Postgres.
	auth := ApiKeyAuthMiddleware(deps.resolveAPIKeyLookup())
	return func(h http.Handler) http.Handler {
		return RequestIDMiddleware(auth(rl.RateLimitMiddleware(RequireScope(resource, level)(h))))
	}
}

// chainWrite layers idempotency on top of `chain` for POST/PATCH/DELETE
// routes. Idempotency middleware reads APIKeyFromContext, so it MUST run
// AFTER ApiKeyAuthMiddleware — chained via `idem(h)` inside the returned
// handler so the api-key context value is available when the idempotency
// middleware looks it up.
//
// All write-resource routes use this helper; SSH-key writes are
// synchronous (no D-08 Operation-Id) but still benefit from idempotency
// for retried-create / retried-delete safety.
func chainWrite(deps PublicDeps, resource string) func(http.Handler) http.Handler {
	idem := PublicIdempotencyMiddleware(deps.DB)
	return func(h http.Handler) http.Handler {
		return chain(deps, resource, "write")(idem(h))
	}
}
