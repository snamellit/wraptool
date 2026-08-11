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
		{"pidfile present but dead", func() *MapEnv {
			e := NewMapEnv()
			runtimeSetPIDFile(e, "4412\n")
			return e
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runRuntimeBindDivergence(context.Background(), runtimeTestInput(tc.env, runtimeCfg("127.0.0.1:8717", "")))
			if res.Outcome() != OutcomeSkipped {
				t.Fatalf("outcome %v, want skipped", res.Outcome())
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

func TestRunRuntimeBindDivergenceEmptyConfigListenIsPass(t *testing.T) {
	// A config with no MCP.Listen at all cannot meaningfully diverge — asserting
	// a fail from an unset comparison value would be a false positive that is
	// really config.validate's job, not this check's.
	env := NewMapEnv()
	runtimeSetPIDFile(env, "4412\n")
	env.Pids[4412] = true
	runtimeSetServeState(env, "listen=0.0.0.0:8717\nauth=true\n")

	res := runRuntimeBindDivergence(context.Background(), runtimeTestInput(env, runtimeCfg("", "")))
	if res.Outcome() != OutcomePass {
		t.Fatalf("empty config listen: outcome %v, want pass", res.Outcome())
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

func TestRunRuntimeBindDivergenceOtherShapesAreFail(t *testing.T) {
	cases := []struct {
		name         string
		cfgListen    string
		stateListen  string
		auth         bool
		wantSeverity Severity
		wantOutcome  Outcome
	}{
		{
			name:         "authenticated but different port (not up's transformation)",
			cfgListen:    "127.0.0.1:8717",
			stateListen:  "0.0.0.0:9999",
			auth:         true,
			wantSeverity: SeverityFail,
			wantOutcome:  OutcomeFinding,
		},
		{
			name:         "authenticated but different host, not the wildcard",
			cfgListen:    "127.0.0.1:8717",
			stateListen:  "10.0.0.5:8717",
			auth:         true,
			wantSeverity: SeverityFail,
			wantOutcome:  OutcomeFinding,
		},
		{
			name:         "config listen was already non-loopback",
			cfgListen:    "0.0.0.0:8717",
			stateListen:  "0.0.0.0:9000",
			auth:         true,
			wantSeverity: SeverityFail,
			wantOutcome:  OutcomeFinding,
		},
		{
			name:         "unix socket path, divergent, unauthenticated",
			cfgListen:    "/run/wraptool/wraptool.sock",
			stateListen:  "/run/wraptool/other.sock",
			auth:         false,
			wantSeverity: SeverityFail,
			wantOutcome:  OutcomeFinding,
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
			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("outcome %v, want %v", res.Outcome(), tc.wantOutcome)
			}
			if got := res.Findings[0].Severity; got != tc.wantSeverity {
				t.Errorf("severity %v, want %v", got, tc.wantSeverity)
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
