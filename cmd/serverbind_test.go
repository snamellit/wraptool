package cmd

import (
	"testing"

	"github.com/pti/wraptool/internal/config"
)

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

	// guix: loopback left untouched, no token forced.
	gx := &config.Config{MCP: config.MCPConfig{Transport: "sse", Listen: "127.0.0.1:8717"}}
	if err := adjustServerBindForRuntime(gx, runtimeGuix); err != nil {
		t.Fatal(err)
	}
	if gx.MCP.Listen != "127.0.0.1:8717" || gx.MCP.AuthTokenFile != "" {
		t.Errorf("guix bind must be untouched, got listen=%q token=%q", gx.MCP.Listen, gx.MCP.AuthTokenFile)
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
