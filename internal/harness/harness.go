// Package harness is the single source of truth for how each supported coding
// harness (Claude Code, opencode, …) is pointed at the wraptool MCP
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

// ReleaseSpec describes a harness distributed as GitHub release assets
// (design/harness/omp-release-source.md §6.1). Assets is keyed by the string
// form of pool.PlatformKey so this package keeps no dependency on
// internal/pool; the release Source validates each key with
// pool.ParsePlatformKey, and a registry test asserts every key parses.
type ReleaseSpec struct {
	// Repo is the "owner/name" GitHub repository publishing the releases.
	Repo string
	// VersionFeed is the npm package whose versions track the release tags.
	VersionFeed string
	// Checksums is the release asset name carrying the SHA-256 sums.
	Checksums string
	// Assets maps a platform key (e.g. "linux-amd64-glibc") to the release
	// asset name of that platform's binary.
	Assets map[string]string
}

// Descriptor is one harness's knowledge: its name, how it authenticates, how to
// render its MCP config, and — for CLI harnesses that run inside the container —
// its pool binary name and optional install recipe.
type Descriptor struct {
	Name string
	Auth AuthMode
	// Bin is the executable name in the harness pool.
	Bin string
	// Install is an argv template run by `wraptool harness install` to fetch the
	// binary into the pool; the token "{prefix}" is replaced with the pool root.
	// Nil means no automated installer (drop the binary in pool/bin by hand).
	Install []string
	// PoolPackage is the npm package identity used by the version-locked
	// harness pool (design/harness/unified-harness-pool.md §7-§8) to
	// refresh/resolve this harness via the npm Source, e.g.
	// "@anthropic-ai/claude-code". Mutually exclusive with PoolArchive; see
	// PoolLockable/PoolPackageID.
	PoolPackage string
	// PoolArchive marks a harness whose pool Source is the direct-download
	// ("archive") adapter (internal/pool/source's Archive, §6.2/§8.1) rather
	// than npm — agy (Antigravity), whose release channel publishes a
	// single per-platform .tar.gz with no npm package at all. Mutually
	// exclusive with PoolPackage; see PoolLockable/PoolPackageID.
	PoolArchive bool
	// PoolRelease marks a harness whose pool Source is the GitHub-release
	// adapter (design/harness/omp-release-source.md): per-platform raw
	// binaries published on a repository's releases, versioned by an npm
	// feed and verified against a checksums asset. Mutually exclusive with
	// PoolPackage and PoolArchive; see PoolLockable/PoolPackageID. A
	// PoolRelease harness may have no Install recipe at all (a pool-only
	// harness: omp).
	PoolRelease *ReleaseSpec
	// NodeLine pins the Node major line (e.g. "22") this npm-sourced
	// harness's pool generation bundles (design/harness/node-runtime-
	// tracking.md: "pin the line, not the patch") — passed through as
	// pool.UpdateOptions.NodeLine. Empty means the pool-wide default line
	// (internal/pool's defaultNodeLine). Moving a harness — or the default —
	// to a new major stays a deliberate code change: majors are
	// compatibility decisions, the exact patch within a line is a
	// maintenance detail the pool now resolves itself (§9's quarantine,
	// applied to node releases exactly like package versions). Meaningless
	// for an archive-sourced harness (agy bundles no node runtime at all).
	NodeLine string
	Render   func(Params) (path string, content []byte, err error)
}

// PoolLockable reports whether the version-locked harness pool
// (design/harness/unified-harness-pool.md) can refresh/resolve/realize this
// harness at all — via either PoolPackage (npm) or PoolArchive (direct
// download). A harness with neither is "legacy (not lockable)": `harness
// list` reports it as such and `install` falls back to today's unpinned
// behavior with a warning (host-side IDEs have no installable package at
// all).
func (d Descriptor) PoolLockable() bool {
	return d.PoolPackage != "" || d.PoolArchive || d.PoolRelease != nil
}

// PoolPackageID returns the identity a pool-lockable harness is
// refreshed/resolved/observed under — the orchestrator's "pkg" parameter
// (design/harness/unified-harness-pool.md §8.1): PoolPackage for an
// npm-sourced harness, the harness's own Name for an archive-sourced one, or
// the release repository (ReleaseSpec.Repo) for a release-sourced one, so a
// repository move is a new identity and forces a fresh resolve.
// An archive Source's Refresh/Resolve "pkg" parameter has no separate
// package identity on that channel (every platform's manifest describes the
// same release, internal/pool/source/archive.go's own doc comment); it only
// labels the lock/observation entry, so the harness name is the natural,
// stable choice. Empty for a harness that is not PoolLockable.
func (d Descriptor) PoolPackageID() string {
	switch {
	case d.PoolPackage != "":
		return d.PoolPackage
	case d.PoolArchive:
		return d.Name
	case d.PoolRelease != nil:
		return d.PoolRelease.Repo
	default:
		return ""
	}
}

// order is the stable listing order (used for `all`, help, and error messages).
var order = []string{"agy", "claude", "omp", "opencode", "pi"}

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
				`install -Dm755 "$HOME/.local/bin/agy" "{prefix}/bin/agy"`},
		// agy has no npm package (its official channel ships a direct-download
		// .tar.gz per platform, internal/pool/source/archive.go), so it is
		// pool-lockable via PoolArchive rather than PoolPackage — the legacy
		// curl|bash Install above still runs after realize (§16 Phase 2
		// posture, WIDENED by wave2-integration-fixes item 4: the realized/
		// activated generation IS now mounted read-only into a guix container
		// as runtime/, alongside legacy bin/+lib/, once an active pointer
		// exists — cmd/guixcontainer.go's guixPoolPlan, §12.1 mixed-state
		// RULING — so the legacy curl|bash install is no longer the ONLY
		// thing that makes agy runnable; it stays the only thing that makes
		// it runnable OUTSIDE a guix container, or before the first
		// successful realize+activate, and remains unconditional here for
		// that reason. Full Phase-4 cutover removes it (§16)).
		PoolArchive: true},
	"claude": {Name: "claude", Auth: AuthHeader, Bin: "claude", Render: renderClaude,
		Install: npmInstall("@anthropic-ai/claude-code"), PoolPackage: "@anthropic-ai/claude-code"},
	// omp (oh-my-pi) is the first pool-only harness: no Install recipe, its
	// binary arrives solely through the pool's release Source
	// (design/harness/omp-release-source.md §6.1).
	"omp": {Name: "omp", Auth: AuthHeader, Bin: "omp", Render: renderOMP,
		PoolRelease: &ReleaseSpec{
			Repo:        "can1357/oh-my-pi",
			VersionFeed: "@oh-my-pi/pi-coding-agent",
			Checksums:   "SHA256SUMS.txt",
			Assets: map[string]string{
				"linux-amd64-glibc": "omp-linux-x64",
				"linux-arm64-glibc": "omp-linux-arm64",
				"linux-amd64-musl":  "omp-linux-musl-x64",
				"linux-arm64-musl":  "omp-linux-musl-arm64",
			},
		}},
	"opencode": {Name: "opencode", Auth: AuthHeader, Bin: "opencode", Render: renderOpenCode, Install: npmInstall("opencode-ai"), PoolPackage: "opencode-ai"},
	"pi": {Name: "pi", Auth: AuthHeader, Bin: "pi", Render: renderPi, Install: npmInstall("@earendil-works/pi-coding-agent"),
		PoolPackage: "@earendil-works/pi-coding-agent"},
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

// Plan is a validated, merged harness config that has not been written yet.
// Prepare/Commit are split so a multi-harness caller (`init --harness all`)
// can validate every target before writing any — a mid-list failure must not
// leave a partial set of configs behind.
type Plan struct {
	Path    string
	content []byte
}

// Prepare renders the harness config, resolves its target path, and merges
// into an existing file (so other MCP servers are preserved) — every failure
// mode of a write except the write itself. Commit the returned Plan to put it
// on disk.
func Prepare(name string, p Params) (*Plan, error) {
	d, ok := Registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown harness %q; supported: %v", name, Names())
	}
	if p.Token != "" && d.Auth == AuthUnverified {
		return nil, fmt.Errorf("harness %q cannot be used with a networked wraptool yet: "+
			"its auth support is unverified. Use a localhost setup, or add its "+
			"AuthMode (design/harness/harness-descriptor.md phase 3)", name)
	}
	path, content, err := d.Render(p)
	if err != nil {
		return nil, err
	}
	// Resolve a project-relative path against Dir (empty = process cwd, init's
	// behaviour). Absolute paths (global configs) are left as-is.
	if p.Dir != "" && !filepath.IsAbs(path) {
		path = filepath.Join(p.Dir, path)
	}

	if existing, rerr := os.ReadFile(path); rerr == nil {
		merged, merr := mergeJSON(existing, content)
		if merr != nil {
			return nil, fmt.Errorf("refusing to overwrite %s: existing file is not JSON wraptool can merge: %w", path, merr)
		}
		content = merged
	}
	return &Plan{Path: path, content: content}, nil
}

// Write renders the harness config and writes it to disk, merging into an
// existing file so other MCP servers are preserved. It returns the path written.
func Write(name string, p Params) (string, error) {
	plan, err := Prepare(name, p)
	if err != nil {
		return "", err
	}
	return plan.Commit()
}

// Commit writes the prepared config to its path.
func (plan *Plan) Commit() (string, error) {
	path, content := plan.Path, plan.content
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", fmt.Errorf("creating directory for %s: %w", path, err)
	}
	// 0600: the rendered config carries an Authorization: Bearer token
	// whenever one is set (see bearer), and it is written into the project
	// worktree.
	if err := os.WriteFile(path, content, 0600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	// WriteFile applies the mode only on creation; tighten a pre-existing
	// 0644 config from before the hardening.
	if err := os.Chmod(path, 0600); err != nil {
		return "", fmt.Errorf("tightening permissions on %s: %w", path, err)
	}
	if err := ensureGitExclude(path); err != nil {
		return "", fmt.Errorf("excluding %s from git: %w", path, err)
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

// renderOMP targets omp (oh-my-pi), which recommends Streamable HTTP for
// remote servers, so it points at /mcp like agy. Project file .omp/mcp.json;
// global ~/.omp/agent/mcp.json (omp no longer reads the older ~/.omp/mcp.json).
func renderOMP(p Params) (string, []byte, error) {
	path := filepath.Join(".omp", "mcp.json")
	if p.Global {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".omp", "agent", "mcp.json")
	}
	server := stdioOr(p, func(s map[string]any) {
		s["type"] = "http"
		s["url"] = appendCWD(streamableURL(p), p)
		bearer(s, p)
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
