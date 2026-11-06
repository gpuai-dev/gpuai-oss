package public

import "testing"

// TestValidateEnvironmentValue_Grammar pins the environment format grammar,
// including the launch version picker's `certified:<fw>@<ver>` addition. Kept in
// the internal test package so it can exercise the unexported validator directly.
func TestValidateEnvironmentValue_Grammar(t *testing.T) {
	valid := []string{
		"certified", "raw_vm", "provider_template",
		"certified:pytorch", "certified:cuda-devel",
		"certified:pytorch@2.13", "certified:pytorch@2.11", "certified:jupyter@1.0.0",
		"raw_vm:ubuntu-24.04", "raw_vm:ubuntu-22.04-cuda",
	}
	for _, v := range valid {
		if err := validateEnvironmentValue(v); err != nil {
			t.Errorf("validateEnvironmentValue(%q) = %v; want nil", v, err)
		}
	}
	invalid := []string{
		"", "unknown", "certified:", "certified:PyTorch", "certified:pytorch@",
		"certified:pytorch@-2.13", "certified:-bad@1.0", "certified:pytorch@2.13@extra",
		"raw_vm:", "raw_vm:Ubuntu", "provider_template:x",
	}
	for _, v := range invalid {
		if err := validateEnvironmentValue(v); err == nil {
			t.Errorf("validateEnvironmentValue(%q) = nil; want error", v)
		}
	}
}
