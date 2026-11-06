package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
	"github.com/gpuai-dev/gpuai-oss/internal/cliprint"
)

// newKeysCmd builds the `gpu keys` command group with create, list, get and
// delete subcommands.
func newKeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage SSH keys",
	}
	cmd.AddCommand(newKeysCreateCmd(), newKeysListCmd(), newKeysGetCmd(), newKeysDeleteCmd())
	return cmd
}

func newKeysListCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your SSH keys",
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			page, err := deps.Client.ListSSHKeys(cmd.Context(), apiclient.ListSSHKeysOpts{Limit: limit})
			if err != nil {
				return mapClientError(err)
			}
			return cliprint.SSHKeys(deps.Output, cmd.OutOrStdout(), page.Data)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Max keys to fetch (1-200)")
	return cmd
}

func newKeysCreateCmd() *cobra.Command {
	var (
		name          string
		publicKeyFile string
		publicKey     string
	)
	cmd := &cobra.Command{
		Use:         "create",
		Short:       "Upload an SSH public key",
		Annotations: mutatingCmd,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if publicKey == "" && publicKeyFile == "" {
				return fmt.Errorf("provide --public-key or --public-key-file")
			}
			if publicKey == "" {
				b, err := os.ReadFile(publicKeyFile)
				if err != nil {
					return fmt.Errorf("read public key file: %w", err)
				}
				publicKey = string(b)
			}
			deps := depsFromCtx(cmd.Context())
			k, err := deps.Client.CreateSSHKey(cmd.Context(), name, publicKey)
			if err != nil {
				return mapClientError(err)
			}
			return cliprint.SSHKey(deps.Output, cmd.OutOrStdout(), k)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Display name for the key (required)")
	cmd.Flags().StringVar(&publicKeyFile, "public-key-file", "",
		"Path to a public key file (e.g., ~/.ssh/id_ed25519.pub)")
	cmd.Flags().StringVar(&publicKey, "public-key", "",
		"Raw public key string (alternative to --public-key-file)")
	return cmd
}

func newKeysGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key-id>",
		Short: "Show one SSH key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			k, err := deps.Client.GetSSHKey(cmd.Context(), args[0])
			if err != nil {
				return mapClientError(err)
			}
			return cliprint.SSHKey(deps.Output, cmd.OutOrStdout(), k)
		},
	}
}

func newKeysDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:         "delete <key-id>",
		Short:       "Delete an SSH key",
		Annotations: mutatingCmd,
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			if err := deps.Client.DeleteSSHKey(cmd.Context(), args[0]); err != nil {
				return mapClientError(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "SSH key %s deleted.\n", args[0])
			return nil
		},
	}
}
