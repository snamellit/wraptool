package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	cfg := "mcp:\n  listen: 127.0.0.1:8717\n  auth_token_file: " + tokenFile + "\n"
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

// TestInitProvisionsTokenForTokenlessConfig covers the upgrade path
// docs/security.qmd points at: an sse server is authenticated whether or not
// the operator configured a token, so `wraptool init` against a config with no
// auth_token_file must provision the default token and write the header —
// otherwise the documented remedy for a 401 generates another 401.
func TestInitProvisionsTokenForTokenlessConfig(t *testing.T) {
	configHome := withIsolatedConfigHome(t)
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("mcp:\n  listen: 127.0.0.1:8717\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Chdir(dir)

	if err := runInit([]string{"agy"}, "sse", "http://127.0.0.1:8717/sse", "", false, cfgFile); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	token, err := os.ReadFile(filepath.Join(configHome, "wraptool", "mcp-token"))
	if err != nil {
		t.Fatalf("init must provision the default token file: %v", err)
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
	if want := "Bearer " + strings.TrimSpace(string(token)); got != want {
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

// TestInitHelpNeverMentionsStdioTransport guards CLAUDE.md's hard rule: wraptool's
// own stdio MCP server transport is tests-only and must never be advertised on a
// user-visible surface (mirrors cmd/doctor_test.go's TestDoctorNeverMentionsStdioTransport).
// The flag must still ACCEPT "stdio" — only the advertisement is banned.
func TestInitHelpNeverMentionsStdioTransport(t *testing.T) {
	c := newInitCmd()
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs([]string{"--help"})
	if err := c.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "stdio") {
		t.Errorf("init --help must never advertise the stdio MCP transport, got: %s", out)
	}
}

// TestInitTransportFlagUsageNeverMentionsStdio guards the --transport flag's own
// usage string specifically, independent of the rest of --help's rendering.
func TestInitTransportFlagUsageNeverMentionsStdio(t *testing.T) {
	c := newInitCmd()
	f := c.Flags().Lookup("transport")
	if f == nil {
		t.Fatal("no --transport flag registered")
	}
	if strings.Contains(f.Usage, "stdio") {
		t.Errorf("--transport usage must never advertise stdio, got: %s", f.Usage)
	}
}
