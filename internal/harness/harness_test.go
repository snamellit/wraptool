package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serverEntry renders a descriptor and returns the wraptool server map under the
// given top-level key ("mcpServers" or "mcp").
func serverEntry(t *testing.T, name, topKey string, p Params) (string, map[string]any) {
	t.Helper()
	d, ok := Registry[name]
	if !ok {
		t.Fatalf("no descriptor for %q", name)
	}
	path, content, err := d.Render(p)
	if err != nil {
		t.Fatalf("render %q: %v", name, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(content, &cfg); err != nil {
		t.Fatalf("parse %q json: %v\n%s", name, err, content)
	}
	servers, ok := cfg[topKey].(map[string]any)
	if !ok {
		t.Fatalf("%q: missing %q object in:\n%s", name, topKey, content)
	}
	srv, ok := servers["wraptool"].(map[string]any)
	if !ok {
		t.Fatalf("%q: missing wraptool server in:\n%s", name, content)
	}
	return path, srv
}

func TestRenderAgy(t *testing.T) {
	const url = "http://127.0.0.1:8717/sse"
	base := Params{URL: url, WraptoolBin: "/usr/local/bin/wraptool", ConfigFile: "/etc/wraptool.yaml"}

	t.Run("stdio", func(t *testing.T) {
		p := base
		p.Transport = "stdio"
		path, srv := serverEntry(t, "agy", "mcpServers", p)
		if path != ".agents/mcp_config.json" {
			t.Errorf("path = %q", path)
		}
		if srv["command"] != base.WraptoolBin {
			t.Errorf("command = %v", srv["command"])
		}
		args, ok := srv["args"].([]any)
		if !ok || len(args) != 3 || args[0] != "serve" || args[1] != "--config" || args[2] != base.ConfigFile {
			t.Errorf("args = %v", srv["args"])
		}
	})

	t.Run("sse project (cwd appended)", func(t *testing.T) {
		p := base
		p.Transport = "sse"
		_, srv := serverEntry(t, "agy", "mcpServers", p)
		u, _ := srv["serverUrl"].(string)
		if !strings.HasPrefix(u, url) {
			t.Errorf("serverUrl = %q, want prefix %q", u, url)
		}
		if !strings.Contains(u, "cwd=") {
			t.Errorf("serverUrl missing cwd=: %q", u)
		}
	})

	t.Run("sse global (no cwd, global path)", func(t *testing.T) {
		p := base
		p.Transport = "sse"
		p.Global = true
		path, srv := serverEntry(t, "agy", "mcpServers", p)
		home, _ := os.UserHomeDir()
		if want := filepath.Join(home, ".gemini", "antigravity-cli", "mcp_config.json"); path != want {
			t.Errorf("global path = %q, want %q", path, want)
		}
		if u, _ := srv["serverUrl"].(string); u != url {
			t.Errorf("global serverUrl = %q, want %q (no cwd)", u, url)
		}
	})
}

// TestRenderURLKeys pins the per-harness server-URL key variance (§2 of the
// design): the whole point of the registry is that these differ.
func TestRenderURLKeys(t *testing.T) {
	p := Params{Transport: "sse", URL: "http://127.0.0.1:8717/sse", Global: true} // global → stable URL, no cwd
	cases := map[string]struct {
		topKey string
		urlKey string
	}{
		"claude":   {"mcpServers", "url"},
		"cursor":   {"mcpServers", "url"},
		"gemini":   {"mcpServers", "httpUrl"},
		"windsurf": {"mcpServers", "serverUrl"},
		"agy":      {"mcpServers", "serverUrl"},
		"opencode": {"mcp", "url"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, srv := serverEntry(t, name, c.topKey, p)
			if _, ok := srv[c.urlKey].(string); !ok {
				t.Errorf("%s: expected url key %q in %v", name, c.urlKey, srv)
			}
		})
	}
}

func TestExpand(t *testing.T) {
	all := Expand([]string{"all"})
	found := false
	for _, h := range all {
		if h == "agy" {
			found = true
		}
	}
	if !found || len(all) != len(Names()) {
		t.Errorf("Expand(all) = %v", all)
	}

	explicit := Expand([]string{"agy", "claude"})
	if len(explicit) != 2 || explicit[0] != "agy" || explicit[1] != "claude" {
		t.Errorf("Expand explicit changed: %v", explicit)
	}
}

func TestWriteMergesPreservingOtherServers(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// Pre-existing config with an unrelated server.
	if err := os.WriteFile(".mcp.json", []byte(`{"mcpServers":{"other":{"url":"x"}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	// Project-level (Global:false) so it targets ./.mcp.json in the temp dir,
	// not the real ~/.claude.json.
	if _, err := Write("claude", Params{Transport: "sse", URL: "http://127.0.0.1:8717/sse"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(".mcp.json")
	var cfg struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.MCPServers["other"]; !ok {
		t.Errorf("merge dropped the pre-existing server: %s", b)
	}
	if _, ok := cfg.MCPServers["wraptool"]; !ok {
		t.Errorf("merge did not add wraptool: %s", b)
	}
}
