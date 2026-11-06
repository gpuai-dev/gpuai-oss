package provision

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"time"
)

const sshReadyProbeTimeout = 3 * time.Second

// ProbeSSHBanner reports whether ip:port answers with an SSH protocol banner.
// An open port alone is not enough: a supplier's proxy can accept connections
// long before sshd is up behind it.
//
// A loopback address always passes. It only means something on the supplier's
// own host, so it cannot be checked from here (gpuctl usually runs in its own
// container), and the local mock supplier waits for sshd itself.
func ProbeSSHBanner(ctx context.Context, ip string, port int) bool {
	if parsed := net.ParseIP(ip); parsed != nil && parsed.IsLoopback() {
		return true
	}
	return probeBanner(ctx, ip, port)
}

func probeBanner(ctx context.Context, ip string, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, sshReadyProbeTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	return err == nil && strings.HasPrefix(line, "SSH-")
}
