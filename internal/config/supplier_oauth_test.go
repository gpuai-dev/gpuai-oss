package config

import (
	"strings"
	"testing"
)

// setRequiredEnv sets the always-required vars so Load() reaches the
// supplier OAuth validation instead of bailing earlier.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")
}

func TestLoad_SupplierOAuthComplete(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SUPPLIER_OAUTH_CLIENT_ID_ACME_GPU", "id-123")
	t.Setenv("SUPPLIER_OAUTH_CLIENT_SECRET_ACME_GPU", "sec-456")
	t.Setenv("SUPPLIER_OAUTH_TOKEN_URL_ACME_GPU", "https://auth.acme-gpu.example/oauth/token")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	cred, ok := cfg.SupplierOAuth["acme-gpu"]
	if !ok {
		t.Fatalf("SupplierOAuth missing acme-gpu entry (ACME_GPU env suffix maps to the dashed slug): %+v", cfg.SupplierOAuth)
	}
	if cred.ClientID != "id-123" || cred.ClientSecret != "sec-456" || cred.TokenURL != "https://auth.acme-gpu.example/oauth/token" {
		t.Errorf("acme-gpu cred = %+v, want id-123/sec-456/token URL", cred)
	}
}

func TestLoad_SupplierOAuthPartialFailsFast(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SUPPLIER_OAUTH_CLIENT_ID_ACME_GPU", "id-123")
	// No matching CLIENT_SECRET.

	_, err := Load()
	if err == nil {
		t.Fatal("Load() with partial supplier OAuth config: want error, got nil")
	}
	if !strings.Contains(err.Error(), "acme-gpu") {
		t.Errorf("error %q does not name the misconfigured supplier", err)
	}
}

func TestLoad_SupplierOAuthTokenURLWithoutCredsFailsFast(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SUPPLIER_OAUTH_TOKEN_URL_ACME_GPU", "https://auth.acme-gpu.example/oauth/token")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() with token URL but no credentials: want error, got nil")
	}
}

func TestLoad_SupplierOAuthAbsent(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.SupplierOAuth) != 0 {
		t.Errorf("SupplierOAuth = %+v, want empty when no SUPPLIER_OAUTH_* vars set", cfg.SupplierOAuth)
	}
}
