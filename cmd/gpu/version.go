package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newVersionCmd builds the `gpu version` subcommand. Prints the same
// metadata format as `gpu --version`.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the gpu CLI version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "gpu version %s (commit %s, built %s)\n", version, commit, date)
			return nil
		},
	}
}
