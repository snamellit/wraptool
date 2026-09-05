package cmd

import (
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"runtime"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/webui"

	"github.com/spf13/cobra"
)

func newAdminCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Administration web UI helpers",
	}

	cmd.PersistentFlags().StringVarP(&configPath, "config", "c", "", "path to config file")

	cmd.AddCommand(newAdminOpenCmd(&configPath))

	return cmd
}

func newAdminOpenCmd(configPath *string) *cobra.Command {
	var printOnly bool

	cmd := &cobra.Command{
		Use:   "open",
		Short: "Open the admin web UI in a browser, already logged in",
		Long: `Open the admin web UI in the default browser, authenticated via a one-shot
?token= login link (the UI immediately swaps it for a session cookie and strips
the token from the URL). Reads admin.listen and admin.auth_token_file from the
config, so there is nothing to copy-paste.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(resolveConfigPath(configPath))
			if err != nil {
				return err
			}
			if cfg.Admin.Listen == "" {
				return fmt.Errorf("admin.listen is not set in %s: the admin UI is disabled", resolveConfigPath(configPath))
			}
			token, err := webui.LoadToken(cfg.Admin.AuthTokenFile)
			if err != nil {
				return err
			}
			loginURL := adminLoginURL(cfg.Admin.Listen, token)
			if printOnly {
				fmt.Println(loginURL)
				return nil
			}
			if err := openBrowser(loginURL); err != nil {
				return fmt.Errorf("opening browser: %w (use --print to get the URL instead)", err)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&printOnly, "print", false, "print the login URL instead of opening a browser")

	return cmd
}

// adminLoginURL builds the ?token= login link for an admin.listen address. A
// wildcard or empty host is dialled as 127.0.0.1 — the bind address says where
// the server listens, not where a local browser should connect.
func adminLoginURL(listen, token string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = listen, ""
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	u := url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(host, port),
		Path:     "/",
		RawQuery: url.Values{"token": {token}}.Encode(),
	}
	if port == "" {
		u.Host = host
	}
	return u.String()
}

// openBrowser launches the platform's URL opener detached; explicit argv, no
// shell (see CLAUDE.md).
func openBrowser(u string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", u)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		c = exec.Command("xdg-open", u)
	}
	return c.Start()
}
