package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// defaultProvisioningImage is the pinned pool-provisioning OCI image (§11.2):
// a small image containing only resolver/build prerequisites (node+npm, tar,
// coreutils), built by .forgejo/workflows/publish-provisioning-image.yml
// from images/provisioning-manifest.scm and pinned by DIGEST, not just tag —
// §11.2 makes "the provisioning image reference and digest" a lock input
// (recipe_revision, §8/orchestrator.go's npmRecipeRevision), so an image
// upgrade must be an explicit, auditable recipe-revision bump, never a
// silent content change under a floating tag. The digest placeholder below
// is substituted by the architect from the CI job log before merge; nothing
// in this package (or its tests) depends on the literal digest value — see
// TestProvisioningImageIsDigestPinned, which stays skipped until the
// placeholder is gone. This is the true, immutable constant; provisioningImage
// below is the var seam production code actually reads.
const defaultProvisioningImage = "forge.snamellit.com/pti/wraptool/pool-provisioning@sha256:6d1d4434fadb1bf1f16166f4dfc6227a6eb6509ac3b005cc56e3824646e4bd03"

// provisioningImage is the package-level var every production call site
// reads (backend selection, recipe_revision folding) — a var, not a plain
// use of defaultProvisioningImage, so a cmd-level test can swap it (like
// hasGuix/ociAvailable/hostArch) to prove the image reference actually
// reaches recipe_revision end to end, without needing a real second image to
// install against. Starts equal to defaultProvisioningImage; nothing outside
// a test ever reassigns it.
var provisioningImage = defaultProvisioningImage

// harnessDeps bundles the harness commands' host-environment dependencies:
// pool paths, the legacy pool bin/ directory, the artifact-acquisition
// Source, and the resolve-phase container backend. Passed by constructor
// injection — newHarnessCmd threads one harnessDeps to every subcommand
// constructor, and each subcommand's RunE closure captures it — rather than
// package-level vars tests mutate globally. This makes the cmd tests safe
// to run with t.Parallel(): two tests building independent commands with
// independent fake deps never contend on shared state.
type harnessDeps struct {
	poolPaths  func() pool.PoolPaths
	poolBinDir func() (poolDir, poolBin string, err error)
	newSource  func(d harness.Descriptor) (pool.Source, error)
	// resolveBackend picks the resolve/realize container backend for one
	// target platform (§11.2's auto policy: guix when available AND the
	// target's arch matches the host's, else oci) — see defaultResolveBackend.
	resolveBackend func(target pool.PlatformKey) (backend.Backend, error)
}

// defaultHarnessDeps returns the real, production harnessDeps: the
// developer's actual pool (rooted at pool.Root()), the legacy pool bin/
// directory (ensureHarnessPool, cmd/guixcontainer.go), the real
// artifact-acquisition Source selected per descriptor (npm or archive,
// internal/pool/source), and the guix resolve-phase backend.
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
		// newSource constructs the harness pool's artifact-acquisition Source
		// for d (design/harness/unified-harness-pool.md §11.3, pool.Source):
		// the archive Source for an archive-sourced descriptor (agy), the
		// npm Source for everything else. Both concrete types live in
		// internal/pool/source; Go's structural typing binds each to
		// pool.Source, including Realize (§11.4).
		newSource: func(d harness.Descriptor) (pool.Source, error) {
			if d.PoolArchive {
				return source.NewArchive(""), nil // "" = the production Antigravity CLI release channel
			}
			return source.NewNPM(""), nil // "" = the default npm registry
		},
		resolveBackend: defaultResolveBackend,
	}
}

// platformTarget is the target-resolution thunk harnessBackend/backendCache
// take, rather than a plain pool.PlatformKey value: an archive-sourced
// harness needs no backend at all (harnessBackend's own gate below), so it
// must never pay for — or fail on — resolving a target platform it will
// never use. HostPlatformKey can itself fail (an unsupported host
// architecture), and that failure must only surface for a harness that
// actually needs a backend.
type platformTarget func() (pool.PlatformKey, error)

// hostPlatformTarget is the platformTarget install/update always use:
// resolve/realize for those two commands operate on the HOST's own platform,
// never a caller-chosen one (orchestrator.go's UpdateHarnessLocked doc
// comment: "platformKey is always the HOST's own platform ... for both
// install and update; only `sync` lets a caller target a different
// platform"). A named function, not an inline closure, so every install/
// update call site shares the exact same value.
func hostPlatformTarget() (pool.PlatformKey, error) { return pool.HostPlatformKey() }

// harnessBackend resolves the container backend for d's resolve/realize
// phases, gated on whether d's Source actually needs one (§6.2's phase
// table): an npm-sourced harness does (Resolve/Realize both run inside a
// Linux backend container), an archive-sourced harness never does (Resolve
// and Realize are pure Go, internal/pool/source/archive.go's own doc
// comment — "EVERY method here is pure Go, host-side, and NEVER invokes the
// caller-supplied backend.Backend"). Neither deps.resolveBackend nor target
// is even called for an archive-sourced harness, so an off-guix/off-OCI host
// never fails this probe for one — nor does a target() failure (e.g.
// install/update's hostPlatformTarget hitting an unsupported host
// architecture; sync's own target thunk never calls HostPlatformKey at all,
// since --platform is already validated against the closed PlatformKey enum)
// — that is what makes `wraptool harness install agy` reach
// refresh/resolve/fetch/realize/activate with no container runtime present
// at all (this work package's headline capability). The returned nil
// Backend for that case is safe: RealizeGeneration/UpdateHarnessLocked only
// ever pass it through to Source.Resolve/Source.Realize, and Archive's
// implementations of both never dereference it.
func harnessBackend(deps harnessDeps, d harness.Descriptor, target platformTarget) (backend.Backend, error) {
	if d.PoolArchive {
		return nil, nil
	}
	key, err := target()
	if err != nil {
		return nil, err
	}
	return deps.resolveBackend(key)
}

// backendCache memoizes harnessBackend's npm-sourced resolveBackend probe
// across one multi-harness update/sync run (review round 2, nit): every
// npm-sourced harness in a single bare `update`/`sync` needs the exact same
// backend (the target platform is fixed for the whole run — the host's own
// platform for update, or one `--platform` value for sync), so probing
// deps.resolveBackend fresh per harness would print the same no-backend
// message once per npm-sourced harness. Probing once and reusing the result
// (success OR error) avoids the noise. Archive-sourced harnesses never
// consult it at all (harnessBackend's own gate), so this cache is only ever
// populated by the FIRST npm-sourced harness a loop encounters — a zero
// value is ready to use.
type backendCache struct {
	done bool
	b    backend.Backend
	err  error
}

// get returns d's backend via harnessBackend, probing deps.resolveBackend
// at most once per backendCache regardless of how many npm-sourced
// descriptors are passed to it. The archive-source gate itself is NOT
// re-implemented here — it delegates to harnessBackend so the §6.2
// source-gating rule (archive needs no backend at all) lives in exactly
// one place.
func (c *backendCache) get(deps harnessDeps, d harness.Descriptor, target platformTarget) (backend.Backend, error) {
	if d.PoolArchive {
		return harnessBackend(deps, d, target)
	}
	if !c.done {
		c.b, c.err = harnessBackend(deps, d, target)
		c.done = true
	}
	return c.b, c.err
}

// poolSourceKind maps d to the pool.SourceKind its orchestrator calls must
// be driven with (§8.1's Lock.Source values) — the cmd-level counterpart of
// harnessBackend/newSource's own per-descriptor dispatch. Only meaningful
// for a PoolLockable descriptor; callers only invoke it after checking that.
func poolSourceKind(d harness.Descriptor) pool.SourceKind {
	if d.PoolArchive {
		return pool.SourceArchive
	}
	return pool.SourceNPM
}

// guixResolveRealizePackages is the guix package set BOTH the npm resolve
// phase (§6.2/§8.1) AND the realize phase (§6.2/§11.4) need, since one guix
// Backend value serves both: node (npm, for Resolve's `npm install
// --package-lock-only` and Realize's `npm cache add`/`npm ci --offline`),
// tar and xz (Realize's node-runtime unpack: `tar -xJf`; Go stdlib has no xz
// and §6.7 forbids adding one, so tar's -J needs the real `xz` on PATH), and
// coreutils. coreutils is NOT optional here even though the guix backend
// itself auto-appends it whenever a RunSpec sets Workdir (backend/guix.go):
// every one of Realize's three backend.Run calls prefixes its argv with
// `env HOME=...` (Realize's own doc comment — HOME must travel as an argv
// prefix, never RunSpec.Env, so it never leaks into the host guix client's
// own environment), but only ONE of those three calls (`npm ci`) sets
// Workdir. The other two (`npm cache add`, the node-runtime `tar` unpack)
// would run inside a container with no `env` binary at all if this list
// didn't name coreutils explicitly — a per-call, not a per-backend,
// property the backend's own Workdir-triggered auto-append cannot cover.
// bash is likewise required: `npm ci` runs with `--foreground-scripts`
// (Realize's own doc comment), so a package's lifecycle scripts (the
// claude/opencode postinstall that places the native ELF binary — load-
// bearing, not cosmetic) spawn via a shell inside the container; without a
// real `bash` on PATH there, that spawn fails and the postinstall step never
// runs.
var guixResolveRealizePackages = []string{"node", "tar", "xz", "coreutils", "bash"}

// hostArch is this host's architecture in the "amd64"/"arm64" form
// pool.PlatformKey's arch component and backend.RunSpec.Platform use. A
// package-level var — the same pattern cmd/guixcontainer.go's hasGuix and
// internal/pool/backend's own lookPath/toSlash/hostGOOS use — so a test can
// exercise the cross-arch backend-routing decision below without depending
// on the architecture the test binary itself happens to run on.
var hostArch = func() string { return runtime.GOARCH }

// ociAvailable reports whether an OCI engine (docker or podman) is usable on
// this host, via a throwaway backend.NewOCI instance's own Available()
// probe — the image reference passed to NewOCI is irrelevant to that probe
// (backend.OCI.Available only checks for docker/podman on PATH), so
// provisioningImage is used here purely for convenience, not because its
// value matters. A package-level var, like hasGuix, so tests can stub
// OCI-engine detection without a real docker/podman install.
var ociAvailable = func() bool {
	o, err := backend.NewOCI(provisioningImage)
	if err != nil {
		// provisioningImage is a non-empty package constant; NewOCI only
		// errors on an empty image, so this is unreachable outside a
		// wraptool bug — treated as "not available" rather than panicking.
		return false
	}
	return o.Available()
}

// targetArch maps target to the arch string ("amd64"/"arm64")
// backend.RunSpec.Platform and this file's own host/target-arch comparison
// use (§6.1: Platform carries architecture only, never libc) — the same
// mapping internal/pool/source's own platformArch applies when it builds
// Realize's RunSpec values; duplicated here (rather than exported from that
// package) because backend SELECTION is cmd's own concern, and the mapping
// itself is a two-line exhaustive switch, not shared logic. Exhaustive over
// pool.PlatformKey, as the project's exhaustive linter requires.
func targetArch(target pool.PlatformKey) (string, error) {
	switch target {
	case pool.PlatformLinuxAMD64Glibc, pool.PlatformLinuxAMD64Musl:
		return "amd64", nil
	case pool.PlatformLinuxARM64Glibc, pool.PlatformLinuxARM64Musl:
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported platform key %q", target)
	}
}

// defaultResolveBackend implements §11.2's auto backend-selection policy for
// one target platform: the guix backend when guix is on PATH AND target's
// arch matches this host's own arch, otherwise the oci backend (docker or
// podman, pinned to provisioningImage) — including the cross-arch case,
// where guix is available but cannot serve the request: guix --container has
// no cross-arch execution mode (backend/guix.go's own hostArch guard rejects
// a mismatched RunSpec.Platform at Run time), so `sync --platform
// linux-arm64-glibc` on an amd64 host routes to oci even though guix itself
// is installed. Both backends serve BOTH the npm resolve phase (§6.2/§8.1)
// and the realize phase (§6.2/§11.4) — the same Backend value is threaded
// through Source.Resolve and RealizeGeneration/Source.Realize by every
// caller in this file. Only ever called for an npm-sourced harness
// (harnessBackend skips it entirely for an archive-sourced one, which needs
// no backend at all — see harnessBackend's doc comment), so both error
// messages below are npm-specific by construction, not merely by wording.
//
// When neither backend can serve the request, the error names the ACTUAL
// cause rather than one generic message (fix-round RULING) — the two
// distinct shapes reaching this point can't both be blamed on "guix is
// missing":
//
//   - guix absent (on PATH or not, it doesn't matter which — no candidate
//     backend exists at all): names all three install options.
//   - guix PRESENT but target's arch differs from the host's, and no oci
//     engine either: the failure is guix's lack of cross-arch execution, not
//     guix being "absent" — attributing it to the wrong cause would send a
//     developer chasing a guix reinstall that can never fix a cross-arch
//     request.
func defaultResolveBackend(target pool.PlatformKey) (backend.Backend, error) {
	arch, err := targetArch(target)
	if err != nil {
		return nil, err
	}

	guixOK := hasGuix()
	if guixOK && arch == hostArch() {
		return backend.Select("guix", backend.Options{GuixPackages: guixResolveRealizePackages})
	}
	if ociAvailable() {
		return backend.Select("oci", backend.Options{OCIImage: provisioningImage})
	}

	if guixOK {
		// Reaching here with guixOK true means the early guix-selection
		// branch above did NOT return — i.e. arch != hostArch(): guix is
		// on PATH but cannot serve this cross-arch target, and no oci engine
		// is available to fall back to.
		return nil, fmt.Errorf("guix cannot cross-build for %s on this %s host; the oci backend "+
			"handles cross-platform realize — install docker or podman", target, hostArch())
	}

	return nil, fmt.Errorf("no provisioning backend available for target platform %s: guix requires "+
		"a matching host architecture (this host is %q) and either was not found on PATH or has a "+
		"different architecture, and neither docker nor podman were found for the oci backend; "+
		"install guix, or docker, or podman (this applies to npm-sourced harnesses only — an "+
		"archive-sourced harness like agy resolves/realizes in pure Go and needs no container backend "+
		"at all).\nOn a devcontainer host, provision harnesses via the wraptool feature instead: add\n  "+
		`"forge.snamellit.com/pti/wraptool/wraptool:0": {"harness": "<name>"}`+
		"\nto your .devcontainer/devcontainer.json (see examples/devcontainer). `wraptool up` "+
		"shares the pool home so auth/skills persist across projects", target, hostArch())
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
		newHarnessRollbackCmd(deps),
		newHarnessGCCmd(deps),
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
		if !d.PoolLockable() {
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
		if !harness.Registry[name].PoolLockable() {
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
		if !d.PoolLockable() {
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
		if !harness.Registry[name].PoolLockable() {
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
// LOCKED ACTIVE LATEST ELIGIBLE QUARANTINED REALIZED. For a pool-lockable
// harness, ACTIVE is the VERSION of the generation currently activated
// (runtime/active/<host-platform>/<harness>.gen) for THIS HOST's own
// platform — "-" when nothing is active there, regardless of whether an
// older/newer version is locked or realized for a different platform. For a
// legacy (not pool-lockable) harness, ACTIVE instead reports the pre-Phase-2
// legacy pool bin/ install state (there is no generation/activation concept
// for those). It reads the pool's manifest/lock/observations directly — no
// pool lock (list never mutates, §6.5) and no Source (pool.StatusHarness
// works from files already on disk). Factored out from the cobra RunE,
// taking its inputs as plain parameters, so it can be tested against a temp
// pool.
func runHarnessList(paths pool.PoolPaths, poolBin string, now time.Time, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "HARNESS\tCONSTRAINT\tLOCKED\tACTIVE\tLATEST\tELIGIBLE\tQUARANTINED\tREALIZED")
	// A single HostPlatformKey() failure (unsupported host arch) must not
	// abort the whole table — every pool-lockable row just falls back to
	// ACTIVE="-", the same as "nothing active yet".
	hostKey, hostKeyErr := pool.HostPlatformKey()
	for _, name := range harness.Names() {
		d := harness.Registry[name]

		if !d.PoolLockable() {
			active := "-"
			if d.IsCLI() {
				if fileExists(filepath.Join(poolBin, d.Bin)) {
					active = "yes"
				} else {
					active = "no"
				}
			}
			fmt.Fprintf(w, "%s\t-\t-\t%s\t-\t-\tlegacy (not lockable)\t-\n", name, active)
			continue
		}

		status, err := pool.StatusHarness(paths, name, poolSourceKind(d), d.PoolPackageID(), now)
		if err != nil {
			return fmt.Errorf("harness %q: %w", name, err)
		}
		active := "-"
		if hostKeyErr == nil {
			if genID, ok := status.ActiveGenerations[hostKey]; ok {
				if v, err := pool.GenerationVersion(paths, genID); err == nil {
					active = v
				}
			}
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

			if !d.PoolLockable() {
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
// with zero side effects. For an archive-sourced harness (agy), that probe
// never even calls deps.resolveBackend (harnessBackend's doc comment) — an
// off-guix/off-OCI host reaches refresh/resolve/fetch/realize/activate
// exactly the same as an on-guix one.
func runHarnessInstall(ctx context.Context, cmd *cobra.Command, deps harnessDeps, name string, d harness.Descriptor, constraint string, allowYoung bool, reason string) error {
	paths := deps.poolPaths()

	src, err := deps.newSource(d)
	if err != nil {
		return err
	}
	b, err := harnessBackend(deps, d, hostPlatformTarget)
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

	pkg := d.PoolPackageID()
	kind := poolSourceKind(d)
	now := time.Now().UTC()
	if _, err := pool.RefreshHarnessLocked(ctx, src, paths, kind, pkg, now); err != nil {
		return fmt.Errorf("refreshing %s: %w", name, err)
	}

	result, err := pool.UpdateHarnessLocked(ctx, src, b, paths, pool.UpdateOptions{
		Harness: name, Package: pkg, Source: kind, BinName: d.Bin, Now: now,
		AllowYoung: allowYoung, Reason: reason,
		// install never advances a floating constraint (§10) — it's
		// idempotent once manifest/lock/recipe agree, even if a newer
		// eligible version exists.
		Advance: false,
		// ProvisioningImage is folded into recipe_revision regardless of
		// which backend b actually resolved to (orchestrator.go's
		// UpdateOptions.ProvisioningImage doc comment) — always the pinned
		// constant, never a value derived from b.
		ProvisioningImage: provisioningImage,
	})
	if err != nil {
		// UpdateHarnessLocked covers resolve+lock+fetch (§16 Phase 2) — a
		// Realize failure is no longer reported through this err (it's
		// non-fatal, result.RealizeErr, handled by printRealizeWarning
		// below), so anything reaching here is a genuine resolve/fetch/lock
		// failure. Its own per-phase errors ("resolving %s@%s", "fetching
		// %s@%s", "validating lock") are already specific, so this wrap only
		// adds the harness name's context, not a (possibly wrong) phase
		// claim.
		return fmt.Errorf("updating %s: %w", name, err)
	}
	printUpdateResult(cmd, name, result)
	printRealizeWarning(cmd, name, result)
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
		// no-op, but RealizeGeneration DID run this call — "(up to date)"
		// alone would misreport that as nothing having happened (review
		// finding 5). The generation id itself lands via the shared suffix
		// below.
		msg = fmt.Sprintf("%s: locked version %s unchanged", name, result.Harness.Version)
	default:
		msg = fmt.Sprintf("%s: already locked at %s (up to date)", name, result.Harness.Version)
	}
	if result.RealizedGenerationID != "" {
		msg += fmt.Sprintf("; realized %s", result.RealizedGenerationID)
	}
	fmt.Fprintln(out, msg)
}

// printRealizeWarning prints a clearly-labeled warning to stderr when
// result.RealizeErr is set (wave1-integration-fixes finding 2, §16 Phase 2
// item 4 "legacy fallback retained"): a Realize failure must not abort the
// legacy install path, since dispatcher/activation/mounting are a later
// wave and the legacy npm install (maybeInstallLocked) is the ONLY thing
// `install`/`update` actually execute this wave. install and update both
// call this immediately after printUpdateResult and then proceed to run the
// legacy installer regardless of what it prints. A no-op when RealizeErr is
// nil.
func printRealizeWarning(cmd *cobra.Command, name string, result *pool.UpdateResult) {
	if result.RealizeErr == nil {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: generation realization failed (legacy install continues): %v\n", name, result.RealizeErr)
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
//
// Realized-generation legacy-install downgrade (§16 Phase 2 posture, review
// round 2 major, WIDENED by wave2-integration-fixes item 4, and WIDENED
// AGAIN by this work package's fix round to cover every pool-lockable
// source, not just archive): for a pool-lockable harness (npm OR archive)
// whose pool generation was genuinely built AND activated (result.RealizeErr
// == nil AND this host's platform has a live active generation — NOT merely
// result.RealizedGenerationID != "", which is empty on the already-up-to-date
// path, UpdateResult's own doc comment), a legacy-install failure is
// downgraded to a warning (naming the underlying error) and this function
// returns nil: the developer's actual goal (`install`/`update`) substantively
// succeeded — a locked, realized, active generation exists, and
// cmd/guixcontainer.go's guix ACTIVE plan now genuinely mounts runtime/
// (read-only) into the container and puts runtime/bin on PATH — so the
// legacy bin/ copy is no longer the only way the generation reaches a
// container; it is Phase 4 cleanup, not day-0 blocking.
//
// The downgrade's SCOPE differs by source, though (fix-round RULING):
//
//   - archive (agy): ANY legacy-install failure downgrades, unchanged from
//     before — its curl|bash legacy installer has failure modes (a genuine
//     network error, a changed release layout) that are just as "the pool
//     already did the real work" as guix being absent, and nothing about
//     this source's realize story makes one cause more forgivable than
//     another.
//   - npm (claude/opencode/pi): ONLY the guix-absent cause downgrades
//     (errors.Is against errGuixAbsent). This is deliberately narrower: on a
//     docker/podman-only host (this work package's headline capability),
//     the host-side legacy npm installer is impossible PERMANENTLY and BY
//     DESIGN — it literally shells `guix shell node -- npm install ...`
//     (internal/harness's npmInstall recipe) — so guix being absent there is
//     not a "real installer problem" at all, it is the expected shape of a
//     successful oci-backend install. Any OTHER legacy-install failure on a
//     guix host (a broken npm registry, a failing postinstall script) IS a
//     real installer problem and must still fail the command — the pool's
//     own realize succeeding doesn't mean the legacy binary SHOULD have
//     failed to install too.
//
// install/update called via the shared runPerHarness path must still exit
// non-zero when the pool's own work didn't genuinely succeed (a real Realize
// failure, a non-guix-absent npm legacy-install failure, or a non-lockable
// harness).
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

	installErr := legacyInstallVersionLocked(cmd, deps, name, d, result.Harness.Version)
	if installErr == nil {
		return nil
	}
	if warned := warnInsteadOfFailForRealizedGeneration(cmd, deps, name, d, result, installErr); warned {
		return nil
	}
	return installErr
}

// warnInsteadOfFailForRealizedGeneration implements maybeInstallLocked's
// realized-generation legacy-install downgrade (see that function's doc
// comment for the full rationale, the archive-vs-npm scope split, and the
// exact preconditions) — factored out so its several preconditions read as
// one guarded sequence instead of nesting inside maybeInstallLocked itself.
// Returns true (having already printed the warning, which names installErr)
// iff every condition held; false means the caller must still treat
// installErr as a real failure.
func warnInsteadOfFailForRealizedGeneration(cmd *cobra.Command, deps harnessDeps, name string, d harness.Descriptor, result *pool.UpdateResult, installErr error) bool {
	if !d.PoolLockable() {
		return false
	}
	if result.RealizeErr != nil {
		return false
	}
	paths := deps.poolPaths()
	hostKey, err := pool.HostPlatformKey()
	if err != nil {
		return false
	}
	if _, ok := pool.ActiveGeneration(paths, name, hostKey); !ok {
		return false
	}

	if d.PoolArchive {
		// installErr is printed in full, not just its top-level message: for
		// the guix-absent cause specifically, offGuixInstallError's own text
		// already carries the devcontainer-feature guidance below — appending
		// it again here unconditionally would print it twice (a real
		// regression this widening's own transcript caught). For any OTHER
		// legacy-install failure (e.g. a genuine curl/network error), installErr
		// carries no such guidance, so printing it in full is exactly the
		// "names the underlying legacy error" the RULING requires; the devcontainer
		// route remains a generically-correct next step regardless of cause, but
		// this warning states the fact (pool generation built and locked) and
		// the concrete error rather than re-deriving guidance installErr may or
		// may not already include.
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: %s: pool generation built and locked, but the pool binary was NOT installed (%v)\n",
			name, installErr)
		return true
	}

	// npm-sourced: only the guix-absent cause is downgradable (see this
	// function's caller's doc comment for why) — any other legacy-install
	// failure is a real installer problem and must stay fatal.
	if !errors.Is(installErr, errGuixAbsent) {
		return false
	}
	// installErr (offGuixInstallError's text) already names what succeeded
	// from a legacy-installer-shape perspective is impossible here AND
	// carries the devcontainer-feature guidance — printed in full below, not
	// duplicated. This message additionally names the pool-side fact
	// installErr itself knows nothing about: a generation WAS built and
	// activated for this harness, so containers on this host already have a
	// working path to it (the guix runtime's runtime/ mount) even though the
	// host-side legacy binary does not exist.
	fmt.Fprintf(cmd.ErrOrStderr(),
		"warning: %s: pool generation built and activated (locked %s); host-side legacy install was skipped (%v)\n",
		name, result.Harness.Version, installErr)
	return true
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

	// pinInstallVersion only applies to an npm-style recipe (npmInstall's
	// argv embeds the bare package name to substitute pkg@version into). An
	// archive-sourced harness's legacy recipe (agy's curl|bash installer)
	// has no such token — its upstream channel has no "install this exact
	// version" mode at all — so it runs verbatim, exactly as it always has
	// (§16 Phase 2 posture: the actual version pin lives in the pool's own
	// lock/generation, not in this legacy fallback; a version-pinned native
	// install for these harnesses lands with the Phase 4 cutover).
	recipe := d.Install
	if d.PoolPackage != "" {
		pinned, err := pinInstallVersion(d.Install, d.PoolPackage, version)
		if err != nil {
			return fmt.Errorf("pinning install recipe for %s: %w", name, err)
		}
		recipe = pinned
	}
	argv := make([]string, len(recipe))
	for i, a := range recipe {
		argv[i] = strings.ReplaceAll(a, "{prefix}", poolDir)
	}
	if argv[0] == "guix" && !hasGuix() {
		return offGuixInstallError(name)
	}
	// An unpinned recipe (d.PoolPackage == "") never actually installs
	// exactly `version` — printing "installing agy@1.1.10" would claim a
	// precision this channel's installer doesn't have (review round 2,
	// minor).
	if d.PoolPackage != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "installing %s@%s into %s ...\n  %s\n", name, version, poolBin, strings.Join(argv, " "))
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "installing %s (unpinned - this channel has no version selector) into %s ...\n  %s\n", name, poolBin, strings.Join(argv, " "))
	}
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

// errGuixAbsent is offGuixInstallError's wrapped sentinel — matched via
// errors.Is (never string-matching, review round 2 major) by
// maybeInstallLocked's archive-source downgrade-to-warning path: an
// archive-sourced harness's pool generation can be fully built and
// activated with no guix at all (§6.2), so a guix-absent failure of the
// LEGACY bin/ install step alone must not be conflated (by string-matching
// some OTHER failure's text) with a genuine, harder failure.
var errGuixAbsent = errors.New("`guix` is not on PATH")

// offGuixInstallError is the off-guix pointer error shared by every install
// path (legacy and pool-lockable): the pool installers run inside a guix
// container (FHS emulation dodges npm's detect-libc misread on bare guix).
// Off guix — a devcontainer host — the pool isn't the delivery path at all.
func offGuixInstallError(name string) error {
	return fmt.Errorf("`wraptool harness install` provisions the guix-container pool, but %w.\n"+
		"On a devcontainer host, provision %q via the "+
		"wraptool feature instead: add\n  "+
		`"forge.snamellit.com/pti/wraptool/wraptool:0": {"harness": %q}`+
		"\nto your .devcontainer/devcontainer.json (see examples/devcontainer). "+
		"`wraptool up` shares the pool home so auth/skills persist across projects", errGuixAbsent, name, name)
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

			release, err := pool.LockPool(context.Background(), paths)
			if err != nil {
				return err
			}
			defer release()

			// deps.newSource is per-descriptor (npm vs archive), so it moves
			// inside the per-harness closure below — a bare `refresh` may
			// touch harnesses of both source styles in one invocation.
			now := time.Now().UTC()
			return runPerHarness(cmd, names, func(name string) error {
				d := harness.Registry[name]
				src, err := deps.newSource(d)
				if err != nil {
					return err
				}
				candidates, err := pool.RefreshHarnessLocked(context.Background(), src, paths, poolSourceKind(d), d.PoolPackageID(), now)
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

			release, err := pool.LockPool(context.Background(), paths)
			if err != nil {
				return err
			}
			defer release()

			// deps.newSource/the backend probe are per-descriptor (npm vs
			// archive), so they move inside the per-harness closure below —
			// a bare `update` may touch harnesses of both source styles in
			// one invocation, and an archive-sourced one must never probe
			// resolveBackend at all (harnessBackend's doc comment). bc
			// memoizes the (possibly failing) resolveBackend probe across
			// every npm-sourced harness in this one run, so a host with no
			// usable backend prints the no-backend-available message once,
			// not once per npm-sourced harness (review round 2, nit —
			// backendCache's doc comment).
			now := time.Now().UTC()
			var bc backendCache
			return runPerHarness(cmd, names, func(name string) error {
				d := harness.Registry[name]
				src, err := deps.newSource(d)
				if err != nil {
					return err
				}
				b, err := bc.get(deps, d, hostPlatformTarget)
				if err != nil {
					return err
				}
				result, err := pool.UpdateHarnessLocked(context.Background(), src, b, paths, pool.UpdateOptions{
					Harness: name, Package: d.PoolPackageID(), Source: poolSourceKind(d), BinName: d.Bin, Now: now,
					AllowYoung: allowYoung, Reason: reason,
					// update is the only normal command that advances a
					// floating constraint (§10): always scan for a newer
					// eligible candidate, even when the existing lock
					// already agrees with manifest+recipe.
					Advance: true,
					// See runHarnessInstall's matching comment: always the
					// pinned constant, regardless of which backend b
					// resolved to.
					ProvisioningImage: provisioningImage,
				})
				if err != nil {
					return err
				}
				printUpdateResult(cmd, name, result)
				printRealizeWarning(cmd, name, result)
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
harness-lock.json, then realizes (§16 Phase 2) exactly that version — never
resolving a new one. For a new workstation, a lost cache, or a platform the
lock already knows about but this pool hasn't realized yet (--platform,
default: this host's own platform). With no harness argument, syncs every
harness currently present in the lock.

When --platform names this host's own platform (the default), sync also
installs the harness's legacy host-side binary, same as install/update. When
it names a DIFFERENT platform (cross-platform realize, e.g. building a Linux
arm64 artifact from an amd64 or non-Linux host, §11.2), the legacy install
step is skipped entirely: it is a foreign-platform artifact operation, and
the host's own legacy bin/ is out of scope for it.`,
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
			// crossPlatform gates the legacy-install step below (fix-round
			// RULING, item 5): the legacy installer always produces a
			// HOST-native artifact (it shells the harness's own npm/curl
			// installer on THIS machine), so it can only ever be correct for
			// this host's own platform. A HostPlatformKey failure (an
			// unsupported host architecture) is treated the same as a
			// mismatch — such a host has no "own platform" the legacy
			// installer could target correctly either.
			hostKey, hostErr := pool.HostPlatformKey()
			crossPlatform := hostErr != nil || key != hostKey

			paths := deps.poolPaths()
			names, err := lockHarnessArgs(paths, args)
			if err != nil {
				return err
			}

			release, err := pool.LockPool(context.Background(), paths)
			if err != nil {
				return err
			}
			defer release()

			// deps.newSource/the backend probe are per-descriptor (npm vs
			// archive), so they move inside the per-harness closure below —
			// see newHarnessRefreshCmd/newHarnessUpdateCmd's matching
			// comment (including bc's no-backend-message dedup). syncTarget
			// always reports the already-resolved key (never the host's own
			// platform) — this is what routes a cross-arch `sync --platform`
			// to the oci backend (§11.2) even when guix is available but
			// can't serve a foreign arch.
			var bc backendCache
			syncTarget := func() (pool.PlatformKey, error) { return key, nil }
			return runPerHarness(cmd, names, func(name string) error {
				d := harness.Registry[name]
				src, err := deps.newSource(d)
				if err != nil {
					return err
				}
				b, err := bc.get(deps, d, syncTarget)
				if err != nil {
					return err
				}
				lh, err := pool.SyncHarnessLocked(context.Background(), src, b, paths, name, d.Bin, key)
				if err != nil {
					return err
				}
				if crossPlatform {
					// Foreign-platform artifact operation (fix-round RULING,
					// item 5): the realized generation is for a DIFFERENT
					// platform than this host runs, so a host-side legacy
					// install here would produce a broken (wrong-platform)
					// binary at best — skip it entirely, not attempt-and-warn.
					fmt.Fprintf(cmd.OutOrStdout(), "%s: fetched cache objects for locked %s; realized for %s "+
						"(not this host's own platform — skipping host-side legacy install)\n", name, lh.Version, key)
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: fetched cache objects for locked %s; realized for %s; installing ...\n", name, lh.Version, key)
				return legacyInstallVersionLocked(cmd, deps, name, d, lh.Version)
			})
		},
	}
	cmd.Flags().StringVar(&platform, "platform", "", "target platform key to realize (default: this host's own platform); "+
		"one of linux-amd64-glibc, linux-arm64-glibc, linux-amd64-musl, linux-arm64-musl "+
		"(musl targets deferred; glibc only for now)")
	return cmd
}

// resolveSyncPlatform resolves a `--platform` flag string to a validated
// pool.PlatformKey: empty defaults to HostPlatformKey() (the one helper
// §5.1 designates for "this host's own platform"); non-empty is validated
// against the closed enum via ParsePlatformKey, whose error already lists
// every valid value. Shared by `sync --platform` and `rollback --platform`
// (both default to the host's own platform the same way).
func resolveSyncPlatform(platform string) (pool.PlatformKey, error) {
	if platform == "" {
		return pool.HostPlatformKey()
	}
	return pool.ParsePlatformKey(platform)
}

// --- rollback ---

func newHarnessRollbackCmd(deps harnessDeps) *cobra.Command {
	var toVersion, platform string
	cmd := &cobra.Command{
		Use:   "rollback <harness>",
		Short: "Switch a harness's active generation to a previously realized one (no network access)",
		Long: `Switches runtime/active/<platform>/<harness>.gen to a generation already
realized under runtime/generations/ — no network access, and
harness-lock.json is never rewritten (§10). With --to, rolls back to the
exact named version (error, listing what IS available, if that version was
never realized for this platform; ties between multiple generations sharing
that version break toward the most recently created one); without it, rolls
back to the most recently created generation other than the one currently
active (error if none is retained). Selection with no --to relies on
generation directory creation times, so if the pool was copied or restored
in a way that lost or reset those times, pass --to explicitly instead of
relying on the default "most recent" choice. --platform defaults to this
host's own platform.

The rolled-back pointer does not survive a later "wraptool harness sync" for
this harness/platform — sync always re-activates the lock's own generation,
undoing the rollback. update/install can do the same whenever they resolve
a new version or need to repair a missing generation for this platform.
Re-run rollback afterward if you still need the older generation active.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			d, ok := harness.Registry[name]
			if !ok {
				return fmt.Errorf("unknown harness %q; supported: %v", name, harness.Names())
			}
			if !d.PoolLockable() {
				return fmt.Errorf("%q is not pool-lockable yet (legacy install path); see `wraptool harness list`", name)
			}
			key, err := resolveSyncPlatform(platform)
			if err != nil {
				return err
			}

			genID, err := pool.RollbackHarness(context.Background(), deps.poolPaths(), name, key, toVersion)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: rolled back to generation %s for %s\n", name, genID, key)
			// §13, a cheap honor with no snapshot machinery: shared harness
			// home/ state may already have been migrated/written by the
			// newer version being rolled back from, and the older binary
			// activated here has no way to know or repair that.
			fmt.Fprintln(cmd.ErrOrStderr(), "warning: shared harness home state may have been written by a newer version; downgraded harnesses may misread it")
			return nil
		},
	}
	cmd.Flags().StringVar(&toVersion, "to", "", "roll back to this exact version (default: the most recently realized OTHER generation)")
	cmd.Flags().StringVar(&platform, "platform", "", "target platform key (default: this host's own platform)")
	return cmd
}

// --- gc ---

func newHarnessGCCmd(deps harnessDeps) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Remove unreferenced generations and cached artifacts",
		Long: `Removes every generation under runtime/generations/ that is neither
currently active for any platform, nor referenced by harness-lock.json, nor
among the 2 most recently created generations for its (harness, platform) —
that last rule is what keeps an immediately-previous version rollback-able
after an update replaces the lock's recorded digest (§15). Also removes
every cache/sha256/ object not referenced by the lock, and every leftover
staging transaction directory. --dry-run reports what would be removed
without deleting anything (it still takes the pool lock, for a consistent
view).

A container started before this ran may still hold a generation gc has
since removed (its bind mount was resolved at container-start time, not
live). If a harness misbehaves right after a gc, restart the container
(wraptool up) to pick up gc's retained set.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := pool.GCPool(context.Background(), deps.poolPaths(), dryRun)
			if err != nil {
				return err
			}
			printGCReport(cmd, report)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be removed without deleting anything")
	return cmd
}

// printGCReport reports one GCReport to cmd.OutOrStdout(), phrasing both
// removal and freed space as a future ("would remove" / "would free") for a
// dry run and as a completed action otherwise.
func printGCReport(cmd *cobra.Command, report pool.GCReport) {
	verb, freeVerb := "removed", "freed"
	if report.DryRun {
		verb, freeVerb = "would remove", "would free"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %d generation(s) (%d retained), %d cache object(s) (%d retained), %d staging directory(ies); %s %d bytes\n",
		verb, report.RemovedGenerations, report.RetainedGenerations, report.RemovedCacheObjects, report.RetainedCacheObjects,
		report.RemovedStagingDirs, freeVerb, report.FreedBytes)
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
