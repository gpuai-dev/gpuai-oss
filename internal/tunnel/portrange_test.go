package tunnel

import (
	"context"
	"strings"
	"testing"
)

// TestDefaultPortRangeMatchesConfigDefault pins the two halves that used to be
// able to drift apart silently. internal/config defaults FRP_ALLOW_PORTS to
// this literal and the Terraform module variable frp_tunnel_port_range defaults
// to the same one; if either moves without the constants, an environment that
// sets nothing would allocate outside the span its gateway allows.
func TestDefaultPortRangeMatchesConfigDefault(t *testing.T) {
	const configDefault = "10000-10255"
	if got := DefaultPortRange().String(); got != configDefault {
		t.Errorf("DefaultPortRange() = %s, want %s — internal/config's FRP_ALLOW_PORTS default and the frp-gateway Terraform default must agree with MinPort/MaxPort", got, configDefault)
	}
	parsed, err := ParsePortRange(configDefault)
	if err != nil {
		t.Fatalf("ParsePortRange(%q): %v", configDefault, err)
	}
	if parsed != DefaultPortRange() {
		t.Errorf("ParsePortRange(%q) = %+v, want %+v", configDefault, parsed, DefaultPortRange())
	}
}

func TestParsePortRange(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    PortRange
		wantErr bool
	}{
		{"widened demo range", "10000-11023", PortRange{10000, 11023}, false},
		{"historical range", "10000-10255", PortRange{10000, 10255}, false},
		{"surrounding whitespace", "  10000-10255 ", PortRange{10000, 10255}, false},
		{"single port", "10000-10000", PortRange{10000, 10000}, false},
		// Empty must not error: an environment that never set the variable has
		// to keep booting, and that is exactly where a hard failure would be
		// most surprising.
		{"empty falls back to default", "", DefaultPortRange(), false},
		{"no separator", "10000", PortRange{}, true},
		{"non-numeric low", "abc-10255", PortRange{}, true},
		{"non-numeric high", "10000-xyz", PortRange{}, true},
		{"inverted", "11023-10000", PortRange{}, true},
		{"above 65535", "10000-70000", PortRange{}, true},
		{"zero low", "0-10255", PortRange{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePortRange(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParsePortRange(%q) = %+v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePortRange(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParsePortRange(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestPortRangeSizeAndContains(t *testing.T) {
	r := PortRange{Min: 10000, Max: 11023}
	if r.Size() != 1024 {
		t.Errorf("Size() = %d, want 1024", r.Size())
	}
	if DefaultPortRange().Size() != 256 {
		t.Errorf("default Size() = %d, want 256 — the span prod exhausted on 2026-09-12", DefaultPortRange().Size())
	}
	for _, p := range []int{10000, 10500, 11023} {
		if !r.Contains(p) {
			t.Errorf("Contains(%d) = false, want true", p)
		}
	}
	for _, p := range []int{9999, 11024} {
		if r.Contains(p) {
			t.Errorf("Contains(%d) = true, want false", p)
		}
	}
}

func TestPortRangeOrDefault(t *testing.T) {
	if got := (PortRange{}).OrDefault(); got != DefaultPortRange() {
		t.Errorf("zero.OrDefault() = %+v, want %+v", got, DefaultPortRange())
	}
	set := PortRange{Min: 20000, Max: 20010}
	if got := set.OrDefault(); got != set {
		t.Errorf("set.OrDefault() = %+v, want %+v", got, set)
	}
}

// TestAllocatePortHonorsWidenedRange is the point of the whole change: a
// widened configuration must actually widen what the allocator hands out. Under
// the old constants this port was unreachable no matter what FRP_ALLOW_PORTS
// said, so the env var looked like a capacity knob and was not one.
func TestAllocatePortHonorsWidenedRange(t *testing.T) {
	widened := PortRange{Min: 10000, Max: 11023}

	// Every port in the OLD 256 span is taken; only the widened tail is free.
	active := map[int]bool{}
	for p := 10000; p <= 10255; p++ {
		active[p] = true
	}
	tx := &mockTx{activePorts: active}

	port, err := AllocatePort(context.Background(), tx, widened)
	if err != nil {
		t.Fatalf("AllocatePort with widened range: %v — a widened config must not report exhaustion while ports remain", err)
	}
	if port <= 10255 {
		t.Errorf("AllocatePort() = %d, want a port above the old 10255 ceiling", port)
	}
	if !widened.Contains(port) {
		t.Errorf("AllocatePort() = %d, outside the configured span %s", port, widened)
	}
}

// TestAllocatePortExhaustionNamesConfiguredRange keeps the operator-facing error
// honest: it must name the span that actually ran out, not the constants.
func TestAllocatePortExhaustionNamesConfiguredRange(t *testing.T) {
	narrow := PortRange{Min: 20000, Max: 20002}
	active := map[int]bool{20000: true, 20001: true, 20002: true}
	tx := &mockTx{activePorts: active}

	_, err := AllocatePort(context.Background(), tx, narrow)
	if err == nil {
		t.Fatal("want an exhaustion error when every port in the span is held")
	}
	if !strings.Contains(err.Error(), narrow.String()) {
		t.Errorf("error %q does not name the configured span %s", err, narrow)
	}
}

// TestAllocatePortZeroRangeFallsBackToDefault — a caller that has not been
// taught about the configurable range must keep allocating inside the historical
// span rather than out of a [0,0] window.
func TestAllocatePortZeroRangeFallsBackToDefault(t *testing.T) {
	tx := &mockTx{activePorts: map[int]bool{}}
	port, err := AllocatePort(context.Background(), tx, PortRange{})
	if err != nil {
		t.Fatalf("AllocatePort with zero range: %v", err)
	}
	if !DefaultPortRange().Contains(port) {
		t.Errorf("AllocatePort() = %d, outside the default span %s", port, DefaultPortRange())
	}
}
