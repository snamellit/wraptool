package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pti/wraptool/internal/paths"

	"github.com/spf13/cobra"
)

var supportedHarnesses = []string{"claude", "cursor", "opencode", "gemini", "roo", "windsurf"}

func newInitCmd() *cobra.Command {
	var (
		transport string
		url       string
		socketPath string
		harnesses []string
		global    bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up wraptool for AI coding tools",
		Long: `Generate MCP server configuration for AI coding tools in the current project.

Supported harnesses:
  claude    - Claude Code (.mcp.json)
  cursor    - Cursor IDE (.cursor/mcp.json)
  opencode  - OpenCode (opencode.json)
  gemini    - Gemini CLI (.gemini/settings.json)
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

	cmd.Flags().StringSliceVar(&harnesses, "harness", nil, "coding harness to configure (claude, cursor, opencode, gemini, roo, windsurf, all)")
	cmd.Flags().StringVar(&transport, "transport", "sse", "transport type (stdio, sse, unix)")
	cmd.Flags().StringVar(&url, "url", "http://127.0.0.1:8717/sse", "SSE URL for the wraptool server")
	cmd.Flags().StringVar(&socketPath, "socket", "", "Unix socket path (for unix transport)")
	cmd.Flags().BoolVar(&global, "global", false, "write to user-level config instead of project-level")
	cmd.MarkFlagRequired("harness")

	return cmd
}

func runInit(harnesses []string, transport, url, socketPath string, global bool) error {
	// Expand "all".
	expanded := expandHarnesses(harnesses)

	// Find wraptool binary path.
	wraptoolBin, err := os.Executable()
	if err != nil {
		wraptoolBin = "wraptool"
	}

	configFile := paths.DefaultConfigFile()

	for _, h := range expanded {
		gen, ok := harnessGenerators[h]
		if !ok {
			return fmt.Errorf("unknown harness %q; supported: %v", h, supportedHarnesses)
		}

		path, content, err := gen(transport, url, socketPath, wraptoolBin, configFile, global)
		if err != nil {
			return fmt.Errorf("generating config for %s: %w", h, err)
		}

		if err := writeConfig(path, content, h); err != nil {
			return err
		}
	}

	return nil
}

func expandHarnesses(harnesses []string) []string {
	for _, h := range harnesses {
		if h == "all" {
			return supportedHarnesses
		}
	}
	return harnesses
}

type harnessGenerator func(transport, url, socketPath, wraptoolBin, configFile string, global bool) (path string, content []byte, err error)

var harnessGenerators = map[string]harnessGenerator{
	"claude":   generateClaude,
	"cursor":   generateCursor,
	"opencode": generateOpenCode,
	"gemini":   generateGemini,
	"roo":      generateRoo,
	"windsurf": generateWindsurf,
}

func generateClaude(transport, url, socketPath, wraptoolBin, configFile string, global bool) (string, []byte, error) {
	path := ".mcp.json"
	if global {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".claude.json")
	}

	server := make(map[string]interface{})
	switch transport {
	case "stdio":
		server["command"] = wraptoolBin
		server["args"] = []string{"serve", "--config", configFile}
	case "sse":
		server["type"] = "sse"
		server["url"] = url
	case "unix":
		server["type"] = "sse"
		server["url"] = "http://localhost/sse"
		// Claude Code doesn't natively support Unix sockets in MCP config;
		// document the socat bridge approach.
	}

	cfg := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"wraptool": server,
		},
	}

	return path, mustJSON(cfg), nil
}

func generateCursor(transport, url, socketPath, wraptoolBin, configFile string, global bool) (string, []byte, error) {
	path := ".cursor/mcp.json"
	if global {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".cursor", "mcp.json")
	}

	server := make(map[string]interface{})
	switch transport {
	case "stdio":
		server["command"] = wraptoolBin
		server["args"] = []string{"serve", "--config", configFile}
	case "sse", "unix":
		server["url"] = effectiveURL(transport, url, socketPath)
	}

	cfg := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"wraptool": server,
		},
	}

	return path, mustJSON(cfg), nil
}

func generateOpenCode(transport, url, socketPath, wraptoolBin, configFile string, global bool) (string, []byte, error) {
	path := "opencode.json"
	if global {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".config", "opencode", "config.json")
	}

	server := make(map[string]interface{})
	switch transport {
	case "stdio":
		server["type"] = "local"
		server["command"] = wraptoolBin
		server["args"] = []string{"serve", "--config", configFile}
	case "sse", "unix":
		server["type"] = "remote"
		server["url"] = effectiveURL(transport, url, socketPath)
	}
	server["enabled"] = true

	cfg := map[string]interface{}{
		"mcp": map[string]interface{}{
			"wraptool": server,
		},
	}

	return path, mustJSON(cfg), nil
}

func generateGemini(transport, url, socketPath, wraptoolBin, configFile string, global bool) (string, []byte, error) {
	path := ".gemini/settings.json"
	if global {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".gemini", "settings.json")
	}

	server := make(map[string]interface{})
	switch transport {
	case "stdio":
		server["command"] = wraptoolBin
		server["args"] = []string{"serve", "--config", configFile}
	case "sse", "unix":
		server["httpUrl"] = effectiveURL(transport, url, socketPath)
	}

	cfg := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"wraptool": server,
		},
	}

	return path, mustJSON(cfg), nil
}

func generateRoo(transport, url, socketPath, wraptoolBin, configFile string, global bool) (string, []byte, error) {
	path := ".roo/mcp.json"
	if global {
		return "", nil, fmt.Errorf("roo global config is managed via VS Code UI; use project-level instead")
	}

	server := make(map[string]interface{})
	switch transport {
	case "stdio":
		server["command"] = wraptoolBin
		server["args"] = []string{"serve", "--config", configFile}
	case "sse", "unix":
		server["type"] = "sse"
		server["url"] = effectiveURL(transport, url, socketPath)
	}

	cfg := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"wraptool": server,
		},
	}

	return path, mustJSON(cfg), nil
}

func generateWindsurf(transport, url, socketPath, wraptoolBin, configFile string, global bool) (string, []byte, error) {
	// Windsurf only supports global config.
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".codeium", "windsurf", "mcp_config.json")

	server := make(map[string]interface{})
	switch transport {
	case "stdio":
		server["command"] = wraptoolBin
		server["args"] = []string{"serve", "--config", configFile}
	case "sse", "unix":
		server["serverUrl"] = effectiveURL(transport, url, socketPath)
	}

	cfg := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"wraptool": server,
		},
	}

	return path, mustJSON(cfg), nil
}

func effectiveURL(transport, url, socketPath string) string {
	if transport == "unix" && socketPath != "" {
		// Most tools don't support Unix sockets natively.
		// Return the URL as-is; the user needs a socat bridge.
		return url
	}
	return url
}

func mustJSON(v interface{}) []byte {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(data, '\n')
}

func writeConfig(path string, content []byte, harness string) error {
	// Merge into existing config if the file exists.
	if existing, err := os.ReadFile(path); err == nil {
		merged, err := mergeJSONConfig(existing, content)
		if err == nil {
			content = merged
		}
		// If merge fails, fall through and write the new content.
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}

	if err := os.WriteFile(path, content, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	fmt.Printf("  %s: wrote %s\n", harness, path)
	return nil
}

// mergeJSONConfig merges the wraptool server entry into an existing config file,
// preserving other MCP servers and settings.
func mergeJSONConfig(existing, newContent []byte) ([]byte, error) {
	var existingMap, newMap map[string]interface{}
	if err := json.Unmarshal(existing, &existingMap); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(newContent, &newMap); err != nil {
		return nil, err
	}

	// Find the servers key (mcpServers or mcp).
	for key, newVal := range newMap {
		newServers, ok := newVal.(map[string]interface{})
		if !ok {
			continue
		}

		existingVal, exists := existingMap[key]
		if !exists {
			existingMap[key] = newVal
			continue
		}

		existingServers, ok := existingVal.(map[string]interface{})
		if !ok {
			existingMap[key] = newVal
			continue
		}

		// Merge: add/update wraptool entry, keep everything else.
		for name, server := range newServers {
			existingServers[name] = server
		}
		existingMap[key] = existingServers
	}

	return mustJSON(existingMap), nil
}
