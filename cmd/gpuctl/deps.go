package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/instanceid"
	"github.com/gpuai-dev/gpuai-oss/internal/privacy"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/provider/generic"
	"github.com/gpuai-dev/gpuai-oss/internal/provider/vultr"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
	"github.com/gpuai-dev/gpuai-oss/internal/supplier"
	"github.com/gpuai-dev/gpuai-oss/internal/tunnel"
	"github.com/redis/go-redis/v9"
)

// commonDeps holds the infrastructure shared by the serve loop.
type commonDeps struct {
	Config        *config.Config
	DB            *db.Pool
	Redis         *redis.Client
	Registry      *provider.Registry
	TunnelMgr     *tunnel.Manager
	Engine        *provision.Engine
	Breaker       *availability.Breaker
	LaunchBreaker *availability.Breaker
	FundingGate   *availability.FundingGate
	Logger        *slog.Logger
}

// fatal logs and exits. Used during boot, where there is nothing to unwind.
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

// loadConfig loads and validates the environment configuration.
func loadConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		fatal("failed to load configuration", "error", err)
	}
	return cfg
}

// connectDB opens the Postgres pool with retry and, when cfg.AutoMigrate (or
// forceMigrate) is set, applies pending migrations.
func connectDB(ctx context.Context, cfg *config.Config, forceMigrate bool) *db.Pool {
	var pool *db.Pool
	err := db.ConnectWithRetry(ctx, "postgres", 8, func(ctx context.Context) error {
		p, err := db.NewPool(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		pool = p
		return nil
	})
	if err != nil {
		fatal("failed to connect to postgres", "error", err)
	}
	if cfg.AutoMigrate || forceMigrate {
		applied, err := pool.Migrate(ctx)
		if err != nil {
			fatal("database migration failed", "error", err)
		}
		slog.Info("database schema up to date", "applied_now", len(applied))
	}
	return pool
}

// setupCommonDeps wires config, Postgres, Redis, the provider registry, the
// optional FRP tunnel and the provisioning engine.
func setupCommonDeps(ctx context.Context, logger *slog.Logger) *commonDeps {
	cfg := loadConfig()

	// Stamp this deployment onto the upstream resource names every adapter
	// mints, and scope the reconciler's ownership check to it, so two
	// deployments sharing one supplier account never reap each other.
	instanceid.SetLocalEnv(cfg.GpuctlEnv)
	slog.Info("environment configured", "gpuctl_env", cfg.GpuctlEnv)

	dbPool := connectDB(ctx, cfg, false)

	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		fatal("failed to parse REDIS_URL", "error", err)
	}
	redisClient := redis.NewClient(opt)
	if err := db.ConnectWithRetry(ctx, "redis", 8, func(ctx context.Context) error {
		return redisClient.Ping(ctx).Err()
	}); err != nil {
		fatal("failed to connect to redis", "error", err)
	}

	// Single tenant: one default organization owns every key and instance.
	if _, _, err := ensureDefaultOrg(ctx, dbPool); err != nil {
		fatal("failed to ensure the default organization", "error", err)
	}
	if err := seedBootstrapAPIKey(ctx, dbPool, cfg.BootstrapAPIKey); err != nil {
		fatal("failed to seed GPUCTL_BOOTSTRAP_API_KEY", "error", err)
	}

	registry := provider.NewRegistry()

	// Generic supplier adapter: always registered. It multiplexes every
	// active row of the suppliers table; GENERIC_SUPPLIERS rows are upserted
	// into that table first.
	supplierStore := supplier.NewStore(dbPool)
	staticTokens, err := seedGenericSuppliers(ctx, supplierStore, cfg.GenericSuppliers)
	if err != nil {
		fatal("failed to seed GENERIC_SUPPLIERS", "error", err)
	}
	genericAdapter := generic.NewAdapter(supplierStore, logger)
	genericAdapter.SetTokenFunc(supplierTokenFunc(cfg, staticTokens, logger))
	registry.Register(genericAdapter)
	slog.Info("registered provider", "name", "generic", "configured_suppliers", len(cfg.GenericSuppliers))

	// Vultr: the example real-supplier adapter, gated on its API key.
	if cfg.VultrAPIKey != "" {
		registry.Register(vultr.NewAdapter(cfg.VultrAPIKey).
			SetAccountLimits(cfg.VultrMaxInstances, cfg.VultrMaxMonthlyCostUSD))
		slog.Info("registered provider", "name", "vultr")
	}

	// Optional FRP tunnel. Off by default: instances are reached directly at
	// the supplier-reported address.
	var tunnelMgr *tunnel.Manager
	if cfg.FRPToken != "" {
		tunnelMgr, err = tunnel.NewManager(cfg.FRPBindPort, cfg.FRPToken, cfg.FRPAllowPorts, logger)
		if err != nil {
			fatal("failed to create FRP tunnel manager", "error", err)
		}
		slog.Info("FRP tunnel enabled", "bind_port", cfg.FRPBindPort, "allow_ports", cfg.FRPAllowPorts)
	}

	launchBreaker := availability.NewRateBreaker(
		cfg.LaunchBreakerWindow,
		cfg.LaunchBreakerMinSamples,
		cfg.LaunchBreakerFailRate,
		cfg.LaunchBreakerCooldown,
		logger,
	)
	fundingGate := availability.NewFundingGate(availability.DefaultFundingCooldown, logger)

	var sshReady func(ctx context.Context, ip string, port int) bool
	if cfg.DirectSSHCheck {
		sshReady = provision.ProbeSSHBanner
	}
	engine := provision.NewEngine(provision.EngineDeps{
		SSHReady:      sshReady,
		Registry:      registry,
		DB:            dbPool,
		Config:        cfg,
		Logger:        logger,
		TunnelMgr:     tunnelMgr,
		LaunchBreaker: launchBreaker,
		FundingGate:   fundingGate,
	})

	breaker := availability.NewBreaker(cfg.BreakerFailureThreshold, cfg.BreakerCooldown, logger)

	return &commonDeps{
		Config:        cfg,
		DB:            dbPool,
		Redis:         redisClient,
		Registry:      registry,
		TunnelMgr:     tunnelMgr,
		Engine:        engine,
		Breaker:       breaker,
		LaunchBreaker: launchBreaker,
		FundingGate:   fundingGate,
		Logger:        logger,
	}
}

// seedGenericSuppliers upserts GENERIC_SUPPLIERS into the suppliers table,
// registers every supplier identity with the customer-facing scrubber, and
// returns the static bearer tokens keyed by slug.
func seedGenericSuppliers(ctx context.Context, store *supplier.Store, sups []config.GenericSupplier) (map[string]string, error) {
	tokens := map[string]string{}
	for _, gs := range sups {
		status := supplier.StatusActive
		if gs.Disabled {
			status = supplier.StatusSuspended
		}
		row := &supplier.Supplier{
			Slug:    gs.Slug,
			Name:    gs.Name,
			BaseURL: gs.BaseURL,
			Status:  status,
			Regions: gs.Regions,
		}
		if err := store.Upsert(ctx, row); err != nil {
			return nil, fmt.Errorf("upsert supplier %q: %w", gs.Slug, err)
		}
		if gs.APIKey != "" {
			tokens[gs.Slug] = gs.APIKey
		}
		// Supplier identity never reaches a customer: error text naming the
		// slug or display name is neutralised at the API boundary.
		privacy.RegisterBrandTokens(gs.Slug, gs.Name)
		slog.Info("generic supplier configured", "slug", gs.Slug, "base_url", gs.BaseURL, "status", status)
	}
	// Rows added to the table by hand are scrubbed too.
	if active, err := store.ListActive(ctx); err == nil {
		for _, s := range active {
			privacy.RegisterBrandTokens(s.Slug, s.Name)
		}
	}
	return tokens, nil
}

// supplierTokenFunc resolves the bearer token for a supplier call: outbound
// OAuth client credentials (SUPPLIER_OAUTH_*) win, then the static api_key
// from GENERIC_SUPPLIERS, else no Authorization header.
func supplierTokenFunc(cfg *config.Config, static map[string]string, logger *slog.Logger) generic.TokenFunc {
	var oauth *supplier.TokenSource
	if len(cfg.SupplierOAuth) > 0 {
		creds := make(map[string]supplier.OutboundCred, len(cfg.SupplierOAuth))
		slugs := make([]string, 0, len(cfg.SupplierOAuth))
		for slug, c := range cfg.SupplierOAuth {
			creds[slug] = supplier.OutboundCred{ClientID: c.ClientID, ClientSecret: c.ClientSecret, TokenURL: c.TokenURL}
			slugs = append(slugs, slug)
		}
		sort.Strings(slugs)
		oauth = supplier.NewTokenSource(creds, logger)
		slog.Info("supplier outbound OAuth configured", "suppliers", strings.Join(slugs, ","))
	}
	return func(ctx context.Context, s supplier.Supplier) (string, error) {
		slug := strings.ToLower(s.Slug)
		if oauth != nil {
			if _, ok := cfg.SupplierOAuth[slug]; ok {
				return oauth.Token(ctx, s)
			}
		}
		return static[slug], nil
	}
}

// newAvailCache builds the Redis-backed offerings cache.
func newAvailCache(redisClient *redis.Client, key string) *availability.Cache {
	return availability.NewCacheWithKey(redisClient, 90*time.Second, key)
}
