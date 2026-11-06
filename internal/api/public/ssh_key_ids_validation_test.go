package public

import (
	"strings"
	"testing"
)

// 2026-09-27: one org's 32 creates failed with a raw SQL error from the key
// lookup because ssh_key_ids carried "KEYID=..." strings. Malformed ids are
// now a clear 400 before anything reaches the database.
func TestCreateInstanceValidate_SSHKeyIDs(t *testing.T) {
	base := func(ids ...string) *CreateInstanceRequest {
		return &CreateInstanceRequest{GPUType: "rtx_4090", GPUCount: 1, Tier: "on_demand", SSHKeyIDs: ids}
	}
	if err := base().Validate(); err != nil {
		t.Errorf("no ids: %v", err)
	}
	if err := base("11111111-2222-4333-8444-555555555555").Validate(); err != nil {
		t.Errorf("a key id was refused: %v", err)
	}
	for _, bad := range []string{"KEYID=11111111-2222-4333-8444-555555555555", "ssh-ed25519 AAAAC3Nza", "my-laptop", ""} {
		err := base("11111111-2222-4333-8444-555555555555", bad).Validate()
		if err == nil || !strings.Contains(err.Error(), "ssh_key_ids") {
			t.Errorf("%q: err %v; want an ssh_key_ids validation error", bad, err)
		}
	}
	long := "ssh-ed25519 " + strings.Repeat("A", 400)
	if err := base(long).Validate(); err == nil || len(err.Error()) > 140 {
		t.Errorf("a pasted public key must be refused without echoing it whole: %v", err)
	}
}
