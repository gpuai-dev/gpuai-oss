package public

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestWriteProblemEnvelope: verifies the wire shape of writeProblem.
func TestWriteProblemEnvelope(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/instances", nil)
	// Inject a request-id into context the way RequestIDMiddleware will.
	req = req.WithContext(WithRequestID(req.Context(), "req-test-123"))
	w := httptest.NewRecorder()

	writeProblem(w, req, http.StatusUnauthorized, "unauthenticated", "Missing API key")

	if got := w.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type: got %q, want application/problem+json", got)
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", w.Code)
	}
	var p Problem
	if err := json.NewDecoder(w.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Type != "https://api.gpu.ai/errors/unauthenticated" {
		t.Errorf("type: got %q", p.Type)
	}
	if p.Title != "Unauthorized" {
		t.Errorf("title: got %q, want Unauthorized", p.Title)
	}
	if p.Status != 401 {
		t.Errorf("status: got %d", p.Status)
	}
	if p.Detail != "Missing API key" {
		t.Errorf("detail: got %q", p.Detail)
	}
	if p.Code != "unauthenticated" {
		t.Errorf("code: got %q", p.Code)
	}
	if p.RequestID != "req-test-123" {
		t.Errorf("request_id: got %q, want req-test-123", p.RequestID)
	}
}

// TestCodeRegistryNonEmpty: sanity check that the registry has the
// canonical 13 entries from RESEARCH.md §6.
func TestCodeRegistryNonEmpty(t *testing.T) {
	if len(codeRegistry) < 13 {
		t.Fatalf("registry has %d entries, want >= 13 (RESEARCH.md §6)", len(codeRegistry))
	}
	// Spot-check the canonical codes.
	canonical := []string{"unauthenticated", "invalid_api_key", "insufficient_scope",
		"not_found", "idempotency_conflict", "idempotency_mismatch", "validation_failed",
		"rate_limited", "internal_error"}
	for _, c := range canonical {
		if _, ok := codeRegistry[c]; !ok {
			t.Errorf("registry missing canonical code %q", c)
		}
	}
}

// TestCodeRegistryCoherence: greps the public-API package for every
// writeProblem call and asserts the code argument is in the registry.
// Handlers can't drift to undocumented codes without this test failing.
//
// Skipped when grep is unavailable (e.g. unusual CI runner) so the test
// is informational rather than fragile.
func TestCodeRegistryCoherence(t *testing.T) {
	if _, err := exec.LookPath("grep"); err != nil {
		t.Skip("grep unavailable")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// Find every writeProblem(...) literal call and capture the 4th arg
	// (the code string). Pattern: writeProblem(w, r, <status>, "<code>", ...)
	cmd := exec.Command("grep", "-rhn",
		`writeProblem(`,
		cwd)
	out, _ := cmd.CombinedOutput()
	re := regexp.MustCompile(`writeProblem\([^)]*?,\s*\d+,\s*"([a-z_]+)"`)
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "_test.go") {
			continue
		}
		if m := re.FindStringSubmatch(line); m != nil {
			seen[m[1]] = true
		}
	}
	for code := range seen {
		if _, ok := codeRegistry[code]; !ok {
			t.Errorf("handler uses code %q which is NOT in codeRegistry — add it to internal/api/public/errors.go", code)
		}
	}
}
