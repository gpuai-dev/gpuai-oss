package main

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"
)

// execOpenBrowser is the injectable "open a URL in the default browser"
// function used by `gpu instances open`. Tests override it to capture the URL
// without spawning a real browser.
var execOpenBrowser func(url string) error = openBrowserImpl

// openBrowserImpl launches the platform's URL opener. Errors are surfaced to
// the caller, which has already printed the URL — so a headless box degrades
// to copy/paste instead of failing the command.
func openBrowserImpl(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

func newInstancesOpenCmd() *cobra.Command {
	var printOnly bool
	var terminal bool
	cmd := &cobra.Command{
		Use:   "open <instance-id>",
		Short: "Open a running instance's web app or web console in your browser",
		Long: "Opens the instance's app URL (its primary http port, set with --app-port at create " +
			"time) in your default browser. With --terminal, opens the instance's browser web " +
			"console instead: a shell on the instance with no SSH key setup. Both URLs are " +
			"protected by HTTP Basic Auth; the login is printed here.\n\n" +
			"App and console URLs are served through the server's optional tunnel. On a " +
			"deployment without it, instances have neither; use `gpu instances ssh`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			// This command prints the basic-auth login, so it opts in to the
			// credential read (?include=credentials).
			inst, err := deps.Client.GetInstanceWithCredentials(cmd.Context(), args[0])
			if err != nil {
				return mapClientError(err)
			}
			if inst.Connection == nil {
				return fmt.Errorf("instance %s has no connection info yet (status: %s) — wait for it to reach running", inst.ID, inst.Status)
			}

			out := cmd.OutOrStdout()
			var openURL string
			if terminal {
				if inst.Connection.TerminalURL == "" {
					return fmt.Errorf("instance %s has no web console (the server may not run the optional tunnel); use `gpu instances ssh %s` instead", inst.ID, inst.ID)
				}
				openURL = inst.Connection.TerminalURL
				fmt.Fprintf(out, "Console: %s\n", openURL)
			} else {
				if inst.Connection.AppURL == "" {
					return fmt.Errorf("instance %s has no app URL — only instances launched with --app-port serve one, and only when the server runs the optional tunnel; try `gpu instances open %s --terminal` for a browser shell, or `gpu instances ssh %s`", inst.ID, inst.ID, inst.ID)
				}
				openURL = inst.Connection.AppURL
				fmt.Fprintf(out, "App URL: %s\n", openURL)
			}
			if inst.Connection.AppUser != "" {
				fmt.Fprintf(out, "Login:   %s / %s\n", inst.Connection.AppUser, inst.Connection.AppPassword)
			} else {
				fmt.Fprintln(out, "Login:   not available for this instance")
			}
			if printOnly {
				return nil
			}
			if err := execOpenBrowser(openURL); err != nil {
				fmt.Fprintf(out, "(couldn't launch a browser: %v — open the URL above manually)\n", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "Print the URL and login without launching a browser")
	cmd.Flags().BoolVar(&terminal, "terminal", false, "Open the browser web console (a shell on the instance) instead of the app")
	return cmd
}
