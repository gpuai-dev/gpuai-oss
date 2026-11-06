// Stage-0 bootstrap fetch tests. Mirrors handlers_internal_telemetry_test.go:
// a fake bootstrapFetchStore lets the fetch be asserted without a live
// Postgres. The per-instance token auth (InstanceTokenAuth) is exercised in
// the middleware tests; this file asserts the handler serves the staged
// script as text/plain on the happy path, 404s when nothing is staged, and
// 400s on a missing id.

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// fakeBootstrapStore satisfies the bootstrap fetch seam: a fixed script per
// instance id, db.ErrNotFound otherwise.
type fakeBootstrapStore struct {
	scripts map[string]string
}

func (f *fakeBootstrapStore) GetStagedBootstrap(_ context.Context, instanceID string) (string, error) {
	s, ok := f.scripts[instanceID]
	if !ok {
		return "", db.ErrNotFound
	}
	return s, nil
}

func fireBootstrapFetch(t *testing.T, store *fakeBootstrapStore, instID string) *httptest.ResponseRecorder {
	t.Helper()
	s := newBootstrapHandlerForTest(store)
	req := httptest.NewRequest(http.MethodGet, "/internal/instances/"+instID+"/bootstrap", nil)
	req.SetPathValue("id", instID)
	w := httptest.NewRecorder()
	s.handleInstanceBootstrap(w, req)
	return w
}

// TestInstanceBootstrap_HappyPath asserts a staged script is served verbatim
// as no-store text/plain.
func TestInstanceBootstrap_HappyPath(t *testing.T) {
	const script = "#!/bin/bash\necho frp-bootstrap\n"
	store := &fakeBootstrapStore{scripts: map[string]string{"gpu-bs1": script}}

	w := fireBootstrapFetch(t, store, "gpu-bs1")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != script {
		t.Errorf("body = %q, want the staged script %q", got, script)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (script embeds per-instance tokens)", cc)
	}
}

// TestInstanceBootstrap_NotStaged asserts 404 once the row is consumed/swept
// (or never staged) — the loader's retry loop treats it as a hard stop after
// its budget, and a post-boot fetch must not leak anything.
func TestInstanceBootstrap_NotStaged(t *testing.T) {
	store := &fakeBootstrapStore{scripts: map[string]string{}}
	if w := fireBootstrapFetch(t, store, "gpu-missing"); w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestInstanceBootstrap_MissingID asserts a blank path id 400s.
func TestInstanceBootstrap_MissingID(t *testing.T) {
	if w := fireBootstrapFetch(t, &fakeBootstrapStore{}, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}
