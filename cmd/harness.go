package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pti/wraptool/internal/harness"
	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
	"github.com/pti/wraptool/internal/pool/source"
	"github.com/pti/wraptool/internal/yamledit"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// harnessDeps bundles the harness commands' host-environment dependencies:
// pool paths, the legacy pool bin/ directory, the artifact-acquisition
// Source, and the resolve-phase container backend. Passed by constructor
// injection — newHarnessCmd threads one harnessDeps to every subcommand
// constructor, and each subcommand's RunE closure captures it — rather than
// package-level vars tests mutate globally. This makes the cmd tests safe
// to run with t.Parallel(): two tests building independent commands with
// independent fake deps never contend on shared state.
type harnessDeps struct {
	poolPaths      func() pool.PoolPaths
	poolBinDir     func() (poolDir, poolBin string, err error)
	newSource      func() (pool.Source, error)
	resolveBackend func() (backend.Backend, error)
}

// defaultHarnessDeps returns the real, production harnessDeps: the
// developer's actual pool (rooted at pool.Root()), the legacy pool bin/
// directory (ensureHarnessPool, cmd/guixcontainer.go), the real npm
// artifact-acquisition Source (internal/pool/source), and the guix
// resolve-phase backend.
func defaultHarnessDeps() harnessDeps {
	return harnessDeps{
		// poolPaths builds the pool.PoolPaths for the developer's real pool
		// (rooted at pool.Root()), mirroring internal/pool/layout.go's
		// accessors. Orchestrator functions themselves never call
		// pool.Root(); only this cmd wiring does.
		poolPaths: func() pool.PoolPaths {
			return pool.PoolPaths{
				Manifest:     pool.ManifestFile(),
				Lock:         pool.LockFile(),
				Observations: pool.ObservationsFile(),
				Audit:        pool.AuditFile(),
				AdvisoryLock: pool.AdvisoryLockFile(),
				// pool.PoolPaths.Cache must be the flat, content-addressed
				// cache root (cache/sha256) — see its doc comment.
				// pool.CacheDir() is the cache/ parent, one level too
				// shallow; Source.Fetch would silently treat every object
				// as a permanent cache miss.
				Cache:   pool.CacheSHA256Dir(),
				Staging: pool.StagingDir(),
				Runtime: pool.RuntimeDir(),
			}
		},
		// poolBinDir returns the legacy pool root and bin/ directory (where
		// a harness's own installer drops its binary, and where the
		// {prefix} template in a Descriptor.Install recipe expands to).
		poolBinDir: func() (string, string, error) {
			poolDir, poolBin, _, _, err := ensureHarnessPool()
			return poolDir, poolBin, err
		},
		// newSource constructs the harness pool's npm artifact-acquisition
		// Source (design/unified-harness-pool.md §11.3, pool.Source). The
		// concrete npm Source lives in internal/pool/source; Go's
		// structural typing binds it to pool.Source.
		newSource: func() (pool.Source, error) {
			// npmSourceWithoutRealize (below) is a stand-in for
			// impl/npm-realize's *source.NPM.Realize until that sibling,
			// parallel work package merges — see its doc comment.
			return npmSourceWithoutRealize{source.NewNPM("")}, nil // "" = the default npm registry
		},
		resolveBackend: defaultResolveBackend,
	}
}

// npmSourceWithoutRealize wraps internal/pool/source's NPM Source so this
// package's production wiring keeps satisfying the (now Realize-extended)
// pool.Source interface while impl/npm-realize — a sibling, parallel work
// package — has not yet landed *source.NPM's own Realize method. Its Realize
// fails loudly rather than pretending to work: sync/install/update's realize
// step (this work package's addition) is fully wired end-to-end against
// this Source, so once impl/npm-realize merges, deleting this adapter (and
// returning *source.NPM directly from newSource, as before) is the only
// change needed to make it real. Not exported: it exists purely as a build
// seam, never something a test or another package should construct.
type npmSourceWithoutRealize struct {
	*source.NPM
}

func (npmSourceWithoutRealize) Realize(context.Context, backend.Backend, string, string, *pool.LockHarness, pool.PlatformKey, string, string) (*pool.Receipt, error) {
	return nil, fmt.Errorf("realizing a generation from the npm source is not wired yet " +
		"(internal/pool/source.NPM.Realize; pending the impl/npm-realize work package)")
}

// defaultResolveBackend picks the Linux-container backend the npm resolve
// phase (§6.2/§8.1) runs in. Phase 1 wires guix only — the same off-guix
// pointer error `install` has always printed — OCI/--backend selection is
// Phase 3+ (design/unified-harness-pool.md §16).
func defaultResolveBackend() (backend.Backend, error) {
	if !hasGuix() {
		return nil, fmt.Errorf("`wraptool harness update`/`install` resolves the exact npm version " +
			"inside a guix container, but `guix` is not on PATH.\nOn a devcontainer host, provision " +
			"harnesses via the wraptool feature instead: add\n  " +
			`"forge.snamellit.com/pti/wraptool/wraptool:0": {"harness": "<name>"}` +
			"\nto your .devcontainer/devcontainer.json (see examples/devcontainer). `wraptool up` " +
			"shares the pool home so auth/skills persist across projects")
	}
	b, err := backend.Select("guix", backend.Options{GuixPackages: []string{"node"}})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// newHarnessCmd builds the `harness` command tree with the real, production
// dependencies. root.go calls this with no arguments; newHarnessCmdWithDeps
// is the constructor-injection seam tests use to supply fakes.
func newHarnessCmd() *cobra.Command {
	return newHarnessCmdWithDeps(defaultHarnessDeps())
}

func newHarnessCmdWithDeps(deps harnessDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "harness",
		Short: "Manage the developer harness pool (binaries shared into every project container)",
		Long: `The harness pool is a developer-scoped directory of AI-harness binaries and
version-lock state shared into every guix/devcontainer container, so a
harness is installed once across all projects (not per session). It lives at
$XDG_DATA_HOME/wraptool/harness-pool (manifest.yaml records developer intent
and update policy, harness-lock.json the exact resolved version, home/ the
shared harness HOME). This is the developer's toolbox — not the project
(manifest.scm) and not host credentials. See design/unified-harness-pool.md.`,
	}
	cmd.AddCommand(
		newHarnessListCmd(deps),
		newHarnessInstallCmd(deps),
		newHarnessRefreshCmd(deps),
		newHarnessUpdateCmd(deps),
		newHarnessSyncCmd(deps),
		newHarnessPolicyCmd(deps),
	)
	return cmd
}

// manifestLockableHarnessArgs resolves a `[harness]` optional-positional-arg
// cobra pattern for refresh/update: the one named harness (validated known,
// lockable, AND present in the manifest — an explicitly named harness with
// no manifest entry still errors), or every lockable harness the manifest
// currently names when no argument is given. Bare refresh/update therefore
// touches only harnesses someone actually asked the pool to manage, rather
// than every registry-known lockable harness — which would otherwise
// needlessly seed observations.json for packages nobody has installed.
func manifestLockableHarnessArgs(paths pool.PoolPaths, args []string) ([]string, error) {
	// Pure argument validation first, no filesystem access: an unknown or
	// non-lockable harness name must fail the same way regardless of
	// whether the pool manifest is even readable.
	//
	// The callers' cobra commands declare MaximumNArgs(1), but this helper
	// enforces its own contract too: without this guard, two args would fall
	// through to the bare-command branch and operate on every manifest-listed
	// harness — a typo'd space must never widen the target set.
	if len(args) > 1 {
		return nil, fmt.Errorf("expected at most one harness, got %d (%q)", len(args), args)
	}
	if len(args) == 1 {
		name := args[0]
		d, ok := harness.Registry[name]
		if !ok {
			return nil, fmt.Errorf("unknown harness %q; supported: %v", name, harness.Names())
		}
		if d.PoolPackage == "" {
			return nil, fmt.Errorf("%q is not pool-lockable yet (legacy install path); see `wraptool harness list`", name)
		}
		manifestNames, err := pool.ManifestHarnessNames(paths.Manifest)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(manifestNames, name) {
			return nil, fmt.Errorf("%q has no manifest entry; run `wraptool harness install %s` first", name, name)
		}
		return []string{name}, nil
	}

	manifestNames, err := pool.ManifestHarnessNames(paths.Manifest)
	if err != nil {
		return nil, err
	}
	inManifest := make(map[string]bool, len(manifestNames))
	for _, n := range manifestNames {
		inManifest[n] = true
	}
	var names []string
	for _, name := range harness.Names() {
		if harness.Registry[name].PoolPackage == "" {
			continue
		}
		if inManifest[name] {
			names = append(names, name)
		}
	}
	return names, nil
}

// lockHarnessArgs is manifestLockableHarnessArgs' analogue for sync: it
// iterates harnesses present in harness-lock.json instead of the manifest —
// sync realizes an existing lock, so a harness that has never been resolved
// has nothing to sync.
func lockHarnessArgs(paths pool.PoolPaths, args []string) ([]string, error) {
	// Pure argument validation first, no filesystem access — see
	// manifestLockableHarnessArgs' matching comments, including the >1 guard:
	// extra args must error, never silently widen to the whole lock.
	if len(args) > 1 {
		return nil, fmt.Errorf("expected at most one harness, got %d (%q)", len(args), args)
	}
	if len(args) == 1 {
		name := args[0]
		d, ok := harness.Registry[name]
		if !ok {
			return nil, fmt.Errorf("unknown harness %q; supported: %v", name, harness.Names())
		}
		if d.PoolPackage == "" {
			return nil, fmt.Errorf("%q is not pool-lockable yet (legacy install path); see `wraptool harness list`", name)
		}
		lockNames, err := pool.LockHarnessNames(paths.Lock)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(lockNames, name) {
			return nil, fmt.Errorf("no lock entry for harness %q; run `wraptool harness install %s` first", name, name)
		}
		return []string{name}, nil
	}

	lockNames, err := pool.LockHarnessNames(paths.Lock)
	if err != nil {
		return nil, err
	}
	inLock := make(map[string]bool, len(lockNames))
	for _, n := range lockNames {
		inLock[n] = true
	}
	var names []string
	for _, name := range harness.Names() {
		if harness.Registry[name].PoolPackage == "" {
			continue
		}
		if inLock[name] {
			names = append(names, name)
		}
	}
	return names, nil
}

// runPerHarness runs fn for every name, collecting failures instead of
// aborting at the first one: every harness gets a chance to run (a broken
// claude refresh must not block opencode's), and the command still exits
// non-zero if anything failed. fn is expected to print its own per-harness
// progress/success line to cmd.OutOrStdout(); runPerHarness itself prints
// only the FAILED line, to cmd.ErrOrStderr() (not stdout), so a caller
// capturing stdout doesn't see error noise mixed in and tests can assert on
// each stream independently.
//
// The returned error is a perHarnessFailure: its Error() is a one-line
// summary (the detailed messages were already streamed to stderr above, and
// cobra prints the returned error once more — repeating every joined message
// there would report each failure twice), while Unwrap exposes the full
// errors.Join so errors.Is/As still finds any underlying sentinel through
// the aggregate.
func runPerHarness(cmd *cobra.Command, names []string, fn func(name string) error) error {
	var errs []error
	for _, name := range names {
		if err := fn(name); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: FAILED: %v\n", name, err)
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &perHarnessFailure{failed: len(errs), total: len(names), joined: errors.Join(errs...)}
}

// perHarnessFailure aggregates per-harness errors whose details were already
// printed as they occurred (see runPerHarness).
type perHarnessFailure struct {
	failed, total int
	joined        error
}

func (e *perHarnessFailure) Error() string {
	return fmt.Sprintf("%d of %d harnesses failed (details above)", e.failed, e.total)
}

func (e *perHarnessFailure) Unwrap() error { return e.joined }

// --- list ---

func newHarnessListCmd(deps harnessDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List known harnesses and their pool lock/install status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, poolBin, err := deps.poolBinDir()
			if err != nil {
				return err
			}
			return runHarnessList(deps.poolPaths(), poolBin, time.Now().UTC(), cmd.OutOrStdout())
		},
	}
}

// runHarnessList renders `harness list`'s table (§10): HARNESS CONSTRAINT
// LOCKED ACTIVE LATEST ELIGIBLE QUARANTINED. ACTIVE reports the legacy pool
// bin/ install state (Phase 2's real activation-pointer state isn't wired
// yet). It reads the pool's manifest/lock/observations directly — no pool
// lock (list never mutates, §6.5) and no Source (pool.StatusHarness works
// from files already on disk). Factored out from the cobra RunE, taking its
// inputs as plain parameters, so it can be tested against a temp pool.
func runHarnessList(paths pool.PoolPaths, poolBin string, now time.Time, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "HARNESS\tCONSTRAINT\tLOCKED\tACTIVE\tLATEST\tELIGIBLE\tQUARANTINED\tREALIZED")
	for _, name := range harness.Names() {
		d := harness.Registry[name]

		active := "-"
		if d.IsCLI() {
			if fileExists(filepath.Join(poolBin, d.Bin)) {
				active = "yes"
			} else {
				active = "no"
			}
		}

		if d.PoolPackage == "" {
			fmt.Fprintf(w, "%s\t-\t-\t%s\t-\t-\tlegacy (not lockable)\t-\n", name, active)
			continue
		}

		status, err := pool.StatusHarness(paths, name, d.PoolPackage, now)
		if err != nil {
			return fmt.Errorf("harness %q: %w", name, err)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			name, dashIfEmpty(status.Constraint), dashIfEmpty(status.Locked), active,
			dashIfEmpty(status.Latest), dashIfEmpty(status.Eligible), renderQuarantined(status),
			renderRealized(status.Realized))
	}
	return w.Flush()
}

// renderRealized renders the REALIZED column: "-" when the harness has never
// been realized for any platform, otherwise every realized platform key
// joined with ",", in status.Realized's existing (sorted) order.
func renderRealized(realized []pool.PlatformKey) string {
	if len(realized) == 0 {
		return "-"
	}
	keys := make([]string, len(realized))
	for i, k := range realized {
		keys[i] = string(k)
	}
	return strings.Join(keys, ",")
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// renderQuarantined renders the QUARANTINED column: "-" when nothing is
// quarantined, "<version> (<remaining> left)" when age alone is holding a
// newer version back (§10's example: "2.1.224 (3d left)"), or "<version>
// (not eligible)" for the rare case a candidate is unconditionally
// ineligible (a missing publish timestamp) rather than merely aging out.
func renderQuarantined(status pool.HarnessStatus) string {
	if status.QuarantinedVersion == "" {
		return "-"
	}
	if !status.QuarantineOK {
		return status.QuarantinedVersion + " (not eligible)"
	}
	return fmt.Sprintf("%s (%s left)", status.QuarantinedVersion, formatRemaining(status.QuarantineRemaining))
}

// formatRemaining renders a duration to one whole unit, rounded up so "just
// under 3 days" reads as "3d" rather than the more misleading-in-either-
// direction "2d" — used both for QUARANTINED's countdown ("3d left") and for
// reporting an --allow-young candidate's age ("published 3d ago").
func formatRemaining(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		days := int(d / (24 * time.Hour))
		if d%(24*time.Hour) != 0 {
			days++
		}
		return fmt.Sprintf("%dd", days)
	case d >= time.Hour:
		hours := int(d / time.Hour)
		if d%time.Hour != 0 {
			hours++
		}
		return fmt.Sprintf("%dh", hours)
	default:
		minutes := int(d / time.Minute)
		if d%time.Minute != 0 {
			minutes++
		}
		if minutes == 0 {
			minutes = 1
		}
		return fmt.Sprintf("%dm", minutes)
	}
}

// --- install ---

func newHarnessInstallCmd(deps harnessDeps) *cobra.Command {
	var constraint string
	var allowYoung bool
	var reason string
	cmd := &cobra.Command{
		Use:   "install <harness>",
		Short: "Add pool intent for a harness, lock its version, and install it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			d, ok := harness.Registry[name]
			if !ok {
				return fmt.Errorf("unknown harness %q; supported: %v", name, harness.Names())
			}
			if !d.IsCLI() {
				return fmt.Errorf("%q is a host-side IDE/extension, not a container CLI; "+
					"configure it with `wraptool init --harness %s`", name, name)
			}
			if allowYoung && reason == "" {
				return fmt.Errorf("--allow-young requires --reason")
			}

			if d.PoolPackage == "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %q has no locked-pool adapter yet; installing unpinned (legacy path, §8)\n", name)
				paths := deps.poolPaths()
				release, err := pool.LockPool(context.Background(), paths)
				if err != nil {
					return err
				}
				defer release()
				return legacyInstall(cmd, deps, name, d)
			}
			return runHarnessInstall(context.Background(), cmd, deps, name, d, constraint, allowYoung, reason)
		},
	}
	cmd.Flags().StringVar(&constraint, "constraint", "", "version constraint to record in the pool manifest "+
		"(e.g. '>=2 <3' or '=2.1.220'); empty keeps any existing constraint, or the stable channel on first install")
	cmd.Flags().BoolVar(&allowYoung, "allow-young", false, "bypass the release/observation age quarantine for this install (requires --reason)")
	cmd.Flags().StringVar(&reason, "reason", "", "required with --allow-young; recorded in the pool audit log")
	return cmd
}

// runHarnessInstall implements the pool-lockable `install` flow (§10): write
// intent to the manifest, refresh candidate metadata, resolve+lock+fetch the
// newest eligible version (idempotent when everything already agrees), then
// run the harness's own installer pinned to that exact locked version
// (§16 Phase 1: legacy execution). The whole flow — constraint write,
// refresh, update, install — runs under ONE pool-lock acquisition (§6.5):
// Source/backend availability is probed BEFORE the lock is taken (and before
// any manifest mutation), so an off-guix host or an unwired Source fails
// with zero side effects.
func runHarnessInstall(ctx context.Context, cmd *cobra.Command, deps harnessDeps, name string, d harness.Descriptor, constraint string, allowYoung bool, reason string) error {
	paths := deps.poolPaths()

	src, err := deps.newSource()
	if err != nil {
		return err
	}
	b, err := deps.resolveBackend()
	if err != nil {
		return err
	}

	release, err := pool.LockPool(ctx, paths)
	if err != nil {
		return err
	}
	defer release()

	if err := setHarnessConstraintLocked(paths, name, constraint); err != nil {
		return fmt.Errorf("recording pool intent for %s: %w", name, err)
	}

	now := time.Now().UTC()
	if _, err := pool.RefreshHarnessLocked(ctx, src, paths, d.PoolPackage, now); err != nil {
		return fmt.Errorf("refreshing %s: %w", name, err)
	}

	result, err := pool.UpdateHarnessLocked(ctx, src, b, paths, pool.UpdateOptions{
		Harness: name, Package: d.PoolPackage, BinName: d.Bin, Now: now,
		AllowYoung: allowYoung, Reason: reason,
		// install never advances a floating constraint (§10) — it's
		// idempotent once manifest/lock/recipe agree, even if a newer
		// eligible version exists.
		Advance: false,
	})
	if err != nil {
		// UpdateHarnessLocked covers resolve+lock+fetch AND realize
		// (§16 Phase 2); a wrap naming just one phase ("locking %s") would
		// misreport a Realize failure — locking may well have already
		// succeeded. Its own per-phase errors ("resolving %s@%s", "fetching
		// %s@%s", "realizing %s for %s", "validating lock") are already
		// specific, so this wrap only adds the harness name's context, not
		// a (possibly wrong) phase claim.
		return fmt.Errorf("updating %s: %w", name, err)
	}
	printUpdateResult(cmd, name, result)
	if allowYoung && !result.AllowYoungApplied && !result.Changed {
		// install's no-advance short-circuit fires before the bypass is
		// consulted, so the flag can no-op invisibly on an existing lock.
		fmt.Fprintf(cmd.OutOrStdout(), "%s: --allow-young had no effect (already locked); use `wraptool harness update %s --allow-young` to advance\n", name, name)
	}

	return maybeInstallLocked(cmd, deps, name, d, result)
}

// printUpdateResult reports one harness's pool.UpdateResult to
// cmd.OutOrStdout(): an effective --allow-young bypass prints the
// candidate's version, age, and integrity (§9 minor: "prints candidate age
// and integrity" — not just the audit record); otherwise it reports whether
// a new version was locked, an informative kept-version note (MAJOR 1's
// "nothing eligible, kept the existing lock" case), or plain up-to-date.
//
// result.RealizedGenerationID (§16 Phase 2) is appended to whichever of
// those it reports, whenever set: DecisionRealize and a DecisionKeepExisting
// that still needed realizing both have Changed=false and (DecisionRealize)
// no Note, and would otherwise print the same "already locked ... (up to
// date)" text as a true no-op — hiding the fact that this call actually
// built or confirmed a generation. A caller must not read Changed=false as
// "nothing happened" (review finding 5).
func printUpdateResult(cmd *cobra.Command, name string, result *pool.UpdateResult) {
	out := cmd.OutOrStdout()

	var msg string
	realizedFolded := false // true once the case below has already worked RealizedGenerationID into msg
	switch {
	case result.AllowYoungApplied:
		msg = fmt.Sprintf("%s: --allow-young bypassed quarantine for %s (published %s ago)",
			name, result.Harness.Version, formatRemaining(result.CandidateAge))
		if result.CandidateIntegrity != "" {
			msg += fmt.Sprintf("; integrity %s", result.CandidateIntegrity)
		}
	case result.Changed:
		msg = fmt.Sprintf("%s: locked %s", name, result.Harness.Version)
	case result.Note != "":
		msg = fmt.Sprintf("%s: %s", name, result.Note)
	case result.RealizedGenerationID != "":
		// Changed=false and no Note: on the resolve axis this is a genuine
		// no-op, but RealizeGeneration DID run and build/confirm a
		// generation this call — "(up to date)" alone would misreport that
		// as nothing having happened (review finding 5).
		msg = fmt.Sprintf("%s: locked version %s unchanged; realized %s", name, result.Harness.Version, result.RealizedGenerationID)
		realizedFolded = true
	default:
		msg = fmt.Sprintf("%s: already locked at %s (up to date)", name, result.Harness.Version)
	}
	if result.RealizedGenerationID != "" && !realizedFolded {
		msg += fmt.Sprintf("; realized %s", result.RealizedGenerationID)
	}
	fmt.Fprintln(out, msg)
}

// maybeInstallLocked runs the harness's legacy installer pinned to
// result.Harness.Version, UNLESS the update/install was idempotent
// (!result.Changed) AND the pool binary is already present — in which case
// the npm run is skipped entirely (§10 minor: install's idempotent path is
// consistent with update's "continue", and printUpdateResult already
// reported the up-to-date/kept status, so nothing more is printed here).
// When the lock says a version is current but the binary is actually
// missing (e.g. a fresh workstation with a pre-existing lock), it still
// installs — "up to date" must not be a lie. Must be called with the pool
// lock already held (§6.5 MAJOR 3) — see legacyInstallVersionLocked.
//
// §16 Phase 1 item 4 ("import existing installations") ships here as an
// implicit reinstall, not a real import: this package never inspects an
// existing binary's provenance (it could be a prior unpinned/legacy
// install, or a hand-placed one — nothing proves it matches any lock).
// result.Changed with the binary already present means it's about to be
// silently overwritten by the freshly locked version, so that case gets one
// explicit line rather than passing in complete silence.
func maybeInstallLocked(cmd *cobra.Command, deps harnessDeps, name string, d harness.Descriptor, result *pool.UpdateResult) error {
	_, poolBin, err := deps.poolBinDir()
	if err != nil {
		return err
	}
	binExists := fileExists(filepath.Join(poolBin, d.Bin))

	if !result.Changed {
		if binExists {
			return nil
		}
	} else if binExists {
		fmt.Fprintf(cmd.OutOrStdout(), "%s: replacing existing pool install with locked %s (prior install's provenance unverified)\n", name, result.Harness.Version)
	}

	return legacyInstallVersionLocked(cmd, deps, name, d, result.Harness.Version)
}

// runInstallArgv executes one harness install recipe's argv (guix npm
// install, or agy's curl|bash) with the process's stdio wired through. A
// package var so tests can substitute a fake instead of actually invoking
// guix/npm/curl — e.g. to prove the pool lock is held for the whole
// constraint-write→refresh→update→install span (MAJOR 3) without running a
// real installer. Out of scope for the harnessDeps constructor-injection
// refactor (MR59 review): it gates on invoking an external process, not on
// per-command state, the same reasoning that keeps hasGuix and
// internal/pool/backend's host-probing vars as package vars too.
var runInstallArgv = func(argv []string) error {
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// legacyInstall is today's unpinned pool install (pre-dating this work
// package): it runs a harness's own installer with a floating version. It
// remains the only path for harnesses with no PoolPackage (§8: curl|bash
// recipes like agy's stay legacy until an artifact-specific adapter exists).
// Must be called with the pool lock already held (§6.5 MAJOR 3): it mutates
// the pool's bin/+lib/ directories, the same mutable pool state every other
// mutating command protects.
func legacyInstall(cmd *cobra.Command, deps harnessDeps, name string, d harness.Descriptor) error {
	poolDir, poolBin, err := deps.poolBinDir()
	if err != nil {
		return err
	}
	if len(d.Install) == 0 {
		return fmt.Errorf("no automated installer for %q; install it into %s yourself, "+
			"then `wraptool up --harness %s`", name, poolBin, name)
	}

	argv := make([]string, len(d.Install))
	for i, a := range d.Install {
		argv[i] = strings.ReplaceAll(a, "{prefix}", poolDir)
	}
	if argv[0] == "guix" && !hasGuix() {
		return offGuixInstallError(name)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "installing %s into %s ...\n  %s\n", name, poolBin, strings.Join(argv, " "))
	if err := runInstallArgv(argv); err != nil {
		return fmt.Errorf("installing %s: %w", name, err)
	}

	if bin := filepath.Join(poolBin, d.Bin); fileExists(bin) {
		fmt.Fprintf(cmd.OutOrStdout(), "installed %s -> %s\n", name, bin)
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %q not found after install (expected %s)\n", d.Bin, bin)
	}
	return nil
}

// legacyInstallVersionLocked runs a pool-lockable harness's own npm
// installer pinned to exactly version — Phase 1's legacy execution path
// (§16): resolve, lock, and fetch now go through the pool, but the actual
// install still shells out to npm, now pinned instead of floating "latest".
// Must be called with the pool lock already held (§6.5 MAJOR 3): the npm run
// mutates the pool's bin/+lib/ directories.
func legacyInstallVersionLocked(cmd *cobra.Command, deps harnessDeps, name string, d harness.Descriptor, version string) error {
	poolDir, poolBin, err := deps.poolBinDir()
	if err != nil {
		return err
	}
	if len(d.Install) == 0 {
		return fmt.Errorf("no automated installer for %q; install version %s into %s yourself", name, version, poolBin)
	}

	pinned, err := pinInstallVersion(d.Install, d.PoolPackage, version)
	if err != nil {
		return fmt.Errorf("pinning install recipe for %s: %w", name, err)
	}
	argv := make([]string, len(pinned))
	for i, a := range pinned {
		argv[i] = strings.ReplaceAll(a, "{prefix}", poolDir)
	}
	if argv[0] == "guix" && !hasGuix() {
		return offGuixInstallError(name)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "installing %s@%s into %s ...\n  %s\n", name, version, poolBin, strings.Join(argv, " "))
	if err := runInstallArgv(argv); err != nil {
		return fmt.Errorf("installing %s@%s: %w", name, version, err)
	}

	if bin := filepath.Join(poolBin, d.Bin); fileExists(bin) {
		fmt.Fprintf(cmd.OutOrStdout(), "installed %s -> %s\n", name, bin)
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %q not found after install (expected %s)\n", d.Bin, bin)
	}
	return nil
}

// pinInstallVersion returns a copy of a harness's install argv (built by
// internal/harness's npmInstall recipe) with its floating npm package
// argument pinned to pkg@version. It looks for pkg verbatim among argv's
// elements — the exact string npmInstall(pkg) embeds — and errors if it
// can't find it rather than silently leaving the recipe unpinned.
func pinInstallVersion(argv []string, pkg, version string) ([]string, error) {
	out := make([]string, len(argv))
	copy(out, argv)
	for i, a := range out {
		if a == pkg {
			out[i] = pkg + "@" + version
			return out, nil
		}
	}
	return nil, fmt.Errorf("could not find package %q in install recipe %v to pin version %s", pkg, argv, version)
}

// offGuixInstallError is the off-guix pointer error shared by every install
// path (legacy and pool-lockable): the pool installers run inside a guix
// container (FHS emulation dodges npm's detect-libc misread on bare guix).
// Off guix — a devcontainer host — the pool isn't the delivery path at all.
func offGuixInstallError(name string) error {
	return fmt.Errorf("`wraptool harness install` provisions the guix-container pool, "+
		"but `guix` is not on PATH.\nOn a devcontainer host, provision %q via the "+
		"wraptool feature instead: add\n  "+
		`"forge.snamellit.com/pti/wraptool/wraptool:0": {"harness": %q}`+
		"\nto your .devcontainer/devcontainer.json (see examples/devcontainer). "+
		"`wraptool up` shares the pool home so auth/skills persist across projects", name, name)
}

// --- refresh ---

func newHarnessRefreshCmd(deps harnessDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "refresh [harness]",
		Short: "Fetch candidate version metadata for one or every manifest-listed harness (metadata only, no download)",
		Long: `Fetches candidate version metadata from the registry and records it in
observations.json — no download, no lock change. With no harness argument,
refresh applies only to harnesses already listed in the pool manifest (i.e.
harnesses "wraptool harness install" has recorded intent for); a harness
that has never been installed has nothing to refresh. "install" runs its
own refresh internally as its first step, so a fresh install never needs a
separate "refresh" first — this command is for keeping already-installed
harnesses' candidate data current between installs (e.g. before "update").`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths := deps.poolPaths()
			names, err := manifestLockableHarnessArgs(paths, args)
			if err != nil {
				return err
			}
			src, err := deps.newSource()
			if err != nil {
				return err
			}

			release, err := pool.LockPool(context.Background(), paths)
			if err != nil {
				return err
			}
			defer release()

			now := time.Now().UTC()
			return runPerHarness(cmd, names, func(name string) error {
				d := harness.Registry[name]
				candidates, err := pool.RefreshHarnessLocked(context.Background(), src, paths, d.PoolPackage, now)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: observed %d candidate version(s)\n", name, len(candidates))
				return nil
			})
		},
	}
}

// --- update ---

func newHarnessUpdateCmd(deps harnessDeps) *cobra.Command {
	var allowYoung bool
	var reason string
	cmd := &cobra.Command{
		Use:   "update [harness]",
		Short: "Advance one or every manifest-listed harness's locked version to the newest eligible release satisfying its constraint",
		Long: `Chooses the newest release satisfying the harness's manifest constraint and
the pool's release/observation-age quarantine among already-refreshed
candidates, locks it, and installs it. Run "wraptool harness refresh" first
to pick up new candidate versions from the registry; update never fetches
metadata itself. With no harness argument, updates every harness currently
listed in the pool manifest.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if allowYoung && reason == "" {
				return fmt.Errorf("--allow-young requires --reason")
			}
			paths := deps.poolPaths()
			names, err := manifestLockableHarnessArgs(paths, args)
			if err != nil {
				return err
			}
			src, err := deps.newSource()
			if err != nil {
				return err
			}
			b, err := deps.resolveBackend()
			if err != nil {
				return err
			}

			release, err := pool.LockPool(context.Background(), paths)
			if err != nil {
				return err
			}
			defer release()

			now := time.Now().UTC()
			return runPerHarness(cmd, names, func(name string) error {
				d := harness.Registry[name]
				result, err := pool.UpdateHarnessLocked(context.Background(), src, b, paths, pool.UpdateOptions{
					Harness: name, Package: d.PoolPackage, BinName: d.Bin, Now: now,
					AllowYoung: allowYoung, Reason: reason,
					// update is the only normal command that advances a
					// floating constraint (§10): always scan for a newer
					// eligible candidate, even when the existing lock
					// already agrees with manifest+recipe.
					Advance: true,
				})
				if err != nil {
					return err
				}
				printUpdateResult(cmd, name, result)
				return maybeInstallLocked(cmd, deps, name, d, result)
			})
		},
	}
	cmd.Flags().BoolVar(&allowYoung, "allow-young", false, "bypass the release/observation age quarantine (requires --reason)")
	cmd.Flags().StringVar(&reason, "reason", "", "required with --allow-young; recorded in the pool audit log")
	return cmd
}

// --- sync ---

func newHarnessSyncCmd(deps harnessDeps) *cobra.Command {
	var platform string
	cmd := &cobra.Command{
		Use:   "sync [harness]",
		Short: "Realize the exact existing lock without resolving a new version",
		Long: `Fetches any cache objects missing for the version already recorded in
harness-lock.json, then realizes (§16 Phase 2) and installs exactly that
version — never resolving a new one. For a new workstation, a lost cache, or
a platform the lock already knows about but this pool hasn't realized yet
(--platform, default: this host's own platform). With no harness argument,
syncs every harness currently present in the lock.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// --platform validation is pure (no filesystem access) and runs
			// before lockHarnessArgs, which does touch the pool's lock file
			// — mirrors this file's "pure argument validation first"
			// convention (see manifestLockableHarnessArgs' doc comment).
			key, err := resolveSyncPlatform(platform)
			if err != nil {
				return err
			}
			paths := deps.poolPaths()
			names, err := lockHarnessArgs(paths, args)
			if err != nil {
				return err
			}
			src, err := deps.newSource()
			if err != nil {
				return err
			}
			b, err := deps.resolveBackend()
			if err != nil {
				return err
			}

			release, err := pool.LockPool(context.Background(), paths)
			if err != nil {
				return err
			}
			defer release()

			return runPerHarness(cmd, names, func(name string) error {
				d := harness.Registry[name]
				lh, err := pool.SyncHarnessLocked(context.Background(), src, b, paths, name, d.Bin, key)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: fetched cache objects for locked %s; realized for %s; installing ...\n", name, lh.Version, key)
				return legacyInstallVersionLocked(cmd, deps, name, d, lh.Version)
			})
		},
	}
	cmd.Flags().StringVar(&platform, "platform", "", "target platform key to realize (default: this host's own platform); "+
		"one of linux-amd64-glibc, linux-arm64-glibc, linux-amd64-musl, linux-arm64-musl")
	return cmd
}

// resolveSyncPlatform resolves `sync --platform`'s string to a validated
// pool.PlatformKey: empty defaults to HostPlatformKey() (the one helper
// §5.1 designates for "this host's own platform"); non-empty is validated
// against the closed enum via ParsePlatformKey, whose error already lists
// every valid value.
func resolveSyncPlatform(platform string) (pool.PlatformKey, error) {
	if platform == "" {
		return pool.HostPlatformKey()
	}
	return pool.ParsePlatformKey(platform)
}

// --- policy set ---

func newHarnessPolicyCmd(deps harnessDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Configure pool-wide or per-harness update policy (release-age quarantine, prereleases)",
	}
	cmd.AddCommand(newHarnessPolicySetCmd(deps))
	return cmd
}

func newHarnessPolicySetCmd(deps harnessDeps) *cobra.Command {
	var harnessName string
	var minReleaseAge, minObservationAge time.Duration
	var allowPrereleases bool
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set pool-wide (or, with --harness, per-harness) update policy",
		Long: `Edits manifest.yaml in place, preserving comments and key ordering. Any
inline comment on a field this command changes survives (e.g.
"minimum_release_age: 168h # seven days"), but its TEXT is not updated to
match the new value — after "policy set --minimum-release-age 24h" that
same line reads "minimum_release_age: 24h # seven days", now describing the
old 168h. Edit the comment by hand if it needs to match. Comment spacing is
also normalized to the YAML encoder's own single-space style regardless of
the original formatting.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if harnessName != "" {
				if _, ok := harness.Registry[harnessName]; !ok {
					return fmt.Errorf("unknown harness %q; supported: %v", harnessName, harness.Names())
				}
			}
			releaseSet := cmd.Flags().Changed("minimum-release-age")
			observationSet := cmd.Flags().Changed("minimum-observation-age")
			prereleaseSet := cmd.Flags().Changed("allow-prereleases")
			if !releaseSet && !observationSet && !prereleaseSet {
				return fmt.Errorf("specify at least one of --minimum-release-age, --minimum-observation-age, or --allow-prereleases")
			}
			if releaseSet && minReleaseAge < 0 {
				return fmt.Errorf("--minimum-release-age must not be negative, got %s", minReleaseAge)
			}
			if observationSet && minObservationAge < 0 {
				return fmt.Errorf("--minimum-observation-age must not be negative, got %s", minObservationAge)
			}

			paths := deps.poolPaths()
			release, err := pool.LockPool(context.Background(), paths)
			if err != nil {
				return err
			}
			defer release()

			err = editManifest(paths.Manifest, func(doc *yaml.Node) error {
				var target *yaml.Node
				if harnessName != "" {
					harnessesNode := yamledit.EnsureMapChild(doc, "harnesses")
					target = yamledit.EnsureMapChild(harnessesNode, harnessName)
				} else {
					target = yamledit.EnsureMapChild(doc, "policy")
				}
				if releaseSet {
					yamledit.MapSet(target, "minimum_release_age", yamledit.ScalarNode(minReleaseAge.String()))
				}
				if observationSet {
					yamledit.MapSet(target, "minimum_observation_age", yamledit.ScalarNode(minObservationAge.String()))
				}
				if prereleaseSet {
					yamledit.MapSet(target, "allow_prereleases", yamledit.ScalarNode(boolString(allowPrereleases)))
				}
				return nil
			})
			if err != nil {
				return err
			}

			if harnessName != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "updated policy for %s in %s\n", harnessName, paths.Manifest)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "updated pool-wide policy in %s\n", paths.Manifest)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&harnessName, "harness", "", "apply to one harness instead of the pool-wide default")
	cmd.Flags().DurationVar(&minReleaseAge, "minimum-release-age", 0, "minimum time since publish before a release is eligible (e.g. 168h); 0h disables the check")
	cmd.Flags().DurationVar(&minObservationAge, "minimum-observation-age", 0, "minimum local observation age before a release is eligible; 0h disables the check")
	cmd.Flags().BoolVar(&allowPrereleases, "allow-prereleases", false, "allow prerelease versions to be selected")
	return cmd
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// setHarnessConstraint acquires the pool lock and records `install`'s
// --constraint via setHarnessConstraintLocked. Exists for a standalone
// caller (and this package's own tests) that isn't already composing
// multiple pool calls under one pool.LockPool span; `install` itself calls
// setHarnessConstraintLocked directly, inside its single command-level lock
// (§6.5 MAJOR 3) alongside refresh/update/the actual npm install.
func setHarnessConstraint(paths pool.PoolPaths, name, constraint string) error {
	release, err := pool.LockPool(context.Background(), paths)
	if err != nil {
		return err
	}
	defer release()
	return setHarnessConstraintLocked(paths, name, constraint)
}

// setHarnessConstraintLocked is setHarnessConstraint's body, assuming the
// caller already holds the pool lock — it never acquires one itself. It
// records constraint into harnesses.<name>.constraint (creating the
// harness's manifest entry, and the manifest itself, if this is its first
// `install`). An empty constraint leaves any existing value untouched (§7
// "empty constraint means the descriptor's stable channel" — `install
// claude` after `install claude --constraint '>=2 <3'` must not silently
// widen back to "any version").
func setHarnessConstraintLocked(paths pool.PoolPaths, name, constraint string) error {
	return editManifest(paths.Manifest, func(doc *yaml.Node) error {
		harnessesNode := yamledit.EnsureMapChild(doc, "harnesses")
		entry := yamledit.EnsureMapChild(harnessesNode, name)
		if constraint != "" {
			yamledit.MapSet(entry, "constraint", yamledit.StrNode(constraint))
		}
		return nil
	})
}

// --- manifest.yaml node-tree editor ---
//
// manifest.yaml is user-owned, comment-friendly YAML (§7), just like
// wraptool's own config.yaml — both editors share their node-tree
// primitives and edit driver via internal/yamledit, a single
// comment-preserving editor instead of two independently drifting copies;
// only the schema_version bootstrap and the pool.ParseManifest/
// pool.WriteFileAtomic wiring below are manifest-specific.

// editManifest reads manifest.yaml (seeding an empty document if it doesn't
// exist yet — `policy set`/`install` are the manifest's own bootstrap, no
// separate init step), parses it into a YAML node tree (preserving comments
// and ordering), ensures schema_version is set, hands the document's root
// mapping node to mutate, then re-encodes, round-trip validates through
// pool.ParseManifest's strict decoder, and writes the result atomically
// under the pool lock discipline (§6.4/§6.5, via pool.WriteFileAtomic). The
// file is left untouched if mutate or validation fails.
func editManifest(path string, mutate func(doc *yaml.Node) error) error {
	return yamledit.Edit(path, yamledit.Options{
		Kind:          "manifest",
		FileMode:      0o644,
		SeedIfMissing: true,
		Validate: func(data []byte) error {
			_, err := pool.ParseManifest(data)
			return err
		},
		Write: pool.WriteFileAtomic,
	}, func(doc *yaml.Node) error {
		if yamledit.MapGet(doc, "schema_version") == nil {
			yamledit.MapSet(doc, "schema_version", yamledit.ScalarNode("1"))
		}
		return mutate(doc)
	})
}
