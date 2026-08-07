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

	// Antigravity speaks Streamable HTTP, so agy's serverUrl targets /mcp, not the
	// SSE /sse the base URL carries. See renderAgy / newStreamableServer.
	const streamURL = "http://127.0.0.1:8717/mcp"

	t.Run("sse project (streamable /mcp, cwd appended)", func(t *testing.T) {
		p := base
		p.Transport = "sse"
		_, srv := serverEntry(t, "agy", "mcpServers", p)
		u, _ := srv["serverUrl"].(string)
		if !strings.HasPrefix(u, streamURL) {
			t.Errorf("serverUrl = %q, want prefix %q", u, streamURL)
		}
		if !strings.Contains(u, "cwd=") {
			t.Errorf("serverUrl missing cwd=: %q", u)
		}
	})

	t.Run("sse global (streamable /mcp, no cwd, global path)", func(t *testing.T) {
		p := base
		p.Transport = "sse"
		p.Global = true
		path, srv := serverEntry(t, "agy", "mcpServers", p)
		home, _ := os.UserHomeDir()
		if want := filepath.Join(home, ".gemini", "antigravity-cli", "mcp_config.json"); path != want {
			t.Errorf("global path = %q, want %q", path, want)
		}
		if u, _ := srv["serverUrl"].(string); u != streamURL {
			t.Errorf("global serverUrl = %q, want %q (no cwd)", u, streamURL)
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
		"agy": "mcpServers", "opencode": "mcp", "pi": "mcpServers",
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

// TestHarnessParityWithConnectSh asserts the registry's output matches the
// wraptool feature's connect.sh (the devcontainer path) for every
// container-CLI harness. Comparison is semantic, not byte (connect.sh minimally
// URL-encodes cwd; the Go path uses url.QueryEscape) — they must decode
// identically. Host IDEs (cursor, roo) run host-side and are not in connect.sh.
func TestHarnessParityWithConnectSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh available")
	}
	connect := filepath.Join("..", "..", "features", "src", "wraptool", "connect.sh")
	if _, err := os.Stat(connect); err != nil {
		t.Skipf("connect.sh not found at %s", connect)
	}
	connectAbs, _ := filepath.Abs(connect)

	const base = "http://127.0.0.1:8717"
	const cwd = "/home/dev/proj with space"

	harnesses := []struct {
		name     string
		topKey   string // top-level object holding the "wraptool" entry
		urlKey   string // field carrying the server URL
		wantType string // expected "type" value; "" = no type field
		endpoint string // path the harness connects on (/sse or /mcp)
		relPath  string // config file, relative to the workspace
	}{
		{"claude", "mcpServers", "url", "sse", "/sse", ".mcp.json"},
		{"agy", "mcpServers", "serverUrl", "", "/mcp", ".agents/mcp_config.json"},
		{"opencode", "mcp", "url", "remote", "/sse", "opencode.json"},
		{"gemini", "mcpServers", "httpUrl", "", "/sse", ".gemini/settings.json"},
		{"pi", "mcpServers", "url", "", "/sse", ".pi/mcp.json"},
	}

	for _, h := range harnesses {
		for _, tc := range []struct {
			name  string
			token string
		}{
			{"with token", "s3cr3t"},
			{"no token", ""},
		} {
			t.Run(h.name+"/"+tc.name, func(t *testing.T) {
				// Registry (guix/container path): write into a temp workspace.
				goDir := t.TempDir()
				if _, err := Write(h.name, Params{Transport: "sse", URL: base + "/sse", Token: tc.token, CWD: cwd, Dir: goDir}); err != nil {
					t.Fatal(err)
				}
				goSrv := readServer(t, filepath.Join(goDir, filepath.FromSlash(h.relPath)), h.topKey)

				// connect.sh (devcontainer path). Stub the harness binary onto PATH:
				// connect.sh's postCreate check (§12.3) hard-fails when WT_HARNESS
				// doesn't resolve and WT_CONNECT defaults to true, but this test only
				// cares about the config it writes, not a real harness install.
				stubDir := t.TempDir()
				if err := os.WriteFile(filepath.Join(stubDir, h.name), []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				shOut := filepath.Join(t.TempDir(), filepath.FromSlash(h.relPath))
				cmd := exec.Command(sh, connectAbs)
				cmd.Env = append(os.Environ(),
					"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
					"WT_URL="+base, "WRAPTOOL_TOKEN="+tc.token, "WRAPTOOL_CWD="+cwd,
					"WT_HARNESS="+h.name, "WT_CONFIG_OUT="+shOut,
					// Isolate from a host-installed options.env (present when the
					// wraptool feature is installed on the test machine), which would
					// otherwise clobber WT_URL above.
					"WT_SHARE_DIR="+t.TempDir(),
				)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("connect.sh failed: %v\n%s", err, out)
				}
				shSrv := readServer(t, shOut, h.topKey)

				if h.wantType == "" {
					if _, ok := goSrv["type"]; ok {
						t.Errorf("go %s: unexpected type field %v", h.name, goSrv["type"])
					}
					if _, ok := shSrv["type"]; ok {
						t.Errorf("sh %s: unexpected type field %v", h.name, shSrv["type"])
					}
				} else if goSrv["type"] != h.wantType || shSrv["type"] != h.wantType {
					t.Errorf("type: go=%v sh=%v want %q", goSrv["type"], shSrv["type"], h.wantType)
				}

				goURL, _ := goSrv[h.urlKey].(string)
				shURL, _ := shSrv[h.urlKey].(string)
				goBase, goCwd := splitURL(t, goURL)
				shBase, shCwd := splitURL(t, shURL)
				if goBase != shBase || goBase != base+h.endpoint {
					t.Errorf("%s base/path: go=%q sh=%q want %q", h.urlKey, goBase, shBase, base+h.endpoint)
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
}

func readServer(t *testing.T, path, topKey string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("bad json %s: %v", path, err)
	}
	return doc[topKey]["wraptool"]
}

func splitURL(t *testing.T, raw string) (basePath, cwd string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Scheme + "://" + u.Host + u.Path, u.Query().Get("cwd")
}

// --- pool-source-wiring: PoolLockable / PoolPackageID (review round 2, minor) ---

// TestPoolPackageID pins Descriptor.PoolPackageID's per-source dispatch:
// PoolPackage verbatim for an npm-sourced harness, the harness's own Name
// for an archive-sourced one.
func TestPoolPackageID(t *testing.T) {
	agy, ok := Registry["agy"]
	if !ok {
		t.Fatal("no agy descriptor")
	}
	if got := agy.PoolPackageID(); got != "agy" {
		t.Errorf("agy.PoolPackageID() = %q, want %q", got, "agy")
	}

	claude, ok := Registry["claude"]
	if !ok {
		t.Fatal("no claude descriptor")
	}
	if got := claude.PoolPackageID(); got != "@anthropic-ai/claude-code" {
		t.Errorf("claude.PoolPackageID() = %q, want %q", got, "@anthropic-ai/claude-code")
	}
}

// TestPoolLockable pins Descriptor.PoolLockable(): true for both an
// npm-sourced (claude) and an archive-sourced (agy) descriptor, false for a
// harness with neither (gemini).
func TestPoolLockable(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"agy", true},
		{"claude", true},
		{"gemini", false},
	}
	for _, tc := range cases {
		d, ok := Registry[tc.name]
		if !ok {
			t.Fatalf("no descriptor for %q", tc.name)
		}
		if got := d.PoolLockable(); got != tc.want {
			t.Errorf("Registry[%q].PoolLockable() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRegistryNeverSetsBothPoolPackageAndPoolArchive is a registry-wide
// invariant check: PoolPackage and PoolArchive are documented as mutually
// exclusive (each Descriptor field's own doc comment) — a descriptor
// setting both would make PoolPackageID's dispatch order silently pick one
// over the other. Guards against a future harness's descriptor entry
// accidentally setting both.
func TestRegistryNeverSetsBothPoolPackageAndPoolArchive(t *testing.T) {
	for name, d := range Registry {
		if d.PoolPackage != "" && d.PoolArchive {
			t.Errorf("Registry[%q] sets both PoolPackage (%q) and PoolArchive (true); they are mutually exclusive", name, d.PoolPackage)
		}
	}
}

// TestPoolLockableDescriptorsHaveNameEqualBin is a registry-wide invariant
// check (wave2-integration-fixes item 6): every pool-lockable descriptor's
// Name must equal its Bin. Two independent consumers assume this without
// re-deriving it — cmd/harness.go's dispatcher/orchestrator calls, which key
// the pool by the harness name (map key into Registry, e.g. "claude") but
// pass d.Bin as the generation's executable name (RealizeGeneration's
// binName), and cmd/guixcontainer.go's guixPoolPlanInput.Harness (also the
// registry key), which resolveGuixPoolPlan uses to build BOTH the dispatcher
// path (RuntimeBinDir/Harness) and the generation's own entrypoint path
// (RuntimeGenerationsDir/<genID>/bin/Harness) — the latter must match
// exactly what RealizeGeneration wrote under d.Bin, or the guix-plan probe
// silently falls back to legacy on every pool-lockable harness.
func TestPoolLockableDescriptorsHaveNameEqualBin(t *testing.T) {
	for name, d := range Registry {
		if !d.PoolLockable() {
			continue
		}
		if d.Name != d.Bin {
			t.Errorf("Registry[%q]: Name = %q, Bin = %q — a pool-lockable descriptor's dispatcher/guix-plan lookups assume these are equal", name, d.Name, d.Bin)
		}
	}
}
