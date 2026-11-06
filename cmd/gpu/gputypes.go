package main

import (
	"github.com/spf13/cobra"

	"github.com/gpuai-dev/gpuai-oss/internal/cliprint"
)

// newGPUTypesCmd builds the `gpu gpu-types` command group. The list subcommand
// hits the public, unauthenticated /gpu-types catalog endpoint.
func newGPUTypesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gpu-types",
		Short: "List supported GPU types",
	}
	cmd.AddCommand(newGPUTypesListCmd())
	return cmd
}

func newGPUTypesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all GPU types",
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			types, err := deps.Client.ListGPUTypes(cmd.Context())
			if err != nil {
				return mapClientError(err)
			}
			return cliprint.GPUTypes(deps.Output, cmd.OutOrStdout(), types)
		},
	}
}
