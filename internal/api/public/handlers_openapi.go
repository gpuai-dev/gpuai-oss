package public

import (
	"log/slog"
	"net/http"

	"github.com/gpuai-dev/gpuai-oss/internal/api/public/openapi"
)

// handleOpenAPISpec serves GET /v1/openapi.json. No auth required (D-03);
// SDK generators and curious customers consume the spec anonymously.
//
// The spec is embedded at compile time and converted from YAML→JSON exactly
// once on first call (sync.Once inside openapi.SpecJSON). Subsequent calls
// are pure bytes-write — no per-request CPU.
//
// Cache-Control: public, max-age=300 lets CDNs cache the spec for 5 min;
// the spec only changes on releases, but we keep the window short so a hot
// fix propagates within a few minutes.
func handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	spec, err := openapi.SpecJSON()
	if err != nil {
		slog.ErrorContext(r.Context(), "openapi spec failed to load", "error", err.Error())
		writeProblem(w, r, http.StatusInternalServerError, "internal_error",
			"OpenAPI specification temporarily unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(spec)
}
