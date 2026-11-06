package public

import (
	"encoding/json"
	"net/http"
)

// RegisterPublicRoutes wires every /v1/* route on the supplied mux.
// Wave 3+ plans extend this function with auth/idempotency/rate-limit chains.
//
// Currently registered (no auth):
//   - GET /v1/health        — stub liveness probe (Wave 2)
//   - GET /v1/openapi.json  — embedded OpenAPI 3.1 spec (Wave 4, Plan 18-10, D-03)
//   - GET /v1/gpu-types     — public GPU catalog (Wave 5, Plan 18-12, PUBAPI-03)
//   - GET /v1/pricing       — public pricing catalog (Wave 5, Plan 18-12, PUBAPI-03)
//
// Wave 5 router-serialization convention: each handler module defines its
// own RegisterXxxRoutes helper so this function stays a thin aggregator.
// Plans 18-13/14/15/16 each add ONE LINE here for their own helper.
//
// The function takes the mux as an argument (rather than constructing one
// internally) so api.Server can keep a single source of truth for the mux
// it later dispatches to.
func RegisterPublicRoutes(mux *http.ServeMux, deps PublicDeps) {
	mux.Handle("GET /v1/health", RequestIDMiddleware(http.HandlerFunc(handleHealth)))
	mux.Handle("GET /v1/openapi.json", RequestIDMiddleware(http.HandlerFunc(handleOpenAPISpec)))

	// Wave 5 — public catalog (no auth). Each handler module self-registers
	// via Register*Routes; subsequent plans (18-13/14/15/16) follow this
	// pattern to limit router.go edits to a single line per plan.
	RegisterCatalogRoutes(mux, deps)

	// Wave 6 — authenticated SSH-key CRUD (Plan 18-13, PUBAPI-06). The
	// chain/chainWrite helpers from chain.go wrap each route with the
	// standard ApiKeyAuth → RateLimit → RequireScope (+ Idempotency on
	// writes) middleware stack.
	RegisterSSHKeyRoutes(mux, deps)

	// Custom images Phase 2 — private-registry credential CRUD, scoped under
	// "instances" (see RegisterRegistryCredentialRoutes for why no new scope
	// resource). 503s environment-wide until REGISTRY_CREDENTIALS_KEY is set.
	RegisterRegistryCredentialRoutes(mux, deps)

	// Wave 7 — instance lifecycle + operations FSM (Plan 18-14,
	// PUBAPI-04 + PUBAPI-05). Adds the 5 instance handlers + the
	// GET /v1/operations/{id} polling endpoint that exposes the D-08
	// async-write Operation FSM.
	RegisterInstanceRoutes(mux, deps)

}

// handleHealth is a stub liveness probe. Returns 200 with {"status":"ok"}.
// Replaced by a real health check in a follow-up plan if needed; for now
// it exists so smoke tests can verify the new mux is reachable.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
