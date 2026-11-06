package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// runMigrate applies pending migrations and exits. Useful when
// GPUCTL_AUTO_MIGRATE=false and migrations are run as a separate step.
func runMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	_ = fs.Parse(args)
	setupLogger("info", "text")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg := loadConfig()
	pool := connectDB(ctx, cfg, true)
	pool.Close()
	slog.Info("migrations complete")
}

// runAPIKey implements `gpuctl apikey create --name NAME [--scopes JSON]`.
// The raw key is printed exactly once on stdout; only its hash is stored.
func runAPIKey(args []string) {
	if len(args) < 1 || args[0] != "create" {
		fmt.Fprintln(os.Stderr, "usage: gpuctl apikey create --name NAME [--scopes '{\"full_access\":true}']")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("apikey create", flag.ExitOnError)
	name := fs.String("name", "", "a label for the key (required)")
	scopes := fs.String("scopes", `{"full_access":true}`, "scope document as JSON")
	_ = fs.Parse(args[1:])
	if *name == "" {
		fmt.Fprintln(os.Stderr, "error: --name is required")
		os.Exit(2)
	}
	// Logs go to stderr so stdout carries only the key.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg := loadConfig()
	pool := connectDB(ctx, cfg, false)
	defer pool.Close()

	raw, k, err := createAPIKey(ctx, pool, *name, json.RawMessage(*scopes))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "created API key %q (id %s). It is shown once; store it now.\n", k.Name, k.APIKeyID)
	fmt.Println(raw)
}
