// tools.go implements a slice of the tools.* group (design §7.4): the CLI
// tool wiring checks that internal/config's validateTool does not cover —
// executable mode bits, a group/other-writable path to the binary, an
// invalid working_dir, and env_files integrity on BOTH tools.* and
// mcp_servers.* — plus tools.inert. tools.mcp-server-* (the mcp_servers-only
// checks) is B7's file (mcpservers.go); this file reads
// mcp_servers.*.env_files only, nothing else on that side.
//
// validateTool (internal/config/validate.go) already rejects a non-absolute,
// missing, or non-regular binary at config.validate time — that is a
// different check (config.validate), not this file's concern. Every check
// here is gated on Meta.Needs so it only ever sees a binary/working_dir/
// env_files entry after those preconditions plausibly hold; a subject this
// file cannot resolve (relative path, missing file) is silently skipped
// rather than double-reported, on the assumption config.validate already
// said so.
package doctor

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/pti/wraptool/internal/config"
)

const (
	toolBinaryExecutable CheckID = "tools.binary-executable"
	toolBinaryWritable   CheckID = "tools.binary-writable"
	toolWorkingDir       CheckID = "tools.working-dir"
	toolEnvFileMissing   CheckID = "tools.env-file-missing"
	toolEnvFilePerms     CheckID = "tools.env-file-perms"
	toolInert            CheckID = "tools.inert"
)

// toolBinaryExecutableMeta, toolBinaryWritableMeta, ... are the checks' Meta
// literals, named so tools_test.go's TestToolCheckMeta can assert each
// check's ID/Needs directly against the same literal init() registers —
// never through Registered() (plan §1 rule 8, pool.go models this).
var (
	toolBinaryExecutableMeta = Meta{
		ID:      toolBinaryExecutable,
		Needs:   NeedsConfig | NeedsModeBits,
		Summary: "checks that every configured tool binary has an execute bit set for the current user",
	}
	toolBinaryWritableMeta = Meta{
		ID:      toolBinaryWritable,
		Needs:   NeedsConfig | NeedsModeBits,
		Summary: "checks that no tool binary, or a directory leading to it, is group- or other-writable",
	}
	toolWorkingDirMeta = Meta{
		ID:      toolWorkingDir,
		Needs:   NeedsConfig,
		Summary: "checks that a configured working_dir is an existing absolute directory",
	}
	toolEnvFileMissingMeta = Meta{
		ID:      toolEnvFileMissing,
		Needs:   NeedsConfig,
		Summary: "checks that every tools.*/mcp_servers.* env_files entry exists and is readable",
	}
	toolEnvFilePermsMeta = Meta{
		ID:      toolEnvFilePerms,
		Needs:   NeedsConfig | NeedsModeBits,
		Summary: "checks that every tools.*/mcp_servers.* env_files entry is not group- or other-readable",
	}
	toolInertMeta = Meta{
		ID:      toolInert,
		Needs:   NeedsConfig,
		Summary: "reports a tool with zero allow rules, which registers no MCP tools",
	}
)

func init() {
	register(toolBinaryExecutableMeta, runToolBinaryExecutable)
	register(toolBinaryWritableMeta, runToolBinaryWritable)
	register(toolWorkingDirMeta, runToolWorkingDir)
	register(toolEnvFileMissingMeta, runToolEnvFileMissing)
	register(toolEnvFilePermsMeta, runToolEnvFilePerms)
	register(toolInertMeta, runToolInert)
}

// ---------------------------------------------------------------- shared helpers

// toolSortedNames returns m's keys sorted — every check below ranges over
// cfg.Tools/cfg.MCPServers, and TestChecksAreDeterministic runs each check 8
// times against a >=2-entry fixture map, which an unsorted `range` fails at
// better than even odds (invariants_test.go's invariantsDeterminismRuns).
func toolSortedNames[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// toolResolvedBinary resolves tc.Binary through Env.EvalSymlinks, the same
// resolution validateTool performs. ok is false whenever the binary cannot be
// meaningfully checked here: empty, relative, or unresolvable — every one of
// those is already config.validate's own finding, not this file's.
func toolResolvedBinary(env Env, binary string) (resolved string, ok bool) {
	if binary == "" || !filepath.IsAbs(binary) {
		return "", false
	}
	resolved, err := env.EvalSymlinks(binary)
	if err != nil {
		return "", false
	}
	return resolved, true
}

// toolExpandHome expands a leading "~" the same way
// internal/config.expandPath does (bare "~" and "~/..."), but through Env
// rather than os.UserHomeDir — Env is the only host seam this package may use
// (host.go, CLOSED, plan §1 rule 10), and env_files entries are expanded
// lazily at resolveEnv time, so in.Config's own EnvFiles strings are still
// exactly what the operator wrote.
//
// Platform-aware: os.UserHomeDir (what expandPath calls) reads %USERPROFILE%
// on Windows and never consults HOME, and this check runs there (
// tools.env-file-missing declares NeedsConfig, not NeedsModeBits, so it is
// not Windows-skipped) — docs/platform-windows.qmd documents PowerShell as a
// first-class path, where $env:HOME is unset by default. A HOME-only lookup
// would report a present env_files entry (e.g. a documented
// ~/.config/wraptool/kubectl.env) as missing on every fresh Windows install.
// goos=="windows" therefore prefers USERPROFILE (mirroring os.UserHomeDir's
// own key), falling back to HOME; every other platform stays HOME-only, per
// expandPath. An unresolvable home (both unset, or non-Windows with no HOME)
// leaves the path unchanged, mirroring expandPath's own fallback.
func toolExpandHome(env Env, goos, path string) string {
	home := env.Getenv("HOME")
	if goos == "windows" {
		if up := env.Getenv("USERPROFILE"); up != "" {
			home = up
		}
	}
	if home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return path
}

// ---------------------------------------------------------------- tools.binary-executable

// toolExecutableBy mirrors env.go's envDirWritable, but for the execute bit
// (0100/0010/0001) instead of the write bit: the owner bit if uid matches,
// else the group bit if gid matches, else the other bit; falls back to "any
// execute bit at all" when ownership is unknown (no syscall.Stat_t, or a
// MapEnv fixture that left UID/GID at -1). There is deliberately no uid 0
// case — wraptool refuses to run as root before any check executes
// (cmd/rootuser.go) — and, like envDirWritable, gid membership is
// necessarily approximate: a supplementary group this package cannot see may
// still grant access, so a set group-execute bit is treated as executable
// even when the primary gid does not match.
func toolExecutableBy(fi FileInfo, uid, gid int) bool {
	perm := fi.Perm()
	switch {
	case !fi.OwnerKnown():
		return perm&0o111 != 0
	case uid >= 0 && fi.UID == uid:
		return perm&0o100 != 0
	case gid >= 0 && fi.GID == gid:
		return perm&0o010 != 0
	default:
		if perm&0o010 != 0 {
			return true // group-execute bit set; a supplementary group might match
		}
		return perm&0o001 != 0
	}
}

// runToolBinaryExecutable fires when a resolved, regular-file tool binary
// has no execute bit for the current user — validateTool checks
// absolute/exists/regular but never the mode bits (design §7.4), so a
// binary installed 0644 passes config.validate cleanly and only fails at the
// first exec.CommandContext call.
func runToolBinaryExecutable(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range toolSortedNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		resolved, ok := toolResolvedBinary(in.Env, tc.Binary)
		if !ok {
			continue
		}
		fi, err := in.Env.Stat(resolved)
		if err != nil || !fi.Mode.IsRegular() {
			continue // existence/regularity is config.validate's finding, not this one's
		}
		if toolExecutableBy(fi, in.Env.Getuid(), in.Env.Getgid()) {
			continue
		}
		findings = append(findings, Finding{
			Severity: SeverityFail,
			Title:    "Tool binary is not executable",
			Detail: fmt.Sprintf(
				"tools.%s: binary %s (resolved to %s) is mode %s, with no execute bit for the current user — every call to this tool will fail at exec",
				name, tc.Binary, resolved, fi.Mode),
			Path:   At("tools", name, "binary"),
			Remedy: fmt.Sprintf("chmod +x %s", resolved),
			DocRef: "docs/configuration.qmd",
		})
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- tools.binary-writable

// toolAncestorDirs returns every directory ancestor of path, from the
// filesystem root down to (but not including) path itself. Each entry
// governs whether path's own directory ENTRY could be replaced from
// underneath it — POSIX write permission on a directory controls adding,
// removing, or renaming the entries inside it, not the target's own mode
// bits — so this is the correct walk for "could this binary be substituted",
// not merely "is some ancestor's mode loose".
func toolAncestorDirs(path string) []string {
	var dirs []string
	cur := filepath.Dir(path)
	for {
		dirs = append(dirs, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	slices.Reverse(dirs)
	return dirs
}

// toolWritableSegments is the ordered list runToolBinaryWritable inspects:
// the UNION (de-duplicated, root-most first) of the ancestor directories of
// BOTH binary (as configured — the symlink itself, if tc.Binary names one)
// and resolved (EvalSymlinks' target), followed by resolved itself. Checking
// only resolved's ancestors misses the case that matters most: a
// group/other-writable directory holding the SYMLINK — replace the link and
// every invocation of the tool is hijacked, with resolved's own ancestor
// chain (e.g. a clean /gnu/store/.../bin) never seeing it. The symlink node
// itself is deliberately never Stat'd here: an lstat'd symlink is always
// lrwxrwxrwx, which would false-positive on every symlinked binary
// regardless of who can replace it — only its DIRECTORY entry matters, and
// that directory is already the leaf of toolAncestorDirs(binary).
func toolWritableSegments(binary, resolved string) []string {
	var dirs []string
	seen := make(map[string]bool)
	for _, chain := range [][]string{toolAncestorDirs(binary), toolAncestorDirs(resolved)} {
		for _, d := range chain {
			if seen[d] {
				continue
			}
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return append(dirs, resolved)
}

// toolGroupLabel names a FileInfo's GID for a finding's Detail. There is no
// group-NAME lookup here: Env is closed (plan §1 rule 10) and has no
// supplementary accessor beyond Getgid/FileInfo.GID, so the numeric gid is
// the most this file can name — "its group" in the design catalog's wording.
func toolGroupLabel(gid int) string {
	if gid < 0 {
		return "gid unknown"
	}
	return fmt.Sprintf("gid %d", gid)
}

// toolHomebrewShaped reports whether seg IS the one shape design §7.4
// documents as a legitimate, unavoidable exception: Intel-macOS Homebrew's
// own group-writable prefix (or a path under it). This check cannot know the
// host is actually macOS, or that seg is actually Homebrew's doing rather
// than an unrelated misconfiguration that merely happens to share the
// prefix — so the reassurance below is gated on this, rather than attached
// to every finding regardless of segment: an unconditional "safe to leave
// as-is" on an arbitrary /opt or /home/shared segment would assert
// something this check cannot know, teaching an operator to ignore a real
// finding.
func toolHomebrewShaped(seg string) bool {
	return seg == "/usr/local" || strings.HasPrefix(seg, "/usr/local/")
}

// runToolBinaryWritable fires on the FIRST group- or other-writable segment
// (root to leaf, across both tc.Binary's own ancestor chain and resolved's —
// see toolWritableSegments) in a tool binary's path — arbitrary code could be
// substituted inside the credential boundary, either by rewriting resolved
// in place or by replacing the symlink named by binary: to point somewhere
// else entirely. warn, not fail (design §7.4): Intel-macOS Homebrew makes
// /usr/local group-writable by design and the repo ships a tap (README/docs),
// so this is a shape that is dangerous in most deployments but legitimately
// unavoidable in some.
func runToolBinaryWritable(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range toolSortedNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		resolved, ok := toolResolvedBinary(in.Env, tc.Binary)
		if !ok {
			continue
		}
		for _, seg := range toolWritableSegments(tc.Binary, resolved) {
			fi, err := in.Env.Stat(seg)
			if err != nil {
				continue
			}
			if !fi.GroupOrOtherWritable() {
				continue
			}
			detail := fmt.Sprintf(
				"tools.%s: binary %s resolves through %s, which is group- or other-writable (mode %s, %s) — "+
					"this lets arbitrary code be substituted inside the credential boundary",
				name, tc.Binary, seg, fi.Mode, toolGroupLabel(fi.GID))
			remedy := fmt.Sprintf(
				"tighten permissions on %s (or move the binary to a path with no group/other-writable segment)", seg)
			if toolHomebrewShaped(seg) {
				detail += ". Expected and safe to leave as-is on Intel-macOS Homebrew, whose /usr/local is group-writable by design"
				remedy += ", unless this is the expected Homebrew layout"
			}
			findings = append(findings, Finding{
				Severity: SeverityWarn,
				Title:    "Tool binary is reachable through a group- or other-writable path",
				Detail:   detail,
				Path:     At("tools", name, "binary"),
				Remedy:   remedy,
				DocRef:   "docs/configuration.qmd",
			})
			break // report the highest (root-most) offending segment only
		}
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- tools.working-dir

// runToolWorkingDir fires when tools.<name>.working_dir is set but is not an
// existing absolute directory — validated nowhere today. warn, not fail
// (design §7.4 amendment): working_dir is only the LAST fallback in
// paths.ResolveCWD (internal/paths/context.go) — an explicit client ?cwd=
// wins first, a unix-socket peer PID's /proc/<pid>/cwd second — and every
// harness config wraptool generates appends ?cwd= unconditionally unless
// --global (internal/harness/harness.go's appendCWD; cmd/serve.go's
// cwdContextFunc injects it), so under `wraptool up`/`wraptool init` the
// configured value is never consulted at all. This check cannot know whether
// any given deployment ever reaches the fallback, so it cannot assert "every
// call will fail" the way tools.binary-executable can (a bad binary has no
// analogous override) — it can only report what IS knowable: this is the
// fallback used when the client supplies no ?cwd= and no peer-PID cwd
// resolves, and a bad value fails the call when that fallback is reached.
func runToolWorkingDir(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range toolSortedNames(in.Config.Tools) {
		wd := in.Config.Tools[name].WorkingDir
		if wd == "" {
			continue
		}
		reason := ""
		switch {
		case !filepath.IsAbs(wd):
			reason = "is not an absolute path"
		default:
			fi, err := in.Env.Stat(wd)
			switch {
			case err != nil:
				reason = fmt.Sprintf("does not exist: %v", err)
			case !fi.IsDir():
				reason = "is not a directory"
			}
		}
		if reason == "" {
			continue
		}
		findings = append(findings, Finding{
			Severity: SeverityWarn,
			Title:    "working_dir is not an existing absolute directory",
			Detail: fmt.Sprintf(
				"tools.%s: working_dir %q %s — this is only the fallback paths.ResolveCWD uses when the client "+
					"supplies no ?cwd= and no peer-PID cwd is resolvable (every wraptool-generated harness config "+
					"appends ?cwd=, so the fallback is not consulted there); when it IS used, the call fails at exec",
				name, wd, reason),
			Path:   At("tools", name, "working_dir"),
			Remedy: "set working_dir to an existing absolute directory, or remove it",
			DocRef: "docs/configuration.qmd",
		})
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- env_files (tools.* AND mcp_servers.*)

// toolEnvFileSubject is one tools.* or mcp_servers.* entry that declares
// env_files — the shared subject runToolEnvFileMissing/runToolEnvFilePerms
// both range over, since design §7.4 scopes both checks to "tools.* OR
// mcp_servers.*" identically.
type toolEnvFileSubject struct {
	Group string // "tools" or "mcp_servers" — the config's own top-level key
	Name  string
	Files []string
}

// toolEnvFileSubjects returns every tools.*/mcp_servers.* entry with a
// non-empty env_files, tools first then mcp_servers, each sorted by name —
// deterministic for the same reason toolSortedNames is.
func toolEnvFileSubjects(cfg *config.Config) []toolEnvFileSubject {
	var out []toolEnvFileSubject
	for _, name := range toolSortedNames(cfg.Tools) {
		if files := cfg.Tools[name].EnvFiles; len(files) > 0 {
			out = append(out, toolEnvFileSubject{Group: "tools", Name: name, Files: files})
		}
	}
	for _, name := range toolSortedNames(cfg.MCPServers) {
		if files := cfg.MCPServers[name].EnvFiles; len(files) > 0 {
			out = append(out, toolEnvFileSubject{Group: "mcp_servers", Name: name, Files: files})
		}
	}
	return out
}

// runToolEnvFileMissing fires when a tools.*/mcp_servers.* env_files entry
// cannot be read at all — missing or unreadable, design §7.4. Uses
// Env.ReadFile directly (the same primitive internal/config's resolveEnv
// uses via os.ReadFile) rather than Stat-then-permission-arithmetic, since
// "unreadable" is exactly what ReadFile failing means, on every platform.
func runToolEnvFileMissing(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, subj := range toolEnvFileSubjects(in.Config) {
		for _, raw := range subj.Files {
			expanded := toolExpandHome(in.Env, in.GOOS, raw)
			if _, err := in.Env.ReadFile(expanded); err != nil {
				findings = append(findings, Finding{
					Severity: SeverityFail,
					Title:    "env_files entry is missing or unreadable",
					Detail: fmt.Sprintf("%s.%s: env_files entry %q (%s): %v",
						subj.Group, subj.Name, raw, expanded, err),
					Path:   At(subj.Group, subj.Name, "env_files").Elem(raw),
					Remedy: "create the file at that path, fix the path, or remove the env_files entry",
					DocRef: "docs/configuration.qmd",
				})
			}
		}
	}
	return Found(findings...)
}

// runToolEnvFilePerms fires when an env_files entry IS readable but is
// group- or other-readable — these hold credentials by definition (design
// §7.4). Skips an entry runToolEnvFileMissing would already report (Stat
// failing there too) rather than double-reporting the same root cause under
// two IDs.
func runToolEnvFilePerms(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, subj := range toolEnvFileSubjects(in.Config) {
		for _, raw := range subj.Files {
			expanded := toolExpandHome(in.Env, in.GOOS, raw)
			fi, err := in.Env.Stat(expanded)
			if err != nil {
				continue
			}
			if !fi.GroupOrOtherReadable() {
				continue
			}
			findings = append(findings, Finding{
				Severity: SeverityFail,
				Title:    "env_files entry is group- or other-readable",
				Detail: fmt.Sprintf(
					"%s.%s: env_files entry %q (%s) is mode %s — this file holds credentials by definition",
					subj.Group, subj.Name, raw, expanded, fi.Mode),
				Path:   At(subj.Group, subj.Name, "env_files").Elem(raw),
				Remedy: fmt.Sprintf("chmod 600 %s", expanded),
				DocRef: "docs/configuration.qmd",
			})
		}
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- tools.inert

// runToolInert fires, at info severity, for every tools.* entry with zero
// allow rules. This is NOT a problem (design §7.4): validateTool permits it
// explicitly as the deliberate bootstrap state wraptool_discover exists to
// serve, so it is info, never warn.
func runToolInert(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range toolSortedNames(in.Config.Tools) {
		if len(in.Config.Tools[name].Allow) > 0 {
			continue
		}
		findings = append(findings, Finding{
			Severity: SeverityInfo,
			Title:    "Tool has no allow rules",
			Detail: fmt.Sprintf(
				"tools.%s registers zero MCP tools until an allow rule is added — this is the deliberate "+
					"bootstrap state wraptool_discover exists to serve, and validateTool permits it explicitly",
				name),
			Path:   At("tools", name),
			DocRef: "docs/configuration.qmd",
		})
	}
	return Found(findings...)
}
