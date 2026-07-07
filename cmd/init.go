package cmd

import (
	"fmt"
	"os"

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
  windsurf  - Windsurf (~/.codeium/windsurf/mcp_config.json)

Examples:
  # Set up for Claude Code and Cursor with SSE transport
  wraptool init --harness claude --harness cursor --url http://127.0.0.1:8717/sse

  # Set up for all harnesses with a Unix socket
  wraptool init --harness all --transport unix --socket /run/user/1000/wraptool.sock

  # Set up with stdio transport (wraptool started by the harness)
  wraptool init --harness claude --transport stdio`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(harnesses, transport, url, socketPath, global)
		},
	}

	cmd.Flags().StringSliceVar(&harnesses, "harness", nil, "coding harness to configure (agy, claude, cursor, opencode, gemini, pi, roo, windsurf, all)")
	cmd.Flags().StringVar(&transport, "transport", "sse", "transport type (stdio, sse, unix)")
	cmd.Flags().StringVar(&url, "url", "http://127.0.0.1:8717/sse", "SSE URL for the wraptool server")
	cmd.Flags().StringVar(&socketPath, "socket", "", "Unix socket path (for unix transport)")
	cmd.Flags().BoolVar(&global, "global", false, "write to user-level config instead of project-level")
	cmd.MarkFlagRequired("harness")

	return cmd
}

func runInit(harnesses []string, transport, url, socketPath string, global bool) error {
	wraptoolBin, err := os.Executable()
	if err != nil {
		wraptoolBin = "wraptool"
	}

	p := harness.Params{
		Transport:   transport,
		URL:         url,
		SocketPath:  socketPath,
		WraptoolBin: wraptoolBin,
		ConfigFile:  paths.DefaultConfigFile(),
		Global:      global,
	}

	for _, h := range harness.Expand(harnesses) {
		path, err := harness.Write(h, p)
		if err != nil {
			return fmt.Errorf("generating config for %s: %w", h, err)
		}
		fmt.Printf("  %s: wrote %s\n", h, path)
	}
	return nil
}
