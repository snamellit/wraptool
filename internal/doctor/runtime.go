package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pti/wraptool/internal/config"
)

// This file ships §7.5's runtime.* group. Phase 1 (B9) shipped against a
// serve.state carrying exactly two keys (listen=, auth=); E0 landed §4.1's
// extension (uid/config_path/path/started_at/config_loaded_at/transport/
// admin_listen — cmd/server.go's writeServeState/readServeState), and this
// file's E1/E2 revision reads the rest of it: runRuntimeUID and
// runRuntimeStalePolicy below, plus env.go's env.path-differs-from-server and
// exposure_files.go's exposure.token-owner (both in this same package, so
// they call runtimeLiveMatchingState directly rather than duplicating its
// gate). runRuntimeBindDivergence deliberately still does NOT gate on
// config_path — see its own doc comment for why that stays a Phase-1
// decision rather than being widened here. A key this file does not
// recognize is simply absent, never a value (parsed tolerantly, mirroring
// cmd/server.go's own reader).
//
// Per Input's own doc comment there is no Input.Server field: B9 sits inside
// the nine-way parallel wave and cannot widen the shared Input type, so this
// file reads and decodes the pidfile and serve.state itself, via
// Input.Paths.RuntimeDir + Env.ReadFile — never importing package cmd (whose
// serveState/pidFilePath/serveStatePath are unexported anyway).
const (
	runtimeStalePIDFile   CheckID = "runtime.stale-pidfile"
	runtimeBindDivergence CheckID = "runtime.bind-divergence"
	runtimeUID            CheckID = "runtime.uid"
	runtimeStalePolicy    CheckID = "runtime.stale-policy"
)

func init() {
	register(Meta{
		ID:      runtimeStalePIDFile,
		Needs:   NeedsNothing,
		Summary: "reports a pidfile naming no live process",
	}, runRuntimeStalePIDFile)
	register(Meta{
		ID:      runtimeBindDivergence,
		Needs:   NeedsConfig,
		Summary: "compares the running server's effective bind to the config",
	}, runRuntimeBindDivergence)
	register(Meta{
		ID:      runtimeUID,
		Needs:   NeedsNothing,
		Summary: "reports the uid the server runs as, when recorded",
	}, runRuntimeUID)
	register(Meta{
		ID:      runtimeStalePolicy,
		Needs:   NeedsNothing,
		Summary: "warns when the config file changed since the server's last successful load",
	}, runRuntimeStalePolicy)
}

// runtimePIDFileName / runtimeServeStateName are cmd/server.go's own file
// basenames (pidFilePath/serveStatePath, both filepath.Join(paths.RuntimeDir(),
// ...)) — this file reads the same files cmd writes, by name, without
// importing cmd.
const (
	runtimePIDFileName    = "serve.pid"
	runtimeServeStateName = "serve.state"
)

func runtimePIDFilePath(in *Input) string {
	return filepath.Join(in.Paths.RuntimeDir, runtimePIDFileName)
}

func runtimeServeStatePath(in *Input) string {
	return filepath.Join(in.Paths.RuntimeDir, runtimeServeStateName)
}

// runtimeReadPID reads and parses the pidfile. present=false (pid=0, err=nil)
// means "no pidfile at all" — the normal, unremarkable state on a host that
// has never run `serve`. present=true with a non-nil err means the file
// exists but its content did not parse as a pid: reported, never silently
// treated as pid 0 (which Env.ProcessAlive could conceivably answer for).
// A pid <= 0 is treated the same as a parse failure: 0 or a negative number
// parses fine as an int, but Env.ProcessAlive's real implementation is
// `syscall.Kill(pid, 0)`, and kill(0, 0) signals the caller's own process
// group rather than reporting "no such process" — so a literal "0" in the
// pidfile must never reach that call.
func runtimeReadPID(in *Input) (pid int, present bool, err error) {
	path := runtimePIDFilePath(in)
	b, readErr := in.Env.ReadFile(path)
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, readErr
	}
	n, parseErr := strconv.Atoi(strings.TrimSpace(string(b)))
	if parseErr == nil && n <= 0 {
		parseErr = fmt.Errorf("pid %d is not a valid process id", n)
	}
	if parseErr != nil {
		return 0, true, fmt.Errorf("pidfile %s does not contain a valid pid: %w", path, parseErr)
	}
	return n, true, nil
}

// runRuntimeStalePIDFile is the ONE runtime.* check that fires precisely when
// the pid is DEAD (§7.5), so — unlike runtime.bind-divergence — it must NOT be
// gated on liveness itself: gating it on Env.ProcessAlive would suppress the
// only case it exists to report.
func runRuntimeStalePIDFile(_ context.Context, in *Input) Result {
	pid, present, err := runtimeReadPID(in)
	if !present {
		if err != nil {
			// Present-but-unreadable is a data problem doctor cannot diagnose
			// further; report it as "could not check" rather than asserting
			// either a clean or a stale verdict from data it could not read.
			return Skipped("%s", err)
		}
		return Pass()
	}
	if err != nil {
		// The pidfile exists but its content did not parse: same tolerant
		// posture — unknown, never asserted as a value either way.
		return Skipped("%s", err)
	}
	if in.Env.ProcessAlive(pid) {
		return Pass()
	}
	path := runtimePIDFilePath(in)
	return Found(Finding{
		Severity: SeverityWarn,
		Title:    "Stale pidfile",
		Detail:   fmt.Sprintf("names pid %d, which is not running", pid),
		Location: path,
		Remedy:   "run `wraptool server stop` to clear it, or start the server with `wraptool up` / `wraptool server start`",
	})
}

// runtimeServeState is this file's own decode of cmd/server.go's key=value
// serve.state wire format (§4.1). Absent keys decode to the zero value and
// are never distinguished from "recorded as false/empty" here EXCEPT uid
// (see below) — an old-format state file (written before E0) simply lacks
// the Phase 2 keys, and every consumer of those fields treats that the same
// as "unknown", skipping rather than asserting a value.
//
// uid mirrors cmd/server.go's own serveState.UID(): it cannot be a plain
// int, since 0 is root, a legitimate value, and also the zero value an
// absent "uid" key would decode to. It is tracked with its own presence flag
// and read back only through UID(), so a consumer can never mistake
// "unknown" (an old-format state file, or Windows — cmd/server.go's
// effectiveUID omits the key there since os.Getuid() is meaningless) for
// "root".
type runtimeServeState struct {
	Listen string
	Auth   bool

	Transport      string
	AdminListen    string
	ConfigPath     string // EvalSymlinks(Abs(...)) as written; see runtimeResolvedConfigPath
	Path           string // the server's PATH at start
	StartedAt      string // RFC3339, process start
	ConfigLoadedAt string // RFC3339, last successful load or reload

	uid    int
	hasUID bool
}

// UID returns the uid this server is running as and whether it was recorded.
func (s runtimeServeState) UID() (uid int, ok bool) { return s.uid, s.hasUID }

// runtimeParseServeState mirrors cmd/server.go's readServeState: unknown keys
// and unparseable lines are ignored, so a newer or older serve.state is read
// without error (the same forward/backward compatibility cmd's own reader
// relies on). Values are collected into a map first, then unescaped and
// assigned by key — mirroring cmd/server.go's own two-pass structure, which
// is what lets uid's presence (as opposed to its zero value) survive the
// same "value or absent" ambiguity every other field's Go zero value already
// resolves for free.
func runtimeParseServeState(b []byte) runtimeServeState {
	m := make(map[string]string)
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		m[key] = runtimeUnescapeServeStateValue(val)
	}

	s := runtimeServeState{
		Listen:         m["listen"],
		Auth:           m["auth"] == "true",
		Transport:      m["transport"],
		AdminListen:    m["admin_listen"],
		ConfigPath:     m["config_path"],
		Path:           m["path"],
		StartedAt:      m["started_at"],
		ConfigLoadedAt: m["config_loaded_at"],
	}
	if v, present := m["uid"]; present {
		if n, err := strconv.Atoi(v); err == nil {
			s.uid, s.hasUID = n, true
		}
	}
	return s
}

// runtimeUnescapeServeStateValue mirrors cmd/server.go's serveStateUnescape
// exactly: this file cannot import cmd (cmd imports doctor, not the other
// way around — see this file's own header), so it re-implements the inverse
// of writeServeState's escaping here rather than depending on it. A newline
// is the one hazard a POSIX filename may contain (config_path, path are both
// filesystem-derived), so `\n`/`\\` are the two escapes to undo.
func runtimeUnescapeServeStateValue(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			switch v[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			}
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// runtimeResolvedConfigPath resolves in.ConfigPath the same way
// cmd/server.go's effectiveConfigPath resolved it when writing config_path:
// EvalSymlinks, best-effort. in.ConfigPath is already filepath.Abs'd
// (cmd/doctor.go), so only the symlink-resolution step remains here — a path
// that does not exist (or cannot be resolved for any other reason) compares
// by its already-absolute form, exactly like effectiveConfigPath's own
// fallback.
func runtimeResolvedConfigPath(in *Input) string {
	if resolved, err := in.Env.EvalSymlinks(in.ConfigPath); err == nil {
		return resolved
	}
	return in.ConfigPath
}

// runtimeLiveMatchingState is the shared gate every Phase 2 serve.state
// consumer in this package starts from — runRuntimeUID and
// runRuntimeStalePolicy below, plus env.go's env.path-differs-from-server and
// exposure_files.go's exposure.token-owner (same package, so they call this
// directly rather than duplicating it). Design §7.5's own preamble: gated on
// a LIVE pid AND on config_path resolving to the SAME file as the config
// under audit — the state file survives SIGKILL, so its mere presence proves
// nothing, and a shared server started from a different config is not this
// audit's concern to report on. skip is "" on success; a non-empty skip is
// the reason to hand to Skipped() verbatim.
//
// Deliberately duplicates (rather than factors out) runRuntimeBindDivergence's
// own "live pid, read serve.state" prologue: that function is Phase 1,
// already reviewed and tested against its own documented limitation, and
// reworking it to share this helper is a behavior-risking refactor this step
// was not asked to make.
func runtimeLiveMatchingState(in *Input) (state runtimeServeState, skip string) {
	pid, present, err := runtimeReadPID(in)
	if !present || err != nil || !in.Env.ProcessAlive(pid) {
		return runtimeServeState{}, "no live server process recorded"
	}
	b, readErr := in.Env.ReadFile(runtimeServeStatePath(in))
	if readErr != nil {
		return runtimeServeState{}, "no server state recorded"
	}
	state = runtimeParseServeState(b)
	if state.ConfigPath == "" {
		return runtimeServeState{}, "server state does not record a config path"
	}
	if state.ConfigPath != runtimeResolvedConfigPath(in) {
		return runtimeServeState{}, "server was started from a different config"
	}
	return state, ""
}

// runRuntimeBindDivergence is design §7.5's sharpest single check:
// `adjustServerBindForRuntime` (cmd/up.go) widens a loopback bind to
// 0.0.0.0 in memory only, for the devcontainer runtime, and never writes the
// change back to config — so a file-only audit reports "loopback, ok" while
// the server actually listens off-host.
//
// Gated on a LIVE pid (via Env.ProcessAlive): serve.state survives SIGKILL,
// so its mere presence proves nothing about whether the recorded bind is
// still in effect. Today's serve.state carries no config_path (that is
// Phase 2, §4.1/E0), so this check cannot confirm the running server was
// even started from the config under audit — `wraptool up` starts one
// shared server from the XDG config, and `wraptool doctor --config
// ./project.yaml` audits a different file entirely, which diverges by
// design. Severity reflects that: SeverityFail is reserved for the one
// shape that is dangerous no matter which config started the server — an
// unauthenticated listener reachable off-host — because leaving that as a
// mere warning would let a genuinely exposed, credential-backed server pass
// doctor clean. Every other divergence is SeverityWarn, and its Detail says
// so explicitly rather than implying doctor confirmed a same-config
// mismatch it cannot confirm.
func runRuntimeBindDivergence(_ context.Context, in *Input) Result {
	pid, present, err := runtimeReadPID(in)
	if !present || err != nil || !in.Env.ProcessAlive(pid) {
		return Skipped("no live server process recorded")
	}

	b, readErr := in.Env.ReadFile(runtimeServeStatePath(in))
	if readErr != nil {
		return Skipped("no server state recorded")
	}
	state := runtimeParseServeState(b)
	if state.Listen == "" {
		return Skipped("server state does not record a listen address")
	}

	cfgListen := in.Config.MCP.Listen
	if cfgListen == "" {
		// An explicit `mcp.transport: sse` with no listen leaves this empty
		// (applyDefaults only fills it when Transport itself is empty) — this
		// check has nothing to compare the running server's bind against, so
		// it could not evaluate the case, not "checked and found no divergence".
		return Skipped("config does not record an mcp.listen to compare against")
	}
	if state.Listen == cfgListen {
		return Pass()
	}

	detail := fmt.Sprintf("server listens on %s, config says %s", state.Listen, cfgListen)
	const limitation = " Today's serve.state records no config path, so this compares against " +
		"whichever config the running server was started from — the divergence is expected if that " +
		"was a different config (e.g. a shared server from `wraptool up` audited with `--config`)."

	if runtimeIsUpBindAdjustment(cfgListen, state) {
		return Found(Finding{
			Severity: SeverityWarn,
			Title:    "Server bind differs from config",
			Detail: detail + ". This matches `up`'s devcontainer bind adjustment and the listener " +
				"is authenticated — expected if you started it with `wraptool up`.",
			Remedy: "no action needed if this server was started by `wraptool up`; otherwise restart it against the current config",
		})
	}

	if !state.Auth && runtimeListenIsNetworkNonLoopback(state.Listen) {
		return Found(Finding{
			Severity: SeverityFail,
			Title:    "Server bind differs from config",
			Detail:   detail + ". The running server is an unauthenticated listener reachable off-host — dangerous regardless of which config started it.",
			Remedy:   "restart the server (`wraptool server stop` then `wraptool up` / `wraptool server start`) so it picks up the configured bind, or set mcp.auth_token_file",
		})
	}

	return Found(Finding{
		Severity: SeverityWarn,
		Title:    "Server bind differs from config",
		Detail:   detail + fmt.Sprintf(" (authenticated=%v). This is not `up`'s known bind adjustment.", state.Auth) + limitation,
		Remedy:   "if this server was started from the config under audit, restart it (`wraptool server stop` then `wraptool up` / `wraptool server start`) so it picks up the configured bind",
	})
}

// runtimeListenIsNetworkNonLoopback reports whether listen names a network
// address — host:port — that is reachable off-host. Unlike
// config.ListenIsLoopback (which treats an unparsable value as "the whole
// string is the host" so it can classify a bare hostname), this helper
// requires a genuine host:port split to succeed before calling anything
// non-loopback: a unix socket path has no port and does not parse, and this
// check must never assert a network-exposure verdict about a value it could
// not confirm is even a network address (unix sockets are filesystem-perms
// guarded, not this check's concern — config.Validate agrees, gating its
// own auth requirement on transport == "sse").
func runtimeListenIsNetworkNonLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "" {
		return true // wildcard bind
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// runtimeIsUpBindAdjustment reports whether state matches
// adjustServerBindForRuntime's (cmd/up.go) EXACT transformation: a loopback
// config bind rewritten to the same port on 0.0.0.0, with the running server
// authenticated (a non-loopback bind must be, and `up` defaults a token in
// when it widens one, cmd/up.go:225-227). Anything else — including an
// authenticated divergence of any other shape, or ANY unauthenticated
// widening — is a genuine, unexplained divergence and stays `fail` (§7.5): a
// `warn` here would be permanently red in every devcontainer deployment, but
// a `fail` for every OTHER shape is the point of the check.
func runtimeIsUpBindAdjustment(cfgListen string, state runtimeServeState) bool {
	if !state.Auth {
		return false
	}
	if !config.ListenIsLoopback(cfgListen) {
		return false
	}
	_, port, err := net.SplitHostPort(cfgListen)
	if err != nil {
		return false
	}
	return state.Listen == net.JoinHostPort("0.0.0.0", port)
}

// ------------------------------------------------------------- runtime.uid

// runRuntimeUID is always SeverityInfo (§7.5): it reports state, never a
// judgement — the ownership checks that DO judge (exposure.token-owner,
// exposure_files.go) read the same uid via runtimeLiveMatchingState
// themselves. Gated on runtimeLiveMatchingState, then further on the uid key
// itself being present: it is absent from an old-format state file (written
// before E0) and on Windows (cmd/server.go's effectiveUID omits it there),
// and absent means UNKNOWN, never 0/root — wraptool refuses to run as root
// before any check executes (cmd/rootuser.go), so a recorded uid 0 is
// unreachable outside a test fixture, but this check must still never
// *assert* root from data it does not have.
func runRuntimeUID(_ context.Context, in *Input) Result {
	state, skip := runtimeLiveMatchingState(in)
	if skip != "" {
		return Skipped("%s", skip)
	}
	uid, ok := state.UID()
	if !ok {
		return Skipped("server state does not record a uid")
	}
	return Found(Finding{
		Severity: SeverityInfo,
		Title:    "Server runs as",
		Detail:   fmt.Sprintf("uid %d", uid),
	})
}

// -------------------------------------------------------- runtime.stale-policy

// runRuntimeStalePolicy compares the audited config file's mtime to the
// server's own config_loaded_at (E0's reload hook,
// cmd/server.go:updateServeStateConfigLoadedAt): cmd/reload.go's
// reloadConfig only advances that timestamp on a SUCCESSFUL reload, so a
// parse or validate failure leaves it untouched — a clean file can therefore
// sit on disk while the server keeps enforcing the OLD, possibly weaker,
// policy in memory.
//
// Deliberately NOT gated on NeedsConfig: the exact case this check exists to
// catch is a config the operator just broke, which fails doctor's OWN
// lenient load too (in.ConfigState != ConfigLoaded) — gating on a successful
// local parse would suppress the finding in precisely the situation it is
// for. It reads in.ConfigPath's mtime directly instead.
func runRuntimeStalePolicy(_ context.Context, in *Input) Result {
	state, skip := runtimeLiveMatchingState(in)
	if skip != "" {
		return Skipped("%s", skip)
	}
	if state.ConfigLoadedAt == "" {
		return Skipped("server state does not record a config load time")
	}
	loadedAt, err := time.Parse(time.RFC3339, state.ConfigLoadedAt)
	if err != nil {
		return Skipped("server state config_loaded_at does not parse as RFC3339: %s", err)
	}
	fi, statErr := in.Env.Stat(in.ConfigPath)
	if statErr != nil {
		return Skipped("could not stat config file: %s", statErr)
	}
	if !fi.ModTime.After(loadedAt) {
		return Pass()
	}
	return Found(Finding{
		Severity: SeverityWarn,
		Title:    "Config changed since the server's last successful load",
		Detail: fmt.Sprintf("config mtime %s is newer than the server's last successful load at %s — reload keeps the OLD "+
			"policy on any parse or validate failure, so a clean file on disk can coexist with a weaker policy in memory",
			fi.ModTime.Format(time.RFC3339), loadedAt.Format(time.RFC3339)),
		Remedy: "check the server log for a reload error, then send SIGHUP (or restart the server) once it is fixed",
		DocRef: "docs/configuration.qmd",
	})
}
