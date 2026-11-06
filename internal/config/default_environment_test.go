package config

import (
	"os"
	"testing"
)

// PROVISION_DEFAULT_ENVIRONMENT decides what a launch that names no environment
// boots. Unset means the certified PyTorch image, "off" restores the legacy
// adapter defaults, and anything outside the certified class is refused back
// to legacy rather than defaulting every API launch to a raw VM.
func TestLoad_ProvisionDefaultEnvironment(t *testing.T) {
	for _, tt := range []struct {
		name, val, want string
		set             bool
	}{
		{"unset", "", "certified:pytorch", false},
		{"empty", "", "certified:pytorch", true},
		{"off", "off", "", true},
		{"base", "certified", "certified", true},
		{"pinned version", "certified:pytorch@2.11", "certified:pytorch@2.11", true},
		{"raw vm refused", "raw_vm", "", true},
		{"garbage refused", "pytorch", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://test")
			t.Setenv("REDIS_URL", "redis://test")
			t.Setenv("PROVISION_DEFAULT_ENVIRONMENT", tt.val)
			if !tt.set {
				os.Unsetenv("PROVISION_DEFAULT_ENVIRONMENT") // t.Setenv above restores it
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.ProvisionDefaultEnvironment != tt.want {
				t.Errorf("ProvisionDefaultEnvironment = %q; want %q", cfg.ProvisionDefaultEnvironment, tt.want)
			}
		})
	}
}

func TestLoad_ProvisionDefaultEnvScope(t *testing.T) {
	for _, tt := range []struct{ val, want string }{
		{"", "incompatible"}, {"incompatible", "incompatible"}, {"all", "all"}, {"everything", "incompatible"},
	} {
		t.Run(tt.val, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://test")
			t.Setenv("REDIS_URL", "redis://test")
			t.Setenv("PROVISION_DEFAULT_ENVIRONMENT_SCOPE", tt.val)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.ProvisionDefaultEnvScope != tt.want {
				t.Errorf("scope = %q; want %q", cfg.ProvisionDefaultEnvScope, tt.want)
			}
		})
	}
}
