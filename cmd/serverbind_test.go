package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/paths"
)

// withIsolatedConfigHome points XDG_CONFIG_HOME at a temp dir so a test that
// provisions the default token file never writes into the developer's real
// config dir; see withIsolatedXDG.
func withIsolatedConfigHome(t *testing.T) string {
	t.Helper()
	return withIsolatedXDG(t, "XDG_CONFIG_HOME")
}

// TestProvisionDefaultMCPToken covers the one decision shared by serve, up and
// init: an sse listener never runs (or gets a harness config written) without a
// token, and an operator's own choices are never overridden.
func TestProvisionDefaultMCPToken(t *testing.T) {
	withIsolatedConfigHome(t)

	// Unset + sse: the default path is chosen AND the file is created 0600 —
	// `init` can run before any server has, so a path alone is not enough.
	cfg := &config.Config{MCP: config.MCPConfig{Transport: "sse", Listen: "127.0.0.1:8717"}}
	if err := provisionDefaultMCPToken(cfg); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(paths.ConfigDir(), "mcp-token")
	if cfg.MCP.AuthTokenFile != want {
		t.Errorf("token file = %q, want %q", cfg.MCP.AuthTokenFile, want)
	}
	fi, err := os.Stat(want)
	if err != nil {
		t.Fatalf("token file must exist: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}
	first, err := os.ReadFile(want)
	if err != nil || len(first) == 0 {
		t.Fatalf("token file must hold a token: %q, %v", first, err)
	}

	// A second call is idempotent: the value a client was already handed must
	// not change under it (create-exclusive, not read-then-write).
	again := &config.Config{MCP: config.MCPConfig{Transport: "sse"}}
	if err := provisionDefaultMCPToken(again); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Error("an existing token was rewritten")
	}

	// A configured path is left alone, and no default file is invented for it.
	set := &config.Config{MCP: config.MCPConfig{Transport: "sse", AuthTokenFile: "/my/token"}}
	if err := provisionDefaultMCPToken(set); err != nil {
		t.Fatal(err)
	}
	if set.MCP.AuthTokenFile != "/my/token" {
		t.Errorf("operator token file was overridden: %q", set.MCP.AuthTokenFile)
	}

	// Non-sse transports keep their own auth story (unix is a mode-0600 socket).
	unix := &config.Config{MCP: config.MCPConfig{Transport: "unix", Listen: "/run/wraptool.sock"}}
	if err := provisionDefaultMCPToken(unix); err != nil {
		t.Fatal(err)
	}
	if unix.MCP.AuthTokenFile != "" {
		t.Errorf("unix transport must not be given a token: %q", unix.MCP.AuthTokenFile)
	}
}

// TestEnsureTokenFileRepairsEmptyFile covers the one case create-exclusive
// alone would leave broken: a file created by an earlier run that was
// interrupted before the write, which would otherwise fail the server at load
// with "auth token file is empty".
func TestEnsureTokenFileRepairsEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-token")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureTokenFile(path, "MCP"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		t.Fatalf("empty token file was not repaired: %q, %v", b, err)
	}
}

func TestServeOverridesFlags(t *testing.T) {
	// Fixed order, and only set fields are emitted.
	full := serveOverrides{
		mcpListen:          "0.0.0.0:8717",
		mcpAuthTokenFile:   "/t/mcp",
		adminListen:        "127.0.0.1:8718",
		adminAuthTokenFile: "/t/admin",
	}
	want := []string{
		"--listen", "0.0.0.0:8717",
		"--auth-token-file", "/t/mcp",
		"--admin-listen", "127.0.0.1:8718",
		"--admin-auth-token-file", "/t/admin",
	}
	// Fixed order by construction (the slice literal), so one check suffices.
	if got := full.flags(); !reflect.DeepEqual(got, want) {
		t.Fatalf("flags() = %v, want %v", got, want)
	}

	partial := serveOverrides{mcpListen: "127.0.0.1:8717"}
	if got := partial.flags(); !reflect.DeepEqual(got, []string{"--listen", "127.0.0.1:8717"}) {
		t.Errorf("partial flags() = %v", got)
	}
	if got := (serveOverrides{}).flags(); len(got) != 0 {
		t.Errorf("empty flags() = %v, want none", got)
	}
}

func TestServeOverridesApply(t *testing.T) {
	cfg := &config.Config{}
	ov := serveOverrides{
		mcpListen:          "0.0.0.0:8717",
		mcpAuthTokenFile:   "/t/mcp",
		adminListen:        "127.0.0.1:8718",
		adminAuthTokenFile: "/t/admin",
	}
	ov.apply(cfg)
	if cfg.MCP.Listen != "0.0.0.0:8717" || cfg.MCP.AuthTokenFile != "/t/mcp" {
		t.Errorf("MCP override not applied: %+v", cfg.MCP)
	}
	if cfg.Admin.Listen != "127.0.0.1:8718" || cfg.Admin.AuthTokenFile != "/t/admin" {
		t.Errorf("admin override not applied: %+v", cfg.Admin)
	}

	// Empty override fields must not clobber existing config.
	keep := &config.Config{MCP: config.MCPConfig{Listen: "keep"}}
	serveOverrides{}.apply(keep)
	if keep.MCP.Listen != "keep" {
		t.Errorf("empty override clobbered existing listen: %q", keep.MCP.Listen)
	}
}

func TestAdjustServerBindForRuntime(t *testing.T) {
	// devcontainer: loopback widened to 0.0.0.0, token defaulted in.
	dc := &config.Config{MCP: config.MCPConfig{Transport: "sse", Listen: "127.0.0.1:8717"}}
	if err := adjustServerBindForRuntime(dc, runtimeDevcontainer); err != nil {
		t.Fatal(err)
	}
	if dc.MCP.Listen != "0.0.0.0:8717" {
		t.Errorf("devcontainer bind = %q, want 0.0.0.0:8717", dc.MCP.Listen)
	}
	if dc.MCP.AuthTokenFile == "" {
		t.Error("devcontainer non-loopback bind must get a default token file")
	}

	// guix: loopback left untouched (host netns, no LAN exposure), but a token
	// is still provisioned — a loopback listener is reachable from the browser.
	gx := &config.Config{MCP: config.MCPConfig{Transport: "sse", Listen: "127.0.0.1:8717"}}
	if err := adjustServerBindForRuntime(gx, runtimeGuix); err != nil {
		t.Fatal(err)
	}
	if gx.MCP.Listen != "127.0.0.1:8717" {
		t.Errorf("guix bind must be untouched, got %q", gx.MCP.Listen)
	}
	if gx.MCP.AuthTokenFile == "" {
		t.Error("guix runtime must get a default token file")
	}

	// sbx: the bind stays loopback (D7); reachability is the proxy pinhole.
	sb := &config.Config{MCP: config.MCPConfig{Transport: "sse", Listen: "127.0.0.1:8717"}}
	if err := adjustServerBindForRuntime(sb, runtimeSbx); err != nil {
		t.Fatal(err)
	}
	if sb.MCP.Listen != "127.0.0.1:8717" {
		t.Errorf("sbx bind must be untouched, got %q", sb.MCP.Listen)
	}

	// An operator-configured token file is never overridden.
	set := &config.Config{MCP: config.MCPConfig{Transport: "sse", Listen: "127.0.0.1:8717", AuthTokenFile: "/my/token"}}
	if err := adjustServerBindForRuntime(set, runtimeGuix); err != nil {
		t.Fatal(err)
	}
	if set.MCP.AuthTokenFile != "/my/token" {
		t.Errorf("operator token file was overridden: %q", set.MCP.AuthTokenFile)
	}
}

func TestEnableAdminDefault(t *testing.T) {
	// Unconfigured: loopback + a token, never a wildcard.
	cfg := &config.Config{}
	enableAdminDefault(cfg)
	if !config.ListenIsLoopback(cfg.Admin.Listen) {
		t.Errorf("admin must default to a loopback bind, got %q", cfg.Admin.Listen)
	}
	if cfg.Admin.AuthTokenFile == "" {
		t.Error("admin must get a default token file")
	}

	// Operator-configured values are preserved.
	set := &config.Config{Admin: config.AdminConfig{Listen: "127.0.0.1:9000", AuthTokenFile: "/my/token"}}
	enableAdminDefault(set)
	if set.Admin.Listen != "127.0.0.1:9000" || set.Admin.AuthTokenFile != "/my/token" {
		t.Errorf("operator admin config was overridden: %+v", set.Admin)
	}
}
