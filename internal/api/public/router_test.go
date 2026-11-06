package public_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/api/public"
)

// TestRegisterPublicRoutes_Health asserts that the stub /v1/health endpoint
// is wired and returns the expected JSON shape. This is the smallest
// possible end-to-end test that the public mux is reachable; Wave 3+
// plans replace this with real handler tests.
func TestRegisterPublicRoutes_Health(t *testing.T) {
	mux := http.NewServeMux()
	public.RegisterPublicRoutes(mux, public.PublicDeps{})

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body: got %q, want {\"status\":\"ok\"}", w.Body.String())
	}
}
