// exposure_files.go implements the token/socket/request-log half of the
// exposure.* group (design §7.3): checks over specific FILES the config
// points at (an auth_token_file, a unix socket path, the request log) — as
// opposed to exposure_listen.go's listener/bind checks (this file's sibling
// group file, landing in the same parallel wave).
//
// exposure.token-perms, exposure.unix-socket-dir, and exposure.request-log-perms
// are about a REAL file/directory's own permission bits, not the config
// document itself — the sanctioned non-config-subject case for Finding.Location
// (finding.go's own doc comment: "a binary path, a socket directory, a pool
// generation dir"; pool.go's devcontainer.json findings are the other merged
// example). exposure.cwd-roots-unset is the one check here that reports on
// config CONTENT (mcp.allowed_cwd_roots itself), so it sets Finding.Path
// instead (RULING R5, plan §1 rule 9).
//
// exposure.token-owner is explicitly Phase 2 (needs serve.state's uid, §4.1)
// and is NOT shipped here.
package doctor

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/pti/wraptool/internal/config"
)

const (
	exposureFilesTokenPerms         CheckID = "exposure.token-perms"
	exposureFilesTokenWeak          CheckID = "exposure.token-weak"
	exposureFilesUnixSocketDir      CheckID = "exposure.unix-socket-dir"
	exposureFilesCWDRootsUnset      CheckID = "exposure.cwd-roots-unset"
	exposureFilesRequestLogWritable CheckID = "exposure.request-log-writable"
	exposureFilesRequestLogPerms    CheckID = "exposure.request-log-perms"
)

// exposureFilesTokenPermsMeta, ... are the checks' Meta literals, named so
// exposure_files_test.go's TestExposureFilesCheckMeta can assert each check's
// ID/Needs directly against the same literal init() registers — never through
// Registered() (plan §1 rule 8; pool.go models this pattern).
var (
	exposureFilesTokenPermsMeta = Meta{
		ID:      exposureFilesTokenPerms,
		Needs:   NeedsConfig | NeedsModeBits,
		Summary: "checks that a configured auth_token_file has no group or other permission bits",
	}
	exposureFilesTokenWeakMeta = Meta{
		ID:      exposureFilesTokenWeak,
		Needs:   NeedsConfig,
		Summary: "checks that a configured auth token is not empty, short, or a known placeholder",
	}
	exposureFilesUnixSocketDirMeta = Meta{
		ID:      exposureFilesUnixSocketDir,
		Needs:   NeedsConfig | NeedsModeBits,
		Summary: "checks that the nearest existing ancestor of a unix-transport socket path is not group- or other-writable",
	}
	exposureFilesCWDRootsUnsetMeta = Meta{
		ID:      exposureFilesCWDRootsUnset,
		Needs:   NeedsConfig,
		Summary: "warns when mcp.allowed_cwd_roots is empty (the documented default-open case)",
	}
	exposureFilesRequestLogWritableMeta = Meta{
		ID:      exposureFilesRequestLogWritable,
		Needs:   NeedsConfig | NeedsModeBits,
		Summary: "checks that the nearest existing ancestor of mcp.request_log is writable",
	}
	exposureFilesRequestLogPermsMeta = Meta{
		ID:      exposureFilesRequestLogPerms,
		Needs:   NeedsConfig | NeedsModeBits,
		Summary: "checks that an existing request log has no group or other permission bits",
	}
)

func init() {
	register(exposureFilesTokenPermsMeta, runExposureFilesTokenPerms)
	register(exposureFilesTokenWeakMeta, runExposureFilesTokenWeak)
	register(exposureFilesUnixSocketDirMeta, runExposureFilesUnixSocketDir)
	register(exposureFilesCWDRootsUnsetMeta, runExposureFilesCWDRootsUnset)
	register(exposureFilesRequestLogWritableMeta, runExposureFilesRequestLogWritable)
	register(exposureFilesRequestLogPermsMeta, runExposureFilesRequestLogPerms)
}

// ---------------------------------------------------------------- shared: token files

// exposureFilesTokenFile pairs a config key's label with the path it names —
// used by both exposure.token-perms and exposure.token-weak so a finding can
// say WHICH of the two (mcp vs admin) auth_token_file is at issue.
type exposureFilesTokenFile struct {
	Label string
	Path  string
}

// exposureFilesTokenFiles returns every configured auth_token_file, in a
// FIXED order (mcp then admin, never a map range) — TestChecksAreDeterministic
// runs each check 8 times.
func exposureFilesTokenFiles(cfg *config.Config) []exposureFilesTokenFile {
	var out []exposureFilesTokenFile
	if cfg.MCP.AuthTokenFile != "" {
		out = append(out, exposureFilesTokenFile{Label: "mcp.auth_token_file", Path: cfg.MCP.AuthTokenFile})
	}
	if cfg.Admin.AuthTokenFile != "" {
		out = append(out, exposureFilesTokenFile{Label: "admin.auth_token_file", Path: cfg.Admin.AuthTokenFile})
	}
	return out
}

// exposureFilesGroupOrOtherBits reports whether perm has ANY group or other
// bit set (read, write, or execute) — the literal "has group or other bits"
// condition §7.3 names for exposure.token-perms and exposure.request-log-perms,
// broader than host.go's GroupOrOtherWritable/GroupOrOtherReadable (which each
// test only one axis).
func exposureFilesGroupOrOtherBits(perm fs.FileMode) bool { return perm&0o077 != 0 }

// ---------------------------------------------------------------- exposure.token-perms

// runExposureFilesTokenPerms fails once per configured auth_token_file (mcp
// and/or admin) whose mode has any group or other bit set. A token file that
// cannot be Stat'd (absent, or a broken config's dangling path) is silently
// skipped here — config.validate (B2) already reports an auth_token_file that
// does not exist; this check's own concern is the permission bits of one that
// does.
func runExposureFilesTokenPerms(_ context.Context, in *Input) Result {
	files := exposureFilesTokenFiles(in.Config)
	if len(files) == 0 {
		return Skipped("no auth_token_file configured")
	}

	var findings []Finding
	for _, tf := range files {
		fi, err := in.Env.Stat(tf.Path)
		if err != nil {
			continue
		}
		if !exposureFilesGroupOrOtherBits(fi.Perm()) {
			continue
		}
		findings = append(findings, Finding{
			Severity: SeverityFail,
			Title:    "Auth token file is group- or other-accessible",
			Detail: fmt.Sprintf("%s (%s) has mode %04o — group or other has some access to it, and its content "+
				"is a bearer credential", tf.Label, tf.Path, fi.Perm()),
			// A non-config subject (the token file's own bits, not the config
			// document) — Location, never Path (plan §1 rule 9).
			Location: tf.Path,
			Remedy:   fmt.Sprintf("chmod 600 %s", tf.Path),
			DocRef:   "docs/configuration.qmd",
		})
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- exposure.token-weak

// exposureFilesMinTokenBytes is §7.3's literal "under 32 bytes" threshold.
const exposureFilesMinTokenBytes = 32

// exposureFilesPlaceholderTokens are exact, lower-cased known-placeholder
// token values. Matched exactly rather than by substring: a substring rule
// like Contains(token, "token") would flag a real random token that happens
// to contain that word, which is exactly the false-positive-by-heuristic
// shape design §7.3's "report length and shape only" framing warns against.
var exposureFilesPlaceholderTokens = map[string]bool{
	"changeme":                         true,
	"change-me":                        true,
	"change_me":                        true,
	"changethis":                       true,
	"your-token-here":                  true,
	"yourtokenhere":                    true,
	"insert-token-here":                true,
	"replace-me":                       true,
	"replaceme":                        true,
	"placeholder":                      true,
	"secret":                           true,
	"password":                         true,
	"token":                            true,
	"example":                          true,
	"example-token":                    true,
	"test":                             true,
	"test-token":                       true,
	"todo":                             true,
	"tbd":                              true,
	"fixme":                            true,
	"xxxx":                             true,
	"xxxxxxxx":                         true,
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx": true,
	"000000":                           true,
	"123456":                           true,
	"changeit":                         true,
}

// exposureFilesTokenWeakness classifies a token's CONTENT — trimmed of
// surrounding whitespace, since an operator-created token file typically ends
// in a trailing newline (`echo $TOKEN > file`) that must not count toward
// length. Returns ok=false when the token is not weak by any of §7.3's three
// criteria. The reason string never echoes the token itself (§10.1: "report
// length and shape only, never the value").
func exposureFilesTokenWeakness(content string) (reason string, weak bool) {
	switch {
	case content == "":
		return "token is empty", true
	case exposureFilesPlaceholderTokens[strings.ToLower(content)]:
		// Checked before the length threshold: a short placeholder like
		// "changeme" is both under 32 bytes AND a known placeholder, and
		// naming it as a placeholder is the more actionable, more specific
		// reason — "under 32 bytes" alone would be true but less useful.
		return "token matches a known placeholder value", true
	case len(content) < exposureFilesMinTokenBytes:
		return fmt.Sprintf("token is %d byte(s), under the recommended minimum of %d",
			len(content), exposureFilesMinTokenBytes), true
	default:
		return "", false
	}
}

// runExposureFilesTokenWeak reads each configured auth_token_file's content
// and reports its length/shape only, never the value. A token file that
// cannot be read is silently skipped — exposure.token-perms and
// config.validate already cover an unreadable/absent token file.
func runExposureFilesTokenWeak(_ context.Context, in *Input) Result {
	files := exposureFilesTokenFiles(in.Config)
	if len(files) == 0 {
		return Skipped("no auth_token_file configured")
	}

	var findings []Finding
	for _, tf := range files {
		data, err := in.Env.ReadFile(tf.Path)
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(data))
		reason, weak := exposureFilesTokenWeakness(content)
		if !weak {
			continue
		}
		findings = append(findings, Finding{
			Severity: SeverityWarn,
			Title:    "Auth token is weak",
			Detail:   fmt.Sprintf("%s (%s): %s", tf.Label, tf.Path, reason),
			Location: tf.Path,
			Remedy:   fmt.Sprintf("replace it with a long random value, e.g. `openssl rand -hex 32 > %s`", tf.Path),
			DocRef:   "docs/configuration.qmd",
		})
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- exposure.unix-socket-dir

// runExposureFilesUnixSocketDir applies only under unix transport (§7.3:
// gated on transport, mirroring exposure.sse-non-loopback's own gating
// rationale — a socket path is not a network listen address). The subject is
// the DIRECTORY CHAIN, per §7.3's literal wording ("the nearest existing
// ancestor of the socket path" — an ancestor of it, never the path itself):
// another local user's ability to unlink, rename, or race the socket is
// governed by who can write the directory that CONTAINS it, not by the
// socket's own mode. It therefore walks from filepath.Dir(sock), not from
// sock — `serve` creates the socket file at bind (cmd/serve.go's serveUnix),
// so in the common case (server already started) sock exists, and Stat-ing
// it first would make envNearestExistingAncestor return the socket file's
// own FileInfo on its very first step, which means the directory chain is
// NEVER examined once the socket exists — checking the opposite of the
// check's stated subject in exactly the state this check runs in most
// often. Existence of the socket itself is still not required either way
// (envNearestExistingAncestor, env.go, already merged, walks up to whichever
// ancestor DOES exist, exactly as env.xdg-writable does).
//
// A DIRECTORY ancestor carrying the sticky bit (fs.ModeSticky, e.g. /tmp) is
// deliberately never flagged even when group/other-writable: the sticky bit
// is precisely what stops another local user from unlinking or renaming a
// file they do not own within it, which is the actual hijack/race this check
// exists to catch — flagging /tmp unconditionally would fire SeverityFail on
// a configuration working as designed (a real, common socket location),
// which SeverityFail's own doc comment forbids.
func runExposureFilesUnixSocketDir(_ context.Context, in *Input) Result {
	if in.Config.MCP.Transport != "unix" {
		return Skipped("mcp.transport is not unix")
	}
	sock := in.Config.MCP.Listen
	if sock == "" {
		return Skipped("mcp.listen is not set")
	}

	fi, ok := envNearestExistingAncestor(in.Env, filepath.Dir(sock))
	if !ok {
		return Skipped("no existing ancestor found for %s", sock)
	}
	if fi.IsDir() && fi.Mode&fs.ModeSticky != 0 {
		return Pass()
	}
	if !fi.GroupOrOtherWritable() {
		return Pass()
	}

	return Found(Finding{
		Severity: SeverityFail,
		Title:    "Unix socket path is group- or other-writable",
		Detail: fmt.Sprintf("mcp.listen %s: nearest existing ancestor %s (mode %s) is group- or other-writable — "+
			"another local user could remove, replace, or race the socket", sock, fi.Path, fi.Mode),
		Location: fi.Path,
		Remedy:   fmt.Sprintf("restrict permissions on %s, or move mcp.listen under a directory only you can write", fi.Path),
		DocRef:   "docs/configuration.qmd",
	})
}

// ---------------------------------------------------------------- exposure.cwd-roots-unset

// runExposureFilesCWDRootsUnset warns when mcp.allowed_cwd_roots is empty —
// audit F5's documented default-open case: a client-supplied working
// directory is not constrained to any root at all. This is the one check in
// this file reporting on config CONTENT itself, so it sets Finding.Path
// (RULING R5) rather than Location.
func runExposureFilesCWDRootsUnset(_ context.Context, in *Input) Result {
	if len(in.Config.MCP.AllowedCWDRoots) > 0 {
		return Pass()
	}
	return Found(Finding{
		Severity: SeverityWarn,
		Title:    "No allowed_cwd_roots configured",
		Detail: "mcp.allowed_cwd_roots is empty — a client-supplied working directory is not constrained to any " +
			"root (the documented default-open case, audit F5)",
		Path:   At("mcp", "allowed_cwd_roots"),
		Remedy: "set mcp.allowed_cwd_roots to the project root(s) you want to allow, or accept the default-open behavior deliberately",
		DocRef: "docs/configuration.qmd",
	})
}

// ---------------------------------------------------------------- exposure.request-log-writable

// runExposureFilesRequestLogWritable checks that mcp.request_log is
// reachable: internal/requestlog/store.go's appendEventLocked MkdirAlls the
// log's own directory lazily on first write, so the file (and often its
// directory) not existing YET is normal on a fresh install, never itself a
// finding — only "the nearest existing ancestor is not writable" is.
// envNearestExistingAncestor/envDirWritable (env.go, already merged) are the
// same helpers env.xdg-writable itself uses, for the identical writability
// judgement.
func runExposureFilesRequestLogWritable(_ context.Context, in *Input) Result {
	logPath := in.Config.MCP.RequestLog
	if logPath == "" {
		return Skipped("mcp.request_log is not set")
	}

	fi, ok := envNearestExistingAncestor(in.Env, logPath)
	if !ok {
		return Skipped("no existing ancestor found for %s", logPath)
	}
	if envDirWritable(fi, in.Env.Getuid(), in.Env.Getgid()) {
		return Pass()
	}

	return Found(Finding{
		Severity: SeverityFail,
		Title:    "Request log directory not writable",
		Detail: fmt.Sprintf("mcp.request_log %s: nearest existing ancestor %s (mode %s) is not writable",
			logPath, fi.Path, fi.Mode),
		Location: fi.Path,
		Remedy:   fmt.Sprintf("make %s writable by the current user, or point mcp.request_log elsewhere", fi.Path),
		DocRef:   "docs/configuration.qmd",
	})
}

// ---------------------------------------------------------------- exposure.request-log-perms

// runExposureFilesRequestLogPerms fires only for an EXISTING request log:
// Store.append (internal/requestlog/store.go) creates it 0600 on first write
// but never repairs a pre-existing permissive file (e.g. one an operator
// touched by hand, or restored from a backup with different permissions),
// and it records every tool call — request/response metadata an operator
// would not want group- or other-readable.
func runExposureFilesRequestLogPerms(_ context.Context, in *Input) Result {
	logPath := in.Config.MCP.RequestLog
	if logPath == "" {
		return Skipped("mcp.request_log is not set")
	}

	fi, err := in.Env.Stat(logPath)
	if err != nil {
		return Skipped("request log does not exist yet")
	}
	if !exposureFilesGroupOrOtherBits(fi.Perm()) {
		return Pass()
	}

	return Found(Finding{
		Severity: SeverityFail,
		Title:    "Request log is group- or other-accessible",
		Detail: fmt.Sprintf("mcp.request_log %s has mode %04o — Store.append creates it 0600 but never repairs "+
			"a pre-existing permissive file, and it records every tool call", logPath, fi.Perm()),
		Location: logPath,
		Remedy:   fmt.Sprintf("chmod 600 %s", logPath),
		DocRef:   "docs/configuration.qmd",
	})
}
