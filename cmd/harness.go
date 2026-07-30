package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pti/wraptool/internal/harness"
	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newHarnessCmd() *cobra.Command {
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
		newHarnessListCmd(),
		newHarnessInstallCmd(),
		newHarnessRefreshCmd(),
		newHarnessUpdateCmd(),
		newHarnessSyncCmd(),
		newHarnessPolicyCmd(),
	)
	return cmd
}

// harnessPoolPaths builds the pool.PoolPaths for the developer's real pool
// (rooted at pool.Root()), mirroring internal/pool/layout.go's accessors.
// Orchestrator functions themselves never call pool.Root(); only this cmd
// wiring does, so tests can point the orchestrator at a temp pool instead
// (internal/pool/orchestrator_test.go).
func harnessPoolPaths() pool.PoolPaths {
	return pool.PoolPaths{
		Manifest:     pool.ManifestFile(),
		Lock:         pool.LockFile(),
		Observations: pool.ObservationsFile(),
		Audit:        pool.AuditFile(),
		AdvisoryLock: pool.AdvisoryLockFile(),
		Cache:        pool.CacheDir(),
	}
}

// newHarnessSource constructs the harness pool's npm artifact-acquisition
// Source (design/unified-harness-pool.md §11.3, pool.Source). The concrete
// npm Source (internal/pool/source) is a sibling work package built in
// parallel with this one — Go's structural typing binds it to pool.Source
// with no change needed here once it lands. Until that integration wiring
// happens, this seam errors clearly instead of silently no-op'ing; `list`
// and `policy set` need no Source and work today regardless.
var newHarnessSource = func() (pool.Source, error) {
	return nil, fmt.Errorf("the harness pool's npm source is not wired yet " +
		"(internal/pool/source integration is pending); `wraptool harness list` and " +
		"`policy set` work today, `refresh`/`update`/`install`/`sync` do not yet")
}

// resolveBackend picks the Linux-container backend the npm resolve phase
// (§6.2/§8.1) runs in. Phase 1 wires guix only — the same off-guix pointer
// error `install` has always printed — OCI/--backend selection is Phase 3+
// (design/unified-harness-pool.md §16).
func resolveBackend() (backend.Backend, error) {
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

// lockableHarnessArgs resolves a `[harness]` optional-positional-arg cobra
// pattern to the set of pool-lockable harness names (d.PoolPackage != "") to
// operate on: the one named harness (validated known + lockable), or every
// lockable harness in stable order when no name is given.
func lockableHarnessArgs(args []string) ([]string, error) {
	if len(args) == 1 {
		name := args[0]
		d, ok := harness.Registry[name]
		if !ok {
			return nil, fmt.Errorf("unknown harness %q; supported: %v", name, harness.Names())
		}
		if d.PoolPackage == "" {
			return nil, fmt.Errorf("%q is not pool-lockable yet (legacy install path); see `wraptool harness list`", name)
		}
		return []string{name}, nil
	}
	var names []string
	for _, name := range harness.Names() {
		if harness.Registry[name].PoolPackage != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// --- list ---

func newHarnessListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List known harnesses and their pool lock/install status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, poolBin, _, _, err := ensureHarnessPool()
			if err != nil {
				return err
			}
			return runHarnessList(harnessPoolPaths(), poolBin, time.Now().UTC(), os.Stdout)
		},
	}
}

// runHarnessList renders `harness list`'s table (§10): HARNESS CONSTRAINT
// LOCKED ACTIVE LATEST ELIGIBLE QUARANTINED. ACTIVE reports the legacy pool
// bin/ install state (Phase 2's real activation-pointer state isn't wired
// yet). It reads the pool's manifest/lock/observations directly — no pool
// lock (list never mutates, §6.5) and no Source (pool.StatusHarness works
// from files already on disk). Factored out from the cobra RunE so it can be
// tested against a temp pool.
func runHarnessList(paths pool.PoolPaths, poolBin string, now time.Time, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "HARNESS\tCONSTRAINT\tLOCKED\tACTIVE\tLATEST\tELIGIBLE\tQUARANTINED")
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
			fmt.Fprintf(w, "%s\t-\t-\t%s\t-\t-\tlegacy (not lockable)\n", name, active)
			continue
		}

		status, err := pool.StatusHarness(paths, name, d.PoolPackage, now)
		if err != nil {
			return fmt.Errorf("harness %q: %w", name, err)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			name, dashIfEmpty(status.Constraint), dashIfEmpty(status.Locked), active,
			dashIfEmpty(status.Latest), dashIfEmpty(status.Eligible), renderQuarantined(status))
	}
	return w.Flush()
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

// formatRemaining renders a countdown to whole units, rounded up so "just
// under 3 days left" reads as "3d" rather than the more alarming-looking
// "2d" (a developer should not be told there's less time than there is).
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

func newHarnessInstallCmd() *cobra.Command {
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
				fmt.Printf("warning: %q has no locked-pool adapter yet; installing unpinned (legacy path, §8)\n", name)
				return legacyInstall(name, d)
			}
			return runHarnessInstall(context.Background(), name, d, constraint, allowYoung, reason)
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
// (§16 Phase 1: legacy execution).
func runHarnessInstall(ctx context.Context, name string, d harness.Descriptor, constraint string, allowYoung bool, reason string) error {
	paths := harnessPoolPaths()

	if err := setHarnessConstraint(paths, name, constraint); err != nil {
		return fmt.Errorf("recording pool intent for %s: %w", name, err)
	}

	src, err := newHarnessSource()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := pool.RefreshHarness(ctx, src, paths, d.PoolPackage, now); err != nil {
		return fmt.Errorf("refreshing %s: %w", name, err)
	}

	b, err := resolveBackend()
	if err != nil {
		return err
	}
	lh, changed, err := pool.UpdateHarness(ctx, src, b, paths, pool.UpdateOptions{
		Harness: name, Package: d.PoolPackage, Now: now,
		AllowYoung: allowYoung, Reason: reason,
	})
	if err != nil {
		return fmt.Errorf("locking %s: %w", name, err)
	}
	if changed {
		fmt.Printf("%s: locked %s\n", name, lh.Version)
	} else {
		fmt.Printf("%s: already locked at %s (up to date)\n", name, lh.Version)
	}

	return legacyInstallVersion(name, d, lh.Version)
}

// legacyInstall is today's unpinned pool install (pre-dating this work
// package): it runs a harness's own installer with a floating version. It
// remains the only path for harnesses with no PoolPackage (§8: curl|bash
// recipes like agy's stay legacy until an artifact-specific adapter exists).
func legacyInstall(name string, d harness.Descriptor) error {
	poolDir, poolBin, _, _, err := ensureHarnessPool()
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
	fmt.Printf("installing %s into %s ...\n  %s\n", name, poolBin, strings.Join(argv, " "))
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("installing %s: %w", name, err)
	}

	if bin := filepath.Join(poolBin, d.Bin); fileExists(bin) {
		fmt.Printf("installed %s -> %s\n", name, bin)
	} else {
		fmt.Printf("warning: %q not found after install (expected %s)\n", d.Bin, bin)
	}
	return nil
}

// legacyInstallVersion runs a pool-lockable harness's own npm installer
// pinned to exactly version — Phase 1's legacy execution path (§16): resolve,
// lock, and fetch now go through the pool, but the actual install still
// shells out to npm, now pinned instead of floating "latest".
func legacyInstallVersion(name string, d harness.Descriptor, version string) error {
	poolDir, poolBin, _, _, err := ensureHarnessPool()
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
	fmt.Printf("installing %s@%s into %s ...\n  %s\n", name, version, poolBin, strings.Join(argv, " "))
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("installing %s@%s: %w", name, version, err)
	}

	if bin := filepath.Join(poolBin, d.Bin); fileExists(bin) {
		fmt.Printf("installed %s -> %s\n", name, bin)
	} else {
		fmt.Printf("warning: %q not found after install (expected %s)\n", d.Bin, bin)
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

func newHarnessRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh [harness]",
		Short: "Fetch candidate version metadata for one or every lockable harness (metadata only, no download)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			names, err := lockableHarnessArgs(args)
			if err != nil {
				return err
			}
			src, err := newHarnessSource()
			if err != nil {
				return err
			}
			paths := harnessPoolPaths()
			now := time.Now().UTC()
			for _, name := range names {
				d := harness.Registry[name]
				candidates, err := pool.RefreshHarness(context.Background(), src, paths, d.PoolPackage, now)
				if err != nil {
					return fmt.Errorf("refreshing %s: %w", name, err)
				}
				fmt.Printf("%s: observed %d candidate version(s)\n", name, len(candidates))
			}
			return nil
		},
	}
}

// --- update ---

func newHarnessUpdateCmd() *cobra.Command {
	var allowYoung bool
	var reason string
	cmd := &cobra.Command{
		Use:   "update [harness]",
		Short: "Advance one or every harness's locked version to the newest eligible release satisfying its constraint",
		Long: `Chooses the newest release satisfying the harness's manifest constraint and
the pool's release/observation-age quarantine among already-refreshed
candidates, locks it, and installs it. Run "wraptool harness refresh" first
to pick up new candidate versions from the registry; update never fetches
metadata itself.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if allowYoung && reason == "" {
				return fmt.Errorf("--allow-young requires --reason")
			}
			names, err := lockableHarnessArgs(args)
			if err != nil {
				return err
			}
			src, err := newHarnessSource()
			if err != nil {
				return err
			}
			b, err := resolveBackend()
			if err != nil {
				return err
			}
			paths := harnessPoolPaths()
			now := time.Now().UTC()
			for _, name := range names {
				d := harness.Registry[name]
				lh, changed, err := pool.UpdateHarness(context.Background(), src, b, paths, pool.UpdateOptions{
					Harness: name, Package: d.PoolPackage, Now: now,
					AllowYoung: allowYoung, Reason: reason,
				})
				if err != nil {
					return fmt.Errorf("updating %s: %w", name, err)
				}
				if !changed {
					fmt.Printf("%s: already up to date at %s\n", name, lh.Version)
					continue
				}
				fmt.Printf("%s: locked %s; installing ...\n", name, lh.Version)
				if err := legacyInstallVersion(name, d, lh.Version); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&allowYoung, "allow-young", false, "bypass the release/observation age quarantine (requires --reason)")
	cmd.Flags().StringVar(&reason, "reason", "", "required with --allow-young; recorded in the pool audit log")
	return cmd
}

// --- sync ---

func newHarnessSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync [harness]",
		Short: "Realize the exact existing lock without resolving a new version",
		Long: `Fetches any cache objects missing for the version already recorded in
harness-lock.json, then installs exactly that version — never resolving a
new one. For a new workstation, a lost cache, or a platform the lock already
knows about but this pool hasn't realized yet.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			names, err := lockableHarnessArgs(args)
			if err != nil {
				return err
			}
			src, err := newHarnessSource()
			if err != nil {
				return err
			}
			paths := harnessPoolPaths()
			for _, name := range names {
				d := harness.Registry[name]
				lh, err := pool.SyncHarness(context.Background(), src, paths, name)
				if err != nil {
					return fmt.Errorf("syncing %s: %w", name, err)
				}
				fmt.Printf("%s: fetched cache objects for locked %s; installing ...\n", name, lh.Version)
				if err := legacyInstallVersion(name, d, lh.Version); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// --- policy set ---

func newHarnessPolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Configure pool-wide or per-harness update policy (release-age quarantine, prereleases)",
	}
	cmd.AddCommand(newHarnessPolicySetCmd())
	return cmd
}

func newHarnessPolicySetCmd() *cobra.Command {
	var harnessName string
	var minReleaseAge, minObservationAge time.Duration
	var allowPrereleases bool
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set pool-wide (or, with --harness, per-harness) update policy",
		Args:  cobra.NoArgs,
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

			paths := harnessPoolPaths()
			lock := pool.NewPoolLock(paths.AdvisoryLock)
			ctx := context.Background()
			if err := lock.Acquire(ctx); err != nil {
				return err
			}
			defer lock.Release()

			err := editManifest(paths.Manifest, func(doc *yaml.Node) error {
				var target *yaml.Node
				if harnessName != "" {
					harnessesNode := ensureMapChild(doc, "harnesses")
					target = ensureMapChild(harnessesNode, harnessName)
				} else {
					target = ensureMapChild(doc, "policy")
				}
				if releaseSet {
					mapSetNode(target, "minimum_release_age", scalarNode(minReleaseAge.String()))
				}
				if observationSet {
					mapSetNode(target, "minimum_observation_age", scalarNode(minObservationAge.String()))
				}
				if prereleaseSet {
					mapSetNode(target, "allow_prereleases", scalarNode(boolString(allowPrereleases)))
				}
				return nil
			})
			if err != nil {
				return err
			}

			if harnessName != "" {
				fmt.Printf("updated policy for %s in %s\n", harnessName, paths.Manifest)
			} else {
				fmt.Printf("updated pool-wide policy in %s\n", paths.Manifest)
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

// setHarnessConstraint records `install`'s --constraint into
// harnesses.<name>.constraint (creating the harness's manifest entry, and
// the manifest itself, if this is its first `install`). An empty constraint
// leaves any existing value untouched (§7 "empty constraint means the
// descriptor's stable channel" — `install claude` after `install claude
// --constraint '>=2 <3'` must not silently widen back to "any version").
func setHarnessConstraint(paths pool.PoolPaths, name, constraint string) error {
	lock := pool.NewPoolLock(paths.AdvisoryLock)
	ctx := context.Background()
	if err := lock.Acquire(ctx); err != nil {
		return err
	}
	defer lock.Release()

	return editManifest(paths.Manifest, func(doc *yaml.Node) error {
		harnessesNode := ensureMapChild(doc, "harnesses")
		entry := ensureMapChild(harnessesNode, name)
		if constraint != "" {
			mapSetNode(entry, "constraint", strNode(constraint))
		}
		return nil
	})
}

// --- manifest.yaml node-tree editor ---
//
// manifest.yaml is user-owned, comment-friendly YAML (§7), just like
// wraptool's own config.yaml. This mirrors internal/config/edit.go's
// editConfig/mapGet/mapSet pattern (read before writing this) rather than
// importing it: internal/config's helpers are unexported and its editConfig
// validates through config.Config, not pool.Manifest, so reuse would need an
// internal/config export this work package's brief scopes out.

// editManifest reads manifest.yaml (seeding an empty document if it doesn't
// exist yet — `policy set`/`install` are the manifest's own bootstrap, no
// separate init step), parses it into a YAML node tree (preserving comments
// and ordering), hands the document's root mapping node to mutate, ensures
// schema_version is set, then re-encodes, round-trip validates through
// pool.ParseManifest's strict decoder, and writes the result atomically
// under the pool lock discipline (§6.4/§6.5, via pool.WriteFileAtomic). The
// file is left untouched if mutate or validation fails.
func editManifest(path string, mutate func(doc *yaml.Node) error) error {
	var root yaml.Node
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if uerr := yaml.Unmarshal(data, &root); uerr != nil {
			return fmt.Errorf("parsing manifest: %w", uerr)
		}
	case os.IsNotExist(err):
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	default:
		return fmt.Errorf("reading manifest: %w", err)
	}

	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return fmt.Errorf("manifest is not a YAML document")
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return fmt.Errorf("manifest root is not a mapping")
	}

	if mapGetNode(doc, "schema_version") == nil {
		mapSetNode(doc, "schema_version", scalarNode("1"))
	}

	if err := mutate(doc); err != nil {
		return err
	}

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return fmt.Errorf("re-encoding manifest: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("re-encoding manifest: %w", err)
	}

	if _, err := pool.ParseManifest(out.Bytes()); err != nil {
		return fmt.Errorf("edited manifest failed validation: %w", err)
	}

	return pool.WriteFileAtomic(path, out.Bytes(), 0o644)
}

// ensureMapChild returns the mapping node at key under parent, creating (and
// appending) an empty one if absent, and coercing an existing non-mapping
// value (e.g. a stray `harnesses: null`) into an empty mapping rather than
// erroring — the caller is always about to add fields under it.
func ensureMapChild(parent *yaml.Node, key string) *yaml.Node {
	if child := mapGetNode(parent, key); child != nil {
		if child.Kind != yaml.MappingNode {
			child.Kind = yaml.MappingNode
			child.Tag = "!!map"
			child.Value = ""
			child.Content = nil
		}
		return child
	}
	child := &yaml.Node{Kind: yaml.MappingNode}
	mapSetNode(parent, key, child)
	return child
}

func mapGetNode(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapSetNode(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, scalarNode(key), value)
}

// scalarNode is an untagged plain scalar: the encoder resolves it to its
// natural YAML type (bool, duration-parseable string, etc.).
func scalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: value}
}

// strNode is a scalar explicitly tagged as a string, so the encoder quotes
// values that would otherwise misparse as YAML (a version constraint like
// ">=2 <3" starts with a folded-block-scalar indicator character) —
// mirroring internal/config/edit.go's strNode.
func strNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}
