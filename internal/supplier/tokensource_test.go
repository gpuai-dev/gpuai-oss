package supplier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func testSupplier(baseURL string) Supplier {
	return Supplier{
		SupplierID: "sup-1",
		Slug:       "acme-gpu",
		Name:       "Test Supplier",
		BaseURL:    baseURL,
		Status:     StatusActive,
	}
}

// newTokenServer returns an httptest server implementing a client_credentials
// token endpoint, plus a counter of token requests served.
func newTokenServer(t *testing.T, wantID, wantSecret string, expiresIn int, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %q", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.FormValue("grant_type"); got != "client_credentials" {
			t.Errorf("grant_type = %q, want client_credentials", got)
		}
		if r.FormValue("client_id") != wantID || r.FormValue("client_secret") != wantSecret {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok-abc",
			"token_type":   "Bearer",
			"expires_in":   expiresIn,
		})
	}))
}

func TestTokenSource_FetchesAndCaches(t *testing.T) {
	var calls atomic.Int64
	srv := newTokenServer(t, "id-1", "sec-1", 3600, &calls)
	defer srv.Close()

	ts := NewTokenSource(map[string]OutboundCred{
		"acme-gpu": {ClientID: "id-1", ClientSecret: "sec-1"},
	}, nil)

	sup := testSupplier(srv.URL)
	for i := 0; i < 3; i++ {
		token, err := ts.Token(context.Background(), sup)
		if err != nil {
			t.Fatalf("Token() call %d: %v", i, err)
		}
		if token != "tok-abc" {
			t.Fatalf("token = %q, want tok-abc", token)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("token endpoint hit %d times, want 1 (cache miss only on first call)", got)
	}
}

func TestTokenSource_RefreshesNearExpiry(t *testing.T) {
	var calls atomic.Int64
	srv := newTokenServer(t, "id-1", "sec-1", 3600, &calls)
	defer srv.Close()

	now := time.Now()
	clock := &now
	ts := NewTokenSource(map[string]OutboundCred{
		"acme-gpu": {ClientID: "id-1", ClientSecret: "sec-1"},
	}, nil, WithClock(func() time.Time { return *clock }))

	sup := testSupplier(srv.URL)
	if _, err := ts.Token(context.Background(), sup); err != nil {
		t.Fatal(err)
	}
	// Advance past expires_in minus the safety margin: must refetch.
	later := now.Add(3600*time.Second - 30*time.Second)
	clock = &later
	if _, err := ts.Token(context.Background(), sup); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("token endpoint hit %d times, want 2 (refresh near expiry)", got)
	}
}

func TestTokenSource_ShortTTLNeverCached(t *testing.T) {
	var calls atomic.Int64
	// expires_in below the 60s safety margin: every call refetches.
	srv := newTokenServer(t, "id-1", "sec-1", 30, &calls)
	defer srv.Close()

	ts := NewTokenSource(map[string]OutboundCred{
		"acme-gpu": {ClientID: "id-1", ClientSecret: "sec-1"},
	}, nil)

	sup := testSupplier(srv.URL)
	for i := 0; i < 2; i++ {
		if _, err := ts.Token(context.Background(), sup); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("token endpoint hit %d times, want 2 (sub-margin TTL must not cache)", got)
	}
}

func TestTokenSource_NoCredsIsUnauthenticated(t *testing.T) {
	ts := NewTokenSource(map[string]OutboundCred{}, nil)
	token, err := ts.Token(context.Background(), testSupplier("http://unused.invalid"))
	if err != nil {
		t.Fatalf("Token() with no creds: %v", err)
	}
	if token != "" {
		t.Errorf("token = %q, want empty for unconfigured supplier", token)
	}
}

func TestTokenSource_InvalidCredentialsError(t *testing.T) {
	var calls atomic.Int64
	srv := newTokenServer(t, "id-1", "sec-1", 3600, &calls)
	defer srv.Close()

	ts := NewTokenSource(map[string]OutboundCred{
		"acme-gpu": {ClientID: "id-1", ClientSecret: "wrong"},
	}, nil)

	if _, err := ts.Token(context.Background(), testSupplier(srv.URL)); err == nil {
		t.Fatal("Token() with bad secret: want error, got nil")
	}
}

func TestTokenSource_ExplicitTokenURLOverride(t *testing.T) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/custom/token", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-custom", "expires_in": 3600})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := NewTokenSource(map[string]OutboundCred{
		"acme-gpu": {ClientID: "id-1", ClientSecret: "sec-1", TokenURL: srv.URL + "/custom/token"},
	}, nil)

	// BaseURL points nowhere routable — only the override URL can succeed.
	token, err := ts.Token(context.Background(), testSupplier("http://unused.invalid"))
	if err != nil {
		t.Fatal(err)
	}
	if token != "tok-custom" {
		t.Errorf("token = %q, want tok-custom", token)
	}
	if calls.Load() != 1 {
		t.Errorf("custom endpoint hit %d times, want 1", calls.Load())
	}
}

func TestTokenSource_MissingAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer"})
	}))
	defer srv.Close()

	ts := NewTokenSource(map[string]OutboundCred{
		"acme-gpu": {ClientID: "id-1", ClientSecret: "sec-1"},
	}, nil)

	if _, err := ts.Token(context.Background(), testSupplier(srv.URL)); err == nil {
		t.Fatal("Token() with empty access_token: want error, got nil")
	}
}
