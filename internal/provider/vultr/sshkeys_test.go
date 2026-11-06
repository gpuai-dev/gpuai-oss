package vultr

import (
	"strings"
	"testing"
)

func TestSSHKeysCloudConfig(t *testing.T) {
	got := sshKeysCloudConfig([]string{"ssh-ed25519 AAA me@host\n", "ssh-rsa BBB\n  - injected"})
	if !strings.HasPrefix(got, "#cloud-config\n") {
		t.Fatalf("cloud-init only reads user data with the header: %q", got)
	}
	if !strings.Contains(got, `- "ssh-ed25519 AAA me@host"`) {
		t.Fatalf("key missing or not trimmed: %q", got)
	}
	// A newline inside a key must stay inside its quoted string.
	if n := strings.Count(got, "\n      - "); n != 2 {
		t.Fatalf("got %d key entries, want 2: %q", n, got)
	}
}
