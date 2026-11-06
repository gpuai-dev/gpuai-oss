package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/api"
	"github.com/gpuai-dev/gpuai-oss/internal/api/public"
	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/health"
	"github.com/gpuai-dev/gpuai-oss/internal/manifest"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
	"github.com/gpuai-dev/gpuai-oss/internal/reconcile"
	"github.com/gpuai-dev/gpuai-oss/internal/registrycheck"
	"github.com/gpuai-dev/gpuai-oss/internal/registrycred"
	"github.com/gpuai-dev/gpuai-oss/internal/spotrouter"
	tmplcatalog "github.com/gpuai-dev/gpuai-oss/internal/template/catalog"
)

// lifecycleInterval is the cadence of the stuck-provision reaper and the
// per-instance auto-terminate sweep.
const lifecycleInterval = 60 * time.Second

// setupLogger installs the process-wide slog handler.
func setupLogger(levelName, format string) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(levelName) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

// safeGo runs fn in a goroutine that logs (instead of crashing the process on)
// a panic, so one misbehaving worker cannot take the API down with it.
func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("background worker panicked", "worker", name, "panic", r)
			}
		}()
		fn()
	}()
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	logLevel := fs.String("log-level", envOr("GPUCTL_LOG_LEVEL", "info"), "log level: debug, info, warn, error")
	logFormat := fs.String("log-format", envOr("GPUCTL_LOG_FORMAT", "json"), "log format: json, text")
	_ = fs.Parse(args)
	logger := setupLogger(*logLevel, *logFormat)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	deps := setupCommonDeps(ctx, logger)
	defer deps.DB.Close()
	defer deps.Redis.Close()
	cfg := deps.Config

	if deps.TunnelMgr != nil {
		safeGo("frp-tunnel", func() { deps.TunnelMgr.Start(ctx) })
	}

	// Pinned base environments (per-GPU driver floors and digest-pinned
	// certified images). Fail fast on a malformed embedded manifest.
	pinnedManifest, err := manifest.LoadEmbedded()
	if err != nil {
		fatal("pinned-base-environment manifest load failed", "error", err)
	}
	deps.Engine.SetManifest(pinnedManifest)

	if cfg.ProvisionHostQuarantine {
		deps.Engine.SetHostQuarantine(availability.NewHostQuarantine(
			availability.DefaultHostQuarantineHold, availability.DefaultHostQuarantineMax, logger))
	}
	var placementMemory *availability.PlacementMemory
	if cfg.ProvisionPlacementMemory {
		placementMemory = availability.NewPlacementMemory(availability.DefaultPlacementBackoff,
			availability.DefaultPlacementStreakWindow, logger)
		deps.Engine.SetPlacementMemory(placementMemory)
	}

	// Availability: poll every provider into the Redis offerings cache that
	// GET /v1/pricing and /v1/gpu-types read.
	availCache := newAvailCache(deps.Redis, cfg.OfferingsCacheKey)
	slog.Info("retail pricing policy",
		"markup_pct", cfg.PricingMarkupPct,
		"min_retail_markup_pct", cfg.MinRetailMarkupPct())
	availPoller := availability.NewPoller(
		deps.Registry,
		availCache,
		30*time.Second,
		cfg.MarkupPctFor,
		logger,
		availability.WithBreaker(deps.Breaker),
		availability.WithLaunchBreaker(deps.LaunchBreaker),
		availability.WithFundingGate(deps.FundingGate),
		availability.WithPlacementMemory(placementMemory),
		availability.WithProbeTimeout(cfg.BreakerProbeTimeout),
		availability.WithRetention(cfg.AvailabilityRetention),
		availability.WithMinRetailMarkup(cfg.MinRetailMarkupPct()),
		availability.WithCostDiscountFor(cfg.CostDiscountPctFor),
		availability.WithDefaultInstanceDisk(cfg.DefaultDiskGB()),
		availability.WithDiskPriceFor(cfg.DiskPricePerGBHourFor),
		availability.WithDisabledCheck(cfg.IsProviderDisabled),
		availability.WithManifest(pinnedManifest),
	)
	safeGo("availability-poller", func() { availPoller.Start(ctx) })

	// Template deploys are not part of GPU.ai Open Source: ship an empty
	// catalog so every template_id is rejected as unknown.
	templates, err := tmplcatalog.LoadEmpty()
	if err != nil {
		fatal("template catalog load failed", "error", err)
	}
	sealer, err := registrycred.NewSealer(cfg.RegistryCredentialsKey)
	if err != nil {
		fatal("REGISTRY_CREDENTIALS_KEY invalid", "error", err)
	}

	srv := api.NewServer(api.ServerDeps{
		DB:    deps.DB,
		Redis: deps.Redis,
		Public: public.PublicDeps{
			DB:                 deps.DB,
			Redis:              deps.Redis,
			Config:             cfg,
			Engine:             deps.Engine,
			AvailCache:         availCache,
			Providers:          deps.Registry,
			Logger:             logger,
			TemplateCatalog:    templates,
			RegistryChecker:    registrycheck.New(),
			RegistryCredSealer: sealer,
		},
	})

	// Health monitor: upstream status checks and, with the FRP tunnel, SSH
	// reachability probes through the gateway.
	tunnelProbeHost := ""
	if deps.TunnelMgr != nil {
		tunnelProbeHost = cfg.FRPPublicHost
		if tunnelProbeHost == "" {
			tunnelProbeHost = hostFromURL(cfg.GpuctlPublicURL)
		}
	}
	healthMonitor := health.NewMonitor(health.MonitorDeps{
		DB:              deps.DB,
		Registry:        deps.Registry,
		Logger:          logger,
		Interval:        60 * time.Second,
		TunnelProbeHost: tunnelProbeHost,
		FailThreshold:   cfg.TunnelFailThreshold,
		EscalateAfter:   cfg.TunnelEscalateAfter,
	})
	safeGo("health-monitor", func() { healthMonitor.Start(ctx) })

	// Spot preemption watcher: reroutes reclaimed spot instances.
	spotWatcher := spotrouter.NewWatcher(spotrouter.WatcherDeps{
		DB:        deps.DB,
		Providers: deps.Registry,
		Reroute: func(ctx context.Context, inst db.Instance) error {
			_, err := deps.Engine.RerouteSpot(ctx, inst)
			return err
		},
		Logger: logger,
	})
	safeGo("spot-preemption-watcher", func() { spotWatcher.Start(ctx) })

	// Drift reconciler: orphans, ghosts and stuck terminations.
	reconciler := reconcile.New(reconcile.Deps{
		DB:               deps.DB,
		Registry:         deps.Registry,
		Terminator:       deps.Engine,
		Logger:           logger,
		Interval:         time.Duration(cfg.ReconcilerIntervalSeconds) * time.Second,
		RecentTermWindow: time.Duration(cfg.ReconcilerRecentTermWindowHours) * time.Hour,
		DryRun:           !cfg.ReconcilerEnforce,
		GhostDryRun:      true,
	})
	safeGo("reconciler", func() { reconciler.Start(ctx) })

	// Lifecycle sweeper: reap stuck provisions, enforce auto_terminate_hours.
	safeGo("lifecycle-sweeper", func() { runLifecycleSweeper(ctx, deps.DB, deps.Engine) })

	httpServer := &http.Server{
		Addr:              net.JoinHostPort("", cfg.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("gpuctl listening", "addr", httpServer.Addr, "version", versionOrDev())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("http server failed", "error", err)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

// runLifecycleSweeper ticks the engine's stuck-provision reaper and
// auto-terminate sweep while this process holds the lifecycle lease, so a
// multi-replica deployment acts on each deadline once.
func runLifecycleSweeper(ctx context.Context, pool *db.Pool, engine *provision.Engine) {
	for ctx.Err() == nil {
		lease, err := pool.AcquireLifecycleLease(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("lifecycle lease acquire failed; retrying", "error", err)
			time.Sleep(5 * time.Second)
			continue
		}
		slog.Info("lifecycle sweeper is leader")
		ticker := time.NewTicker(lifecycleInterval)
	loop:
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				lease.Release()
				return
			case <-lease.Done():
				ticker.Stop()
				slog.Warn("lifecycle lease lost; re-electing")
				break loop
			case <-ticker.C:
				if err := engine.ReapStuckProvisions(ctx); err != nil {
					slog.Error("reap stuck provisions failed", "error", err)
				}
				if err := engine.SweepAutoTerminate(ctx); err != nil {
					slog.Error("auto-terminate sweep failed", "error", err)
				}
			}
		}
	}
}

// hostFromURL returns the hostname of rawURL, or "" when it has none.
func hostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func versionOrDev() string {
	if version == "" {
		return "dev"
	}
	return version
}
