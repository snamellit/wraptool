// mcpservers.go implements the upstream MCP server wiring checks (design
// §7.4's tools.mcp-server-* trio) plus the one mcp_servers-shaped content
// lint the catalog assigns alongside them, policy.mcp-dangerous-tool (§7.7).
// All four range over Input.Config.MCPServers, which internal/config keys by
// server name in a Go map — iteration order MUST be sorted (server name) or
// TestChecksAreDeterministic (invariants_test.go, 8 runs against a 2-entry
// MCPServers fixture) fails on the merge commit.
//
// internal/config's validateMCPServers (internal/config/validate.go) checks
// only that mcp_servers.*.command is non-empty and that allow/deny tool names
// are non-empty — it has no equivalent of tools.* validateTool's
// absolute/exists/regular-file requirement on the binary, so `command: npx`
// or `command: uvx` resolves from whatever PATH the serve process inherits at
// spawn time (internal/executor.ChildEnv), a hijack vector the CLI side does
// not have. tools.mcp-server-command closes the absolute/exists half of that
// gap; tools.mcp-server-args-unpinned closes the other half an absolute,
// existing `command` still leaves open — an unpinned npx/uvx package spec
// fetches and executes whatever code the registry currently serves for that
// name, at every serve start, with this server's own env (including any
// env_files credentials, ToolConfig's mirror) available to it
// (internal/mcpproxy/client.go passes Args verbatim to
// client.NewStdioMCPClientWithOptions with no validation at all).
package doctor

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pti/wraptool/internal/config"
)

const (
	mcpServerCommand      CheckID = "tools.mcp-server-command"
	mcpServerArgsUnpinned CheckID = "tools.mcp-server-args-unpinned"
	mcpServerInert        CheckID = "tools.mcp-server-inert"
	mcpDangerousTool      CheckID = "policy.mcp-dangerous-tool"
)

// mcpServerCommandMeta, mcpServerArgsUnpinnedMeta, mcpServerInertMeta, and
// mcpDangerousToolMeta are named so mcpservers_test.go's meta assertions read
// the SAME literal init() registers — never through Registered() (plan §1
// rule 8: by the time this package's eight Phase-B siblings have merged,
// Registered() also carries every one of THEIR checks, which a test asserting
// one check's own ID/Needs must not depend on).
var (
	mcpServerCommandMeta = Meta{
		ID:      mcpServerCommand,
		Needs:   NeedsConfig,
		Summary: "checks that every mcp_servers.*.command is an absolute, existing, executable file",
	}
	mcpServerArgsUnpinnedMeta = Meta{
		ID:    mcpServerArgsUnpinned,
		Needs: NeedsConfig,
		Summary: "checks that an mcp_servers.*.args npx/uvx invocation pins an exact package version " +
			"rather than fetching whatever is currently latest",
	}
	mcpServerInertMeta = Meta{
		ID:      mcpServerInert,
		Needs:   NeedsConfig,
		Summary: "warns when an mcp_servers entry has zero allow rules but the upstream child is still spawned",
	}
	mcpDangerousToolMeta = Meta{
		ID:    mcpDangerousTool,
		Needs: NeedsConfig,
		Summary: "warns when an mcp_servers.*.allow entry names an upstream tool matching an " +
			"execute-or-write name shape",
	}
)

func init() {
	register(mcpServerCommandMeta, runMcpServerCommand)
	register(mcpServerArgsUnpinnedMeta, runMcpServerArgsUnpinned)
	register(mcpServerInertMeta, runMcpServerInert)
	register(mcpDangerousToolMeta, runMcpDangerousTool)
}

// mcpSortedServerNames returns Input.Config.MCPServers' keys, sorted — the
// single helper every check below ranges through, so no two of the four
// checks in this file can independently forget to sort (see this file's own
// doc comment).
func mcpSortedServerNames(servers map[string]config.MCPServerConfig) []string {
	out := make([]string, 0, len(servers))
	for name := range servers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- tools.mcp-server-command

// runMcpServerCommand mirrors internal/config/validate.go's validateTool
// (absolute path, exists, regular file) — the same three properties the
// CLI-tool `binary` field is required to have — applied to
// mcp_servers.*.command, which validateMCPServers does not check at all
// beyond non-emptiness (§7.4). An empty command is deliberately NOT flagged
// here: that is config.validate's own "command is required" failure
// (validateMCPServers), and re-reporting it under a second check ID would be
// noise, not a distinct finding — this check's whole reason to exist is the
// PATH-resolution hijack a *present but relative* command opens, which an
// empty command cannot exhibit at all.
func runMcpServerCommand(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range mcpSortedServerNames(in.Config.MCPServers) {
		srv := in.Config.MCPServers[name]
		cmd := srv.Command
		if cmd == "" {
			continue
		}
		path := At("mcp_servers", name, "command")

		if !filepath.IsAbs(cmd) {
			findings = append(findings, Finding{
				Severity: SeverityFail,
				Title:    "MCP server command is not absolute",
				Detail: fmt.Sprintf(
					"mcp_servers.%s.command is %q — a non-absolute command resolves from whatever PATH the "+
						"serve process inherits at spawn time, letting anything earlier on that PATH shadow the "+
						"real binary; internal/config's validateMCPServers checks only that command is non-empty",
					name, cmd),
				Path:   path,
				Remedy: fmt.Sprintf("point mcp_servers.%s.command at the binary's absolute path (e.g. via `which %s`)", name, cmd),
				DocRef: "docs/configuration.qmd",
			})
			continue
		}

		fi, err := in.Env.Stat(cmd)
		switch {
		case err != nil:
			findings = append(findings, Finding{
				Severity: SeverityFail,
				Title:    "MCP server command does not exist",
				Detail:   fmt.Sprintf("mcp_servers.%s.command %q could not be found: %v", name, cmd, err),
				Path:     path,
				Remedy:   fmt.Sprintf("fix mcp_servers.%s.command so it points at an existing, executable file", name),
				DocRef:   "docs/configuration.qmd",
			})
		case !fi.Mode.IsRegular():
			findings = append(findings, Finding{
				Severity: SeverityFail,
				Title:    "MCP server command is not a regular file",
				Detail:   fmt.Sprintf("mcp_servers.%s.command %q resolves to a non-regular file (mode %s)", name, cmd, fi.Mode),
				Path:     path,
				Remedy:   fmt.Sprintf("point mcp_servers.%s.command at the actual server binary", name),
				DocRef:   "docs/configuration.qmd",
			})
		}
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- tools.mcp-server-args-unpinned

// mcpFetchRunners are the two package-runner commands design §7.4 names by
// name: `npx` (Node) and `uvx` (uv's `uv tool run` alias, Python). Matched by
// the basename of mcp_servers.*.command, case-insensitively — `command:
// /usr/bin/npx` and `command: npx` (already flagged separately by
// tools.mcp-server-command if unresolved) are the same runner.
var mcpFetchRunners = map[string]bool{
	"npx": true,
	"uvx": true,
}

// mcpFloatingVersionTags are package-spec version tags that name a MOVING
// target rather than pinning one — "@latest" is the design doc's own named
// example, and npm's other well-known dist-tags share the same problem.
var mcpFloatingVersionTags = map[string]bool{
	"latest": true,
	"next":   true,
	"canary": true,
	"beta":   true,
	"alpha":  true,
	"rc":     true,
}

// mcpFirstNonFlagArg returns the first element of args that does not start
// with "-", and whether one was found. npx/uvx invocations conventionally
// place the package spec immediately after any leading flags (`-y`), so this
// is the arg both runners' own docs treat as "the package" — a full argv
// parser modeling every runner flag's arity is out of scope for a doctor
// heuristic (this check's severity is fail because an unpinned fetch-execute
// IS the danger regardless of parse precision, not because the parse itself
// is exhaustive).
func mcpFirstNonFlagArg(args []string) (string, bool) {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, true
		}
	}
	return "", false
}

// mcpHasFlag reports whether args contains any of names verbatim.
func mcpHasFlag(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

// mcpSpecPinned reports whether spec — a package-runner argument, e.g.
// "@modelcontextprotocol/server-filesystem", "left-pad@1.3.0", "ruff@latest"
// — names an exact, non-floating version. The version separator is the LAST
// "@" in spec, deliberately not the first: an npm-scoped package name itself
// starts with "@" ("@scope/name"), so "@scope/name@1.2.3" must split after
// "name", not at the scope marker, and "@scope/name" with no version at all
// must not be misread as version "scope/name". A leading "@" at index 0 with
// nothing else is therefore never treated as a version marker.
func mcpSpecPinned(spec string) bool {
	_, version, ok := mcpSplitSpec(spec)
	if !ok || version == "" {
		return false
	}
	return !mcpFloatingVersionTags[strings.ToLower(version)]
}

// mcpSplitSpec splits spec into its bare package name and version, at the
// LAST "@" in spec — see mcpSpecPinned's doc comment for why last-not-first
// (an npm-scoped name itself starts with "@"). ok is false when spec carries
// no version marker at all (a bare "@scope/name" included, per the same
// scope-marker rule).
func mcpSplitSpec(spec string) (name, version string, ok bool) {
	at := strings.LastIndex(spec, "@")
	if at <= 0 {
		return spec, "", false // no "@" at all, or only the scope marker at index 0
	}
	return spec[:at], spec[at+1:], true
}

// mcpUnpinnedSpec returns the offending package spec and true when srv's
// command/args match design §7.4's fetch-and-execute shape with no version
// pin: an npx invocation carrying `-y`/`--yes` (npx's own non-interactive
// auto-install flag — without it npx would block on a confirmation prompt no
// spawned MCP server child ever gets to answer, so an npx call with no -y is
// not a live "runs unattended" risk this check can confirm), or ANY uvx
// invocation (uv tool run has no such interactive gate to begin with, so its
// mere use with an unpinned spec is already the risk).
// mcpSpecBase strips a version suffix (if any) off spec, for the remedy
// text's "pin an exact version" suggestion.
func mcpSpecBase(spec string) string {
	name, _, ok := mcpSplitSpec(spec)
	if !ok {
		return spec
	}
	return name
}

func mcpUnpinnedSpec(command string, args []string) (spec string, unpinned bool) {
	base := strings.ToLower(filepath.Base(command))
	if !mcpFetchRunners[base] {
		return "", false
	}
	if base == "npx" && !mcpHasFlag(args, "-y", "--yes") {
		return "", false
	}
	spec, found := mcpFirstNonFlagArg(args)
	if !found {
		return "", false
	}
	if mcpSpecPinned(spec) {
		return "", false
	}
	return spec, true
}

func runMcpServerArgsUnpinned(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range mcpSortedServerNames(in.Config.MCPServers) {
		srv := in.Config.MCPServers[name]
		spec, unpinned := mcpUnpinnedSpec(srv.Command, srv.Args)
		if !unpinned {
			continue
		}
		path := At("mcp_servers", name, "args").Elem(spec)
		findings = append(findings, Finding{
			Severity: SeverityFail,
			Title:    "MCP server runs an unpinned package",
			Detail: fmt.Sprintf(
				"mcp_servers.%s runs %s %s with package spec %q, which has no pinned version — this fetches and "+
					"executes whatever code the registry currently serves for that name at every serve start, with "+
					"this server's own env (including any env_files credentials) available to it. An absolute, "+
					"existing command alone (tools.mcp-server-command) does not close this: %s itself is the "+
					"fetch-and-execute vector",
				name, srv.Command, strings.Join(srv.Args, " "), spec, filepath.Base(srv.Command)),
			Path: path,
			Remedy: fmt.Sprintf(
				"pin an exact version in the package spec (e.g. %s@<version>), or install the server locally and "+
					"point mcp_servers.%s.command at its absolute binary path instead of %s",
				mcpSpecBase(spec), name, filepath.Base(srv.Command)),
			DocRef: "docs/configuration.qmd",
		})
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- tools.mcp-server-inert

// runMcpServerInert mirrors tools.inert's rationale (§7.4) but at warn, not
// info: a CLI tool with zero allow rules registers zero MCP tools and calls
// nothing (config.validateTool permits it as the deliberate
// wraptool_discover bootstrap state), but an mcp_servers entry with zero
// allow rules still SPAWNS the upstream child at every serve start — a
// running, credentialed process granting no capability at all is worth
// flagging on its own (audit M1), not merely informational.
func runMcpServerInert(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range mcpSortedServerNames(in.Config.MCPServers) {
		srv := in.Config.MCPServers[name]
		if len(srv.Allow) > 0 {
			continue
		}
		findings = append(findings, Finding{
			Severity: SeverityWarn,
			Title:    "MCP server is spawned but grants no capability",
			Detail: fmt.Sprintf(
				"mcp_servers.%s has zero allow rules, so no upstream tool is ever permitted, but the child "+
					"process is still spawned (and its credentials/env still exposed to it) at every serve start",
				name),
			Path:   At("mcp_servers", name),
			Remedy: fmt.Sprintf("add an allow rule under mcp_servers.%s, or remove the entry if it is not in use", name),
			DocRef: "docs/configuration.qmd",
		})
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- policy.mcp-dangerous-tool

// mcpDangerousToolShapes is the execute-or-write name-shape list design §7.7
// gives verbatim: "*execute*", "*command*", "*shell*", "*write_file*",
// "*delete*". Matched as a case-insensitive substring of the allow rule's
// Tool name — MCPAllowRule (internal/config/config.go) is a bare {Tool
// string}, with no subcommand or flag structure at all to constrain further,
// so a name heuristic is the only lint available; the nine CLI-side
// policy.* heuristics (D1-D4) have no MCP-side counterpart without this one.
var mcpDangerousToolShapes = []string{"execute", "command", "shell", "write_file", "delete"}

// mcpDangerousShape returns the matched shape substring and true when tool
// matches one of mcpDangerousToolShapes.
func mcpDangerousShape(tool string) (string, bool) {
	lower := strings.ToLower(tool)
	for _, shape := range mcpDangerousToolShapes {
		if strings.Contains(lower, shape) {
			return shape, true
		}
	}
	return "", false
}

func runMcpDangerousTool(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range mcpSortedServerNames(in.Config.MCPServers) {
		srv := in.Config.MCPServers[name]
		for _, rule := range srv.Allow {
			shape, dangerous := mcpDangerousShape(rule.Tool)
			if !dangerous {
				continue
			}
			findings = append(findings, Finding{
				Severity: SeverityWarn,
				Title:    "MCP server allows an execute-or-write-shaped tool",
				Detail: fmt.Sprintf(
					"mcp_servers.%s.allow permits upstream tool %q, which matches the %q execute-or-write name "+
						"shape — MCPAllowRule has no subcommand or flag structure to constrain it further, so this "+
						"name heuristic is the only signal available before the call reaches the upstream server",
					name, rule.Tool, shape),
				Path: At("mcp_servers", name, "allow").Item("tool", rule.Tool),
				Remedy: fmt.Sprintf(
					"confirm mcp_servers.%s's allow entry for %q is intentional and scoped as tightly as the "+
						"upstream server allows", name, rule.Tool),
				DocRef: "design/doctor/wraptool-doctor.md",
			})
		}
	}
	return Found(findings...)
}
