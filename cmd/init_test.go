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

// TestInitNoConfigScaffoldsAndWritesHeader pins the first-run contract: run
// before any serve/up, init scaffolds the config (like they do), provisions
// the sse token, and the generated harness config carries the Authorization
// header — never a tokenless config that 401s against the server serve will
// start (PR #37 finding 1).
func TestInitNoConfigScaffoldsAndWritesHeader(t *testing.T) {
	withIsolatedConfigHome(t)
	dir := t.TempDir()
	t.Chdir(dir)

	cfgPath := filepath.Join(dir, "cfg", "config.yaml")
	if err := runInit([]string{"agy"}, "sse", "http://127.0.0.1:8717/sse", "", false, cfgPath); err != nil {
		t.Fatalf("runInit (no config): %v", err)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("config was not scaffolded: %v", err)
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
	if h := parsed.MCPServers["wraptool"].Headers["Authorization"]; !strings.HasPrefix(h, "Bearer ") {
		t.Errorf("expected a Bearer Authorization header on first-run init, got %v", parsed.MCPServers["wraptool"].Headers)
	}
}

// TestInitAllSkipsAuthUnverifiedAndIsAtomic covers PR #37 finding 2: with a
// token in play, --harness all skips the auth-unverified harnesses instead of
// dying on the first one mid-list, and nothing is written when any prepared
// harness fails validation (prepare-then-commit, no partial sets).
func TestInitAllSkipsAuthUnverifiedAndIsAtomic(t *testing.T) {
	withIsolatedConfigHome(t)
	dir := t.TempDir()
	t.Chdir(dir)

	cfgPath := filepath.Join(dir, "cfg", "config.yaml")
	// Succeeding at all is the skip check: an auth-unverified harness reaching
	// harness.Prepare with a token fails the whole run. (None is registered
	// since roo's removal; the guard stays for the next one.)
	if err := runInit([]string{"all"}, "sse", "http://127.0.0.1:8717/sse", "", false, cfgPath); err != nil {
		t.Fatalf("runInit --harness all: %v", err)
	}

	// Atomicity: make one harness's existing target unmergeable; NO config may
	// be (re)written, including the ones that would have preceded it.
	dir2 := t.TempDir()
	t.Chdir(dir2)
	if err := os.WriteFile(filepath.Join(dir2, ".mcp.json"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	err := runInit([]string{"all"}, "sse", "http://127.0.0.1:8717/sse", "", false, cfgPath)
	if err == nil {
		t.Fatal("expected refuse-to-clobber error from the claude target")
	}
	if _, statErr := os.Stat(filepath.Join(dir2, ".agents", "mcp_config.json")); !os.IsNotExist(statErr) {
		t.Errorf("agy config written despite a later harness failing validation (partial set)")
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
