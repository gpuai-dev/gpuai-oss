package provision

import (
	"context"
	"net"
	"strconv"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// listen serves greeting to every connection and returns the port. It binds
// loopback, so tests use probeBanner, which has no loopback bypass.
func listen(t *testing.T, greeting string) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(greeting))
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	n, _ := strconv.Atoi(port)
	return n
}

func TestProbeBanner(t *testing.T) {
	ctx := context.Background()
	if !probeBanner(ctx, "127.0.0.1", listen(t, "SSH-2.0-OpenSSH_9.6\r\n")) {
		t.Fatal("an SSH banner must count as ready")
	}
	if probeBanner(ctx, "127.0.0.1", listen(t, "HTTP/1.1 400 Bad Request\r\n")) {
		t.Fatal("an open port that is not SSH must not count as ready")
	}
	closed := listen(t, "")
	if probeBanner(ctx, "127.0.0.1", closed) {
		t.Fatal("a port that closes without a banner must not count as ready")
	}
}

func TestProbeSSHBanner_LoopbackPasses(t *testing.T) {
	if !ProbeSSHBanner(context.Background(), "127.0.0.1", 1) {
		t.Fatal("a loopback address cannot be probed from gpuctl and must pass")
	}
}

// TestDirectConnect_WaitsForSSH: while the endpoint does not answer the row
// stays booting, and the first tick where it answers flips it exactly once.
func TestDirectConnect_WaitsForSSH(t *testing.T) {
	store := &spyProgressStore{inst: &db.Instance{InstanceID: "gpu-direct3", Status: StateBooting}}
	e := newEngineForTest(store)
	e.directConnect = true
	answering := false
	var probed []string
	e.sshReady = func(_ context.Context, ip string, port int) bool {
		probed = append(probed, net.JoinHostPort(ip, strconv.Itoa(port)))
		return answering
	}
	status := provider.InstanceStatus{
		Status: "running",
		IP:     "192.0.2.10",
		Ports:  []provider.PortMapping{{PrivatePort: 22, PublicPort: 40022, Protocol: "tcp"}},
	}

	e.progressStatusStep(context.Background(), "gpu-direct3", status)
	store.mu.Lock()
	calls := store.setRunningCall
	store.mu.Unlock()
	if calls != 0 {
		t.Fatalf("flipped to running with SSH down: %d calls", calls)
	}

	answering = true
	e.progressStatusStep(context.Background(), "gpu-direct3", status)
	store.mu.Lock()
	calls = store.setRunningCall
	store.mu.Unlock()
	if calls != 1 {
		t.Fatalf("SetInstanceRunning called %d times once SSH answered; want 1", calls)
	}
	if len(probed) != 2 || probed[0] != "192.0.2.10:40022" {
		t.Fatalf("probed = %v; want 192.0.2.10:40022 twice", probed)
	}
}
