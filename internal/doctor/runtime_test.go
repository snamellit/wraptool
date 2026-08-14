package doctor

import (
	"context"
	"strings"
	"testing"
	"time"

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

// runtimeTestInputWithConfigPath is runtimeTestInput plus ConfigPath, for
// every E1/E2 test that exercises runtimeLiveMatchingState's config_path
// gate (runtime.uid, runtime.stale-policy, and — via env_test.go/
// exposure_files_test.go's own helpers — env.path-differs-from-server and
// exposure.token-owner). Kept separate from runtimeTestInput rather than
// widening it, so every already-merged Phase 1 test above (which never sets
// ConfigPath) is untouched.
func runtimeTestInputWithConfigPath(env *MapEnv, cfg *config.Config, configPath string) *Input {
	in := runtimeTestInput(env, cfg)
	in.ConfigPath = configPath
	return in
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

// TestRuntimeParseServeStateParsesPhase2Fields locks in that every field E0
// added (§4.1) round-trips through this file's own reader — the counterpart
// to cmd/server.go's readServeState/writeServeState pair, which this file
// cannot import (see the header comment) and therefore re-implements.
func TestRuntimeParseServeStateParsesPhase2Fields(t *testing.T) {
	s := runtimeParseServeState([]byte("listen=127.0.0.1:8717\n" +
		"auth=true\n" +
		"transport=sse\n" +
		"admin_listen=127.0.0.1:8718\n" +
		"uid=1000\n" +
		"config_path=/etc/wraptool/config.yaml\n" +
		"path=/usr/local/bin:/usr/bin\n" +
		"started_at=2024-01-01T00:00:00Z\n" +
		"config_loaded_at=2024-01-01T00:05:00Z\n"))
	if s.Listen != "127.0.0.1:8717" || !s.Auth {
		t.Fatalf("got %+v", s)
	}
	if s.Transport != "sse" {
		t.Errorf("Transport = %q, want sse", s.Transport)
	}
	if s.AdminListen != "127.0.0.1:8718" {
		t.Errorf("AdminListen = %q", s.AdminListen)
	}
	if uid, ok := s.UID(); !ok || uid != 1000 {
		t.Errorf("UID() = (%d, %v), want (1000, true)", uid, ok)
	}
	if s.ConfigPath != "/etc/wraptool/config.yaml" {
		t.Errorf("ConfigPath = %q", s.ConfigPath)
	}
	if s.Path != "/usr/local/bin:/usr/bin" {
		t.Errorf("Path = %q", s.Path)
	}
	if s.StartedAt != "2024-01-01T00:00:00Z" {
		t.Errorf("StartedAt = %q", s.StartedAt)
	}
	if s.ConfigLoadedAt != "2024-01-01T00:05:00Z" {
		t.Errorf("ConfigLoadedAt = %q", s.ConfigLoadedAt)
	}
}

// TestRuntimeParseServeStateUIDAbsentIsNotZero pins the "absent means
// UNKNOWN, never 0/root" invariant (this file's header, cmd/server.go's own
// serveState.UID() doc comment) against an old-format state file written
// before E0 — no uid key at all.
func TestRuntimeParseServeStateUIDAbsentIsNotZero(t *testing.T) {
	s := runtimeParseServeState([]byte("listen=127.0.0.1:8717\nauth=true\n"))
	if uid, ok := s.UID(); ok {
		t.Errorf("UID() = (%d, true), want ok=false for an old-format state file", uid)
	}
}

// TestRuntimeParseServeStateIgnoresTrulyUnknownKeys keeps the forward-compat
// guarantee this reader has always had (renamed from
// TestRuntimeParseServeStateIgnoresUnknownKeys now that uid=/config_path=
// ARE recognized by this revision): a key even THIS revision does not know
// about — a hypothetical future field — is silently ignored, never
// misparsed into an existing one.
func TestRuntimeParseServeStateIgnoresTrulyUnknownKeys(t *testing.T) {
	s := runtimeParseServeState([]byte("listen=127.0.0.1:8717\nauth=true\nsome_future_key=surprise\n"))
	if s.Listen != "127.0.0.1:8717" || !s.Auth {
		t.Errorf("got %+v, want listen=127.0.0.1:8717 auth=true, unrecognized key ignored", s)
	}
}

// TestRuntimeParseServeStateUnescapes pins the \n/\\ escaping
// cmd/server.go's writeServeState applies to every value (serveStateEscape) —
// a config_path or PATH entry may legitimately contain either character, and
// this file re-implements the inverse rather than importing cmd (header
// comment). Encoded input is built from raw string literals so the
// backslashes below are the literal on-disk bytes, not a Go escape.
func TestRuntimeParseServeStateUnescapes(t *testing.T) {
	line1 := "config_path=" + `/etc/wrap\ntool/config.yaml` + "\n" // encodes an embedded real newline
	line2 := "path=" + `C:\\bin` + "\n"                            // encodes a single literal backslash
	s := runtimeParseServeState([]byte(line1 + line2))

	if want := "/etc/wrap\ntool/config.yaml"; s.ConfigPath != want {
		t.Errorf("ConfigPath = %q, want %q (embedded newline unescaped)", s.ConfigPath, want)
	}
	if want := `C:\bin`; s.Path != want {
		t.Errorf("Path = %q, want %q (doubled backslash collapsed to one)", s.Path, want)
	}
}

// -------------------------------------------------- runtimeResolvedConfigPath

func TestRuntimeResolvedConfigPath(t *testing.T) {
	t.Run("no symlink involved: falls back to the raw absolute path", func(t *testing.T) {
		in := &Input{Env: NewMapEnv(), ConfigPath: "/etc/wraptool/config.yaml"}
		if got := runtimeResolvedConfigPath(in); got != "/etc/wraptool/config.yaml" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("symlinked dotfiles layout resolves to the real target", func(t *testing.T) {
		env := NewMapEnv()
		env.Links["/home/user/.config/wraptool/config.yaml"] = "/home/user/dotfiles/wraptool.yaml"
		env.Files["/home/user/dotfiles/wraptool.yaml"] = MapFile{Mode: 0o644}
		in := &Input{Env: env, ConfigPath: "/home/user/.config/wraptool/config.yaml"}
		if got := runtimeResolvedConfigPath(in); got != "/home/user/dotfiles/wraptool.yaml" {
			t.Errorf("got %q, want the resolved symlink target", got)
		}
	})
}

// -------------------------------------------------- runtimeLiveMatchingState

func TestRuntimeLiveMatchingState(t *testing.T) {
	const cfgPath = "/etc/wraptool/config.yaml"

	t.Run("no live server at all", func(t *testing.T) {
		in := runtimeTestInputWithConfigPath(NewMapEnv(), nil, cfgPath)
		_, skip := runtimeLiveMatchingState(in)
		if skip != "no live server process recorded" {
			t.Errorf("skip = %q", skip)
		}
	})

	t.Run("pidfile present but the process is dead", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		// Deliberately no env.Pids[4412] entry.
		runtimeSetServeState(env, "listen=127.0.0.1:8717\nauth=true\nconfig_path="+cfgPath+"\n")
		in := runtimeTestInputWithConfigPath(env, nil, cfgPath)
		_, skip := runtimeLiveMatchingState(in)
		if skip != "no live server process recorded" {
			t.Errorf("skip = %q", skip)
		}
	})

	t.Run("live pid, no serve.state file at all", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		env.Pids[4412] = true
		in := runtimeTestInputWithConfigPath(env, nil, cfgPath)
		_, skip := runtimeLiveMatchingState(in)
		if skip != "no server state recorded" {
			t.Errorf("skip = %q", skip)
		}
	})

	t.Run("serve.state has no config_path (pre-E0 format)", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		env.Pids[4412] = true
		runtimeSetServeState(env, "listen=127.0.0.1:8717\nauth=true\n")
		in := runtimeTestInputWithConfigPath(env, nil, cfgPath)
		_, skip := runtimeLiveMatchingState(in)
		if skip != "server state does not record a config path" {
			t.Errorf("skip = %q", skip)
		}
	})

	t.Run("config_path names a different config: not this audit's concern", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		env.Pids[4412] = true
		runtimeSetServeState(env, "listen=127.0.0.1:8717\nauth=true\nconfig_path=/other/config.yaml\n")
		in := runtimeTestInputWithConfigPath(env, nil, cfgPath)
		_, skip := runtimeLiveMatchingState(in)
		if skip != "server was started from a different config" {
			t.Errorf("skip = %q", skip)
		}
	})

	t.Run("matching config_path succeeds and returns the decoded state", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		env.Pids[4412] = true
		runtimeSetServeState(env, "listen=127.0.0.1:8717\nauth=true\nuid=1000\nconfig_path="+cfgPath+"\n")
		in := runtimeTestInputWithConfigPath(env, nil, cfgPath)
		state, skip := runtimeLiveMatchingState(in)
		if skip != "" {
			t.Fatalf("skip = %q, want empty", skip)
		}
		if uid, ok := state.UID(); !ok || uid != 1000 {
			t.Errorf("UID() = (%d, %v), want (1000, true)", uid, ok)
		}
	})

	t.Run("matching config_path follows a symlinked config to the same target the server resolved", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		env.Pids[4412] = true
		env.Links["/home/user/.config/wraptool/config.yaml"] = "/home/user/dotfiles/wraptool.yaml"
		env.Files["/home/user/dotfiles/wraptool.yaml"] = MapFile{Mode: 0o644}
		runtimeSetServeState(env, "listen=127.0.0.1:8717\nauth=true\nconfig_path=/home/user/dotfiles/wraptool.yaml\n")
		in := runtimeTestInputWithConfigPath(env, nil, "/home/user/.config/wraptool/config.yaml")
		_, skip := runtimeLiveMatchingState(in)
		if skip != "" {
			t.Errorf("skip = %q, want empty (both spellings resolve to the same file)", skip)
		}
	})
}

// -------------------------------------------------------------- runtime.uid

func TestRunRuntimeUID(t *testing.T) {
	const cfgPath = "/etc/wraptool/config.yaml"

	t.Run("no live matching server: skipped", func(t *testing.T) {
		res := runRuntimeUID(context.Background(), runtimeTestInputWithConfigPath(NewMapEnv(), nil, cfgPath))
		if res.Outcome() != OutcomeSkipped {
			t.Fatalf("outcome = %v, want skipped", res.Outcome())
		}
	})

	t.Run("live matching server, old-format state with no uid key: skipped, never asserted as root", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		env.Pids[4412] = true
		runtimeSetServeState(env, "listen=127.0.0.1:8717\nauth=true\nconfig_path="+cfgPath+"\n")
		res := runRuntimeUID(context.Background(), runtimeTestInputWithConfigPath(env, nil, cfgPath))
		if res.Outcome() != OutcomeSkipped {
			t.Fatalf("outcome = %v, want skipped", res.Outcome())
		}
	})

	t.Run("uid recorded: info finding naming it", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		env.Pids[4412] = true
		runtimeSetServeState(env, "listen=127.0.0.1:8717\nauth=true\nuid=1000\nconfig_path="+cfgPath+"\n")
		res := runRuntimeUID(context.Background(), runtimeTestInputWithConfigPath(env, nil, cfgPath))
		if res.Outcome() != OutcomeFinding {
			t.Fatalf("outcome = %v, want finding", res.Outcome())
		}
		f := res.Findings[0]
		if f.Severity != SeverityInfo {
			t.Errorf("severity = %v, want info", f.Severity)
		}
		if !strings.Contains(f.Detail, "1000") {
			t.Errorf("Detail %q does not name the uid", f.Detail)
		}
	})

	// A fixture-only case: wraptool refuses to run as root before any check
	// executes (cmd/rootuser.go), so uid 0 is unreachable in practice, but
	// this pins that a RECORDED 0 is reported verbatim rather than being
	// treated as "unknown" by any accidental zero-value check.
	t.Run("uid 0 is reported verbatim, never conflated with absent", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		env.Pids[4412] = true
		runtimeSetServeState(env, "listen=127.0.0.1:8717\nauth=true\nuid=0\nconfig_path="+cfgPath+"\n")
		res := runRuntimeUID(context.Background(), runtimeTestInputWithConfigPath(env, nil, cfgPath))
		if res.Outcome() != OutcomeFinding {
			t.Fatalf("outcome = %v, want finding", res.Outcome())
		}
		if !strings.Contains(res.Findings[0].Detail, "uid 0") {
			t.Errorf("Detail %q does not report uid 0 explicitly", res.Findings[0].Detail)
		}
	})
}

// -------------------------------------------------------- runtime.stale-policy

func TestRunRuntimeStalePolicy(t *testing.T) {
	const cfgPath = "/etc/wraptool/config.yaml"
	const loadedAt = "2024-01-01T00:05:00Z"
	baseState := "listen=127.0.0.1:8717\nauth=true\nconfig_path=" + cfgPath + "\nconfig_loaded_at=" + loadedAt + "\n"

	t.Run("no live matching server: skipped", func(t *testing.T) {
		res := runRuntimeStalePolicy(context.Background(), runtimeTestInputWithConfigPath(NewMapEnv(), nil, cfgPath))
		if res.Outcome() != OutcomeSkipped {
			t.Fatalf("outcome = %v, want skipped", res.Outcome())
		}
	})

	t.Run("live matching server, no config_loaded_at recorded: skipped", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		env.Pids[4412] = true
		runtimeSetServeState(env, "listen=127.0.0.1:8717\nauth=true\nconfig_path="+cfgPath+"\n")
		res := runRuntimeStalePolicy(context.Background(), runtimeTestInputWithConfigPath(env, nil, cfgPath))
		if res.Outcome() != OutcomeSkipped {
			t.Fatalf("outcome = %v, want skipped", res.Outcome())
		}
	})

	t.Run("config_loaded_at does not parse as RFC3339: skipped, never asserted", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		env.Pids[4412] = true
		runtimeSetServeState(env, "listen=127.0.0.1:8717\nauth=true\nconfig_path="+cfgPath+"\nconfig_loaded_at=not-a-time\n")
		res := runRuntimeStalePolicy(context.Background(), runtimeTestInputWithConfigPath(env, nil, cfgPath))
		if res.Outcome() != OutcomeSkipped {
			t.Fatalf("outcome = %v, want skipped", res.Outcome())
		}
	})

	t.Run("config file cannot be stat'd: skipped", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		env.Pids[4412] = true
		runtimeSetServeState(env, baseState)
		// Deliberately no env.Files[cfgPath] entry.
		res := runRuntimeStalePolicy(context.Background(), runtimeTestInputWithConfigPath(env, nil, cfgPath))
		if res.Outcome() != OutcomeSkipped {
			t.Fatalf("outcome = %v, want skipped", res.Outcome())
		}
	})

	t.Run("config mtime at or before config_loaded_at: pass", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		env.Pids[4412] = true
		runtimeSetServeState(env, baseState)
		env.Files[cfgPath] = MapFile{Mode: 0o600, ModTime: time.Date(2024, 1, 1, 0, 5, 0, 0, time.UTC)}
		res := runRuntimeStalePolicy(context.Background(), runtimeTestInputWithConfigPath(env, nil, cfgPath))
		if res.Outcome() != OutcomePass {
			t.Fatalf("outcome = %v, want pass", res.Outcome())
		}
	})

	t.Run("config mtime newer than config_loaded_at: warn, reload kept the OLD policy", func(t *testing.T) {
		env := NewMapEnv()
		runtimeSetPIDFile(env, "4412\n")
		env.Pids[4412] = true
		runtimeSetServeState(env, baseState)
		env.Files[cfgPath] = MapFile{Mode: 0o600, ModTime: time.Date(2024, 1, 1, 0, 6, 0, 0, time.UTC)}
		res := runRuntimeStalePolicy(context.Background(), runtimeTestInputWithConfigPath(env, nil, cfgPath))
		if res.Outcome() != OutcomeFinding {
			t.Fatalf("outcome = %v, want finding", res.Outcome())
		}
		f := res.Findings[0]
		if f.Severity != SeverityWarn {
			t.Errorf("severity = %v, want warn", f.Severity)
		}
		if f.Remedy == "" {
			t.Error("warn finding must carry a Remedy")
		}
	})
}

// TestRuntimeStalePolicyIgnoresSubSecondMtime pins the resolution mismatch that
// made this check warn after every successful reload: cmd/server.go records
// config_loaded_at at whole-second RFC3339 precision, while a file mtime carries
// nanoseconds, so a config saved a fraction of a second before the load that
// read it is not stale.
func TestRuntimeStalePolicyIgnoresSubSecondMtime(t *testing.T) {
	loaded := time.Date(2026, 8, 11, 12, 0, 5, 0, time.UTC)

	cases := []struct {
		name  string
		mtime time.Time
		want  Outcome
	}{
		{"saved within the same second as the load", loaded.Add(400 * time.Millisecond), OutcomePass},
		{"saved exactly at the recorded load time", loaded, OutcomePass},
		{"saved before the load", loaded.Add(-2 * time.Second), OutcomePass},
		{"genuinely edited after the load", loaded.Add(30 * time.Second), OutcomeFinding},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const cfgPath = "/etc/wraptool/config.yaml"
			env := NewMapEnv()
			runtimeSetPIDFile(env, "4412\n")
			env.Pids[4412] = true
			runtimeSetServeState(env, "listen=127.0.0.1:8717\nauth=true\nconfig_path="+cfgPath+
				"\nconfig_loaded_at="+loaded.Format(time.RFC3339)+"\n")
			env.Files[cfgPath] = MapFile{Mode: 0o600, ModTime: tc.mtime}
			res := runRuntimeStalePolicy(context.Background(), runtimeTestInputWithConfigPath(env, nil, cfgPath))
			if res.Outcome() != tc.want {
				t.Errorf("outcome = %v, want %v (findings=%+v, skip=%q)", res.Outcome(), tc.want, res.Findings, res.SkipReason)
			}
		})
	}
}
