// Package harness is the single source of truth for how each supported coding
// harness (Claude Code, Cursor, opencode, …) is pointed at the wraptool MCP
// server. It is shared by `wraptool init` and, over time, the container
// bring-up paths, so adding a harness is a one-descriptor change rather than a
// reimplementation in several places.
//
// Phase 1 (this file) is a behaviour-preserving extraction of the generators
// that previously lived in cmd/init.go. Auth (a bearer token for networked
// containers) and the container callers arrive in later phases; the Params
// struct already carries the fields they need so the descriptors don't change
// shape again.
package harness

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// Params is everything a descriptor's Render might need. Callers populate the
// subset relevant to their transport; unused fields stay zero.
type Params struct {
	Transport   string // "stdio" | "sse" | "unix"
	URL         string // full server URL for sse/unix (e.g. http://host:8717/sse)
	SocketPath  string // unix transport
	Token       string // bearer token; "" = none. Unused until the auth phase.
	CWD         string // host worktree path for ?cwd=; "" falls back to os.Getwd()
	WraptoolBin string // stdio transport: the wraptool binary to launch
	ConfigFile  string // stdio transport: --config value
	Global      bool   // user-level vs project-level config path
}

// Descriptor is one harness's knowledge: its name and how to render its MCP
// config (where it lives + the bytes to write).
type Descriptor struct {
	Name   string
	Render func(Params) (path string, content []byte, err error)
}

// order is the stable listing order (used for `all`, help, and error messages).
var order = []string{"agy", "claude", "cursor", "opencode", "gemini", "roo", "windsurf"}

// Registry maps a harness name to its descriptor.
var Registry = map[string]Descriptor{
	"agy":      {Name: "agy", Render: renderAgy},
	"claude":   {Name: "claude", Render: renderClaude},
	"cursor":   {Name: "cursor", Render: renderCursor},
	"opencode": {Name: "opencode", Render: renderOpenCode},
	"gemini":   {Name: "gemini", Render: renderGemini},
	"roo":      {Name: "roo", Render: renderRoo},
	"windsurf": {Name: "windsurf", Render: renderWindsurf},
}

// Names returns the supported harness names in stable order.
func Names() []string {
	out := make([]string, len(order))
	copy(out, order)
	return out
}

// Expand replaces the pseudo-name "all" with every supported harness.
func Expand(names []string) []string {
	for _, h := range names {
		if h == "all" {
			return Names()
		}
	}
	return names
}

// Write renders the harness config and writes it to disk, merging into an
// existing file so other MCP servers are preserved. It returns the path written.
func Write(name string, p Params) (string, error) {
	d, ok := Registry[name]
	if !ok {
		return "", fmt.Errorf("unknown harness %q; supported: %v", name, Names())
	}
	path, content, err := d.Render(p)
	if err != nil {
		return "", err
	}

	if existing, rerr := os.ReadFile(path); rerr == nil {
		if merged, merr := mergeJSON(existing, content); merr == nil {
			content = merged
		}
		// If the merge fails, fall through and write the freshly rendered content.
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", fmt.Errorf("creating directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}

// --- descriptors ---

func renderAgy(p Params) (string, []byte, error) {
	path := ".agents/mcp_config.json"
	if p.Global {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".gemini", "antigravity-cli", "mcp_config.json")
	}
	server := stdioOr(p, func(s map[string]any) {
		s["serverUrl"] = appendCWD(effectiveURL(p), p)
	})
	return path, mcpServers(server), nil
}

func renderClaude(p Params) (string, []byte, error) {
	path := ".mcp.json"
	if p.Global {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".claude.json")
	}
	server := make(map[string]any)
	switch p.Transport {
	case "stdio":
		stdioServer(p, server)
	case "sse":
		server["type"] = "sse"
		server["url"] = appendCWD(p.URL, p)
	case "unix":
		server["type"] = "sse"
		// Claude Code has no native Unix-socket MCP support; document socat bridge.
		server["url"] = appendCWD("http://localhost/sse", p)
	}
	return path, mcpServers(server), nil
}

func renderCursor(p Params) (string, []byte, error) {
	path := ".cursor/mcp.json"
	if p.Global {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".cursor", "mcp.json")
	}
	server := stdioOr(p, func(s map[string]any) {
		s["url"] = appendCWD(effectiveURL(p), p)
	})
	return path, mcpServers(server), nil
}

func renderOpenCode(p Params) (string, []byte, error) {
	path := "opencode.json"
	if p.Global {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".config", "opencode", "config.json")
	}
	server := make(map[string]any)
	switch p.Transport {
	case "stdio":
		server["type"] = "local"
		server["command"] = p.WraptoolBin
		server["args"] = []string{"serve", "--config", p.ConfigFile}
	case "sse", "unix":
		server["type"] = "remote"
		server["url"] = appendCWD(effectiveURL(p), p)
	}
	server["enabled"] = true
	cfg := map[string]any{"mcp": map[string]any{"wraptool": server}}
	return path, mustJSON(cfg), nil
}

func renderGemini(p Params) (string, []byte, error) {
	path := ".gemini/settings.json"
	if p.Global {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".gemini", "settings.json")
	}
	server := stdioOr(p, func(s map[string]any) {
		s["httpUrl"] = appendCWD(effectiveURL(p), p)
	})
	return path, mcpServers(server), nil
}

func renderRoo(p Params) (string, []byte, error) {
	if p.Global {
		return "", nil, fmt.Errorf("roo global config is managed via VS Code UI; use project-level instead")
	}
	server := stdioOr(p, func(s map[string]any) {
		s["type"] = "sse"
		s["url"] = appendCWD(effectiveURL(p), p)
	})
	return ".roo/mcp.json", mcpServers(server), nil
}

func renderWindsurf(p Params) (string, []byte, error) {
	// Windsurf only supports global config.
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".codeium", "windsurf", "mcp_config.json")
	server := stdioOr(p, func(s map[string]any) {
		s["serverUrl"] = appendCWD(effectiveURL(p), p)
	})
	return path, mcpServers(server), nil
}

// --- helpers ---

// stdioOr builds a server entry: the standard stdio command for the stdio
// transport, otherwise whatever setRemote adds (sse/unix).
func stdioOr(p Params, setRemote func(map[string]any)) map[string]any {
	server := make(map[string]any)
	if p.Transport == "stdio" {
		stdioServer(p, server)
	} else {
		setRemote(server)
	}
	return server
}

func stdioServer(p Params, server map[string]any) {
	server["command"] = p.WraptoolBin
	server["args"] = []string{"serve", "--config", p.ConfigFile}
}

func mcpServers(server map[string]any) []byte {
	return mustJSON(map[string]any{"mcpServers": map[string]any{"wraptool": server}})
}

func effectiveURL(p Params) string {
	if p.Transport == "unix" && p.SocketPath != "" {
		// Most tools don't support Unix sockets natively; return the URL as-is
		// (the user needs a socat bridge).
		return p.URL
	}
	return p.URL
}

// appendCWD adds a ?cwd= query param scoping tools to the worktree. It uses
// p.CWD when set, else the current working directory (init's behaviour).
func appendCWD(rawURL string, p Params) string {
	if p.Global || rawURL == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	cwd := p.CWD
	if cwd == "" {
		if wd, werr := os.Getwd(); werr == nil {
			cwd = wd
		} else {
			return rawURL
		}
	}
	q := u.Query()
	q.Set("cwd", cwd)
	u.RawQuery = q.Encode()
	return u.String()
}

// mergeJSON merges the wraptool server entry into an existing config, preserving
// other MCP servers and settings.
func mergeJSON(existing, newContent []byte) ([]byte, error) {
	var existingMap, newMap map[string]any
	if err := json.Unmarshal(existing, &existingMap); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(newContent, &newMap); err != nil {
		return nil, err
	}
	for key, newVal := range newMap {
		newServers, ok := newVal.(map[string]any)
		if !ok {
			continue
		}
		existingVal, exists := existingMap[key]
		if !exists {
			existingMap[key] = newVal
			continue
		}
		existingServers, ok := existingVal.(map[string]any)
		if !ok {
			existingMap[key] = newVal
			continue
		}
		for name, server := range newServers {
			existingServers[name] = server
		}
		existingMap[key] = existingServers
	}
	return mustJSON(existingMap), nil
}

func mustJSON(v any) []byte {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(data, '\n')
}
