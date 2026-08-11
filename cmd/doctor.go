package cmd

import (
	"errors"
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
	env        func() doctor.Env
	paths      func() doctor.HostPaths
	load       func(path string) (*config.Config, error)
	goos       string
	render     func(io.Writer, doctor.Report) error
	renderJSON func(io.Writer, doctor.Report) error
	// getwd resolves the starting directory for --workspace auto-detection
	// (doctorDetectWorkspace). Injected, like everything else here, so a cmd
	// test is never at the mercy of the actual process cwd.
	getwd func() (string, error)
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
// a finding, not step A1's), and the human/JSON renderers.
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
		load:       config.Load,
		goos:       runtime.GOOS,
		render:     doctor.RenderHuman,
		renderJSON: doctor.RenderJSON,
		getwd:      os.Getwd,
	}
}

func newDoctorCmd() *cobra.Command { return newDoctorCmdWithDeps(defaultDoctorDeps()) }

func newDoctorCmdWithDeps(deps doctorDeps) *cobra.Command {
	var (
		configPath   string
		jsonOut      bool
		only         []string
		skip         []string
		strict       bool
		noProbe      bool
		workspaceDir string
	)

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
  0  no fail findings (warn may be present, unless --strict)
  1  at least one fail finding (or, under --strict, an unsuppressed warn)
  2  doctor could not complete (e.g. an unparseable config file)

--config resolves like 'wraptool config validate': the flag, then
$WRAPTOOL_CONFIG, then the default config location.

--only/--skip accept a comma-separated list of check IDs or group prefixes
(e.g. "--only env" or "--only env,config"); --skip wins over --only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := configPath
			if path == "" {
				path = os.Getenv("WRAPTOOL_CONFIG")
			}
			if path == "" {
				path = paths.DefaultConfigFile()
			}
			return runDoctor(cmd, deps, path, doctorRunFlags{
				jsonOut:   jsonOut,
				only:      only,
				skip:      skip,
				strict:    strict,
				noProbe:   noProbe,
				workspace: workspaceDir,
			})
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to config file (default: "+paths.DefaultConfigFile()+")")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the report as JSON (see design/doctor/wraptool-doctor.md §9); "+
		"check IDs are a stability/compatibility surface, an ID's severity may still change between releases")
	cmd.Flags().StringSliceVar(&only, "only", nil, "run only these check IDs or group prefixes (comma-separated, e.g. \"env\" or \"env,config\")")
	cmd.Flags().StringSliceVar(&skip, "skip", nil, "skip these check IDs or group prefixes (comma-separated); wins over --only")
	cmd.Flags().BoolVar(&strict, "strict", false, "promote an unsuppressed warn finding to exit code 1; NOT a stable CI gate "+
		"(a new check can ship as warn in any release) — gate CI on --only/--json instead, see §9.1")
	cmd.Flags().BoolVar(&noProbe, "no-probe", false, "disable inert local execution of configured tool binaries (help/version probing)")
	cmd.Flags().StringVar(&workspaceDir, "workspace", "", "workspace root for workspace-gated checks "+
		"(default: walk upward from the current directory for .git or manifest.scm)")

	return cmd
}

// doctorRunFlags bundles A2's selection/output flags so runDoctor's signature
// stays a fixed arity as more land in later steps.
type doctorRunFlags struct {
	jsonOut   bool
	only      []string
	skip      []string
	strict    bool
	noProbe   bool
	workspace string
}

// runDoctor never calls config.EnsureConfigFile: serve and up scaffold a
// starter config, doctor never mutates anything on disk.
func runDoctor(cmd *cobra.Command, deps doctorDeps, path string, flags doctorRunFlags) error {
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
		Workspace:  doctorResolveWorkspace(deps, flags.workspace),
		Probe:      !flags.noProbe,
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

	report := doctor.Run(cmd.Context(), in, doctor.RunOptions{
		Checks: deps.checks,
		Only:   flags.only,
		Skip:   flags.skip,
		Strict: flags.strict,
	})

	render := deps.render
	if flags.jsonOut {
		render = deps.renderJSON
	}

	// Render BEFORE deciding the exit code, and on every path including
	// exit-2: §10.1 requires the env.* group to be emitted even when the
	// config itself could not be parsed.
	if err := render(cmd.OutOrStdout(), report); err != nil {
		return err
	}

	if code := report.ExitCode(); code != 0 {
		return &ExitError{Code: code, Err: incompleteCause(report)}
	}
	return nil
}

// doctorResolveWorkspace returns explicit's value verbatim when set, else
// walks upward from deps.getwd() for a project-root marker
// (doctorDetectWorkspace). A getwd failure or no marker found yields "", which
// the engine already handles: workspace-gated checks are skipped with a
// canned reason (NeedsWorkspace, design §5/§10).
func doctorResolveWorkspace(deps doctorDeps, explicit string) string {
	if explicit != "" {
		if abs, err := filepath.Abs(explicit); err == nil {
			return abs
		}
		return explicit
	}
	wd, err := deps.getwd()
	if err != nil {
		return ""
	}
	return doctorDetectWorkspace(wd)
}

// doctorDetectWorkspace walks upward from dir for a `.git` or `manifest.scm`
// marker using only os.Stat, never a subprocess: `up` derives its root via
// `exec.Command("git", "rev-parse", "--show-toplevel")` (cmd/up.go's
// gitToplevel), but doctor may not shell out at all (design §5). Returns ""
// when no marker is found before reaching the filesystem root.
func doctorDetectWorkspace(dir string) string {
	dir = filepath.Clean(dir)
	for {
		if fileExists(filepath.Join(dir, ".git")) || fileExists(filepath.Join(dir, "manifest.scm")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// incompleteCause is the *ExitError.Err a doctor exit carries: nil for exit 1
// (the rendered report already said everything there is to say about the
// fail findings — main.go prints nothing extra), the incomplete reason for
// exit 2 (so main.go's stderr line names the actual cause).
func incompleteCause(r doctor.Report) error {
	if r.Incomplete {
		return errors.New(r.IncompleteReason)
	}
	return nil
}
