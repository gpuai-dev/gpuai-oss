package config

import (
	"os"
	"testing"
)

// TestParseBoolEnv locks the Plan 25-03 feature-flag helper. Default applies
// when unset; valid bool strings parse; invalid strings log + fall back to
// default so a typo'd feature flag never crashes the binary.
func TestParseBoolEnv(t *testing.T) {
	cases := []struct {
		name       string
		envValue   string
		setEnv     bool // when false, the env var is explicitly unset
		defaultVal bool
		want       bool
	}{
		{name: "unset_default_false", setEnv: false, defaultVal: false, want: false},
		{name: "unset_default_true", setEnv: false, defaultVal: true, want: true},
		{name: "empty_default_false", setEnv: true, envValue: "", defaultVal: false, want: false},
		{name: "empty_default_true", setEnv: true, envValue: "", defaultVal: true, want: true},
		{name: "true_lower", setEnv: true, envValue: "true", defaultVal: false, want: true},
		{name: "true_upper", setEnv: true, envValue: "TRUE", defaultVal: false, want: true},
		{name: "true_mixed", setEnv: true, envValue: "True", defaultVal: false, want: true},
		{name: "true_one", setEnv: true, envValue: "1", defaultVal: false, want: true},
		{name: "true_t", setEnv: true, envValue: "t", defaultVal: false, want: true},
		{name: "false_lower", setEnv: true, envValue: "false", defaultVal: true, want: false},
		{name: "false_upper", setEnv: true, envValue: "FALSE", defaultVal: true, want: false},
		{name: "false_mixed", setEnv: true, envValue: "False", defaultVal: true, want: false},
		{name: "false_zero", setEnv: true, envValue: "0", defaultVal: true, want: false},
		{name: "false_f", setEnv: true, envValue: "f", defaultVal: true, want: false},
		{name: "invalid_garbage_falls_back_false", setEnv: true, envValue: "maybe", defaultVal: false, want: false},
		{name: "invalid_garbage_falls_back_true", setEnv: true, envValue: "yep", defaultVal: true, want: true},
		{name: "invalid_yes_falls_back", setEnv: true, envValue: "yes", defaultVal: false, want: false},
		{name: "invalid_no_falls_back", setEnv: true, envValue: "no", defaultVal: true, want: true},
		{name: "invalid_on_falls_back", setEnv: true, envValue: "on", defaultVal: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const key = "TEST_PARSE_BOOL_ENV_KEY"
			// Always reset state at the top of each sub-test. t.Setenv
			// restores prior value on cleanup; we explicitly unset for the
			// "unset" cases since t.Setenv("") still sets the var to "".
			if tc.setEnv {
				t.Setenv(key, tc.envValue)
			} else {
				// Ensure any leakage from a parallel/earlier test is cleared.
				os.Unsetenv(key)
				t.Cleanup(func() { os.Unsetenv(key) })
			}
			got := parseBoolEnv(key, tc.defaultVal)
			if got != tc.want {
				t.Errorf("parseBoolEnv(%q, %v) = %v; want %v",
					tc.envValue, tc.defaultVal, got, tc.want)
			}
		})
	}
}

// TestParseKillSwitchEnv locks the default-on kill switch parse
// (PROVISION_PLACEMENT_MEMORY): the words an operator types to turn a feature
// off all work, in any case, and anything unrecognized leaves it on.
func TestParseKillSwitchEnv(t *testing.T) {
	cases := []struct {
		envValue string
		setEnv   bool
		want     bool
	}{
		{setEnv: false, want: true},
		{setEnv: true, envValue: "", want: true},
		{setEnv: true, envValue: "off", want: false},
		{setEnv: true, envValue: "OFF", want: false},
		{setEnv: true, envValue: " Off ", want: false},
		{setEnv: true, envValue: "false", want: false},
		{setEnv: true, envValue: "FALSE", want: false},
		{setEnv: true, envValue: "fAlSe", want: false},
		{setEnv: true, envValue: "0", want: false},
		{setEnv: true, envValue: "no", want: false},
		{setEnv: true, envValue: "No", want: false},
		{setEnv: true, envValue: "on", want: true},
		{setEnv: true, envValue: "true", want: true},
		{setEnv: true, envValue: "1", want: true},
		{setEnv: true, envValue: "YES", want: true},
		{setEnv: true, envValue: "disable-please", want: true},
	}
	for _, tc := range cases {
		t.Run("value="+tc.envValue, func(t *testing.T) {
			const key = "TEST_PARSE_KILL_SWITCH_ENV_KEY"
			if tc.setEnv {
				t.Setenv(key, tc.envValue)
			} else {
				os.Unsetenv(key)
				t.Cleanup(func() { os.Unsetenv(key) })
			}
			if got := parseKillSwitchEnv(key); got != tc.want {
				t.Errorf("parseKillSwitchEnv(%q) = %v; want %v", tc.envValue, got, tc.want)
			}
		})
	}
}

// TestLoad_PlacementMemoryKillSwitch: PROVISION_PLACEMENT_MEMORY turns the
// memory off with any of off/false/0/no in any case, through Load. It used to
// match only the exact string "off", so "OFF" or "false" left it running.
func TestLoad_PlacementMemoryKillSwitch(t *testing.T) {
	for value, want := range map[string]bool{"": true, "on": true, "off": false, "OFF": false,
		"false": false, "False": false, "0": false, "no": false} {
		t.Run("value="+value, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("PROVISION_PLACEMENT_MEMORY", value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if cfg.ProvisionPlacementMemory != want {
				t.Errorf("PROVISION_PLACEMENT_MEMORY=%q: ProvisionPlacementMemory = %v; want %v",
					value, cfg.ProvisionPlacementMemory, want)
			}
		})
	}
}

// TestReconcilerEnforceDefault locks the Plan 32-05 reconciler config (D-03):
// RECONCILER_ENFORCE unset -> ReconcilerEnforce=false (dry-run, the safe
// default — the reconciler logs intended ORPHAN/GHOST/STUCK actions without
// terminating anything), RECONCILER_ENFORCE=true -> ReconcilerEnforce=true.
// The interval/window knobs default to 0 so the reconciler's own New()
// defaults (5m / 4h) apply when they are unset.
//
// Always-required vars are set to dummy non-"change-me" values so Load()
// succeeds; GPUCTL_ENV is left unset so the Plan 32-04 fail-closed auth guard
// (which requires CLERK_SECRET_KEY for non-dev envs) does not trip.
func TestReconcilerEnforceDefault(t *testing.T) {
	cases := []struct {
		name        string
		setEnforce  bool
		enforceVal  string
		wantEnforce bool
	}{
		{name: "unset_defaults_false_dryrun", setEnforce: false, wantEnforce: false},
		{name: "true_enables_enforce", setEnforce: true, enforceVal: "true", wantEnforce: true},
		{name: "one_enables_enforce", setEnforce: true, enforceVal: "1", wantEnforce: true},
		{name: "false_stays_dryrun", setEnforce: true, enforceVal: "false", wantEnforce: false},
		// A typo'd value must fall back to the SAFE default (false), never
		// silently enforce — mirrors parseBoolEnv's fail-to-default contract.
		{name: "garbage_falls_back_to_dryrun", setEnforce: true, enforceVal: "maybe", wantEnforce: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://test")
			t.Setenv("REDIS_URL", "redis://test")
			// Leave GPUCTL_ENV unset (dev) so the auth guard does not require Clerk.
			os.Unsetenv("GPUCTL_ENV")
			t.Cleanup(func() { os.Unsetenv("GPUCTL_ENV") })
			os.Unsetenv("CLERK_SECRET_KEY")
			t.Cleanup(func() { os.Unsetenv("CLERK_SECRET_KEY") })

			if tc.setEnforce {
				t.Setenv("RECONCILER_ENFORCE", tc.enforceVal)
			} else {
				os.Unsetenv("RECONCILER_ENFORCE")
				t.Cleanup(func() { os.Unsetenv("RECONCILER_ENFORCE") })
			}
			// Interval/window left unset -> default 0 (reconciler New() applies 5m/4h).
			os.Unsetenv("RECONCILER_INTERVAL_SECONDS")
			os.Unsetenv("RECONCILER_RECENT_TERM_WINDOW_HOURS")
			t.Cleanup(func() {
				os.Unsetenv("RECONCILER_INTERVAL_SECONDS")
				os.Unsetenv("RECONCILER_RECENT_TERM_WINDOW_HOURS")
			})

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.ReconcilerEnforce != tc.wantEnforce {
				t.Errorf("ReconcilerEnforce = %v; want %v", cfg.ReconcilerEnforce, tc.wantEnforce)
			}
			if cfg.ReconcilerIntervalSeconds != 0 {
				t.Errorf("ReconcilerIntervalSeconds default = %d; want 0", cfg.ReconcilerIntervalSeconds)
			}
			if cfg.ReconcilerRecentTermWindowHours != 0 {
				t.Errorf("ReconcilerRecentTermWindowHours default = %d; want 0", cfg.ReconcilerRecentTermWindowHours)
			}
		})
	}
}

// TestReconcilerIntervalWindowParse verifies the optional cadence/lookback
// knobs parse to ints when set (0 when unset -> New() defaults apply).
func TestReconcilerIntervalWindowParse(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")
	os.Unsetenv("GPUCTL_ENV")
	t.Cleanup(func() { os.Unsetenv("GPUCTL_ENV") })
	t.Setenv("RECONCILER_INTERVAL_SECONDS", "120")
	t.Setenv("RECONCILER_RECENT_TERM_WINDOW_HOURS", "6")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.ReconcilerIntervalSeconds != 120 {
		t.Errorf("ReconcilerIntervalSeconds = %d; want 120", cfg.ReconcilerIntervalSeconds)
	}
	if cfg.ReconcilerRecentTermWindowHours != 6 {
		t.Errorf("ReconcilerRecentTermWindowHours = %d; want 6", cfg.ReconcilerRecentTermWindowHours)
	}
}

// TestLoad_PerProviderMarkup verifies PRICING_MARKUP_PCT_<NAME> overrides:
// parsing into ProviderMarkupPct (lowercased keys), MarkupPctFor resolution
// (override beats global, case-insensitive lookup, global fallback for
// providers without an override), and the parse-error path.
func TestLoad_PerProviderMarkup(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")
	t.Setenv("GPUCTL_MARKUP_PCT", "6.0")
	t.Setenv("PRICING_MARKUP_PCT_ALPHA", "12.5")
	t.Setenv("PRICING_MARKUP_PCT_BRAVO", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if got := cfg.MarkupPctFor("alpha"); got != 12.5 {
		t.Errorf("MarkupPctFor(alpha) = %v; want 12.5 (env override)", got)
	}
	if got := cfg.MarkupPctFor("AlPhA"); got != 12.5 {
		t.Errorf("MarkupPctFor(AlPhA) = %v; want 12.5 (case-insensitive)", got)
	}
	if got := cfg.MarkupPctFor("bravo"); got != 0 {
		t.Errorf("MarkupPctFor(bravo) = %v; want 0 (explicit zero override, not global)", got)
	}
	if got := cfg.MarkupPctFor("charlie"); got != 6.0 {
		t.Errorf("MarkupPctFor(charlie) = %v; want 6.0 (global fallback)", got)
	}
}

func TestLoad_PerProviderMarkupInvalid(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")
	t.Setenv("PRICING_MARKUP_PCT_ALPHA", "not-a-number")

	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error; want parse error for PRICING_MARKUP_PCT_ALPHA")
	}
}

// TestMarkupPctFor_NilSafe: a nil *Config resolves to 0% so engine fixtures
// without config don't panic (production always loads a config at boot).
func TestMarkupPctFor_NilSafe(t *testing.T) {
	var c *Config
	if got := c.MarkupPctFor("anything"); got != 0 {
		t.Errorf("nil Config MarkupPctFor = %v; want 0", got)
	}
}

// TestLoad_MinRetailMarkup verifies PRICING_MIN_RETAIL_MARKUP_PCT: the 0
// default when unset (sitewide passthrough — an undeclared 5.0 default is what
// silently billed +5% on prod until 2026-08-12), an explicit override, and the
// nil-safe accessor. 0 is not "no floor": retail still clamps at bare cost
// rounded up to the cent.
func TestLoad_MinRetailMarkup(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if got := cfg.MinRetailMarkupPct(); got != 0 {
		t.Errorf("MinRetailMarkupPct() = %v; want default 0 (passthrough)", got)
	}

	t.Setenv("PRICING_MIN_RETAIL_MARKUP_PCT", "7.5")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() unexpected error with override: %v", err)
	}
	if got := cfg.MinRetailMarkupPct(); got != 7.5 {
		t.Errorf("MinRetailMarkupPct() = %v; want overridden 7.5", got)
	}

	var nilCfg *Config
	if got := nilCfg.MinRetailMarkupPct(); got != 0 {
		t.Errorf("nil Config MinRetailMarkupPct() = %v; want 0", got)
	}
}

// TestLoad_MinRetailMarkupInvalid: a negative floor would sanction publishing
// below cost — exactly the misconfiguration the floor exists to stop — and a
// non-numeric value is a typo; both fail fast at boot.
func TestLoad_MinRetailMarkupInvalid(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")

	t.Setenv("PRICING_MIN_RETAIL_MARKUP_PCT", "-1")
	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error; want rejection of negative PRICING_MIN_RETAIL_MARKUP_PCT")
	}

	t.Setenv("PRICING_MIN_RETAIL_MARKUP_PCT", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error; want parse error for PRICING_MIN_RETAIL_MARKUP_PCT")
	}
}

// TestLoad_InstanceDisk verifies the four instance-disk knobs: their defaults
// when unset, explicit overrides, and the nil-safe accessors. Unlike the markup
// accessors, the disk accessors degrade to their real defaults rather than 0 —
// a nil Config must not resolve to a zero-sized filesystem.
func TestLoad_InstanceDisk(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if got := cfg.DefaultDiskGB(); got != 100 {
		t.Errorf("DefaultDiskGB() = %d; want default 100", got)
	}
	if got := cfg.IncludedDiskGB(); got != 100 {
		t.Errorf("IncludedDiskGB() = %d; want default 100", got)
	}
	if got := cfg.MaxDiskGB(); got != 250 {
		t.Errorf("MaxDiskGB() = %d; want default 250", got)
	}
	if got := cfg.DiskPricePerGBHour(); got != 0 {
		t.Errorf("DiskPricePerGBHour() = %v; want default 0", got)
	}

	t.Setenv("LAUNCH_DEFAULT_DISK_GB", "150")
	t.Setenv("LAUNCH_INCLUDED_DISK_GB", "120")
	t.Setenv("LAUNCH_MAX_DISK_GB", "1000")
	t.Setenv("DISK_PRICE_PER_GB_HOUR", "0.000137")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() unexpected error with overrides: %v", err)
	}
	if got := cfg.DefaultDiskGB(); got != 150 {
		t.Errorf("DefaultDiskGB() = %d; want overridden 150", got)
	}
	if got := cfg.IncludedDiskGB(); got != 120 {
		t.Errorf("IncludedDiskGB() = %d; want overridden 120", got)
	}
	if got := cfg.MaxDiskGB(); got != 1000 {
		t.Errorf("MaxDiskGB() = %d; want overridden 1000", got)
	}
	if got := cfg.DiskPricePerGBHour(); got != 0.000137 {
		t.Errorf("DiskPricePerGBHour() = %v; want overridden 0.000137", got)
	}

	var nilCfg *Config
	if got := nilCfg.DefaultDiskGB(); got != 100 {
		t.Errorf("nil Config DefaultDiskGB() = %d; want 100, not 0", got)
	}
	if got := nilCfg.IncludedDiskGB(); got != 100 {
		t.Errorf("nil Config IncludedDiskGB() = %d; want 100", got)
	}
	if got := nilCfg.MaxDiskGB(); got != 250 {
		t.Errorf("nil Config MaxDiskGB() = %d; want 250", got)
	}
	if got := nilCfg.DiskPricePerGBHour(); got != 0 {
		t.Errorf("nil Config DiskPricePerGBHour() = %v; want 0 (never bill without a loaded price)", got)
	}
}

// TestLoad_InstanceDiskShipsInert pins the property that makes this change safe
// to deploy: with the shipped defaults, the included allowance equals the
// default disk AND the price is zero, so no launch can be quoted a disk uplift.
// If someone changes a default such that included < default or price > 0, every
// existing customer's quote moves — that must be a deliberate config change,
// not a code default.
func TestLoad_InstanceDiskShipsInert(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.IncludedDiskGB() < cfg.DefaultDiskGB() {
		t.Errorf("IncludedDiskGB() = %d < DefaultDiskGB() = %d; a default launch would be billed for disk",
			cfg.IncludedDiskGB(), cfg.DefaultDiskGB())
	}
	if cfg.DiskPricePerGBHour() != 0 {
		t.Errorf("DiskPricePerGBHour() = %v; want 0 until the upstream rate is confirmed against an invoice",
			cfg.DiskPricePerGBHour())
	}
}

// TestLoad_InstanceDiskInvalid: a non-positive default would provision a
// zero-sized filesystem, a max below the default would reject the disk we hand
// out by default, and a negative price would credit customers for disk. All
// fail fast at boot rather than surfacing as a broken launch.
func TestLoad_InstanceDiskInvalid(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")

	cases := []struct {
		name string
		env  map[string]string
	}{
		{"zero default", map[string]string{"LAUNCH_DEFAULT_DISK_GB": "0"}},
		{"negative default", map[string]string{"LAUNCH_DEFAULT_DISK_GB": "-10"}},
		{"non-numeric default", map[string]string{"LAUNCH_DEFAULT_DISK_GB": "big"}},
		{"negative included", map[string]string{"LAUNCH_INCLUDED_DISK_GB": "-1"}},
		{"non-numeric included", map[string]string{"LAUNCH_INCLUDED_DISK_GB": "lots"}},
		{"max below default", map[string]string{"LAUNCH_DEFAULT_DISK_GB": "100", "LAUNCH_MAX_DISK_GB": "50"}},
		{"non-numeric max", map[string]string{"LAUNCH_MAX_DISK_GB": "huge"}},
		{"negative price", map[string]string{"DISK_PRICE_PER_GB_HOUR": "-0.01"}},
		{"non-numeric price", map[string]string{"DISK_PRICE_PER_GB_HOUR": "free"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://test")
			t.Setenv("REDIS_URL", "redis://test")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatalf("Load() = nil error; want rejection for %s", tc.name)
			}
		})
	}
}

// TestLoad_ProviderDiskPricePerGBHour covers the per-provider disk-rate map:
// parsing, case-insensitive resolution, fallback to the global knob, and the
// same fail-fast validation as DISK_PRICE_PER_GB_HOUR (negative or
// non-numeric rates reject at boot, never silently meter at a garbage rate).
func TestLoad_ProviderDiskPricePerGBHour(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")
	t.Setenv("DISK_PRICE_PER_GB_HOUR", "0.00014")
	t.Setenv("PROVIDER_DISK_PRICE_PER_GB_HOUR_ALPHA", "0.00028")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if got := cfg.DiskPricePerGBHourFor("Alpha"); got != 0.00028 {
		t.Errorf("DiskPricePerGBHourFor(Alpha) = %v; want the 0.00028 override, case-insensitive", got)
	}
	if got := cfg.DiskPricePerGBHourFor("bravo"); got != 0.00014 {
		t.Errorf("DiskPricePerGBHourFor(bravo) = %v; want the 0.00014 global fallback", got)
	}

	var nilCfg *Config
	if got := nilCfg.DiskPricePerGBHourFor("bravo"); got != 0 {
		t.Errorf("nil Config DiskPricePerGBHourFor() = %v; want 0 (never bill without a loaded price)", got)
	}

	invalid := []struct {
		name string
		val  string
	}{
		{"negative per-provider rate", "-0.0001"},
		{"non-numeric per-provider rate", "free"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://test")
			t.Setenv("REDIS_URL", "redis://test")
			t.Setenv("PROVIDER_DISK_PRICE_PER_GB_HOUR_ALPHA", tc.val)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() = nil error; want rejection for %s", tc.name)
			}
		})
	}
}

// TestLoad_ProvidersDisabled locks the sitewide kill-switch parsing:
// PROVIDERS_DISABLED is comma-split, case-insensitive, whitespace-trimmed, and
// deduped, and IsProviderDisabled resolves names case-insensitively. An
// empty/unset value disables nothing.
func TestLoad_ProvidersDisabled(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		set       bool
		disabled  []string
		enabled   []string
		wantCount int
	}{
		{name: "unset", set: false, enabled: []string{"alpha", "charlie"}, wantCount: 0},
		{name: "empty", raw: "", set: true, enabled: []string{"alpha"}, wantCount: 0},
		{name: "single", raw: "alpha", set: true, disabled: []string{"alpha"}, enabled: []string{"bravo", "charlie"}, wantCount: 1},
		{name: "multi", raw: "alpha,bravo", set: true, disabled: []string{"alpha", "bravo"}, enabled: []string{"charlie"}, wantCount: 2},
		{name: "case+whitespace", raw: " Alpha , BRAVO ", set: true, disabled: []string{"alpha", "ALPHA", "bravo", "Bravo"}, enabled: []string{"charlie"}, wantCount: 2},
		{name: "dedup+empty-fields", raw: "alpha,,alpha, ", set: true, disabled: []string{"alpha"}, wantCount: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://test")
			t.Setenv("REDIS_URL", "redis://test")
			if tc.set {
				t.Setenv("PROVIDERS_DISABLED", tc.raw)
			} else {
				os.Unsetenv("PROVIDERS_DISABLED")
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if got := len(cfg.DisabledProviders); got != tc.wantCount {
				t.Errorf("len(DisabledProviders) = %d; want %d", got, tc.wantCount)
			}
			for _, n := range tc.disabled {
				if !cfg.IsProviderDisabled(n) {
					t.Errorf("IsProviderDisabled(%q) = false; want true", n)
				}
			}
			for _, n := range tc.enabled {
				if cfg.IsProviderDisabled(n) {
					t.Errorf("IsProviderDisabled(%q) = true; want false", n)
				}
			}
		})
	}
}

// TestIsProviderDisabled_NilSafe: a nil *Config and the empty default set both
// disable nothing, so the poller predicate and engine candidate filter can call
// it unconditionally without a guard.
func TestIsProviderDisabled_NilSafe(t *testing.T) {
	var c *Config
	if c.IsProviderDisabled("alpha") {
		t.Error("nil Config IsProviderDisabled = true; want false")
	}
	if (&Config{}).IsProviderDisabled("alpha") {
		t.Error("empty Config IsProviderDisabled = true; want false")
	}
}

// TestLoad_CostDriftTolerance verifies the quote-to-accept drift allowance:
// the shipped defaults, explicit overrides, nil-safety, and fail-fast on
// values that would invert the guard's meaning.
func TestLoad_CostDriftTolerance(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if got := cfg.CostDriftTolerancePct(); got != 5.0 {
		t.Errorf("CostDriftTolerancePct() = %v; want default 5.0", got)
	}
	if got := cfg.CostDriftToleranceAbs(); got != 0.02 {
		t.Errorf("CostDriftToleranceAbs() = %v; want default 0.02", got)
	}

	t.Setenv("PRICING_COST_DRIFT_TOLERANCE_PCT", "2.5")
	t.Setenv("PRICING_COST_DRIFT_TOLERANCE_ABS", "0.05")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() unexpected error with overrides: %v", err)
	}
	if got := cfg.CostDriftTolerancePct(); got != 2.5 {
		t.Errorf("CostDriftTolerancePct() = %v; want overridden 2.5", got)
	}
	if got := cfg.CostDriftToleranceAbs(); got != 0.05 {
		t.Errorf("CostDriftToleranceAbs() = %v; want overridden 0.05", got)
	}

	// Nil-safe resolves to 0 — exact-or-re-quote, which never eats an
	// unbudgeted cost.
	var nilCfg *Config
	if got := nilCfg.CostDriftTolerancePct(); got != 0 {
		t.Errorf("nil Config CostDriftTolerancePct() = %v; want 0", got)
	}
	if got := nilCfg.CostDriftToleranceAbs(); got != 0 {
		t.Errorf("nil Config CostDriftToleranceAbs() = %v; want 0", got)
	}
}

// TestLoad_CostDriftToleranceInvalid: a negative allowance would mean
// re-quoting launches that got CHEAPER, and a non-numeric value is a typo in a
// knob that silently controls how much margin a launch may burn.
func TestLoad_CostDriftToleranceInvalid(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")

	for _, key := range []string{"PRICING_COST_DRIFT_TOLERANCE_PCT", "PRICING_COST_DRIFT_TOLERANCE_ABS"} {
		t.Setenv(key, "-1")
		if _, err := Load(); err == nil {
			t.Fatalf("Load() = nil error; want rejection of negative %s", key)
		}
		t.Setenv(key, "not-a-number")
		if _, err := Load(); err == nil {
			t.Fatalf("Load() = nil error; want parse error for %s", key)
		}
		t.Setenv(key, "") // restore the default for the next iteration
	}
}

// TestLoad_RescueCostAllowance verifies the boot-stall rescue allowance. It is
// deliberately separate from the quote-drift allowance: a rescue excludes the
// stalled host, so the replacement is pricier by construction, and the customer
// keeps paying the original rate either way.
func TestLoad_RescueCostAllowance(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if got := cfg.RescueCostAllowancePct(); got != 150.0 {
		t.Errorf("RescueCostAllowancePct() = %v; want default 150.0", got)
	}
	if got := cfg.RescueCostAllowanceAbs(); got != 0.25 {
		t.Errorf("RescueCostAllowanceAbs() = %v; want default 0.25", got)
	}
	// The rescue allowance must be the wider of the two — a rescue that priced
	// out under the quote allowance is a dead instance, not a saved one.
	if cfg.RescueCostAllowanceAbs() <= cfg.CostDriftToleranceAbs() {
		t.Errorf("rescue allowance %v must exceed the quote allowance %v",
			cfg.RescueCostAllowanceAbs(), cfg.CostDriftToleranceAbs())
	}

	t.Setenv("PRICING_RESCUE_COST_ALLOWANCE_PCT", "50")
	t.Setenv("PRICING_RESCUE_COST_ALLOWANCE_ABS", "1.5")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() unexpected error with overrides: %v", err)
	}
	if got := cfg.RescueCostAllowancePct(); got != 50 {
		t.Errorf("RescueCostAllowancePct() = %v; want overridden 50", got)
	}
	if got := cfg.RescueCostAllowanceAbs(); got != 1.5 {
		t.Errorf("RescueCostAllowanceAbs() = %v; want overridden 1.5", got)
	}

	var nilCfg *Config
	if got := nilCfg.RescueCostAllowancePct(); got != 0 {
		t.Errorf("nil Config RescueCostAllowancePct() = %v; want 0", got)
	}
	if got := nilCfg.RescueCostAllowanceAbs(); got != 0 {
		t.Errorf("nil Config RescueCostAllowanceAbs() = %v; want 0", got)
	}
}

// TestLoad_RescueCostAllowanceInvalid: a negative subsidy is meaningless and a
// typo in a knob that controls unattended spend must not boot.
func TestLoad_RescueCostAllowanceInvalid(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("REDIS_URL", "redis://test")

	for _, key := range []string{"PRICING_RESCUE_COST_ALLOWANCE_PCT", "PRICING_RESCUE_COST_ALLOWANCE_ABS"} {
		t.Setenv(key, "-1")
		if _, err := Load(); err == nil {
			t.Fatalf("Load() = nil error; want rejection of negative %s", key)
		}
		t.Setenv(key, "not-a-number")
		if _, err := Load(); err == nil {
			t.Fatalf("Load() = nil error; want parse error for %s", key)
		}
		t.Setenv(key, "")
	}
}
