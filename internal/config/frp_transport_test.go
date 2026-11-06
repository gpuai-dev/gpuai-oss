package config

import "testing"

// setRequiredEnv lives in supplier_oauth_test.go — same package, reused here.

// TestFRPTransportParsing covers FRP_TRANSPORT_<NAME>. The value is rendered
// verbatim into an instance's frpc.toml, so an unsupported protocol must fail
// the process at boot rather than reach a customer instance -- and "tcp" must
// normalize away so the explicit default cannot drift from no-override.
func TestFRPTransportParsing(t *testing.T) {
	cases := []struct {
		name     string
		envKey   string
		envValue string
		wantErr  bool
		lookup   string
		want     string
	}{
		{
			name:   "websocket override is accepted and resolves",
			envKey: "FRP_TRANSPORT_ALPHA", envValue: "websocket",
			lookup: "alpha", want: "websocket",
		},
		{
			name:   "wss is accepted",
			envKey: "FRP_TRANSPORT_ALPHA", envValue: "wss",
			lookup: "alpha", want: "wss",
		},
		{
			name:   "value is case-insensitive",
			envKey: "FRP_TRANSPORT_ALPHA", envValue: "WebSocket",
			lookup: "alpha", want: "websocket",
		},
		{
			name:   "provider name is matched case-insensitively",
			envKey: "FRP_TRANSPORT_ALPHA", envValue: "websocket",
			lookup: "Alpha", want: "websocket",
		},
		{
			// The explicit default must render identically to no override at
			// all, so it normalizes to "" rather than the literal "tcp".
			name:   "tcp normalizes to empty",
			envKey: "FRP_TRANSPORT_ALPHA", envValue: "tcp",
			lookup: "alpha", want: "",
		},
		{
			name:   "unset provider resolves to empty",
			envKey: "FRP_TRANSPORT_ALPHA", envValue: "websocket",
			lookup: "bravo", want: "",
		},
		{
			// A typo must be loud. A silently-ignored bad value would look
			// exactly like "the transport swap didn't help".
			name:   "unsupported protocol is rejected at boot",
			envKey: "FRP_TRANSPORT_ALPHA", envValue: "http2",
			wantErr: true,
		},
		{
			// Defence in depth against TOML injection through the env.
			name:   "toml-injection attempt is rejected",
			envKey: "FRP_TRANSPORT_ALPHA", envValue: "websocket\"\nadminAddr = \"0.0.0.0",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(tc.envKey, tc.envValue)

			cfg, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load() error = %v; wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got := cfg.FRPTransportFor(tc.lookup); got != tc.want {
				t.Errorf("FRPTransportFor(%q) = %q; want %q", tc.lookup, got, tc.want)
			}
		})
	}
}

// TestFRPTransportForNilConfig pins the nil-receiver contract: a nil Config
// resolves to the default transport, never a panic.
func TestFRPTransportForNilConfig(t *testing.T) {
	var c *Config
	if got := c.FRPTransportFor("alpha"); got != "" {
		t.Errorf("nil Config FRPTransportFor = %q; want \"\"", got)
	}
}

// TestFRPTransportAbsentByDefault is the blast-radius guard: with no
// FRP_TRANSPORT_* var set, no provider gets an override, so every existing
// provider keeps rendering frp's default raw-TCP config.
func TestFRPTransportAbsentByDefault(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, name := range []string{"alpha", "bravo", "charlie", "generic", "vultr"} {
		if got := cfg.FRPTransportFor(name); got != "" {
			t.Errorf("FRPTransportFor(%q) = %q; want \"\" with no override set", name, got)
		}
	}
}
