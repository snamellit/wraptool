package cmd

import (
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/mcptools"
	"github.com/pti/wraptool/internal/requestlog"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/fsnotify/fsnotify"
	"github.com/mark3labs/mcp-go/server"
)

// reloadState tracks MCP tool names registered from config so SIGHUP can
// replace them atomically, plus the live sources so their backing processes
// (e.g. spawned MCP servers) can be closed on reload.
type reloadState struct {
	mu         sync.Mutex
	registered []string
	sources    []mcptools.Source
}

// closeSources tears down the currently registered sources, closing any
// spawned MCP-server child processes. Safe to call from the reload path and
// from serve's deferred shutdown; it takes the state lock and is idempotent
// (a closed Source's Close is a no-op).
func (st *reloadState) closeSources() {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, src := range st.sources {
		if err := src.Close(); err != nil {
			log.Printf("closing source: %s", err)
		}
	}
}

func handleSIGHUP(s *server.MCPServer, configPath string, policy *whitelist.Policy, mcpPolicy *whitelist.MCPPolicy, state *reloadState, store *requestlog.Store) {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	for range sighup {
		reloadConfig(s, configPath, policy, mcpPolicy, state, store, "SIGHUP")
	}
}

// watchConfigFile watches the config file's parent directory for changes
// and triggers a reload. Watching the directory (not the file) survives
// editor save patterns that replace the file via rename.
func watchConfigFile(s *server.MCPServer, configPath string, policy *whitelist.Policy, mcpPolicy *whitelist.MCPPolicy, state *reloadState, store *requestlog.Store) {
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		log.Printf("config watch: resolving %s: %s", configPath, err)
		return
	}
	dir := filepath.Dir(absPath)
	name := filepath.Base(absPath)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("config watch: %s (file-based reload disabled, SIGHUP still works)", err)
		return
	}
	defer w.Close()

	if err := w.Add(dir); err != nil {
		log.Printf("config watch: adding %s: %s (file-based reload disabled)", dir, err)
		return
	}
	log.Printf("watching %s for changes", absPath)

	// Debounce: a single save can emit CREATE+WRITE+CHMOD; coalesce
	// rapid events into one reload.
	var debounce *time.Timer
	const debounceDelay = 200 * time.Millisecond

	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if filepath.Base(ev.Name) != name {
				continue
			}
			// Ignore pure CHMOD; reload on write/create/rename.
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			if debounce != nil {
				debounce.Stop()
			}
			debounce = time.AfterFunc(debounceDelay, func() {
				reloadConfig(s, configPath, policy, mcpPolicy, state, store, "file change")
			})
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("config watch error: %s", err)
		}
	}
}

func reloadConfig(s *server.MCPServer, configPath string, policy *whitelist.Policy, mcpPolicy *whitelist.MCPPolicy, state *reloadState, store *requestlog.Store, trigger string) {
	log.Printf("reloading config (%s)...", trigger)
	newCfg, err := config.Load(configPath)
	if err != nil {
		log.Printf("reload failed: %s", err)
		return
	}
	if err := newCfg.Validate(); err != nil {
		log.Printf("reload validation failed: %s", err)
		return
	}
	policy.Reload(toolConfigPointers(newCfg.Tools))
	mcpPolicy.Reload(mcpServerConfigPointers(newCfg.MCPServers))

	// Swap MCP tool registrations: remove the previously registered set, close
	// the old sources (tearing down any spawned MCP-server children), then
	// register fresh from the new config. mcp-go sends
	// notifications/tools/list_changed on both calls, so connected clients
	// refresh their tool lists without needing to reconnect.
	//
	// Phase 1 MVP: all old sources are closed and rebuilt unconditionally.
	// Phase 2 will reconcile the server set (add/remove/change/unchanged) so
	// unchanged upstreams are not needlessly restarted.
	state.mu.Lock()
	if len(state.registered) > 0 {
		s.DeleteTools(state.registered...)
	}
	for _, src := range state.sources {
		if err := src.Close(); err != nil {
			log.Printf("reload: closing source: %s", err)
		}
	}
	sources := buildSources(newCfg, policy, mcpPolicy, store)
	names, err := mcptools.RegisterAll(s, sources)
	state.registered = names
	state.sources = sources
	state.mu.Unlock()
	if err != nil {
		log.Printf("reload tool registration failed: %s", err)
		return
	}
	log.Println("config reloaded successfully")
}
