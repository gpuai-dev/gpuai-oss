package public

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// stubIdemDeps is an in-memory implementation of PublicIdempotencyDeps for
// fast unit tests. Keyed by (apiKeyID, key) — independent of Postgres.
type stubIdemDeps struct {
	mu   sync.Mutex
	rows map[string]*db.PublicIdempotencyKey
}

func newStubIdemDeps() *stubIdemDeps {
	return &stubIdemDeps{rows: make(map[string]*db.PublicIdempotencyKey)}
}

func stubKey(apiKeyID, key string) string { return apiKeyID + "::" + key }

func (s *stubIdemDeps) GetPublicIdempotencyKey(_ context.Context, apiKeyID, key string) (*db.PublicIdempotencyKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[stubKey(apiKeyID, key)]
	if !ok {
		return nil, db.ErrNotFound
	}
	// Return a copy so tests reading `existing` don't mutate stub state.
	cp := *r
	return &cp, nil
}

func (s *stubIdemDeps) CreatePublicIdempotencyKey(_ context.Context, apiKeyID, key, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[stubKey(apiKeyID, key)]; ok {
		return db.ErrIdempotencyKeyExists
	}
	s.rows[stubKey(apiKeyID, key)] = &db.PublicIdempotencyKey{
		APIKeyID:    apiKeyID,
		Key:         key,
		RequestHash: hash,
	}
	return nil
}

func (s *stubIdemDeps) CompletePublicIdempotencyKey(_ context.Context, apiKeyID, key string, code int, body []byte, headers json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[stubKey(apiKeyID, key)]
	if !ok {
		return errors.New("not found")
	}
	c := code
	r.ResponseCode = &c
	r.ResponseBody = append([]byte(nil), body...)
	r.ResponseHeaders = append(json.RawMessage(nil), headers...)
	return nil
}

// idemReq builds a POST with optional Idempotency-Key header and an APIKey on
// context. body is included verbatim.
func idemReq(apiKeyID, idemKey, body string) *http.Request {
	r := httptest.NewRequest("POST", "/v1/instances", strings.NewReader(body))
	if idemKey != "" {
		r.Header.Set("Idempotency-Key", idemKey)
	}
	if apiKeyID != "" {
		ctx := WithAPIKey(r.Context(), &APIKey{APIKeyID: apiKeyID, OrgID: "org-1"})
		r = r.WithContext(ctx)
	}
	return r
}

func TestIdempotency_NoHeaderPassesThrough(t *testing.T) {
	deps := newStubIdemDeps()
	calls := 0
	h := PublicIdempotencyMiddleware(deps)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, idemReq("api-1", "", `{}`))
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (no Idempotency-Key → pass-through)", calls)
	}
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", w.Code)
	}
	if len(deps.rows) != 0 {
		t.Errorf("DB rows created = %d, want 0 (no header should not touch DB)", len(deps.rows))
	}
}

// TestIdempotency_NilDepsWithHeaderPassesThrough — WR-12: a nil deps wiring
// with a customer-supplied Idempotency-Key header must pass through as a
// no-op, NOT dereference the nil interface (customer-triggerable panic).
func TestIdempotency_NilDepsWithHeaderPassesThrough(t *testing.T) {
	calls := 0
	h := PublicIdempotencyMiddleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, idemReq("api-1", "k-nil-deps", `{}`))
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (nil deps → no-op pass-through)", calls)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestIdempotency_NoAPIKeyPassesThrough(t *testing.T) {
	deps := newStubIdemDeps()
	calls := 0
	h := PublicIdempotencyMiddleware(deps)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	// Idempotency-Key set but no APIKey on ctx — should pass through (auth would have rejected before).
	r := httptest.NewRequest("POST", "/v1/instances", strings.NewReader("{}"))
	r.Header.Set("Idempotency-Key", "k1")
	h.ServeHTTP(w, r)
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (no APIKey → pass-through)", calls)
	}
}

func TestIdempotency_FirstCallStoresResponse(t *testing.T) {
	deps := newStubIdemDeps()
	body := `{"gpu_type":"H100"}`
	called := 0
	h := PublicIdempotencyMiddleware(deps)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"inst_1"}`))
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, idemReq("api-1", "k1", body))

	if called != 1 {
		t.Fatalf("handler calls = %d, want 1", called)
	}
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", w.Code)
	}
	if got := w.Body.String(); got != `{"id":"inst_1"}` {
		t.Errorf("body = %q, want %q", got, `{"id":"inst_1"}`)
	}

	// DB row populated.
	row, err := deps.GetPublicIdempotencyKey(context.Background(), "api-1", "k1")
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.ResponseCode == nil || *row.ResponseCode != 201 {
		t.Errorf("ResponseCode = %v, want 201", row.ResponseCode)
	}
	if string(row.ResponseBody) != `{"id":"inst_1"}` {
		t.Errorf("ResponseBody = %q", string(row.ResponseBody))
	}
	wantHash := sha256.Sum256([]byte(body))
	if row.RequestHash != hex.EncodeToString(wantHash[:]) {
		t.Errorf("RequestHash mismatch")
	}
}

func TestIdempotency_ReplaysSecondCall(t *testing.T) {
	deps := newStubIdemDeps()
	body := `{"gpu_type":"A100"}`
	calls := 0
	h := PublicIdempotencyMiddleware(deps)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"inst_42"}`))
	}))

	// First call.
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, idemReq("api-1", "k1", body))
	if w1.Code != http.StatusCreated {
		t.Fatalf("first call status = %d", w1.Code)
	}

	// Second identical call — handler must NOT run again, response is replayed.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, idemReq("api-1", "k1", body))

	if calls != 1 {
		t.Errorf("handler calls = %d, want 1 (replay must not re-run handler)", calls)
	}
	if w2.Code != http.StatusCreated {
		t.Errorf("replay status = %d, want 201", w2.Code)
	}
	if w2.Body.String() != `{"id":"inst_42"}` {
		t.Errorf("replay body = %q, want byte-for-byte match", w2.Body.String())
	}
}

func TestIdempotency_BodyMismatchReturns422(t *testing.T) {
	deps := newStubIdemDeps()
	calls := 0
	h := PublicIdempotencyMiddleware(deps)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo body so we can confirm the second handler call did NOT happen.
		_, _ = io.Copy(io.Discard, r.Body)
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))

	// First call — populates the row.
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, idemReq("api-1", "k1", `{"a":1}`))
	if w1.Code != http.StatusCreated {
		t.Fatalf("first status = %d", w1.Code)
	}

	// Same key, DIFFERENT body → 422.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, idemReq("api-1", "k1", `{"a":2}`))
	if w2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch status = %d, want 422", w2.Code)
	}
	if calls != 1 {
		t.Errorf("handler calls = %d, want 1 (mismatch must short-circuit)", calls)
	}
	var p Problem
	if err := json.NewDecoder(w2.Body).Decode(&p); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if p.Code != "idempotency_mismatch" {
		t.Errorf("code = %q, want idempotency_mismatch", p.Code)
	}
	if got := w2.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
}

func TestIdempotency_InFlightReturns409(t *testing.T) {
	deps := newStubIdemDeps()
	// Pre-populate an in-flight row (response_code nil).
	deps.rows[stubKey("api-1", "k1")] = &db.PublicIdempotencyKey{
		APIKeyID:    "api-1",
		Key:         "k1",
		RequestHash: "deadbeef",
	}

	called := false
	h := PublicIdempotencyMiddleware(deps)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, idemReq("api-1", "k1", `{"a":1}`))

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if called {
		t.Error("handler ran while in-flight — middleware did not short-circuit")
	}
	var p Problem
	if err := json.NewDecoder(w.Body).Decode(&p); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if p.Code != "idempotency_conflict" {
		t.Errorf("code = %q, want idempotency_conflict", p.Code)
	}
}

func TestIdempotency_ReplaysOperationIdHeader(t *testing.T) {
	deps := newStubIdemDeps()

	// First handler emits Operation-Id; should be captured + replayed.
	h := PublicIdempotencyMiddleware(deps)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Operation-Id", "op_abc")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"op":"op_abc"}`))
	}))

	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, idemReq("api-1", "k1", `{}`))
	if w1.Header().Get("Operation-Id") != "op_abc" {
		t.Fatalf("first call Operation-Id = %q", w1.Header().Get("Operation-Id"))
	}

	// Replay must set the SAME Operation-Id even though a fresh writer is used.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, idemReq("api-1", "k1", `{}`))
	if got := w2.Header().Get("Operation-Id"); got != "op_abc" {
		t.Errorf("replay Operation-Id = %q, want op_abc", got)
	}
	if w2.Code != http.StatusAccepted {
		t.Errorf("replay status = %d, want 202", w2.Code)
	}
	if !bytes.Equal(w2.Body.Bytes(), []byte(`{"op":"op_abc"}`)) {
		t.Errorf("replay body byte-mismatch")
	}
}
