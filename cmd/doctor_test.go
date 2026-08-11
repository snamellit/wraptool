package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/doctor"
	"github.com/pti/wraptool/internal/paths"
)

// fakeDoctorCheck implements doctor.Check locally: doctor's own funcCheck is
// unexported, so a cmd-level test builds its own — the same technique any
// external package would use to feed doctor.RunOptions.Checks.
type fakeDoctorCheck struct {
	meta doctor.Meta
	run  func(context.Context, *doctor.Input) doctor.Result
}

func (c fakeDoctorCheck) Meta() doctor.Meta { return c.meta }

func (c fakeDoctorCheck) Run(ctx context.Context, in *doctor.Input) doctor.Result {
	return c.run(ctx, in)
}

// fakeDoctorHostPaths mirrors what cmd/doctor.go always populates: absolute
// roots, never the zero HostPaths. A check that resolves a root-parameterized
// sibling-package path from Input.Paths (pool.PoolPaths from PoolRoot, say)
// would otherwise resolve it RELATIVE to the test's working directory and read
// the real repository — silently, since the run still succeeds.
var fakeDoctorHostPaths = doctor.HostPaths{
	ConfigDir:  "/home/user/.config/wraptool",
	DataDir:    "/home/user/.local/share/wraptool",
	CacheDir:   "/home/user/.cache/wraptool",
	RuntimeDir: "/run/user/1000/wraptool",
	PoolRoot:   "/home/user/.local/share/wraptool/harness-pool",
}

func fakeDoctorDeps(load func(string) (*config.Config, error), checks []doctor.Check) doctorDeps {
	return doctorDeps{
		env:        func() doctor.Env { return doctor.NewMapEnv() },
		paths:      func() doctor.HostPaths { return fakeDoctorHostPaths },
		load:       load,
		goos:       "linux",
		render:     doctor.RenderHuman,
		renderJSON: doctor.RenderJSON,
		getwd:      func() (string, error) { return "/home/user/project", nil },
		checks:     checks,
	}
}

// runDoctorWithDeps executes the doctor command standalone (not through the
// root command, since production deps must never be touched by a test) and
// returns its rendered output plus the returned error.
func runDoctorWithDeps(t *testing.T, deps doctorDeps, args ...string) (string, error) {
	t.Helper()
	c := newDoctorCmdWithDeps(deps)
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs(args)
	err := c.Execute()
	return buf.String(), err
}

func TestDoctorCleanExitsZero(t *testing.T) {
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return &config.Config{}, nil }, []doctor.Check{})
	out, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml")
	if err != nil {
		t.Fatalf("expected exit 0, got err: %v", err)
	}
	if !strings.Contains(out, "0 failed") {
		t.Errorf("missing footer, got: %s", out)
	}
}

func TestDoctorFailFindingExitsOneWithNoExtraStderr(t *testing.T) {
	fail := fakeDoctorCheck{
		meta: doctor.Meta{ID: "policy.synthetic-fail", Needs: doctor.NeedsNothing, Summary: "synthetic test-only check"},
		run: func(context.Context, *doctor.Input) doctor.Result {
			return doctor.Found(doctor.Finding{Severity: doctor.SeverityFail, Title: "t", Detail: "d", Remedy: "fix it"})
		},
	}
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return &config.Config{}, nil }, []doctor.Check{fail})
	out, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml")
	if err == nil {
		t.Fatal("expected a non-nil error for exit 1")
	}
	if code := ExitCode(err); code != 1 {
		t.Errorf("ExitCode = %d, want 1", code)
	}
	// The exit-1 *ExitError carries a nil Err: main.go prints err.Error() only
	// when non-empty, and the rendered report already said everything there
	// is to say about the fail finding.
	if err.Error() != "" {
		t.Errorf("exit-1 error must carry no message, got %q", err.Error())
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("missing FAIL finding in rendered output: %s", out)
	}
}

func TestDoctorUnreadableConfigExitsTwo(t *testing.T) {
	loadErr := fmt.Errorf("parsing config: %w", errors.New("yaml: bad indent"))
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return nil, loadErr }, []doctor.Check{})
	out, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml")
	if err == nil {
		t.Fatal("expected a non-nil error for exit 2")
	}
	if code := ExitCode(err); code != 2 {
		t.Errorf("ExitCode = %d, want 2", code)
	}
	if !strings.Contains(out, "unreadable") {
		t.Errorf("missing unreadable-config header: %s", out)
	}
	if !strings.Contains(out, "could not complete") {
		t.Errorf("missing incomplete banner: %s", out)
	}
}

// TestDoctorMissingConfigExitsZero is the explicit acceptance behaviour: a
// missing config is informational, never an error — doctor must be safe as
// the very first command a new user runs.
func TestDoctorMissingConfigExitsZero(t *testing.T) {
	notExist := fmt.Errorf("reading config: %w", fs.ErrNotExist)
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return nil, notExist }, []doctor.Check{})
	out, err := runDoctorWithDeps(t, deps, "--config", "/nope/config.yaml")
	if err != nil {
		t.Fatalf("expected exit 0 for a missing config, got err: %v", err)
	}
	if !strings.Contains(out, "no config file yet") {
		t.Errorf("missing absent-config header: %s", out)
	}
	if !strings.Contains(out, "info  no config yet") {
		t.Errorf("missing absent-config info line: %s", out)
	}
}

func TestDoctorConfigPathPrecedence(t *testing.T) {
	var gotPath string
	load := func(p string) (*config.Config, error) {
		gotPath = p
		return &config.Config{}, nil
	}
	deps := fakeDoctorDeps(load, []doctor.Check{})

	t.Run("default", func(t *testing.T) {
		gotPath = ""
		// WRAPTOOL_CONFIG is a documented, supported user env var — clear it so
		// this subtest asserts flag-absent, env-absent behaviour instead of
		// whatever the ambient environment (a developer machine, a CI runner
		// that exports it) happens to hold.
		t.Setenv("WRAPTOOL_CONFIG", "")
		if _, err := runDoctorWithDeps(t, deps); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotPath != paths.DefaultConfigFile() {
			t.Errorf("got %q, want the default config file %q", gotPath, paths.DefaultConfigFile())
		}
	})

	t.Run("env var", func(t *testing.T) {
		gotPath = ""
		t.Setenv("WRAPTOOL_CONFIG", "/env/path/config.yaml")
		if _, err := runDoctorWithDeps(t, deps); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotPath != "/env/path/config.yaml" {
			t.Errorf("got %q, want the WRAPTOOL_CONFIG value", gotPath)
		}
	})

	t.Run("flag wins over env var", func(t *testing.T) {
		gotPath = ""
		t.Setenv("WRAPTOOL_CONFIG", "/env/path/config.yaml")
		if _, err := runDoctorWithDeps(t, deps, "--config", "/flag/path/config.yaml"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotPath != "/flag/path/config.yaml" {
			t.Errorf("got %q, want the --config value", gotPath)
		}
	})
}

func TestDoctorNeverMentionsStdioTransport(t *testing.T) {
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return &config.Config{}, nil }, []doctor.Check{})
	out, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "stdio") {
		t.Errorf("doctor output must never name the stdio MCP transport, got: %s", out)
	}
}

// TestDoctorProductionWiringRunsRealRegistry leaves doctorDeps.checks nil, so
// doctor.Run falls back to the real doctor.Registered() — every other cmd-level
// test injects an explicit (possibly empty) check slice and therefore never
// exercises the production wiring at all (acceptance criterion #2: "wraptool
// doctor prints one info line"). The Env is still MapEnv-backed (no PATH
// binaries populated), so the one shipped check's output is deterministic
// across CI runners.
func TestDoctorProductionWiringRunsRealRegistry(t *testing.T) {
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return &config.Config{}, nil }, nil)
	out, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "info  env.container-backend") {
		t.Errorf("missing the real env.container-backend line from the production check registry, got: %s", out)
	}
	if strings.Contains(out, "stdio") {
		t.Errorf("doctor output must never name the stdio MCP transport, got: %s", out)
	}
}

// -------------------------------------------------------------- A2 flags

func TestDoctorJSONFlagRendersJSONNotHuman(t *testing.T) {
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return &config.Config{}, nil }, []doctor.Check{})
	out, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml", "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var decoded map[string]any
	if jsonErr := json.Unmarshal([]byte(out), &decoded); jsonErr != nil {
		t.Fatalf("--json output did not decode as JSON: %v\noutput: %s", jsonErr, out)
	}
	if decoded["schema"] == nil {
		t.Errorf("missing schema field, got: %s", out)
	}
	if strings.Contains(out, "wraptool doctor —") {
		t.Errorf("--json output must not contain the human header, got: %s", out)
	}
}

func TestDoctorOnlyAndSkipFlagsSelectChecks(t *testing.T) {
	checks := []doctor.Check{
		fakeDoctorCheck{meta: doctor.Meta{ID: "env.a", Needs: doctor.NeedsNothing, Summary: "s"},
			run: func(context.Context, *doctor.Input) doctor.Result { return doctor.Pass() }},
		fakeDoctorCheck{meta: doctor.Meta{ID: "env.b", Needs: doctor.NeedsNothing, Summary: "s"},
			run: func(context.Context, *doctor.Input) doctor.Result { return doctor.Pass() }},
		fakeDoctorCheck{meta: doctor.Meta{ID: "tools.c", Needs: doctor.NeedsNothing, Summary: "s"},
			run: func(context.Context, *doctor.Input) doctor.Result { return doctor.Pass() }},
	}
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return &config.Config{}, nil }, checks)

	out, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml", "--only", "env", "--skip", "env.a", "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var decoded struct {
		Summary struct {
			Pass int `json:"pass"`
		} `json:"summary"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &decoded); jsonErr != nil {
		t.Fatalf("decoding --json output: %v", jsonErr)
	}
	// --only env selects env.a/env.b; --skip env.a then wins, leaving env.b
	// as the only selected (and passing) check.
	if decoded.Summary.Pass != 1 {
		t.Errorf("pass = %d, want 1 (only env.b selected)", decoded.Summary.Pass)
	}
}

func TestDoctorSelectionUnknownTokenExitsTwo(t *testing.T) {
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return &config.Config{}, nil }, []doctor.Check{})
	out, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml", "--only", "no-such-group")
	if err == nil {
		t.Fatal("expected a non-nil error for an unknown --only selector")
	}
	if code := ExitCode(err); code != 2 {
		t.Errorf("ExitCode = %d, want 2", code)
	}
	if !strings.Contains(out, "could not complete") {
		t.Errorf("missing incomplete banner: %s", out)
	}
}

func TestDoctorStrictPromotesWarnToExitOne(t *testing.T) {
	warn := fakeDoctorCheck{
		meta: doctor.Meta{ID: "policy.synthetic-warn", Needs: doctor.NeedsNothing, Summary: "s"},
		run: func(context.Context, *doctor.Input) doctor.Result {
			return doctor.Found(doctor.Finding{Severity: doctor.SeverityWarn, Title: "t", Detail: "d", Remedy: "fix it"})
		},
	}
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return &config.Config{}, nil }, []doctor.Check{warn})

	// Without --strict: exit 0.
	if _, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml"); err != nil {
		t.Fatalf("expected exit 0 without --strict, got: %v", err)
	}
	// With --strict: exit 1.
	_, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml", "--strict")
	if err == nil {
		t.Fatal("expected a non-nil error under --strict with an unsuppressed warn")
	}
	if code := ExitCode(err); code != 1 {
		t.Errorf("ExitCode = %d, want 1", code)
	}
}

func TestDoctorNoProbeFlagGatesProbeRequiringChecks(t *testing.T) {
	probeCheck := fakeDoctorCheck{
		meta: doctor.Meta{ID: "drift.synthetic", Needs: doctor.NeedsProbe, Summary: "s"},
		run:  func(context.Context, *doctor.Input) doctor.Result { return doctor.Pass() },
	}
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return &config.Config{}, nil }, []doctor.Check{probeCheck})

	// Default (no --no-probe): the check runs and passes.
	out, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "1 passed") {
		t.Errorf("expected the probe-requiring check to run by default, got: %s", out)
	}

	// --no-probe: the check is skipped instead.
	out, err = runDoctorWithDeps(t, deps, "--config", "/some/path.yaml", "--no-probe")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "1 skipped") {
		t.Errorf("expected the probe-requiring check to be skipped under --no-probe, got: %s", out)
	}
}

func TestDoctorWorkspaceFlagExplicitOverridesAutoDetect(t *testing.T) {
	wsCheck := fakeDoctorCheck{
		meta: doctor.Meta{ID: "env.needs-workspace", Needs: doctor.NeedsWorkspace, Summary: "s"},
		run:  func(context.Context, *doctor.Input) doctor.Result { return doctor.Pass() },
	}
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return &config.Config{}, nil }, []doctor.Check{wsCheck})

	dir := t.TempDir()
	out, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml", "--workspace", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "1 passed") {
		t.Errorf("expected the workspace-gated check to run with an explicit --workspace, got: %s", out)
	}
}

func TestDoctorWorkspaceAutoDetectFindsGitRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	wsCheck := fakeDoctorCheck{
		meta: doctor.Meta{ID: "env.needs-workspace", Needs: doctor.NeedsWorkspace, Summary: "s"},
		run:  func(context.Context, *doctor.Input) doctor.Result { return doctor.Pass() },
	}
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return &config.Config{}, nil }, []doctor.Check{wsCheck})
	deps.getwd = func() (string, error) { return sub, nil }

	out, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "1 passed") {
		t.Errorf("expected the workspace-gated check to run once .git is found walking up from cwd, got: %s", out)
	}
}

func TestDoctorWorkspaceNoMarkerFoundSkipsWorkspaceGatedChecks(t *testing.T) {
	wsCheck := fakeDoctorCheck{
		meta: doctor.Meta{ID: "env.needs-workspace", Needs: doctor.NeedsWorkspace, Summary: "s"},
		run:  func(context.Context, *doctor.Input) doctor.Result { return doctor.Pass() },
	}
	deps := fakeDoctorDeps(func(string) (*config.Config, error) { return &config.Config{}, nil }, []doctor.Check{wsCheck})
	deps.getwd = func() (string, error) { return t.TempDir(), nil } // no .git/manifest.scm anywhere above a fresh temp dir

	out, err := runDoctorWithDeps(t, deps, "--config", "/some/path.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "1 skipped") {
		t.Errorf("expected the workspace-gated check to be skipped with no marker found, got: %s", out)
	}
}

func TestDoctorDetectWorkspaceWalksUpward(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.scm"), []byte("()"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := doctorDetectWorkspace(sub); got != root {
		t.Errorf("doctorDetectWorkspace(%q) = %q, want %q", sub, got, root)
	}
}

func TestDoctorDetectWorkspaceNoMarkerReturnsEmpty(t *testing.T) {
	if got := doctorDetectWorkspace(t.TempDir()); got != "" {
		t.Errorf("doctorDetectWorkspace with no marker = %q, want empty", got)
	}
}
