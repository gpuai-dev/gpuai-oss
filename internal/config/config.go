// Package config loads gpuctl configuration from environment variables.
//
// Every variable is documented in docs/configuration.md and .env.example. Only
// DATABASE_URL and REDIS_URL are required; everything else has a default that
// runs a single-node, single-tenant deployment.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/registrycred"
)

// Instance-disk defaults. Declared as constants so the env-var default and the
// nil-Config fallback of each accessor cannot drift apart.
const (
	defaultLaunchDiskGB     = 100
	defaultIncludedDiskGB   = 100
	defaultMaxDiskGB        = 250
	defaultDiskPricePerGBHr = 0.0
)

// DefaultProvisionPullStallAfter is the default dead-pull window of the
// boot-stall watchdog: how long a pre-running boot on an adapter that reports
// boot progress may show the same progress message before it is judged
// stalled and rescued. Large container images can sit silently on their last
// layer while docker unpacks them for several minutes, so a shorter window
// kills healthy boots.
const DefaultProvisionPullStallAfter = 8 * time.Minute

// Config holds all configuration values for gpuctl.
type Config struct {
	// Port is the HTTP listen port (GPUCTL_PORT). Default "8080".
	Port string
	// DatabaseURL is the PostgreSQL connection string (DATABASE_URL). Required.
	DatabaseURL string
	// RedisURL is the Redis connection string (REDIS_URL). Required: Redis
	// holds the live offerings cache and the per-key rate-limit counters.
	RedisURL string
	// GpuctlEnv names the deployment (GPUCTL_ENV). Default "development". It
	// is stamped onto upstream resource names so two deployments sharing one
	// supplier account never reconcile each other's instances away.
	GpuctlEnv string
	// DirectSSHCheck makes direct-connect instances report running only once
	// their SSH endpoint answers (GPUCTL_DIRECT_SSH_CHECK). Default true. Turn
	// it off when gpuctl has no network route to instances.
	DirectSSHCheck bool

	// GpuctlPublicURL is the externally reachable base URL of this gpuctl
	// (GPUCTL_PUBLIC_URL). Only needed with the FRP tunnel enabled: instance
	// bootstraps call back to it, and the tunnel host defaults to its hostname.
	GpuctlPublicURL string
	// AutoMigrate applies pending database migrations at startup
	// (GPUCTL_AUTO_MIGRATE). Default true.
	AutoMigrate bool
	// BootstrapAPIKey, when set, is seeded at startup as a full-access API key
	// for the default organization (GPUCTL_BOOTSTRAP_API_KEY). It must have the
	// gpuai_live_ shape; only its SHA-256 hash is stored.
	BootstrapAPIKey string
	// OfferingsCacheKey overrides the Redis key that holds the live offerings
	// snapshot (OFFERINGS_CACHE_KEY). Set it when two deployments share one
	// Redis.
	OfferingsCacheKey string
	// RegistryCredentialsKey is the 32-byte (base64) key that seals stored
	// private-registry passwords (REGISTRY_CREDENTIALS_KEY). Empty leaves the
	// /v1/registry-credentials endpoints answering 503.
	RegistryCredentialsKey string
	// AuthzEnforce turns role authorization on instance writes from shadow
	// (log would-deny decisions) to enforce (403) (AUTHZ_ENFORCE). Default false.
	AuthzEnforce bool

	// GenericSuppliers are the suppliers reached through the generic adapter
	// (GENERIC_SUPPLIERS, a JSON array). They are upserted into the suppliers
	// table at startup; rows added to that table directly work too.
	GenericSuppliers []GenericSupplier
	// SupplierOAuth holds per-supplier outbound OAuth2 client credentials,
	// keyed by lowercase supplier slug, parsed from
	// SUPPLIER_OAUTH_CLIENT_ID_<SLUG> / SUPPLIER_OAUTH_CLIENT_SECRET_<SLUG>
	// (+ optional SUPPLIER_OAUTH_TOKEN_URL_<SLUG>). The generic adapter
	// exchanges them at the supplier's token endpoint for a short-lived bearer
	// before each call. A token URL left unset defaults to the supplier's
	// base_url + "/oauth/token".
	SupplierOAuth map[string]SupplierOAuthCred
	// VultrAPIKey enables the Vultr adapter (VULTR_API_KEY). Empty leaves it
	// unregistered.
	VultrAPIKey string
	// VultrMaxInstances / VultrMaxMonthlyCostUSD mirror the account-side caps
	// Vultr enforces (VULTR_MAX_INSTANCES default 15,
	// VULTR_MAX_MONTHLY_COST_USD default 4000). 0 disables the guard.
	VultrMaxInstances      int
	VultrMaxMonthlyCostUSD float64
	// DisabledProviders is the sitewide kill-switch (PROVIDERS_DISABLED, a
	// comma-separated list of registry names such as "generic,vultr"). A
	// disabled provider leaves the catalog and placement but stays registered
	// so existing instances can still be managed and terminated.
	DisabledProviders map[string]bool

	// PricingMarkupPct is the retail markup applied over supplier prices
	// (GPUCTL_MARKUP_PCT). Default 0: prices pass through unchanged.
	PricingMarkupPct float64
	// ProviderMarkupPct are per-provider markup overrides
	// (PRICING_MARKUP_PCT_<NAME>), keyed by lowercase registry name.
	ProviderMarkupPct map[string]float64
	// PricingMinRetailMarkupPct is the retail price floor: no offering is
	// published or launched below cost x (1 + this/100), rounded up to the
	// cent (PRICING_MIN_RETAIL_MARKUP_PCT). Default 0.
	PricingMinRetailMarkupPct float64
	// PricingCostDriftTolerancePct / Abs bound how far a supplier's accepted
	// cost may exceed its quote before the launch is refused and re-quoted
	// (PRICING_COST_DRIFT_TOLERANCE_PCT default 5, _ABS default 0.02 $/hr).
	PricingCostDriftTolerancePct float64
	PricingCostDriftToleranceAbs float64
	// PricingRescueCostAllowancePct / Abs bound how much more a boot-stall
	// rescue replacement may cost than the original placement
	// (PRICING_RESCUE_COST_ALLOWANCE_PCT default 150, _ABS default 0.25 $/hr).
	PricingRescueCostAllowancePct float64
	PricingRescueCostAllowanceAbs float64
	// ProviderCostDiscountPct are per-provider negotiated discounts below list
	// price (PROVIDER_COST_DISCOUNT_PCT_<NAME>, in [0,100)).
	ProviderCostDiscountPct map[string]float64

	// LaunchDefaultDiskGB is the disk given to launches that request none
	// (LAUNCH_DEFAULT_DISK_GB, default 100).
	LaunchDefaultDiskGB int
	// LaunchIncludedDiskGB is the disk bundled into the GPU hourly rate
	// (LAUNCH_INCLUDED_DISK_GB, default 100).
	LaunchIncludedDiskGB int
	// LaunchMaxDiskGB caps a requested disk_gb (LAUNCH_MAX_DISK_GB, default 250).
	LaunchMaxDiskGB int
	// PricingDiskPerGBHour is the wholesale hourly cost of one GB of disk
	// beyond the included allowance (DISK_PRICE_PER_GB_HOUR, default 0).
	PricingDiskPerGBHour float64
	// ProviderDiskPricePerGBHour are per-provider disk rates
	// (PROVIDER_DISK_PRICE_PER_GB_HOUR_<NAME>).
	ProviderDiskPricePerGBHour map[string]float64

	// FRPToken enables the optional FRP tunnel (FRP_TOKEN). Empty (the
	// default) runs in direct-connect mode: instances are reached at the
	// supplier-reported address and SSH port.
	FRPToken string
	// FRPBindPort is the port the embedded frps listens on for frpc clients
	// (FRP_BIND_PORT, default 7000).
	FRPBindPort int
	// FRPAllowPorts is the remote port range handed out to instances
	// (FRP_ALLOW_PORTS, default "10000-10255").
	FRPAllowPorts string
	// FRPPublicHost is the public hostname of frps (FRP_PUBLIC_HOST); empty
	// falls back to the hostname of GPUCTL_PUBLIC_URL.
	FRPPublicHost string
	// ProviderFRPTransport are per-provider frpc transport overrides
	// (FRP_TRANSPORT_<NAME>: tcp, websocket, wss, kcp or quic).
	ProviderFRPTransport map[string]string
	// AppsHost is the wildcard host for app URLs served through the tunnel
	// (APPS_HOST). Only used with the FRP tunnel.
	AppsHost string
	// WebTerminalDisabled turns off the browser console rendered into tunnel
	// bootstraps (WEB_TERMINAL_DISABLED=true).
	WebTerminalDisabled bool

	// ProvisionTunnelStallAfter / ProvisionPullStallAfter are the boot-stall
	// watchdog windows (PROVISION_TUNNEL_STALL_AFTER default 8m,
	// PROVISION_PULL_STALL_AFTER default 8m). 0 disables that leg.
	ProvisionTunnelStallAfter time.Duration
	ProvisionPullStallAfter   time.Duration
	// ProvisionBootRescues caps rescue relaunches per launch
	// (PROVISION_BOOT_RESCUES, default 1).
	ProvisionBootRescues int
	// ProvisionDefaultEnvironment is applied to launches that name no
	// environment or image on container-native placements
	// (PROVISION_DEFAULT_ENVIRONMENT, default "certified:pytorch"; "off"
	// keeps each adapter's own default image).
	ProvisionDefaultEnvironment string
	// ProvisionDefaultEnvScope widens the default environment to every
	// manifest-covered GPU ("all") or only GPUs the legacy images cannot run
	// ("incompatible", the default) (PROVISION_DEFAULT_ENVIRONMENT_SCOPE).
	ProvisionDefaultEnvScope string
	// ProvisionGPUCheck renders the bootstrap's startup GPU compute check:
	// "off", "shadow" (report only, the default) or "enforce"
	// (PROVISION_GPU_CHECK). It only runs where a bootstrap runs, that is
	// with the FRP tunnel enabled; direct-connect launches never run it.
	ProvisionGPUCheck string
	// ProvisionHostQuarantine keeps hosts whose bootstrap never phoned home
	// out of placement for a while (PROVISION_HOST_QUARANTINE, default on).
	ProvisionHostQuarantine bool
	// ProvisionPlacementMemory backs off offerings that keep failing to place
	// (PROVISION_PLACEMENT_MEMORY, default on).
	ProvisionPlacementMemory bool

	// BreakerFailureThreshold / BreakerCooldown govern the per-provider
	// ListAvailable circuit breaker (BREAKER_FAILURE_THRESHOLD default 5,
	// BREAKER_COOLDOWN_SECONDS default 60).
	BreakerFailureThreshold int
	BreakerCooldown         time.Duration
	// BreakerProbeTimeout caps a single ListAvailable call
	// (BREAKER_PROBE_TIMEOUT_SECONDS, default 10).
	BreakerProbeTimeout time.Duration
	// LaunchBreaker* tune the launch-outcome breaker that suppresses a
	// (provider, gpu_type) whose launches keep failing to boot
	// (LAUNCH_BREAKER_WINDOW, LAUNCH_BREAKER_MIN_SAMPLES,
	// LAUNCH_BREAKER_FAIL_RATE, LAUNCH_BREAKER_COOLDOWN_SECONDS default 600).
	// 0 means "derive the default".
	LaunchBreakerWindow     int
	LaunchBreakerMinSamples int
	LaunchBreakerFailRate   float64
	LaunchBreakerCooldown   time.Duration
	// AvailabilityRetention serves a provider's last good snapshot for this
	// long after a failed poll (AVAILABILITY_RETENTION_SECONDS, default 300).
	AvailabilityRetention time.Duration

	// TunnelFailThreshold / TunnelEscalateAfter tune the tunnel reachability
	// probe (TUNNEL_FAIL_THRESHOLD, TUNNEL_ESCALATE_AFTER). 0 = built-in default.
	TunnelFailThreshold int
	TunnelEscalateAfter time.Duration

	// ReconcilerEnforce flips the drift reconciler from dry-run (log intended
	// actions) to enforce (RECONCILER_ENFORCE, default false).
	ReconcilerEnforce bool
	// ReconcilerIntervalSeconds / ReconcilerRecentTermWindowHours expose the
	// reconciler cadence and recently-terminated lookback
	// (RECONCILER_INTERVAL_SECONDS, RECONCILER_RECENT_TERM_WINDOW_HOURS).
	// 0 = defaults (5m / 4h).
	ReconcilerIntervalSeconds       int
	ReconcilerRecentTermWindowHours int
}

// GenericSupplier is one entry of GENERIC_SUPPLIERS: a supplier that
// implements the Generic Supplier API (docs/suppliers.md).
type GenericSupplier struct {
	// Slug is the stable, URL-safe identifier ("acme-gpu"). It prefixes
	// upstream ids, so it must not change while instances are running.
	Slug string `json:"slug"`
	// Name is a display name for operators. Never shown to API customers.
	Name string `json:"name"`
	// BaseURL is the supplier API root; the adapter calls BaseURL + "/v1/...".
	BaseURL string `json:"base_url"`
	// APIKey, when set, is sent as "Authorization: Bearer <api_key>" on every
	// call. Suppliers using OAuth client credentials configure
	// SUPPLIER_OAUTH_* instead, which takes precedence.
	APIKey string `json:"api_key,omitempty"`
	// Regions optionally lists the supplier's regions (informational).
	Regions []string `json:"regions,omitempty"`
	// Disabled, when true, keeps the supplier row but marks it suspended.
	Disabled bool `json:"disabled,omitempty"`
}

// SupplierOAuthCred is one supplier's outbound OAuth2 client credentials, plus
// an optional explicit token endpoint. See Config.SupplierOAuth.
type SupplierOAuthCred struct {
	ClientID     string
	ClientSecret string
	// TokenURL is the supplier's OAuth token endpoint. Empty means derive it
	// from the supplier's base_url + "/oauth/token" at call time.
	TokenURL string
}

var supplierSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// ParseGenericSuppliers parses and validates a GENERIC_SUPPLIERS value. An
// empty string yields no suppliers.
func ParseGenericSuppliers(raw string) ([]GenericSupplier, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []GenericSupplier
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("GENERIC_SUPPLIERS must be a JSON array of suppliers: %w", err)
	}
	seen := map[string]bool{}
	for i := range out {
		s := &out[i]
		s.Slug = strings.ToLower(strings.TrimSpace(s.Slug))
		s.BaseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
		if !supplierSlugRe.MatchString(s.Slug) {
			return nil, fmt.Errorf("GENERIC_SUPPLIERS[%d]: slug %q must match %s", i, s.Slug, supplierSlugRe)
		}
		if seen[s.Slug] {
			return nil, fmt.Errorf("GENERIC_SUPPLIERS[%d]: duplicate slug %q", i, s.Slug)
		}
		seen[s.Slug] = true
		u, err := url.Parse(s.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("GENERIC_SUPPLIERS[%d] (%s): base_url %q must be an http(s) URL", i, s.Slug, s.BaseURL)
		}
		if strings.TrimSpace(s.Name) == "" {
			s.Name = s.Slug
		}
	}
	return out, nil
}

// Load reads configuration from environment variables. It fails fast on a
// missing required variable or a malformed value whose silent fallback would
// misprice or misroute launches; tunables fall back to their default with a
// warning instead.
func Load() (*Config, error) {
	var missing []string
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		missing = append(missing, "REDIS_URL")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	// A PRESENT but malformed sealing key must fail boot (a typo silently
	// darking credential storage is worse than a crash); absent stays valid.
	if k := os.Getenv("REGISTRY_CREDENTIALS_KEY"); k != "" {
		if _, err := registrycred.NewSealer(k); err != nil {
			return nil, fmt.Errorf("REGISTRY_CREDENTIALS_KEY: %w", err)
		}
	}

	genericSuppliers, err := ParseGenericSuppliers(os.Getenv("GENERIC_SUPPLIERS"))
	if err != nil {
		return nil, err
	}

	pricingMarkupPct, err := parseFloatEnv("GPUCTL_MARKUP_PCT", 0)
	if err != nil {
		return nil, err
	}
	providerMarkupPct, err := parsePerProviderFloat("PRICING_MARKUP_PCT_", func(float64) bool { return true }, "")
	if err != nil {
		return nil, err
	}
	minRetailMarkupPct, err := parseNonNegativeFloatEnv("PRICING_MIN_RETAIL_MARKUP_PCT", 0)
	if err != nil {
		return nil, err
	}
	costDriftPct, err := parseNonNegativeFloatEnv("PRICING_COST_DRIFT_TOLERANCE_PCT", 5.0)
	if err != nil {
		return nil, err
	}
	costDriftAbs, err := parseNonNegativeFloatEnv("PRICING_COST_DRIFT_TOLERANCE_ABS", 0.02)
	if err != nil {
		return nil, err
	}
	rescueAllowPct, err := parseNonNegativeFloatEnv("PRICING_RESCUE_COST_ALLOWANCE_PCT", 150.0)
	if err != nil {
		return nil, err
	}
	rescueAllowAbs, err := parseNonNegativeFloatEnv("PRICING_RESCUE_COST_ALLOWANCE_ABS", 0.25)
	if err != nil {
		return nil, err
	}
	providerCostDiscountPct, err := parsePerProviderFloat("PROVIDER_COST_DISCOUNT_PCT_",
		func(v float64) bool { return v >= 0 && v < 100 }, "must be in [0, 100)")
	if err != nil {
		return nil, err
	}

	// Instance disk sizing: range-checked here rather than clamped at the
	// point of use, so a misconfiguration surfaces as a deploy that will not
	// start instead of a launch that silently misbehaves.
	launchDefaultDiskGB, err := parseIntStrict("LAUNCH_DEFAULT_DISK_GB", defaultLaunchDiskGB)
	if err != nil {
		return nil, err
	}
	if launchDefaultDiskGB <= 0 {
		return nil, fmt.Errorf("LAUNCH_DEFAULT_DISK_GB must be > 0, got %d", launchDefaultDiskGB)
	}
	launchIncludedDiskGB, err := parseIntStrict("LAUNCH_INCLUDED_DISK_GB", defaultIncludedDiskGB)
	if err != nil {
		return nil, err
	}
	if launchIncludedDiskGB < 0 {
		return nil, fmt.Errorf("LAUNCH_INCLUDED_DISK_GB must be >= 0, got %d", launchIncludedDiskGB)
	}
	launchMaxDiskGB, err := parseIntStrict("LAUNCH_MAX_DISK_GB", defaultMaxDiskGB)
	if err != nil {
		return nil, err
	}
	if launchMaxDiskGB < launchDefaultDiskGB {
		return nil, fmt.Errorf("LAUNCH_MAX_DISK_GB (%d) must be >= LAUNCH_DEFAULT_DISK_GB (%d)", launchMaxDiskGB, launchDefaultDiskGB)
	}
	diskPricePerGBHour, err := parseNonNegativeFloatEnv("DISK_PRICE_PER_GB_HOUR", defaultDiskPricePerGBHr)
	if err != nil {
		return nil, err
	}
	providerDiskPricePerGBHour, err := parsePerProviderFloat("PROVIDER_DISK_PRICE_PER_GB_HOUR_",
		func(v float64) bool { return v >= 0 }, "must be >= 0")
	if err != nil {
		return nil, err
	}

	// Per-provider frpc transport overrides: FRP_TRANSPORT_<NAME>=<protocol>.
	// The value is rendered verbatim into the instance's frpc.toml, so it is
	// validated against frp's closed set and rejected at boot otherwise. "tcp"
	// normalizes to no override so the default renders byte-identically.
	providerFRPTransport := make(map[string]string)
	for _, kv := range os.Environ() {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, "FRP_TRANSPORT_") {
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(key, "FRP_TRANSPORT_"))
		if name == "" {
			continue
		}
		proto := strings.ToLower(strings.TrimSpace(val))
		switch proto {
		case "", "tcp":
			continue
		case "websocket", "wss", "kcp", "quic":
			providerFRPTransport[name] = proto
		default:
			return nil, fmt.Errorf("%s must be one of tcp, websocket, wss, kcp, quic; got %q", key, val)
		}
	}

	supplierOAuth, err := parseSupplierOAuth()
	if err != nil {
		return nil, err
	}

	frpBindPort, err := parseIntStrict("FRP_BIND_PORT", 7000)
	if err != nil {
		return nil, err
	}

	breakerThreshold, err := parsePositiveInt("BREAKER_FAILURE_THRESHOLD", 5)
	if err != nil {
		return nil, err
	}
	breakerCooldownSecs, err := parsePositiveInt("BREAKER_COOLDOWN_SECONDS", 60)
	if err != nil {
		return nil, err
	}
	launchBreakerWindow, err := parsePositiveInt("LAUNCH_BREAKER_WINDOW", 0)
	if err != nil {
		return nil, err
	}
	launchBreakerMinSamples, err := parsePositiveInt("LAUNCH_BREAKER_MIN_SAMPLES", 0)
	if err != nil {
		return nil, err
	}
	launchBreakerFailRate := 0.0
	if raw := os.Getenv("LAUNCH_BREAKER_FAIL_RATE"); raw != "" {
		v, perr := strconv.ParseFloat(raw, 64)
		if perr != nil || math.IsNaN(v) || v <= 0 || v > 1 {
			slog.Warn("invalid LAUNCH_BREAKER_FAIL_RATE; using derived default", "raw", raw)
		} else {
			launchBreakerFailRate = v
		}
	}
	launchBreakerCooldownSecs, err := parsePositiveInt("LAUNCH_BREAKER_COOLDOWN_SECONDS", 600)
	if err != nil {
		return nil, err
	}
	breakerProbeSecs, err := parsePositiveInt("BREAKER_PROBE_TIMEOUT_SECONDS", 10)
	if err != nil {
		return nil, err
	}
	availabilityRetentionSecs, err := parsePositiveInt("AVAILABILITY_RETENTION_SECONDS", 300)
	if err != nil {
		return nil, err
	}
	reconcilerInterval, err := parsePositiveInt("RECONCILER_INTERVAL_SECONDS", 0)
	if err != nil {
		return nil, err
	}
	reconcilerWindow, err := parsePositiveInt("RECONCILER_RECENT_TERM_WINDOW_HOURS", 0)
	if err != nil {
		return nil, err
	}

	disabledProviders := make(map[string]bool)
	if raw := os.Getenv("PROVIDERS_DISABLED"); raw != "" {
		var list []string
		for _, name := range strings.Split(raw, ",") {
			name = strings.TrimSpace(strings.ToLower(name))
			if name != "" && !disabledProviders[name] {
				disabledProviders[name] = true
				list = append(list, name)
			}
		}
		if len(list) > 0 {
			slog.Warn("provider kill-switch active (PROVIDERS_DISABLED)", "disabled", list)
		}
	}

	// Boot-stall watchdog knobs. Fail-soft: a garbage value logs and falls
	// back to the default rather than block boot. 0 disables that leg.
	tunnelStallAfter := parseDurationEnv("PROVISION_TUNNEL_STALL_AFTER", 8*time.Minute)
	pullStallAfter := parseDurationEnv("PROVISION_PULL_STALL_AFTER", DefaultProvisionPullStallAfter)
	defaultEnvironment := "certified:pytorch"
	if raw := strings.TrimSpace(os.Getenv("PROVISION_DEFAULT_ENVIRONMENT")); raw != "" {
		switch {
		case raw == "off":
			defaultEnvironment = ""
		case raw == "certified" || strings.HasPrefix(raw, "certified:"):
			defaultEnvironment = raw
		default:
			slog.Warn("invalid PROVISION_DEFAULT_ENVIRONMENT (only the certified class may be a default); using adapter defaults",
				"raw", raw)
			defaultEnvironment = ""
		}
	}
	defaultEnvironmentScope := "incompatible"
	if raw := strings.TrimSpace(os.Getenv("PROVISION_DEFAULT_ENVIRONMENT_SCOPE")); raw == "all" || raw == "incompatible" {
		defaultEnvironmentScope = raw
	} else if raw != "" {
		slog.Warn("invalid PROVISION_DEFAULT_ENVIRONMENT_SCOPE; using incompatible", "raw", raw)
	}
	gpuCheck := "shadow"
	if raw := strings.TrimSpace(os.Getenv("PROVISION_GPU_CHECK")); raw != "" {
		switch raw {
		case "off", "shadow", "enforce":
			gpuCheck = raw
		default:
			slog.Warn("invalid PROVISION_GPU_CHECK; using default", "raw", raw, "default", gpuCheck)
		}
	}
	bootRescues := 1
	if raw := os.Getenv("PROVISION_BOOT_RESCUES"); raw != "" {
		if v, perr := strconv.Atoi(raw); perr != nil || v < 0 {
			slog.Warn("invalid PROVISION_BOOT_RESCUES; using default", "raw", raw, "default", bootRescues)
		} else {
			bootRescues = v
		}
	}

	// Tunnel reachability knobs. 0 means "unset": health.NewMonitor
	// substitutes its built-in default.
	tunnelFailThreshold := 0
	if raw := os.Getenv("TUNNEL_FAIL_THRESHOLD"); raw != "" {
		if v, perr := strconv.Atoi(raw); perr != nil || v < 0 {
			slog.Warn("invalid TUNNEL_FAIL_THRESHOLD; using built-in default", "raw", raw)
		} else {
			tunnelFailThreshold = v
		}
	}
	tunnelEscalateAfter := parseDurationEnv("TUNNEL_ESCALATE_AFTER", 0)

	vultrMaxInstances := 15
	if raw := os.Getenv("VULTR_MAX_INSTANCES"); raw != "" {
		if v, perr := strconv.Atoi(raw); perr != nil || v < 0 {
			slog.Warn("invalid VULTR_MAX_INSTANCES; using default", "raw", raw, "default", vultrMaxInstances)
		} else {
			vultrMaxInstances = v
		}
	}
	vultrMaxMonthlyCostUSD := 4000.0
	if raw := os.Getenv("VULTR_MAX_MONTHLY_COST_USD"); raw != "" {
		if v, perr := strconv.ParseFloat(raw, 64); perr != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			slog.Warn("invalid VULTR_MAX_MONTHLY_COST_USD; using default", "raw", raw, "default", vultrMaxMonthlyCostUSD)
		} else {
			vultrMaxMonthlyCostUSD = v
		}
	}

	frpToken := os.Getenv("FRP_TOKEN")
	if frpToken == "" {
		slog.Info("FRP tunnel not configured; running in direct-connect mode")
	}

	return &Config{
		Port:                   getEnvDefault("GPUCTL_PORT", "8080"),
		DatabaseURL:            databaseURL,
		RedisURL:               redisURL,
		GpuctlEnv:              strings.ToLower(strings.TrimSpace(getEnvDefault("GPUCTL_ENV", "development"))),
		GpuctlPublicURL:        os.Getenv("GPUCTL_PUBLIC_URL"),
		AutoMigrate:            parseBoolEnv("GPUCTL_AUTO_MIGRATE", true),
		DirectSSHCheck:         parseBoolEnv("GPUCTL_DIRECT_SSH_CHECK", true),
		BootstrapAPIKey:        strings.TrimSpace(os.Getenv("GPUCTL_BOOTSTRAP_API_KEY")),
		OfferingsCacheKey:      os.Getenv("OFFERINGS_CACHE_KEY"),
		RegistryCredentialsKey: os.Getenv("REGISTRY_CREDENTIALS_KEY"),
		AuthzEnforce:           parseBoolEnv("AUTHZ_ENFORCE", false),

		GenericSuppliers:       genericSuppliers,
		SupplierOAuth:          supplierOAuth,
		VultrAPIKey:            os.Getenv("VULTR_API_KEY"),
		VultrMaxInstances:      vultrMaxInstances,
		VultrMaxMonthlyCostUSD: vultrMaxMonthlyCostUSD,
		DisabledProviders:      disabledProviders,

		PricingMarkupPct:              pricingMarkupPct,
		ProviderMarkupPct:             providerMarkupPct,
		PricingMinRetailMarkupPct:     minRetailMarkupPct,
		PricingCostDriftTolerancePct:  costDriftPct,
		PricingCostDriftToleranceAbs:  costDriftAbs,
		PricingRescueCostAllowancePct: rescueAllowPct,
		PricingRescueCostAllowanceAbs: rescueAllowAbs,
		ProviderCostDiscountPct:       providerCostDiscountPct,

		LaunchDefaultDiskGB:        launchDefaultDiskGB,
		LaunchIncludedDiskGB:       launchIncludedDiskGB,
		LaunchMaxDiskGB:            launchMaxDiskGB,
		PricingDiskPerGBHour:       diskPricePerGBHour,
		ProviderDiskPricePerGBHour: providerDiskPricePerGBHour,

		FRPToken:             frpToken,
		FRPBindPort:          frpBindPort,
		FRPAllowPorts:        getEnvDefault("FRP_ALLOW_PORTS", "10000-10255"),
		FRPPublicHost:        os.Getenv("FRP_PUBLIC_HOST"),
		ProviderFRPTransport: providerFRPTransport,
		AppsHost:             os.Getenv("APPS_HOST"),
		WebTerminalDisabled:  os.Getenv("WEB_TERMINAL_DISABLED") == "true",

		ProvisionTunnelStallAfter:   tunnelStallAfter,
		ProvisionPullStallAfter:     pullStallAfter,
		ProvisionBootRescues:        bootRescues,
		ProvisionDefaultEnvironment: defaultEnvironment,
		ProvisionDefaultEnvScope:    defaultEnvironmentScope,
		ProvisionGPUCheck:           gpuCheck,
		ProvisionHostQuarantine:     parseKillSwitchEnv("PROVISION_HOST_QUARANTINE"),
		ProvisionPlacementMemory:    parseKillSwitchEnv("PROVISION_PLACEMENT_MEMORY"),

		BreakerFailureThreshold: breakerThreshold,
		BreakerCooldown:         time.Duration(breakerCooldownSecs) * time.Second,
		BreakerProbeTimeout:     time.Duration(breakerProbeSecs) * time.Second,
		LaunchBreakerWindow:     launchBreakerWindow,
		LaunchBreakerMinSamples: launchBreakerMinSamples,
		LaunchBreakerFailRate:   launchBreakerFailRate,
		LaunchBreakerCooldown:   time.Duration(launchBreakerCooldownSecs) * time.Second,
		AvailabilityRetention:   time.Duration(availabilityRetentionSecs) * time.Second,

		TunnelFailThreshold: tunnelFailThreshold,
		TunnelEscalateAfter: tunnelEscalateAfter,

		ReconcilerEnforce:               parseBoolEnv("RECONCILER_ENFORCE", false),
		ReconcilerIntervalSeconds:       reconcilerInterval,
		ReconcilerRecentTermWindowHours: reconcilerWindow,
	}, nil
}

// parseSupplierOAuth scans SUPPLIER_OAUTH_{CLIENT_ID,CLIENT_SECRET,TOKEN_URL}_<SLUG>.
// A client_id without its client_secret (or vice versa) fails fast: the
// generic adapter would otherwise silently fall back to unauthenticated calls.
func parseSupplierOAuth() (map[string]SupplierOAuthCred, error) {
	out := make(map[string]SupplierOAuthCred)
	for _, kv := range os.Environ() {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || val == "" {
			continue
		}
		var slug string
		var set func(c *SupplierOAuthCred)
		switch {
		case strings.HasPrefix(key, "SUPPLIER_OAUTH_CLIENT_ID_"):
			slug = strings.TrimPrefix(key, "SUPPLIER_OAUTH_CLIENT_ID_")
			set = func(c *SupplierOAuthCred) { c.ClientID = val }
		case strings.HasPrefix(key, "SUPPLIER_OAUTH_CLIENT_SECRET_"):
			slug = strings.TrimPrefix(key, "SUPPLIER_OAUTH_CLIENT_SECRET_")
			set = func(c *SupplierOAuthCred) { c.ClientSecret = val }
		case strings.HasPrefix(key, "SUPPLIER_OAUTH_TOKEN_URL_"):
			slug = strings.TrimPrefix(key, "SUPPLIER_OAUTH_TOKEN_URL_")
			set = func(c *SupplierOAuthCred) { c.TokenURL = val }
		default:
			continue
		}
		// Env var names cannot carry '-', so a slug's dashes are written as
		// underscores (acme-gpu -> SUPPLIER_OAUTH_CLIENT_ID_ACME_GPU).
		slug = strings.ReplaceAll(strings.ToLower(slug), "_", "-")
		if slug == "" {
			continue
		}
		cred := out[slug]
		set(&cred)
		out[slug] = cred
	}
	for slug, cred := range out {
		env := strings.ToUpper(strings.ReplaceAll(slug, "-", "_"))
		if (cred.ClientID == "") != (cred.ClientSecret == "") {
			return nil, fmt.Errorf("supplier %q OAuth config is partial: SUPPLIER_OAUTH_CLIENT_ID_%s and SUPPLIER_OAUTH_CLIENT_SECRET_%s must be set as a pair (client_id_set=%v client_secret_set=%v)",
				slug, env, env, cred.ClientID != "", cred.ClientSecret != "")
		}
		if cred.TokenURL != "" && cred.ClientID == "" {
			return nil, fmt.Errorf("supplier %q OAuth config has SUPPLIER_OAUTH_TOKEN_URL_%s but no client credentials",
				slug, env)
		}
	}
	return out, nil
}

// parsePerProviderFloat scans <prefix><NAME>=<number> variables into a map
// keyed by lowercase NAME, rejecting values that fail valid.
func parsePerProviderFloat(prefix string, valid func(float64) bool, rule string) (map[string]float64, error) {
	out := make(map[string]float64)
	for _, kv := range os.Environ() {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, prefix) {
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(key, prefix))
		if name == "" {
			continue
		}
		v, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return nil, fmt.Errorf("%s must be a valid number: %w", key, err)
		}
		if !valid(v) {
			return nil, fmt.Errorf("%s %s, got %v", key, rule, v)
		}
		out[name] = v
	}
	return out, nil
}

func parseFloatEnv(key string, defaultVal float64) (float64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%s must be a valid number, got %q", key, raw)
	}
	return v, nil
}

func parseNonNegativeFloatEnv(key string, defaultVal float64) (float64, error) {
	v, err := parseFloatEnv(key, defaultVal)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, fmt.Errorf("%s must be >= 0, got %v", key, v)
	}
	return v, nil
}

func parseIntStrict(key string, defaultVal int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid integer: %w", key, err)
	}
	return v, nil
}

func parseDurationEnv(key string, defaultVal time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		slog.Warn("invalid duration env var; using default", "key", key, "raw", raw, "default", defaultVal)
		return defaultVal
	}
	return d
}

func parsePositiveInt(key string, defaultVal int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid integer: %w", key, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("%s must be non-negative, got %d", key, v)
	}
	return v, nil
}

func parseBoolEnv(key string, defaultVal bool) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		slog.Warn("invalid bool env var; using default",
			"key", key, "raw", raw, "default", defaultVal)
		return defaultVal
	}
	return v
}

// parseKillSwitchEnv reads a default-on feature switch. It is reached for
// mid-incident, so every usual spelling of "off" works in any case.
func parseKillSwitchEnv(key string) bool {
	raw := os.Getenv(key)
	word := strings.ToLower(strings.TrimSpace(raw))
	switch word {
	case "", "on", "yes":
		return true
	case "off", "no":
		return false
	}
	if v, err := strconv.ParseBool(word); err == nil {
		return v
	}
	slog.Warn("invalid kill-switch env var; leaving the feature on",
		"key", key, "raw", raw)
	return true
}

// getEnvDefault returns the value of the environment variable named by key,
// or defaultVal if the variable is not set or is empty.
func getEnvDefault(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return defaultVal
}

// MarkupPctFor returns the retail markup percentage for a provider: the
// PRICING_MARKUP_PCT_<NAME> override when one is set, otherwise the global
// GPUCTL_MARKUP_PCT. A nil Config resolves to 0% (prices pass through).
func (c *Config) MarkupPctFor(providerName string) float64 {
	if c == nil {
		return 0
	}
	if pct, ok := c.ProviderMarkupPct[strings.ToLower(providerName)]; ok {
		return pct
	}
	return c.PricingMarkupPct
}

// MinRetailMarkupPct returns the retail markup floor percentage. A nil Config
// resolves to 0, which still floors at "never below cost, rounded up to the
// cent".
func (c *Config) MinRetailMarkupPct() float64 {
	if c == nil {
		return 0
	}
	return c.PricingMinRetailMarkupPct
}

// CostDriftTolerancePct returns the percentage half of the quote-to-accept
// drift allowance. A nil Config resolves to 0 (exact-or-re-quote, the safe
// direction).
func (c *Config) CostDriftTolerancePct() float64 {
	if c == nil {
		return 0
	}
	return c.PricingCostDriftTolerancePct
}

// CostDriftToleranceAbs returns the absolute half of the quote-to-accept drift
// allowance in $/hr. Nil-safe like CostDriftTolerancePct.
func (c *Config) CostDriftToleranceAbs() float64 {
	if c == nil {
		return 0
	}
	return c.PricingCostDriftToleranceAbs
}

// RescueCostAllowancePct returns the percentage half of the boot-stall rescue
// allowance. A nil Config resolves to 0: a rescue then places only at or below
// the original cost.
func (c *Config) RescueCostAllowancePct() float64 {
	if c == nil {
		return 0
	}
	return c.PricingRescueCostAllowancePct
}

// RescueCostAllowanceAbs returns the absolute half of the boot-stall rescue
// allowance in $/hr. Nil-safe like RescueCostAllowancePct.
func (c *Config) RescueCostAllowanceAbs() float64 {
	if c == nil {
		return 0
	}
	return c.PricingRescueCostAllowanceAbs
}

// DefaultDiskGB returns the disk given to launches that request none. A nil or
// zero-valued Config resolves to the default rather than 0, which would ask an
// adapter for a zero-sized filesystem.
func (c *Config) DefaultDiskGB() int {
	if c == nil || c.LaunchDefaultDiskGB == 0 {
		return defaultLaunchDiskGB
	}
	return c.LaunchDefaultDiskGB
}

// IncludedDiskGB returns the disk allowance bundled into the GPU hourly rate.
func (c *Config) IncludedDiskGB() int {
	if c == nil {
		return defaultIncludedDiskGB
	}
	return c.LaunchIncludedDiskGB
}

// MaxDiskGB returns the ceiling on a requested disk_gb. A nil or zero-valued
// Config resolves to the default so validation still bounds the request.
func (c *Config) MaxDiskGB() int {
	if c == nil || c.LaunchMaxDiskGB == 0 {
		return defaultMaxDiskGB
	}
	return c.LaunchMaxDiskGB
}

// DiskPricePerGBHour returns the wholesale hourly cost of one GB of disk beyond
// the included allowance. A nil Config resolves to 0 (no uplift).
func (c *Config) DiskPricePerGBHour() float64 {
	if c == nil {
		return 0
	}
	return c.PricingDiskPerGBHour
}

// FRPTransportFor returns the frpc transport the named provider's instances
// tunnel with: the FRP_TRANSPORT_<NAME> override, otherwise "" (raw TCP).
func (c *Config) FRPTransportFor(providerName string) string {
	if c == nil {
		return ""
	}
	return c.ProviderFRPTransport[strings.ToLower(providerName)]
}

// DiskPricePerGBHourFor returns the disk rate for the named provider: the
// PROVIDER_DISK_PRICE_PER_GB_HOUR_<NAME> override, otherwise the global rate.
func (c *Config) DiskPricePerGBHourFor(providerName string) float64 {
	if c == nil {
		return 0
	}
	if rate, ok := c.ProviderDiskPricePerGBHour[strings.ToLower(providerName)]; ok {
		return rate
	}
	return c.PricingDiskPerGBHour
}

// CostDiscountPctFor returns the negotiated cost discount for a provider, or 0.
func (c *Config) CostDiscountPctFor(providerName string) float64 {
	if c == nil {
		return 0
	}
	return c.ProviderCostDiscountPct[strings.ToLower(providerName)]
}

// IsProviderDisabled reports whether the named provider has been switched off
// via PROVIDERS_DISABLED. A nil Config disables nothing.
func (c *Config) IsProviderDisabled(providerName string) bool {
	if c == nil || len(c.DisabledProviders) == 0 {
		return false
	}
	return c.DisabledProviders[strings.ToLower(providerName)]
}
