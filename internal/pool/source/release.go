package source

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
)

// DefaultGitHubBaseURL is the public GitHub web host release assets are
// served from; NewRelease takes it (or a test server's URL) explicitly.
const DefaultGitHubBaseURL = "https://github.com"

// maxChecksumFileBytes bounds the checksum asset Resolve reads: it is a
// handful of lines in practice, so a larger body is a wrong or hostile
// endpoint, not a release.
const maxChecksumFileBytes = 1 << 20

// ReleaseConfig is the descriptor-derived configuration of one release-binary
// source (design/harness/omp-release-source.md §4, §6.1).
type ReleaseConfig struct {
	Repo        string                      // "owner/name", e.g. "can1357/oh-my-pi"
	VersionFeed string                      // npm package whose packument supplies versions
	Checksums   string                      // checksum asset name, e.g. "SHA256SUMS.txt"
	Assets      map[pool.PlatformKey]string // platform -> release asset name
}

// Release implements pool artifact acquisition for a release-binary source
// (omp-release-source.md §4): one binary per platform attached to a GitHub
// release and checksummed by a checksum asset of the same release. Versions
// and publish times come from an npm packument (§4.1), the checksum file is
// the lock (§4.2).
//
// Like Archive, EVERY method is pure Go and host-side and never invokes the
// caller-supplied backend.Backend (§4); it is accepted only for Source
// interface parity. It satisfies pool.Source structurally. The zero value is
// not usable; construct with NewRelease.
type Release struct {
	cfg        ReleaseConfig
	githubBase string
	// feed supplies Refresh: the NPM source's packument fetch, parsing and
	// prerelease/yank handling, reused verbatim (§4.1).
	feed *NPM
	// metadataClient and downloadClient mirror NPM's injectable HTTP seams:
	// production code never sets them directly, but a same-package test can
	// point them at httptest servers (including a TLS one, since the lock
	// accepts https URLs only).
	metadataClient *http.Client
	downloadClient *http.Client
	progress       io.Writer
}

// SetProgress makes Fetch report each artifact download to w, mirroring
// NPM.SetProgress. A nil w (the default) disables reporting.
func (r *Release) SetProgress(w io.Writer) {
	r.progress = w
	r.feed.SetProgress(w)
}

// NewRelease constructs a Release source. Empty githubBaseURL/registryBaseURL
// default to DefaultGitHubBaseURL and the public npm registry (as NewNPM
// does); non-empty values are used verbatim minus a trailing slash — chiefly
// a test seam. cfg is validated: every field non-empty, at least one asset,
// every platform key a member of the closed enum.
func NewRelease(cfg ReleaseConfig, githubBaseURL, registryBaseURL string) (*Release, error) {
	if parts := strings.Split(cfg.Repo, "/"); len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("release config: repo %q must be of the form owner/name", cfg.Repo)
	}
	if cfg.VersionFeed == "" {
		return nil, fmt.Errorf("release config %q: version feed must not be empty", cfg.Repo)
	}
	if cfg.Checksums == "" {
		return nil, fmt.Errorf("release config %q: checksum asset name must not be empty", cfg.Repo)
	}
	if len(cfg.Assets) == 0 {
		return nil, fmt.Errorf("release config %q: at least one platform asset is required", cfg.Repo)
	}
	assets := make(map[pool.PlatformKey]string, len(cfg.Assets))
	for key, name := range cfg.Assets {
		if _, err := pool.ParsePlatformKey(string(key)); err != nil {
			return nil, fmt.Errorf("release config %q: %w", cfg.Repo, err)
		}
		if name == "" {
			return nil, fmt.Errorf("release config %q: platform %s has an empty asset name", cfg.Repo, key)
		}
		assets[key] = name
	}
	cfg.Assets = assets
	if githubBaseURL == "" {
		githubBaseURL = DefaultGitHubBaseURL
	}
	return &Release{
		cfg:            cfg,
		githubBase:     strings.TrimRight(githubBaseURL, "/"),
		feed:           NewNPM(registryBaseURL),
		metadataClient: &http.Client{Timeout: metadataTimeout},
		downloadClient: &http.Client{Timeout: downloadTimeout},
	}, nil
}

// Refresh lists the versions of the descriptor's npm VersionFeed as
// candidates with their genuine publish times (§4.1). pkg is the release
// repo and must equal the configured one: the pool addresses a release
// harness by its repo (Package), and a mismatch is a wiring bug.
func (r *Release) Refresh(ctx context.Context, pkg string) ([]pool.Candidate, error) {
	if pkg != r.cfg.Repo {
		return nil, fmt.Errorf("release refresh: package %q does not match configured repo %q", pkg, r.cfg.Repo)
	}
	return r.feed.Refresh(ctx, r.cfg.VersionFeed)
}

// releaseBaseURL is the download directory of tag in the configured repo.
func (r *Release) releaseBaseURL(tag string) string {
	return r.githubBase + "/" + r.cfg.Repo + "/releases/download/" + tag
}

// parseChecksumFile parses a SHA256SUMS-style file strictly (§4.2): every
// non-empty line is `<64 lowercase hex><two spaces><name>`. A malformed line,
// or a name listed twice with different digests, is an error; an identical
// duplicate is tolerated.
func parseChecksumFile(r io.Reader) (map[string]string, error) {
	sums := make(map[string]string)
	sc := bufio.NewScanner(r)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			continue
		}
		if len(line) < 67 || line[64:66] != "  " || !validSHA256Hex(line[:64]) || strings.TrimSpace(line[66:]) != line[66:] {
			return nil, fmt.Errorf("checksum file line %d is malformed (want <64 lowercase hex><two spaces><name>)", lineNo)
		}
		digest, name := line[:64], line[66:]
		if prev, dup := sums[name]; dup && prev != digest {
			return nil, fmt.Errorf("checksum file lists %q twice with different digests (%s, %s)", name, prev, digest)
		}
		sums[name] = digest
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading checksum file: %w", err)
	}
	return sums, nil
}

// Resolve locks version from the release's checksum file (§4.2): the file's
// digests are both the upstream integrity values and the pool's cache
// addresses. b, workDir and nodeVersion are ignored. PublishedAt is left zero
// — the orchestrator fills it from the candidate's first-seen time, exactly
// as for Archive.Resolve.
//
// A platform whose asset is absent from the file is omitted, not an error;
// zero mapped platforms, or a 404 on the file itself, fails with a message
// that names both "assets not published yet" and "release predates checksum
// publication", since neither is a corrupt release.
func (r *Release) Resolve(ctx context.Context, b backend.Backend, pkg, version, workDir, nodeVersion, recipeRevision string) (pool.LockHarness, error) {
	if pkg != r.cfg.Repo {
		return pool.LockHarness{}, fmt.Errorf("release resolve: package %q does not match configured repo %q", pkg, r.cfg.Repo)
	}
	tag := pool.ReleaseTagPrefix + version
	sumsURL := r.releaseBaseURL(tag) + "/" + r.cfg.Checksums

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumsURL, nil)
	if err != nil {
		return pool.LockHarness{}, fmt.Errorf("building request for %s: %w", sumsURL, err)
	}
	resp, err := r.metadataClient.Do(req)
	if err != nil {
		return pool.LockHarness{}, fmt.Errorf("release resolve %s@%s: fetching %s: %w", pkg, version, sumsURL, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return pool.LockHarness{}, fmt.Errorf(
			"release resolve %s@%s: %s not found (HTTP 404): the release assets may not be published yet (retry later), "+
				"or this release predates checksum publication and cannot be locked by this source",
			pkg, version, sumsURL)
	default:
		return pool.LockHarness{}, fmt.Errorf("release resolve %s@%s: fetching %s: unexpected status %s", pkg, version, sumsURL, resp.Status)
	}
	sums, err := parseChecksumFile(io.LimitReader(resp.Body, maxChecksumFileBytes))
	if err != nil {
		return pool.LockHarness{}, fmt.Errorf("release resolve %s@%s: %s: %w", pkg, version, sumsURL, err)
	}

	artifacts := make(map[pool.PlatformKey]pool.LockReleaseArtifact)
	for key, asset := range r.cfg.Assets {
		digest, ok := sums[asset]
		if !ok {
			continue
		}
		artifacts[key] = pool.LockReleaseArtifact{URL: r.releaseBaseURL(tag) + "/" + asset, SHA256: digest}
	}
	if len(artifacts) == 0 {
		return pool.LockHarness{}, fmt.Errorf(
			"release resolve %s@%s: %s lists none of the configured platform assets: the release assets may not be published yet (retry later), "+
				"or this release predates checksum publication / the configured asset names",
			pkg, version, sumsURL)
	}

	return pool.LockHarness{
		Source:         "release",
		Package:        pkg,
		Version:        version,
		PublishedAt:    pool.FormatTimestamp(time.Time{}),
		FirstSeenAt:    pool.FormatTimestamp(time.Time{}),
		RecipeRevision: recipeRevision,
		Release:        &pool.LockRelease{Repo: r.cfg.Repo, Tag: tag, Artifacts: artifacts},
	}, nil
}

// Fetch downloads each locked artifact whose platform is in targets into
// cacheDir (the flat pool.CacheSHA256Dir()), verifying its sha256 while it
// streams; a mismatching download is never admitted (§4.3). An existing cache
// object is re-hashed and reused without any request. Like Archive.Fetch,
// empty targets is an error (platform-scoped-fetch.md). h is not mutated:
// the lock already carries each cache address.
func (r *Release) Fetch(ctx context.Context, h *pool.LockHarness, cacheDir string, targets []pool.PlatformKey) error {
	if h == nil {
		return fmt.Errorf("fetch: nil LockHarness")
	}
	if len(targets) == 0 {
		return fmt.Errorf("fetch: no target platforms given")
	}
	if h.Release == nil {
		return fmt.Errorf("fetch: %s: lock has no release artifacts (this Source only fetches release harnesses)", h.Package)
	}
	keys := make([]string, 0, len(h.Release.Artifacts))
	for key := range h.Release.Artifacts {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	for _, k := range keys {
		key := pool.PlatformKey(k)
		if !slices.Contains(targets, key) {
			continue
		}
		artifact := h.Release.Artifacts[key]
		if artifact.URL == "" {
			return fmt.Errorf("release artifact %s: no URL recorded in lock", key)
		}
		if err := r.downloadVerify(ctx, fmt.Sprintf("%s (%s)", h.Package, key), artifact.URL, artifact.SHA256, cacheDir); err != nil {
			return fmt.Errorf("release artifact %s: %w", key, err)
		}
	}
	return nil
}

// downloadVerify admits url's content to cacheDir/<wantSHA256> iff it hashes
// to wantSHA256 (temp file, verify, same-volume rename — the discipline of
// NPM.downloadVerify and Archive.downloadVerify). A cache object already
// present under that name is re-hashed and reused when it matches; one that
// no longer matches is treated as absent and replaced.
func (r *Release) downloadVerify(ctx context.Context, label, url, wantSHA256, cacheDir string) error {
	if !validSHA256Hex(wantSHA256) {
		return fmt.Errorf("malformed sha256 %q recorded in lock", wantSHA256)
	}
	dest := filepath.Join(cacheDir, wantSHA256)
	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		if _, got, err := hashFile(dest); err == nil && got == wantSHA256 {
			return nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building request for %s: %w", url, err)
	}
	resp, err := r.downloadClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: unexpected status %s", url, resp.Status)
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("creating cache dir %s: %w", cacheDir, err)
	}
	tmp, err := os.CreateTemp(cacheDir, ".wraptool-pool-fetch-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", cacheDir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h, newDownloadProgress(r.progress, label, resp.ContentLength)), resp.Body); err != nil {
		cleanup()
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
		cleanup()
		return fmt.Errorf("integrity mismatch downloading %s: want sha256 %s, got %s", url, wantSHA256, got)
	}
	if err := tmp.Chmod(0o644); err != nil {
		cleanup()
		return fmt.Errorf("setting permissions on temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsyncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming %s into cache: %w", url, err)
	}
	// Parent-directory fsync makes the rename durable; skipped on Windows,
	// which has no equivalent (§6.4).
	if runtime.GOOS != "windows" {
		if dirFile, err := os.Open(cacheDir); err == nil {
			_ = dirFile.Sync()
			_ = dirFile.Close()
		}
	}
	return nil
}

// Realize builds one platform generation: the cached binary placed at
// <stagingGenDir>/bin/<binName> with mode 0o755 (§4.4), plus a receipt with
// backend "none", exactly as Archive.Realize. b and ctx are accepted for
// interface parity only. The placement is a hardlink where possible (the
// chmod therefore also marks the cache object executable, which is harmless:
// cache verification is content-based).
func (r *Release) Realize(ctx context.Context, b backend.Backend, harnessName, binName string, h *pool.LockHarness, key pool.PlatformKey, cacheDir, stagingGenDir string) (*pool.Receipt, error) {
	if h == nil {
		return nil, fmt.Errorf("realize: nil LockHarness")
	}
	if harnessName == "" || binName == "" {
		return nil, fmt.Errorf("realize: harnessName and binName must both be non-empty")
	}
	if binName != path.Base(binName) || binName == "." || binName == ".." {
		return nil, fmt.Errorf("realize: %s: binName %q must be a bare file name", harnessName, binName)
	}
	if h.Release == nil {
		return nil, fmt.Errorf("realize: %s: lock has no release artifacts (this Source only realizes release harnesses)", harnessName)
	}
	if _, err := pool.ParsePlatformKey(string(key)); err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}
	artifact, ok := h.Release.Artifacts[key]
	if !ok {
		recorded := make([]string, 0, len(h.Release.Artifacts))
		for k := range h.Release.Artifacts {
			recorded = append(recorded, string(k))
		}
		sort.Strings(recorded)
		if len(recorded) == 0 {
			recorded = []string{"none"}
		}
		return nil, fmt.Errorf("realize: %s: no artifact recorded for platform %q (lock records: %s)",
			harnessName, key, strings.Join(recorded, ", "))
	}
	if err := verifyCacheObject(cacheDir, artifact.SHA256, fmt.Sprintf("release artifact for platform %q", key)); err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}

	binDir := filepath.Join(stagingGenDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return nil, fmt.Errorf("realize: %s: creating staging bin dir: %w", harnessName, err)
	}
	dst := filepath.Join(binDir, binName)
	if err := hardlinkOrCopy(filepath.Join(cacheDir, artifact.SHA256), dst); err != nil {
		return nil, fmt.Errorf("realize: %s: placing %s: %w", harnessName, binName, err)
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		return nil, fmt.Errorf("realize: %s: setting mode on %s: %w", harnessName, binName, err)
	}

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
		Backend:        pool.ReceiptBackend{Name: "none"},
		Files:          files,
		ManifestDigest: manifestDigest,
	}
	if err := receipt.Validate(); err != nil {
		return nil, fmt.Errorf("realize: %s: assembling receipt: %w", harnessName, err)
	}
	return receipt, nil
}
