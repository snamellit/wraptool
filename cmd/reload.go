package cmd

import (
	"log"
	"path/filepath"
	"time"

	"github.com/pti/wraptool/internal/config"

	"github.com/fsnotify/fsnotify"
)

// watchConfigFile watches the config file's parent directory for changes
// and triggers a reload. Watching the directory (not the file) survives
// editor save patterns that replace the file via rename.
func watchConfigFile(configPath string, reg *registrar) {
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
				reloadConfig(configPath, reg, "file change")
			})
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("config watch error: %s", err)
		}
	}
}

func reloadConfig(configPath string, reg *registrar, trigger string) {
	log.Printf("reloading config (%s)...", trigger)
	// Strict load (see serve): a typo'd restrictive key must fail the reload, not
	// silently weaken the live policy. On any error the old policy stays in
	// effect (fail-closed) — F1 in design/policy/security-audit-policy-enforcement.md.
	newCfg, err := config.LoadStrict(configPath)
	if err != nil {
		log.Printf("reload failed: %s", err)
		return
	}
	if err := newCfg.Validate(); err != nil {
		log.Printf("reload validation failed: %s", err)
		return
	}

	// Reconcile the running source set in place: unchanged MCP-server children
	// keep running, only added/removed/changed ones are touched (see
	// registrar.reload). mcp-go emits notifications/tools/list_changed on each
	// AddTool/DeleteTools, so connected clients refresh without reconnecting.
	reg.reload(newCfg)
	log.Println("config reloaded successfully")
}
