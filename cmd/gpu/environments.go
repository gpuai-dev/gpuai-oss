package main

import (
	"github.com/spf13/cobra"

	"github.com/gpuai-dev/gpuai-oss/internal/cliprint"
)

// newEnvironmentsCmd builds `gpu environments` — the launch environment catalog:
// the certified framework roster with its version-picker options and the raw-VM
// OS catalog, i.e. every value the `--environment` flag on
// `gpu instances create` accepts. Public, unauthenticated (/v1/environments).
func newEnvironmentsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "environments",
		Aliases: []string{"envs"},
		Short:   "List launch environments (certified frameworks, versions, and raw-VM OS options)",
		Long: "List the launch environment catalog — the certified framework roster, the version\n" +
			"picker options for each framework, and the raw-VM OS choices. These are exactly the\n" +
			"values you can pass to `gpu instances create --environment`.",
		Example: "  # List frameworks, versions, and OS options\n" +
			"  gpu environments\n\n" +
			"  # JSON for scripting\n" +
			"  gpu environments --output json | jq '.frameworks'\n\n" +
			"  # Then launch with a chosen environment\n" +
			"  gpu instances create -t h100_sxm --environment certified:pytorch@2.13\n" +
			"  gpu instances create -t h100_sxm --environment raw-vm:ubuntu-24.04",
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			env, err := deps.Client.GetEnvironments(cmd.Context())
			if err != nil {
				return mapClientError(err)
			}
			return cliprint.Environments(deps.Output, cmd.OutOrStdout(), env)
		},
	}
}
