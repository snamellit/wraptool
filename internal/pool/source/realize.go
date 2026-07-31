package source

import (
	"bytes"
	"context"
	"encoding/json"
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
	runtimeNode   string // <staging>/runtime/node — the unpacked node runtime
}

func newRealizeStagingPaths() realizeStagingPaths {
	build := path.Join(realizeStagingContainerDir, ".build")
	return realizeStagingPaths{
		lib:           path.Join(realizeStagingContainerDir, "lib"),
		buildHome:     path.Join(build, "home"),
		buildCache:    path.Join(build, "npm-cache"),
		buildTarballs: path.Join(build, "npm-tarballs"),
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
// b must provide npm, tar, xz (for the node runtime's .tar.xz — Go stdlib has
// no xz and §6.7 forbids a new dependency to add one), and coreutils (guix's
// Workdir realization uses `env --chdir=`, see backend/guix.go); NewGuix's
// caller selects that package set, Realize does not.
//
// RunSpec.User is left at its zero value on every call the guix backend
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
	if h.Runtime == nil {
		return nil, fmt.Errorf("realize: %s: lock has no runtime requirement", harnessName)
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
		return nil, fmt.Errorf("realize: %s: no runtime artifact recorded for platform %q", harnessName, key)
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

	packageJSON, packageLock, err := reconstructNPMManifests(h)
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
	if err := os.WriteFile(filepath.Join(libDir, "package-lock.json"), packageLock, 0o644); err != nil {
		return nil, fmt.Errorf("realize: %s: writing synthesized package-lock.json: %w", harnessName, err)
	}

	// Stage every locked npm tarball as a .tgz-suffixed hardlink (falling back
	// to a copy across a volume boundary): `npm cache add` decides whether an
	// argument is a tarball file from its path syntax, not its content, and
	// the pool's cache objects are named by bare sha256 hex with no extension
	// (confirmed empirically: `npm cache add <bare-hex-file>` fails with
	// "DirFetcher requires either a tree or an Arborist constructor to pack";
	// the identical file renamed to end in ".tgz" succeeds).
	sp := newRealizeStagingPaths()
	containerTarballs := make([]string, 0, len(pkgNames))
	for _, name := range pkgNames {
		digest := h.NPMLock.Packages[name].SHA256
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
	env := map[string]string{"HOME": sp.buildHome}

	// Step 2 (§11.4): seed npm's offline cache from the verified tarballs.
	// One invocation for every locked package (batched — npm accepts multiple
	// package-specs — to keep the number of container runs minimal).
	cacheAddArgv := append([]string{"npm", "cache", "add", "--cache", sp.buildCache}, containerTarballs...)
	if err := b.Run(ctx, backend.RunSpec{
		Argv: cacheAddArgv, Mounts: mounts, Network: false, Platform: arch, Env: env,
	}); err != nil {
		return nil, fmt.Errorf("realize: %s: npm cache add: %w", harnessName, err)
	}

	// Step 3: install from that cache, fully offline. Lifecycle scripts run
	// (--foreground-scripts, not --ignore-scripts, unlike Resolve): a script
	// that needs network fails here, which is correct — that dependency was
	// not locked (§11.4).
	if err := b.Run(ctx, backend.RunSpec{
		Argv:     []string{"npm", "ci", "--offline", "--foreground-scripts", "--cache", sp.buildCache},
		Mounts:   mounts,
		Network:  false,
		Platform: arch,
		Env:      env,
		Workdir:  sp.lib,
	}); err != nil {
		return nil, fmt.Errorf("realize: %s: npm ci --offline: %w", harnessName, err)
	}

	// Step 4: unpack the pinned node runtime, stripping the tarball's
	// top-level node-vX-Y-Z directory. Must happen in-container: the artifact
	// is .tar.xz and Go stdlib has no xz (§6.7).
	if err := b.Run(ctx, backend.RunSpec{
		Argv: []string{
			"tar", "--strip-components=1", "-xJf",
			path.Join(realizeCacheContainerDir, artifact.SHA256),
			"-C", sp.runtimeNode,
		},
		Mounts: mounts, Network: false, Platform: arch, Env: env,
	}); err != nil {
		return nil, fmt.Errorf("realize: %s: unpacking node runtime: %w", harnessName, err)
	}

	// --- host-side post-processing (§11.4 step 5, §6.4) ---
	if err := postProcessGeneration(stagingGenDir, h.Package, binName); err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}
	if err := removeBuildResidue(stagingGenDir, buildDir); err != nil {
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

// verifyCacheObject checks that digest is a well-formed sha256 hex string and
// that cacheDir/<digest> exists with non-zero size — the "verify its sha256
// cache object exists before running" precondition (§11.4 step 3) generalized
// to every artifact Realize consumes (npm tarballs included, defense in
// depth: Fetch should already guarantee this, but Realize does not trust that
// blindly, the same stance NPM.Fetch itself takes on caller-supplied data).
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

// --- host-side manifest reconstruction (§11.4 step 1) -----------------------

// npmReconstructedPackageJSON is the synthesized package.json Realize writes
// to drive `npm ci`.
type npmReconstructedPackageJSON struct {
	Dependencies map[string]string `json:"dependencies"`
}

// npmReconstructedLockPackage is one "packages" map entry of the
// reconstructed package-lock.json: either the root ("") entry (Dependencies
// only) or a package entry (Version/Resolved/Integrity only — never
// pool.NPMLockPackage.SHA256, which is pool-internal and has no npm
// lockfile equivalent).
type npmReconstructedLockPackage struct {
	Dependencies map[string]string `json:"dependencies,omitempty"`
	Version      string            `json:"version,omitempty"`
	Resolved     string            `json:"resolved,omitempty"`
	Integrity    string            `json:"integrity,omitempty"`
}

// npmReconstructedLock is the synthesized package-lock.json shape.
type npmReconstructedLock struct {
	LockfileVersion int                                    `json:"lockfileVersion"`
	Packages        map[string]npmReconstructedLockPackage `json:"packages"`
}

// npmTopLevelPackageName reports the package name for a "packages" map key
// that names a TOP-LEVEL install (directly under node_modules/, not nested
// under another package's own node_modules/) — e.g. "node_modules/leftpad"
// -> "leftpad", "node_modules/@scope/name" -> "@scope/name" (scoped names
// contain their own "/", which is not a nesting marker). A key nested under
// another package (e.g. "node_modules/a/node_modules/b", used by real npm
// lockfiles to represent a version-conflicting dependency) reports ok=false.
func npmTopLevelPackageName(key string) (name string, ok bool) {
	const prefix = "node_modules/"
	rest, found := strings.CutPrefix(key, prefix)
	if !found {
		return "", false
	}
	if strings.Contains(rest, "/node_modules/") {
		return "", false
	}
	return rest, true
}

// npmTopLevelDependencies collects every top-level package in pkgs as a
// direct dependency edge (name -> locked version).
//
// DEVIATION FROM THE LITERAL SPEC, DOCUMENTED HERE AND IN THE FINAL REPORT:
// the work package text describes the root "" entry as
// {"dependencies": {h.Package: h.Version}} — the harness package alone.
// Empirically (verified against a real npm 11 install, no network), `npm ci`
// builds its install set purely from lockfile-recorded dependency EDGES: a
// "packages" entry with no "dependencies" field, and no other entry's
// "dependencies" pointing at it, is silently skipped — no error, just a
// missing package — even though pool.NPMLockPackage has no field to carry
// those per-package edges (only version/resolved/integrity/sha256), so this
// package cannot reconstruct them. Declaring every TOP-LEVEL locked package
// as a direct root dependency is the strongest fix available without
// touching the lock schema: for the common case (no version-conflicting
// nested dependency — the overwhelming majority of flat npm installs, and
// everything this package's own tests exercise) it makes `npm ci` install
// the complete locked set. A NESTED entry (a key with more than one
// "node_modules/" segment — how a real npm lockfile represents two different
// versions of the same package required by different parents) has no
// possible root-level edge and is therefore still silently dropped by `npm
// ci`; that residual gap is a genuine schema limitation, not something this
// function's reconstruction strategy can paper over. See this package's
// Realize doc comment and the work-package final report for the full
// finding.
func npmTopLevelDependencies(pkgs map[string]pool.NPMLockPackage) map[string]string {
	deps := make(map[string]string, len(pkgs))
	for key, pkg := range pkgs {
		name, ok := npmTopLevelPackageName(key)
		if !ok {
			continue
		}
		deps[name] = pkg.Version
	}
	return deps
}

// reconstructNPMManifests rebuilds package.json + package-lock.json from h
// (§11.4 step 1). See npmTopLevelDependencies for the documented deviation
// from the work package's literal single-entry root-dependency text.
func reconstructNPMManifests(h *pool.LockHarness) (packageJSON, packageLock []byte, err error) {
	deps := npmTopLevelDependencies(h.NPMLock.Packages)
	if len(deps) == 0 {
		return nil, nil, fmt.Errorf("npm_lock has no top-level packages to install")
	}
	if _, ok := deps[h.Package]; !ok {
		return nil, nil, fmt.Errorf("npm_lock has no top-level entry for the harness package %q", h.Package)
	}

	packages := make(map[string]npmReconstructedLockPackage, len(h.NPMLock.Packages)+1)
	packages[""] = npmReconstructedLockPackage{Dependencies: deps}
	for name, pkg := range h.NPMLock.Packages {
		packages[name] = npmReconstructedLockPackage{
			Version:   pkg.Version,
			Resolved:  pkg.Resolved,
			Integrity: pkg.Integrity,
			// pkg.SHA256 deliberately not copied here: it is the pool's own
			// cache address, not an npm lockfile field.
		}
	}

	packageJSON, err = marshalDeterministicJSON(npmReconstructedPackageJSON{Dependencies: deps})
	if err != nil {
		return nil, nil, fmt.Errorf("encoding synthesized package.json: %w", err)
	}
	packageLock, err = marshalDeterministicJSON(npmReconstructedLock{LockfileVersion: 3, Packages: packages})
	if err != nil {
		return nil, nil, fmt.Errorf("encoding synthesized package-lock.json: %w", err)
	}
	return packageJSON, packageLock, nil
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
// specific mode chmod afterward).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// --- host-side post-processing (§11.4 step 5, §6.4) -------------------------

// wrapperShebang is the fixed first line of every generated wrapper script.
const wrapperShebang = "#!/bin/sh\n"

// postProcessGeneration runs every host-side post-processing step §11.4 step
// 5 requires, in order: generate the harness's own bin/<binName> wrapper,
// replace every remaining symlink in the tree with an equivalent wrapper or
// copy, then make sure the bundled runtime's own binaries are executable.
// Split out from Realize so it can be exercised directly against a
// synthetic, hand-built "npm ci output" tree with no Backend at all.
func postProcessGeneration(stagingGenDir, pkg, binName string) error {
	if err := generateHarnessWrapper(stagingGenDir, pkg, binName); err != nil {
		return err
	}
	if err := despymlinkTree(stagingGenDir); err != nil {
		return err
	}
	return ensureRuntimeBinariesExecutable(filepath.Join(stagingGenDir, "runtime", "node"))
}

// generateHarnessWrapper writes stagingGenDir/bin/<binName>, the $0-relative
// wrapper that execs the generation's own bundled node against the installed
// package's declared bin entry (§11.4 step 5). pkg is the npm package name
// (h.Package); its installed package.json's "bin" field is read to find the
// script path for binName.
func generateHarnessWrapper(stagingGenDir, pkg, binName string) error {
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

	binDir := filepath.Join(stagingGenDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", binDir, err)
	}
	wrapperPath := filepath.Join(binDir, binName)
	content := wrapperShebang +
		`d=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)` + "\n" +
		fmt.Sprintf(`exec "$d/runtime/node/bin/node" "$d/lib/node_modules/%s/%s" "$@"`, pkg, binRel) + "\n"
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

// despymlinkTree walks root and replaces every symlink it finds with an
// equivalent $0-relative wrapper (for a script target — anything starting
// with a shebang) or a plain file copy (anything else) — §11.4 step 5,
// §6.4's symlink-free invariant. filepath.WalkDir's directory entries never
// resolve symlinks (the same property pool.ScanGenerationFiles' doc comment
// relies on), so a symlink-to-directory is seen as a symlink (and rejected —
// npm layouts only ever symlink files) rather than silently walked into.
func despymlinkTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking %s: %w", p, err)
		}
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		return replaceSymlink(root, p)
	})
}

// replaceSymlink replaces the single symlink at p (somewhere under root)
// with a wrapper or copy of its resolved target, per despymlinkTree's doc
// comment.
func replaceSymlink(root, p string) error {
	target, err := os.Readlink(p)
	if err != nil {
		return fmt.Errorf("reading symlink %s: %w", p, err)
	}
	if filepath.IsAbs(target) {
		return fmt.Errorf("symlink %s has an absolute target %q, which the pool must never contain (§6.3)", p, target)
	}

	targetAbs := filepath.Clean(filepath.Join(filepath.Dir(p), target))
	if !pathWithin(root, targetAbs) {
		return fmt.Errorf("symlink %s targets %s, which is outside the generation tree", p, targetAbs)
	}

	info, err := os.Stat(targetAbs) // fully resolves, so a chained symlink target still yields real content/mode.
	if err != nil {
		return fmt.Errorf("stat symlink target of %s: %w", p, err)
	}
	if info.IsDir() {
		return fmt.Errorf("symlink %s targets a directory (%s); only file symlinks are supported", p, targetAbs)
	}

	script, err := hasShebang(targetAbs)
	if err != nil {
		return fmt.Errorf("reading symlink target of %s: %w", p, err)
	}

	if err := os.Remove(p); err != nil {
		return fmt.Errorf("removing symlink %s: %w", p, err)
	}

	if script {
		nodeBin := filepath.Join(root, "runtime", "node", "bin", "node")
		return writeRelativeWrapper(p, nodeBin, targetAbs)
	}
	if err := copyFile(targetAbs, p); err != nil {
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

// hasShebang reports whether the file at path starts with "#!".
func hasShebang(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	buf := make([]byte, 2)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false, err
	}
	return n >= 2 && buf[0] == '#' && buf[1] == '!', nil
}

// writeRelativeWrapper writes a $0-relative wrapper at dstPath (replacing the
// symlink that used to live there) execing nodeBin against script — both
// absolute host paths used only to COMPUTE the relative segments embedded in
// the wrapper text; no absolute path is ever written to disk (§6.3).
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

	content := wrapperShebang +
		`d=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)` + "\n" +
		fmt.Sprintf(`exec "$d/%s" "$d/%s" "$@"`, filepath.ToSlash(relNode), filepath.ToSlash(relScript)) + "\n"
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
// staged tarball hardlinks), the scratch package.json/package-lock.json that
// drove `npm ci` (not part of the runtime graph — nothing in the generation
// reads them), and node_modules/.package-lock.json (npm's own auto-generated
// bookkeeping file).
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
