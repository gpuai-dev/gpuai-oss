package tunnel

import (
	"strings"
	"testing"
)

func transportTestData() BootstrapData {
	return BootstrapData{
		InstanceID:        "gpu-abcd1234",
		ProxyHost:         "frp.example.com",
		FRPServerPort:     7000,
		FRPToken:          "frp-token-value",
		RemotePort:        10042,
		SSHAuthorizedKeys: "ssh-ed25519 AAAAC3Nz test@host",
		InternalToken:     "internal-token",
		Hostname:          "gpu-abcd1234.example.com",
		CallbackURL:       "https://api.example.com/internal/instances/gpu-abcd1234/ready",
	}
}

// TestFRPTransportDefaultRendersNothing is the blast-radius guard for every
// provider that does NOT opt in. The frpc config block must come out exactly
// as it did before the knob existed -- same bytes, no stray blank line -- so
// adding this field cannot perturb any existing provider's boot.
func TestFRPTransportDefaultRendersNothing(t *testing.T) {
	got, err := RenderBootstrap(transportTestData())
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}

	if strings.Contains(got, "transport.protocol") {
		t.Error("default render emitted a transport.protocol line; it must render nothing")
	}

	// Byte-exact: auth.token is followed by ONE blank line and then the first
	// proxy block. An extra newline here is the classic text/template
	// whitespace bug and would make this a non-identical render.
	const want = "auth.token = \"frp-token-value\"\n\n[[proxies]]"
	if !strings.Contains(got, want) {
		i := strings.Index(got, "auth.token")
		t.Errorf("frpc config block changed shape.\n got: %q\nwant contains: %q", got[i:i+60], want)
	}
}

// TestFRPTransportRendersWhenSet pins the opted-in shape: the protocol line
// sits inside the [common]-equivalent top-level block, i.e. AFTER auth.token
// and BEFORE the first [[proxies]] table. A TOML key that lands after a table
// header silently belongs to that table instead, which would make frpc ignore
// it -- the exact failure that looks like "websocket didn't help".
func TestFRPTransportRendersWhenSet(t *testing.T) {
	for _, proto := range []string{"websocket", "wss", "kcp", "quic"} {
		t.Run(proto, func(t *testing.T) {
			d := transportTestData()
			d.FRPTransport = proto

			got, err := RenderBootstrap(d)
			if err != nil {
				t.Fatalf("RenderBootstrap: %v", err)
			}

			line := "transport.protocol = \"" + proto + "\""
			if !strings.Contains(got, line) {
				t.Fatalf("render missing %q", line)
			}

			idxTransport := strings.Index(got, line)
			idxProxies := strings.Index(got, "[[proxies]]")
			idxToken := strings.Index(got, "auth.token")
			if idxTransport < idxToken {
				t.Error("transport.protocol rendered before auth.token")
			}
			if idxProxies > 0 && idxTransport > idxProxies {
				t.Error("transport.protocol rendered after [[proxies]]; it would bind to the proxy table, not the client config")
			}
		})
	}
}

// TestFRPTransportValidationRejectsUnknown is the second gate on a value
// that gets interpolated into a TOML file. Config rejects a bad
// FRP_TRANSPORT_<NAME> at boot; this makes sure no other caller can route a
// bogus or injected transport into a rendered instance config.
func TestFRPTransportValidationRejectsUnknown(t *testing.T) {
	cases := []string{
		"http2",
		"TCP", // only the normalized lowercase empty/known set is accepted
		"websocket\"\nadminAddr = \"0.0.0.0",
		"; rm -rf /",
	}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			d := transportTestData()
			d.FRPTransport = bad

			if err := ValidateBootstrapData(d); err == nil {
				t.Errorf("ValidateBootstrapData accepted FRPTransport %q; want rejection", bad)
			}
			if _, err := RenderBootstrap(d); err == nil {
				t.Errorf("RenderBootstrap accepted FRPTransport %q; want rejection", bad)
			}
		})
	}
}
