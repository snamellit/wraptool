package introspect

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/executor"
)

// TestRunProbe_UsesChildEnv covers Ruling 4 (the architectural decision that
// replaced item 3's original env-parameter design): RunProbe has no env
// parameter to choose wrong — it always runs the child under
// executor.ChildEnv(tc.Binary, nil), so ambient state from whoever is
// calling it (an operator's interactive shell, or wraptool's own service
// environment) never reaches the probed tool, while PATH/HOME still do,
// exactly as executor.Run's own child processes get.
func TestRunProbe_UsesChildEnv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "envprobe")
	script := "#!/bin/sh\necho \"marker=$SECRET_MARKER\"\necho \"home=$HOME\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := &config.ToolConfig{Binary: bin}

	t.Setenv("SECRET_MARKER", "leaked-if-inherited")

	got, err := RunProbe(context.Background(), tc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "marker=\nhome="+os.Getenv("HOME")+"\n" {
		t.Errorf("RunProbe output = %q, want SECRET_MARKER absent (ChildEnv, not caller inheritance) and HOME passed through", got)
	}
}

// TestRunProbeEnv_ReplacesEnv is runProbeEnv's own test — the seam RunProbe
// itself no longer exposes a choice about (production always supplies
// executor.ChildEnv(tc.Binary, nil)), but the underlying exec mechanics
// still deserve a direct test: a non-nil env, empty or not, REPLACES the
// child's environment rather than letting it inherit the caller's.
func TestRunProbeEnv_ReplacesEnv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "envprobe")
	script := "#!/bin/sh\necho \"marker=$SECRET_MARKER\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := &config.ToolConfig{Binary: bin}

	t.Setenv("SECRET_MARKER", "leaked-if-inherited")

	// A non-empty, caller-supplied env replaces the child's environment
	// entirely — SECRET_MARKER must not reach it even though the CALLING
	// process (this test) has it set.
	injected, _, err := runProbeEnv(context.Background(), tc, nil, []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if injected != "marker=\n" {
		t.Errorf("with an injected env, got %q, want SECRET_MARKER absent (not inherited)", injected)
	}

	// An EMPTY-but-non-nil env is the strongest isolation a caller can ask
	// for (no environment at all) and must be honoured exactly — not
	// silently upgraded to "inherit everything" the way RunProbe's
	// predecessor did by gating on len(env) > 0.
	empty, _, err := runProbeEnv(context.Background(), tc, nil, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if empty != "marker=\n" {
		t.Errorf("with env == []string{} (empty, non-nil), got %q, want SECRET_MARKER absent — nil and empty are NOT the same request", empty)
	}
}

// TestRunProbeEnv_ExitStatus covers Ruling 3: runProbeEnv surfaces the
// probe's actual exit status rather than swallowing it the moment any bytes
// appear on stdout/stderr.
func TestRunProbeEnv_ExitStatus(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "exitprobe")
	// Prints output AND exits non-zero — the shape that let a killed-or-
	// failing catalog probe be recorded as if it had cleanly produced a
	// complete listing.
	script := "#!/bin/sh\necho \"partial output\"\nexit 3\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := &config.ToolConfig{Binary: bin}

	output, result, err := runProbeEnv(context.Background(), tc, nil, []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatalf("runProbeEnv error = %v, want nil (output was produced)", err)
	}
	if output != "partial output\n" {
		t.Errorf("output = %q", output)
	}
	if !result.Exited {
		t.Error("Exited should be true — the process ran to completion and returned a real exit code")
	}
	if result.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", result.ExitCode)
	}
	if result.DeadlineExceeded {
		t.Error("DeadlineExceeded should be false — nothing here hit the 10s timeout")
	}
}

// TestRunProbeEnv_DeadlineKill covers the other exit-status case Ruling 3
// names explicitly: a probe killed by ITS OWN context deadline must be
// distinguishable from one that exited cleanly, even though both can leave
// partial output behind.
func TestRunProbeEnv_DeadlineKill(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "hangprobe")
	// A builtin-only busy wait, deliberately: an earlier version ran
	// `exec sleep 5`, which needs sleep to resolve on the PATH this test
	// hands the child -- and that PATH is a hardcoded /usr/bin:/bin, so the
	// test failed on a Guix host where coreutils lives in the store rather
	// than under an FHS prefix. echo and : are POSIX shell builtins, so the
	// fixture now depends on nothing but the shell itself.
	//
	// It also hangs in the shell process rather than a forked child, so
	// SIGKILL from the context deadline ends the probe promptly instead of
	// leaving this test blocked on an orphan still holding the output pipe.
	script := "#!/bin/sh\necho \"partial output\"\nwhile :; do :; done\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := &config.ToolConfig{Binary: bin}

	// runProbeEnv applies its own fixed 10s timeout internally; give the
	// caller's context a much shorter deadline so this test doesn't take
	// 10s to prove the point.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	output, result, err := runProbeEnv(ctx, tc, nil, []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatalf("runProbeEnv error = %v, want nil (partial output was produced before the kill)", err)
	}
	if output != "partial output\n" {
		t.Errorf("output = %q, want only the pre-sleep line", output)
	}
	if !result.DeadlineExceeded {
		t.Error("DeadlineExceeded should be true — the caller's own context deadline killed the process")
	}
}

func TestProbeVersion(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "versionprobe")
	script := "#!/bin/sh\necho \"tool version 1.2.3\"\necho \"extra line\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := &config.ToolConfig{Binary: bin}

	got := ProbeVersion(context.Background(), tc)
	if got != "tool version 1.2.3" {
		t.Errorf("ProbeVersion = %q, want first line only", got)
	}
}

func TestProbeVersion_MissingBinary(t *testing.T) {
	tc := &config.ToolConfig{Binary: filepath.Join(t.TempDir(), "does-not-exist")}
	if got := ProbeVersion(context.Background(), tc); got != "" {
		t.Errorf("ProbeVersion for a missing binary = %q, want empty", got)
	}
}

// TestRunProbe_MatchesExecutorChildEnv is a light guard against RunProbe and
// executor.ChildEnv silently drifting apart: both must agree on PATH's
// shape (the binary's own directory prepended to the inherited PATH).
func TestRunProbe_MatchesExecutorChildEnv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "pathprobe")
	script := "#!/bin/sh\necho \"path=$PATH\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := &config.ToolConfig{Binary: bin}

	got, err := RunProbe(context.Background(), tc, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "path=" + executor.ChildEnv(bin, nil)[0][len("PATH="):] + "\n"
	if got != want {
		t.Errorf("RunProbe PATH = %q, want %q (executor.ChildEnv's own construction)", got, want)
	}
}
