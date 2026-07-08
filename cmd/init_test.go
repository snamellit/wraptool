package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestInitEmbedsAuthHeader is the regression test for the Antigravity 401: an
// auth-enabled server's token must flow into the generated agy config as an
// Authorization: Bearer header, without the user hand-editing it.
func TestInitEmbedsAuthHeader(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("sekret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	cfgFile := filepath.Join(dir, "config.yaml")
	cfg := "server:\n  listen: 127.0.0.1:8717\n  auth_token_file: " + tokenFile + "\n"
	if err := os.WriteFile(cfgFile, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Chdir(dir) // agy writes .agents/mcp_config.json relative to cwd

	if err := runInit([]string{"agy"}, "sse", "http://127.0.0.1:8717/sse", "", false, cfgFile); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".agents", "mcp_config.json"))
	if err != nil {
		t.Fatalf("read agy config: %v", err)
	}
	var parsed struct {
		MCPServers map[string]struct {
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse agy config: %v\n%s", err, data)
	}
	got := parsed.MCPServers["wraptool"].Headers["Authorization"]
	if want := "Bearer sekret"; got != want {
		t.Errorf("Authorization header = %q, want %q; config:\n%s", got, want, data)
	}
}

// TestInitNoConfigNoHeader confirms init stays localhost-friendly: with no config
// (no auth token) the generated config carries no Authorization header.
func TestInitNoConfigNoHeader(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	missing := filepath.Join(dir, "does-not-exist.yaml")
	if err := runInit([]string{"agy"}, "sse", "http://127.0.0.1:8717/sse", "", false, missing); err != nil {
		t.Fatalf("runInit (no config): %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".agents", "mcp_config.json"))
	if err != nil {
		t.Fatalf("read agy config: %v", err)
	}
	var parsed struct {
		MCPServers map[string]struct {
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse agy config: %v\n%s", err, data)
	}
	if h := parsed.MCPServers["wraptool"].Headers; len(h) != 0 {
		t.Errorf("expected no headers without a token, got %v", h)
	}
}
