package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

// config.parse and config.validate go through the sanctioned config.LoadStrict/
// Validate escape hatch (host.go's Env doc comment, plan §1 rule 10), which
// stats the config file and — for config.validate's passing case — the tool
// binary directly against the real filesystem. Every test here therefore
// builds a t.TempDir() fixture tree instead of a MapEnv, per that rule.

// configLoadWriteFile writes content to name inside dir and returns the
// absolute path, failing the test on error.
func configLoadWriteFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	return path
}

// configLoadTestInput is this test file's own Input builder. config_load.go's
// checks read only in.ConfigPath directly — Needs gating on ConfigState/
// Config happens in the engine (doctor.Run), never inside the RunFunc — but
// this mirrors what NeedsConfig requires the engine to have already provided
// (Meta docs in config_load.go) so a test reads like a real invocation.
func configLoadTestInput(configPath string) *Input {
	return &Input{
		Env:         NewMapEnv(),
		ConfigPath:  configPath,
		ConfigState: ConfigLoaded,
		Config:      &config.Config{},
	}
}

func TestRunConfigLoadParse_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	bin := configLoadWriteFile(t, dir, "tool", "#!/bin/sh\n")
	cfgPath := configLoadWriteFile(t, dir, "config.yaml", `
tools:
  git:
    binary: `+bin+`
    allow:
      - subcommand: [pull]
`)
	in := configLoadTestInput(cfgPath)

	res := runConfigLoadParse(context.Background(), in)

	if res.Outcome() != OutcomePass {
		t.Fatalf("Outcome() = %v, want pass; findings=%+v fatal=%v", res.Outcome(), res.Findings, res.Fatal)
	}
}

func TestRunConfigLoadParse_UnknownKeyAborts(t *testing.T) {
	dir := t.TempDir()
	cfgPath := configLoadWriteFile(t, dir, "config.yaml", `
mcp:
  transport: sse
bogus_top_level_key: true
`)
	in := configLoadTestInput(cfgPath)

	res := runConfigLoadParse(context.Background(), in)

	if res.Fatal == nil {
		t.Fatalf("Fatal = nil, want non-nil (config.parse aborts on strict-decode failure)")
	}
	if len(res.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1: %+v", len(res.Findings), res.Findings)
	}
	f := res.Findings[0]
	if f.Severity != SeverityFail {
		t.Errorf("Severity = %q, want fail", f.Severity)
	}
	if f.Remedy == "" {
		t.Error("Remedy is empty for a fail finding")
	}
	if !f.Path.Empty() {
		t.Errorf("Path = %v, want empty (this is not a config-content location, it's a decode failure)", f.Path)
	}
	if f.Location != "" {
		t.Errorf("Location = %q, want empty — the engine owns Location (plan §1 rule 9)", f.Location)
	}
	if !strings.Contains(f.Detail, "bogus_top_level_key") {
		t.Errorf("Detail = %q, want it to name the offending key", f.Detail)
	}
}

func TestRunConfigLoadParse_MissingFileIsSkippedNotAborted(t *testing.T) {
	dir := t.TempDir()
	// NeedsConfig gates this check on the engine's lenient load having
	// succeeded moments earlier at this exact path — simulate the path having
	// since vanished (a TOCTOU race) rather than never having existed.
	in := configLoadTestInput(filepath.Join(dir, "gone.yaml"))

	res := runConfigLoadParse(context.Background(), in)

	if res.Fatal != nil {
		t.Fatalf("Fatal = %v, want nil — a vanished file is not this check's decode-failure domain", res.Fatal)
	}
	if res.Outcome() != OutcomeSkipped {
		t.Fatalf("Outcome() = %v, want skipped", res.Outcome())
	}
	if len(res.Findings) != 0 {
		t.Errorf("Findings = %+v, want none", res.Findings)
	}
}

func TestRunConfigLoadValidate_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	bin := configLoadWriteFile(t, dir, "tool", "#!/bin/sh\n")
	cfgPath := configLoadWriteFile(t, dir, "config.yaml", `
tools:
  git:
    binary: `+bin+`
    allow:
      - subcommand: [pull]
`)
	in := configLoadTestInput(cfgPath)

	res := runConfigLoadValidate(context.Background(), in)

	if res.Outcome() != OutcomePass {
		t.Fatalf("Outcome() = %v, want pass; findings=%+v", res.Outcome(), res.Findings)
	}
}

func TestRunConfigLoadValidate_SemanticFailureIsNotAborted(t *testing.T) {
	dir := t.TempDir()
	// Decodes cleanly (LoadStrict succeeds: no unknown keys) but fails
	// Config.Validate's "no tools configured" rule — an empty document is a
	// zero Config, matching Load's io.EOF-as-empty-config behaviour.
	cfgPath := configLoadWriteFile(t, dir, "config.yaml", "")
	in := configLoadTestInput(cfgPath)

	res := runConfigLoadValidate(context.Background(), in)

	if res.Fatal != nil {
		t.Fatalf("Fatal = %v, want nil — config.validate reports fail, it does not abort (§10.1: exit 1, not 2)", res.Fatal)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1: %+v", len(res.Findings), res.Findings)
	}
	f := res.Findings[0]
	if f.Severity != SeverityFail {
		t.Errorf("Severity = %q, want fail", f.Severity)
	}
	if f.Remedy == "" {
		t.Error("Remedy is empty for a fail finding")
	}
	if !strings.Contains(f.Detail, "no tools configured") {
		t.Errorf("Detail = %q, want it to name Validate's own error", f.Detail)
	}
	if !strings.Contains(f.Detail, "stops at the first problem") {
		t.Errorf("Detail = %q, want it to note validation stops at the first problem (design §7.2)", f.Detail)
	}
}

func TestRunConfigLoadValidate_SkipsWhenStrictDecodeFails(t *testing.T) {
	dir := t.TempDir()
	cfgPath := configLoadWriteFile(t, dir, "config.yaml", `
bogus_top_level_key: true
`)
	in := configLoadTestInput(cfgPath)

	res := runConfigLoadValidate(context.Background(), in)

	if res.Outcome() != OutcomeSkipped {
		t.Fatalf("Outcome() = %v, want skipped — config.parse owns this failure, not config.validate", res.Outcome())
	}
	if len(res.Findings) != 0 {
		t.Errorf("Findings = %+v, want none", res.Findings)
	}
}

// The registration presence and Meta shape (non-empty Summary, known group,
// NeedsConfig) of both checks are already covered registry-WIDE by
// TestRegistryIsWellFormed and TestChecksSurviveAVirginHost/
// TestChecksAreDeterministic/TestFindingsAreWellFormed (invariants_test.go,
// via invariantsFullyPopulatedInput — its ConfigPath does not exist on the
// real host, which is exactly runConfigLoadParse's "file went away" Skipped
// branch, not the abort branch). Asserting them again here over the global
// Registered() is exactly what plan §1 rule 8 forbids for a check's OWN
// behaviour.
