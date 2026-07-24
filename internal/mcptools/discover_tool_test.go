package mcptools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrg/xdg"
	"github.com/pti/wraptool/internal/config"
)

// isolateCacheDir points the introspection cache at a temp dir so these tests
// neither read nor write the developer's real cache.
func isolateCacheDir(t *testing.T) {
	t.Helper()
	old, had := os.LookupEnv("XDG_CACHE_HOME")
	if err := os.Setenv("XDG_CACHE_HOME", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	xdg.Reload()
	t.Cleanup(func() {
		if had {
			os.Setenv("XDG_CACHE_HOME", old)
		} else {
			os.Unsetenv("XDG_CACHE_HOME")
		}
		xdg.Reload()
	})
}

// fakeTool writes an executable that prints a git-style subcommand listing.
func fakeTool(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fj")
	script := "#!/bin/sh\ncat <<'HELP'\nusage: fj <command>\n\n" +
		"Available commands:\n  pr        manage pull requests\n  issue     manage issues\nHELP\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// The bootstrap case: a tool with no allow rules registers no MCP tools, so
// discovery is the only way to learn what to name in a capability request.
func TestDiscoverInertToolAdvertisesCatalog(t *testing.T) {
	isolateCacheDir(t)

	d := discoverTool(context.Background(), "fj", &config.ToolConfig{Binary: fakeTool(t)})

	if len(d.Allowed) != 0 {
		t.Errorf("Allowed = %v, want empty for a tool with no allow rules", d.Allowed)
	}
	if len(d.Available) != 2 {
		t.Fatalf("Available = %v, want pr and issue", d.Available)
	}
	if got := d.Available["pr"]; got != "manage pull requests" {
		t.Errorf("pr = %q, want %q", got, "manage pull requests")
	}
	if d.Note == "" {
		t.Error("inert tool carries no note explaining nothing is callable yet")
	}
}

// An allowed subcommand must be reported under the MCP tool name the caller
// actually invokes, and the catalog still lists what policy has not permitted.
func TestDiscoverReportsAllowedAndUnpermitted(t *testing.T) {
	isolateCacheDir(t)

	d := discoverTool(context.Background(), "fj", &config.ToolConfig{
		Binary: fakeTool(t),
		Allow:  []config.AllowRule{{Subcommand: []string{"pr"}}},
	})

	if len(d.Allowed) != 1 || d.Allowed[0] != "pr" {
		t.Errorf("Allowed = %v, want [pr]", d.Allowed)
	}
	if len(d.Tools) != 1 || d.Tools[0] != ToolName("fj", []string{"pr"}) {
		t.Errorf("Tools = %v, want the registered MCP tool name", d.Tools)
	}
	if _, ok := d.Available["pr"]; ok {
		t.Errorf("Available = %v, permitted pr should be excluded", d.Available)
	}
	// `issue` has no allow rule; it must still be visible as requestable.
	if _, ok := d.Available["issue"]; !ok {
		t.Errorf("Available = %v, want it to still list the unpermitted issue", d.Available)
	}
	if d.Note != "" {
		t.Errorf("Note = %q, want none when something is callable", d.Note)
	}
}

// A tool whose binary is gone must report the failure rather than looking like
// a tool that simply advertises nothing.
func TestDiscoverSurfacesIntrospectionFailure(t *testing.T) {
	isolateCacheDir(t)

	d := discoverTool(context.Background(), "fj", &config.ToolConfig{
		Binary: filepath.Join(t.TempDir(), "does-not-exist"),
	})

	if d.Note == "" {
		t.Error("missing binary produced no note")
	}
	if len(d.Available) != 0 {
		t.Errorf("Available = %v, want empty when introspection failed", d.Available)
	}
}
