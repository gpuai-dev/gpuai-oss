package config

import (
	"strings"
	"testing"
)

func TestParseGenericSuppliers(t *testing.T) {
	got, err := ParseGenericSuppliers(`[
		{"slug":"Mock","base_url":"http://mock-supplier:8080/","api_key":"test-key-not-secret","regions":["us-east"]},
		{"slug":"acme-gpu","name":"Acme GPU","base_url":"https://supplier.example.com"}
	]`)
	if err != nil {
		t.Fatalf("ParseGenericSuppliers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d suppliers, want 2", len(got))
	}
	if got[0].Slug != "mock" || got[0].BaseURL != "http://mock-supplier:8080" || got[0].Name != "mock" {
		t.Errorf("supplier 0 not normalized: %+v", got[0])
	}
	if got[0].APIKey != "test-key-not-secret" || len(got[0].Regions) != 1 {
		t.Errorf("supplier 0 lost fields: %+v", got[0])
	}
	if got[1].Name != "Acme GPU" {
		t.Errorf("explicit name not kept: %+v", got[1])
	}

	if none, err := ParseGenericSuppliers("  "); err != nil || none != nil {
		t.Fatalf("empty value must yield no suppliers, got %v, %v", none, err)
	}
}

func TestParseGenericSuppliers_Invalid(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":       `{"slug":"x"}`,
		"bad slug":       `[{"slug":"Not A Slug!","base_url":"http://x"}]`,
		"duplicate slug": `[{"slug":"aa","base_url":"http://x"},{"slug":"aa","base_url":"http://y"}]`,
		"bad url":        `[{"slug":"aa","base_url":"ftp://x"}]`,
		"missing url":    `[{"slug":"aa"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseGenericSuppliers(raw); err == nil {
				t.Fatalf("expected an error for %s", raw)
			}
		})
	}
}

func TestLoad_GenericSuppliersAndDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")
	t.Setenv("GENERIC_SUPPLIERS", `[{"slug":"mock","base_url":"http://mock-supplier:8080"}]`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.GenericSuppliers) != 1 || cfg.GenericSuppliers[0].Slug != "mock" {
		t.Fatalf("GenericSuppliers = %+v", cfg.GenericSuppliers)
	}
	// Self-host defaults: prices pass through, migrations run, no tunnel.
	if cfg.PricingMarkupPct != 0 {
		t.Errorf("default markup = %v, want 0 (pass-through)", cfg.PricingMarkupPct)
	}
	if !cfg.AutoMigrate {
		t.Error("AutoMigrate must default to true")
	}
	if cfg.FRPToken != "" {
		t.Error("the FRP tunnel must be off by default")
	}
	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want 8080", cfg.Port)
	}
}

func TestLoad_GenericSuppliersInvalidFailsFast(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")
	t.Setenv("GENERIC_SUPPLIERS", `[{"slug":"mock"}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "GENERIC_SUPPLIERS") {
		t.Fatalf("expected a GENERIC_SUPPLIERS error, got %v", err)
	}
}

func TestLoad_MarkupPct(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")
	t.Setenv("GPUCTL_MARKUP_PCT", "12.5")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.MarkupPctFor("generic"); got != 12.5 {
		t.Errorf("MarkupPctFor(generic) = %v, want 12.5", got)
	}

	t.Setenv("GPUCTL_MARKUP_PCT", "lots")
	if _, err := Load(); err == nil {
		t.Fatal("a non-numeric GPUCTL_MARKUP_PCT must fail fast")
	}
}

func TestLoad_RequiresDatabaseAndRedis(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("REDIS_URL", "")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") || !strings.Contains(err.Error(), "REDIS_URL") {
		t.Fatalf("expected missing DATABASE_URL and REDIS_URL, got %v", err)
	}
}
