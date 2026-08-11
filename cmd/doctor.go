package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/doctor"
	"github.com/pti/wraptool/internal/paths"
	"github.com/pti/wraptool/internal/pool"

	"github.com/spf13/cobra"
)

// doctorDeps bundles `wraptool doctor`'s host-environment dependencies via
// constructor injection, mirroring cmd/harness.go's harnessDeps (documented
// there as chosen over package vars so cmd tests are t.Parallel()-safe): a
// cmd test builds its own doctorDeps with a fake Env/HostPaths/load and never
// touches process-global state.
type doctorDeps struct {
	env    func() doctor.Env
	paths  func() doctor.HostPaths
	load   func(path string) (*config.Config, error)
	goos   string
	render func(io.Writer, doctor.Report) error
	// checks overrides the registered check set passed to doctor.Run. Nil in
	// every production doctorDeps (defaultDoctorDeps leaves it unset, so
	// doctor.Run falls back to doctor.Registered() as normal). A cmd-level
	// test sets it to a synthetic doctor.Check list — built from a local type
	// implementing the doctor.Check interface, since doctor's own funcCheck is
	// unexported — so the exit-code matrix (fail=1 in particular) is testable
	// without a real fail-severity check needing to exist yet.
	checks []doctor.Check
}

// defaultDoctorDeps returns the real, production doctorDeps: the real OS Env
// (processAlive is cmd's own platform-split predicate, cmd/platform_unix.go /
// cmd/platform_windows.go), internal/paths resolved once, the lenient
// config.Load (never LoadStrict — strict decode is step B2's job, reported as
// a finding, not step A1's), and the human renderer.
func defaultDoctorDeps() doctorDeps {
	return doctorDeps{
		env: func() doctor.Env { return doctor.NewOSEnv(processAlive) },
		paths: func() doctor.HostPaths {
			return doctor.HostPaths{
				ConfigDir:  paths.ConfigDir(),
				DataDir:    paths.DataDir(),
				CacheDir:   paths.CacheDir(),
				RuntimeDir: paths.RuntimeDir(),
				PoolRoot:   pool.Root(),
			}
		},
		load:   config.Load,
		goos:   runtime.GOOS,
		render: doctor.RenderHuman,
	}
}

func newDoctorCmd() *cobra.Command { return newDoctorCmdWithDeps(defaultDoctorDeps()) }

func newDoctorCmdWithDeps(deps doctorDeps) *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run an operator-facing preflight report on this wraptool installation",
		Long: `wraptool doctor checks host dependencies, config validity, deployment
posture, and (in later releases) policy content and drift, and reports
exactly where the installation is weakest. It never edits config, never
starts or contacts the server, and never mutates the harness pool.

It is safe to run before any config has been created: a missing config is
reported as informational, not an error.

Exit codes:
  0  no fail findings
  1  at least one fail finding
  2  doctor could not complete (e.g. an unparseable config file)

--config resolves like 'wraptool config validate': the flag, then
$WRAPTOOL_CONFIG, then the default config location.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := configPath
			if path == "" {
				path = os.Getenv("WRAPTOOL_CONFIG")
			}
			if path == "" {
				path = paths.DefaultConfigFile()
			}
			return runDoctor(cmd, deps, path)
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to config file (default: "+paths.DefaultConfigFile()+")")

	return cmd
}

// runDoctor never calls config.EnsureConfigFile: serve and up scaffold a
// starter config, doctor never mutates anything on disk.
func runDoctor(cmd *cobra.Command, deps doctorDeps, path string) error {
	path = expandTilde(path)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	env := deps.env()

	in := &doctor.Input{
		Env:        env,
		Paths:      deps.paths(),
		GOOS:       deps.goos,
		ConfigPath: path,
	}

	cfg, loadErr := deps.load(path)
	switch {
	case loadErr == nil:
		in.ConfigState, in.Config = doctor.ConfigLoaded, cfg
		in.ConfigRaw, _ = env.ReadFile(path) // best-effort; C1 hardens this
	case errors.Is(loadErr, fs.ErrNotExist):
		// A missing config is INFO, not an error: doctor must be safe as the
		// first command a new user runs, before `serve`/`up` has scaffolded one.
		in.ConfigState, in.ConfigErr = doctor.ConfigAbsent, loadErr
	default:
		in.ConfigState, in.ConfigErr = doctor.ConfigUnreadable, loadErr
	}

	report := doctor.Run(cmd.Context(), in, doctor.RunOptions{Checks: deps.checks})

	// Render BEFORE deciding the exit code, and on every path including
	// exit-2: §10.1 requires the env.* group to be emitted even when the
	// config itself could not be parsed.
	if err := deps.render(cmd.OutOrStdout(), report); err != nil {
		return err
	}

	if code := report.ExitCode(); code != 0 {
		return &ExitError{Code: code, Err: incompleteCause(report)}
	}
	return nil
}

// incompleteCause is the *ExitError.Err a doctor exit carries: nil for exit 1
// (the rendered report already said everything there is to say about the
// fail findings — main.go prints nothing extra), the incomplete reason for
// exit 2 (so main.go's stderr line names the actual cause).
func incompleteCause(r doctor.Report) error {
	if r.Incomplete {
		return fmt.Errorf("%s", r.IncompleteReason)
	}
	return nil
}
