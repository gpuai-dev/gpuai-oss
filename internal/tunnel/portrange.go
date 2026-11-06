package tunnel

import (
	"fmt"
	"strconv"
	"strings"
)

// PortRange is the inclusive span of FRP remote ports the allocator may hand
// out. It exists because the span used to be a pair of Go CONSTANTS while
// looking, from the outside, like configuration.
//
// FRP_ALLOW_PORTS configured only the frps gateway's allow-list (and any
// firewall rule an operator derived from it).
// The allocator in ports.go walked MinPort/MaxPort instead. Widening capacity by
// setting the env var therefore did NOTHING — and widening the constants without
// the env var would have been worse: the allocator would hand out a port the
// gateway refuses and the security group drops, so the tunnel never establishes
// and the customer gets an instance they cannot reach.
//
// That trap was live when prod exhausted its 256 ports on 2026-09-12 (3,980
// failed launches in 92 minutes, peak demand 262). Making the allocator read the
// SAME configured string the gateway does means the two can no longer disagree,
// and an environment can widen on its own schedule: demo can run 1024 ports
// while prod is still on 256, with no code change in between.
type PortRange struct {
	Min int
	Max int
}

// DefaultPortRange is the span assumed when nothing is configured. It matches
// the historical MinPort/MaxPort constants, so an environment that sets no
// FRP_ALLOW_PORTS behaves exactly as it did before.
func DefaultPortRange() PortRange { return PortRange{Min: MinPort, Max: MaxPort} }

// IsZero reports whether the range was never set, so callers can fall back to
// DefaultPortRange rather than allocating out of a [0,0] span.
func (r PortRange) IsZero() bool { return r.Min == 0 && r.Max == 0 }

// OrDefault returns r, or DefaultPortRange when r was never populated.
func (r PortRange) OrDefault() PortRange {
	if r.IsZero() {
		return DefaultPortRange()
	}
	return r
}

// Size is the number of ports in the span, and therefore the maximum number of
// instances that can hold a tunnel concurrently.
func (r PortRange) Size() int { return r.Max - r.Min + 1 }

// Contains reports whether port falls inside the span.
func (r PortRange) Contains(port int) bool { return port >= r.Min && port <= r.Max }

// String renders the span in the same "low-high" form the config uses, so a log line can be compared to the configured value by eye.
func (r PortRange) String() string { return strconv.Itoa(r.Min) + "-" + strconv.Itoa(r.Max) }

// ParsePortRange parses the "low-high" form used by FRP_ALLOW_PORTS
// (e.g. "10000-11023").
//
// An empty string yields DefaultPortRange rather than an error: an environment
// that has never set the variable must keep working, and that is precisely the
// deployment where a hard failure would be most surprising.
func ParsePortRange(s string) (PortRange, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultPortRange(), nil
	}
	low, high, ok := strings.Cut(s, "-")
	if !ok {
		return PortRange{}, fmt.Errorf("tunnel: port range %q must be in low-high form", s)
	}
	min, err := strconv.Atoi(strings.TrimSpace(low))
	if err != nil {
		return PortRange{}, fmt.Errorf("tunnel: port range %q: bad low bound: %w", s, err)
	}
	max, err := strconv.Atoi(strings.TrimSpace(high))
	if err != nil {
		return PortRange{}, fmt.Errorf("tunnel: port range %q: bad high bound: %w", s, err)
	}
	if min < 1 || max > 65535 {
		return PortRange{}, fmt.Errorf("tunnel: port range %q outside 1-65535", s)
	}
	if min > max {
		return PortRange{}, fmt.Errorf("tunnel: port range %q has low bound above high bound", s)
	}
	return PortRange{Min: min, Max: max}, nil
}
