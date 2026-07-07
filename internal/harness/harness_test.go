package harness

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
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

// TestAuthHeaderHarnesses: every AuthHeader harness emits an
// Authorization: Bearer header when a token is set, and none when it isn't.
func TestAuthHeaderHarnesses(t *testing.T) {
	topKey := map[string]string{
		"claude": "mcpServers", "cursor": "mcpServers", "gemini": "mcpServers",
		"windsurf": "mcpServers", "agy": "mcpServers", "opencode": "mcp",
		"pi": "mcpServers",
	}
	for name, d := range Registry {
		if d.Auth != AuthHeader {
			continue
		}
		tk, ok := topKey[name]
		if !ok {
			t.Fatalf("AuthHeader harness %q missing from topKey map", name)
		}
		t.Run(name, func(t *testing.T) {
			base := Params{Transport: "sse", URL: "http://127.0.0.1:8717/sse", Global: true}

			with := base
			with.Token = "s3cr3t"
			_, srv := serverEntry(t, name, tk, with)
			h, ok := srv["headers"].(map[string]any)
			if !ok || h["Authorization"] != "Bearer s3cr3t" {
				t.Errorf("%s with token: want Bearer header, got %v", name, srv["headers"])
			}

			_, srv = serverEntry(t, name, tk, base)
			if _, ok := srv["headers"]; ok {
				t.Errorf("%s no token: headers should be absent, got %v", name, srv["headers"])
			}
		})
	}
}

func TestWriteRejectsTokenForUnverifiedHarness(t *testing.T) {
	// roo is AuthUnverified (SSE header bug): a token (networked use) must fail loudly.
	if _, err := Write("roo", Params{Transport: "sse", URL: "http://x/sse", Token: "t", Dir: t.TempDir()}); err == nil {
		t.Fatal("expected error using an unverified harness with a token")
	}
	// claude is AuthHeader: a token is fine (write to a temp dir, not real home).
	if _, err := Write("claude", Params{Transport: "sse", URL: "http://x/sse", Token: "t", Dir: t.TempDir()}); err != nil {
		t.Fatalf("claude with token should succeed: %v", err)
	}
}

func TestWriteDirResolution(t *testing.T) {
	dir := t.TempDir()
	path, err := Write("claude", Params{Transport: "sse", URL: "http://127.0.0.1:8717/sse", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, ".mcp.json"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Errorf("config not written into Dir: %v", err)
	}
}

// TestClaudeParityWithConnectSh asserts the registry's claude output matches the
// wraptool-connect feature's connect.sh (the devcontainer path) for the same
// inputs. Semantic, not byte (connect.sh minimally URL-encodes cwd; the Go path
// uses url.QueryEscape) — they must decode identically. Retargeted from the old
// writeMCPConfig parity test.
func TestClaudeParityWithConnectSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh available")
	}
	connect := filepath.Join("..", "..", "features", "src", "wraptool-connect", "connect.sh")
	if _, err := os.Stat(connect); err != nil {
		t.Skipf("connect.sh not found at %s", connect)
	}
	connectAbs, _ := filepath.Abs(connect)

	const base = "http://127.0.0.1:8717"
	const cwd = "/home/dev/proj with space"

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"with token", "s3cr3t"},
		{"no token", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Registry (guix/container path): write into a temp workspace.
			goDir := t.TempDir()
			if _, err := Write("claude", Params{Transport: "sse", URL: base + "/sse", Token: tc.token, CWD: cwd, Dir: goDir}); err != nil {
				t.Fatal(err)
			}
			goSrv := readServer(t, filepath.Join(goDir, ".mcp.json"))

			// connect.sh (devcontainer path).
			shOut := filepath.Join(t.TempDir(), ".mcp.json")
			cmd := exec.Command(sh, connectAbs)
			cmd.Env = append(os.Environ(),
				"WT_URL="+base, "WRAPTOOL_TOKEN="+tc.token, "WRAPTOOL_CWD="+cwd,
				"WT_HARNESS=claude", "WT_CONFIG_OUT="+shOut,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("connect.sh failed: %v\n%s", err, out)
			}
			shSrv := readServer(t, shOut)

			if goSrv["type"] != "sse" || shSrv["type"] != "sse" {
				t.Errorf("type: go=%v sh=%v", goSrv["type"], shSrv["type"])
			}
			goBase, goCwd := splitURL(t, goSrv["url"].(string))
			shBase, shCwd := splitURL(t, shSrv["url"].(string))
			if goBase != shBase || goBase != base+"/sse" {
				t.Errorf("base/path: go=%q sh=%q", goBase, shBase)
			}
			if goCwd != cwd || shCwd != cwd {
				t.Errorf("decoded cwd: go=%q sh=%q want=%q", goCwd, shCwd, cwd)
			}
			gh, ghas := goSrv["headers"].(map[string]any)
			sh2, shas := shSrv["headers"].(map[string]any)
			if ghas != shas {
				t.Fatalf("headers presence: go=%v sh=%v", ghas, shas)
			}
			if ghas && gh["Authorization"] != sh2["Authorization"] {
				t.Errorf("Authorization: go=%v sh=%v", gh["Authorization"], sh2["Authorization"])
			}
		})
	}
}

func readServer(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("bad json %s: %v", path, err)
	}
	return doc.MCPServers["wraptool"]
}

func splitURL(t *testing.T, raw string) (basePath, cwd string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Scheme + "://" + u.Host + u.Path, u.Query().Get("cwd")
}
