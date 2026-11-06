package vultr

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAccountBalance covers the supplier balance-guard probe and locks Vultr's
// inverted sign convention: deposited credit is a NEGATIVE balance with
// pending_charges accruing positive against it, so runway =
// -balance - pending_charges.
func TestAccountBalance(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		// Live shape (2026-08-14 probe): $60 credit, $3.79 accrued.
		_, _ = w.Write([]byte(`{"account":{"name":"test","email":"x@y.z","balance":-60,"pending_charges":3.79}}`))
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))
	balance, err := c.AccountBalance(context.Background())
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if math.Abs(balance-56.21) > 1e-9 {
		t.Errorf("balance = %v, want 56.21 (-(-60) - 3.79)", balance)
	}
	if gotPath != "/account" {
		t.Errorf("path = %q, want /account", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth = %q, want Bearer test-key", gotAuth)
	}
}

// TestAccountBalance_Overdrawn: a positive Vultr balance means the account
// OWES money — the probe must return it negative so the guard reds.
func TestAccountBalance_Overdrawn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account":{"balance":4.20,"pending_charges":1.30}}`))
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))
	balance, err := c.AccountBalance(context.Background())
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if math.Abs(balance-(-5.50)) > 1e-9 {
		t.Errorf("balance = %v, want -5.50 (owed + accruing)", balance)
	}
}

// TestAccountBalance_AuthError proves a 401 surfaces as an error, never a zero
// balance — the guard must distinguish "can't read the balance" from "broke".
func TestAccountBalance_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Invalid API token.","status":401}`))
	}))
	defer srv.Close()

	c := NewClient("bad-key", WithBaseURL(srv.URL))
	if _, err := c.AccountBalance(context.Background()); err == nil {
		t.Fatal("AccountBalance on 401 returned nil error")
	}
}
