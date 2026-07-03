package mcptools

import (
	"context"
	"testing"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/whitelist"
)

// TestCLISourceTools guards the Source seam: cliSource must build one MCP tool
// per allow-rule, with the expected namespaced names and flag-derived schema
// parameters — the behaviour the registration path relied on before the refactor.
func TestCLISourceTools(t *testing.T) {
	cfg := &config.Config{
		Tools: map[string]config.ToolConfig{
			"git": {
				Binary: "/nonexistent/git", // introspection fails → config-only metadata
				Allow: []config.AllowRule{
					{Subcommand: []string{"status"}, Flags: []string{"--short"}},
					{Subcommand: []string{"log"}, Flags: []string{"--oneline"}},
				},
			},
		},
	}

	pointers := make(map[string]*config.ToolConfig, len(cfg.Tools))
	for name, tc := range cfg.Tools {
		pointers[name] = &tc
	}
	policy := whitelist.NewPolicy(pointers)

	src := NewCLISource(cfg, policy, nil)
	tools, err := src.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools() error: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}

	byName := make(map[string]RegisteredTool, len(tools))
	for _, rt := range tools {
		if rt.Handler == nil {
			t.Errorf("tool %q has nil handler", rt.Name)
		}
		if rt.Tool.Name != rt.Name {
			t.Errorf("RegisteredTool.Name %q != Tool.Name %q", rt.Name, rt.Tool.Name)
		}
		byName[rt.Name] = rt
	}

	cases := []struct {
		name  string
		param string // flag-derived schema property expected present
	}{
		{ToolName("git", []string{"status"}), "short"},
		{ToolName("git", []string{"log"}), "oneline"},
	}
	for _, c := range cases {
		rt, ok := byName[c.name]
		if !ok {
			t.Errorf("missing tool %q; got %v", c.name, keys(byName))
			continue
		}
		props := rt.Tool.InputSchema.Properties
		if _, ok := props[c.param]; !ok {
			t.Errorf("tool %q schema missing param %q; props=%v", c.name, c.param, keys(props))
		}
		if _, ok := props["args"]; !ok {
			t.Errorf("tool %q schema missing positional %q param", c.name, "args")
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
