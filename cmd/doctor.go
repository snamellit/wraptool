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
	// detectWorkspace performs the upward `.git`/`manifest.scm` walk used when
	// no explicit --workspace is given (doctorDetectWorkspace in production).
	// Injected so a test can bound the walk to its own fixture tree instead of
	// the real host filesystem: a t.TempDir() is not guaranteed isolated from
	// an ancestor `.git`/`manifest.scm` (e.g. TMPDIR configured under a
	// git-managed $HOME), which would otherwise make the walk find a marker no
	// test created.
	detectWorkspace func(dir string) string
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
		load:            config.Load,
		goos:            runtime.GOOS,
		render:          doctor.RenderHuman,
		renderJSON:      doctor.RenderJSON,
		getwd:           os.Getwd,
		detectWorkspace: doctorDetectWorkspace,
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
		verifyPool   bool
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

--only/--skip accept a comma-separated list of check IDs or group names
(e.g. "--only env" or "--only env,config"); --skip wins over --only. A group
name must match one of the report's section names exactly (e.g. "config",
not "conf") — it is not a prefix.`,
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
				jsonOut:    jsonOut,
				only:       only,
				skip:       skip,
				strict:     strict,
				noProbe:    noProbe,
				verifyPool: verifyPool,
				workspace:  workspaceDir,
			})
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to config file (default: "+paths.DefaultConfigFile()+")")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the report as JSON (see design/doctor/wraptool-doctor.md §9); "+
		"check IDs are a stability/compatibility surface, an ID's severity may still change between releases")
	cmd.Flags().StringSliceVar(&only, "only", nil, "run only these check IDs or exact group names (comma-separated, e.g. \"env\" or \"env,config\")")
	cmd.Flags().StringSliceVar(&skip, "skip", nil, "skip these check IDs or exact group names (comma-separated); wins over --only")
	cmd.Flags().BoolVar(&strict, "strict", false, "promote an unsuppressed warn finding to exit code 1; NOT a stable CI gate "+
		"(a new check can ship as warn in any release) — gate CI on --only/--json instead, see §9.1")
	cmd.Flags().BoolVar(&noProbe, "no-probe", false, "disable inert local execution of configured tool binaries (help/version probing)")
	cmd.Flags().BoolVar(&verifyPool, "verify-pool", false, "additionally hash every file in the active harness generation and verify it against its receipt (slow: a node harness is 10k+ files)")
	cmd.Flags().StringVar(&workspaceDir, "workspace", "", "workspace root for workspace-gated checks "+
		"(default: walk upward from the current directory for .git or manifest.scm)")

	return cmd
}

// doctorRunFlags bundles A2's selection/output flags so runDoctor's signature
// stays a fixed arity as more land in later steps.
type doctorRunFlags struct {
	jsonOut    bool
	only       []string
	skip       []string
	strict     bool
	noProbe    bool
	verifyPool bool
	workspace  string
}

// runDoctor never calls config.EnsureConfigFile: serve and up scaffold a
// starter config, doctor never mutates anything on disk.
func runDoctor(cmd *cobra.Command, deps doctorDeps, path string, flags doctorRunFlags) error {
	path = expandTilde(path)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	env := deps.env()

	workspace, wsErr := doctorResolveWorkspace(deps, flags.workspace)
	if wsErr != nil {
		// An explicit --workspace that does not exist is a usage error, not a
		// finding: silently falling back to "no workspace" would satisfy
		// NeedsWorkspace against nothing and let a workspace-gated check (e.g.
		// env.no-backend-with-project) run against a directory that was never
		// actually checked, producing a false finding instead of telling the
		// operator their flag was wrong.
		return &ExitError{Code: 2, Err: wsErr}
	}

	in := &doctor.Input{
		Env:        env,
		Paths:      deps.paths(),
		GOOS:       deps.goos,
		ConfigPath: path,
		Workspace:  workspace,
		Probe:      !flags.noProbe,
		VerifyPool: flags.verifyPool,
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

	// C1: resolve every finding's Path to a real file:line by parsing the
	// same bytes just read above. NewLocator degrades to a no-op Locator
	// (never a panic, never a guessed line) when ConfigRaw is nil/unreadable/
	// unparseable, so this is safe to wire unconditionally on every path
	// above, including ConfigAbsent/ConfigUnreadable.
	report := doctor.Run(cmd.Context(), in, doctor.RunOptions{
		Checks:  deps.checks,
		Only:    flags.only,
		Skip:    flags.skip,
		Strict:  flags.strict,
		Locator: doctor.NewLocator(path, in.ConfigRaw),
	})

	// C2: mark every warn finding a `# doctor:ignore <id>` config comment
	// suppresses (design §6.2), against the SAME ConfigRaw bytes just parsed
	// for the Locator above. Safe to call unconditionally, including on the
	// ConfigAbsent/ConfigUnreadable paths — nil/unparseable raw is a no-op.
	report = doctor.ApplySuppression(report, in.ConfigRaw)

	render := deps.render
	if flags.jsonOut {
		render = deps.renderJSON
		if render == nil {
			render = doctor.RenderJSON // no-panic fallback; every production doctorDeps sets this already
		}
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

// doctorResolveWorkspace returns explicit's value, resolved to an absolute
// path and STAT'd, when set; else it walks upward from deps.getwd() for a
// project-root marker (deps.detectWorkspace). An explicit path that does not
// exist, or is not a directory, is a usage error naming the path — see the
// caller in runDoctor for why silently treating it as "no workspace" is
// wrong. A getwd failure or no marker found during auto-detection still
// yields ("", nil): the engine already handles that case, skipping
// workspace-gated checks with a canned reason (NeedsWorkspace, design
// §5/§10).
func doctorResolveWorkspace(deps doctorDeps, explicit string) (string, error) {
	if explicit != "" {
		abs := explicit
		if a, err := filepath.Abs(explicit); err == nil {
			abs = a
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("--workspace %s: %w", explicit, err)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("--workspace %s: not a directory", explicit)
		}
		return abs, nil
	}

	getwd := deps.getwd
	if getwd == nil {
		getwd = os.Getwd // no-panic fallback: every production doctorDeps sets this already
	}
	wd, err := getwd()
	if err != nil {
		return "", nil
	}

	detect := deps.detectWorkspace
	if detect == nil {
		detect = doctorDetectWorkspace // no-panic fallback: every production doctorDeps sets this already
	}
	return detect(wd), nil
}

// doctorDetectWorkspace walks upward from dir for a `.git` or `manifest.scm`
// marker using only os.Stat, never a subprocess: `up` derives its root via
// `exec.Command("git", "rev-parse", "--show-toplevel")` (cmd/up.go's
// gitToplevel), but doctor may not shell out at all (design §5). Returns ""
// when no marker is found before reaching the filesystem root.
func doctorDetectWorkspace(dir string) string {
	return doctorDetectWorkspaceWith(dir, fileExists)
}

// doctorDetectWorkspaceWith is doctorDetectWorkspace with an injectable
// existence probe, so a test can bound the walk to its own fixture tree
// instead of the real host filesystem — see doctorDeps.detectWorkspace's doc
// comment for why that matters.
func doctorDetectWorkspaceWith(dir string, exists func(string) bool) string {
	dir = filepath.Clean(dir)
	for {
		if exists(filepath.Join(dir, ".git")) || exists(filepath.Join(dir, "manifest.scm")) {
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
