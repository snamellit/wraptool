package mcptools

import (
	"context"
	"fmt"
	"testing"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/introspect"
	"github.com/pti/wraptool/internal/whitelist"
)

// propType returns the JSON Schema "type" of a tool's flag-derived parameter.
func propType(t *testing.T, rt RegisteredTool, param string) string {
	t.Helper()
	p, ok := rt.Tool.InputSchema.Properties[param]
	if !ok {
		t.Fatalf("tool %q schema missing param %q; props=%v", rt.Name, param, keys(rt.Tool.InputSchema.Properties))
	}
	m, ok := p.(map[string]any)
	if !ok {
		t.Fatalf("param %q schema is %T, want map[string]any", param, p)
	}
	return fmt.Sprint(m["type"])
}

// TestBuildCLIToolsBooleanFlagTyping pins the flag-typing precedence:
// well-known boolean names default to boolean, but --help introspection that
// says a flag takes a value overrides that name heuristic.
func TestBuildCLIToolsBooleanFlagTyping(t *testing.T) {
	tc := &config.ToolConfig{
		Binary: "/nonexistent/tool",
		Allow: []config.AllowRule{
			{Subcommand: []string{"create"}, Flags: []string{"--yes", "--force"}},
		},
	}
	policy := whitelist.NewPolicy(map[string]*config.ToolConfig{"tool": tc})

	// No introspection: well-known names default to boolean (bare --flag).
	noIntro, err := buildCLITools("tool", tc, policy, nil, nil)
	if err != nil {
		t.Fatalf("buildCLITools: %v", err)
	}
	if got := propType(t, noIntro[0], "yes"); got != "boolean" {
		t.Errorf("--yes without introspection: type %q, want boolean", got)
	}
	if got := propType(t, noIntro[0], "force"); got != "boolean" {
		t.Errorf("--force without introspection: type %q, want boolean", got)
	}

	// Introspection says --force takes a string value (e.g. `tool create --force <x>`):
	// introspection is authoritative and must beat the well-known boolean name.
	schema := &introspect.Schema{
		Subcommands: map[string]introspect.Subcommand{
			"create": {Flags: map[string]introspect.Flag{
				"--force": {Type: "string", TakesValue: true, Description: "force target"},
			}},
		},
	}
	withIntro, err := buildCLITools("tool", tc, policy, schema, nil)
	if err != nil {
		t.Fatalf("buildCLITools: %v", err)
	}
	if got := propType(t, withIntro[0], "force"); got != "string" {
		t.Errorf("--force with value-taking introspection: type %q, want string (introspection should win)", got)
	}
	// --yes is absent from introspection, so it still falls back to the name heuristic.
	if got := propType(t, withIntro[0], "yes"); got != "boolean" {
		t.Errorf("--yes (not introspected): type %q, want boolean", got)
	}
}

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
