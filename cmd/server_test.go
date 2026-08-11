package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pti/wraptool/internal/paths"
	"github.com/pti/wraptool/internal/requestlog"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/adrg/xdg"
	"github.com/mark3labs/mcp-go/server"
)

// reloadableConfig returns a config body with one allow-ruled tool whose
// binary is a tiny shell script — never the test binary itself: a real
// reload introspects every configured tool (internal/mcptools/cli_source.go's
// cliSource.Tools), and exec'ing the compiled go test binary as a CLI would
// recurse into the whole test suite instead of returning promptly.
func reloadableConfig(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\necho usage: git [--help]\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`
mcp:
  transport: stdio
tools:
  git:
    binary: %s
    allow:
      - subcommand: [status]
        flags: [--short]
        flag_constraints:
          --short:
            type: boolean
`, bin)
}

// withIsolatedRuntime points XDG_RUNTIME_DIR at a temp dir (so pidfile/state
// helpers never touch a real running server) and reloads xdg. Registers a
// t.Cleanup that restores the prior env, so callers don't need to defer.
func withIsolatedRuntime(t *testing.T) {
	t.Helper()
	old, had := os.LookupEnv("XDG_RUNTIME_DIR")
	if err := os.Setenv("XDG_RUNTIME_DIR", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	xdg.Reload()
	t.Cleanup(func() {
		if had {
			os.Setenv("XDG_RUNTIME_DIR", old)
		} else {
			os.Unsetenv("XDG_RUNTIME_DIR")
		}
		xdg.Reload()
	})
}

// TestPidFileRoundTrip covers the single pidfile writer used by every start path
// (up, direct serve, service manager): writePidFile records this process's pid,
// readPidFile reads it back, removePidFile clears it. Runs against an isolated
// XDG_RUNTIME_DIR so it never touches a real running server's pidfile.
func TestPidFileRoundTrip(t *testing.T) {
	withIsolatedRuntime(t)

	if err := writePidFile(); err != nil {
		t.Fatalf("writePidFile: %v", err)
	}
	pid, err := readPidFile()
	if err != nil {
		t.Fatalf("readPidFile: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("pidfile pid = %d, want %d", pid, os.Getpid())
	}

	removePidFile()
	if _, err := readPidFile(); err == nil {
		t.Error("readPidFile succeeded after removePidFile; want error (file gone)")
	}
}

// TestServeStateRoundTrip covers every field of the extended serve state
// (design/doctor/wraptool-doctor.md §4.1): a fully populated serveState
// survives write+read unchanged, including UID() reporting the recorded uid
// as present.
func TestServeStateRoundTrip(t *testing.T) {
	withIsolatedRuntime(t)

	want := serveState{
		Listen:         "0.0.0.0:8717",
		Auth:           true,
		Transport:      "sse",
		AdminListen:    "127.0.0.1:8718",
		ConfigPath:     "/home/dev/.config/wraptool/config.yaml",
		Path:           "/usr/local/bin:/usr/bin:/bin",
		StartedAt:      "2024-01-01T00:00:00Z",
		ConfigLoadedAt: "2024-01-01T00:00:05Z",
		uid:            1000,
		hasUID:         true,
	}
	if err := writeServeState(want); err != nil {
		t.Fatalf("writeServeState: %v", err)
	}
	got, err := readServeState()
	if err != nil {
		t.Fatalf("readServeState: %v", err)
	}
	if got != want {
		t.Errorf("serveState round trip = %+v, want %+v", got, want)
	}
	if uid, ok := got.UID(); !ok || uid != 1000 {
		t.Errorf("UID() = (%d, %v), want (1000, true)", uid, ok)
	}

	removeServeState()
	if _, err := readServeState(); err == nil {
		t.Error("readServeState succeeded after removeServeState; want error (file gone)")
	}
}

// TestServeStateOldFormatFileIsForwardCompatible reads a state file in the
// original two-field format (as an already-running, not-yet-upgraded wraptool
// would have written it): the new fields must decode absent, never to a
// colliding zero value — most importantly uid, where 0 is root as well as the
// zero value an absent key yields, so UID()'s ok must come back false rather
// than silently reporting root.
func TestServeStateOldFormatFileIsForwardCompatible(t *testing.T) {
	withIsolatedRuntime(t)

	if err := os.MkdirAll(paths.RuntimeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	old := "listen=0.0.0.0:8717\nauth=true\n"
	if err := os.WriteFile(serveStatePath(), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readServeState()
	if err != nil {
		t.Fatalf("readServeState: %v", err)
	}
	if got.Listen != "0.0.0.0:8717" || !got.Auth {
		t.Errorf("existing fields decoded wrong: %+v", got)
	}
	if got.Transport != "" || got.AdminListen != "" || got.ConfigPath != "" ||
		got.Path != "" || got.StartedAt != "" || got.ConfigLoadedAt != "" {
		t.Errorf("new string fields should decode absent from an old-format file, got %+v", got)
	}
	if uid, ok := got.UID(); ok {
		t.Errorf("UID() = (%d, true) reading an old-format file; want ok=false (unknown, not root)", uid)
	}

	// A state file that explicitly records uid=0 (root) must be
	// distinguishable from the old-format case above: both decode the value
	// 0, but only one of them is actually known to be root.
	if err := writeServeState(serveState{Listen: "0.0.0.0:8717", Auth: true, uid: 0, hasUID: true}); err != nil {
		t.Fatal(err)
	}
	gotRoot, err := readServeState()
	if err != nil {
		t.Fatal(err)
	}
	if uid, ok := gotRoot.UID(); !ok || uid != 0 {
		t.Errorf("UID() = (%d, %v), want (0, true) for an explicitly recorded root uid", uid, ok)
	}
}

// TestServeStateEscaping covers the two wire-format hazards §4.1 calls out: a
// value containing "=" must survive intact (cutting on the first "=" only),
// and a value containing a newline — the one real hazard, since a POSIX
// filename may contain one and config_path/path are filesystem/PATH-derived —
// must round-trip via escape/unescape without corrupting the file (splitting
// it into a bogus extra line that could clobber a neighboring field).
func TestServeStateEscaping(t *testing.T) {
	withIsolatedRuntime(t)

	want := serveState{
		Listen:     "0.0.0.0:8717",
		Auth:       true,
		ConfigPath: "/home/dev/weird=name/config.yaml",
		Path:       "/usr/bin:/opt/weird\nname/bin",
	}
	if err := writeServeState(want); err != nil {
		t.Fatalf("writeServeState: %v", err)
	}
	got, err := readServeState()
	if err != nil {
		t.Fatalf("readServeState: %v", err)
	}
	if got.ConfigPath != want.ConfigPath {
		t.Errorf("ConfigPath = %q, want %q", got.ConfigPath, want.ConfigPath)
	}
	if got.Path != want.Path {
		t.Errorf("Path = %q, want %q", got.Path, want.Path)
	}
	// The embedded newline must not have split the file into a garbage extra
	// line that corrupts a neighboring field.
	if got.Listen != want.Listen || got.Auth != want.Auth {
		t.Errorf("neighboring fields corrupted by an embedded newline: got %+v", got)
	}
}

// TestServeStateAtomicWriteNoTornRead stresses writeServeState's
// temp-then-rename discipline: a concurrent reader must always observe either
// a complete prior write or a complete new one, never a partial file — the
// hazard write-temp-then-rename exists to close now that serve.state is
// rewritten on every reload while doctor and `up` may be reading it.
func TestServeStateAtomicWriteNoTornRead(t *testing.T) {
	withIsolatedRuntime(t)

	a := serveState{
		Listen: "127.0.0.1:1", Auth: true, Transport: "sse", AdminListen: "127.0.0.1:2",
		ConfigPath: "/a/config.yaml", Path: "/usr/bin", StartedAt: "2024-01-01T00:00:00Z",
		ConfigLoadedAt: "2024-01-01T00:00:00Z", uid: 1000, hasUID: true,
	}
	b := serveState{
		Listen: "0.0.0.0:3", Auth: false, Transport: "unix", AdminListen: "",
		ConfigPath: "/b/config.yaml", Path: "/bin", StartedAt: "2024-01-01T00:00:01Z",
		ConfigLoadedAt: "2024-01-01T00:00:01Z", uid: 0, hasUID: true,
	}
	if err := writeServeState(a); err != nil {
		t.Fatal(err)
	}

	const iterations = 300
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range iterations {
			s := a
			if i%2 == 1 {
				s = b
			}
			if err := writeServeState(s); err != nil {
				t.Errorf("writeServeState: %v", err)
				return
			}
		}
	}()

	for range iterations {
		got, err := readServeState()
		if err != nil {
			t.Fatalf("readServeState: %v", err)
		}
		if got != a && got != b {
			t.Fatalf("readServeState observed a torn/partial write: %+v", got)
		}
	}
	wg.Wait()
}

// TestRunningMatches asserts the reconciliation predicate: a recorded state
// matches an identical desired state, differs on bind or auth change, and
// reports no match when the state file is missing (a server started by an
// older wraptool or out-of-band — `up` restarts it).
func TestRunningMatches(t *testing.T) {
	withIsolatedRuntime(t)

	if err := writeServeState(serveState{Listen: "0.0.0.0:8717", Auth: true}); err != nil {
		t.Fatal(err)
	}
	if !runningMatches(serveState{Listen: "0.0.0.0:8717", Auth: true}) {
		t.Error("identical state should match")
	}
	if runningMatches(serveState{Listen: "127.0.0.1:8717", Auth: true}) {
		t.Error("different listen should not match")
	}
	if runningMatches(serveState{Listen: "0.0.0.0:8717", Auth: false}) {
		t.Error("different auth should not match")
	}

	removeServeState()
	if runningMatches(serveState{Listen: "0.0.0.0:8717", Auth: true}) {
		t.Error("missing state file should not match (unknown bind → restart)")
	}
}

// TestRunningMatchesIgnoresNewFields confirms runningMatches keeps comparing
// only Listen+Auth: folding config_path/uid/etc. into that comparison would
// make `up` restart a perfectly good server just because it was started from
// a different config path (design/doctor/wraptool-doctor.md §4.1).
func TestRunningMatchesIgnoresNewFields(t *testing.T) {
	withIsolatedRuntime(t)

	recorded := serveState{
		Listen: "0.0.0.0:8717", Auth: true,
		Transport: "sse", AdminListen: "127.0.0.1:9000",
		ConfigPath: "/a/config.yaml", Path: "/usr/bin",
		StartedAt: "2024-01-01T00:00:00Z", ConfigLoadedAt: "2024-01-01T00:00:00Z",
		uid: 1000, hasUID: true,
	}
	if err := writeServeState(recorded); err != nil {
		t.Fatal(err)
	}

	desired := serveState{Listen: "0.0.0.0:8717", Auth: true} // every new field left zero
	if !runningMatches(desired) {
		t.Error("runningMatches should match on listen+auth alone, ignoring differing new fields")
	}
}

// TestReloadConfigConfigLoadedAt is the compatibility test for
// cmd/reload.go's success-path hook: a failed reload (invalid config) must
// leave config_loaded_at — and every other recorded field — untouched, since
// that untouched timestamp is runtime.stale-policy's stale-config signal; a
// successful reload advances config_loaded_at and nothing else (the process
// does not rebind on reload).
func TestReloadConfigConfigLoadedAt(t *testing.T) {
	withIsolatedRuntime(t)

	configPath := writeCfg(t, reloadableConfig(t))
	reg := newRegistrar(server.NewMCPServer("wraptool-test", "0.0.0"),
		whitelist.NewPolicy(nil), whitelist.NewMCPPolicy(nil),
		requestlog.NewStore(filepath.Join(t.TempDir(), "requests.jsonl")))

	initial := serveState{
		Listen: "0.0.0.0:8717", Auth: true, Transport: "stdio",
		ConfigLoadedAt: "2000-01-01T00:00:00Z",
	}
	if err := writeServeState(initial); err != nil {
		t.Fatal(err)
	}

	// Corrupt the config: reload must fail without touching serve state at all.
	if err := os.WriteFile(configPath, []byte("not: [valid yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	reloadConfig(configPath, reg, "test")
	got, err := readServeState()
	if err != nil {
		t.Fatal(err)
	}
	if got != initial {
		t.Errorf("failed reload changed serve state: got %+v, want unchanged %+v", got, initial)
	}

	// Restore a valid config: reload succeeds and advances config_loaded_at,
	// leaving listen/auth/transport untouched.
	if err := os.WriteFile(configPath, []byte(reloadableConfig(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	reloadConfig(configPath, reg, "test")
	got2, err := readServeState()
	if err != nil {
		t.Fatal(err)
	}
	if got2.ConfigLoadedAt == initial.ConfigLoadedAt {
		t.Error("successful reload did not advance config_loaded_at")
	}
	if got2.Listen != initial.Listen || got2.Auth != initial.Auth || got2.Transport != initial.Transport {
		t.Errorf("successful reload changed bind/auth/transport, want unchanged: %+v", got2)
	}
}
