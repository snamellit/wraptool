package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
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

func fakeDoctorDeps(load func(string) (*config.Config, error), checks []doctor.Check) doctorDeps {
	return doctorDeps{
		env:    func() doctor.Env { return doctor.NewMapEnv() },
		paths:  func() doctor.HostPaths { return doctor.HostPaths{} },
		load:   load,
		goos:   "linux",
		render: doctor.RenderHuman,
		checks: checks,
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
