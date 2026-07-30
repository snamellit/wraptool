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
	Token       string // bearer token; "" = none (localhost-trust)
	CWD         string // host worktree path for ?cwd=; "" falls back to os.Getwd()
	Dir         string // base dir for project-level configs; "" = process cwd (init's behaviour)
	WraptoolBin string // stdio transport: the wraptool binary to launch
	ConfigFile  string // stdio transport: --config value
	Global      bool   // user-level vs project-level config path
}

// AuthMode says how (or whether) a harness can authenticate to a networked
// wraptool. It gates the loud failure in Write when a token is required but the
// harness can't carry it.
type AuthMode int

const (
	// AuthUnverified: auth support has not been checked for this harness. Used
	// with a token (i.e. networked), Write errors rather than silently emitting
	// an unauthenticated config that will 401. Localhost/stdio use (no token)
	// is unaffected. Phase 3 replaces this with the harness's real mode.
	AuthUnverified AuthMode = iota
	// AuthHeader: an Authorization: Bearer header on the server entry.
	AuthHeader
	// AuthNone: the harness authenticates by other means / localhost only; a
	// token is accepted but not emitted.
	AuthNone
)

// Descriptor is one harness's knowledge: its name, how it authenticates, how to
// render its MCP config, and — for CLI harnesses that run inside the container —
// its pool binary name and optional install recipe.
type Descriptor struct {
	Name string
	Auth AuthMode
	// Bin is the executable name in the harness pool. Empty means the harness is
	// a host-side IDE/extension (cursor, roo), not a container CLI, so
	// it is not pool-installable.
	Bin string
	// Install is an argv template run by `wraptool harness install` to fetch the
	// binary into the pool; the token "{prefix}" is replaced with the pool root.
	// Nil means no automated installer (drop the binary in pool/bin by hand).
	Install []string
	// PoolPackage is the npm package identity used by the version-locked
	// harness pool (design/unified-harness-pool.md §7-§8) to refresh/resolve
	// this harness, e.g. "@anthropic-ai/claude-code". Empty means the
	// harness is not pool-lockable yet: `harness list` reports it as
	// "legacy (not lockable)" and `install` falls back to today's unpinned
	// behavior with a warning (§8: curl|bash recipes like agy's stay legacy
	// until an artifact-specific adapter exists; host-side IDEs have no
	// installable package at all).
	PoolPackage string
	Render      func(Params) (path string, content []byte, err error)
}

// IsCLI reports whether the harness runs as a binary inside the container pool
// (vs a host-side IDE/extension).
func (d Descriptor) IsCLI() bool { return d.Bin != "" }

// order is the stable listing order (used for `all`, help, and error messages).
var order = []string{"agy", "claude", "cursor", "opencode", "gemini", "pi", "roo"}

// Registry maps a harness name to its descriptor.
// Auth defaults to AuthUnverified (the zero value) for every harness except the
// ones whose networked auth has actually been verified. claude carries a Bearer
// header (matches connect.sh + the container path). The rest are verified in
// phase 3; until then a token forces a loud failure in Write.
// npmInstall builds an install recipe that npm-installs a package into the pool
// prefix (bins land in <prefix>/bin). It runs npm inside a Guix container with
// FHS emulation (-F): npm's detect-libc mis-reads glibc as musl on the bare
// (non-FHS) Guix host, which breaks platform-split native CLIs (opencode,
// claude, …) with EBADPLATFORM. -F supplies a glibc FHS so the right platform
// binary is chosen; --share persists the install into the pool; --network is
// for the download.
func npmInstall(pkg string) []string {
	return []string{
		"guix", "shell", "--container", "--network", "--emulate-fhs",
		"--share={prefix}={prefix}", "node", "--",
		"npm", "install", "-g", pkg, "--prefix", "{prefix}",
	}
}

var Registry = map[string]Descriptor{
	// agy (Antigravity) is a Go binary with no npm package; its official script
	// installs to ~/.local/bin, so we run it and relocate the binary into the
	// pool. Best-effort — it runs Google's installer (curl | bash).
	"agy": {Name: "agy", Auth: AuthHeader, Bin: "agy", Render: renderAgy,
		Install: []string{"guix", "shell", "curl", "bash", "coreutils", "--", "sh", "-c",
			`curl -fsSL https://antigravity.google/cli/install.sh | bash && ` +
				`install -Dm755 "$HOME/.local/bin/agy" "{prefix}/bin/agy"`}},
	"claude": {Name: "claude", Auth: AuthHeader, Bin: "claude", Render: renderClaude,
		Install: npmInstall("@anthropic-ai/claude-code"), PoolPackage: "@anthropic-ai/claude-code"},
	"cursor":   {Name: "cursor", Auth: AuthHeader, Render: renderCursor}, // host IDE, not pool CLI
	"opencode": {Name: "opencode", Auth: AuthHeader, Bin: "opencode", Render: renderOpenCode, Install: npmInstall("opencode-ai"), PoolPackage: "opencode-ai"},
	"gemini":   {Name: "gemini", Auth: AuthHeader, Bin: "gemini", Render: renderGemini},
	"pi": {Name: "pi", Auth: AuthHeader, Bin: "pi", Render: renderPi, Install: npmInstall("@earendil-works/pi-coding-agent"),
		PoolPackage: "@earendil-works/pi-coding-agent"},
	// roo accepts a headers object but a known bug drops the Authorization
	// header on SSE connections (RooCodeInc/Roo-Code#2480), so networked auth is
	// unverified — a token forces a loud failure rather than a silent 401.
	"roo": {Name: "roo", Render: renderRoo}, // host VS Code ext
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
	if p.Token != "" && d.Auth == AuthUnverified {
		return "", fmt.Errorf("harness %q cannot be used with a networked wraptool yet: "+
			"its auth support is unverified. Use a localhost/stdio setup, or add its "+
			"AuthMode (design/harness-descriptor.md phase 3)", name)
	}
	path, content, err := d.Render(p)
	if err != nil {
		return "", err
	}
	// Resolve a project-relative path against Dir (empty = process cwd, init's
	// behaviour). Absolute paths (global configs) are left as-is.
	if p.Dir != "" && !filepath.IsAbs(path) {
		path = filepath.Join(p.Dir, path)
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
		// Antigravity negotiates the 2025-03 Streamable HTTP transport (POSTs
		// initialize to the URL), so point it at wraptool's /mcp endpoint, not the
		// legacy SSE /sse (which would 405 the POST). See newStreamableServer.
		s["serverUrl"] = appendCWD(streamableURL(p), p)
		bearer(s, p)
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
		bearer(server, p)
	case "unix":
		server["type"] = "sse"
		// Claude Code has no native Unix-socket MCP support; document socat bridge.
		server["url"] = appendCWD("http://localhost/sse", p)
		bearer(server, p)
	}
	return path, mcpServers(server), nil
}

// bearer adds an Authorization: Bearer header to an sse/unix server entry when a
// token is set (Claude Code's AuthHeader mode).
func bearer(server map[string]any, p Params) {
	if p.Token != "" {
		server["headers"] = map[string]any{"Authorization": "Bearer " + p.Token}
	}
}

func renderCursor(p Params) (string, []byte, error) {
	path := ".cursor/mcp.json"
	if p.Global {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".cursor", "mcp.json")
	}
	server := stdioOr(p, func(s map[string]any) {
		s["url"] = appendCWD(effectiveURL(p), p)
		bearer(s, p)
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
		bearer(server, p)
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
		bearer(s, p)
	})
	return path, mcpServers(server), nil
}

// renderPi targets the Pi coding agent. Pi has no native MCP; the third-party
// pi-mcp-adapter (github.com/nicobailon/pi-mcp-adapter) reads this config. We
// write the Pi-specific .pi/mcp.json (project) / ~/.pi/agent/mcp.json (global)
// so it never collides with claude's .mcp.json, using the adapter's remote
// shape: mcpServers → {url, headers:{Authorization: Bearer}}.
func renderPi(p Params) (string, []byte, error) {
	path := filepath.Join(".pi", "mcp.json")
	if p.Global {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".pi", "agent", "mcp.json")
	}
	server := stdioOr(p, func(s map[string]any) {
		s["url"] = appendCWD(effectiveURL(p), p)
		bearer(s, p)
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

// streamableURL derives wraptool's Streamable HTTP endpoint (/mcp) from the
// configured base URL, for harnesses that speak the 2025-03 transport instead of
// legacy SSE. It swaps only the path, preserving scheme/host/port.
func streamableURL(p Params) string {
	u, err := url.Parse(p.URL)
	if err != nil {
		return p.URL
	}
	u.Path = "/mcp"
	return u.String()
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
