package config

import (
	"os"
	"testing"
	"time"
)

func loadWithPullStall(t *testing.T, raw string, set bool) *Config {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")
	if set {
		t.Setenv("PROVISION_PULL_STALL_AFTER", raw)
	} else {
		t.Setenv("PROVISION_PULL_STALL_AFTER", "")
		os.Unsetenv("PROVISION_PULL_STALL_AFTER")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	return cfg
}

// The dead-pull window defaults to 8 minutes: 5 killed healthy marketplace boots
// still unpacking a large image (see DefaultProvisionPullStallAfter).
func TestLoad_ProvisionPullStallAfter(t *testing.T) {
	if got := loadWithPullStall(t, "", false).ProvisionPullStallAfter; got != 8*time.Minute {
		t.Errorf("default = %s; want 8m", got)
	}
	if got := loadWithPullStall(t, "5m", true).ProvisionPullStallAfter; got != 5*time.Minute {
		t.Errorf("PROVISION_PULL_STALL_AFTER=5m gave %s", got)
	}
	if got := loadWithPullStall(t, "0", true).ProvisionPullStallAfter; got != 0 {
		t.Errorf("PROVISION_PULL_STALL_AFTER=0 must disable the leg, got %s", got)
	}
	if got := loadWithPullStall(t, "soon", true).ProvisionPullStallAfter; got != DefaultProvisionPullStallAfter {
		t.Errorf("a garbage value must fall back to the default, got %s", got)
	}
}
