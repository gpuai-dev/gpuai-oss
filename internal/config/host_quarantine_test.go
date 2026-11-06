package config

import (
	"os"
	"testing"
)

func TestLoad_ProvisionHostQuarantine(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")
	t.Setenv("PROVISION_HOST_QUARANTINE", "")
	os.Unsetenv("PROVISION_HOST_QUARANTINE")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ProvisionHostQuarantine {
		t.Error("host quarantine must default on")
	}
	// A kill switch reached for mid-incident: every usual "off" works, any case.
	for _, off := range []string{"off", "OFF", " Off ", "false", "FALSE", "0", "no", "No"} {
		t.Setenv("PROVISION_HOST_QUARANTINE", off)
		if cfg, _ = Load(); cfg.ProvisionHostQuarantine {
			t.Errorf("PROVISION_HOST_QUARANTINE=%q must disable it", off)
		}
	}
	// Anything else, a typo included, keeps the default on.
	for _, on := range []string{"on", "true", "1", "YES", "disable"} {
		t.Setenv("PROVISION_HOST_QUARANTINE", on)
		if cfg, _ = Load(); !cfg.ProvisionHostQuarantine {
			t.Errorf("PROVISION_HOST_QUARANTINE=%q must leave it on", on)
		}
	}
}
