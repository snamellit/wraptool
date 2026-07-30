package cmd

import (
	"os"
	"testing"

	"github.com/adrg/xdg"
)

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

// TestServeStateRoundTrip covers the bind/auth state file serve writes next to
// the pidfile, which `up` reads to decide whether a running server matches the
// desired bind/auth (and restarts it if not).
func TestServeStateRoundTrip(t *testing.T) {
	withIsolatedRuntime(t)

	if err := writeServeState(serveState{Listen: "0.0.0.0:8717", Auth: true}); err != nil {
		t.Fatalf("writeServeState: %v", err)
	}
	got, err := readServeState()
	if err != nil {
		t.Fatalf("readServeState: %v", err)
	}
	if got.Listen != "0.0.0.0:8717" || !got.Auth {
		t.Errorf("serveState = %+v, want {0.0.0.0:8717 true}", got)
	}

	removeServeState()
	if _, err := readServeState(); err == nil {
		t.Error("readServeState succeeded after removeServeState; want error (file gone)")
	}
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
