package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

// execSSH is the injectable function used by `gpu instances ssh`. Tests
// override it to assert argv without actually executing ssh. The real
// implementation lives in instances_ssh_exec_unix.go (syscall.Exec) and
// instances_ssh_exec_other.go (friendly error stub).
//
// On success, the unix impl NEVER returns — syscall.Exec replaces the
// process image. On failure (ssh missing on PATH, exec error) it returns
// an error.
var execSSH func(host string, port int, extraArgs []string) error = syscallExecImpl

// sshOptionFlags are the ssh -o flags applied to every `gpu instances ssh`
// invocation. Defined as a package-level slice so tests can reference the
// exact same list. Rationale:
//   - StrictHostKeyChecking=no: every instance comes up with fresh host keys
//   - UserKnownHostsFile=/dev/null: don't pollute ~/.ssh/known_hosts
//   - LogLevel=ERROR: suppress "Warning: Permanently added" noise
//   - ServerAliveInterval=60: keep the session alive across NAT timeouts
var sshOptionFlags = []string{
	"-o", "StrictHostKeyChecking=no",
	"-o", "UserKnownHostsFile=/dev/null",
	"-o", "LogLevel=ERROR",
	"-o", "ServerAliveInterval=60",
}

func newInstancesSSHCmd() *cobra.Command {
	var (
		printOnly   bool
		wait        bool
		waitTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "ssh <instance-id> [-- ssh-extra-args...]",
		Short: "SSH into a running GPU instance",
		Long: "Connects to a running GPU instance over SSH. Resolves the host:port " +
			"from the API, then execs ssh with StrictHostKeyChecking=no etc. so first-connect " +
			"doesn't prompt. Anything after `--` is forwarded to ssh (e.g., `-L 8888:localhost:8888` " +
			"for port-forwarding).",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			id := args[0]
			extraArgs := args[1:] // anything after `--` (cobra collects positional args after `--`)

			inst, err := deps.Client.GetInstance(cmd.Context(), id)
			if err != nil {
				return mapClientError(err)
			}

			// Terminal-status fail-fast: don't enter the poll loop when the
			// instance is already terminated/errored.
			if inst.Status == "terminated" || inst.Status == "error" {
				return fmt.Errorf("instance %s is %s — cannot SSH", id, inst.Status)
			}

			if inst.Status != "running" {
				if !wait {
					return fmt.Errorf("instance %s is %s (not running) — re-run with --wait or wait for status=running", id, inst.Status)
				}
				inst, err = waitForRunning(cmd.Context(), deps.Client, cmd.ErrOrStderr(), id, waitTimeout)
				if err != nil {
					return err
				}
			}
			if inst.Connection == nil {
				return fmt.Errorf("instance %s is running but has no connection info — check the server logs for the supplier's connection details", id)
			}
			host := inst.Connection.Hostname
			port := inst.Connection.Port

			if printOnly {
				line := buildPrintLine(host, port, extraArgs)
				fmt.Fprintln(cmd.OutOrStdout(), line)
				return nil
			}
			return execSSH(host, port, extraArgs)
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "Print the equivalent ssh command instead of executing it")
	cmd.Flags().BoolVar(&wait, "wait", true, "Wait for instance to become running before SSH-ing")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 10*time.Minute, "How long to wait for status=running")
	return cmd
}

// buildPrintLine constructs the ssh command line for the --print mode.
// extraArgs are shell-quoted so the line is copy-paste safe.
func buildPrintLine(host string, port int, extraArgs []string) string {
	parts := []string{"ssh", "-p", fmt.Sprintf("%d", port)}
	parts = append(parts, sshOptionFlags...)
	parts = append(parts, fmt.Sprintf("root@%s", host))
	for _, a := range extraArgs {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote wraps a string in single quotes if it contains shell-special
// characters; otherwise returns unchanged. Single quotes inside the input
// are escaped via the `'\”` idiom.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			continue
		case r == '_' || r == '-' || r == '/' || r == '.' || r == ':' || r == '=':
			continue
		default:
			safe = false
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildSSHArgv returns the argv for syscall.Exec. Used by both the unix
// syscallExecImpl and instances_ssh_test.go (which compares against this
// helper to verify argv shape).
func buildSSHArgv(host string, port int, extraArgs []string) []string {
	argv := []string{"ssh", "-p", fmt.Sprintf("%d", port)}
	argv = append(argv, sshOptionFlags...)
	argv = append(argv, fmt.Sprintf("root@%s", host))
	argv = append(argv, extraArgs...)
	return argv
}

// waitForRunning polls GetInstance every 5 seconds until status=running,
// status enters a terminal failure state, or the deadline elapses.
// Surfaces status transitions to errOut so users see progress.
//
// a 10-minute default deadline is a safety net so
// CLI invocations can never infinite-loop on a stuck provider boot.
func waitForRunning(ctx context.Context, c *apiclient.Client, errOut io.Writer, id string, deadline time.Duration) (apiclient.Instance, error) {
	end := time.Now().Add(deadline)
	lastStatus := ""
	for time.Now().Before(end) {
		inst, err := c.GetInstance(ctx, id)
		if err != nil {
			return apiclient.Instance{}, mapClientError(err)
		}
		if inst.Status != lastStatus {
			fmt.Fprintf(errOut, "instance %s: %s\n", id, inst.Status)
			lastStatus = inst.Status
		}
		switch inst.Status {
		case "running":
			return inst, nil
		case "terminated", "error":
			return apiclient.Instance{}, fmt.Errorf("instance %s is %s — cannot SSH", id, inst.Status)
		}
		select {
		case <-ctx.Done():
			return apiclient.Instance{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return apiclient.Instance{}, fmt.Errorf("timed out waiting for instance %s to become running (last status=%s)", id, lastStatus)
}
