package provision

import (
	"context"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// TestNewEngine_DirectConnectWithoutTunnel pins the self-host default: an
// engine built without an FRP tunnel manager runs in direct-connect mode, and
// one built with a tunnel does not.
func TestNewEngine_DirectConnectWithoutTunnel(t *testing.T) {
	if e := NewEngine(EngineDeps{}); !e.directConnect {
		t.Fatal("engine without a tunnel manager must run in direct-connect mode")
	}
}

// TestProgressStatusDirectConnect_FlipsRunning: with no tunnel there is no
// bootstrap to call back, so the supplier reporting running with an address
// is the readiness signal. The step records the SSH port mapped to container
// port 22 and flips the row to running exactly once.
func TestProgressStatusDirectConnect_FlipsRunning(t *testing.T) {
	store := &spyProgressStore{inst: &db.Instance{
		InstanceID: "gpu-direct1",
		Status:     StateBooting,
		GPUType:    "rtx4090",
		GPUCount:   1,
	}}
	e := newEngineForTest(store)
	e.directConnect = true
	var changes []string
	e.onStatusChange = func(_, status string) { changes = append(changes, status) }

	status := provider.InstanceStatus{
		Status: "running",
		IP:     "192.0.2.10",
		Ports:  []provider.PortMapping{{PrivatePort: 22, PublicPort: 40022, Protocol: "tcp"}},
	}
	e.progressStatusStep(context.Background(), "gpu-direct1", status)

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setRunningCall != 1 {
		t.Fatalf("SetInstanceRunning called %d times; want 1", store.setRunningCall)
	}
	if len(store.sshPorts) != 1 || store.sshPorts[0] != 40022 {
		t.Fatalf("ssh ports recorded = %v; want [40022]", store.sshPorts)
	}
	if len(changes) != 1 || changes[0] != StateRunning {
		t.Fatalf("status changes = %v; want [running]", changes)
	}
}

// TestProgressStatusDirectConnect_DefaultsPort22: a supplier that reports no
// port mapping is reached on port 22 at its address.
func TestProgressStatusDirectConnect_DefaultsPort22(t *testing.T) {
	store := &spyProgressStore{inst: &db.Instance{InstanceID: "gpu-direct2", Status: StateBooting}}
	e := newEngineForTest(store)
	e.directConnect = true

	e.progressStatusStep(context.Background(), "gpu-direct2", bootingStatus())

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.sshPorts) != 1 || store.sshPorts[0] != 22 {
		t.Fatalf("ssh ports recorded = %v; want [22]", store.sshPorts)
	}
	if store.setRunningCall != 1 {
		t.Fatalf("SetInstanceRunning called %d times; want 1", store.setRunningCall)
	}
}
