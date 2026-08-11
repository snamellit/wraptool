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

// mcpNpxValuelessFlags and mcpUvxValuelessFlags are the switches (no value
// argument follows) each runner documents, per runner since a bare "-y" for
// npx and a bare "--refresh" for uvx are meaningless to the OTHER runner.
// mcpFirstNonFlagArg's positional fallback may skip over any of these ahead
// of the first positional without losing track of what's the spec — see its
// own doc comment for why every OTHER leading flag stops the scan instead.
var (
	mcpNpxValuelessFlags = map[string]bool{
		"-y": true, "--yes": true, "--no": true,
		"-q": true, "--quiet": true, "--silent": true,
	}
	mcpUvxValuelessFlags = map[string]bool{
		"--refresh": true, "--no-cache": true, "--isolated": true,
		"--offline": true, "--quiet": true, "--native-tls": true,
	}
)

// mcpKnownValuelessFlags returns the runner-appropriate switch set for base
// (mcpFetchRunners' key), or nil for an unrecognized runner.
func mcpKnownValuelessFlags(base string) map[string]bool {
	switch base {
	case "npx":
		return mcpNpxValuelessFlags
	case "uvx":
		return mcpUvxValuelessFlags
	default:
		return nil
	}
}

// mcpNpxSpecFlags and mcpUvxSpecFlags are the flags each runner documents as
// explicitly naming the package spec, as opposed to the implicit "first
// positional" convention: npx's `-p`/`--package`, uv's `--from`. Kept PER
// RUNNER, not a single shared list, because uv's own `-p`/`--python` short
// flag collides literally with npx's `-p`/`--package` — treating "-p" as a
// universal spec flag would misread `uvx -p 3.12 ruff`'s Python-version
// value as the package spec, the exact false-positive class ruling B7-1
// exists to close. When one of a runner's own spec flags is present, its
// VALUE is the package spec — never the trailing "bin name" positional
// uvx's `--from PKG CMD` form carries (CMD selects which entry point of PKG
// to run; it is not a package spec at all and must never be evaluated as
// one), and never some earlier, unrelated flag's own value either.
var (
	mcpNpxSpecFlags = []string{"-p", "--package"}
	mcpUvxSpecFlags = []string{"--from"}
)

// mcpRunnerSpecFlags returns base's own spec-naming flags (mcpNpxSpecFlags /
// mcpUvxSpecFlags), or nil for an unrecognized runner.
func mcpRunnerSpecFlags(base string) []string {
	switch base {
	case "npx":
		return mcpNpxSpecFlags
	case "uvx":
		return mcpUvxSpecFlags
	default:
		return nil
	}
}

// mcpSpecArg returns the package-spec argument for an npx/uvx invocation (base
// is the runner's basename) and whether one could be identified at all.
//
// ONE bounded left-to-right walk that STOPS at the first bare positional. That
// bound is the whole point: everything after the spec belongs to the spawned
// SERVER, not to the runner, so an unbounded scan reads the child's own flags
// as if they were the runner's — a pinned `npx -y pkg@1.0.0 --package foo`
// would otherwise report foo as its package spec. The same defect was fixed
// twice before, once for the positional scan and once for the flag scan; a
// single bounded pass is what makes it unreachable rather than relocated.
//
// At each argument, in order:
//   - -- ends the runner's own arguments: the spec cannot be identified.
//   - a bare positional IS the spec (npx/uvx's implicit convention).
//   - a joined --flag=value: the value travels with its own flag, so it can
//     never be mistaken for the spec; it is the spec only if the flag is one of
//     base's own spec flags.
//   - one of base's spec flags: its VALUE is the spec.
//   - one of base's KNOWN valueless switches: skipped.
//   - anything else — an unrecognized bare flag — stops the walk with ok=false.
//     An unidentifiable spec is not evidence of an unpinned one, and reporting
//     a guess is how this check twice told operators their pinned invocation
//     was unpinned.
func mcpSpecArg(base string, args []string) (spec string, ok bool) {
	specFlags := mcpRunnerSpecFlags(base)
	valueless := mcpKnownValuelessFlags(base)
	isSpecFlag := func(a string) bool {
		for _, f := range specFlags {
			if a == f {
				return true
			}
		}
		return false
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return "", false
		case !strings.HasPrefix(a, "-"):
			return a, true
		}
		if flag, val, joined := strings.Cut(a, "="); joined {
			if isSpecFlag(flag) {
				return val, true
			}
			continue
		}
		if isSpecFlag(a) {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
		if valueless[a] {
			continue
		}
		return "", false
	}
	return "", false
}

// mcpRangeVersion reports whether an npm version is a RANGE or wildcard rather
// than an exact version. "@*", "@^1.2.0", "@>=1.0" and "@1.x" resolve to
// whatever the registry serves at that moment, which is the same moving target
// "@latest" names — the dist-tag list alone let every one of them through.
func mcpRangeVersion(version string) bool {
	if strings.ContainsAny(version, "*^~><| ") {
		return true
	}
	for _, seg := range strings.Split(version, ".") {
		if seg == "x" || seg == "X" {
			return true
		}
	}
	return false
}

// mcpLocalSpec reports whether spec names a local directory or tarball rather
// than a registry package. `npx ./local-server` contacts no registry, so the
// finding's central claim — that this fetches whatever the registry currently
// serves — would be false of it, and a check may not assert what it cannot know.
//
// Keyed on the PREFIX only. A generic "contains a separator" rule is wrong:
// every scoped npm package name carries one ("@scope/name"), so it would
// exempt the most common registry spec there is.
func mcpLocalSpec(spec string) bool {
	return strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") ||
		strings.HasPrefix(spec, "~") || strings.HasPrefix(spec, "\\")
}

// mcpSpecPinned reports whether spec — a package-runner argument, e.g.
// "@modelcontextprotocol/server-filesystem", "left-pad@1.3.0", "ruff@latest",
// "mcp-server-git==2024.11.5" — names an exact, non-floating version. Two
// ecosystems' pin syntax are recognized, since mcpFetchRunners covers both:
// npm's (npx) trailing "@version", and PEP 440's (uvx/uv, pip) "==version" —
// uv's own documented exact-pin form. The npm version separator is the LAST
// "@" in spec, deliberately not the first: an npm-scoped package name itself
// starts with "@" ("@scope/name"), so "@scope/name@1.2.3" must split after
// "name", not at the scope marker, and "@scope/name" with no version at all
// must not be misread as version "scope/name". A leading "@" at index 0 with
// nothing else is therefore never treated as a version marker.
func mcpSpecPinned(spec string) bool {
	if mcpPEP440ExactPinned(spec) {
		return true
	}
	_, version, ok := mcpSplitSpec(spec)
	if !ok || version == "" {
		return false
	}
	if mcpFloatingVersionTags[strings.ToLower(version)] {
		return false
	}
	return !mcpRangeVersion(version)
}

// mcpPEP440ExactPinned reports whether spec carries a PEP 440 exact-version
// pin — "==" is the only operator this check recognizes as pinned (uv/pip's
// documented exact-pin form, e.g. "mcp-server-git==2024.11.5"); a range
// operator ("mcp-server-git>=1.0") does not pin an exact version and is left
// unpinned. The "==" must not be the very first rune (an empty package
// name) and must have a non-empty version after it.
func mcpPEP440ExactPinned(spec string) bool {
	idx := strings.Index(spec, "==")
	return idx > 0 && spec[idx+2:] != ""
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

// mcpSpecBase strips a version pin (if any) off spec, for the remedy text's
// "pin an exact version" suggestion — a PEP 440 "==version" suffix first
// (mcpPEP440ExactPinned's marker), else an npm "@version" suffix
// (mcpSplitSpec), so an unpinned spec that already carries a non-exact PEP
// 440 operator ("mcp-server-git>=1.0") is not left dangling in front of a
// remedy-appended pin.
func mcpSpecBase(spec string) string {
	if idx := strings.Index(spec, "=="); idx > 0 {
		return spec[:idx]
	}
	name, _, ok := mcpSplitSpec(spec)
	if !ok {
		return spec
	}
	return name
}

// mcpPinExample returns the runner-appropriate "pin an exact version"
// example for spec's remedy text: PEP 440's "==version" for uvx, npm's
// "@version" for npx — using the wrong ecosystem's syntax would itself
// produce an invalid spec (ruling B7(c): the remedy must never append
// "@<version>" onto a spec that already carries "==", which mixing syntaxes
// unconditionally would risk).
func mcpPinExample(command, spec string) string {
	name := mcpSpecBase(spec)
	if strings.ToLower(filepath.Base(command)) == "uvx" {
		return name + "==<version>"
	}
	return name + "@<version>"
}

// mcpUnpinnedSpec returns the offending package spec and true when srv's
// command/args match design §7.4's fetch-and-execute shape with no version
// pin: ANY npx or uvx invocation, regardless of `-y`/`--yes`.
//
// This check used to gate npx on `-y`/`--yes` being present, on the premise
// that npx without it "would block on a confirmation prompt" a spawned MCP
// server child could never answer. That premise is false (ruling B7-2):
// measured directly against the built binary with stdin redirected from
// /dev/null — internal/mcpproxy's own spawn shape, no TTY — `npx cowsay hi
// </dev/null` printed npm's install warning and RAN, with no prompt. With no
// TTY, npm's exec confirmation prompt has nothing to block on and proceeds
// non-interactively, so an unpinned npx server with NO `-y` at all was
// silently passing this check while remaining exactly as dangerous as one
// that carries it. The candidate spec itself comes from mcpSpecArg, not a
// bare positional scan — see its doc comment for why an explicit spec
// flag's value must win over any positional guess, and mcpFirstNonFlagArg's
// for why an unrecognized flag ahead of the positional yields no spec at
// all rather than a guess.
func mcpUnpinnedSpec(command string, args []string) (spec string, unpinned bool) {
	base := mcpRunnerBase(command)
	if !mcpFetchRunners[base] {
		return "", false
	}
	spec, found := mcpSpecArg(base, args)
	if !found {
		return "", false
	}
	if mcpLocalSpec(spec) || mcpSpecPinned(spec) {
		return "", false
	}
	return spec, true
}

// mcpRunnerBase reduces a command path to the runner name mcpFetchRunners is
// keyed on. The executable-suffix strip is what makes the check work on
// Windows at all: npm ships npx.cmd/npx.ps1 there, so the runner IS "npx.cmd"
// — and unlike a permission check this one declares no NeedsModeBits, so
// without the strip it would not be skipped with a reason, it would silently
// pass on the exact shape it exists to catch.
func mcpRunnerBase(command string) string {
	// Cut on both separators, not filepath.Base alone: doctor may run on Linux
	// against a config authored on Windows, where filepath.Base never splits a
	// backslash path and would hand back the whole string.
	base := strings.ToLower(filepath.Base(command))
	if i := strings.LastIndexByte(base, '\\'); i >= 0 {
		base = base[i+1:]
	}
	for _, ext := range []string{".cmd", ".exe", ".bat", ".ps1"} {
		if trimmed, cut := strings.CutSuffix(base, ext); cut {
			return trimmed
		}
	}
	return base
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
		base := mcpRunnerBase(srv.Command)
		detail := fmt.Sprintf(
			"mcp_servers.%s runs %s %s with package spec %q, which has no pinned version — this fetches and "+
				"executes whatever code the registry currently serves for that name at every serve start, with "+
				"this server's own env (including any env_files credentials) available to it. An absolute, "+
				"existing command alone (tools.mcp-server-command) does not close this: %s itself is the "+
				"fetch-and-execute vector",
			name, srv.Command, strings.Join(srv.Args, " "), spec, filepath.Base(srv.Command))
		if base == "npx" {
			detail += ". npm proceeds non-interactively even without --yes because the spawned child has no " +
				"TTY to prompt on — an npx invocation with no -y/--yes is not a lesser risk"
		}
		findings = append(findings, Finding{
			Severity: SeverityFail,
			Title:    "MCP server runs an unpinned package",
			Detail:   detail,
			Path:     path,
			Remedy: fmt.Sprintf(
				"pin an exact version in the package spec (e.g. %s), or install the server locally and "+
					"point mcp_servers.%s.command at its absolute binary path instead of %s",
				mcpPinExample(srv.Command, spec), name, filepath.Base(srv.Command)),
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
