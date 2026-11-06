// Package public implements the public developer API mounted at /v1/*.
//
// This package is intentionally separate from internal/api: response shapes
// and middleware here MUST NOT depend on Clerk session context, dashboard
// types, or any internal/api exports. The TestNoInternalAPIImports test in
// banned_imports_test.go enforces this at CI time.
//
// See .planning/phases/18-public-developer-api-v1/18-RESEARCH.md for the
// architecture rationale (Pitfall 1) and full package layout.
package public

import (
	"log/slog"
	"sync"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
	"github.com/gpuai-dev/gpuai-oss/internal/registrycheck"
	"github.com/gpuai-dev/gpuai-oss/internal/registrycred"
	tmplcatalog "github.com/gpuai-dev/gpuai-oss/internal/template/catalog"
	"github.com/redis/go-redis/v9"
)

// defaultRegistryChecker is the shared production pre-flight client, built on
// first use so DB-less test suites that never launch a custom image pay no
// setup cost and open no sockets.
var defaultRegistryChecker = sync.OnceValue(func() *registrycheck.Checker { return registrycheck.New() })

// registryChecker returns the injected pre-flight checker, or the shared
// production default when none was wired.
func (deps PublicDeps) registryChecker() *registrycheck.Checker {
	if deps.RegistryChecker != nil {
		return deps.RegistryChecker
	}
	return defaultRegistryChecker()
}

// PublicDeps holds the dependencies injected into the public-API package.
// Mirrors the constructor-injection pattern used by api.ServerDeps.
type PublicDeps struct {
	DB         *db.Pool
	Redis      *redis.Client
	Config     *config.Config
	Engine     *provision.Engine
	AvailCache *availability.Cache
	Providers  *provider.Registry
	Logger     *slog.Logger

	// TemplateCatalog backs the GET /v1/templates surface (Phase 31, TMPL-02).
	// Held as the read-only interface (forward-compat for a future DB-backed
	// swap); the concrete *tmplcatalog.Catalog satisfies it. cmd/gpuctl injects
	// the boot-loaded catalog via api.ServerDeps → this field.
	TemplateCatalog tmplcatalog.TemplateCatalog

	// RegistryChecker backs the custom-image pre-flight (Phase 1) in
	// handleCreateInstance. nil => the accessor falls back to a lazily built
	// production checker, so cmd/gpuctl needs no wiring; tests inject a
	// checker pointed at an httptest registry to keep handler tests offline.
	RegistryChecker *registrycheck.Checker

	// RegistryCredSealer seals/opens private-registry passwords (Phase 2,
	// REGISTRY_CREDENTIALS_KEY). nil or disabled => the registry-credential
	// endpoints answer 503 and instance launches refuse
	// registry_credential_id — the feature is dark until the environment
	// carries the key.
	RegistryCredSealer *registrycred.Sealer

	// registryCredStore is the test seam for the registry-credential store;
	// production falls through to DB.
	registryCredStore registryCredentialDeps

	// sshKeyStore is the test-only override for SSH-key persistence. nil in
	// production (handlers fall through to deps.DB). Populated via
	// PublicDepsWithSSHKeyStore — which is the only allowed seam, since the
	// field is unexported. See handlers_ssh_keys.go for the rationale.
	sshKeyStore sshKeyDeps

	// opStore is the test-only override for the operations FSM persistence
	// seam consumed by handlers_operations.go and handlers_instances.go.
	// nil in production (handlers fall through to deps.DB). Populated via
	// PublicDepsWithOperationStore. Kept unexported so production code
	// cannot accidentally inject a stub.
	opStore operationStore

	// instStore is the test-only override for the instance-list/get/update
	// seam consumed by handlers_instances.go. nil in production. Populated
	// via PublicDepsWithInstanceStore.
	instStore instanceStore

	// engineRunner is the test-only override for the provisioning engine
	// surface (Provision + Terminate). nil in production (handlers fall
	// through to deps.Engine). Populated via PublicDepsWithEngineRunner.
	// Kept unexported so production code cannot accidentally inject a stub.
	engineRunner engineRunner

	// apiKeyLookup is the test-only override for the API-key resolution seam
	// consumed by the auth chain (chain.go → ApiKeyAuthMiddleware). nil in
	// production — the chain falls through to deps.DB, which already
	// implements ApiKeyLookup, so production auth stays fail-closed and
	// byte-identical. Populated via PublicDepsWithAPIKeyLookup (BLOCK-08).
	// Kept unexported so production code cannot accidentally inject a stub
	// that would bypass the real DB-backed key validation.
	apiKeyLookup ApiKeyLookup

	// auditStore is the test-only override for the audit-log write seam
	// consumed by handlers_instances.go's terminate audit emit (Phase 39
	// Plan 05, SEC-09). nil in production — the handler falls through to
	// deps.DB which already implements auditStore via CreateAuditEntry.
	// Populated via PublicDepsWithAuditStore.
	auditStore auditStore
}

// resolveAPIKeyLookup returns the test override when present, otherwise the
// *db.Pool which already implements ApiKeyLookup. The auth chain (chain.go)
// consults this accessor so a nil override falls through to the production
// DB-backed lookup — fail-closed and unchanged.
func (deps PublicDeps) resolveAPIKeyLookup() ApiKeyLookup {
	if deps.apiKeyLookup != nil {
		return deps.apiKeyLookup
	}
	return deps.DB
}

// PublicDepsWithOperationStore returns a copy of deps wired with a custom
// operations-FSM store. Tests use this to inject in-memory fixtures
// without spinning up Postgres. Production callers MUST NOT use this
// seam — *db.Pool already implements operationStore and is wired
// automatically when DB is set.
func PublicDepsWithOperationStore(deps PublicDeps, store operationStore) PublicDeps {
	deps.opStore = store
	return deps
}

// PublicDepsWithInstanceStore returns a copy of deps wired with a custom
// instance store. Same test-only contract as PublicDepsWithSSHKeyStore.
func PublicDepsWithInstanceStore(deps PublicDeps, store instanceStore) PublicDeps {
	deps.instStore = store
	return deps
}

// PublicDepsWithEngineRunner returns a copy of deps wired with a custom
// engine runner. Tests use this to bypass the real Engine.Provision (which
// requires a provider registry, FRP tunnel manager, and live DB) — the
// stub returns a fixed InstanceID so the handler can exercise its FSM
// transitions in isolation.
func PublicDepsWithEngineRunner(deps PublicDeps, runner engineRunner) PublicDeps {
	deps.engineRunner = runner
	return deps
}

// PublicDepsWithAPIKeyLookup returns a copy of deps wired with a custom
// API-key resolver. Same test-only contract as the other PublicDepsWith*
// helpers — the CLI<->API integration harness (cmd/gpu, BLOCK-08) injects a
// stub so the REAL ApiKeyAuthMiddleware accepts a fixed test key without a
// live Postgres, exercising the full chainWrite middleware stack end-to-end.
//
// Production callers MUST NOT use this seam — *db.Pool already implements
// ApiKeyLookup and is wired automatically via resolveAPIKeyLookup() when DB is
// set. With no lookup injected, auth behaves byte-identically to today (same
// DB lookup, same fail-closed reject on a missing/invalid/revoked key), so
// this seam does NOT regress the BLOCK-05 fail-closed-auth hardening.
func PublicDepsWithAPIKeyLookup(deps PublicDeps, lookup ApiKeyLookup) PublicDeps {
	deps.apiKeyLookup = lookup
	return deps
}

// PublicDepsWithAuditStore returns a copy of deps wired with a custom
// audit-log writer (Phase 39 Plan 05, SEC-09). Same test-only contract as
// the other PublicDepsWith* helpers — tests inject a capturing stub so the
// public-surface instance.terminated emit can be asserted without Postgres.
// Production callers MUST NOT use this seam — *db.Pool already implements
// auditStore and is wired automatically when DB is set.
func PublicDepsWithAuditStore(deps PublicDeps, store auditStore) PublicDeps {
	deps.auditStore = store
	return deps
}
