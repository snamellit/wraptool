package paths

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCWD(t *testing.T) {
	configDir := "/config/dir"

	t.Run("fallback to config working dir when context is empty", func(t *testing.T) {
		ctx := context.Background()
		got := ResolveCWD(ctx, configDir)
		if got != configDir {
			t.Errorf("expected %q, got %q", configDir, got)
		}
	})

	t.Run("uses explicit client CWD when set", func(t *testing.T) {
		ctx := context.Background()
		clientCwd := "/client/explicit/dir"
		ctx = WithClientCWD(ctx, clientCwd)

		got := ResolveCWD(ctx, configDir)
		if got != clientCwd {
			t.Errorf("expected %q, got %q", clientCwd, got)
		}
	})

	t.Run("uses peer PID /proc/pid/cwd when client CWD is not set", func(t *testing.T) {
		ctx := context.Background()
		// Use our own process ID to resolve CWD dynamically
		selfPid := os.Getpid()
		ctx = WithPeerPID(ctx, selfPid)

		expectedCwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("failed to get self cwd: %v", err)
		}

		got := ResolveCWD(ctx, configDir)
		// Resolve both paths via EvalSymlinks to handle potential symlinks.
		gotResolved, err1 := filepath.EvalSymlinks(got)
		expectedResolved, err2 := filepath.EvalSymlinks(expectedCwd)
		if err1 == nil && err2 == nil {
			if gotResolved != expectedResolved {
				t.Errorf("expected %q, got %q", expectedResolved, gotResolved)
			}
		} else {
			if got != expectedCwd {
				t.Errorf("expected %q, got %q", expectedCwd, got)
			}
		}
	})

	t.Run("prefers explicit client CWD over peer PID", func(t *testing.T) {
		ctx := context.Background()
		clientCwd := "/client/explicit/dir"
		ctx = WithClientCWD(ctx, clientCwd)
		ctx = WithPeerPID(ctx, 12345) // Mock PID

		got := ResolveCWD(ctx, configDir)
		if got != clientCwd {
			t.Errorf("expected %q, got %q", clientCwd, got)
		}
	})
}
