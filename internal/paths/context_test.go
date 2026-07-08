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
		got, err := ResolveCWD(ctx, configDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != configDir {
			t.Errorf("expected %q, got %q", configDir, got)
		}
	})

	t.Run("uses explicit client CWD when set", func(t *testing.T) {
		ctx := context.Background()
		clientCwd := "/client/explicit/dir"
		ctx = WithClientCWD(ctx, clientCwd)

		got, err := ResolveCWD(ctx, configDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
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

		got, err := ResolveCWD(ctx, configDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
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

		got, err := ResolveCWD(ctx, configDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != clientCwd {
			t.Errorf("expected %q, got %q", clientCwd, got)
		}
	})

	t.Run("containment: rejects client CWD outside allowed roots", func(t *testing.T) {
		// A real, existing dir tree so EvalSymlinks resolves; root and target diverge.
		base := t.TempDir()
		allowed := filepath.Join(base, "allowed")
		outside := filepath.Join(base, "outside")
		for _, d := range []string{allowed, outside} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}

		inside := filepath.Join(allowed, "proj")
		if err := os.MkdirAll(inside, 0o755); err != nil {
			t.Fatal(err)
		}

		ctx := WithAllowedCWDRoots(WithClientCWD(context.Background(), inside), []string{allowed})
		if _, err := ResolveCWD(ctx, configDir); err != nil {
			t.Errorf("cwd within an allowed root should pass, got: %v", err)
		}

		ctx = WithAllowedCWDRoots(WithClientCWD(context.Background(), outside), []string{allowed})
		if _, err := ResolveCWD(ctx, configDir); err == nil {
			t.Error("cwd outside every allowed root must be rejected")
		}
	})
}
