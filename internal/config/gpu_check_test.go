package config

import (
	"os"
	"testing"
)

// PROVISION_GPU_CHECK selects the startup GPU compute check mode. It defaults
// to shadow so the false-negative rate is measured before anything is blocked,
// and a typo never silently turns enforcement on.
func TestLoad_ProvisionGPUCheck(t *testing.T) {
	for _, tt := range []struct {
		name, val, want string
		set             bool
	}{
		{"unset", "", "shadow", false},
		{"off", "off", "off", true},
		{"shadow", "shadow", "shadow", true},
		{"enforce", "enforce", "enforce", true},
		{"typo falls back to shadow", "enforced", "shadow", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://test")
			t.Setenv("REDIS_URL", "redis://test")
			t.Setenv("PROVISION_GPU_CHECK", tt.val)
			if !tt.set {
				os.Unsetenv("PROVISION_GPU_CHECK") // t.Setenv above restores it
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.ProvisionGPUCheck != tt.want {
				t.Errorf("ProvisionGPUCheck = %q; want %q", cfg.ProvisionGPUCheck, tt.want)
			}
		})
	}
}
