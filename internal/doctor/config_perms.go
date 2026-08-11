// config_perms.go implements the config.* file/directory posture group
// (design §7.2, step B3): whether config.yaml itself — and the directory that
// holds it — are locked down, whether the file leaks inline credentials to
// anyone who can read it, and whether a symlinked config will silently miss
// hot-reload.
//
// Every check here resolves in.ConfigPath through Env.EvalSymlinks first
// (config.file-perms, config.dir-perms, config.file-readable-secrets): a
// world-writable symlink pointing at a locked-down target is not the risk —
// a WRITABLE TARGET is, and rename-in-place substitution of the file (or the
// directory holding it) defeats a locked-down leaf regardless of how the
// config path itself is spelled. config.symlinked is the one check that
// deliberately does NOT resolve first, since its entire subject is the link
// itself.
package doctor

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pti/wraptool/internal/config"
)

const (
	configpermsFilePerms           CheckID = "config.file-perms"
	configpermsDirPerms            CheckID = "config.dir-perms"
	configpermsFileReadableSecrets CheckID = "config.file-readable-secrets"
	configpermsSymlinked           CheckID = "config.symlinked"
)

// configpermsFilePermsMeta, ... are the checks' Meta literals, named so
// config_perms_test.go can assert each check's ID/Needs directly against the
// same literal init() registers — never through Registered() (plan §1 rule
// 8: a test asserting a check's own behaviour must not go through the shared
// registry, which by the time this package's eight Phase-B siblings have
// merged also contains every one of THEIR checks).
var (
	configpermsFilePermsMeta = Meta{
		ID:      configpermsFilePerms,
		Needs:   NeedsConfig | NeedsModeBits,
		Summary: "checks that config.yaml, symlinks resolved, is not group- or other-writable",
	}
	configpermsDirPermsMeta = Meta{
		ID:      configpermsDirPerms,
		Needs:   NeedsConfig | NeedsModeBits,
		Summary: "checks that the directory holding the resolved config.yaml is not group- or other-writable",
	}
	configpermsFileReadableSecretsMeta = Meta{
		ID:    configpermsFileReadableSecrets,
		Needs: NeedsConfig | NeedsModeBits,
		Summary: "checks for inline env: variables with credential-shaped names in a " +
			"config.yaml that is group- or other-readable",
	}
	configpermsSymlinkedMeta = Meta{
		ID:      configpermsSymlinked,
		Needs:   NeedsConfig,
		Summary: "warns when config.yaml is a symlink, which hot-reload's file watch does not follow",
	}
)

func init() {
	register(configpermsFilePermsMeta, runConfigPermsFilePerms)
	register(configpermsDirPermsMeta, runConfigPermsDirPerms)
	register(configpermsFileReadableSecretsMeta, runConfigPermsFileReadableSecrets)
	register(configpermsSymlinkedMeta, runConfigPermsSymlinked)
}

// configpermsResolve resolves in.ConfigPath through Env.EvalSymlinks. ok is
// false when the path could not be resolved at all (the file went away
// between the engine's own lenient load and this check running, or a broken
// symlink target) — every caller treats that as Skipped, mirroring
// config_load.go's own TOCTOU handling.
func configpermsResolve(in *Input) (resolved string, ok bool) {
	resolved, err := in.Env.EvalSymlinks(in.ConfigPath)
	if err != nil {
		return "", false
	}
	return resolved, true
}

// ---------------------------------------------------------------- config.file-perms

// runConfigPermsFilePerms fires when config.yaml, symlinks resolved, is
// group- or other-writable: whoever can write it grants themselves any tool
// (design §7.2). fail, never a judgement call — this is exactly the
// credential boundary the whitelist claims to be.
func runConfigPermsFilePerms(_ context.Context, in *Input) Result {
	resolved, ok := configpermsResolve(in)
	if !ok {
		return Skipped("config path could not be resolved (broken symlink, or the file went away)")
	}
	fi, err := in.Env.Stat(resolved)
	if err != nil {
		return Skipped("resolved config path %s could not be stat'd: %v", resolved, err)
	}
	if !fi.GroupOrOtherWritable() {
		return Pass()
	}
	return Found(Finding{
		Severity: SeverityFail,
		Title:    "Config file is group- or other-writable",
		Detail: fmt.Sprintf(
			"%s (symlinks resolved) is mode %04o (%s) — whoever can write it can grant themselves any tool",
			resolved, fi.Perm(), configpermsWritableDesc(fi)),
		Remedy: fmt.Sprintf("chmod go-w %s", resolved),
		DocRef: "docs/configuration.qmd#validating-a-config",
	})
}

// ---------------------------------------------------------------- config.dir-perms

// runConfigPermsDirPerms fires when the directory holding the RESOLVED
// config is group- or other-writable: rename-in-place substitution of the
// file defeats a locked-down leaf, so the containing directory is the same
// credential boundary as the file itself (design §7.2, same fail severity by
// the same argument as config.file-perms) — UNLESS the directory has the
// sticky bit set (e.g. mode 1777, as ls shows drwxrwxrwt). Sticky is exactly
// the mechanism that prevents the rename-in-place substitution this check
// exists to catch: with it set, only the file's owner (or root) may rename
// or unlink an entry inside the directory that they don't own, regardless of
// the directory's own group/other write bits. A sticky, world-writable
// directory (mode 1777, e.g. /tmp) is a configuration working as designed,
// not a fail (SeverityFail's own doc comment).
func runConfigPermsDirPerms(_ context.Context, in *Input) Result {
	resolved, ok := configpermsResolve(in)
	if !ok {
		return Skipped("config path could not be resolved (broken symlink, or the file went away)")
	}
	dir := filepath.Dir(resolved)
	fi, err := in.Env.Stat(dir)
	if err != nil {
		return Skipped("config directory %s could not be stat'd: %v", dir, err)
	}
	if fi.Mode&fs.ModeSticky != 0 {
		return Pass()
	}
	if !fi.GroupOrOtherWritable() {
		return Pass()
	}
	return Found(Finding{
		Severity: SeverityFail,
		Title:    "Config directory is group- or other-writable",
		Detail: fmt.Sprintf(
			"%s, the directory holding the resolved config %s, is mode %s, %s — anyone who can write into it "+
				"can replace config.yaml outright, which defeats a locked-down file",
			dir, resolved, configpermsModeString(fi), configpermsWritableDesc(fi)),
		Remedy: fmt.Sprintf("chmod go-w %s", dir),
		DocRef: "docs/configuration.qmd#validating-a-config",
	})
}

// configpermsModeString renders fi's type, permission, and sticky bits the
// way `ls -l`/`stat` show them on the SAME path (e.g. "1777 (drwxrwxrwt)"):
// config.dir-perms's subject is a directory, which can carry the sticky bit,
// and printing only the bare permission octet — dropping the type letter and
// the sticky bit that neutralizes this very check's risk — would show an
// operator a mode string that does not match what they see with `ls -ld`.
func configpermsModeString(fi FileInfo) string {
	perm := fi.Perm()
	octal := uint32(perm)
	sticky := fi.Mode&fs.ModeSticky != 0
	if sticky {
		octal |= 0o1000
	}

	masks := [9]fs.FileMode{0o400, 0o200, 0o100, 0o040, 0o020, 0o010, 0o004, 0o002, 0o001}
	letters := [9]byte{'r', 'w', 'x', 'r', 'w', 'x', 'r', 'w', 'x'}
	bits := make([]byte, 9)
	for i, mask := range masks {
		if perm&mask != 0 {
			bits[i] = letters[i]
		} else {
			bits[i] = '-'
		}
	}
	if sticky {
		if bits[8] == 'x' {
			bits[8] = 't'
		} else {
			bits[8] = 'T'
		}
	}

	typeChar := byte('-')
	switch {
	case fi.IsDir():
		typeChar = 'd'
	case fi.IsSymlink():
		typeChar = 'l'
	}

	return fmt.Sprintf("%04o (%c%s)", octal, typeChar, bits)
}

// configpermsWritableDesc names which of group/other carries the write bit,
// for a Detail string that says exactly what is wrong rather than just
// "writable".
func configpermsWritableDesc(fi FileInfo) string {
	perm := fi.Perm()
	group := perm&0o020 != 0
	other := perm&0o002 != 0
	switch {
	case group && other:
		return "group- and other-writable"
	case group:
		return "group-writable"
	default:
		return "other-writable"
	}
}

// ---------------------------------------------------------------- config.file-readable-secrets

// runConfigPermsFileReadableSecrets fires when the resolved config is
// group- or other-readable AND holds at least one inline `env:` entry, under
// `tools.*` or `mcp_servers.*`, whose KEY NAME is credential-shaped
// (configpermsIsCredentialKey; design §7.2). It reports the offending key
// names only, never the values (§10.1 forbids printing a secret) — but it
// also does not assert "credential" from a variable name alone: this repo's
// own docs/configuration.qmd prescribes plenty of non-secret inline env
// (CLOUDSDK_CORE_PROJECT, a HOME override) that must get no finding at all,
// not even a warn. A section whose inline env: names nothing
// credential-shaped is silent.
func runConfigPermsFileReadableSecrets(_ context.Context, in *Input) Result {
	resolved, ok := configpermsResolve(in)
	if !ok {
		return Skipped("config path could not be resolved (broken symlink, or the file went away)")
	}
	fi, err := in.Env.Stat(resolved)
	if err != nil {
		return Skipped("resolved config path %s could not be stat'd: %v", resolved, err)
	}
	if !fi.GroupOrOtherReadable() {
		return Pass()
	}
	if in.Config == nil {
		// NeedsConfig guarantees this in production; a hand-built test Input
		// could still leave it nil, so degrade to Skipped rather than panic.
		return Skipped("no loaded config to inspect for inline env: entries")
	}

	var findings []Finding
	for _, name := range configpermsSortedToolNames(in.Config.Tools) {
		if keys := configpermsCredentialKeys(in.Config.Tools[name].Env); len(keys) > 0 {
			findings = append(findings, configpermsSecretsFinding("tools", name, resolved, fi, keys))
		}
	}
	for _, name := range configpermsSortedMCPServerNames(in.Config.MCPServers) {
		if keys := configpermsCredentialKeys(in.Config.MCPServers[name].Env); len(keys) > 0 {
			findings = append(findings, configpermsSecretsFinding("mcp_servers", name, resolved, fi, keys))
		}
	}
	return Found(findings...)
}

func configpermsSecretsFinding(section, name, resolved string, fi FileInfo, keys []string) Finding {
	return Finding{
		Severity: SeverityFail,
		Title:    "World-readable config sets a credential-shaped env variable",
		Detail: fmt.Sprintf(
			"%s is mode %04o (%s), and %s.%s.env sets %d credential-shaped inline variable(s): %s",
			resolved, fi.Perm(), configpermsReadableDesc(fi), section, name, len(keys), strings.Join(keys, ", ")),
		Path: At(section, name, "env"),
		Remedy: fmt.Sprintf(
			"chmod go-r %s, or move these variables out of inline env into an env_files entry with its own tight permissions",
			resolved),
		DocRef: "docs/configuration.qmd#environment-isolation",
	}
}

// configpermsCredentialKeywords are the credential-shaped name fragments
// this check fires on, matched case-insensitively as a WHOLE `_`-delimited
// word/word-sequence in the key — never a loose substring (design §7.2
// ruling; the pool.home-perms precedent, commit 9101b2a, hit exactly that
// bug with a substring rule on "session"/"token"). A key like
// CLOUDSDK_CORE_PROJECT or a plain HOME override — both prescribed by
// docs/configuration.qmd — must not match any of these.
var configpermsCredentialKeywords = [][]string{
	{"TOKEN"},
	{"SECRET"},
	{"PASSWORD"},
	{"PASSWD"},
	{"CREDENTIAL"},
	{"CREDENTIALS"},
	{"API", "KEY"},
	{"APIKEY"},
	{"PRIVATE", "KEY"},
	{"ACCESS", "KEY"},
	{"SESSION", "KEY"},
	{"AUTH"},
}

// configpermsIsCredentialKey reports whether name is credential-shaped: its
// upper-cased, `_`-split segments contain one of configpermsCredentialKeywords
// as a contiguous run. "MY_API_KEY_FOO" matches (API, KEY are adjacent
// segments); "CLOUDSDK_CORE_PROJECT" and "HOME" do not — no segment or
// adjacent-segment run equals a listed keyword.
func configpermsIsCredentialKey(name string) bool {
	segs := strings.Split(strings.ToUpper(name), "_")
	for _, kw := range configpermsCredentialKeywords {
		if configpermsContainsRun(segs, kw) {
			return true
		}
	}
	return false
}

// configpermsContainsRun reports whether sub appears as a contiguous run
// within segs.
func configpermsContainsRun(segs, sub []string) bool {
	if len(sub) > len(segs) {
		return false
	}
	for i := 0; i+len(sub) <= len(segs); i++ {
		match := true
		for j, s := range sub {
			if segs[i+j] != s {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// configpermsCredentialKeys returns m's keys that are credential-shaped
// (configpermsIsCredentialKey), sorted for determinism — never the full key
// set: a section whose inline env: sets only non-credential-shaped names
// (CLOUDSDK_CORE_PROJECT) contributes nothing here, which is what makes it
// produce no finding at all rather than a fail that asserts a credential it
// never saw.
func configpermsCredentialKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		if configpermsIsCredentialKey(k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// configpermsReadableDesc mirrors configpermsWritableDesc for the readable
// bits.
func configpermsReadableDesc(fi FileInfo) string {
	perm := fi.Perm()
	group := perm&0o040 != 0
	other := perm&0o004 != 0
	switch {
	case group && other:
		return "group- and other-readable"
	case group:
		return "group-readable"
	default:
		return "other-readable"
	}
}

// configpermsSortedToolNames/configpermsSortedMCPServerNames impose a fixed
// order over map iteration — required for TestChecksAreDeterministic
// (invariants_test.go runs every check 8 times over a fixture with 3 tools
// and 2 mcp_servers entries). configpermsCredentialKeys (above) sorts its
// own filtered key list the same way.
func configpermsSortedToolNames(m map[string]config.ToolConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func configpermsSortedMCPServerNames(m map[string]config.MCPServerConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- config.symlinked

// runConfigPermsSymlinked fires when in.ConfigPath is itself a symlink —
// deliberately checked via Lstat on the UNRESOLVED path, unlike every other
// check in this file, since the link is the subject here, not something to
// see through. cmd/reload.go's watchConfigFile resolves the path with
// filepath.Abs, never EvalSymlinks, and then watches the LINK's directory —
// so with the common dotfiles layout (~/.config/wraptool/config.yaml ->
// ~/dotfiles/wraptool.yaml) hot-reload never fires and edits apply only on
// SIGHUP or restart (design §7.2).
func runConfigPermsSymlinked(_ context.Context, in *Input) Result {
	fi, err := in.Env.Lstat(in.ConfigPath)
	if err != nil {
		return Skipped("config path could not be lstat'd: %v", err)
	}
	if !fi.IsSymlink() {
		return Pass()
	}

	target := "(target could not be resolved)"
	if resolved, err := in.Env.EvalSymlinks(in.ConfigPath); err == nil {
		target = resolved
	}

	return Found(Finding{
		Severity: SeverityWarn,
		Title:    "Config file is a symlink",
		Detail: fmt.Sprintf(
			"%s -> %s. hot-reload's file watch resolves the path with filepath.Abs (never EvalSymlinks) and then "+
				"watches the LINK's directory, so edits to the target through this common dotfiles layout never "+
				"trigger a reload",
			in.ConfigPath, target),
		Remedy: "send SIGHUP to the running server after editing the target, restart it, or store config.yaml " +
			"directly instead of symlinking it",
		DocRef: "docs/configuration.qmd",
	})
}
