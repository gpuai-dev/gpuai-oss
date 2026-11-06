package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/gpuai-dev/gpuai-oss/internal/cliprint"
)

// newOperationsCmd builds the `gpu operations` command group. The API exposes
// GET-one only, so `get` is the sole subcommand.
func newOperationsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "operations",
		Short: "Inspect async operations",
		Long: "Long-running actions (instance create and terminate) return an operation\n" +
			"id. Use these commands to check an operation's state, or to block until it settles.",
	}
	cmd.AddCommand(newOperationsGetCmd())
	return cmd
}

func newOperationsGetCmd() *cobra.Command {
	var waitTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "get <operation-id>",
		Short: "Get an operation's current state",
		Long: "Fetches an operation by id. By default it prints the current state once and exits 0,\n" +
			"whatever that state is. With --wait-timeout > 0 the CLI polls until the operation\n" +
			"reaches a terminal state (succeeded, failed, or cancelled) or the deadline elapses,\n" +
			"and exits non-zero when the operation failed or was cancelled — so a script can do\n" +
			"`gpu operations get op-123 --wait-timeout 10m && ...`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			// Bare fetch: report state, never fail on it.
			if waitTimeout == 0 {
				op, err := deps.Client.GetOperation(cmd.Context(), args[0])
				if err != nil {
					return mapClientError(err)
				}
				return cliprint.Operation(deps.Output, cmd.OutOrStdout(), op)
			}
			// PollOperation owns the cadence (2s ramping to 10s) and the
			// bounded deadline — the CLI adds no interval logic of its own.
			finalOp, err := deps.Client.PollOperation(cmd.Context(), args[0], waitTimeout)
			if err != nil {
				return mapClientError(err)
			}
			// Render before the failure return so the operation's details are
			// on stdout even when the command exits non-zero.
			if err := cliprint.Operation(deps.Output, cmd.OutOrStdout(), finalOp); err != nil {
				return err
			}
			switch finalOp.State {
			case "failed", "cancelled":
				detail := "no detail"
				if finalOp.Error != nil {
					detail = finalOp.Error.Detail
				}
				return fmt.Errorf("operation %s: %s", finalOp.State, detail)
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 0,
		"How long to poll the operation; 0 = fetch once, don't poll")
	return cmd
}
