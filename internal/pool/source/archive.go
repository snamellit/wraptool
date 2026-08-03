package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
)

// Archive implements pool artifact acquisition for a direct-download
// ("archive") source (design/harness/unified-harness-pool.md §6.2, §8.1,
// §11.3): a fixed per-platform release manifest, and a versioned,
// integrity-verified .tar.gz artifact per platform. It is written against
// the Antigravity CLI ("agy") release channel, whose shape is: GET
// <base>/manifests/<platform>.json -> {"version","url","sha512"}, where url
// points at a stable, versioned .tar.gz containing the flat native binary.
//
// Unlike NPM, EVERY method here is pure Go, host-side, and NEVER invokes the
// caller-supplied backend.Backend (§6.2 phase table: "archive sources
// resolve in pure Go, no backend"; §11.4 last line: "Archive sources skip
// steps 1-3/5 and just unpack + wrap"). There is no transitive dependency
// graph to resolve inside a container and no lifecycle script to run
// offline — every platform's artifact is a single downloadable file,
// already fully built by upstream. Resolve and Realize accept a
// backend.Backend parameter purely for Source-interface-shape parity with
// NPM (see internal/pool.Source, which both satisfy structurally); this
// package's own tests assert zero calls into it across all four methods.
//
// This package owns no orchestration: no eligibility evaluation, no
// Observations/PoolLock writes, no CLI wiring — the same boundary NPM (in
// npm.go, this package) holds.
//
// The zero value is not usable; construct with NewArchive.
type Archive struct {
	manifestBaseURL string
	// metadataClient and downloadClient mirror NPM's injectable HTTP seams
	// (see NPM's doc comment): production code never sets them directly
	// (NewArchive defaults both), but a same-package test can construct an
	// Archive literal with clients pointed at httptest.Servers.
	metadataClient *http.Client
	downloadClient *http.Client
}

// defaultArchiveManifestBaseURL is the production Antigravity CLI release
// channel this package resolves against when NewArchive is given an empty
// base URL.
const defaultArchiveManifestBaseURL = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app"

// NewArchive constructs an Archive source. An empty manifestBaseURL defaults
// to the production Antigravity CLI channel (defaultArchiveManifestBaseURL);
// a non-empty value is used verbatim (minus a trailing slash) — chiefly a
// test seam pointed at an httptest.Server, mirroring NewNPM's
// registryBaseURL parameter.
func NewArchive(manifestBaseURL string) *Archive {
	if manifestBaseURL == "" {
		manifestBaseURL = defaultArchiveManifestBaseURL
	}
	return &Archive{
		manifestBaseURL: strings.TrimRight(manifestBaseURL, "/"),
		// metadataTimeout/downloadTimeout are npm.go's package-level
		// constants (this file lives in the same package, "source") — the
		// same stall-bound reasoning applies to this channel's tiny JSON
		// manifests and its larger .tar.gz artifacts respectively.
		metadataClient: &http.Client{Timeout: metadataTimeout},
		downloadClient: &http.Client{Timeout: downloadTimeout},
	}
}

// archivePlatformKeys is every PlatformKey this Source knows how to resolve
// a manifest for, in a fixed order — used wherever this file needs to
// iterate all four deterministically (Refresh's version collection,
// Resolve's per-platform locking loop).
var archivePlatformKeys = []pool.PlatformKey{
	pool.PlatformLinuxAMD64Glibc,
	pool.PlatformLinuxARM64Glibc,
	pool.PlatformLinuxAMD64Musl,
	pool.PlatformLinuxARM64Musl,
}

// archiveManifestPlatform maps a pool.PlatformKey to the manifest-path
// platform token this channel's per-platform manifest endpoint uses (GET
// <base>/manifests/<platform>.json). Exhaustive over PlatformKey's four
// values, as the project's exhaustive linter requires.
func archiveManifestPlatform(key pool.PlatformKey) (string, error) {
	switch key {
	case pool.PlatformLinuxAMD64Glibc:
		return "linux_amd64", nil
	case pool.PlatformLinuxARM64Glibc:
		return "linux_arm64", nil
	case pool.PlatformLinuxAMD64Musl:
		return "linux_amd64_musl", nil
	case pool.PlatformLinuxARM64Musl:
		return "linux_arm64_musl", nil
	default:
		return "", fmt.Errorf("unsupported platform key %q", key)
	}
}

// archiveManifest is the per-platform release manifest this channel
// publishes: GET <base>/manifests/<platform>.json ->
// {"version","url","sha512"}.
type archiveManifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA512  string `json:"sha512"` // hex-encoded, NOT npm-style base64 SRI
}

// sha512HexPattern is the only shape this package accepts for a manifest's
// "sha512" field: 128 lowercase hex characters (a raw SHA-512 digest,
// hex-encoded) — this channel publishes a bare hex digest, never a base64
// SRI string like npm's registry does (contrast npm.go's parseIntegrity).
var sha512HexPattern = regexp.MustCompile(`^[0-9a-f]{128}$`)

func validSHA512Hex(s string) bool {
	return sha512HexPattern.MatchString(s)
}

// validateManifest checks one fetched archiveManifest's shape: a non-empty
// version, a non-empty artifact URL, and a well-formed sha512 hex digest.
// url is the manifest's OWN request URL, used only to make an error message
// name which manifest failed.
//
// This does NOT enforce the artifact URL's scheme (https): that check is
// lock.go's Lock.Validate/validateArchiveArtifact's job (§8 ruling 1, "URL
// https"), applied to a LockArchiveArtifact once Resolve has locked it —
// not to every manifest this package merely reads. Production manifests are
// always https in practice (the work package brief's live endpoint), but
// pinning that here as well would make this package untestable against a
// plain-http httptest.Server, exactly the seam every other Source/backend
// test in this codebase relies on (NewNPM's own registryBaseURL/
// nodeDistBaseURL test seams are equally scheme-agnostic).
func validateManifest(url string, m *archiveManifest) error {
	if m.Version == "" {
		return fmt.Errorf("manifest %s: missing %q", url, "version")
	}
	if m.URL == "" {
		return fmt.Errorf("manifest %s: missing %q", url, "url")
	}
	if !validSHA512Hex(m.SHA512) {
		return fmt.Errorf("manifest %s: %q must be 128 lowercase hex characters", url, "sha512")
	}
	return nil
}

// fetchManifest performs the one GET this package ever issues per platform:
// <manifestBaseURL>/manifests/<platform>.json (§6.2: Refresh/Resolve need no
// backend).
func (a *Archive) fetchManifest(ctx context.Context, key pool.PlatformKey) (*archiveManifest, error) {
	platform, err := archiveManifestPlatform(key)
	if err != nil {
		return nil, err
	}
	u := a.manifestBaseURL + "/manifests/" + platform + ".json"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", u, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := a.metadataClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: unexpected status %s", u, resp.Status)
	}

	var m archiveManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decoding manifest %s: %w", u, err)
	}
	if err := validateManifest(u, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Refresh lists the version(s) currently published on this channel's
// per-platform manifests, as pool.Candidate values ready for eligibility
// evaluation (§9). It is pure Go, host-side (§6.2), issuing one GET per
// PlatformKey manifest and returning one Candidate per DISTINCT version seen
// (normally exactly one — the channel exposes only its latest release).
//
// pkg is accepted only to satisfy the Source interface's per-package
// Refresh shape (NPM multiplexes many npm packages over one registry); this
// channel has no separate package identity — every platform's manifest
// describes the SAME release — so pkg is not used to select or filter
// anything, only echoed into error messages.
//
// RULING (§9, §8.1): this channel's manifest has no publish-history field,
// only "version" — there is nothing to derive a publication timestamp from.
// Every returned Candidate therefore carries a ZERO PublishedAt; this
// method deliberately does NOT invent one. §9's minimum_release_age is
// consequently NOT the mechanism that quarantines an archive candidate —
// eligibility for an archive source instead rides on this pool's own
// write-once first-seen observations (§6.6 first_seen_at, filled in by the
// orchestrator, never by this Source): minimum_release_age should be read,
// for an archive-sourced harness, as evaluated against first_seen_at rather
// than a (nonexistent) published_at — a conservative proxy, since first
// observation can only ever be at or after actual publication. This is a
// documented property of the CALLER's eligibility evaluation, not something
// this method enforces; wiring pool.Eligible/eligibility.go to that reading
// is this work package's explicit non-goal (see this package's top-level
// doc comment and the work package brief).
//
// A platform whose manifest cannot be fetched or fails validation is simply
// excluded from this call's results, not treated as a hard failure of the
// whole Refresh — mirroring Resolve's "a mid-rollout platform is simply
// absent" tolerance (§8.1 ruling 3). Refresh fails outright only when NOT
// ONE of the four platforms could be reached, aggregating every platform's
// error via errors.Join for diagnosis.
func (a *Archive) Refresh(ctx context.Context, pkg string) ([]pool.Candidate, error) {
	versions := make(map[string]struct{})
	var errs []error
	for _, key := range archivePlatformKeys {
		m, err := a.fetchManifest(ctx, key)
		if err != nil {
			errs = append(errs, fmt.Errorf("platform %s: %w", key, err))
			continue
		}
		versions[m.Version] = struct{}{}
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("archive refresh %s: no platform manifest was reachable: %w", pkg, errors.Join(errs...))
	}

	names := make([]string, 0, len(versions))
	for v := range versions {
		names = append(names, v)
	}
	sort.Strings(names) // map iteration order is randomized; sort for a deterministic result

	candidates := make([]pool.Candidate, 0, len(names))
	for _, v := range names {
		prerelease := false
		if sv, err := semver.NewVersion(v); err == nil {
			prerelease = sv.Prerelease() != ""
		}
		candidates = append(candidates, pool.Candidate{
			Version:     v,
			PublishedAt: time.Time{}, // no publish-history field on this channel — see RULING above
			FirstSeenAt: time.Time{}, // orchestrator fills this in from Observations (§6.6)
			Prerelease:  prerelease,
			Yanked:      false, // this channel has no yank/unpublish concept
		})
	}
	return candidates, nil
}

// Resolve produces the complete lock entry for one exact version by
// re-fetching every platform's manifest (pure Go, no backend) and locking
// that platform's artifact for EVERY platform whose manifest currently
// reports EXACTLY version (§8.1: "Archive/direct-download source (pure Go,
// no backend): resolve version from the adapter's release metadata
// endpoint; record per-platform final URL + sha256" — sha512 here; see
// LockArchiveArtifact's doc comment for the hex-vs-SRI distinction). A
// platform whose manifest reports a DIFFERENT version (a mid-rollout
// platform, §8.1 ruling 3) — or whose manifest could not be fetched or
// failed validation at all — is simply absent from the returned
// LockHarness.Archive.Artifacts; a later `sync` for that platform fails
// with the existing "no artifact recorded" message, which is the intended,
// unchanged UX (see Realize's "no artifact recorded" error below). Resolve
// fails outright only when NOT ONE platform reports version, aggregating
// every platform's reason (fetch failure or version mismatch) into one
// error.
//
// b, workDir, and nodeVersion are accepted only to satisfy the Source
// interface's shape, pinned to match NPM.Resolve's signature so both
// implementations satisfy internal/pool.Source structurally without either
// package importing the other. Archive resolution is pure Go and
// single-request per platform, so none of the three is used:
//   - b MUST NEVER be called — §6.2's phase table marks archive resolve as
//     needing no backend at all, unlike npm's Resolve (which drives `npm
//     install` inside one). This package's own tests assert this with a
//     fake Backend that t.Fatals on any method call.
//   - workDir would be npm's scratch directory for a synthesized
//     package.json/package-lock.json (§8.1 steps 1-3); there is nothing to
//     synthesize here.
//   - nodeVersion: an archive-sourced harness bundles no separate language
//     runtime — the artifact IS the executable — so there is no runtime
//     version to thread through (contrast NPM.Resolve, which passes
//     nodeVersion into nodeRuntimeArtifacts).
//
// version must be the exact version string a platform's manifest publishes
// (a pool.Candidate.Version as Refresh returns it); Resolve does not
// normalize it.
//
// PublishedAt and FirstSeenAt are both left at the zero-formatted timestamp
// on the returned value, mirroring NPM.Resolve's own treatment of
// FirstSeenAt (see that method's doc comment for the same reasoning): this
// channel's manifest carries no publish-history timestamp at all (Refresh's
// RULING above: "Do NOT invent a published_at"), so there is no real value
// to fill PublishedAt with at THIS stage either. Resolve deliberately never
// calls (*pool.Lock).Validate, which would reject both zero timestamps — a
// LockHarness with zero PublishedAt/FirstSeenAt is expected and legitimate
// at this stage. The orchestrator is expected to fill in a real value for
// each before persisting or validating the lock: FirstSeenAt from
// Observations, exactly as it already does for npm, and PublishedAt with
// THAT SAME fill-in value (i.e., for an archive-sourced harness, treat
// first-seen as the only available age signal and let published_at and
// first_seen_at agree) — the "conservative proxy" Refresh's RULING
// documents. That orchestrator wiring is this work package's explicit
// non-goal.
func (a *Archive) Resolve(ctx context.Context, b backend.Backend, pkg, version, workDir, nodeVersion, recipeRevision string) (pool.LockHarness, error) {
	artifacts := make(map[pool.PlatformKey]pool.LockArchiveArtifact)
	var reasons []string
	for _, key := range archivePlatformKeys {
		m, err := a.fetchManifest(ctx, key)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		if m.Version != version {
			reasons = append(reasons, fmt.Sprintf("%s: manifest reports version %q, not %q", key, m.Version, version))
			continue
		}
		artifacts[key] = pool.LockArchiveArtifact{URL: m.URL, SHA512: m.SHA512}
	}
	if len(artifacts) == 0 {
		return pool.LockHarness{}, fmt.Errorf(
			"archive resolve %s@%s: no platform manifest currently reports this version (%s)",
			pkg, version, strings.Join(reasons, "; "))
	}

	return pool.LockHarness{
		Source:         "archive",
		Package:        pkg,
		Version:        version,
		PublishedAt:    pool.FormatTimestamp(time.Time{}),
		FirstSeenAt:    pool.FormatTimestamp(time.Time{}),
		RecipeRevision: recipeRevision,
		Archive:        &pool.LockArchive{Artifacts: artifacts},
	}, nil
}

// Fetch downloads every artifact h.Archive names into cacheDir, verifying
// its upstream sha512 during a single streaming pass before the object is
// admitted (§6.2, §17 "integrity mismatch rejection") — pure Go, host-side,
// and never consults any URL that is not already present on h: an unlisted
// URL is a bug, not a fallback (mirrors NPM.Fetch's contract exactly).
//
// h is MUTATED in place: each artifact's pool-local SHA256 cache-address
// field is filled when empty, or re-verified when already set. cacheDir
// must be the flat, content-addressed cache root (pool.CacheSHA256Dir()),
// not CacheDir() — see NPM.Fetch's doc comment for why getting this wrong
// silently makes every subsequent lookup a permanent cache miss.
func (a *Archive) Fetch(ctx context.Context, h *pool.LockHarness, cacheDir string) error {
	if h == nil {
		return fmt.Errorf("fetch: nil LockHarness")
	}
	if h.Archive == nil {
		return fmt.Errorf("fetch: %s: lock has no archive artifacts (this Source only fetches archive harnesses)", h.Package)
	}

	for key, artifact := range h.Archive.Artifacts {
		if artifact.URL == "" {
			return fmt.Errorf("archive artifact %s: no URL recorded in lock", key)
		}
		if artifact.SHA512 == "" {
			return fmt.Errorf("archive artifact %s: no sha512 recorded in lock — nothing to verify against", key)
		}
		sha256Hex, err := a.downloadVerify(ctx, artifact.URL, artifact.SHA512, artifact.SHA256, cacheDir)
		if err != nil {
			return fmt.Errorf("archive artifact %s: %w", key, err)
		}
		if artifact.SHA256 == "" {
			artifact.SHA256 = sha256Hex
			h.Archive.Artifacts[key] = artifact
		}
	}
	return nil
}

// downloadVerify downloads url, streaming it simultaneously into a temp
// file under cacheDir, a sha256 hash (always — the pool's own cache
// address), and a sha512 hash (this channel's mandatory upstream integrity
// value — unlike npm's optional SRI, every archive artifact carries one).
// It returns the downloaded object's hex sha256 digest. Mirrors
// NPM.downloadVerify's cache-skip / verify-before-admit / same-volume-rename
// discipline exactly; see that method's doc comment for the reasoning
// behind each step (knownSHA256 cache-hit shortcut, temp-file-then-rename,
// parent-dir fsync skipped on Windows).
func (a *Archive) downloadVerify(ctx context.Context, url, wantSHA512Hex, knownSHA256, cacheDir string) (string, error) {
	if !validSHA512Hex(wantSHA512Hex) {
		return "", fmt.Errorf("malformed sha512 %q recorded in lock", wantSHA512Hex)
	}
	wantSHA512, err := hex.DecodeString(wantSHA512Hex)
	if err != nil {
		return "", fmt.Errorf("decoding sha512 %q recorded in lock: %w", wantSHA512Hex, err)
	}

	if knownSHA256 != "" {
		if !validSHA256Hex(knownSHA256) {
			return "", fmt.Errorf("malformed sha256 %q recorded in lock", knownSHA256)
		}
		if fi, err := os.Stat(filepath.Join(cacheDir, knownSHA256)); err == nil && fi.Size() > 0 {
			return knownSHA256, nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building request for %s: %w", url, err)
	}
	resp, err := a.downloadClient.Do(req)
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
	sha512h := sha512.New()
	if _, err := io.Copy(io.MultiWriter(tmp, sha256h, sha512h), resp.Body); err != nil {
		cleanup()
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}

	if got := sha512h.Sum(nil); !bytes.Equal(got, wantSHA512) {
		cleanup()
		return "", fmt.Errorf("integrity mismatch downloading %s: sha512 does not match", url)
	}

	sha256Hex := hex.EncodeToString(sha256h.Sum(nil))
	if knownSHA256 != "" && knownSHA256 != sha256Hex {
		cleanup()
		return "", fmt.Errorf("sha256 mismatch downloading %s: want %s, got %s", url, knownSHA256, sha256Hex)
	}

	if err := tmp.Chmod(0o644); err != nil {
		cleanup()
		return "", fmt.Errorf("setting permissions on temp file: %w", err)
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
	// pool.WriteFileAtomic and NPM.downloadVerify; skipped on Windows, which
	// has no directory-fsync equivalent (§6.4).
	if runtime.GOOS != "windows" {
		if dirFile, err := os.Open(cacheDir); err == nil {
			_ = dirFile.Sync()
			_ = dirFile.Close()
		}
	}

	return sha256Hex, nil
}

// recordedArchiveArtifactKeys renders a lock's recorded archive-artifact
// platform keys, sorted for determinism, for Realize's "no artifact
// recorded for platform %q" error to name what IS recorded — the archive
// counterpart of realize.go's recordedRuntimeArtifactKeys (that one is
// keyed on LockRuntimeArtifact, this one on LockArchiveArtifact; the map
// value types differ, so the helper is not literally reusable).
func recordedArchiveArtifactKeys(artifacts map[pool.PlatformKey]pool.LockArchiveArtifact) string {
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

// Realize builds one platform generation of harnessName (binary binName)
// entirely from the cached archive artifact for key: unpack the verified
// tar.gz and place its binName-matching entry at bin/<binName> with mode
// 0o755 (§11.4 last line: "Archive sources skip steps 1-3/5 and just unpack
// + wrap"). It is pure Go, host-side, and touches NEITHER the network NOR
// b — b is accepted only for Source-interface parity with NPM.Realize (see
// this file's top-level doc comment) and is asserted unused by this
// package's own tests (a fake Backend that t.Fatals on any method call). ctx
// is likewise accepted only for interface parity: Realize performs no I/O
// that benefits from cancellation (no network, no subprocess) beyond local
// file reads, so it is unused.
//
// h must already be Fetched: the platform's artifact must have a verified
// cache object under cacheDir (pool.CacheSHA256Dir()) — verifyCacheObject
// (realize.go, this package) enforces this the same way NPM.Realize does.
//
// Unlike NPM.Realize, there is no backend identity to record beyond "none":
// an archive generation is built entirely on the host, with no guix/OCI
// container involved at all (§11.5: "backend identity fields are
// meaningful only for backend-built generations" — this is exactly that
// case).
func (a *Archive) Realize(ctx context.Context, b backend.Backend, harnessName, binName string, h *pool.LockHarness, key pool.PlatformKey, cacheDir, stagingGenDir string) (*pool.Receipt, error) {
	if h == nil {
		return nil, fmt.Errorf("realize: nil LockHarness")
	}
	if harnessName == "" || binName == "" {
		return nil, fmt.Errorf("realize: harnessName and binName must both be non-empty")
	}
	if h.Archive == nil {
		return nil, fmt.Errorf("realize: %s: lock has no archive artifacts (this Source only realizes archive harnesses)", harnessName)
	}
	if _, err := pool.ParsePlatformKey(string(key)); err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}

	artifact, ok := h.Archive.Artifacts[key]
	if !ok {
		return nil, fmt.Errorf("realize: %s: no artifact recorded for platform %q (lock records: %s)",
			harnessName, key, recordedArchiveArtifactKeys(h.Archive.Artifacts))
	}
	// verifyCacheObject is realize.go's existing helper (this package, npm
	// realization) — an existence/non-empty check only, reused as-is: Fetch
	// already verified content against artifact.SHA512 at admission time.
	if err := verifyCacheObject(cacheDir, artifact.SHA256, fmt.Sprintf("archive artifact for platform %q", key)); err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}
	artifactPath := filepath.Join(cacheDir, artifact.SHA256)

	if err := os.MkdirAll(stagingGenDir, 0o755); err != nil {
		return nil, fmt.Errorf("realize: %s: creating staging generation dir: %w", harnessName, err)
	}
	if err := unpackArchiveArtifact(artifactPath, binName, stagingGenDir); err != nil {
		return nil, fmt.Errorf("realize: %s: %w", harnessName, err)
	}

	// pool.ScanGenerationFiles is also the symlink-free assertion §11.4's
	// unpack step implicitly requires: it fails the whole scan on any
	// symlink, naming the offending path (defense in depth — this package's
	// own tar extraction never writes a symlink in the first place, see
	// readArchiveEntries).
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
		Backend:        pool.ReceiptBackend{Name: "none"}, // no backend built this generation — see doc comment
		Files:          files,
		ManifestDigest: manifestDigest,
	}
	if err := receipt.Validate(); err != nil {
		return nil, fmt.Errorf("realize: %s: assembling receipt: %w", harnessName, err)
	}
	return receipt, nil
}

// --- tar.gz extraction (§11.4 last line: "unpack + wrap") --------------------

// maxArchiveEntrySize is the zip-bomb guard: a single tar entry declaring a
// larger size than this is rejected before any of its content is read.
const maxArchiveEntrySize = 1 << 30 // 1 GiB

// archiveTarEntry is one regular-file entry extracted from the tar.gz
// artifact, captured fully in memory: these artifacts are a single flat
// native binary (tens of megabytes), never large enough to need a
// two-pass-over-disk approach.
type archiveTarEntry struct {
	name string // cleaned, forward-slash, RAW (pre-strip) name from the tar header
	mode os.FileMode
	data []byte
}

// tarLinkKindLabel renders a tar Typeflag naming a link entry, for
// readArchiveEntries' rejection error.
func tarLinkKindLabel(t byte) string {
	if t == tar.TypeLink {
		return "hardlink"
	}
	return "symlink"
}

// isASCIILetter reports whether b is an ASCII letter — used by
// validateArchiveEntryName's drive-letter check. A local copy rather than
// reusing pool package's receipt.go isASCIILetter: that file belongs to a
// sibling work package this one does not touch, and the two live in
// different packages regardless.
func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// validateArchiveEntryName rejects a raw tar entry name that could escape
// stagingGenDir once joined onto it (§6.3, §17 "tar entry names are
// attacker-controlled input — treat with the same reject-don't-escape
// severity as wave 1's shell segments"): an absolute path (leading "/", or a
// Windows drive-letter prefix — checked independent of host OS, the same
// stance receipt.go's validateReceiptFilePath takes and for the same
// reason: a judgment about a foreign archive's content must not depend on
// which host happens to check it), or any ".." path segment.
func validateArchiveEntryName(name string) error {
	if path.IsAbs(name) {
		return fmt.Errorf("archive entry %q must not be an absolute path", name)
	}
	if len(name) >= 2 && isASCIILetter(name[0]) && name[1] == ':' {
		return fmt.Errorf("archive entry %q must not be an absolute path (drive-letter prefix)", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return fmt.Errorf("archive entry %q contains a %q path segment, which is not allowed", name, "..")
		}
	}
	return nil
}

// readArchiveEntries decompresses and parses r as a gzip-compressed tar
// stream, returning every regular-file entry (with its RAW, pre-strip name —
// stripCommonLeadingDir/relativeArchivePath below handle the leading
// directory). It rejects, rather than silently skips or accepts (the
// codebase's "reject, don't escape" stance — see backend.validateMount and
// realize.go's validateGeneratedShellSegment):
//
//   - a symlink or hardlink entry (tar.TypeSymlink / tar.TypeLink) — §6.4
//     generations are symlink-free, and a hardlink shares the same
//     tree-escape/portability hazards on the pool's host filesystems;
//   - any entry whose name is an absolute path or contains a ".." segment
//     (validateArchiveEntryName);
//   - any entry declaring a size over maxArchiveEntrySize (zip-bomb guard);
//   - any entry that is neither a regular file nor a directory (device,
//     fifo, socket, ...).
//
// A directory entry contributes no archiveTarEntry (Realize creates
// directories implicitly from file paths via os.MkdirAll) but its name is
// still validated — a directory entry is exactly as capable of naming an
// escaping path as a file entry.
func readArchiveEntries(r io.Reader) ([]archiveTarEntry, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("opening gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var entries []archiveTarEntry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar entry: %w", err)
		}

		name := path.Clean(filepath.ToSlash(hdr.Name))
		if name == "." {
			continue // the archive's own root entry ("./"), if present
		}
		if err := validateArchiveEntryName(name); err != nil {
			return nil, err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeSymlink, tar.TypeLink:
			return nil, fmt.Errorf("archive entry %q is a %s, which is not allowed (generations must be symlink-free, §6.4)",
				name, tarLinkKindLabel(hdr.Typeflag))
		case tar.TypeReg:
			if hdr.Size > maxArchiveEntrySize {
				return nil, fmt.Errorf("archive entry %q declares size %d bytes, over the %d byte limit (zip-bomb guard)",
					name, hdr.Size, int64(maxArchiveEntrySize))
			}
			data := make([]byte, hdr.Size)
			if _, err := io.ReadFull(tr, data); err != nil {
				return nil, fmt.Errorf("reading content of archive entry %q: %w", name, err)
			}
			entries = append(entries, archiveTarEntry{name: name, mode: os.FileMode(hdr.Mode).Perm(), data: data})
		default:
			return nil, fmt.Errorf("archive entry %q has unsupported type %q", name, string(hdr.Typeflag))
		}
	}
	return entries, nil
}

// stripCommonLeadingDir computes the single top-level directory name shared
// by every entry's name, when one exists ("strip any leading directory",
// §11.4 last line) — the same convention `tar --strip-components=1`
// encodes, reimplemented in pure Go here since Go stdlib has no CLI tar to
// shell out to (and CLAUDE.md forbids shell execution regardless). Returns
// "" when there is no single shared top directory — including when any
// entry is already a top-level file — in which case entry names are used
// as-is: this channel's own artifact, per the work package brief, may ship
// "the flat native binary" with no wrapping directory at all, and this
// function must handle that shape too, not just the wrapped-directory one.
func stripCommonLeadingDir(entries []archiveTarEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var common string
	for i, e := range entries {
		top, _, ok := strings.Cut(e.name, "/")
		if !ok {
			return "" // a top-level file — nothing to strip
		}
		if i == 0 {
			common = top
			continue
		}
		if top != common {
			return ""
		}
	}
	return common
}

// relativeArchivePath computes name's final generation-relative path after
// stripping stripDir (the result of stripCommonLeadingDir), when stripDir is
// non-empty. name is assumed already validated (validateArchiveEntryName)
// and, when stripDir is non-empty, is guaranteed by stripCommonLeadingDir's
// own construction to share that prefix — the prefix-mismatch case below is
// a defensive impossibility, not a reachable failure mode.
func relativeArchivePath(name, stripDir string) (string, error) {
	rel := name
	if stripDir != "" {
		cut, ok := strings.CutPrefix(name, stripDir+"/")
		if !ok {
			return "", fmt.Errorf("archive entry %q does not share the common leading directory %q", name, stripDir)
		}
		rel = cut
	}
	if rel == "" {
		return "", fmt.Errorf("archive entry %q resolves to an empty path after stripping the leading directory %q", name, stripDir)
	}
	return rel, nil
}

// unpackArchiveArtifact extracts artifactPath's tar.gz content into
// stagingGenDir, preserving the archive's own relative layout minus one
// stripped common leading directory (if any, stripCommonLeadingDir), then
// ensures the entry named binName ends up at stagingGenDir/bin/<binName>
// with mode 0o755 — the canonical path the §5.3 dispatcher execs, and the
// "wrap" half of §11.4's "unpack + wrap". binName is located by a basename
// match over every extracted regular file's (post-strip) relative path:
// more than one match, or none, is rejected rather than guessed (the same
// "reject, don't guess" stance the rest of this file takes), naming the
// archive's full post-strip content in the "none" case for diagnosis.
func unpackArchiveArtifact(artifactPath, binName, stagingGenDir string) error {
	f, err := os.Open(artifactPath)
	if err != nil {
		return fmt.Errorf("opening archive %s: %w", artifactPath, err)
	}
	defer f.Close()

	rawEntries, err := readArchiveEntries(f)
	if err != nil {
		return fmt.Errorf("reading archive %s: %w", artifactPath, err)
	}
	if len(rawEntries) == 0 {
		return fmt.Errorf("archive %s contains no regular files", artifactPath)
	}

	stripDir := stripCommonLeadingDir(rawEntries)

	type placedEntry struct {
		rel  string
		mode os.FileMode
		data []byte
	}
	placed := make([]placedEntry, 0, len(rawEntries))
	for _, e := range rawEntries {
		rel, err := relativeArchivePath(e.name, stripDir)
		if err != nil {
			return err
		}
		placed = append(placed, placedEntry{rel: rel, mode: e.mode, data: e.data})
	}

	var binIdx = -1
	names := make([]string, 0, len(placed))
	for i, p := range placed {
		names = append(names, p.rel)
		if path.Base(p.rel) != binName {
			continue
		}
		if binIdx != -1 {
			return fmt.Errorf("archive %s has more than one entry named %q (%q and %q); cannot pick one unambiguously",
				artifactPath, binName, placed[binIdx].rel, p.rel)
		}
		binIdx = i
	}
	if binIdx == -1 {
		sort.Strings(names)
		return fmt.Errorf("archive %s has no entry named %q; contents: %s", artifactPath, binName, strings.Join(names, ", "))
	}

	for _, p := range placed {
		dst := filepath.Join(stagingGenDir, filepath.FromSlash(p.rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("creating directory for %s: %w", p.rel, err)
		}
		// Every extracted regular file gets at least owner read/write,
		// regardless of the archive's own recorded mode — the executable
		// bit specifically is set explicitly on the binary below, not
		// inherited from the tar header (§6.4: realization sets 0755 on
		// bin/*, not "whatever the archive happened to record").
		if err := os.WriteFile(dst, p.data, p.mode.Perm()|0o600); err != nil {
			return fmt.Errorf("writing %s: %w", p.rel, err)
		}
	}

	binDir := filepath.Join(stagingGenDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", binDir, err)
	}
	wrapperPath := filepath.Join(binDir, binName)
	extractedPath := filepath.Join(stagingGenDir, filepath.FromSlash(placed[binIdx].rel))
	if extractedPath != wrapperPath {
		// hardlinkOrCopy is realize.go's existing helper (this package):
		// hardlink when possible, falling back to a byte copy across a
		// volume boundary.
		if err := hardlinkOrCopy(extractedPath, wrapperPath); err != nil {
			return fmt.Errorf("placing %s at %s: %w", placed[binIdx].rel, wrapperPath, err)
		}
	}
	if err := os.Chmod(wrapperPath, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", wrapperPath, err)
	}
	return nil
}
