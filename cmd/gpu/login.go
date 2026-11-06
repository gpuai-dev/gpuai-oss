package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/gpuai-dev/gpuai-oss/internal/cliauth"
)

// newLoginCmd builds the `gpu login` subcommand. It stores an API key and
// the API base URL in ~/.config/gpu/credentials.json (mode 0600). Keys are
// minted on the server with `gpuctl apikey create --name <name>`.
func newLoginCmd() *cobra.Command {
	var apiKey string
	var fromStdin bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store an API key and API base URL",
		Long: "Saves an API key and the API base URL to ~/.config/gpu/credentials.json (mode 0600).\n\n" +
			"Create a key on the server with `gpuctl apikey create --name <name>`. The key is read from\n" +
			"--api-key, from stdin with --stdin, or from an interactive prompt. The API base comes from\n" +
			"--api-base, then GPUAI_API_BASE, then the default " + cliauth.DefaultAPIBase + ".",
		Example: "  gpu login --api-base http://localhost:8080/v1\n" +
			"  echo \"$KEY\" | gpu login --stdin --api-base https://gpu.example.com/v1",
		RunE: func(cmd *cobra.Command, args []string) error {
			apiBase, _ := cmd.Flags().GetString("api-base")
			if !cmd.Flags().Changed("api-base") {
				if v := os.Getenv("GPUAI_API_BASE"); v != "" {
					apiBase = v
				} else {
					apiBase = cliauth.DefaultAPIBase
				}
			}
			key := strings.TrimSpace(apiKey)
			if key == "" {
				var err error
				key, err = readAPIKey(cmd.InOrStdin(), cmd.ErrOrStderr(), fromStdin)
				if err != nil {
					return err
				}
			}
			if key == "" {
				return errors.New("no API key provided")
			}
			if err := cliauth.SaveCredentials(cliauth.Credentials{
				APIKey:   key,
				APIBase:  strings.TrimRight(apiBase, "/"),
				IssuedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved credentials for %s to %s\n", apiHost(apiBase), cliauth.CredentialsPath())
			return nil
		},
	}
	cmd.Flags().StringVar(&apiKey, "api-key", "", "API key to store (avoid on shared machines: it lands in shell history)")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "Read the API key from stdin")
	return cmd
}

// readAPIKey reads the key from stdin (fromStdin, or a non-TTY stdin) or
// prompts for it without echo on a terminal.
func readAPIKey(in io.Reader, prompt io.Writer, fromStdin bool) (string, error) {
	if f, ok := in.(*os.File); ok && !fromStdin && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(prompt, "API key: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(prompt)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
