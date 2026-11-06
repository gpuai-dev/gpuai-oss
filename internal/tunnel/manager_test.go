package tunnel

import (
	"net"
	"strconv"
	"testing"
)

// ephemeralPort finds an available ephemeral port for testing.
func ephemeralPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find ephemeral port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func TestNewManager(t *testing.T) {
	port := ephemeralPort(t)
	mgr, err := NewManager(port, "test-token-abc123", "10000-10255", nil)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	if mgr == nil {
		t.Fatal("NewManager() returned nil manager")
		return
	}
	if mgr.frpSvc == nil {
		t.Error("NewManager() frpSvc should not be nil")
	}
	// Clean up
	_ = mgr.Close()
}

func TestNewManager_InvalidPort(t *testing.T) {
	tests := []struct {
		name string
		port int
	}{
		{"zero port", 0},
		{"negative port", -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewManager(tt.port, "test-token", "10000-10255", nil)
			if err == nil {
				t.Error("NewManager() with invalid port should return error, got nil")
			}
		})
	}
}

func TestNewManager_EmptyToken(t *testing.T) {
	// Empty token should still create a manager (token is optional for dev)
	port := ephemeralPort(t)
	mgr, err := NewManager(port, "", "10000-10255", nil)
	if err != nil {
		t.Fatalf("NewManager() with empty token error: %v", err)
	}
	if mgr == nil {
		t.Fatal("NewManager() returned nil manager")
		return
	}
	_ = mgr.Close()
}

func TestNewManager_PortString(t *testing.T) {
	// Verify that the manager can handle various port numbers
	port := ephemeralPort(t)
	mgr, err := NewManager(port, "test-token", strconv.Itoa(MinPort)+"-"+strconv.Itoa(MaxPort), nil)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	_ = mgr.Close()
}
