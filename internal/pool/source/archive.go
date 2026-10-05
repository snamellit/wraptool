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
	"io/fs"
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
	// progress receives Fetch's download progress; see SetProgress.
	progress io.Writer
}

// SetProgress makes Fetch report each artifact download to w, mirroring
// NPM.SetProgress. A nil w (the default) disables reporting.
func (a *Archive) SetProgress(w io.Writer) { a.progress = w }

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
// This does NOT enforce the artifact URL's scheme (https). That check is
// deliberately NOT here: it is Resolve's own job (see Resolve's "Scheme
// check" doc paragraph), applied only at the point a manifest-supplied URL
// is about to become a LOCKED LockArchiveArtifact, plus lock.go's
// Lock.Validate/validateArchiveArtifact as a second, independent gate on
// the persisted lock (§8 ruling 1, "URL https"). Enforcing it here in
// validateManifest as well — which EVERY fetchManifest call runs, including
// from Refresh, which never locks anything — would make this package
// untestable against a plain-http httptest.Server, exactly the seam every
// other Source/backend test in this codebase relies on (NewNPM's own
// registryBaseURL/nodeDistBaseURL test seams are equally scheme-agnostic).
// Production manifests are always https in practice (the work package
// brief's live endpoint).
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
// RULING (§9, §8.1, wave2-integration-fixes item 1): this channel's manifest
// has no publish-history field, only "version" — there is nothing to derive
// a publication timestamp from. Every returned Candidate therefore carries
// a ZERO PublishedAt; this method deliberately does NOT invent one. For
// archive sources minimum_release_age is INAPPLICABLE (no publish history
// to evaluate it against); local quarantine = minimum_observation_age
// alone, measured against this pool's own write-once first_seen_at (§6.6,
// filled in by the orchestrator, never by this Source).
//
// The wiring: RefreshHarnessLocked (orchestrator.go) synthesizes
// PublishedAt := FirstSeenAt for any Candidate whose Source-returned
// PublishedAt was zero — every one this method returns — and marks it
// Candidate.PublishedAtSynthesized. pool.Eligible/QuarantineRemaining
// (eligibility.go) then skip the minimum_release_age check entirely for a
// synthesized PublishedAt (it does not represent a real publish time, so a
// 0h or any nonzero value are equally meaningless against it) while still
// enforcing minimum_observation_age against the real FirstSeenAt
// unconditionally. Before this wiring, pool.Eligible treated a zero
// PublishedAt as unconditionally ineligible with no path to ever become
// eligible — an archive-sourced harness could never complete a first
// install. This method itself still returns a zero PublishedAt
// unconditionally (the synthesis lives entirely in the orchestrator, not
// here) — see RefreshHarnessLocked's and Candidate.PublishedAtSynthesized's
// doc comments for the full mechanics and the write-once-before-synthesis
// ordering constraint (§6.6) that makes this the only safe place to do it.
//
// PARTIAL-FAILURE OBSERVABILITY: a platform whose manifest cannot be
// fetched or fails validation is simply excluded from this call's results,
// not treated as a hard failure of the whole Refresh — mirroring Resolve's
// "a mid-rollout platform is simply absent" tolerance (§8.1 ruling 3).
// Refresh fails outright only when NOT ONE of the four platforms could be
// reached, aggregating every platform's error via errors.Join for
// diagnosis in that all-failed case. Short of that, the CALLER of Refresh
// cannot distinguish "this platform genuinely has not published this
// release yet" (a routine, expected mid-rollout state) from "this
// platform's manifest endpoint is unreachable/misbehaving right now" (an
// operational problem worth surfacing) — both collapse to the same silent
// exclusion from the returned candidate set. A future caller that wants to
// alert on the latter needs a richer return shape (e.g. per-platform
// results) than this method currently provides; today that distinction is
// only visible by reading logs/errors this method does not itself emit.
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
// PARTIAL-FAILURE OBSERVABILITY (mirrors Refresh's own note): in the
// SUCCESS case — at least one platform matched — the caller has no way to
// tell, from the returned LockHarness alone, WHY a platform absent from
// Archive.Artifacts is absent: "this platform hasn't published this
// version yet" and "this platform's manifest endpoint was unreachable when
// Resolve ran" are indistinguishable from the returned value. The per-
// platform reasons this method collects internally are only surfaced in
// the error message of the ALL-failed case; a partial success silently
// discards them. A future caller wanting to alert on "unreachable, not
// just not-yet-published" needs a richer return shape than this method
// currently provides.
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
// Scheme check (deliberately split from validateManifest): validateManifest,
// which fetchManifest applies to every raw manifest this method reads, is
// intentionally scheme-agnostic (see its doc comment — an httptest.Server
// test seam cannot serve https). Resolve itself enforces https on the
// artifact URL of every platform it is about to LOCK — the one point this
// package turns a manifest-supplied URL into a LockArchiveArtifact that
// Fetch will later download from unauthenticated (integrity is verified
// AFTER download, so the transport itself must not be tamperable). A
// non-https artifact URL for ANY matching platform fails the WHOLE Resolve
// call immediately (not just that platform): a channel serving plain http
// for one artifact is a signal the channel itself may be compromised or
// misconfigured, not a per-platform absence to silently tolerate the way a
// version mismatch is.
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
// documents. CONCRETE CONSEQUENCE: until that wiring lands, a LockHarness
// this method returns cannot be persisted at all — (*pool.Lock).Validate
// rejects a zero published_at/first_seen_at outright (validateLockTimestamp)
// — so this is not merely an inert placeholder value, it is a hard block on
// ever writing an archive-sourced entry into harness-lock.json. That
// orchestrator wiring is this work package's explicit non-goal.
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
		if !strings.HasPrefix(m.URL, "https://") {
			return pool.LockHarness{}, fmt.Errorf(
				"archive resolve %s@%s: platform %s manifest artifact url %q is not https; refusing to lock it",
				pkg, version, key, m.URL)
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
		sha256Hex, err := a.downloadVerify(ctx, fmt.Sprintf("%s (%s)", h.Package, key), artifact.URL, artifact.SHA512, artifact.SHA256, cacheDir)
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
func (a *Archive) downloadVerify(ctx context.Context, label, url, wantSHA512Hex, knownSHA256, cacheDir string) (string, error) {
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
	if hit, err := findCached(cacheDir, wantSHA512, knownSHA256); err != nil || hit != "" {
		return hit, err
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
	if _, err := io.Copy(io.MultiWriter(tmp, sha256h, sha512h, newDownloadProgress(a.progress, label, resp.ContentLength)), resp.Body); err != nil {
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
	recordCached(cacheDir, wantSHA512, sha256Hex)

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
	// streamExtractArchive).
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
//
// SECURITY NOTE (review round 1, BLOCKER): the original implementation here
// buffered every regular entry's FULL declared Size into memory
// (archiveTarEntry.data) before deciding anything about the archive's
// shape. A per-entry cap alone does not bound total memory: GNU/PAX sparse
// tar entries let a tiny PHYSICAL archive declare an enormous LOGICAL Size
// (Go's tar.Reader transparently materializes the sparse holes as zero
// bytes on Read, honoring Header.Size regardless of how little data backs
// it), so a handful of KB on disk could drive tens of gigabytes of Go heap
// allocation across several individually-small entries, well before any
// single entry tripped a per-entry limit. Two independent bounds now apply,
// BOTH evaluated from tar HEADER metadata alone, before any of an entry's
// content is read or written:
//
//  1. maxArchiveEntrySize — unchanged, a per-entry cap.
//  2. maxArchiveTotalBytes — NEW, a running total across every entry in the
//     archive, checked before each entry's content is touched at all.
//
// Extraction is also now single-pass STREAMING: each accepted regular
// entry is copied directly from the tar reader to its destination file via
// io.CopyN, never buffered whole in a Go slice. Because the "strip one
// common leading directory" decision (§11.4 last line) needs to see every
// entry's name to decide whether such a directory exists, and a tar stream
// cannot be rewound, this is a "write-then-scan on disk" design: entries
// are streamed to stagingGenDir under their RAW (validated, but
// unstripped) relative paths first; stripCommonLeadingDirOnDisk then
// inspects (and rewrites, via directory renames — no re-reading of file
// content) the resulting ON-DISK tree; locateBinNameOnDisk finally walks
// that same tree by NAME only to find the binName match. At no point after
// the streaming copy does this package hold more than one entry's name/
// metadata in memory at a time.

// maxArchiveEntrySize is the per-entry zip-bomb guard: a single tar entry
// declaring a larger size than this is rejected before any of its content
// is read. Package-level VAR (not const) so tests can shrink it to make a
// "sum of several small-cap entries exceeds the whole-artifact budget"
// scenario reachable with genuinely tiny fixture data, without needing to
// fabricate gigabyte-scale (or GNU-sparse-encoded, which Go's tar.Writer
// cannot produce via its public API) test archives; production code never
// mutates it.
var maxArchiveEntrySize int64 = 1 << 30 // 1 GiB

// maxArchiveTotalBytes is the whole-artifact zip-bomb guard (review round
// 1, BLOCKER item 1b): the running sum of every regular entry's declared
// Size, checked BEFORE each entry's content is read or written, rejecting
// the archive the instant the prospective total would exceed this budget —
// so no entry that would push the cumulative total over budget is ever
// extracted, regardless of how small its own declared size is. Also a
// package-level var for the same test-seam reason as maxArchiveEntrySize.
var maxArchiveTotalBytes int64 = 2 << 30 // 2 GiB

// maxArchiveEntryCount bounds the number of regular entries a single
// archive may contain (review round 2, item 1): the byte budgets above are
// blind to a "free" attack that never touches either of them — many
// (potentially millions of) genuinely ZERO-size regular entries, each
// costing nothing against maxArchiveEntrySize/maxArchiveTotalBytes but
// still creating a real inode/file on disk per entry, exhausting the
// staging filesystem's inode table (or just its directory-entry count)
// long before any byte-size guard would ever fire. Checked per entry,
// before extraction, the same way as the byte budgets — a package-level
// var for the same test-seam reason.
var maxArchiveEntryCount = 65536

// tarLinkKindLabel renders a tar Typeflag naming a link entry, for
// streamExtractArchive's rejection error.
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
// which host happens to check it), or any ".." path segment. This is the
// ONLY guard standing between an attacker-controlled tar entry name and an
// actual write to disk outside stagingGenDir (see
// TestRealize_ArchiveRejectsPathEscapeEntryMatchingBinName, which proves
// this by neutering the guard and observing the escape actually happen).
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

// streamExtractArchive decompresses and parses r as a gzip-compressed tar
// stream, streaming every regular-file entry directly to disk under
// destRoot at its RAW (validated, but not yet leading-dir-stripped)
// relative path — see this section's top-level SECURITY NOTE for why this
// is a streaming, single-pass design rather than an in-memory one. It
// rejects, rather than silently skips or accepts (the codebase's "reject,
// don't escape" stance — see backend.validateMount and realize.go's
// validateGeneratedShellSegment):
//
//   - a symlink or hardlink entry (tar.TypeSymlink / tar.TypeLink) — §6.4
//     generations are symlink-free, and a hardlink shares the same
//     tree-escape/portability hazards on the pool's host filesystems;
//   - any entry whose name is an absolute path or contains a ".." segment
//     (validateArchiveEntryName);
//   - a regular-entry count over maxArchiveEntryCount (review round 2, item
//     1: an inode-exhaustion guard the byte-size budgets below cannot
//     see — many genuinely zero-size entries cost nothing against them);
//   - any entry declaring a negative or over-maxArchiveEntrySize size
//     (per-entry zip-bomb guard);
//   - a cumulative declared size over maxArchiveTotalBytes across every
//     regular entry seen so far, INCLUDING the one currently being
//     evaluated — checked from hdr.Size alone, before that entry's content
//     is read (whole-artifact zip-bomb guard);
//   - two regular entries whose raw name is identical (would silently
//     overwrite on a naive write, review round 1 item 6) — enforced by
//     O_EXCL on the destination file, so the SECOND entry with a given
//     name fails instead of clobbering the first;
//   - any entry that is neither a regular file, a directory, nor a PAX
//     global extended header (tar.TypeXGlobalHeader — git-produced
//     archives prepend one; Go's tar.Reader does NOT auto-absorb this type
//     the way it does local PAX/GNU-longname headers, so without this case
//     a legitimate `git archive` tarball would be rejected outright,
//     review round 1 item 4).
//
// A directory entry contributes no file (Realize creates directories
// implicitly via os.MkdirAll ahead of each file it writes) but its name is
// still validated — a directory entry is exactly as capable of naming an
// escaping path as a file entry. Returns an error if the archive contains
// no regular file at all.
func streamExtractArchive(r io.Reader, destRoot string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("opening gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var totalSize int64
	var entryCount int
	sawRegular := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		name := path.Clean(filepath.ToSlash(hdr.Name))
		if name == "." {
			continue // the archive's own root entry ("./"), if present
		}
		if err := validateArchiveEntryName(name); err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeXGlobalHeader:
			continue // PAX global extended header (e.g. `git archive`'s pax_global_header) — metadata only, not a file
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("archive entry %q is a %s, which is not allowed (generations must be symlink-free, §6.4)",
				name, tarLinkKindLabel(hdr.Typeflag))
		case tar.TypeReg:
			entryCount++
			if entryCount > maxArchiveEntryCount {
				return fmt.Errorf("archive declares more than %d regular entries; rejecting entry %q before extracting it (inode-exhaustion guard)",
					maxArchiveEntryCount, name)
			}
			if hdr.Size < 0 {
				return fmt.Errorf("archive entry %q declares a negative size %d", name, hdr.Size)
			}
			if hdr.Size > maxArchiveEntrySize {
				return fmt.Errorf("archive entry %q declares size %d bytes, over the %d byte per-entry limit (zip-bomb guard)",
					name, hdr.Size, maxArchiveEntrySize)
			}
			prospectiveTotal := totalSize + hdr.Size
			if prospectiveTotal > maxArchiveTotalBytes {
				return fmt.Errorf(
					"archive's declared total size would reach %d bytes (over the %d byte whole-artifact limit) at entry %q; rejecting before extracting it (zip-bomb guard)",
					prospectiveTotal, maxArchiveTotalBytes, name)
			}
			totalSize = prospectiveTotal
			sawRegular = true

			dst := filepath.Join(destRoot, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return fmt.Errorf("creating directory for %s: %w", name, err)
			}
			if err := streamExtractRegularEntry(tr, dst, hdr.Size, os.FileMode(hdr.Mode).Perm()); err != nil {
				return fmt.Errorf("extracting %q: %w", name, err)
			}
		default:
			return fmt.Errorf("archive entry %q has unsupported type %q", name, string(hdr.Typeflag))
		}
	}
	if !sawRegular {
		return fmt.Errorf("archive contains no regular files")
	}
	return nil
}

// streamExtractRegularEntry streams exactly size bytes from tr (the tar
// reader, currently positioned at one regular entry's content) into a new
// file at dst, then normalizes its mode (review round 1 item 5): 0o755 if
// archiveMode carries ANY exec bit (owner/group/other, 0o111), else 0o644
// — umask-independent, and independent of every OTHER bit the archive's
// header claimed (kills a hostile group/world-writable or setuid mode, and
// keeps the receipt's digested "executable" flag — which is ITSELF
// boolean, not the raw mode, see receipt.go's ReceiptFile — identical
// across hosts regardless of the archive's origin umask, §11.5).
//
// dst is created with O_EXCL: a SECOND entry that would extract to the
// same dst (whether via an exactly-duplicated raw name, review round 1
// item 6) fails here rather than silently overwriting the first entry's
// content — "reject, don't guess," matching this file's stance on an
// ambiguous binName match.
func streamExtractRegularEntry(tr *tar.Reader, dst string, size int64, archiveMode os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// O_EXCL reports EEXIST for BOTH a true duplicate file entry
			// (review round 1, item 6) and for an entry whose path was
			// already created as a DIRECTORY by an earlier, nested entry
			// (e.g. archive contains both "foo/bar" — which MkdirAll's
			// "foo" into existence — and a separate top-level entry
			// literally named "foo"). Lstat distinguishes them so the
			// error names the actual cause instead of always claiming
			// "duplicate entry" for what is really a name/directory
			// collision (review round 2, item 3).
			if fi, statErr := os.Lstat(dst); statErr == nil && fi.IsDir() {
				return fmt.Errorf("entry %s collides with a directory created for nested entries", dst)
			}
			return fmt.Errorf("duplicate entry: %s already extracted from an earlier tar entry", dst)
		}
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	if _, err := io.CopyN(out, tr, size); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("writing content: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("closing %s: %w", dst, err)
	}

	mode := os.FileMode(0o644)
	if archiveMode&0o111 != 0 {
		mode = 0o755
	}
	if err := os.Chmod(dst, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}
	return nil
}

// stripCommonLeadingDirOnDisk inspects destRoot's TOP-LEVEL directory
// listing (names only — no file content is read) and, if it holds EXACTLY
// one entry and that entry is itself a directory, moves every child of
// that directory up one level (via os.Rename — no data copy) and removes
// the now-empty wrapping directory — the on-disk equivalent of `tar
// --strip-components=1` (§11.4 last line: "strip any leading directory"),
// reimplemented here because Go stdlib has no CLI tar to shell out to (and
// CLAUDE.md forbids shell execution regardless) and because the streaming
// design above (see this section's SECURITY NOTE) only knows the archive's
// full shape once every entry is already on disk. Does nothing — leaving
// entries exactly where streamExtractArchive placed them — when destRoot's
// top level holds more than one entry, or holds exactly one entry that is
// itself a plain FILE: this channel's own artifact, per the work package
// brief, may ship "the flat native binary" with no wrapping directory at
// all, and that shape must be left untouched, not stripped.
func stripCommonLeadingDirOnDisk(destRoot string) error {
	entries, err := os.ReadDir(destRoot)
	if err != nil {
		return fmt.Errorf("reading extracted tree %s: %w", destRoot, err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return nil
	}

	topName := entries[0].Name()
	topDir := filepath.Join(destRoot, topName)
	children, err := os.ReadDir(topDir)
	if err != nil {
		return fmt.Errorf("reading wrapping directory %s: %w", topDir, err)
	}
	for _, c := range children {
		oldPath := filepath.Join(topDir, c.Name())
		newPath := filepath.Join(destRoot, c.Name())
		// Defensive, not currently reachable (review round 2, item 4): the
		// `len(entries) != 1` check above already establishes that topDir
		// is destRoot's ONLY top-level entry, so nothing else at
		// destRoot's top level exists for a moved child to collide with
		// in THIS call path. Kept as a guard rather than dropped in case
		// this function's precondition ever changes (e.g. a future caller
		// invoking it on a directory that isn't exclusively
		// freshly-extracted content) — "reject, don't guess" if that ever
		// happens, rather than silently clobbering an existing entry via
		// os.Rename's overwrite-on-collision semantics.
		if _, err := os.Lstat(newPath); err == nil {
			return fmt.Errorf("stripping leading directory %q: %q collides with an existing top-level entry", topName, c.Name())
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			return fmt.Errorf("stripping leading directory %q: moving %q: %w", topName, c.Name(), err)
		}
	}
	if err := os.Remove(topDir); err != nil {
		return fmt.Errorf("removing emptied wrapping directory %s: %w", topDir, err)
	}
	return nil
}

// locateBinNameOnDisk walks root (already leading-dir-stripped, if
// applicable) and returns the full path of the single regular file whose
// basename equals binName. More than one match, or none, is rejected
// rather than guessed (the same "reject, don't guess" stance the rest of
// this file takes); the "none" case names every regular file root actually
// contains, for diagnosis.
func locateBinNameOnDisk(root, binName string) (string, error) {
	var match string
	var names []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking %s: %w", p, err)
		}
		if p == root || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return fmt.Errorf("computing relative path for %s: %w", p, relErr)
		}
		relSlash := filepath.ToSlash(rel)
		names = append(names, relSlash)
		if path.Base(relSlash) != binName {
			return nil
		}
		if match != "" {
			return fmt.Errorf("more than one entry named %q (%q and %q); cannot pick one unambiguously", binName, match, p)
		}
		match = p
		return nil
	})
	if err != nil {
		return "", err
	}
	if match == "" {
		// Flat-binary fallback: real channels name the shipped binary after
		// the PRODUCT, not the harness command (verified live: agy 1.1.10's
		// tarball contains a single file named "antigravity"). When exactly
		// one regular file was extracted, it is unambiguously the binary
		// this flat-native channel ships — use it; installArchiveBin places
		// it at bin/<binName> regardless of its archive name. Anything else
		// (zero or several files, none matching) stays a reject-don't-guess
		// error listing the contents.
		if len(names) == 1 {
			return filepath.Join(root, filepath.FromSlash(names[0])), nil
		}
		sort.Strings(names)
		return "", fmt.Errorf("no entry named %q and the archive is not a single flat binary; contents: %s", binName, strings.Join(names, ", "))
	}
	return match, nil
}

// unpackArchiveArtifact extracts artifactPath's tar.gz content into
// stagingGenDir, preserving the archive's own relative layout minus one
// stripped common leading directory (if any, stripCommonLeadingDirOnDisk),
// then ensures the entry named binName ends up at stagingGenDir/bin/<binName>
// with mode 0o755 — the canonical path the §5.3 dispatcher execs, and the
// "wrap" half of §11.4's "unpack + wrap".
func unpackArchiveArtifact(artifactPath, binName, stagingGenDir string) error {
	f, err := os.Open(artifactPath)
	if err != nil {
		return fmt.Errorf("opening archive %s: %w", artifactPath, err)
	}
	defer f.Close()

	if err := streamExtractArchive(f, stagingGenDir); err != nil {
		return fmt.Errorf("reading archive %s: %w", artifactPath, err)
	}
	if err := stripCommonLeadingDirOnDisk(stagingGenDir); err != nil {
		return fmt.Errorf("archive %s: %w", artifactPath, err)
	}

	binPath, err := locateBinNameOnDisk(stagingGenDir, binName)
	if err != nil {
		return fmt.Errorf("archive %s: %w", artifactPath, err)
	}

	binDir := filepath.Join(stagingGenDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", binDir, err)
	}
	wrapperPath := filepath.Join(binDir, binName)
	if binPath != wrapperPath {
		// hardlinkOrCopy is realize.go's existing helper (this package):
		// hardlink when possible, falling back to a byte copy across a
		// volume boundary.
		if err := hardlinkOrCopy(binPath, wrapperPath); err != nil {
			return fmt.Errorf("placing %s at %s: %w", binPath, wrapperPath, err)
		}
	}
	if err := os.Chmod(wrapperPath, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", wrapperPath, err)
	}
	return nil
}
