package public

import (
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// Direct-connect mode (no FRP tunnel): a running instance's connection block
// points at the supplier-reported address and mapped SSH port, never at a
// tunnel host, and is absent until the instance is running.
func TestInstanceToPublic_DirectConnect(t *testing.T) {
	ip := "192.0.2.10"
	port := 40022
	mk := func(status string, sshPort *int) *db.Instance {
		return &db.Instance{InstanceID: "gpu-d1", Status: status, UpstreamIP: &ip, SSHPort: sshPort}
	}

	out := instanceToPublic(mk("running", &port), "", "", false)
	if out.Connection == nil {
		t.Fatal("running direct-connect instance must carry connection info")
	}
	if out.Connection.Hostname != ip || out.Connection.Port != port {
		t.Fatalf("connection = %+v; want %s:%d", out.Connection, ip, port)
	}
	want := "ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p 40022 root@192.0.2.10"
	if out.Connection.SSHCommand != want {
		t.Fatalf("ssh_command = %q; want %q", out.Connection.SSHCommand, want)
	}

	if out := instanceToPublic(mk("running", nil), "", "", false); out.Connection == nil || out.Connection.Port != 22 {
		t.Fatalf("no recorded ssh port must default to 22, got %+v", out.Connection)
	}
	if out := instanceToPublic(mk("booting", &port), "", "", false); out.Connection != nil {
		t.Fatalf("a booting instance must not expose connection info, got %+v", out.Connection)
	}
}
