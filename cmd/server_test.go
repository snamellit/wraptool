package cmd

import (
	"os"
	"testing"

	"github.com/adrg/xdg"
)

// TestPidFileRoundTrip covers the single pidfile writer used by every start path
// (up, direct serve, service manager): writePidFile records this process's pid,
// readPidFile reads it back, removePidFile clears it. Runs against an isolated
// XDG_RUNTIME_DIR so it never touches a real running server's pidfile.
func TestPidFileRoundTrip(t *testing.T) {
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
