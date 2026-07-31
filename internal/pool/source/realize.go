package source

import (
	"bytes"
	"context"
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

// generatedShellSegmentForbiddenChars is the set of characters
// validateGeneratedShellSegment rejects outright: a double quote, '$', a
// backtick, or a backslash could break out of a generated wrapper's
// double-quoted path segments; reject, don't escape — the same stance
// backend.validateMount takes on Mount paths.
const generatedShellSegmentForbiddenChars = "\"$`\\"

// validateGeneratedShellSegment rejects a string (a package name or a
// package.json-declared bin path — both npm-controlled, not wraptool's own)
// before it is interpolated into a generated sh wrapper's exec line: any of
// generatedShellSegmentForbiddenChars, or a ".." path segment (which could
// otherwise escape the generation tree once joined onto a directory).
func validateGeneratedShellSegment(label, s string) error {
	if strings.ContainsAny(s, generatedShellSegmentForbiddenChars) {
		return fmt.Errorf("%s %q contains a character not allowed in a generated wrapper (one of %q)", label, s, generatedShellSegmentForbiddenChars)
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
// megabytes) file.
func readFilePrefix(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, n)
	got, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
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

// despymlinkTree walks root and replaces every symlink it finds with an
// equivalent $0-relative wrapper (a node-shebang script target) or a plain
// file copy (anything else — a non-node shebang is already directly
// executable on its own, and so is a copy of it; see replaceSymlink) —
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

// maxSymlinkChainDepth bounds resolveSymlinkChain — a defensive guard against
// a symlink cycle that somehow evades the visited-set check below, keeping
// resolution from looping forever in a bug either way.
const maxSymlinkChainDepth = 40

// resolveSymlinkChain follows p (a symlink under root) through as many hops
// as needed to reach a regular file, resolving each hop itself — via
// os.Lstat/os.Readlink, never os.Stat's transparent following — so the
// returned path is the FINAL file's own path on disk, not an intermediate
// hop. This matters because filepath.WalkDir visits every symlink in the
// tree independently and in an arbitrary order: if a wrapper referenced an
// intermediate hop rather than the final regular file, and despymlinkTree
// later (independently) rewrote that intermediate hop's own content into
// something else (e.g. a node-wrapping symlink chain A -> B -> script, where
// A's wrapper is generated before B is visited: node-execing A's captured
// path would, after B's own unrelated rewrite, hand node a /bin/sh wrapper
// instead of the original script), the result would silently depend on
// processing order. Resolving to the final path up front makes classification
// and wrapper generation immune to that order. Rejects a target whose
// resolution leaves root or cycles.
func resolveSymlinkChain(root, p string) (finalPath string, info os.FileInfo, err error) {
	visited := make(map[string]bool)
	cur := p
	for i := 0; ; i++ {
		if i > maxSymlinkChainDepth {
			return "", nil, fmt.Errorf("symlink chain from %s is too deep (possible cycle)", p)
		}
		lst, err := os.Lstat(cur)
		if err != nil {
			return "", nil, fmt.Errorf("lstat %s: %w", cur, err)
		}
		if lst.Mode()&os.ModeSymlink == 0 {
			return cur, lst, nil
		}
		if visited[cur] {
			return "", nil, fmt.Errorf("symlink chain from %s cycles back to %s", p, cur)
		}
		visited[cur] = true

		target, err := os.Readlink(cur)
		if err != nil {
			return "", nil, fmt.Errorf("reading symlink %s: %w", cur, err)
		}
		if filepath.IsAbs(target) {
			return "", nil, fmt.Errorf("symlink %s has an absolute target %q, which the pool must never contain (§6.3)", cur, target)
		}
		next := filepath.Clean(filepath.Join(filepath.Dir(cur), target))
		if !pathWithin(root, next) {
			return "", nil, fmt.Errorf("symlink %s targets %s, which is outside the generation tree", cur, next)
		}
		cur = next
	}
}

// replaceSymlink replaces the single symlink at p (somewhere under root)
// with a wrapper or copy of its FULLY resolved target (resolveSymlinkChain),
// per despymlinkTree's doc comment. Only a node-named shebang is
// node-wrapped (finding 4); any other shebang, or no shebang at all
// (including an ELF target), is copied verbatim — it is already directly
// executable on its own.
func replaceSymlink(root, p string) error {
	finalPath, info, err := resolveSymlinkChain(root, p)
	if err != nil {
		return err
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
