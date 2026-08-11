package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

// runtimeHostPaths is the fixed HostPaths every test in this file builds its
// Input from, so runtimePIDFilePath/runtimeServeStatePath resolve to a
// predictable location the test can also populate in the MapEnv.
var runtimeHostPaths = HostPaths{RuntimeDir: "/run/user/1000/wraptool"}

// runtimeTestInput builds a hand-crafted *Input directly (plan §1 rule 8: no
// test here asserts over the global Registered()).
func runtimeTestInput(env *MapEnv, cfg *config.Config) *Input {
	return &Input{
		Env:    env,
		GOOS:   "linux",
		Paths:  runtimeHostPaths,
		Config: cfg,
	}
}

func runtimeSetPIDFile(env *MapEnv, content string) {
	env.Files[runtimePIDFilePath(&Input{Paths: runtimeHostPaths})] = MapFile{Data: []byte(content)}
}

func runtimeSetServeState(env *MapEnv, content string) {
	env.Files[runtimeServeStatePath(&Input{Paths: runtimeHostPaths})] = MapFile{Data: []byte(content)}
}

// ---------------------------------------------------------- stale-pidfile

func TestRunRuntimeStalePIDFileNoPIDFile(t *testing.T) {
	res := runRuntimeStalePIDFile(context.Background(), runtimeTestInput(NewMapEnv(), nil))
	if res.Outcome() != OutcomePass {
		t.Fatalf("no pidfile: outcome %v, want pass (nothing to report yet)", res.Outcome())
	}
}

func TestRunRuntimeStalePIDFileLiveProcess(t *testing.T) {
	env := NewMapEnv()
	runtimeSetPIDFile(env, "4412\n")
	env.Pids[4412] = true

	res := runRuntimeStalePIDFile(context.Background(), runtimeTestInput(env, nil))
	if res.Outcome() != OutcomePass {
		t.Fatalf("live pid: outcome %v, want pass", res.Outcome())
	}
}

func TestRunRuntimeStalePIDFileDeadProcess(t *testing.T) {
	env := NewMapEnv()
	runtimeSetPIDFile(env, "4412\n")
	// Deliberately no env.Pids[4412] entry: ProcessAlive reports false.

	res := runRuntimeStalePIDFile(context.Background(), runtimeTestInput(env, nil))
	if res.Outcome() != OutcomeFinding {
		t.Fatalf("dead pid: outcome %v, want finding", res.Outcome())
	}
	if len(res.Findings) != 1 {
		t.Fatalf("dead pid: got %d findings, want 1", len(res.Findings))
	}
	f := res.Findings[0]
	if f.Severity != SeverityWarn {
		t.Errorf("dead pid: severity %v, want warn", f.Severity)
	}
	if !strings.Contains(f.Detail, "4412") {
		t.Errorf("dead pid: Detail %q does not name the pid", f.Detail)
	}
	if f.Remedy == "" {
		t.Error("dead pid: warn finding must carry a Remedy")
	}
	// This is a non-config subject (the pidfile itself), so it is one of the
	// sanctioned cases where a check MAY set Location directly (finding.go's
	// Location doc comment) — never Path, which stays empty.
	if f.Location != runtimePIDFilePath(runtimeTestInput(env, nil)) {
		t.Errorf("dead pid: Location %q, want the pidfile path", f.Location)
	}
	if !f.Path.Empty() {
		t.Error("dead pid: Path must stay empty — this is not a config finding (rule 9)")
	}
}

func TestRunRuntimeStalePIDFileMalformedContent(t *testing.T) {
	env := NewMapEnv()
	runtimeSetPIDFile(env, "not-a-pid\n")

	res := runRuntimeStalePIDFile(context.Background(), runtimeTestInput(env, nil))
	if res.Outcome() != OutcomeSkipped {
		t.Fatalf("malformed pidfile: outcome %v, want skipped (never asserted as a value)", res.Outcome())
	}
	if res.SkipReason == "" {
		t.Error("malformed pidfile: SkipReason must explain why")
	}
}

// TestRunRuntimeStalePIDFileZeroPID pins runtimeReadPID's own doc comment: a
// pidfile of literally "0" parses fine as an int, but Env.ProcessAlive's real
// implementation is `syscall.Kill(pid, 0)`, and kill(0, 0) signals the
// caller's OWN process group and returns nil — so an ungated pid 0 would
// read back as "alive" here even though it names no real process. Treated as
// a parse failure, same as non-numeric content, never reaching ProcessAlive.
func TestRunRuntimeStalePIDFileZeroPID(t *testing.T) {
	env := NewMapEnv()
	runtimeSetPIDFile(env, "0\n")

	res := runRuntimeStalePIDFile(context.Background(), runtimeTestInput(env, nil))
	if res.Outcome() != OutcomeSkipped {
		t.Fatalf("pid 0: outcome %v, want skipped (never asserted as a value)", res.Outcome())
	}
	if res.SkipReason == "" {
		t.Error("pid 0: SkipReason must explain why")
	}
}

// TestRunRuntimeStalePIDFileDetailOmitsPath locks in that Detail no longer
// repeats the pidfile path already carried in Location (the renderer prints
// Location itself, so a Detail duplicating it prints the path twice on one
// line).
func TestRunRuntimeStalePIDFileDetailOmitsPath(t *testing.T) {
	env := NewMapEnv()
	runtimeSetPIDFile(env, "4412\n")
	// Deliberately no env.Pids[4412] entry: ProcessAlive reports false.

	res := runRuntimeStalePIDFile(context.Background(), runtimeTestInput(env, nil))
	path := runtimePIDFilePath(runtimeTestInput(env, nil))
	f := res.Findings[0]
	if strings.Contains(f.Detail, path) {
		t.Errorf("Detail %q repeats the pidfile path already in Location %q", f.Detail, f.Location)
	}
}

// ---------------------------------------------------------- bind-divergence

func runtimeCfg(listen, authTokenFile string) *config.Config {
	return &config.Config{MCP: config.MCPConfig{Transport: "sse", Listen: listen, AuthTokenFile: authTokenFile}}
}

func TestRunRuntimeBindDivergenceNoLiveServer(t *testing.T) {
	cases := []struct {
		name string
		env  *MapEnv
	}{
		{"no pidfile at all", NewMapEnv()},
		{"pidfile present but dead, with a state file that would otherwise diverge", func() *MapEnv {
			// A mutation that deletes the `|| !in.Env.ProcessAlive(pid)` liveness
			// gate must turn this red, not stay green by falling through to a
			// LATER guard — so this fixture writes a serve.state whose listen
			// genuinely diverges from cfg (unlike the sibling "no pidfile" case,
			// which would still skip on the "no serve.state" guard alone).
			e := NewMapEnv()
			runtimeSetPIDFile(e, "4412\n")
			// Deliberately no env.Pids[4412] entry: ProcessAlive reports false.
			runtimeSetServeState(e, "listen=0.0.0.0:9999\nauth=false\n")
			return e
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runRuntimeBindDivergence(context.Background(), runtimeTestInput(tc.env, runtimeCfg("127.0.0.1:8717", "")))
			if res.Outcome() != OutcomeSkipped {
				t.Fatalf("outcome %v, want skipped", res.Outcome())
			}
			if res.SkipReason != "no live server process recorded" {
				t.Fatalf("SkipReason %q, want the liveness-gate reason (not a later guard)", res.SkipReason)
			}
		})
	}
}

func TestRunRuntimeBindDivergenceLiveButNoState(t *testing.T) {
	env := NewMapEnv()
	runtimeSetPIDFile(env, "4412\n")
	env.Pids[4412] = true
	// No serve.state written.

	res := runRuntimeBindDivergence(context.Background(), runtimeTestInput(env, runtimeCfg("127.0.0.1:8717", "")))
	if res.Outcome() != OutcomeSkipped {
		t.Fatalf("outcome %v, want skipped", res.Outcome())
	}
}

func TestRunRuntimeBindDivergenceStateHasNoListenKey(t *testing.T) {
	env := NewMapEnv()
	runtimeSetPIDFile(env, "4412\n")
	env.Pids[4412] = true
	runtimeSetServeState(env, "auth=true\n")

	res := runRuntimeBindDivergence(context.Background(), runtimeTestInput(env, runtimeCfg("127.0.0.1:8717", "")))
	if res.Outcome() != OutcomeSkipped {
		t.Fatalf("outcome %v, want skipped (no listen recorded)", res.Outcome())
	}
}

func TestRunRuntimeBindDivergenceMatchingListenIsPass(t *testing.T) {
	env := NewMapEnv()
	runtimeSetPIDFile(env, "4412\n")
	env.Pids[4412] = true
	runtimeSetServeState(env, "listen=127.0.0.1:8717\nauth=false\n")

	res := runRuntimeBindDivergence(context.Background(), runtimeTestInput(env, runtimeCfg("127.0.0.1:8717", "")))
	if res.Outcome() != OutcomePass {
		t.Fatalf("matching listen: outcome %v, want pass", res.Outcome())
	}
}

func TestRunRuntimeBindDivergenceEmptyConfigListenIsSkipped(t *testing.T) {
	// A config with no MCP.Listen at all (an explicit `mcp.transport: sse`
	// with no listen leaves Config.MCP.Listen empty — applyDefaults only
	// fills it when Transport itself is empty) gives this check nothing to
	// compare the running server's bind against: it could not evaluate the
	// case, which is "skipped", not "checked and found no divergence" —
	// mirroring the state.Listen == "" branch one line above it.
	env := NewMapEnv()
	runtimeSetPIDFile(env, "4412\n")
	env.Pids[4412] = true
	runtimeSetServeState(env, "listen=0.0.0.0:8717\nauth=true\n")

	res := runRuntimeBindDivergence(context.Background(), runtimeTestInput(env, runtimeCfg("", "")))
	if res.Outcome() != OutcomeSkipped {
		t.Fatalf("empty config listen: outcome %v, want skipped", res.Outcome())
	}
}

func TestRunRuntimeBindDivergenceUpAdjustmentIsWarn(t *testing.T) {
	env := NewMapEnv()
	runtimeSetPIDFile(env, "4412\n")
	env.Pids[4412] = true
	// Exactly adjustServerBindForRuntime's transformation: loopback -> same
	// port on 0.0.0.0, authenticated.
	runtimeSetServeState(env, "listen=0.0.0.0:8717\nauth=true\n")

	res := runRuntimeBindDivergence(context.Background(), runtimeTestInput(env, runtimeCfg("127.0.0.1:8717", "/etc/wraptool/mcp-token")))
	if res.Outcome() != OutcomeFinding {
		t.Fatalf("outcome %v, want finding", res.Outcome())
	}
	f := res.Findings[0]
	if f.Severity != SeverityWarn {
		t.Fatalf("up's own bind adjustment: severity %v, want warn", f.Severity)
	}
	if f.Remedy == "" {
		t.Error("warn finding must carry a Remedy")
	}
	if f.Path.Empty() && f.Location != "" {
		t.Errorf("bind-divergence has no single config location to point at; Location should stay empty, got %q", f.Location)
	}
}

func TestRunRuntimeBindDivergenceUnauthenticatedWideningIsFail(t *testing.T) {
	env := NewMapEnv()
	runtimeSetPIDFile(env, "4412\n")
	env.Pids[4412] = true
	// Same host/port shape as up's adjustment, but NOT authenticated.
	runtimeSetServeState(env, "listen=0.0.0.0:8717\nauth=false\n")

	res := runRuntimeBindDivergence(context.Background(), runtimeTestInput(env, runtimeCfg("127.0.0.1:8717", "")))
	if res.Outcome() != OutcomeFinding {
		t.Fatalf("outcome %v, want finding", res.Outcome())
	}
	if f := res.Findings[0]; f.Severity != SeverityFail {
		t.Fatalf("unauthenticated widening: severity %v, want fail", f.Severity)
	}
}

// TestRunRuntimeBindDivergenceOtherShapesAreWarn covers the ARCHITECT RULING
// (design §7.5's preamble requires a config_path match this Phase-1 check
// cannot make — see runRuntimeBindDivergence's doc comment): SeverityFail is
// reserved for an unauthenticated listener reachable off-host, which is
// dangerous no matter which config started the server. Every other
// divergence — including shapes the design's own table would otherwise call
// `fail` — is SeverityWarn here, because today's serve.state cannot confirm
// the running server was started from the config under audit, and a
// same-config false FAIL would tell an operator to stop a server that is
// correctly serving a different config.
func TestRunRuntimeBindDivergenceOtherShapesAreWarn(t *testing.T) {
	cases := []struct {
		name        string
		cfgListen   string
		stateListen string
		auth        bool
	}{
		{
			name:        "authenticated but different port (not up's transformation)",
			cfgListen:   "127.0.0.1:8717",
			stateListen: "0.0.0.0:9999",
			auth:        true,
		},
		{
			name:        "authenticated but different host, not the wildcard",
			cfgListen:   "127.0.0.1:8717",
			stateListen: "10.0.0.5:8717",
			auth:        true,
		},
		{
			name:        "config listen was already non-loopback",
			cfgListen:   "0.0.0.0:8717",
			stateListen: "0.0.0.0:9000",
			auth:        true,
		},
		{
			// A unix socket is filesystem-permission guarded, not a network
			// listener (config.Validate agrees: its own auth requirement is
			// gated on transport == "sse", not "unix") — this check cannot
			// confirm network exposure from a value that does not even parse
			// as host:port, so it never asserts SeverityFail from it.
			name:        "unix socket path, divergent, unauthenticated",
			cfgListen:   "/run/wraptool/wraptool.sock",
			stateListen: "/run/wraptool/other.sock",
			auth:        false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			runtimeSetPIDFile(env, "4412\n")
			env.Pids[4412] = true
			authStr := "false"
			if tc.auth {
				authStr = "true"
			}
			runtimeSetServeState(env, "listen="+tc.stateListen+"\nauth="+authStr+"\n")

			res := runRuntimeBindDivergence(context.Background(), runtimeTestInput(env, runtimeCfg(tc.cfgListen, "")))
			if res.Outcome() != OutcomeFinding {
				t.Fatalf("outcome %v, want finding", res.Outcome())
			}
			f := res.Findings[0]
			if f.Severity != SeverityWarn {
				t.Errorf("severity %v, want warn", f.Severity)
			}
			if !strings.Contains(f.Detail, "serve.state records no config path") {
				t.Errorf("Detail %q does not state the config_path limitation", f.Detail)
			}
		})
	}
}

// TestRuntimeListenIsNetworkNonLoopbackTable exercises the network-exposure
// predicate that gates SeverityFail directly.
func TestRuntimeListenIsNetworkNonLoopbackTable(t *testing.T) {
	cases := []struct {
		name   string
		listen string
		want   bool
	}{
		{"wildcard host", "0.0.0.0:8717", true},
		{"LAN ip", "10.0.0.5:8717", true},
		{"loopback ip", "127.0.0.1:8717", false},
		{"localhost spelling", "localhost:8717", false},
		{"unix socket path does not parse as host:port", "/run/wraptool/wraptool.sock", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runtimeListenIsNetworkNonLoopback(tc.listen); got != tc.want {
				t.Errorf("runtimeListenIsNetworkNonLoopback(%q) = %v, want %v", tc.listen, got, tc.want)
			}
		})
	}
}

// TestRuntimeIsUpBindAdjustmentTable exercises the transformation predicate
// directly against the shapes it must and must not recognize.
func TestRuntimeIsUpBindAdjustmentTable(t *testing.T) {
	cases := []struct {
		name      string
		cfgListen string
		state     runtimeServeState
		want      bool
	}{
		{"exact up transformation", "127.0.0.1:8717", runtimeServeState{Listen: "0.0.0.0:8717", Auth: true}, true},
		{"localhost spelling still loopback", "localhost:8717", runtimeServeState{Listen: "0.0.0.0:8717", Auth: true}, true},
		{"unauthenticated does not count", "127.0.0.1:8717", runtimeServeState{Listen: "0.0.0.0:8717", Auth: false}, false},
		{"port must match", "127.0.0.1:8717", runtimeServeState{Listen: "0.0.0.0:9000", Auth: true}, false},
		{"host must be the wildcard, not e.g. a LAN IP", "127.0.0.1:8717", runtimeServeState{Listen: "10.0.0.5:8717", Auth: true}, false},
		{"config listen must itself be loopback", "0.0.0.0:8717", runtimeServeState{Listen: "0.0.0.0:8717", Auth: true}, false},
		{"unparsable config listen (no port)", "not-a-host-port", runtimeServeState{Listen: "0.0.0.0:8717", Auth: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runtimeIsUpBindAdjustment(tc.cfgListen, tc.state); got != tc.want {
				t.Errorf("runtimeIsUpBindAdjustment(%q, %+v) = %v, want %v", tc.cfgListen, tc.state, got, tc.want)
			}
		})
	}
}

// TestRuntimeParseServeStateIgnoresUnknownKeys locks in tolerant parsing: a
// future Phase-2 key (uid=, config_path=, config_loaded_at=) that this Phase-1
// reader does not recognize must be silently ignored, not misparsed into
// listen/auth.
func TestRuntimeParseServeStateIgnoresUnknownKeys(t *testing.T) {
	s := runtimeParseServeState([]byte("listen=127.0.0.1:8717\nauth=true\nuid=1000\nconfig_path=/etc/wraptool/config.yaml\n"))
	if s.Listen != "127.0.0.1:8717" || !s.Auth {
		t.Errorf("got %+v, want listen=127.0.0.1:8717 auth=true, unknown keys ignored", s)
	}
}
