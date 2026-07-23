package cmd

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/mcptools"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// fakeSource is a test double for an mcpproxy.Source: it produces a
// configurable tool list, records lifecycle calls, and can fire its
// onToolsChanged callback on demand (to simulate a list_changed / respawn).
type fakeSource struct {
	name string

	mu             sync.Mutex
	tools          []string
	toolsErr       error // when set, Tools returns it (after "spawning")
	toolsCalls     int
	closed         int
	setConfigCalls int
	onToolsChanged func()
}

func (f *fakeSource) Tools(ctx context.Context) ([]mcptools.RegisteredTool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolsCalls++
	if f.toolsErr != nil {
		return nil, f.toolsErr
	}
	var out []mcptools.RegisteredTool
	for _, t := range f.tools {
		name := t
		out = append(out, mcptools.RegisteredTool{
			Name: name,
			Tool: mcp.NewTool(name),
			Handler: func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("ok"), nil
			},
		})
	}
	return out, nil
}

func (f *fakeSource) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeSource) OnToolsChanged(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onToolsChanged = fn
}

func (f *fakeSource) SetConfig(*config.MCPServerConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setConfigCalls++
}

func (f *fakeSource) setTools(tools ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools = tools
}

func (f *fakeSource) setToolsErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolsErr = err
}

func (f *fakeSource) fire() {
	f.mu.Lock()
	fn := f.onToolsChanged
	f.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// newTestRegistrar builds a registrar whose MCP sources are fakeSources,
// captured by name so tests can drive them. No child process is ever spawned.
func newTestRegistrar(t *testing.T) (*registrar, map[string]*fakeSource) {
	t.Helper()
	s := server.NewMCPServer("wraptool-test", "0.0.0", server.WithToolCapabilities(true))
	reg := &registrar{
		server:    s,
		policy:    whitelist.NewPolicy(nil),
		mcpPolicy: whitelist.NewMCPPolicy(nil),
		mcp:       map[string]*mcpSourceEntry{},
	}
	fakes := map[string]*fakeSource{}
	reg.newMCPSource = func(name string, cfg *config.MCPServerConfig) mcpSource {
		f := &fakeSource{name: name, tools: []string{name + "_tool"}}
		fakes[name] = f
		return f
	}
	return reg, fakes
}

func serverCfg(servers ...string) *config.Config {
	cfg := &config.Config{MCPServers: map[string]config.MCPServerConfig{}}
	for _, name := range servers {
		cfg.MCPServers[name] = config.MCPServerConfig{
			Command:    "/bin/true",
			NamePrefix: name,
			Allow:      []config.MCPAllowRule{{Tool: "tool"}},
		}
	}
	return cfg
}

func TestReconcileUnchangedLeavesChildAlone(t *testing.T) {
	reg, fakes := newTestRegistrar(t)
	if err := reg.start(serverCfg("alpha")); err != nil {
		t.Fatalf("start: %v", err)
	}
	src := fakes["alpha"]
	callsAtStart := src.toolsCalls

	// Reload with an identical config entry.
	reg.reload(serverCfg("alpha"))

	if reg.mcp["alpha"].source != src {
		t.Errorf("unchanged server was replaced with a new source instance")
	}
	if src.closed != 0 {
		t.Errorf("unchanged server's child was closed (%d times)", src.closed)
	}
	if src.toolsCalls != callsAtStart {
		t.Errorf("unchanged server was re-listed (%d extra Tools calls)", src.toolsCalls-callsAtStart)
	}
}

func TestReconcileSpawnChangeRestartsChild(t *testing.T) {
	reg, fakes := newTestRegistrar(t)
	if err := reg.start(serverCfg("alpha")); err != nil {
		t.Fatalf("start: %v", err)
	}
	old := fakes["alpha"]

	// Change a spawn parameter (args) → the child must restart.
	changed := serverCfg("alpha")
	entry := changed.MCPServers["alpha"]
	entry.Args = []string{"--flag"}
	changed.MCPServers["alpha"] = entry
	reg.reload(changed)

	if old.closed != 1 {
		t.Errorf("spawn change should have closed the old child once, got %d", old.closed)
	}
	if reg.mcp["alpha"].source == old {
		t.Errorf("spawn change should have created a new source instance")
	}
}

func TestReconcilePolicyChangeKeepsChild(t *testing.T) {
	reg, fakes := newTestRegistrar(t)
	if err := reg.start(serverCfg("alpha")); err != nil {
		t.Fatalf("start: %v", err)
	}
	src := fakes["alpha"]
	src.setTools("alpha_tool", "alpha_extra") // newly-allowed tool appears on re-list

	// Change only the allow-list → same child, refreshed registrations.
	changed := serverCfg("alpha")
	entry := changed.MCPServers["alpha"]
	entry.Allow = []config.MCPAllowRule{{Tool: "tool"}, {Tool: "extra"}}
	changed.MCPServers["alpha"] = entry
	reg.reload(changed)

	if reg.mcp["alpha"].source != src {
		t.Errorf("policy-only change should NOT restart the child")
	}
	if src.closed != 0 {
		t.Errorf("policy-only change closed the child (%d times)", src.closed)
	}
	if src.setConfigCalls == 0 {
		t.Errorf("policy-only change should push the new config to the source")
	}
	got := reg.mcp["alpha"].registered
	if len(got) != 2 {
		t.Errorf("expected the newly-allowed tool to be registered, got %v", got)
	}
}

func TestReconcileAddAndRemove(t *testing.T) {
	reg, fakes := newTestRegistrar(t)
	if err := reg.start(serverCfg("alpha")); err != nil {
		t.Fatalf("start: %v", err)
	}
	alpha := fakes["alpha"]

	// Drop alpha, add beta.
	reg.reload(serverCfg("beta"))

	if alpha.closed != 1 {
		t.Errorf("removed server's child should be closed once, got %d", alpha.closed)
	}
	if _, ok := reg.mcp["alpha"]; ok {
		t.Errorf("removed server is still tracked")
	}
	if _, ok := reg.mcp["beta"]; !ok {
		t.Errorf("added server was not registered")
	}
	if fakes["beta"] == nil {
		t.Errorf("added server's source was never built")
	}
}

func TestCLIToolNamesFollowReload(t *testing.T) {
	reg, _ := newTestRegistrar(t)
	if err := reg.start(serverCfg()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := reg.CLIToolNames(); len(got) != 0 {
		t.Fatalf("initial CLI tools = %v, want none", got)
	}

	updated := serverCfg()
	updated.Tools = map[string]config.ToolConfig{
		"fj": {
			Binary: "/nonexistent/fj",
			Allow:  []config.AllowRule{{Subcommand: []string{"pr", "create"}}},
		},
	}
	reg.reload(updated)
	if got := reg.CLIToolNames(); len(got) != 1 || got[0] != "fj" {
		t.Errorf("CLI tools after reload = %v, want [fj]", got)
	}
}

func TestAddClosesSourceWhenRegistrationFails(t *testing.T) {
	reg, fakes := newTestRegistrar(t)
	// A source whose Tools() errors after "spawning" its child. addMCPLocked
	// must close it rather than leak the (never-tracked) child.
	reg.newMCPSource = func(name string, cfg *config.MCPServerConfig) mcpSource {
		f := &fakeSource{name: name, toolsErr: errors.New("spawn/list failed")}
		fakes[name] = f
		return f
	}

	err := reg.start(serverCfg("alpha"))
	if err == nil {
		t.Fatal("start should surface the registration error")
	}
	if got := fakes["alpha"].closed; got != 1 {
		t.Errorf("failed source should be closed exactly once, got %d", got)
	}
	if _, ok := reg.mcp["alpha"]; ok {
		t.Error("a source that failed to register must not be tracked")
	}
}

func TestListChangedReregistersSource(t *testing.T) {
	reg, fakes := newTestRegistrar(t)
	if err := reg.start(serverCfg("alpha")); err != nil {
		t.Fatalf("start: %v", err)
	}
	src := fakes["alpha"]
	if got := reg.mcp["alpha"].registered; len(got) != 1 {
		t.Fatalf("expected 1 initial tool, got %v", got)
	}

	// Upstream advertises a different tool set, then fires list_changed.
	src.setTools("alpha_tool", "alpha_new")
	src.fire()

	got := reg.mcp["alpha"].registered
	if len(got) != 2 {
		t.Errorf("list_changed should have swapped in the new tool set, got %v", got)
	}
}

func TestReregisterKeepsToolsOnListError(t *testing.T) {
	reg, fakes := newTestRegistrar(t)
	if err := reg.start(serverCfg("alpha")); err != nil {
		t.Fatalf("start: %v", err)
	}
	src := fakes["alpha"]
	if got := reg.mcp["alpha"].registered; len(got) != 1 {
		t.Fatalf("expected 1 initial tool, got %v", got)
	}

	// A subsequent re-list fails (e.g. a transient blip during a list_changed).
	src.setToolsErr(errors.New("list failed"))
	src.fire()

	// The working registrations must survive the error, not be wiped.
	if got := reg.mcp["alpha"].registered; len(got) != 1 {
		t.Errorf("a transient re-list error wiped registrations: %v", got)
	}
}

func TestConcurrentReloadAndListChanged(t *testing.T) {
	reg, fakes := newTestRegistrar(t)
	if err := reg.start(serverCfg("alpha", "beta")); err != nil {
		t.Fatalf("start: %v", err)
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 50 {
			reg.reload(serverCfg("alpha", "beta"))
		}
	})
	wg.Go(func() {
		for range 50 {
			fakes["alpha"].fire()
		}
	})
	wg.Go(func() {
		for range 50 {
			fakes["beta"].fire()
		}
	})
	wg.Wait()
}
