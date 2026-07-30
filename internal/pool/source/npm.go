// Package source implements the harness-pool artifact-acquisition seam
// (design/unified-harness-pool.md §11.3) for one package family. NPM is the
// npm.js implementation: Refresh and Fetch are pure Go and host-side (§6.2);
// Resolve runs `npm install --package-lock-only` inside a caller-supplied
// backend.Backend, since only npm itself can compute a correct transitive
// dependency graph and lockfile.
//
// This package owns no orchestration: no eligibility evaluation, no
// Observations/PoolLock writes, no realize/generation logic, no CLI wiring.
// It produces pool.Candidate and pool.LockHarness values and downloads the
// artifacts a LockHarness already names — nothing more.
package source

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
)

const (
	// defaultRegistryBaseURL is the public npm registry, used when NewNPM is
	// given an empty base URL.
	defaultRegistryBaseURL = "https://registry.npmjs.org"
	// defaultNodeDistBaseURL is where nodejs.org publishes release tarballs
	// and their SHASUMS256.txt digests.
	defaultNodeDistBaseURL = "https://nodejs.org/dist"
	// npmResolveContainerDir is the container-side scratch directory Resolve
	// mounts read-write for `npm install --package-lock-only` (§8.1 steps
	// 1-2). It is a fixed path inside the resolve container only — never a
	// host path, never exposed to a project container.
	npmResolveContainerDir = "/wraptool-npm-resolve"
)

// NPM implements pool artifact acquisition for the "npm" source (§8.1). The
// zero value is not usable; construct with NewNPM.
type NPM struct {
	registryBaseURL string
	nodeDistBaseURL string
	// httpClient is the injectable HTTP seam: production code never sets it
	// directly (NewNPM defaults it to http.DefaultClient), but a same-package
	// test can construct an NPM literal with a client pointed at an
	// httptest.Server, or override nodeDistBaseURL, so no test ever talks to
	// the real npm registry or nodejs.org.
	httpClient *http.Client
}

// NewNPM constructs an NPM source. An empty registryBaseURL defaults to the
// public npm registry (https://registry.npmjs.org); a non-empty value is
// used verbatim (minus a trailing slash) — chiefly a test seam, since
// production callers have no reason to point at a different registry today.
func NewNPM(registryBaseURL string) *NPM {
	if registryBaseURL == "" {
		registryBaseURL = defaultRegistryBaseURL
	}
	return &NPM{
		registryBaseURL: strings.TrimRight(registryBaseURL, "/"),
		nodeDistBaseURL: defaultNodeDistBaseURL,
		httpClient:      http.DefaultClient,
	}
}

// npmPackument is the subset of an npm registry packument (the full
// document, not the abbreviated/corgi format — that omits "time", which
// Refresh needs) this package reads. Versions is decoded as raw messages:
// this package only needs to know a version key exists, never its manifest
// content.
type npmPackument struct {
	Time     map[string]string          `json:"time"`
	Versions map[string]json.RawMessage `json:"versions"`
}

// registryURL builds the packument request URL for pkg. A scoped package
// name's "/" is percent-encoded, matching what npm clients send for scoped
// packages against the public registry.
func (n *NPM) registryURL(pkg string) string {
	return n.registryBaseURL + "/" + strings.ReplaceAll(pkg, "/", "%2f")
}

// fetchPackument performs the one GET this package ever issues against the
// registry's package endpoint, shared by Refresh (candidate listing) and
// Resolve (published_at lookup for the chosen version).
func (n *NPM) fetchPackument(ctx context.Context, pkg string) (*npmPackument, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.registryURL(pkg), nil)
	if err != nil {
		return nil, fmt.Errorf("building npm registry request for %s: %w", pkg, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching npm packument for %s: %w", pkg, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching npm packument for %s: unexpected status %s", pkg, resp.Status)
	}

	var doc npmPackument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decoding npm packument for %s: %w", pkg, err)
	}
	return &doc, nil
}

// Refresh lists every version npm's packument mentions for pkg, as
// pool.Candidate values ready for eligibility evaluation (§9). It is pure
// Go, host-side, and performs exactly one HTTP GET (§6.2: Refresh needs no
// backend).
//
// PublishedAt comes from the packument's "time" map; a version missing from
// "time" entirely, or carrying a value pool.ParseTimestamp cannot parse,
// yields a zero PublishedAt rather than an invented one — pool.Eligible
// already treats a zero PublishedAt as unconditionally ineligible, which is
// the intended safety net (design/unified-harness-pool.md §9).
//
// npm has no first-class "yank". A version present in "time" but absent from
// the packument's "versions" map has been unpublished, and Refresh reports
// it Yanked.
//
// FirstSeenAt is always the zero time on every returned Candidate: this
// package has no notion of pool-local observation history. The orchestrator
// (not this package) owns Observations and fills FirstSeenAt in once a
// candidate has actually been seen locally (§6.6, §8.1).
func (n *NPM) Refresh(ctx context.Context, pkg string) ([]pool.Candidate, error) {
	doc, err := n.fetchPackument(ctx, pkg)
	if err != nil {
		return nil, err
	}

	// The set of known versions is the union of "versions" (currently
	// published) and "time" minus its two non-version keys (every version
	// ever published, including unpublished ones) — a version can
	// legitimately appear in only one of the two maps (§8.1 test fixtures:
	// "a missing time entry" and "an unpublished version").
	versions := make(map[string]struct{}, len(doc.Versions)+len(doc.Time))
	for v := range doc.Versions {
		versions[v] = struct{}{}
	}
	for v := range doc.Time {
		if v == "created" || v == "modified" {
			continue
		}
		versions[v] = struct{}{}
	}

	candidates := make([]pool.Candidate, 0, len(versions))
	for v := range versions {
		_, inVersions := doc.Versions[v]
		timeStr, inTime := doc.Time[v]

		var publishedAt time.Time
		if inTime {
			if t, err := pool.ParseTimestamp(timeStr); err == nil {
				publishedAt = t
			}
			// An unparsable value falls through with publishedAt left zero
			// rather than failing the whole Refresh over one bad entry.
		}

		prerelease := false
		if sv, err := semver.NewVersion(v); err == nil {
			prerelease = sv.Prerelease() != ""
		}

		candidates = append(candidates, pool.Candidate{
			Version:     v,
			PublishedAt: publishedAt,
			FirstSeenAt: time.Time{},
			Prerelease:  prerelease,
			Yanked:      inTime && !inVersions,
		})
	}

	// Map iteration order is randomized; sort for a deterministic result.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Version < candidates[j].Version })
	return candidates, nil
}

// versionPublishedAt looks up version's publish time with a fresh registry
// request (Resolve does not trust a caller-supplied Candidate, which may be
// stale by the time Resolve runs). It returns the zero time, not an error,
// when the registry has no time entry for version or the entry cannot be
// parsed — the same "missing/unparsable date yields zero" rule Refresh uses.
func (n *NPM) versionPublishedAt(ctx context.Context, pkg, version string) (time.Time, error) {
	doc, err := n.fetchPackument(ctx, pkg)
	if err != nil {
		return time.Time{}, err
	}
	timeStr, ok := doc.Time[version]
	if !ok {
		return time.Time{}, nil
	}
	t, err := pool.ParseTimestamp(timeStr)
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

// npmLockFile is the subset of package-lock.json (lockfileVersion 3) this
// package reads.
type npmLockFile struct {
	LockfileVersion int                            `json:"lockfileVersion"`
	Packages        map[string]npmLockPackageEntry `json:"packages"`
}

// npmLockPackageEntry is one "packages" map entry.
type npmLockPackageEntry struct {
	Version   string `json:"version"`
	Resolved  string `json:"resolved"`
	Integrity string `json:"integrity"`
}

// nodeGlibcPlatforms maps the two glibc PlatformKeys this method realizes
// artifacts for to nodejs.org's dist arch tag. musl realization is deferred
// (design/unified-harness-pool.md §5.1): nodejs.org publishes no official
// musl/Alpine tarball, so linux-amd64-musl and linux-arm64-musl are
// intentionally absent from the returned artifact map — a switch over
// pool.PlatformKey would force the exhaustive linter to name all four keys,
// which is exactly the deferral this map exists to avoid encoding.
var nodeGlibcPlatforms = map[pool.PlatformKey]string{
	pool.PlatformLinuxAMD64Glibc: "linux-x64",
	pool.PlatformLinuxARM64Glibc: "linux-arm64",
}

// Resolve produces the complete lock entry for one exact pkg@version by
// running `npm install --package-lock-only --ignore-scripts` inside b (a
// Linux backend container with registry-only network, §6.2/§8.1) and
// reading back the resulting package-lock.json. b is the caller's choice —
// this method neither constructs a Backend nor selects guix/OCI.
//
// nodeVersion is the exact Node runtime version the harness descriptor
// requires; the descriptor itself lives in internal/harness, which this
// package does not import, so the caller resolves the descriptor's runtime
// requirement to a concrete version and passes it through. recipeRevision is
// the orchestrator-computed "sha256:<hex>" value (§8: recipe hashing is the
// orchestrator's responsibility, not this Source's); Resolve stores it on
// the returned LockHarness verbatim.
//
// PublishedAt is populated from a fresh registry lookup (not a
// caller-supplied Candidate, which may be stale) and is the zero-formatted
// timestamp when the registry has no time entry for version.
//
// FirstSeenAt is always the zero-formatted timestamp on the returned value;
// the orchestrator fills it in from Observations before persisting the lock.
// Resolve deliberately never calls (*pool.Lock).Validate — a LockHarness
// with a zero FirstSeenAt is expected and legitimate at this stage, and
// Validate would reject it.
func (n *NPM) Resolve(ctx context.Context, b backend.Backend, pkg, version, workDir, nodeVersion, recipeRevision string) (pool.LockHarness, error) {
	scratchDir := filepath.Join(workDir, "npm-resolve")
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		return pool.LockHarness{}, fmt.Errorf("creating npm resolve scratch dir: %w", err)
	}

	// §8.1 step 1: synthesize package.json depending on the exact version.
	manifest := struct {
		Dependencies map[string]string `json:"dependencies"`
	}{Dependencies: map[string]string{pkg: version}}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return pool.LockHarness{}, fmt.Errorf("encoding synthesized package.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(scratchDir, "package.json"), manifestBytes, 0o644); err != nil {
		return pool.LockHarness{}, fmt.Errorf("writing synthesized package.json: %w", err)
	}

	// §8.1 step 2: resolve the lockfile inside the backend container.
	spec := backend.RunSpec{
		Argv:    []string{"npm", "install", "--package-lock-only", "--ignore-scripts"},
		Mounts:  []backend.Mount{{Host: scratchDir, Container: npmResolveContainerDir, ReadOnly: false}},
		Network: true,
		Workdir: npmResolveContainerDir,
	}
	if err := b.Run(ctx, spec); err != nil {
		return pool.LockHarness{}, fmt.Errorf("npm install --package-lock-only: %w", err)
	}

	// §8.1 step 3: read back package-lock.json.
	lockBytes, err := os.ReadFile(filepath.Join(scratchDir, "package-lock.json"))
	if err != nil {
		return pool.LockHarness{}, fmt.Errorf("reading package-lock.json: %w", err)
	}
	var lockFile npmLockFile
	if err := json.Unmarshal(lockBytes, &lockFile); err != nil {
		return pool.LockHarness{}, fmt.Errorf("parsing package-lock.json: %w", err)
	}
	if lockFile.LockfileVersion != 3 {
		return pool.LockHarness{}, fmt.Errorf("package-lock.json: unsupported lockfileVersion %d (want 3)", lockFile.LockfileVersion)
	}

	packages := make(map[string]pool.NPMLockPackage, len(lockFile.Packages))
	for name, entry := range lockFile.Packages {
		if name == "" {
			// The "" key is the synthesized project root itself, not a
			// dependency — it has no resolved/integrity by construction and
			// is not part of the harness's dependency graph.
			continue
		}
		if entry.Resolved == "" && entry.Integrity == "" {
			return pool.LockHarness{}, fmt.Errorf(
				"package-lock.json entry %q has neither resolved nor integrity (git dependency, workspace link, or file: dependency cannot be locked)",
				name)
		}
		packages[name] = pool.NPMLockPackage{
			Version:   entry.Version,
			Resolved:  entry.Resolved,
			Integrity: entry.Integrity,
		}
	}

	// §8.1 step 4: pinned runtime requirement + per-platform artifacts.
	publishedAt, err := n.versionPublishedAt(ctx, pkg, version)
	if err != nil {
		return pool.LockHarness{}, err
	}
	runtimeArtifacts, err := n.nodeRuntimeArtifacts(ctx, nodeVersion)
	if err != nil {
		return pool.LockHarness{}, err
	}

	return pool.LockHarness{
		Source:         "npm",
		Package:        pkg,
		Version:        version,
		PublishedAt:    pool.FormatTimestamp(publishedAt),
		FirstSeenAt:    pool.FormatTimestamp(time.Time{}),
		RecipeRevision: recipeRevision,
		Runtime: &pool.LockRuntime{
			Name:      "node",
			Version:   nodeVersion,
			Artifacts: runtimeArtifacts,
		},
		NPMLock: &pool.NPMLock{
			LockfileVersion: lockFile.LockfileVersion,
			Packages:        packages,
		},
	}, nil
}

// nodeRuntimeArtifacts fetches nodeVersion's SHASUMS256.txt and returns the
// glibc-platform artifact map (URL + sha256) described on nodeGlibcPlatforms.
func (n *NPM) nodeRuntimeArtifacts(ctx context.Context, nodeVersion string) (map[pool.PlatformKey]pool.LockRuntimeArtifact, error) {
	shasums, err := n.fetchNodeSHASUMS(ctx, nodeVersion)
	if err != nil {
		return nil, err
	}

	artifacts := make(map[pool.PlatformKey]pool.LockRuntimeArtifact, len(nodeGlibcPlatforms))
	for key, archTag := range nodeGlibcPlatforms {
		filename := fmt.Sprintf("node-v%s-%s.tar.xz", nodeVersion, archTag)
		sha256Hex, ok := shasums[filename]
		if !ok {
			return nil, fmt.Errorf("SHASUMS256.txt for node v%s has no entry for %s", nodeVersion, filename)
		}
		artifacts[key] = pool.LockRuntimeArtifact{
			URL:    fmt.Sprintf("%s/v%s/%s", n.nodeDistBaseURL, nodeVersion, filename),
			SHA256: sha256Hex,
		}
	}
	return artifacts, nil
}

// fetchNodeSHASUMS downloads and parses nodejs.org's SHASUMS256.txt for
// nodeVersion: lines of "<sha256>  <filename>" (a leading "*" on the
// filename, used for binary-mode checksums, is stripped).
func (n *NPM) fetchNodeSHASUMS(ctx context.Context, nodeVersion string) (map[string]string, error) {
	u := fmt.Sprintf("%s/v%s/SHASUMS256.txt", n.nodeDistBaseURL, nodeVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", u, err)
	}
	resp, err := n.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: unexpected status %s", u, resp.Status)
	}

	sums := make(map[string]string)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		sums[strings.TrimPrefix(fields[1], "*")] = fields[0]
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", u, err)
	}
	return sums, nil
}

// parseIntegrity splits an npm SRI string ("sha512-<base64>") into its
// algorithm and decoded digest.
func parseIntegrity(sri string) (algo string, digest []byte, err error) {
	algo, encoded, ok := strings.Cut(sri, "-")
	if !ok {
		return "", nil, fmt.Errorf("malformed integrity value %q", sri)
	}
	digest, err = base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", nil, fmt.Errorf("decoding integrity value %q: %w", sri, err)
	}
	return algo, digest, nil
}

// Fetch downloads every artifact h names — every npm_lock package's
// "resolved" tarball and every runtime artifact — into cacheDir, verifying
// integrity during a single streaming pass before the object is admitted
// (§6.2, §17 "integrity mismatch rejection"). It is pure Go, host-side, and
// never consults any URL that is not already present on h: an unlisted URL
// is a bug by definition (§6.2), not a fallback case this method handles.
//
// An npm package entry is verified against its upstream sha512 SRI
// Integrity; its Sha256 field is filled in when empty, or checked for an
// exact match when already set. A runtime artifact has no SRI and is
// verified against its already-recorded Sha256 (populated by Resolve from
// nodejs.org's SHASUMS256.txt). A cacheDir object already present at the
// expected sha256-named path (size > 0) is trusted as-is and the download is
// skipped entirely — content-addressed storage means a name match already is
// a content match (§6.4).
func (n *NPM) Fetch(ctx context.Context, h *pool.LockHarness, cacheDir string) error {
	if h == nil {
		return fmt.Errorf("fetch: nil LockHarness")
	}

	if h.NPMLock != nil {
		for name, pkg := range h.NPMLock.Packages {
			if pkg.Resolved == "" {
				return fmt.Errorf("npm package %q: no resolved URL recorded in lock", name)
			}
			sha256Hex, err := n.downloadVerify(ctx, pkg.Resolved, pkg.Integrity, pkg.SHA256, cacheDir)
			if err != nil {
				return fmt.Errorf("npm package %q: %w", name, err)
			}
			if pkg.SHA256 == "" {
				pkg.SHA256 = sha256Hex
				h.NPMLock.Packages[name] = pkg
			}
		}
	}

	if h.Runtime != nil {
		for key, artifact := range h.Runtime.Artifacts {
			if artifact.URL == "" {
				return fmt.Errorf("runtime artifact %s: no URL recorded in lock", key)
			}
			if _, err := n.downloadVerify(ctx, artifact.URL, "", artifact.SHA256, cacheDir); err != nil {
				return fmt.Errorf("runtime artifact %s: %w", key, err)
			}
		}
	}

	return nil
}

// downloadVerify downloads url, streaming it simultaneously into a temp file
// under cacheDir and a sha256 hash (always) plus a sha512 hash (only when
// sri is non-empty). It returns the downloaded object's hex sha256 digest.
//
//   - If knownSHA256 is non-empty and cacheDir already holds an object at
//     that name with size > 0, the object is trusted and no request is made.
//   - If sri is non-empty, the streamed sha512 must match its decoded digest
//     or the temp file is discarded and an error is returned — nothing is
//     admitted to cacheDir on a mismatch.
//   - If knownSHA256 is non-empty, the streamed sha256 must equal it or the
//     temp file is discarded and an error is returned.
//   - On success the temp file is fsynced and renamed to cacheDir/<sha256hex>
//     (same directory, so the rename stays on one volume — the same
//     discipline as pool.WriteFileAtomic, but streaming rather than
//     whole-buffer since artifacts can be large).
func (n *NPM) downloadVerify(ctx context.Context, url, sri, knownSHA256, cacheDir string) (string, error) {
	if knownSHA256 != "" {
		if fi, err := os.Stat(filepath.Join(cacheDir, knownSHA256)); err == nil && fi.Size() > 0 {
			return knownSHA256, nil
		}
	}

	var wantSRI []byte
	if sri != "" {
		algo, digest, err := parseIntegrity(sri)
		if err != nil {
			return "", err
		}
		if algo != "sha512" {
			return "", fmt.Errorf("unsupported integrity algorithm %q (want sha512)", algo)
		}
		wantSRI = digest
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building request for %s: %w", url, err)
	}
	resp, err := n.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: unexpected status %s", url, resp.Status)
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("creating cache dir %s: %w", cacheDir, err)
	}
	tmp, err := os.CreateTemp(cacheDir, ".wraptool-pool-fetch-*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating temp file in %s: %w", cacheDir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}

	sha256h := sha256.New()
	writers := []io.Writer{tmp, sha256h}
	var sha512h = sha512.New()
	if sri != "" {
		writers = append(writers, sha512h)
	}
	if _, err := io.Copy(io.MultiWriter(writers...), resp.Body); err != nil {
		cleanup()
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}

	if sri != "" {
		if got := sha512h.Sum(nil); !bytes.Equal(got, wantSRI) {
			cleanup()
			return "", fmt.Errorf("integrity mismatch downloading %s: sha512 SRI does not match", url)
		}
	}

	sha256Hex := hex.EncodeToString(sha256h.Sum(nil))
	if knownSHA256 != "" && knownSHA256 != sha256Hex {
		cleanup()
		return "", fmt.Errorf("sha256 mismatch downloading %s: want %s, got %s", url, knownSHA256, sha256Hex)
	}

	if err := tmp.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("fsyncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("closing temp file: %w", err)
	}

	dest := filepath.Join(cacheDir, sha256Hex)
	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("renaming %s into cache: %w", url, err)
	}
	// Parent-directory fsync makes the rename itself durable, mirroring
	// pool.WriteFileAtomic; skipped on Windows, which has no directory-fsync
	// equivalent (§6.4).
	if runtime.GOOS != "windows" {
		if dirFile, err := os.Open(cacheDir); err == nil {
			_ = dirFile.Sync()
			_ = dirFile.Close()
		}
	}

	return sha256Hex, nil
}
