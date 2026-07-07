package cmd

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectRuntime(t *testing.T) {
	origHasGuix := hasGuix
	t.Cleanup(func() { hasGuix = origHasGuix })

	// root with manifest.scm present; a separate one without.
	withManifest := t.TempDir()
	if err := os.WriteFile(filepath.Join(withManifest, "manifest.scm"), []byte("()"), 0o600); err != nil {
		t.Fatal(err)
	}
	noManifest := t.TempDir()

	cases := []struct {
		name    string
		mode    string
		root    string
		guix    bool
		want    containerRuntime
		wantErr bool
	}{
		{"explicit guix", "guix", noManifest, false, runtimeGuix, false},
		{"explicit devcontainer", "devcontainer", withManifest, true, runtimeDevcontainer, false},
		{"unknown mode errors", "podman", withManifest, true, 0, true},
		{"auto: guix + manifest -> guix", "auto", withManifest, true, runtimeGuix, false},
		{"auto: guix + no manifest -> devcontainer", "auto", noManifest, true, runtimeDevcontainer, false},
		{"auto: no guix + manifest -> devcontainer", "auto", withManifest, false, runtimeDevcontainer, false},
		{"empty is auto", "", withManifest, true, runtimeGuix, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hasGuix = func() bool { return tc.guix }
			got, err := selectRuntime(tc.mode, tc.root)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got runtime %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("runtime = %v, want %v", got, tc.want)
			}
		})
	}
}

// parseMCP decodes the wraptool server entry from a written .mcp.json.
func parseMCP(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, b)
	}
	srv, ok := doc.MCPServers["wraptool"]
	if !ok {
		t.Fatalf("no wraptool server in config:\n%s", b)
	}
	return srv
}

func TestWriteMCPConfig_WithToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	if err := writeMCPConfig(path, "http://127.0.0.1:8717", "s3cr3t", "/home/dev/my proj"); err != nil {
		t.Fatal(err)
	}
	srv := parseMCP(t, path)

	if srv["type"] != "sse" {
		t.Errorf("type = %v, want sse", srv["type"])
	}
	url, _ := srv["url"].(string)
	if !strings.HasPrefix(url, "http://127.0.0.1:8717/sse?cwd=") {
		t.Errorf("url = %q, missing /sse?cwd= prefix", url)
	}
	if strings.Contains(url, " ") {
		t.Errorf("url not encoded (raw space): %q", url)
	}
	// cwd with a space must be percent/plus-encoded, and must round-trip.
	if !strings.Contains(url, "my") || strings.Contains(url, "my proj") {
		t.Errorf("cwd not encoded in url: %q", url)
	}
	headers, ok := srv["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers missing with token set: %v", srv)
	}
	if got := headers["Authorization"]; got != "Bearer s3cr3t" {
		t.Errorf("Authorization = %v, want Bearer s3cr3t", got)
	}
}

func TestWriteMCPConfig_NoToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	if err := writeMCPConfig(path, "http://127.0.0.1:8717/", "", "/repo"); err != nil {
		t.Fatal(err)
	}
	srv := parseMCP(t, path)
	if _, ok := srv["headers"]; ok {
		t.Errorf("headers present without a token: %v", srv)
	}
	if url, _ := srv["url"].(string); !strings.HasPrefix(url, "http://127.0.0.1:8717/sse?cwd=") {
		t.Errorf("trailing slash not trimmed: %q", url)
	}
}

func TestWriteMCPConfig_BacksUpExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	if err := os.WriteFile(path, []byte(`{"existing":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeMCPConfig(path, "http://127.0.0.1:8717", "", "/repo"); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(path + ".wraptool-bak")
	if err != nil {
		t.Fatalf("expected backup: %v", err)
	}
	if string(bak) != `{"existing":true}` {
		t.Errorf("backup content = %q", bak)
	}
}

// splitURL returns the scheme://host/path (query stripped) and the decoded cwd
// query param of an MCP server URL.
func splitURL(t *testing.T, raw string) (basePath, cwd string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing url %q: %v", raw, err)
	}
	return u.Scheme + "://" + u.Host + u.Path, u.Query().Get("cwd")
}

// TestWriteMCPConfigParityWithConnectSh asserts the Go writer (guix path) and
// the wraptool-connect feature's connect.sh (devcontainer path) produce the
// same MCP server entry for the same inputs. Parity is semantic, not byte:
// connect.sh minimally URL-encodes cwd while writeMCPConfig uses
// url.QueryEscape, so the encodings differ but must decode identically.
func TestWriteMCPConfigParityWithConnectSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh available")
	}
	connect := filepath.Join("..", "features", "src", "wraptool-connect", "connect.sh")
	if !fileExists(connect) {
		t.Skipf("connect.sh not found at %s", connect)
	}

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
			// Go writer.
			goPath := filepath.Join(t.TempDir(), ".mcp.json")
			if err := writeMCPConfig(goPath, base, tc.token, cwd); err != nil {
				t.Fatal(err)
			}
			goSrv := parseMCP(t, goPath)

			// connect.sh, fresh target (no jq/merge path needed).
			shPath := filepath.Join(t.TempDir(), ".mcp.json")
			cmd := exec.Command(sh, connect)
			cmd.Env = append(os.Environ(),
				"WT_URL="+base,
				"WRAPTOOL_TOKEN="+tc.token,
				"WRAPTOOL_CWD="+cwd,
				"WT_HARNESS=claude",
				"WT_CONFIG_OUT="+shPath,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("connect.sh failed: %v\n%s", err, out)
			}
			shSrv := parseMCP(t, shPath)

			// type parity.
			if goSrv["type"] != shSrv["type"] || goSrv["type"] != "sse" {
				t.Errorf("type mismatch: go=%v sh=%v", goSrv["type"], shSrv["type"])
			}

			// url: same base+path, and cwd decodes to the same value on both.
			goBase, goCwd := splitURL(t, goSrv["url"].(string))
			shBase, shCwd := splitURL(t, shSrv["url"].(string))
			if goBase != shBase || goBase != base+"/sse" {
				t.Errorf("base/path mismatch: go=%q sh=%q", goBase, shBase)
			}
			if goCwd != cwd || shCwd != cwd {
				t.Errorf("decoded cwd mismatch: go=%q sh=%q want=%q", goCwd, shCwd, cwd)
			}

			// headers: both present with matching Bearer, or both absent.
			goH, goHas := goSrv["headers"].(map[string]any)
			shH, shHas := shSrv["headers"].(map[string]any)
			if goHas != shHas {
				t.Fatalf("headers presence differs: go=%v sh=%v", goHas, shHas)
			}
			if goHas {
				if goH["Authorization"] != shH["Authorization"] {
					t.Errorf("Authorization mismatch: go=%v sh=%v", goH["Authorization"], shH["Authorization"])
				}
			}
		})
	}
}
