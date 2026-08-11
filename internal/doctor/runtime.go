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

	"github.com/pti/wraptool/internal/config"
)

// This file ships the part of §7.5's runtime.* group that works against
// TODAY's serve.state, which records exactly two keys (listen=, auth=;
// cmd/server.go:writeServeState). §4.1's uid/config_path/config_loaded_at
// extension is Phase 2 (E0/E1/E2) and is deliberately NOT read here — a key
// this file does not recognize is simply absent, never a value (parsed
// tolerantly, mirroring cmd/server.go's own reader).
//
// Per Input's own doc comment there is no Input.Server field: B9 sits inside
// the nine-way parallel wave and cannot widen the shared Input type, so this
// file reads and decodes the pidfile and serve.state itself, via
// Input.Paths.RuntimeDir + Env.ReadFile — never importing package cmd (whose
// serveState/pidFilePath/serveStatePath are unexported anyway).
const (
	runtimeStalePIDFile   CheckID = "runtime.stale-pidfile"
	runtimeBindDivergence CheckID = "runtime.bind-divergence"
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
		Detail:   fmt.Sprintf("%s names pid %d, which is not running", path, pid),
		Location: path,
		Remedy:   "run `wraptool server stop` to clear it, or start the server with `wraptool up` / `wraptool server start`",
	})
}

// runtimeServeState is this file's own decode of cmd/server.go's key=value
// serve.state wire format. Absent keys decode to the zero value and are never
// distinguished from "recorded as false/empty" here — today's writer emits
// only listen=/auth=, so that ambiguity is inherent to Phase 1 and is exactly
// what §4.1's config_path extension (Phase 2, NOT this step) exists to close.
type runtimeServeState struct {
	Listen string
	Auth   bool
}

// runtimeParseServeState mirrors cmd/server.go's readServeState: unknown keys
// and unparseable lines are ignored, so a newer or older serve.state is read
// without error (the same forward/backward compatibility cmd's own reader
// relies on).
func runtimeParseServeState(b []byte) runtimeServeState {
	var s runtimeServeState
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "listen":
			s.Listen = val
		case "auth":
			s.Auth = val == "true"
		}
	}
	return s
}

// runRuntimeBindDivergence is design §7.5's sharpest single check:
// `adjustServerBindForRuntime` (cmd/up.go) widens a loopback bind to
// 0.0.0.0 in memory only, for the devcontainer runtime, and never writes the
// change back to config — so a file-only audit reports "loopback, ok" while
// the server actually listens off-host.
//
// Gated on a LIVE pid (via Env.ProcessAlive): serve.state survives SIGKILL,
// so its mere presence proves nothing about whether the recorded bind is
// still in effect. Today's serve.state carries no config_path, so — unlike
// the design's general §7.5 gating note — this check cannot additionally
// confirm the state file belongs to THIS config; that confirmation is Phase
// 2 (E1/E2), once §4.1 lands.
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
	if cfgListen == "" || state.Listen == cfgListen {
		return Pass()
	}

	detail := fmt.Sprintf("server listens on %s, config says %s", state.Listen, cfgListen)

	if runtimeIsUpBindAdjustment(cfgListen, state) {
		return Found(Finding{
			Severity: SeverityWarn,
			Title:    "Server bind differs from config",
			Detail: detail + ". This matches `up`'s devcontainer bind adjustment and the listener " +
				"is authenticated — expected if you started it with `wraptool up`.",
			Remedy: "no action needed if this server was started by `wraptool up`; otherwise restart it against the current config",
		})
	}

	return Found(Finding{
		Severity: SeverityFail,
		Title:    "Server bind differs from config",
		Detail:   detail + fmt.Sprintf(" (authenticated=%v). This is not `up`'s known bind adjustment.", state.Auth),
		Remedy:   "restart the server (`wraptool server stop` then `wraptool up` / `wraptool server start`) so it picks up the configured bind",
	})
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
