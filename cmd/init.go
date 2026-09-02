package cmd

import (
	"fmt"
	"os"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/harness"
	"github.com/pti/wraptool/internal/paths"

	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	var (
		transport  string
		url        string
		socketPath string
		harnesses  []string
		global     bool
		configPath string
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up wraptool for AI coding tools",
		Long: `Generate MCP server configuration for AI coding tools in the current project.

Supported harnesses:
  agy       - Antigravity CLI (.agents/mcp_config.json)
  claude    - Claude Code (.mcp.json)
  cursor    - Cursor IDE (.cursor/mcp.json)
  opencode  - OpenCode (opencode.json)
  gemini    - Gemini CLI (.gemini/settings.json)
  pi        - Pi coding agent, via pi-mcp-adapter (.pi/mcp.json)
  roo       - Roo Code (.roo/mcp.json)

Examples:
  # Set up for Claude Code and Cursor with SSE transport
  wraptool init --harness claude --harness cursor --url http://127.0.0.1:8717/sse

  # Set up for all harnesses with a Unix socket
  wraptool init --harness all --transport unix --socket /run/user/1000/wraptool.sock`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(harnesses, transport, url, socketPath, global, configPath)
		},
	}

	cmd.Flags().StringSliceVar(&harnesses, "harness", nil, "coding harness to configure (agy, claude, cursor, opencode, gemini, pi, roo, all)")
	cmd.Flags().StringVar(&transport, "transport", "sse", "transport type (sse, unix)")
	cmd.Flags().StringVar(&url, "url", "http://127.0.0.1:8717/sse", "SSE URL for the wraptool server")
	cmd.Flags().StringVar(&socketPath, "socket", "", "Unix socket path (for unix transport)")
	cmd.Flags().BoolVar(&global, "global", false, "write to user-level config instead of project-level")
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to config file (default: "+paths.DefaultConfigFile()+")")
	cmd.MarkFlagRequired("harness")

	cmd.AddCommand(newInitDevcontainerCmd())

	return cmd
}

func runInit(harnesses []string, transport, url, socketPath string, global bool, configPath string) error {
	wraptoolBin, err := os.Executable()
	if err != nil {
		wraptoolBin = "wraptool"
	}

	// Read the server's auth token so networked harness configs carry the
	// Authorization: Bearer header (agy/claude/… render it when a token is set).
	// Without this the generated config is header-less and 401s against an
	// auth-enabled server — which an sse server now always is, so scaffold the
	// config if absent (like serve and up do) and provision the default token,
	// rather than emit a config that cannot connect. init may run before the
	// server ever has, so both files must be created, not just named.
	cfgFile := configOrDefault(configPath)
	if _, err := config.EnsureConfigFile(cfgFile); err != nil {
		return err
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	if err := provisionDefaultMCPToken(cfg); err != nil {
		return err
	}
	token, err := readAuthToken(cfg)
	if err != nil {
		return err
	}

	p := harness.Params{
		Transport:   transport,
		URL:         url,
		SocketPath:  socketPath,
		WraptoolBin: wraptoolBin,
		ConfigFile:  cfgFile,
		Global:      global,
		Token:       token,
	}

	names := harness.Expand(harnesses)
	// "all" means "every harness this setup supports": with a token in play,
	// silently skip the auth-unverified ones instead of failing the whole run
	// on them — an explicitly named harness still gets the hard error from
	// harness.Prepare.
	if token != "" && len(names) > len(harnesses) {
		kept := names[:0]
		for _, h := range names {
			if harness.Registry[h].Auth == harness.AuthUnverified {
				fmt.Printf("  %s: skipped (auth support unverified; name it explicitly to see why)\n", h)
				continue
			}
			kept = append(kept, h)
		}
		names = kept
	}

	// Validate every harness before writing any, so a mid-list failure can't
	// leave a partial set of configs behind.
	plans := make([]*harness.Plan, 0, len(names))
	for _, h := range names {
		plan, err := harness.Prepare(h, p)
		if err != nil {
			return fmt.Errorf("generating config for %s: %w", h, err)
		}
		plans = append(plans, plan)
	}
	for i, plan := range plans {
		path, err := plan.Commit()
		if err != nil {
			return fmt.Errorf("writing config for %s: %w", names[i], err)
		}
		fmt.Printf("  %s: wrote %s\n", names[i], path)
	}
	return nil
}
