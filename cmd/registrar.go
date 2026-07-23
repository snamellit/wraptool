package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"sync"

	"github.com/pti/wraptool/internal/buildinfo"
	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/mcpproxy"
	"github.com/pti/wraptool/internal/mcptools"
	"github.com/pti/wraptool/internal/requestlog"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/mark3labs/mcp-go/server"
)

// mcpSource is the slice of *mcpproxy.Source the registrar depends on. It is an
// interface (rather than the concrete type) purely so the reconcile logic can
// be unit-tested with a fake that never spawns a child process.
type mcpSource interface {
	mcptools.Source
	OnToolsChanged(func())
	SetConfig(*config.MCPServerConfig)
}

// registrar owns the live set of tool sources and their registrations on the
// MCP server, and serializes every mutation of that set behind one mutex. That
// matters because three independent triggers all re-register tools: config
// reload (which reconciles the whole set), an upstream tools/list_changed
// notification, and a crash respawn — the latter two arrive on mcpproxy client
// goroutines. All of them funnel through this mutex so the server's tool table
// is never mutated concurrently.
//
// The CLI source is swapped wholesale on reload (it is cheap and stateless);
// only the MCP sources get the add/remove/change/unchanged diff so that an
// unchanged upstream child is never needlessly restarted.
type registrar struct {
	server    *server.MCPServer
	store     *requestlog.Store
	policy    *whitelist.Policy
	mcpPolicy *whitelist.MCPPolicy

	// newMCPSource builds one MCP proxy source; a field so tests can inject a
	// fake. Production sets it to spawn a real mcpproxy.Source.
	newMCPSource func(name string, cfg *config.MCPServerConfig) mcpSource

	mu            sync.Mutex
	cliRegistered []string
	cliTools      []string
	mcp           map[string]*mcpSourceEntry // by mcp_servers key
}

// mcpSourceEntry is one live MCP source plus what the registrar needs to
// reconcile it: the tool names currently registered for it, and the spawn /
// policy fingerprints of the config it was built from.
type mcpSourceEntry struct {
	source     mcpSource
	registered []string
	spawnFP    string
	policyFP   string
}

// newRegistrar builds a registrar wired to spawn real mcpproxy sources.
func newRegistrar(s *server.MCPServer, policy *whitelist.Policy, mcpPolicy *whitelist.MCPPolicy, store *requestlog.Store) *registrar {
	r := &registrar{
		server:    s,
		store:     store,
		policy:    policy,
		mcpPolicy: mcpPolicy,
		mcp:       map[string]*mcpSourceEntry{},
	}
	r.newMCPSource = func(name string, cfg *config.MCPServerConfig) mcpSource {
		return mcpproxy.NewSource(name, cfg, r.mcpPolicy, r.store, buildinfo.Version())
	}
	return r
}

// start performs the initial registration: the CLI source plus one MCP source
// per configured server. A per-source registration error is returned (the CLI
// source can fail to introspect); MCP spawn failures are swallowed inside the
// source and simply contribute no tools.
func (r *registrar) start(cfg *config.Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.cliTools = toolNames(cfg)
	names, err := r.registerSource(mcptools.NewCLISource(cfg, r.policy, r.store))
	if err != nil {
		return err
	}
	r.cliRegistered = names

	for name := range cfg.MCPServers {
		srv := cfg.MCPServers[name] // fresh copy; source holds &srv
		if err := r.addMCPLocked(name, &srv); err != nil {
			return err
		}
	}
	return nil
}

// reload swaps the policy in and reconciles the source set against newCfg,
// under the registrar lock so it is atomic with respect to notification- and
// respawn-driven re-registrations.
func (r *registrar) reload(newCfg *config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Swap policy first so any re-list below sees the new allow/deny.
	r.policy.Reload(configPointers(newCfg.Tools))
	r.mcpPolicy.Reload(configPointers(newCfg.MCPServers))

	// CLI source: cheap wholesale swap.
	r.cliTools = toolNames(newCfg)
	if len(r.cliRegistered) > 0 {
		r.server.DeleteTools(r.cliRegistered...)
	}
	names, err := r.registerSource(mcptools.NewCLISource(newCfg, r.policy, r.store))
	if err != nil {
		log.Printf("reload: registering cli tools: %s", err)
	}
	r.cliRegistered = names

	// MCP sources: name-keyed reconcile so unchanged children are left running.
	for name := range newCfg.MCPServers {
		srv := newCfg.MCPServers[name] // fresh copy
		spawnFP, policyFP := fingerprints(&srv)
		e, exists := r.mcp[name]
		switch {
		case !exists:
			if err := r.addMCPLocked(name, &srv); err != nil {
				log.Printf("reload: adding mcp server %q: %s", name, err)
			}
		case e.spawnFP != spawnFP:
			// Command/args/env/working-dir changed → the child must restart.
			r.removeMCPLocked(name)
			if err := r.addMCPLocked(name, &srv); err != nil {
				log.Printf("reload: recreating mcp server %q: %s", name, err)
			}
		case e.policyFP != policyFP:
			// Only allow/deny/name_prefix changed → keep the live child, refresh
			// its registrations against the reloaded policy.
			e.source.SetConfig(&srv)
			e.policyFP = policyFP
			r.reregisterLocked(name)
		default:
			// Unchanged — leave the source and its child entirely alone.
		}
	}

	// Servers dropped from the config: deregister and stop their children.
	for name := range r.mcp {
		if _, ok := newCfg.MCPServers[name]; !ok {
			r.removeMCPLocked(name)
		}
	}
}

// CLIToolNames returns a snapshot of the configured CLI tools for status
// reporting. It is safe to call while reload is replacing the CLI source.
func (r *registrar) CLIToolNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cliTools...)
}

// reregister re-lists a single MCP source and swaps only its registrations. It
// is the callback target for tools/list_changed and crash respawn, so it takes
// the lock; internal callers that already hold it use reregisterLocked.
func (r *registrar) reregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reregisterLocked(name)
}

func (r *registrar) reregisterLocked(name string) {
	e, ok := r.mcp[name]
	if !ok {
		return // source was removed since the trigger fired; nothing to do.
	}
	tools, err := e.source.Tools(context.Background())
	if err != nil {
		// Keep the last-known-good registrations rather than tearing them down
		// over a transient re-list failure — a list_changed or respawn blip must
		// not strip a working server of its tools. (A real mcpproxy source
		// reports an empty list with a nil error when its child is genuinely
		// down, so a gone upstream still drops its tools via the path below;
		// this only guards the error-return case.)
		log.Printf("reregister: listing tools for mcp server %q: %s (keeping existing registrations)", name, err)
		return
	}
	if len(e.registered) > 0 {
		r.server.DeleteTools(e.registered...)
	}
	e.registered = addTools(r.server, tools)
}

// addMCPLocked builds, wires, and registers a fresh MCP source for name.
func (r *registrar) addMCPLocked(name string, cfg *config.MCPServerConfig) error {
	src := r.newMCPSource(name, cfg)
	src.OnToolsChanged(func() { r.reregister(name) })
	names, err := r.registerSource(src)
	if err != nil {
		// registerSource enumerates the source's tools, which for an mcpproxy
		// source spawns the child. If that errors after the child came up, the
		// source is never stored in r.mcp and so would never be closed — close
		// it here so a failed add never orphans a process.
		if cerr := src.Close(); cerr != nil {
			log.Printf("closing mcp server %q after failed registration: %s", name, cerr)
		}
		return err
	}
	spawnFP, policyFP := fingerprints(cfg)
	r.mcp[name] = &mcpSourceEntry{
		source:     src,
		registered: names,
		spawnFP:    spawnFP,
		policyFP:   policyFP,
	}
	return nil
}

// removeMCPLocked deregisters a source's tools, closes its child, and drops it.
func (r *registrar) removeMCPLocked(name string) {
	e, ok := r.mcp[name]
	if !ok {
		return
	}
	if len(e.registered) > 0 {
		r.server.DeleteTools(e.registered...)
	}
	if err := e.source.Close(); err != nil {
		log.Printf("closing mcp server %q: %s", name, err)
	}
	delete(r.mcp, name)
}

// registerSource enumerates a source's tools and adds them to the server,
// returning the registered names.
func (r *registrar) registerSource(src mcptools.Source) ([]string, error) {
	tools, err := src.Tools(context.Background())
	if err != nil {
		return nil, err
	}
	return addTools(r.server, tools), nil
}

// closeAll tears down every MCP source's child process. Called from serve's
// deferred shutdown.
func (r *registrar) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, e := range r.mcp {
		if err := e.source.Close(); err != nil {
			log.Printf("closing mcp server %q: %s", name, err)
		}
	}
}

// addTools registers each tool and returns the registered names.
func addTools(s *server.MCPServer, tools []mcptools.RegisteredTool) []string {
	var names []string
	for _, rt := range tools {
		s.AddTool(rt.Tool, rt.Handler)
		names = append(names, rt.Name)
	}
	return names
}

// fingerprints splits an MCP server config into a spawn fingerprint (the inputs
// that determine the child process — changing any means the child must restart)
// and a policy fingerprint (the inputs that determine which tools are exposed
// and under what names — changing any means only re-registration is needed).
func fingerprints(cfg *config.MCPServerConfig) (spawn, policy string) {
	sh := sha256.New()
	fmt.Fprintf(sh, "cmd\x00%s\x00", cfg.Command)
	for _, a := range cfg.Args {
		fmt.Fprintf(sh, "arg\x00%s\x00", a)
	}
	fmt.Fprintf(sh, "wd\x00%s\x00", cfg.WorkingDir)
	fmt.Fprintf(sh, "timeout\x00%d\x00%d\x00", cfg.Timeout, cfg.InitTimeout)
	// Resolved env folds in inline vars and env_files contents, so a changed
	// secrets file re-fingerprints and restarts the child. A resolution error
	// is folded in too, so recovery (file appears) is also seen as a change.
	if env, err := cfg.ResolvedEnv(); err == nil {
		for _, k := range sortedKeys(env) {
			fmt.Fprintf(sh, "env\x00%s\x00%s\x00", k, env[k])
		}
	} else {
		fmt.Fprintf(sh, "enverr\x00%v\x00", err)
	}
	spawn = hex.EncodeToString(sh.Sum(nil))

	ph := sha256.New()
	fmt.Fprintf(ph, "prefix\x00%s\x00", cfg.NamePrefix)
	for _, a := range cfg.Allow {
		fmt.Fprintf(ph, "allow\x00%s\x00", a.Tool)
	}
	for _, d := range cfg.Deny {
		fmt.Fprintf(ph, "deny\x00%s\x00", d.Tool)
	}
	policy = hex.EncodeToString(ph.Sum(nil))
	return spawn, policy
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
