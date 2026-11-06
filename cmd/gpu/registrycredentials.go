package main

// gpu registry-credentials — stored private-registry logins for custom-image
// launches. The password is read from the
// GPUAI_REGISTRY_PASSWORD env var or an interactive prompt is refused (no
// --password flag: secrets on argv leak via shell history and ps).

import (
	"fmt"
	"os"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
	"github.com/gpuai-dev/gpuai-oss/internal/cliprint"
	"github.com/spf13/cobra"
)

func newRegistryCredentialsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "registry-credentials",
		Aliases: []string{"registry-credential"},
		Short:   "Manage private-registry logins for custom-image launches",
		Long: "Store a private container-registry login once, then launch private images with\n" +
			"`gpu instances create --image ... --registry-credential <id>`. Passwords are\n" +
			"encrypted at rest and never returned by any read.",
	}
	cmd.AddCommand(newRegistryCredentialsAddCmd(), newRegistryCredentialsListCmd(), newRegistryCredentialsRemoveCmd())
	return cmd
}

func newRegistryCredentialsAddCmd() *cobra.Command {
	var (
		name     string
		registry string
		username string
	)
	cmd := &cobra.Command{
		Use:         "add",
		Short:       "Store a private-registry login",
		Annotations: mutatingCmd,
		Long: "Stores a registry login for private image pulls. The password/token is read from\n" +
			"the GPUAI_REGISTRY_PASSWORD environment variable — never from a flag, so it can't\n" +
			"leak via shell history or the process list.",
		Example: "  GPUAI_REGISTRY_PASSWORD=ghp_xxx gpu registry-credentials add --name ghcr-acme --registry ghcr.io --username acme-bot",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" || registry == "" || username == "" {
				return fmt.Errorf("--name, --registry, and --username are required")
			}
			password := os.Getenv("GPUAI_REGISTRY_PASSWORD")
			if password == "" {
				return fmt.Errorf("set GPUAI_REGISTRY_PASSWORD to the registry password/token (not passed as a flag so it can't leak via shell history)")
			}
			deps := depsFromCtx(cmd.Context())
			cred, err := deps.Client.CreateRegistryCredential(cmd.Context(), apiclient.CreateRegistryCredentialRequest{
				Name: name, Registry: registry, Username: username, Password: password,
			})
			if err != nil {
				return mapClientError(err)
			}
			return cliprint.RegistryCredentialDetail(deps.Output, cmd.OutOrStdout(), cred)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Org-unique handle for this login (e.g. ghcr-acme) — required")
	cmd.Flags().StringVar(&registry, "registry", "", "Registry host the login belongs to (e.g. ghcr.io, docker.io) — required")
	cmd.Flags().StringVar(&username, "username", "", "Registry username — required")
	return cmd
}

func newRegistryCredentialsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List stored registry logins (passwords are never shown)",
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			creds, err := deps.Client.ListRegistryCredentials(cmd.Context())
			if err != nil {
				return mapClientError(err)
			}
			return cliprint.RegistryCredentialList(deps.Output, cmd.OutOrStdout(), creds)
		},
	}
}

func newRegistryCredentialsRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:         "remove <credential-id>",
		Short:       "Delete a stored registry login",
		Args:        cobra.ExactArgs(1),
		Annotations: mutatingCmd,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			if err := deps.Client.DeleteRegistryCredential(cmd.Context(), args[0]); err != nil {
				return mapClientError(err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "deleted "+args[0])
			return nil
		},
	}
}
