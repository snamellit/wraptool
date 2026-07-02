package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateAgy(t *testing.T) {
	wraptoolBin := "/usr/local/bin/wraptool"
	configFile := "/home/user/.config/wraptool/config.yaml"
	url := "http://127.0.0.1:8717/sse"

	t.Run("local stdio config", func(t *testing.T) {
		path, content, err := generateAgy("stdio", url, "", wraptoolBin, configFile, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if path != ".agents/mcp_config.json" {
			t.Errorf("expected path '.agents/mcp_config.json', got %q", path)
		}

		var cfg map[string]interface{}
		if err := json.Unmarshal(content, &cfg); err != nil {
			t.Fatalf("failed to parse generated JSON: %v", err)
		}

		mcpServers, ok := cfg["mcpServers"].(map[string]interface{})
		if !ok {
			t.Fatalf("missing or invalid mcpServers object")
		}

		wraptoolServer, ok := mcpServers["wraptool"].(map[string]interface{})
		if !ok {
			t.Fatalf("missing or invalid wraptool server object")
		}

		if wraptoolServer["command"] != wraptoolBin {
			t.Errorf("expected command %q, got %v", wraptoolBin, wraptoolServer["command"])
		}

		args, ok := wraptoolServer["args"].([]interface{})
		if !ok {
			t.Fatalf("missing or invalid args array")
		}

		if len(args) != 3 || args[0] != "serve" || args[1] != "--config" || args[2] != configFile {
			t.Errorf("unexpected args: %v", args)
		}
	})

	t.Run("local sse config", func(t *testing.T) {
		path, content, err := generateAgy("sse", url, "", wraptoolBin, configFile, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if path != ".agents/mcp_config.json" {
			t.Errorf("expected path '.agents/mcp_config.json', got %q", path)
		}

		var cfg map[string]interface{}
		if err := json.Unmarshal(content, &cfg); err != nil {
			t.Fatalf("failed to parse generated JSON: %v", err)
		}

		mcpServers, ok := cfg["mcpServers"].(map[string]interface{})
		if !ok {
			t.Fatalf("missing or invalid mcpServers object")
		}

		wraptoolServer, ok := mcpServers["wraptool"].(map[string]interface{})
		if !ok {
			t.Fatalf("missing or invalid wraptool server object")
		}

		serverURL, ok := wraptoolServer["serverUrl"].(string)
		if !ok {
			t.Fatalf("missing or invalid serverUrl string")
		}

		if !strings.HasPrefix(serverURL, url) {
			t.Errorf("expected serverUrl to start with %q, got %q", url, serverURL)
		}
		if !strings.Contains(serverURL, "cwd=") {
			t.Errorf("expected serverUrl to contain 'cwd=' parameter, got %q", serverURL)
		}
	})

	t.Run("global sse config", func(t *testing.T) {
		path, content, err := generateAgy("sse", url, "", wraptoolBin, configFile, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		home, _ := os.UserHomeDir()
		expectedPath := filepath.Join(home, ".gemini", "antigravity-cli", "mcp_config.json")
		if path != expectedPath {
			t.Errorf("expected global path %q, got %q", expectedPath, path)
		}

		var cfg map[string]interface{}
		if err := json.Unmarshal(content, &cfg); err != nil {
			t.Fatalf("failed to parse generated JSON: %v", err)
		}

		mcpServers, ok := cfg["mcpServers"].(map[string]interface{})
		if !ok {
			t.Fatalf("missing or invalid mcpServers object")
		}

		wraptoolServer, ok := mcpServers["wraptool"].(map[string]interface{})
		if !ok {
			t.Fatalf("missing or invalid wraptool server object")
		}

		serverURL, ok := wraptoolServer["serverUrl"].(string)
		if !ok {
			t.Fatalf("missing or invalid serverUrl string")
		}

		if serverURL != url {
			t.Errorf("expected serverUrl to be %q, got %q", url, serverURL)
		}
	})
}

func TestExpandHarnesses(t *testing.T) {
	expanded := expandHarnesses([]string{"all"})
	foundAgy := false
	for _, h := range expanded {
		if h == "agy" {
			foundAgy = true
			break
		}
	}
	if !foundAgy {
		t.Errorf("expected 'all' to expand and include 'agy', but it was not found in: %v", expanded)
	}

	explicit := expandHarnesses([]string{"agy", "claude"})
	if len(explicit) != 2 || explicit[0] != "agy" || explicit[1] != "claude" {
		t.Errorf("expected explicit array to remain unchanged, got: %v", explicit)
	}
}
