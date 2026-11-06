package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/gpuai-dev/gpuai-oss/internal/cliauth"
)

// newLogoutCmd builds the `gpu logout` subcommand. Idempotent — removing an
// absent credentials file is not an error.
func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove stored credentials",
		Long:  "Deletes ~/.config/gpu/credentials.json. Idempotent — no error if file is absent.",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := cliauth.CredentialsPath()
			if err := cliauth.RemoveCredentials(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged out — credentials removed from %s\n", path)
			return nil
		},
	}
}
