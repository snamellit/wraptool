package source

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
)

// seedCacheIndexScript is seed_cache_index.js, verbatim (§11.4 step 2a — see
// seedShrinkwrapCacheIndex's doc comment). Static, checked-in content with no
// per-harness data ever interpolated into it; all variable data travels
// through the separate seed-manifest.json this package generates per call.
//
//go:embed seed_cache_index.js
var seedCacheIndexScript string

// Container-side mount points for Realize's backend invocations (§6.3: Linux
// paths built with `path`, from named constants — never a host path). cacheDir
// is mounted read-only at realizeCacheContainerDir; stagingGenDir (the future
// generation root) is mounted read-write at realizeStagingContainerDir, so
// every file the container writes lands directly in the caller's staging
// tree with no copy-back step.
const (
	realizeCacheContainerDir   = "/wraptool-npm-realize-cache"
	realizeStagingContainerDir = "/wraptool-npm-realize-staging"
)

// realizeStagingPaths are the container-side paths derived from
// realizeStagingContainerDir that Realize's three backend invocations use.
// Computed once so the argv-building code below never repeats path.Join
// calls with slightly different arguments.
type realizeStagingPaths struct {
	lib           string // <staging>/lib — npm ci's project root and final node_modules home
	buildHome     string // <staging>/.build/home — scratch $HOME for npm
	buildCache    string // <staging>/.build/npm-cache — scratch, writable npm cache (cacache format)
	buildTarballs string // <staging>/.build/npm-tarballs — .tgz-suffixed hardlinks npm cache add can recognize
	seedScript    string // <staging>/.build/seed-cache-index.js — see seedShrinkwrapCacheIndex
	seedManifest  string // <staging>/.build/seed-manifest.json — see seedShrinkwrapCacheIndex
	runtimeNode   string // <staging>/runtime/node — the unpacked node runtime
}

func newRealizeStagingPaths() realizeStagingPaths {
	build := path.Join(realizeStagingContainerDir, ".build")
	return realizeStagingPaths{
		lib:           path.Join(realizeStagingContainerDir, "lib"),
		buildHome:     path.Join(build, "home"),
		buildCache:    path.Join(build, "npm-cache"),
		buildTarballs: path.Join(build, "npm-tarballs"),
		seedScript:    path.Join(build, "seed-cache-index.js"),
		seedManifest:  path.Join(build, "seed-manifest.json"),
		runtimeNode:   path.Join(realizeStagingContainerDir, "runtime", "node"),
	}
}

// Realize builds one platform generation of the harness named harnessName
// (binary name binName, internal/harness Descriptor.Bin) in stagingGenDir
// (the future generation root — the caller/orchestrator owns naming,
// renaming it into runtime/generations/ and writing receipt.json) via b with
// network OFF throughout (§6.2, §11.1: realize never touches the network),
// then post-processes host-side to satisfy §6.4 (symlink-free, $0-relative
// wrappers, 0o755 exec bits) and returns the receipt. h must already be
// fetched: every npm package and the runtime artifact for key must already
// have a verified cache object under cacheDir (pool.CacheSHA256Dir()).
//
// h.NPMLock.Lockfile — the verbatim package-lock.json Resolve read back from
// npm — is materialized to disk BYTE FOR BYTE and is the only realization
// source of truth for the dependency graph; Realize never hand-reconstructs
// a lockfile from h.NPMLock.Packages (that summary map remains Fetch's
// cache-addressing source of truth and Realize's own cache-object
// verification list, nothing more). A hand-reconstructed lockfile provably
// loses information real npm installs depend on: optional/os/cpu gating (an
// os-mismatched optionalDependency with no recorded "os" field becomes
// unconditionally required — verified against claude-code's real lockfile,
// which carries nine such platform-gated optional dependencies) and nested
// version-conflict edges. A lock produced before this field existed has an
// empty Lockfile; Realize refuses to guess and asks the caller to re-resolve.
//
// b must provide npm, tar, xz (for the node runtime's .tar.xz — Go stdlib has
// no xz and §6.7 forbids a new dependency to add one), and coreutils (every
// Realize argv is itself `env HOME=... <command>...`, and guix's own Workdir
// realization additionally uses `env --chdir=`, see backend/guix.go);
// NewGuix's caller selects that package set, Realize does not.
//
// RunSpec.Env is never used to carry HOME: the guix backend merges spec.Env
// into the HOST guix client's own environment (backend/backend.go's
// execRunner.Run merges the delta onto os.Environ() for the process that
// invokes `guix` itself, not just the container it launches), so a
// container-only HOME there would corrupt the host guix process's own
// environment (e.g. its `~/.cache/guix` resolution). HOME is instead
// prefixed onto the container argv via coreutils' `env KEY=value <cmd>...`,
// which only affects the process inside the container.
//
// RunSpec.User is left at its zero value on every call: the guix backend
// rejects a non-empty User (backend/backend.go's RunSpec.User doc).
func (n *NPM) Realize(ctx context.Context, b backend.Backend, harnessName, binName string, h *pool.LockHarness, key pool.PlatformKey, cacheDir, stagingGenDir string) (*pool.Receipt, error) {
	if h == nil {
		return nil, fmt.Errorf("realize: nil LockHarness")
	}
	if harnessName == "" || binName == "" {
		return nil, fmt.Errorf("realize: harnessName and binName must both be non-empty")
	}
	if h.NPMLock == nil {
		return nil, fmt.Errorf("realize: %s: lock has no npm_lock (this Source only realizes npm harnesses)", harnessName)
	}
	if len(h.NPMLock.Lockfile) == 0 {
		return nil, fmt.Errorf("realize: %s: lock predates the embedded-lockfile schema; re-run wraptool harness update %s", harnessName, harnessName)
	}
	// npm-raw-lockfile-backfill backstop: a lock persisted by a pre-fix
	// binary can have a raw lockfile that is present and syntactically valid
	// but still missing a backfilled integrity on some entry (see
	// pool.NPMLock.RawComplete's doc comment) — `npm ci --offline` below
	// cannot match such an entry's cache object by integrity and would die
	// with an opaque ENOTCACHED. install/update self-heal this automatically
	// (orchestrator.go folds RawComplete into the resolve-axis decision, so
	// a fresh Resolve rewrites the entry before Realize ever runs on it
	// again); this check exists for the one path that reaches Realize
	// WITHOUT ever calling Resolve first — `sync`, which realizes straight
	// from an existing lock entry — where "re-run wraptool harness update"
	// is real, actionable advice rather than circular.
	if !h.NPMLock.RawComplete() {
		return nil, fmt.Errorf("realize: %s: lock's raw package-lock.json is missing an integrity value on at least one entry (predates the raw-lockfile integrity backfill); npm ci --offline cannot match its cache object; re-run wraptool harness update %s", harnessName, harnessName)
	}
	// npm-shrinkwrap — a SEPARATE defect class from the RawComplete check
	// above (see NPMLock.HasShrinkwrap's doc comment): a shrinkwrapped
	// package's own dependency subtree is reified by a real npm install from
	// the npm-shrinkwrap.json embedded in its extracted tarball, not from
	// this lock's Lockfile, and those nested fetches go through npm's
	// URL-indexed HTTP cache (make-fetch-happen) rather than the
	// integrity-addressed shortcut a normal lockfile-driven install takes —
	// `npm cache add` (content-only, keyed by integrity) never populates
	// that URL index. This USED TO be a hard preflight refusal here; it no
	// longer is. seedShrinkwrapCacheIndex (called after `npm cache add`,
	// below) seeds exactly that index from this same lock's already-verified
	// (resolved URL, integrity) pairs, which is sufficient for `npm ci
	// --offline` to satisfy the nested fetches without ever touching the
	// network (§11.4 step 2a; empirically verified against a real
	// @earendil-works/pi-coding-agent lockfile+shrinkwrap — see this
	// package's tests). If the seeded `npm ci --offline` still fails (an npm
	// internals change invalidates the seeding approach, or some other
	// realize-time defect), the ordinary error path below surfaces it like
	// any other realize failure — non-fatal on install/update, hard-fail on
	// sync — there is no longer a special early rejection to keep in sync
	// with that outcome.
	if h.Runtime == nil {
		return nil, fmt.Errorf("realize: %s: lock has no runtime requirement", harnessName)
	}
	// Cheap pre-flight cross-check, before any container run: the harness
	// package itself must be a top-level entry in its own lock. Without this
	// here, a mismatched/malformed lock would only surface three container
	// runs deep, as generateHarnessWrapper failing to find
	// lib/node_modules/<pkg>/package.json — a confusing failure mode for
	// what is really a lock-content problem.
	if _, ok := h.NPMLock.Packages["node_modules/"+h.Package]; !ok {
		return nil, fmt.Errorf("realize: %s: npm_lock has no top-level entry for the harness package %q (expected key %q)", harnessName, h.Package, "node_modules/"+h.Package)
	}
	if _, err := pool.ParsePlatformKey(string(key)); err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}
	arch, err := platformArch(key)
	if err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}

	artifact, ok := h.Runtime.Artifacts[key]
	if !ok {
		return nil, fmt.Errorf("realize: %s: no runtime artifact recorded for platform %q (lock records: %s); "+
			"musl targets are deferred (design §5.1) — realization currently supports glibc platforms only",
			harnessName, key, recordedRuntimeArtifactKeys(h.Runtime.Artifacts))
	}
	if err := verifyCacheObject(cacheDir, artifact.SHA256, fmt.Sprintf("runtime artifact for platform %q", key)); err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}

	pkgNames := sortedNPMPackageKeys(h.NPMLock.Packages)
	if len(pkgNames) == 0 {
		return nil, fmt.Errorf("realize: %s: npm_lock has no packages", harnessName)
	}
	for _, name := range pkgNames {
		if err := verifyCacheObject(cacheDir, h.NPMLock.Packages[name].SHA256, fmt.Sprintf("npm package %q", name)); err != nil {
			return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
		}
	}

	packageJSON, err := npmSynthesizePackageJSON(h.NPMLock.Lockfile)
	if err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}

	// --- host-side scratch layout under stagingGenDir ---
	libDir := filepath.Join(stagingGenDir, "lib")
	buildDir := filepath.Join(stagingGenDir, ".build")
	buildHomeDir := filepath.Join(buildDir, "home")
	buildCacheDir := filepath.Join(buildDir, "npm-cache")
	buildTarballsDir := filepath.Join(buildDir, "npm-tarballs")
	runtimeNodeDir := filepath.Join(stagingGenDir, "runtime", "node")
	binDir := filepath.Join(stagingGenDir, "bin")

	for _, d := range []string{libDir, buildHomeDir, buildCacheDir, buildTarballsDir, runtimeNodeDir, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("realize: %s: creating %s: %w", harnessName, d, err)
		}
	}

	if err := os.WriteFile(filepath.Join(libDir, "package.json"), packageJSON, 0o644); err != nil {
		return nil, fmt.Errorf("realize: %s: writing synthesized package.json: %w", harnessName, err)
	}
	// The verbatim lockfile, byte for byte — see Realize's doc comment.
	if err := os.WriteFile(filepath.Join(libDir, "package-lock.json"), h.NPMLock.Lockfile, 0o644); err != nil {
		return nil, fmt.Errorf("realize: %s: writing package-lock.json: %w", harnessName, err)
	}

	// Stage every locked npm tarball as a .tgz-suffixed hardlink (falling back
	// to a copy across a volume boundary): `npm cache add` decides whether an
	// argument is a tarball file from its path syntax, not its content, and
	// the pool's cache objects are named by bare sha256 hex with no extension
	// (confirmed empirically: `npm cache add <bare-hex-file>` fails with
	// "DirFetcher requires either a tree or an Arborist constructor to pack";
	// the identical file renamed to end in ".tgz" succeeds).
	//
	// Several lock entries can share one tarball (the same package@version
	// vendored under two parents), so each digest is staged once.
	sp := newRealizeStagingPaths()
	containerTarballs := make([]string, 0, len(pkgNames))
	staged := make(map[string]bool, len(pkgNames))
	for _, name := range pkgNames {
		digest := h.NPMLock.Packages[name].SHA256
		if staged[digest] {
			continue
		}
		staged[digest] = true
		dst := filepath.Join(buildTarballsDir, digest+".tgz")
		if err := hardlinkOrCopy(filepath.Join(cacheDir, digest), dst); err != nil {
			return nil, fmt.Errorf("realize: %s: staging npm tarball for %q: %w", harnessName, name, err)
		}
		containerTarballs = append(containerTarballs, path.Join(sp.buildTarballs, digest+".tgz"))
	}

	mounts := []backend.Mount{
		{Host: cacheDir, Container: realizeCacheContainerDir, ReadOnly: true},
		{Host: stagingGenDir, Container: realizeStagingContainerDir, ReadOnly: false},
	}
	// HOME is carried as an argv prefix, never RunSpec.Env — see Realize's
	// doc comment (the guix backend merges Env onto the HOST client's own
	// environment, not just the container's).
	homeEnvArg := "HOME=" + sp.buildHome

	// Step 2 (§11.4): seed npm's offline cache from the verified tarballs.
	// One invocation for every locked package (batched — npm accepts multiple
	// package-specs — to keep the number of container runs minimal).
	cacheAddArgv := append([]string{"env", homeEnvArg, "npm", "cache", "add", "--cache", sp.buildCache}, containerTarballs...)
	if err := b.Run(ctx, backend.RunSpec{
		Argv: cacheAddArgv, Mounts: mounts, Network: false, Platform: arch,
	}); err != nil {
		return nil, fmt.Errorf("realize: %s: npm cache add: %w", harnessName, err)
	}

	// Step 2a: for a shrinkwrapped harness only, additionally seed npm's
	// URL-indexed HTTP cache (make-fetch-happen) — see
	// seedShrinkwrapCacheIndex's doc comment for why this is needed and what
	// it does. Gated on needsShrinkwrapSeeding, not the summary
	// HasShrinkwrap flag alone: this is an extra container run plus
	// coupling to npm's own cacache internals, so it only runs where that
	// cost buys something.
	if needsShrinkwrapSeeding(h.NPMLock) {
		if err := seedShrinkwrapCacheIndex(ctx, b, harnessName, h.Package, h.NPMLock.Packages, cacheDir, buildDir, sp, mounts, homeEnvArg, arch); err != nil {
			return nil, err
		}
	}

	// Step 3: install from that cache, fully offline. Lifecycle scripts run
	// (--foreground-scripts, not --ignore-scripts, unlike Resolve): a script
	// that needs network fails here, which is correct — that dependency was
	// not locked (§11.4). This is also where claude/opencode's postinstall
	// copies the platform-specific native ELF binary over its placeholder
	// bin path (finding 2) — it runs here, before any bin classification.
	if err := b.Run(ctx, backend.RunSpec{
		Argv:     []string{"env", homeEnvArg, "npm", "ci", "--offline", "--foreground-scripts", "--cache", sp.buildCache},
		Mounts:   mounts,
		Network:  false,
		Platform: arch,
		Workdir:  sp.lib,
	}); err != nil {
		return nil, fmt.Errorf("realize: %s: npm ci --offline: %w", harnessName, err)
	}

	// Step 4: unpack the pinned node runtime, stripping the tarball's
	// top-level node-vX-Y-Z directory. Must happen in-container: the artifact
	// is .tar.xz and Go stdlib has no xz (§6.7).
	if err := b.Run(ctx, backend.RunSpec{
		Argv: []string{
			"env", homeEnvArg,
			"tar", "--strip-components=1", "-xJf",
			path.Join(realizeCacheContainerDir, artifact.SHA256),
			"-C", sp.runtimeNode,
		},
		Mounts: mounts, Network: false, Platform: arch,
	}); err != nil {
		return nil, fmt.Errorf("realize: %s: unpacking node runtime: %w", harnessName, err)
	}

	// --- host-side post-processing (§11.4 step 5, §6.4) ---
	// generateHarnessWrapper reads npm ci's own output (package.json + the
	// resolved bin target), so it runs before removeBuildResidue — though
	// residue removal doesn't touch lib/node_modules, order here is by
	// dependency, not necessity.
	if err := generateHarnessWrapper(stagingGenDir, h.Package, binName); err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}
	// Residue removal runs BEFORE the symlink scrub: .build/npm-cache can be
	// gigabytes for a large harness (every locked tarball's cacache-format
	// copy), and walking that only to find zero symlinks in it would be pure
	// waste — delete it first, then scrub only what's left.
	if err := removeBuildResidue(stagingGenDir, buildDir); err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}
	if err := despymlinkTree(stagingGenDir); err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}
	if err := ensureRuntimeBinariesExecutable(runtimeNodeDir); err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}

	// pool.ScanGenerationFiles is also the "assert zero symlinks remain, fail
	// loudly naming the survivor" check §11.4 step 5 calls for: its documented
	// behavior already fails the whole scan on any symlink, naming the
	// offending path, so a second bespoke walk here would only duplicate it.
	files, err := pool.ScanGenerationFiles(stagingGenDir)
	if err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}
	manifestDigest, err := pool.ManifestDigest(files)
	if err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}
	lockHash, err := h.Hash()
	if err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}

	receipt := &pool.Receipt{
		SchemaVersion:  pool.ReceiptSchemaVersion,
		Harness:        harnessName,
		Version:        h.Version,
		Platform:       key,
		RecipeRevision: h.RecipeRevision,
		LockEntryHash:  lockHash,
		Backend:        pool.ReceiptBackend{Name: b.Name()}, // ImageDigest/GuixCommit: a later phase fills these in
		Files:          files,
		ManifestDigest: manifestDigest,
	}
	if err := receipt.Validate(); err != nil {
		return nil, fmt.Errorf("realize: %s: assembling receipt: %w", harnessName, err)
	}
	return receipt, nil
}

// --- shrinkwrap URL-cache seeding (§11.4 step 2a) ---------------------------

// seedManifestEntry is one entry of seed-manifest.json — seed_cache_index.js's
// only per-harness input, besides the npm cache dir itself: a resolved
// tarball URL's upstream integrity and the tarball's own byte size, both
// already known and verified (Fetch verified integrity at cache-admission
// time; size is simply that cache object's file size on disk).
type seedManifestEntry struct {
	Integrity string `json:"integrity"`
	Size      int64  `json:"size"`
}

// seedShrinkwrapCacheIndex seeds npm's URL-indexed HTTP cache
// (make-fetch-happen — distinct from the content-addressed cache `npm cache
// add` already populated, in the same call sequence, just above this one)
// so that `npm ci --offline` can satisfy a shrinkwrapped package's nested,
// install-time fetches without ever touching the network.
//
// THE PROBLEM (empirically diagnosed against a real
// @earendil-works/pi-coding-agent lockfile+shrinkwrap, 2026-08 — see
// TestIntegration_ShrinkwrapSeeding): when npm reifies a package's own
// bundled npm-shrinkwrap.json subtree, each nested dependency goes through
// pacote's normal fetch path (pacote's RemoteFetcher), which takes a direct
// content-addressed cache shortcut (`cacache.get.stream.byDigest` — no
// network, no make-fetch-happen involvement at all) ONLY when BOTH a
// resolved URL and an integrity are already known before the fetch starts
// (pacote's fetcher.js, the `tarballStream` "fromCache" guard). A
// workspace-sibling dependency's entry INSIDE the embedded shrinkwrap
// (frozen into the tarball at publish time — wraptool has no way to patch
// those bytes, unlike its own outer lockfile, which backfillIntegrity
// already fixes) can carry a resolved URL but no integrity: the identical
// defect class backfillIntegrity fixes for wraptool's OWN lockfile, just
// unreachable here because it lives inside already-published tarball
// content. Without integrity, the direct-cache shortcut cannot apply, so
// pacote falls through to an ordinary make-fetch-happen fetch with no
// integrity constraint on the request at all; `npm ci --offline` maps to
// fetch mode "only-if-cached" (npm-registry-fetch's getCacheMode), which
// throws ENOTCACHED the instant there is no matching URL-keyed index entry
// — regardless of the fact that the tarball's CONTENT is already sitting in
// that very same cache, addressed by integrity.
//
// THE FIX: seed one make-fetch-happen index entry per locked package, keyed
// exactly as make-fetch-happen itself would
// ("make-fetch-happen:request-cache:<url>"), pointing at the integrity `npm
// cache add` already verified and stored content for. Since the fetch that
// would otherwise 404 has no integrity constraint of its own (see above),
// make-fetch-happen's entry-matching only checks header shape
// (host/compress/accept-*), which a same-origin, bare GET tarball request
// trivially satisfies against empty recorded headers; "only-if-cached" then
// returns straight from the index entry with no staleness check at all.
// Content is streamed from the SAME cache object `npm cache add` populated
// — nothing is re-downloaded, re-verified against a new value, or trusted
// beyond what Fetch already verified once at cache-admission time.
//
// This is deliberately coupled to npm's own cacache/make-fetch-happen
// internals — no public npm API describes or guarantees this cache format.
// seed_cache_index.js documents the exact internal modules/paths it depends
// on; TestIntegration_ShrinkwrapSeeding (a real `npm ci` against a real,
// shrinkwrapped fixture) is the backstop: it breaks loudly, not silently,
// the day upstream npm changes any of this.
//
// pkgs is h.NPMLock.Packages — every locked package, not only the ones
// nested under the shrinkwrapped subtree specifically — since determining
// in Go which entries the embedded shrinkwrap will actually need (that
// information lives inside the tarball, not this outer lock) is neither
// possible nor worth attempting: seeding the whole locked graph is simple
// and harmless.
func seedShrinkwrapCacheIndex(ctx context.Context, b backend.Backend, harnessName, pkgName string, pkgs map[string]pool.NPMLockPackage, cacheDir, buildDir string, sp realizeStagingPaths, mounts []backend.Mount, homeEnvArg, arch string) error {
	manifest, err := buildSeedManifest(pkgs, cacheDir)
	if err != nil {
		return fmt.Errorf("realize: %s: building shrinkwrap cache-seed manifest for %s: %w", harnessName, pkgName, err)
	}
	manifestJSON, err := marshalDeterministicJSON(manifest)
	if err != nil {
		return fmt.Errorf("realize: %s: encoding shrinkwrap cache-seed manifest for %s: %w", harnessName, pkgName, err)
	}
	// Written into buildDir (the SAME .build scratch tree the cache-add/ci
	// steps use), so the existing removeBuildResidue's whole-tree RemoveAll
	// already deletes both files — no separate residue-cleanup rule needed.
	if err := os.WriteFile(filepath.Join(buildDir, path.Base(sp.seedManifest)), manifestJSON, 0o644); err != nil {
		return fmt.Errorf("realize: %s: writing shrinkwrap cache-seed manifest: %w", harnessName, err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, path.Base(sp.seedScript)), []byte(seedCacheIndexScript), 0o644); err != nil {
		return fmt.Errorf("realize: %s: writing shrinkwrap cache-seed script: %w", harnessName, err)
	}

	if err := b.Run(ctx, backend.RunSpec{
		Argv:     []string{"env", homeEnvArg, "node", sp.seedScript, sp.seedManifest, sp.buildCache},
		Mounts:   mounts,
		Network:  false,
		Platform: arch,
	}); err != nil {
		return fmt.Errorf("realize: %s: seeding npm's URL cache index for shrinkwrapped package %s: %w", harnessName, pkgName, err)
	}
	return nil
}

// needsShrinkwrapSeeding reports whether the lock entry describes a
// shrinkwrapped package, from BOTH recorded sources: the summary
// NPMLock.HasShrinkwrap flag (written by Resolve since the flag existed) OR
// npm's own "hasShrinkwrap" markers inside the verbatim embedded
// package-lock.json. The raw document is the authority: a lock entry
// resolved by a wraptool that predates the summary flag carries the raw
// markers but not the flag, and nothing forces such a lock to re-resolve
// (the version is current and recipe_revision unchanged — found in the
// field on a real pi 0.83.0 lock, which failed `npm ci --offline` with
// ENOTCACHED because seeding was skipped). Deriving from the raw lockfile
// heals every such stale entry with no recipe-format bump and no
// re-resolve. A raw document that fails to parse contributes false rather
// than an error (the summary flag still applies): Realize's own
// materialization step will surface a genuinely corrupt document loudly,
// and the wedge rule says derived niceties must never hard-fail a path a
// re-resolve wouldn't also fix.
func needsShrinkwrapSeeding(l *pool.NPMLock) bool {
	if l.HasShrinkwrap {
		return true
	}
	var raw npmLockFile
	if err := json.Unmarshal(l.Lockfile, &raw); err != nil {
		return false
	}
	for _, entry := range raw.Packages {
		if entry.HasShrinkwrap {
			return true
		}
	}
	return false
}

// buildSeedManifest builds seedShrinkwrapCacheIndex's per-URL manifest from
// pkgs: for every locked package's resolved URL, its upstream integrity and
// the byte size of its already-fetched cache object (cacheDir/<sha256>) —
// exactly what `npm cache add` already verified and stored, restated as the
// (url, integrity, size) triple make-fetch-happen's own index format needs.
// Two packages resolving to the identical URL (the same version reached via
// two different lockfile paths) must record identical integrity and size;
// anything else indicates lock corruption and is rejected outright rather
// than silently picking one arbitrarily.
func buildSeedManifest(pkgs map[string]pool.NPMLockPackage, cacheDir string) (map[string]seedManifestEntry, error) {
	names := sortedNPMPackageKeys(pkgs)
	manifest := make(map[string]seedManifestEntry, len(pkgs))
	for _, name := range names {
		pkg := pkgs[name]
		fi, err := os.Stat(filepath.Join(cacheDir, pkg.SHA256))
		if err != nil {
			return nil, fmt.Errorf("package %q: stat cache object %s: %w", name, pkg.SHA256, err)
		}
		entry := seedManifestEntry{Integrity: pkg.Integrity, Size: fi.Size()}
		if existing, ok := manifest[pkg.Resolved]; ok && existing != entry {
			return nil, fmt.Errorf("package %q: resolved URL %s already recorded with different integrity/size (%+v vs %+v)", name, pkg.Resolved, existing, entry)
		}
		manifest[pkg.Resolved] = entry
	}
	return manifest, nil
}

// platformArch maps a PlatformKey to the RunSpec.Platform arch string
// (§6.1: Platform carries architecture only, never libc). Exhaustive over
// PlatformKey's four values, as the project's exhaustive linter requires.
func platformArch(key pool.PlatformKey) (string, error) {
	switch key {
	case pool.PlatformLinuxAMD64Glibc, pool.PlatformLinuxAMD64Musl:
		return "amd64", nil
	case pool.PlatformLinuxARM64Glibc, pool.PlatformLinuxARM64Musl:
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported platform key %q", key)
	}
}

// verifyCacheObject checks that digest is a well-formed sha256 hex string
// and that cacheDir/<digest> exists with non-zero size. It is an existence
// check only — Fetch already verified the object's content against its
// upstream sha512 SRI (npm packages) or recorded sha256 (runtime artifacts)
// at admission time; Realize does not re-verify integrity, only that the
// object Fetch should have produced is actually present (defense in depth
// against a caller invoking Realize before Fetch, or against a cache object
// having been removed since).
func verifyCacheObject(cacheDir, digest, label string) error {
	if !validSHA256Hex(digest) {
		return fmt.Errorf("%s: missing or malformed sha256 (fetch must run before realize)", label)
	}
	fi, err := os.Stat(filepath.Join(cacheDir, digest))
	if err != nil {
		return fmt.Errorf("%s: cache object %s not found: %w", label, digest, err)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("%s: cache object %s is empty", label, digest)
	}
	return nil
}

// sortedNPMPackageKeys returns pkgs' keys in sorted order, for deterministic
// argv generation (map iteration order is randomized in Go) — the same
// concern backend.sortedKeys addresses for RunSpec.Env.
func sortedNPMPackageKeys(pkgs map[string]pool.NPMLockPackage) []string {
	keys := make([]string, 0, len(pkgs))
	for k := range pkgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// recordedRuntimeArtifactKeys renders a lock's recorded runtime-artifact
// platform keys, sorted for determinism, for the "no runtime artifact
// recorded for platform %q" error above to name what IS recorded (rather
// than leaving the reader to guess). "none" when the map is empty — that
// shouldn't happen for any lock Resolve produced, but a nil/empty map must
// still render something other than an empty string.
func recordedRuntimeArtifactKeys(artifacts map[pool.PlatformKey]pool.LockRuntimeArtifact) string {
	if len(artifacts) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(artifacts))
	for k := range artifacts {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// --- host-side package.json synthesis (§11.4 step 1) ------------------------
//
// package-lock.json itself is NEVER hand-reconstructed (see Realize's doc
// comment) — it is h.NPMLock.Lockfile, written verbatim. The only thing
// Realize synthesizes is package.json, and only because `npm ci` requires
// one to exist alongside the lockfile; its content is derived FROM the raw
// lockfile's own root entry, not from the Packages summary map.

// npmReconstructedPackageJSON is the synthesized package.json Realize writes
// to drive `npm ci`.
type npmReconstructedPackageJSON struct {
	Dependencies map[string]string `json:"dependencies"`
}

// npmRawLockfileRoot is the subset of a raw package-lock.json this package
// reads: just enough to recover the root ("") entry's declared dependencies.
type npmRawLockfileRoot struct {
	Packages map[string]struct {
		Dependencies map[string]string `json:"dependencies"`
	} `json:"packages"`
}

// npmSynthesizePackageJSON extracts the root ("") packages entry's
// "dependencies" from lockfile (h.NPMLock.Lockfile, the verbatim
// package-lock.json Resolve read back from npm) and wraps it as a
// package.json `npm ci` can use to drive the identical install lockfile
// already fully describes.
func npmSynthesizePackageJSON(lockfile json.RawMessage) ([]byte, error) {
	var raw npmRawLockfileRoot
	if err := json.Unmarshal(lockfile, &raw); err != nil {
		return nil, fmt.Errorf("parsing lockfile to synthesize package.json: %w", err)
	}
	root, ok := raw.Packages[""]
	if !ok {
		return nil, fmt.Errorf(`lockfile has no root ("") packages entry`)
	}
	return marshalDeterministicJSON(npmReconstructedPackageJSON{Dependencies: root.Dependencies})
}

// marshalDeterministicJSON serializes v with map keys sorted (encoding/json's
// standard behavior for string-keyed maps) and HTML-escaping disabled (a
// scoped package name or SRI value could otherwise be silently mangled by the
// default escaper) — the same reasoning and pattern as
// pool.Receipt.Marshal/Lock.Marshal.
func marshalDeterministicJSON(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// json.Encoder.Encode appends a trailing newline; strip it so this
	// function's output shape matches a plain json.MarshalIndent caller would
	// expect (same trim Receipt.Marshal/Lock.Marshal apply).
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// hardlinkOrCopy hardlinks src to dst, falling back to a byte copy if the
// link fails (e.g. cacheDir and stagingGenDir are on different volumes).
func hardlinkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

// copyFile copies src's content to dst (mode 0o644; callers that need a
// specific mode chmod afterward). Following Go's usual os.Open/os.Readlink
// symlink semantics, src is followed transparently if it is itself a
// symlink — callers that need the FINAL target's own path (not just its
// content) resolve the chain explicitly first (see resolveSymlinkChain).
//
// It writes a temp file next to dst and renames it over dst, never opening
// an existing dst for writing: dst may be a hardlink to a pool cache object
// (staged by hardlinkOrCopy), and truncating it would empty the cache object
// in place, still named by its old sha256.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".wraptool-copy-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(0o644); err != nil {
		return err
	}
	if _, err = io.Copy(tmp, in); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// --- host-side post-processing (§11.4 step 5, §6.4) -------------------------

// wrapperShebang is the fixed first line of every generated wrapper script.
const wrapperShebang = "#!/bin/sh\n"

// generatedShellSegmentForbiddenChars is the set of characters
// validateGeneratedShellSegment rejects outright: a double quote, '$', a
// backtick, or a backslash could break out of a generated wrapper's
// double-quoted path segments; reject, don't escape — the same stance
// backend.validateMount takes on Mount paths.
const generatedShellSegmentForbiddenChars = "\"$`\\"

// validateGeneratedShellCharacters rejects a string about to be
// interpolated into a generated sh wrapper's exec line if it contains any of
// generatedShellSegmentForbiddenChars — reject, don't escape, the same
// stance backend.validateMount takes on Mount paths. Used both for values
// that must also stay within a fixed directory (see
// validateGeneratedShellSegment, which adds the ".." check on top) and for
// filepath.Rel-COMPUTED relative paths (writeRelativeWrapper), which
// legitimately contain ".." segments by construction and so only need the
// character check.
func validateGeneratedShellCharacters(label, s string) error {
	if strings.ContainsAny(s, generatedShellSegmentForbiddenChars) {
		return fmt.Errorf("%s %q contains a character not allowed in a generated wrapper (one of %q)", label, s, generatedShellSegmentForbiddenChars)
	}
	return nil
}

// validateGeneratedShellSegment rejects a string (a package name or a
// package.json-declared bin path — both npm-controlled, not wraptool's own,
// and both meant to stay WITHIN a fixed directory once joined onto it)
// before it is interpolated into a generated sh wrapper's exec line: any of
// generatedShellSegmentForbiddenChars (validateGeneratedShellCharacters), or
// a ".." path segment, which could otherwise escape that directory.
func validateGeneratedShellSegment(label, s string) error {
	if err := validateGeneratedShellCharacters(label, s); err != nil {
		return err
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." {
			return fmt.Errorf(`%s %q contains a ".." path segment, which is not allowed`, label, s)
		}
	}
	return nil
}

// generateHarnessWrapper writes stagingGenDir/bin/<binName>, the $0-relative
// wrapper for the installed package's declared bin entry (§11.4 step 5, plus
// the finding-2 extension below). pkg is the npm package name (h.Package);
// its installed package.json's "bin" field is read to find binName's script
// path, which is then classified by CONTENT (not assumed to always be a
// node script): claude/opencode ship a native ELF binary that postinstall
// copies over a placeholder path recorded in package.json's own "bin" field,
// so unconditional node-wrapping would silently break them.
func generateHarnessWrapper(stagingGenDir, pkg, binName string) error {
	if err := validateGeneratedShellSegment("package", pkg); err != nil {
		return err
	}

	pkgDir := filepath.Join(stagingGenDir, "lib", "node_modules", filepath.FromSlash(pkg))
	pkgJSONPath := filepath.Join(pkgDir, "package.json")
	data, err := os.ReadFile(pkgJSONPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", pkgJSONPath, err)
	}

	var meta struct {
		Name string          `json:"name"`
		Bin  json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("parsing %s: %w", pkgJSONPath, err)
	}
	binRel, err := resolveBinEntry(meta.Name, meta.Bin, binName)
	if err != nil {
		return fmt.Errorf("package %q: %w", pkg, err)
	}
	if err := validateGeneratedShellSegment("bin", binRel); err != nil {
		return fmt.Errorf("package %q: %w", pkg, err)
	}

	targetPath := filepath.Join(pkgDir, filepath.FromSlash(binRel))
	fi, err := os.Stat(targetPath)
	if err != nil {
		return fmt.Errorf("package %q: bin target %s: %w", pkg, targetPath, err)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("package %q: bin target %s is empty (a placeholder npm's postinstall never replaced?)", pkg, targetPath)
	}

	kind, err := classifyBinTarget(targetPath)
	if err != nil {
		return fmt.Errorf("package %q: classifying bin target %s: %w", pkg, targetPath, err)
	}

	binDir := filepath.Join(stagingGenDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", binDir, err)
	}
	wrapperPath := filepath.Join(binDir, binName)

	var execLine string
	switch kind {
	case binTargetNode:
		execLine = fmt.Sprintf(`exec "$d/runtime/node/bin/node" "$d/lib/node_modules/%s/%s" "$@"`, pkg, binRel)
	case binTargetDirect:
		execLine = fmt.Sprintf(`exec "$d/lib/node_modules/%s/%s" "$@"`, pkg, binRel)
		if err := os.Chmod(targetPath, 0o755); err != nil {
			return fmt.Errorf("chmod %s: %w", targetPath, err)
		}
	}
	content := wrapperShebang +
		`d=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)` + "\n" +
		execLine + "\n"
	if err := os.WriteFile(wrapperPath, []byte(content), 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", wrapperPath, err)
	}
	// WriteFile's mode is subject to umask; chmod explicitly so 0o755 is
	// exact (§6.4 "realization sets 0755 on bin/*").
	return os.Chmod(wrapperPath, 0o755)
}

// resolveBinEntry extracts the script path for binName from a package.json
// "bin" field, which is either a bare JSON string (npm's single-binary
// shorthand — applies only when the package's own name equals binName) or an
// object mapping bin names to script paths.
func resolveBinEntry(pkgName string, raw json.RawMessage, binName string) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf(`no "bin" field in package.json`)
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if pkgName != binName {
			return "", fmt.Errorf(`"bin" is the single string %q, naming the package %q, not %q`, asString, pkgName, binName)
		}
		return asString, nil
	}

	var asMap map[string]string
	if err := json.Unmarshal(raw, &asMap); err == nil {
		rel, ok := asMap[binName]
		if !ok {
			return "", fmt.Errorf(`"bin" has no entry named %q (have %v)`, binName, sortedStringMapKeys(asMap))
		}
		return rel, nil
	}

	return "", fmt.Errorf(`"bin" field has an unrecognized shape (want a string or an object)`)
}

// sortedStringMapKeys returns m's keys sorted, for a deterministic error
// message.
func sortedStringMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- bin-target / symlink-target classification -----------------------------

// binTargetKind is how generateHarnessWrapper's bin/<binName> wrapper should
// invoke a resolved, already-built bin target (finding 2).
type binTargetKind int

const (
	binTargetNode   binTargetKind = iota // route through the bundled node runtime
	binTargetDirect                      // already self-executing (its own shebang, or ELF) — exec directly
)

// elfMagic is the four-byte ELF header magic.
const elfMagic = "\x7fELF"

// readFilePrefix reads up to n bytes from the start of path, without reading
// the rest of a (potentially large — a native harness binary can be tens of
// megabytes) file. Uses io.ReadFull, not a single Read call: a plain Read is
// permitted by the io.Reader contract to return fewer bytes than requested
// even when more are available (regular files usually don't, but nothing
// guarantees it), which could otherwise split a 4-byte ELF magic or a "#!"
// shebang marker across two reads and silently misclassify the file.
// io.ReadFull loops until the buffer is full or the file ends; an
// io.ErrUnexpectedEOF (file shorter than n) is expected and not an error
// here — the caller works with however many bytes actually exist.
func readFilePrefix(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return buf[:got], nil
}

// parseShebang reports prefix's shebang interpreter line — the text after
// "#!", up to the first CR/LF — when prefix starts with "#!".
func parseShebang(prefix []byte) (line string, ok bool) {
	if len(prefix) < 2 || prefix[0] != '#' || prefix[1] != '!' {
		return "", false
	}
	line = string(prefix[2:])
	if idx := strings.IndexAny(line, "\r\n"); idx >= 0 {
		line = line[:idx]
	}
	return line, true
}

// shebangInterpreterIsNode reports whether a shebang interpreter line (as
// parseShebang returns it) names node: "/path/to/node [args]" or
// "/usr/bin/env node [args]" (env's own target name, not env's path, is
// what's checked in that form).
func shebangInterpreterIsNode(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	if filepath.Base(fields[0]) == "env" {
		return len(fields) >= 2 && filepath.Base(fields[1]) == "node"
	}
	return filepath.Base(fields[0]) == "node"
}

// classifyBinTarget inspects the first bytes of path — the resolved bin
// target AFTER npm ci and any postinstall script have run, so its final
// content is in place (finding 2):
//   - ELF magic -> binTargetDirect (a native binary, e.g. claude/opencode's
//     postinstall-copied platform binary);
//   - a shebang naming node -> binTargetNode;
//   - any other shebang -> binTargetDirect (the target already knows how to
//     execute itself);
//   - neither (no shebang, not ELF — npm's default assumption for a bin
//     entry with no other information) -> binTargetNode.
func classifyBinTarget(path string) (binTargetKind, error) {
	prefix, err := readFilePrefix(path, 256)
	if err != nil {
		return 0, err
	}
	if len(prefix) == 0 {
		return 0, fmt.Errorf("target is empty")
	}
	if bytes.HasPrefix(prefix, []byte(elfMagic)) {
		return binTargetDirect, nil
	}
	if line, ok := parseShebang(prefix); ok {
		if shebangInterpreterIsNode(line) {
			return binTargetNode, nil
		}
		return binTargetDirect, nil
	}
	return binTargetNode, nil
}

// --- symlink replacement (§11.4 step 5, §6.4) --------------------------------

// despymlinkTree replaces every symlink under root with an equivalent
// $0-relative wrapper (a node-shebang script target) or a plain file copy
// (anything else — a non-node shebang is already directly executable on its
// own, and so is a copy of it; see replaceSymlink) — §6.4's symlink-free
// invariant.
//
// This is a COLLECT-THEN-REWRITE operation, in that exact order, and the
// order matters for correctness, not just style: snapshotSymlinks first
// walks the WHOLE tree read-only, recording every symlink's path and raw
// target BEFORE anything is mutated. Every chain is then resolved against
// that frozen snapshot (resolveSymlinkChain), and only once every path is
// known does the rewrite loop actually touch disk. A single interleaved
// walk-and-rewrite (visit a symlink, resolve it via the LIVE filesystem,
// rewrite it, move to the next) is NOT equivalent and was a real bug: once
// symlink B has been rewritten into a regular file (a wrapper or a copy),
// a LATER-processed symlink A -> B pointing at it would find B "no longer a
// symlink" and stop resolution right there, silently treating B's NEW
// (rewritten) content as if it were the original target — including across
// different directories, where the resulting wrapper's $0-relative paths
// are computed for B's directory but end up copied into A's, resolving to
// nothing (or the wrong file) when A is actually run. Resolving everything
// against one immutable snapshot up front makes the rewrite phase's order
// irrelevant to the result.
func despymlinkTree(root string) error {
	snap, err := snapshotSymlinks(root)
	if err != nil {
		return err
	}

	// Sorted for a deterministic, reproducible rewrite order — no longer a
	// correctness requirement (every chain already resolves against the
	// frozen snapshot regardless of rewrite order), but deterministic
	// output is still worth keeping.
	paths := make([]string, 0, len(snap))
	for p := range snap {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		if err := replaceSymlink(root, snap, p); err != nil {
			return err
		}
	}
	return nil
}

// symlinkSnapshot maps a symlink's path to its raw (as os.Readlink returned
// it, not yet resolved to an absolute path) target, captured once across the
// whole tree before despymlinkTree rewrites anything.
type symlinkSnapshot map[string]string

// snapshotSymlinks walks root read-only and records every symlink's path and
// raw target. filepath.WalkDir's directory entries never resolve symlinks
// (the same property pool.ScanGenerationFiles' doc comment relies on), so a
// symlink-to-directory is recorded like any other symlink (and later
// rejected by replaceSymlink — npm layouts only ever symlink files) rather
// than silently walked into.
func snapshotSymlinks(root string) (symlinkSnapshot, error) {
	snap := make(symlinkSnapshot)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking %s: %w", p, err)
		}
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(p)
		if err != nil {
			return fmt.Errorf("reading symlink %s: %w", p, err)
		}
		snap[p] = target
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// maxSymlinkChainDepth bounds resolveSymlinkChain — a defensive guard against
// a symlink cycle that somehow evades the visited-set check below, keeping
// resolution from looping forever in a bug either way.
const maxSymlinkChainDepth = 40

// resolveSymlinkChain follows p through snap (the WHOLE tree's pre-rewrite
// symlink topology — see despymlinkTree's doc comment for why this must be a
// frozen snapshot, never the live filesystem) through as many hops as needed
// to reach a path snap does not itself list as a symlink: the final regular
// file, per the ORIGINAL tree shape, independent of what despymlinkTree's
// rewrite phase has done to any other symlink by the time this particular
// one is processed. Rejects a target whose resolution leaves root or cycles.
func resolveSymlinkChain(root string, snap symlinkSnapshot, p string) (finalPath string, err error) {
	visited := make(map[string]bool)
	cur := p
	for i := 0; ; i++ {
		if i > maxSymlinkChainDepth {
			return "", fmt.Errorf("symlink chain from %s is too deep (possible cycle)", p)
		}
		target, isSymlink := snap[cur]
		if !isSymlink {
			return cur, nil
		}
		if visited[cur] {
			return "", fmt.Errorf("symlink chain from %s cycles back to %s", p, cur)
		}
		visited[cur] = true

		if filepath.IsAbs(target) {
			return "", fmt.Errorf("symlink %s has an absolute target %q, which the pool must never contain (§6.3)", cur, target)
		}
		next := filepath.Clean(filepath.Join(filepath.Dir(cur), target))
		if !pathWithin(root, next) {
			return "", fmt.Errorf("symlink %s targets %s, which is outside the generation tree", cur, next)
		}
		cur = next
	}
}

// replaceSymlink replaces the single symlink at p (somewhere under root)
// with a wrapper or copy of its FULLY resolved target (resolveSymlinkChain,
// against snap — the pre-rewrite snapshot, per despymlinkTree's doc
// comment). Only a node-named shebang is node-wrapped (finding 4); any other
// shebang, or no shebang at all (including an ELF target), is copied
// verbatim — it is already directly executable on its own.
//
// finalPath is guaranteed untouched by any OTHER call this despymlinkTree
// run makes: by construction it is not a key of snap (that is exactly why
// resolveSymlinkChain stopped there), and this function only ever writes to
// paths that ARE keys of snap — so reading finalPath's content here always
// sees its original bytes, regardless of how many other symlinks in the
// tree have already been rewritten.
func replaceSymlink(root string, snap symlinkSnapshot, p string) error {
	finalPath, err := resolveSymlinkChain(root, snap, p)
	if err != nil {
		return err
	}
	info, err := os.Lstat(finalPath)
	if err != nil {
		return fmt.Errorf("lstat resolved target of %s: %w", p, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("internal error: resolved target %s of symlink %s is unexpectedly still a symlink", finalPath, p)
	}
	if info.IsDir() {
		return fmt.Errorf("symlink %s targets a directory (%s); only file symlinks are supported", p, finalPath)
	}

	prefix, err := readFilePrefix(finalPath, 256)
	if err != nil {
		return fmt.Errorf("reading symlink target of %s: %w", p, err)
	}
	node := false
	if line, ok := parseShebang(prefix); ok {
		node = shebangInterpreterIsNode(line)
	}

	if err := os.Remove(p); err != nil {
		return fmt.Errorf("removing symlink %s: %w", p, err)
	}

	if node {
		nodeBin := filepath.Join(root, "runtime", "node", "bin", "node")
		return writeRelativeWrapper(p, nodeBin, finalPath)
	}
	if err := copyFile(finalPath, p); err != nil {
		return fmt.Errorf("copying symlink target of %s: %w", p, err)
	}
	return os.Chmod(p, info.Mode().Perm())
}

// pathWithin reports whether target is root or a descendant of root.
func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// writeRelativeWrapper writes a $0-relative wrapper at dstPath (replacing the
// symlink that used to live there) execing nodeBin against script — both
// absolute host paths used only to COMPUTE the relative segments embedded in
// the wrapper text; no absolute path is ever written to disk (§6.3). The
// computed relative segments are themselves character-validated
// (validateGeneratedShellCharacters, NOT the stricter
// validateGeneratedShellSegment — a filepath.Rel result legitimately
// contains ".." segments by construction) before interpolation: their
// FILENAME components are derived from on-disk names, which — unlike
// wraptool's own path constants — can contain whatever bytes a (possibly
// malicious) npm package chose to name a file, including characters that
// would otherwise break out of the generated wrapper's quoting.
func writeRelativeWrapper(dstPath, nodeBin, script string) error {
	fromDir := filepath.Dir(dstPath)
	relNode, err := filepath.Rel(fromDir, nodeBin)
	if err != nil {
		return fmt.Errorf("computing relative path to node runtime: %w", err)
	}
	relScript, err := filepath.Rel(fromDir, script)
	if err != nil {
		return fmt.Errorf("computing relative path to %s: %w", script, err)
	}
	relNode = filepath.ToSlash(relNode)
	relScript = filepath.ToSlash(relScript)
	if err := validateGeneratedShellCharacters("node runtime path", relNode); err != nil {
		return err
	}
	if err := validateGeneratedShellCharacters("script path", relScript); err != nil {
		return err
	}

	content := wrapperShebang +
		`d=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)` + "\n" +
		fmt.Sprintf(`exec "$d/%s" "$d/%s" "$@"`, relNode, relScript) + "\n"
	if err := os.WriteFile(dstPath, []byte(content), 0o755); err != nil {
		return err
	}
	return os.Chmod(dstPath, 0o755)
}

// ensureRuntimeBinariesExecutable sets 0o755 on every entry directly under
// runtimeNodeDir/bin (§6.4: "realization sets 0755 on ... the bundled runtime
// binaries"). tar generally preserves the archive's own permission bits on
// extraction, but this makes the invariant explicit rather than relying on
// that behavior.
func ensureRuntimeBinariesExecutable(runtimeNodeDir string) error {
	binDir := filepath.Join(runtimeNodeDir, "bin")
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", binDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(binDir, e.Name())
		if err := os.Chmod(p, 0o755); err != nil {
			return fmt.Errorf("chmod %s: %w", p, err)
		}
	}
	return nil
}

// removeBuildResidue deletes everything Realize's own build machinery leaves
// behind that must not survive into the digested generation (§11.4 step 5):
// the whole .build scratch tree (npm's writable cache, HOME, logs, and the
// staged tarball hardlinks — this can be gigabytes for a large harness, hence
// removed before the symlink scrub, not after), the scratch
// package.json/package-lock.json that drove `npm ci` (neither is part of the
// runtime graph — nothing in the generation reads them at execution time),
// and node_modules/.package-lock.json (npm's own auto-generated bookkeeping
// file).
func removeBuildResidue(stagingGenDir, buildDir string) error {
	if err := os.RemoveAll(buildDir); err != nil {
		return fmt.Errorf("removing build scratch dir %s: %w", buildDir, err)
	}
	for _, name := range []string{"package.json", "package-lock.json"} {
		p := filepath.Join(stagingGenDir, "lib", name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", p, err)
		}
	}
	npmPkgLock := filepath.Join(stagingGenDir, "lib", "node_modules", ".package-lock.json")
	if err := os.Remove(npmPkgLock); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", npmPkgLock, err)
	}
	return nil
}
