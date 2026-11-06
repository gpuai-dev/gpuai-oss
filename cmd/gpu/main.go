// Package main is the gpu CLI binary entry point.
//
// Version metadata is injected at build time via goreleaser ldflags:
//
//	-X main.version={{.Version}} -X main.commit={{.Commit}} -X main.date={{.Date}}
//
// Default values ("dev"/"none"/"unknown") allow `go run ./cmd/gpu` to work
// without ldflags during local development.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"runtime"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
	"github.com/gpuai-dev/gpuai-oss/internal/cliauth"
)

// Injected via -ldflags at build time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// runtimeDeps is the bag of dependencies stashed on cmd.Context() by
// PersistentPreRunE. Subcommand RunE handlers fetch it via depsFromCtx.
type runtimeDeps struct {
	Client *apiclient.Client
	Output string // "table" | "json"
	Debug  bool
	Logger *slog.Logger
}

type runtimeDepsKey struct{}

// depsFromCtx returns the runtimeDeps stashed by PersistentPreRunE, or nil
// when the command opted out of auth resolution (login/logout/version/help).
func depsFromCtx(ctx context.Context) *runtimeDeps {
	if d, ok := ctx.Value(runtimeDepsKey{}).(*runtimeDeps); ok {
		return d
	}
	return nil
}

// commandsThatSkipAuth lists commands that do NOT require a resolved token.
// These can run before `gpu login`. Matched against the whole parent chain
// (see skipsAuth) so an entry covers its subcommands too.
var commandsThatSkipAuth = map[string]bool{
	"login":   true,
	"logout":  true,
	"version": true,
	"help":    true,
}

func skipsAuth(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if commandsThatSkipAuth[c.Name()] {
			return true
		}
	}
	return false
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// defaultOutput picks "table" on a TTY and "json" when piped, so scripts get
// machine-friendly output by default.
func defaultOutput() string {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		return "table"
	}
	return "json"
}

// mutatingCmd marks a subcommand as state-changing. PersistentPreRunE prints
// the resolved target host to stderr for these, so a command run against the
// wrong server is never silent.
var mutatingCmd = map[string]string{"mutates": "true"}

// apiHost extracts the host from an api-base URL for the target-env notice,
// falling back to the raw value if it isn't a parseable URL.
func apiHost(apiBase string) string {
	if u, err := url.Parse(apiBase); err == nil && u.Host != "" {
		return u.Host
	}
	return apiBase
}

// newRootCmd constructs the gpu cobra root command with persistent flags and
// PersistentPreRunE that resolves auth + builds an apiclient.Client.
func newRootCmd() *cobra.Command {
	var (
		apiBase string
		output  string
		debug   bool
	)
	cmd := &cobra.Command{
		Use:           "gpu",
		Short:         "GPU.ai Open Source command-line interface",
		Long:          "gpu is the GPU.ai Open Source command-line interface. It provisions and manages GPU instances, SSH keys and registry credentials on a self-hosted gpuctl server, and connects to instances over SSH.",
		SilenceErrors: true, // we print our own friendly errors
		SilenceUsage:  true, // don't dump --help on every API error (noisy UX)
		Version:       fmt.Sprintf("%s (commit %s, built %s)", version, commit, date),
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Skip auth setup for commands that don't need a token.
			if skipsAuth(cmd) {
				return nil
			}
			tok, src, err := cliauth.ResolveToken()
			if err != nil {
				return fmt.Errorf("not authenticated — run `gpu login` or set GPUAI_API_KEY (%w)", err)
			}
			if !cmd.Flags().Changed("api-base") {
				apiBase, _ = cliauth.ResolveAPIBase()
			}
			lvl := slog.LevelInfo
			if debug {
				lvl = slog.LevelDebug
			}
			var logger *slog.Logger
			if debug {
				logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
			} else {
				logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			}
			client := apiclient.New(apiBase, tok, logger).
				WithUserAgent(fmt.Sprintf("gpu/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH))
			logger.Debug("authenticated", "source", src, "api_base", apiBase)
			// A state-changing command run against the wrong server is a
			// footgun. For mutating commands, echo the target host to stderr
			// so it's never silent.
			if cmd.Annotations["mutates"] == "true" {
				fmt.Fprintf(os.Stderr, "→ %s targeting %s\n", cmd.CommandPath(), apiHost(apiBase))
			}
			ctx := context.WithValue(cmd.Context(), runtimeDepsKey{}, &runtimeDeps{
				Client: client, Output: output, Debug: debug, Logger: logger,
			})
			cmd.SetContext(ctx)
			return nil
		},
	}
	cmd.PersistentFlags().StringVar(&apiBase, "api-base",
		envOr("GPUAI_API_BASE", cliauth.DefaultAPIBase),
		"API base URL (precedence: this flag, then GPUAI_API_BASE, then the api_base saved by gpu login, then this default)")
	cmd.PersistentFlags().StringVarP(&output, "output", "o", defaultOutput(),
		"Output format: table|json (default table on TTY, json otherwise)")
	cmd.PersistentFlags().BoolVar(&debug, "debug", false, "Enable debug logging to stderr")

	cmd.AddCommand(
		newLoginCmd(),
		newLogoutCmd(),
		newVersionCmd(),
		newInstancesCmd(),
		newKeysCmd(),
		newRegistryCredentialsCmd(),
		newOperationsCmd(),
		newGPUTypesCmd(),
		newPricingCmd(),
		newEnvironmentsCmd(),
	)
	return cmd
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := newRootCmd().ExecuteContext(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// Resource subcommand factories (newInstancesCmd, newKeysCmd, newGPUTypesCmd,
// newPricingCmd) live in their own files: instances.go, keys.go, gputypes.go,
// pricing.go. Each registers its subcommand tree and wires depsFromCtx +
// mapClientError into the cobra RunE handlers.
