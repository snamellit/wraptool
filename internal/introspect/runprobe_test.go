package introspect

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

// TestRunProbe_EnvInjection covers item 3: env, when non-empty, REPLACES the
// probe subprocess's environment rather than letting it inherit the caller's
// — the whole point being that doctor can hand a probe a minimal environment
// instead of an operator's credential-carrying shell.
func TestRunProbe_EnvInjection(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "envprobe")
	// Print exactly the value of SECRET_MARKER, whatever it is (empty if unset).
	script := "#!/bin/sh\necho \"marker=$SECRET_MARKER\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := &config.ToolConfig{Binary: bin}

	t.Setenv("SECRET_MARKER", "leaked-if-inherited")

	// env == nil: inherits the caller's environment (existing runTool
	// behaviour, unchanged).
	inherited, err := RunProbe(context.Background(), tc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if inherited != "marker=leaked-if-inherited\n" {
		t.Errorf("with env == nil, got %q, want the inherited SECRET_MARKER", inherited)
	}

	// A non-empty, caller-supplied env replaces the child's environment
	// entirely — SECRET_MARKER must not reach it even though the CALLING
	// process (this test) has it set.
	injected, err := RunProbe(context.Background(), tc, nil, []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if injected != "marker=\n" {
		t.Errorf("with an injected env, got %q, want SECRET_MARKER absent (not inherited)", injected)
	}
}

func TestProbeVersion_UsesInjectedEnv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "versionprobe")
	script := "#!/bin/sh\necho \"tool version 1.2.3 ($SOME_VAR)\"\necho \"extra line\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := &config.ToolConfig{Binary: bin}

	got := ProbeVersion(context.Background(), tc, []string{"PATH=/usr/bin:/bin", "SOME_VAR=x"})
	if got != "tool version 1.2.3 (x)" {
		t.Errorf("ProbeVersion = %q, want first line only with the injected env applied", got)
	}
}

func TestProbeVersion_MissingBinary(t *testing.T) {
	tc := &config.ToolConfig{Binary: filepath.Join(t.TempDir(), "does-not-exist")}
	if got := ProbeVersion(context.Background(), tc, nil); got != "" {
		t.Errorf("ProbeVersion for a missing binary = %q, want empty", got)
	}
}
