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
	"hash"
	"io"
	"net/http"
	"os"
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

	// metadataTimeout bounds one registry/SHASUMS256.txt request. These are
	// tiny JSON/text documents; a client-level timeout (rather than relying
	// solely on the caller's context) keeps a stalled metadata connection
	// from wedging whatever holds the pool's advisory lock (§6.5) around a
	// Refresh or Resolve call indefinitely.
	metadataTimeout = 30 * time.Second
	// downloadTimeout bounds one artifact download (npm tarball or Node
	// runtime tarball). These can be tens of megabytes, so the budget is far
	// larger than metadataTimeout, but still finite: an artifact download
	// must not be allowed to hang forever either.
	//
	// Both timeouts exist as stall-bounds against the §6.5 pool-lock wedge
	// risk, not as bandwidth guarantees. http.Client's Timeout is a
	// wall-clock cap on the whole request (connect through reading the full
	// response body), so it also caps a slow-but-otherwise-healthy transfer,
	// not only a truly stalled one — this package accepts that trade-off
	// rather than promising any particular throughput. If downloadTimeout
	// ever proves too tight for a legitimately slow-but-progressing
	// download in practice, the right fix is a real stall detector (abort
	// only when no bytes arrive for N seconds) rather than raising this
	// fixed budget further.
	downloadTimeout = 10 * time.Minute
)

// sha256HexPattern is the only shape this package accepts for a sha256
// digest used as a cache filename: exactly 64 lowercase hex characters. Any
// sha256 value that reaches a filepath.Join as a path component — whether it
// came from a LockHarness (round-tripped through JSON, so untrusted) or was
// parsed out of nodejs.org's SHASUMS256.txt (a tampered or malformed line
// could otherwise smuggle "../" segments) — is checked against this pattern
// first. A value this package computed itself via hex.EncodeToString on a
// sha256.Sum256 output always matches by construction and never needs the
// check.
var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validSHA256Hex(s string) bool {
	return sha256HexPattern.MatchString(s)
}

// NPM implements pool artifact acquisition for the "npm" source (§8.1). The
// zero value is not usable; construct with NewNPM.
type NPM struct {
	registryBaseURL string
	nodeDistBaseURL string
	// metadataClient and downloadClient are the injectable HTTP seams:
	// production code never sets them directly (NewNPM defaults both to
	// clients with the timeouts above), but a same-package test can
	// construct an NPM literal with clients pointed at httptest.Servers, or
	// override nodeDistBaseURL, so no test ever talks to the real npm
	// registry or nodejs.org. They are separate fields (rather than one
	// shared client) so the small, frequent metadata calls (packument,
	// SHASUMS256.txt) and the large, occasional artifact downloads can carry
	// different timeout budgets.
	metadataClient *http.Client
	downloadClient *http.Client
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
		metadataClient:  &http.Client{Timeout: metadataTimeout},
		downloadClient:  &http.Client{Timeout: downloadTimeout},
	}
}

// npmPackument is the subset of an npm registry packument (the full
// document, not the abbreviated/corgi format — that omits "time", which
// Refresh needs) this package reads. Both maps are decoded as raw messages:
// Versions because most callers only need to know a version key exists —
// only backfillIntegrity decodes a version's manifest, and then only the one
// version it needs (npmVersionDist); Time because npm's "time" map is not
// uniformly string-valued — an "unpublished" key can carry a nested object
// describing an unpublish event rather than a version's own timestamp — and
// decoding straight into map[string]string would fail the whole packument
// over that one non-version key.
type npmPackument struct {
	Time     map[string]json.RawMessage `json:"time"`
	Versions map[string]json.RawMessage `json:"versions"`
}

// isNpmTimeVersionKey reports whether key (from a packument's "time" map)
// names an actual version rather than one of npm's known non-version
// bookend keys: "created"/"modified" bracket the map, and "unpublished"
// (when present) carries unpublish-event metadata, not a version timestamp.
func isNpmTimeVersionKey(key string) bool {
	switch key {
	case "created", "modified", "unpublished":
		return false
	default:
		return true
	}
}

// rawTimeString decodes raw as a JSON string, reporting false (not an error)
// when it is any other JSON type — the caller treats that exactly like a
// missing entry.
func rawTimeString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
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

	resp, err := n.metadataClient.Do(req)
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
// "time" entirely, or carrying a value that is not a JSON string or that
// pool.ParseTimestamp cannot parse, yields a zero PublishedAt rather than an
// invented one — pool.Eligible already treats a zero PublishedAt as
// unconditionally ineligible, which is the intended safety net
// (design/unified-harness-pool.md §9). This is Refresh's candidate-listing
// path only; Resolve's success path is stricter (see versionPublishedAt).
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
	// published) and "time" minus its non-version keys (every version ever
	// published, including unpublished ones) — a version can legitimately
	// appear in only one of the two maps (§8.1 test fixtures: "a missing
	// time entry" and "an unpublished version").
	versions := make(map[string]struct{}, len(doc.Versions)+len(doc.Time))
	for v := range doc.Versions {
		versions[v] = struct{}{}
	}
	for v := range doc.Time {
		if !isNpmTimeVersionKey(v) {
			continue
		}
		versions[v] = struct{}{}
	}

	candidates := make([]pool.Candidate, 0, len(versions))
	for v := range versions {
		_, inVersions := doc.Versions[v]
		raw, inTime := doc.Time[v]

		var publishedAt time.Time
		if inTime {
			if s, ok := rawTimeString(raw); ok {
				if t, err := pool.ParseTimestamp(s); err == nil {
					publishedAt = t
				}
			}
			// A non-string or unparsable value falls through with
			// publishedAt left zero rather than failing the whole Refresh
			// over one bad entry.
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

// versionPublishedAt looks up version's publish time (Resolve does not
// trust a caller-supplied Candidate, which may be stale by the time Resolve
// runs). Unlike Refresh's candidate listing — where a missing/unparsable
// time deliberately yields a zero, ineligible-by-construction PublishedAt
// (§9's safety net) — Resolve's success path requires a real value:
// published_at is a required lock property (§8), so a missing, non-string,
// or unparsable registry time for the exact version being locked fails
// here, at the cause, rather than surfacing later as an opaque
// (*pool.Lock).Validate rejection. packuments is the SAME per-Resolve-call
// cache checkEnginesNode/backfillIntegrity thread through this method's
// caller (review NIT 8), so a single Resolve call fetches pkg's packument
// at most once no matter how many of these three callers need it.
func (n *NPM) versionPublishedAt(ctx context.Context, packuments map[string]*npmPackument, pkg, version string) (time.Time, error) {
	doc, ok := packuments[pkg]
	if !ok {
		var err error
		doc, err = n.fetchPackument(ctx, pkg)
		if err != nil {
			return time.Time{}, err
		}
		packuments[pkg] = doc
	}
	raw, ok := doc.Time[version]
	if !ok {
		return time.Time{}, fmt.Errorf("npm registry has no time entry for %s@%s (required to lock published_at, §8)", pkg, version)
	}
	s, ok := rawTimeString(raw)
	if !ok {
		return time.Time{}, fmt.Errorf("npm registry time entry for %s@%s is not a string", pkg, version)
	}
	t, err := pool.ParseTimestamp(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("npm registry time entry for %s@%s: %w", pkg, version, err)
	}
	return t, nil
}

// npmVersionDist is the subset of one packument version manifest that
// backfillIntegrity reads: the registry's own SRI value for the version's
// tarball.
type npmVersionDist struct {
	Dist struct {
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

// npmVersionEngines is the subset of one packument version manifest
// checkEnginesNode reads: the package author's own "engines.node" semver
// range for that version (design/harness/node-runtime-tracking.md WP2 item
// 5, issue #38's class of bug — a harness release calling a node API newer
// than the pool's bundled runtime).
type npmVersionEngines struct {
	Engines struct {
		Node string `json:"node"`
	} `json:"engines"`
}

// checkEnginesNode evaluates pkg@version's packument-declared engines.node
// constraint (if any) against nodeVersion — the exact patch this resolve is
// pinning the harness's runtime to (Resolve's own nodeVersion parameter,
// selectNodeVersion's choice one layer up) — failing outright on a genuine
// mismatch (design doc: "catching #38's class at update time instead of
// harness startup"). packuments is the SAME per-Resolve-call cache
// backfillIntegrity already threads through this method's caller, so a
// package that also needed integrity backfill fetches its packument only
// once per Resolve call.
//
// A missing engines field, or an engines.node value this package's
// Masterminds/semver dialect cannot parse as a constraint, is a non-event:
// the design doc is explicit that this check must never fail an update
// merely because it couldn't understand the declaration, only because it
// definitively understood one and found a real mismatch.
func (n *NPM) checkEnginesNode(ctx context.Context, packuments map[string]*npmPackument, pkg, version, nodeVersion string) error {
	doc, ok := packuments[pkg]
	if !ok {
		var err error
		doc, err = n.fetchPackument(ctx, pkg)
		if err != nil {
			return err
		}
		packuments[pkg] = doc
	}
	raw, ok := doc.Versions[version]
	if !ok {
		return nil
	}
	var manifest npmVersionEngines
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest.Engines.Node == "" {
		return nil
	}
	constraint, err := semver.NewConstraint(manifest.Engines.Node)
	if err != nil {
		return nil
	}
	nv, err := semver.NewVersion(nodeVersion)
	if err != nil {
		return nil
	}
	if constraint.Check(nv) {
		return nil
	}
	return fmt.Errorf(
		"%s@%s requires node %s, but the pool's resolved node runtime is %s (line %d); moving this harness to a different Node line is a wraptool code change, not a config option",
		pkg, version, manifest.Engines.Node, nodeVersion, nv.Major())
}

// lockKeyPackageName extracts the package name from a package-lock.json
// "packages" key: everything after the key's last "node_modules/" segment,
// which for nested entries like
// "node_modules/a/node_modules/@scope/b" is the scoped name "@scope/b".
func lockKeyPackageName(key string) (string, bool) {
	const marker = "node_modules/"
	idx := strings.LastIndex(key, marker)
	if idx < 0 {
		return "", false
	}
	name := key[idx+len(marker):]
	return name, name != ""
}

// backfillIntegrity recovers the missing "integrity" of a lockfile entry
// whose tarball nonetheless lives on this source's registry. npm copies a
// dependency's bundled npm-shrinkwrap.json into the consumer's lockfile
// verbatim, and a shrinkwrap published from a workspace monorepo records the
// author's workspace-sibling packages with a registry "resolved" URL but no
// "integrity" (npm never backfills it at install time) — e.g.
// @earendil-works/pi-coding-agent ships exactly this shape for pi-ai/pi-tui/
// pi-agent-core. The registry's own dist.integrity for that exact
// name@version is the same trust root every other entry's integrity came
// from, so fetching it preserves the everything-pinned-by-hash invariant;
// Fetch still verifies the downloaded tarball against it, so a resolved URL
// pointing at different bytes fails there. Anything else missing integrity —
// a git URL, a non-registry host, a version the registry doesn't know —
// stays a hard error here.
//
// packuments caches one fetch per package name across a single Resolve call;
// the caller owns the map.
func (n *NPM) backfillIntegrity(ctx context.Context, packuments map[string]*npmPackument, key string, entry npmLockPackageEntry) (string, error) {
	if !strings.HasPrefix(entry.Resolved, n.registryBaseURL+"/") {
		return "", fmt.Errorf(
			"package-lock.json entry %q has resolved %q but no integrity (git or non-registry dependency cannot be locked)",
			key, entry.Resolved)
	}
	pkg, ok := lockKeyPackageName(key)
	if !ok || entry.Version == "" {
		return "", fmt.Errorf(
			"package-lock.json entry %q has no integrity and no package name/version to backfill it from the registry",
			key)
	}
	doc := packuments[pkg]
	if doc == nil {
		var err error
		doc, err = n.fetchPackument(ctx, pkg)
		if err != nil {
			return "", fmt.Errorf("backfilling integrity for %q: %w", key, err)
		}
		packuments[pkg] = doc
	}
	raw, ok := doc.Versions[entry.Version]
	if !ok {
		return "", fmt.Errorf(
			"backfilling integrity for %q: registry has no version %s of %s",
			key, entry.Version, pkg)
	}
	var manifest npmVersionDist
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return "", fmt.Errorf(
			"backfilling integrity for %q: decoding registry manifest for %s@%s: %w",
			key, pkg, entry.Version, err)
	}
	if manifest.Dist.Integrity == "" {
		return "", fmt.Errorf(
			"backfilling integrity for %q: registry dist for %s@%s carries no integrity",
			key, pkg, entry.Version)
	}
	return manifest.Dist.Integrity, nil
}

// patchLockfileIntegrity sets packages[key].integrity = integrity for every
// (key, integrity) pair in backfilled, on the RAW package-lock.json document
// — never on the typed npmLockFile struct below. npmLockFile only decodes
// four fields per entry (version/resolved/integrity/link); round-tripping
// through it and re-marshaling would silently drop every field it doesn't
// know about (os, cpu, optional, dependencies, engines, license, ...), which
// a real npm lockfile entry commonly carries and Realize's `npm ci` depends
// on (see NPMLock.Lockfile's doc comment). This function instead
// round-trips raw through a generic map[string]any, so every field —
// known or not — survives untouched except the one being patched.
//
// The result is deterministic for a given (raw, backfilled) pair:
// encoding/json's Marshal sorts map[string]any keys alphabetically at every
// nesting level, so two Resolve calls that backfill the same entries produce
// byte-identical output (modulo raw's own content, which a real registry
// response holds fixed for a given package@version). Every JSON number in
// raw is decoded via json.Number (see the UseNumber() call below), not
// float64, so an untouched number field's original decimal text — including
// one beyond float64's 2^53 exact-integer range — survives the round trip
// unchanged rather than being reformatted (or silently losing precision) on
// re-Marshal.
func patchLockfileIntegrity(raw []byte, backfilled map[string]string) ([]byte, error) {
	// UseNumber() keeps every JSON number as a json.Number (its original
	// decimal text) instead of decoding it into a float64 — plain
	// json.Unmarshal into map[string]any would otherwise silently round-trip
	// any large or high-precision number (a future npm field, a package
	// size, ...) through float64, losing precision above 2^53 and risking
	// scientific-notation reformatting on re-Marshal. json.Marshal writes a
	// json.Number back out verbatim, so this keeps every untouched number
	// byte-identical while still letting the one "integrity" string field get
	// patched.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decoding raw lockfile: %w", err)
	}
	packagesAny, ok := doc["packages"]
	if !ok {
		return nil, fmt.Errorf(`raw lockfile has no top-level "packages" key`)
	}
	packages, ok := packagesAny.(map[string]any)
	if !ok {
		return nil, fmt.Errorf(`raw lockfile "packages" is not a JSON object`)
	}
	for key, integrity := range backfilled {
		entryAny, ok := packages[key]
		if !ok {
			return nil, fmt.Errorf("raw lockfile has no packages entry %q to patch", key)
		}
		entry, ok := entryAny.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("raw lockfile packages entry %q is not a JSON object", key)
		}
		entry["integrity"] = integrity
	}
	patched, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("re-marshaling patched raw lockfile: %w", err)
	}
	return patched, nil
}

// npmLockFile is the subset of package-lock.json (lockfileVersion 3) this
// package reads.
type npmLockFile struct {
	LockfileVersion int                            `json:"lockfileVersion"`
	Packages        map[string]npmLockPackageEntry `json:"packages"`
}

// npmLockPackageEntry is one "packages" map entry. Link marks an npm
// workspace symlink entry, which — like a git dependency — has no
// resolved/integrity pair and cannot be locked. HasShrinkwrap marks a
// package that bundles its own npm-shrinkwrap.json (npm sets this on the
// CONSUMER lockfile's entry for that package, e.g.
// @earendil-works/pi-coding-agent) — a real npm install reifies that
// package's own dependency subtree from the shrinkwrap embedded in its
// extracted tarball at install time, not from this outer lockfile's
// "packages" map; those nested fetches need npm's URL-indexed HTTP cache
// (populated by `npm install`'s network fetch), which `npm cache add`
// (content-only, keyed by integrity) never populates. This is a SEPARATE
// defect class from the missing-integrity one backfillIntegrity fixes:
// the shrinkwrap embedded in the published tarball permanently lacks
// integrity fields, so Realize must seed npm's URL cache index for the
// locked (url, integrity) pairs before `npm ci --offline` — this flag is
// the sole gate on that seeding step (see NPMLock.HasShrinkwrap's doc
// comment and seedShrinkwrapCacheIndex in the sibling realize.go).
type npmLockPackageEntry struct {
	Version       string `json:"version"`
	Resolved      string `json:"resolved"`
	Integrity     string `json:"integrity"`
	Link          bool   `json:"link"`
	HasShrinkwrap bool   `json:"hasShrinkwrap"`
}

// validateLockableIntegrity checks an npm_lock candidate's non-empty
// Integrity string against the one SRI shape this package can act on: a
// single "sha512-<base64>" hash. A pre-2017 "sha1-..." package is named
// explicitly (sha1 predates npm's sha512 integrity requirement, so its
// presence usually means an old, otherwise-unlockable dependency); a
// whitespace-separated multi-hash SRI value (the W3C SRI spec allows
// several algorithms per entry) is rejected outright rather than silently
// picking one. Fetch's own parseIntegrity performs the same algorithm check
// again as a backstop — this is the primary, entry-naming validation at
// lock-production time.
func validateLockableIntegrity(sri string) error {
	if len(strings.Fields(sri)) > 1 {
		return fmt.Errorf("multi-hash SRI not supported: %q", sri)
	}
	algo, _, ok := strings.Cut(sri, "-")
	if !ok {
		return fmt.Errorf("malformed integrity value %q", sri)
	}
	switch algo {
	case "sha512":
		return nil
	case "sha1":
		return fmt.Errorf("sha1 integrity %q is not supported (pre-2017 npm packages are not lockable)", sri)
	default:
		return fmt.Errorf("unsupported integrity algorithm %q (want sha512)", algo)
	}
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
// this method neither constructs a Backend nor selects guix/OCI. Resolve
// does not clean up scratchDir (a subdirectory of workDir) when it returns;
// workDir is a staging transaction directory the caller owns and disposes
// of.
//
// A lockfile entry carrying a registry resolved URL but no integrity — the
// shape npm writes when honoring a dependency's bundled npm-shrinkwrap.json
// whose workspace-sibling entries were published without one — has its
// integrity backfilled from the registry's dist metadata (see
// backfillIntegrity); every other incomplete entry fails resolution loudly.
//
// version must be the exact registry version key — a pool.Candidate.Version
// as Refresh returns it — never "v"-prefixed. Unlike nodeVersion below,
// Resolve does not normalize version: npm package versions are bare semver
// (registry packument keys, package.json "dependencies" values, and
// package-lock.json "packages"[...]."version" are all written without a "v"),
// so there is no ambiguous convention here to accommodate, only a caller
// mistake to surface plainly. Passing a "v"-prefixed version would make npm
// resolve a different (likely nonexistent) version than the one the caller
// meant, so the resulting mismatch between the requested version and the
// lockfile's actual top-level entry version surfaces as this method's own
// version-mismatch error rather than as a confusing failure that looks like
// a lockfile problem.
//
// nodeVersion is the exact Node runtime version the harness descriptor
// requires (a leading "v", if present, is stripped before use — nodejs.org
// URLs and this method's own filenames add it back); the descriptor itself
// lives in internal/harness, which this package does not import, so the
// caller resolves the descriptor's runtime requirement to a concrete
// version and passes it through. recipeRevision is the orchestrator-computed
// "sha256:<hex>" value (§8: recipe hashing is the orchestrator's
// responsibility, not this Source's); Resolve stores it on the returned
// LockHarness verbatim.
//
// PublishedAt is populated from a fresh registry lookup (not a
// caller-supplied Candidate, which may be stale); unlike Refresh, Resolve
// fails outright when the registry has no time entry for version (see
// versionPublishedAt) rather than returning a zero timestamp, since
// published_at is a required lock property (§8).
//
// FirstSeenAt is always the zero-formatted timestamp on the returned value;
// the orchestrator fills it in from Observations before persisting the lock.
// Resolve deliberately never calls (*pool.Lock).Validate — a LockHarness
// with a zero FirstSeenAt is expected and legitimate at this stage, and
// Validate would reject it.
func (n *NPM) Resolve(ctx context.Context, b backend.Backend, pkg, version, workDir, nodeVersion, recipeRevision string) (pool.LockHarness, error) {
	nodeVersion = strings.TrimPrefix(nodeVersion, "v")

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

	// Clear any pre-existing package-lock.json (a stale file from a reused
	// scratch dir, or one an earlier failed run left behind) so a backend
	// that exits 0 without actually writing a fresh lockfile can never leave
	// Resolve reading stale or foreign content.
	lockPath := filepath.Join(scratchDir, "package-lock.json")
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		return pool.LockHarness{}, fmt.Errorf("clearing stale package-lock.json: %w", err)
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
	lockBytes, err := os.ReadFile(lockPath)
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

	// Iterate entries in sorted key order so a lockfile with several
	// unlockable entries always reports the same (lexicographically first)
	// one — map iteration order would otherwise name a different entry on
	// every run.
	names := make([]string, 0, len(lockFile.Packages))
	for name := range lockFile.Packages {
		names = append(names, name)
	}
	sort.Strings(names)

	// Packument cache shared by every backfillIntegrity call in this resolve.
	packuments := make(map[string]*npmPackument)

	// backfilled records every entry (by its "packages" map key) whose
	// integrity came from backfillIntegrity rather than npm's own lockfile
	// output. When non-empty after the loop below, the RAW lockfile document
	// (not just the summary map built here) is patched with the same values
	// before compaction — see the patchLockfileIntegrity call below and
	// NPMLock.Lockfile's doc comment for why the raw document must carry it
	// too (realize-side `npm ci --offline` reads Lockfile, not this summary
	// map).
	backfilled := make(map[string]string)

	// hasShrinkwrap becomes true the moment ANY entry in the lockfile carries
	// npm's own "hasShrinkwrap" marker — see npmLockPackageEntry.HasShrinkwrap
	// and NPMLock.HasShrinkwrap's doc comments. Recording it (rather than
	// acting on it here) is what lets Realize run its cache-index seeding
	// step for exactly these harnesses: seedShrinkwrapCacheIndex in the
	// sibling realize.go is gated solely on this flag.
	hasShrinkwrap := false

	packages := make(map[string]pool.NPMLockPackage, len(lockFile.Packages))
	for _, name := range names {
		entry := lockFile.Packages[name]
		if name == "" {
			// The "" key is the synthesized project root itself, not a
			// dependency — it has no resolved/integrity by construction and
			// is not part of the harness's dependency graph.
			continue
		}
		if entry.HasShrinkwrap {
			hasShrinkwrap = true
		}
		if entry.Link {
			return pool.LockHarness{}, fmt.Errorf(
				"package-lock.json entry %q is a workspace link (cannot be locked)", name)
		}
		if entry.Resolved == "" {
			return pool.LockHarness{}, fmt.Errorf(
				"package-lock.json entry %q has no resolved URL (git dependency, workspace link, or file: dependency cannot be locked)",
				name)
		}
		if entry.Integrity == "" {
			integrity, err := n.backfillIntegrity(ctx, packuments, name, entry)
			if err != nil {
				return pool.LockHarness{}, err
			}
			entry.Integrity = integrity
			backfilled[name] = integrity
		}
		if err := validateLockableIntegrity(entry.Integrity); err != nil {
			return pool.LockHarness{}, fmt.Errorf("package-lock.json entry %q: %w", name, err)
		}
		packages[name] = pool.NPMLockPackage{
			Version:   entry.Version,
			Resolved:  entry.Resolved,
			Integrity: entry.Integrity,
		}
	}

	// Cross-check that the exact package@version we asked npm to resolve
	// actually appears as the top-level dependency in the returned graph —
	// catches a backend/lockfile mismatch (wrong scratch dir, stale mount)
	// rather than silently locking whatever happened to be there.
	topLevelKey := "node_modules/" + pkg
	topLevel, ok := packages[topLevelKey]
	if !ok {
		return pool.LockHarness{}, fmt.Errorf("package-lock.json has no top-level entry for %s (expected key %q)", pkg, topLevelKey)
	}
	if topLevel.Version != version {
		return pool.LockHarness{}, fmt.Errorf("package-lock.json entry %q has version %q, want %q", topLevelKey, topLevel.Version, version)
	}

	// §8.1 step 4: pinned runtime requirement + per-platform artifacts.
	publishedAt, err := n.versionPublishedAt(ctx, packuments, pkg, version)
	if err != nil {
		return pool.LockHarness{}, err
	}
	if err := n.checkEnginesNode(ctx, packuments, pkg, version, nodeVersion); err != nil {
		return pool.LockHarness{}, err
	}
	runtimeArtifacts, err := n.nodeRuntimeArtifacts(ctx, nodeVersion)
	if err != nil {
		return pool.LockHarness{}, err
	}

	// When any entry above needed backfillIntegrity, the RAW document must
	// carry that same integrity too — not just the summary map built above —
	// since Realize materializes THIS field verbatim as package-lock.json and
	// a realize-side `npm ci --offline` matches its offline cache by
	// integrity (see NPMLock.Lockfile's doc comment and
	// patchLockfileIntegrity's own doc comment for why this is a generic
	// map[string]any patch, never a round trip through the typed npmLockFile
	// struct above).
	rawLock := lockBytes
	if len(backfilled) > 0 {
		patched, err := patchLockfileIntegrity(lockBytes, backfilled)
		if err != nil {
			return pool.LockHarness{}, fmt.Errorf("patching backfilled integrity into raw package-lock.json: %w", err)
		}
		rawLock = patched
	}

	// Store the verbatim (or, when backfilled, semantically-verbatim — see
	// NPMLock.Lockfile's doc comment) package-lock.json alongside the
	// summary map, so Realize can materialize npm's own output byte-for-byte
	// instead of hand-reconstructing one (§8, review finding: a
	// hand-reconstructed lockfile loses optional/os/cpu gating and nested
	// version-conflict edges that only the full document carries). Compacted
	// once here via json.Compact — NOT for persisted-file compactness
	// (pool.Lock.Marshal's indenting encoder re-indents an embedded
	// json.RawMessage's bytes when it writes harness-lock.json to disk
	// regardless of how they were stored, so compaction buys nothing there),
	// but for DETERMINISM: (*LockHarness).Hash marshals with encoding/json's
	// plain, non-indenting Marshal, which embeds a RawMessage's bytes AS-IS
	// with no reformatting at all. Without compacting here, two
	// functionally-identical lockfiles that happened to arrive with
	// different (but semantically equivalent) whitespace would hash
	// differently, breaking Hash's "equal lock entries hash equal" contract.
	var compactedLock bytes.Buffer
	if err := json.Compact(&compactedLock, rawLock); err != nil {
		return pool.LockHarness{}, fmt.Errorf("compacting package-lock.json: %w", err)
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
			Lockfile:        append([]byte(nil), compactedLock.Bytes()...),
			HasShrinkwrap:   hasShrinkwrap,
		},
	}, nil
}

// nodeRuntimeArtifacts fetches nodeVersion's SHASUMS256.txt and returns the
// glibc-platform artifact map (URL + sha256) described on nodeGlibcPlatforms.
// nodeVersion must already have any leading "v" stripped (Resolve does this
// once for its whole call).
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
		if !validSHA256Hex(sha256Hex) {
			return nil, fmt.Errorf("SHASUMS256.txt for node v%s: malformed digest for %s", nodeVersion, filename)
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
// filename, used for binary-mode checksums, is stripped). Digest shape is
// validated by the caller (nodeRuntimeArtifacts) at the point each specific
// entry is used, not here — this method returns whatever the file contains,
// including filenames this package never looks up.
func (n *NPM) fetchNodeSHASUMS(ctx context.Context, nodeVersion string) (map[string]string, error) {
	u := fmt.Sprintf("%s/v%s/SHASUMS256.txt", n.nodeDistBaseURL, nodeVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", u, err)
	}
	resp, err := n.metadataClient.Do(req)
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
// algorithm and decoded digest. This is Fetch's own backstop check; the
// primary, entry-naming validation happens in Resolve's conversion loop via
// validateLockableIntegrity.
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
// cacheDir must be the flat, content-addressed cache root — pool
// CacheSHA256Dir() (cache/sha256), not CacheDir() — since every object is
// written directly under it by its sha256 hex name. Passing CacheDir()
// instead does not error visibly; it silently makes every subsequent lookup
// a permanent cache miss (nothing is ever found at the expected path),
// so a caller must get this right.
//
// An npm package entry is verified against its upstream sha512 SRI
// Integrity; its Sha256 field is filled in when empty, or checked for an
// exact match when already set. An entry with neither Integrity nor Sha256
// set is rejected before any request is made — Resolve should never produce
// one (its conversion loop already requires both fields), but Fetch does not
// trust that invariant blindly and re-checks it as an independent guard. A
// runtime artifact has no SRI and is verified against its already-recorded
// Sha256 (populated by Resolve from nodejs.org's SHASUMS256.txt). A cacheDir
// object already present at the expected sha256-named path (size > 0) is
// trusted as-is and the download is skipped entirely — content-addressed
// storage means a name match already is a content match (§6.4).
func (n *NPM) Fetch(ctx context.Context, h *pool.LockHarness, cacheDir string) error {
	if h == nil {
		return fmt.Errorf("fetch: nil LockHarness")
	}

	if h.NPMLock != nil {
		for name, pkg := range h.NPMLock.Packages {
			if pkg.Resolved == "" {
				return fmt.Errorf("npm package %q: no resolved URL recorded in lock", name)
			}
			if pkg.Integrity == "" && pkg.SHA256 == "" {
				return fmt.Errorf("npm package %q: neither integrity nor sha256 recorded in lock — nothing to verify against", name)
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
			if artifact.SHA256 == "" {
				return fmt.Errorf("runtime artifact %s: no sha256 recorded in lock — nothing to verify against", key)
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
// sri is non-empty, allocated only in that case). It returns the downloaded
// object's hex sha256 digest.
//
//   - If knownSHA256 is non-empty, it must already match sha256HexPattern —
//     it is about to be used as a cache filename, and this is the only line
//     of defense against a malformed digest (e.g. one containing "../")
//     reaching the filesystem as a path component.
//   - If knownSHA256 is non-empty and cacheDir already holds an object at
//     that name with size > 0, the object is trusted and no request is made.
//   - If sri is non-empty, the streamed sha512 must match its decoded digest
//     or the temp file is discarded and an error is returned — nothing is
//     admitted to cacheDir on a mismatch.
//   - If knownSHA256 is non-empty, the streamed sha256 must equal it or the
//     temp file is discarded and an error is returned.
//   - On success the temp file's permissions are set to 0644, it is fsynced,
//     and renamed to cacheDir/<sha256hex> (same directory, so the rename
//     stays on one volume — the same discipline as pool.WriteFileAtomic, but
//     streaming rather than whole-buffer since artifacts can be large).
func (n *NPM) downloadVerify(ctx context.Context, url, sri, knownSHA256, cacheDir string) (string, error) {
	if knownSHA256 != "" {
		if !validSHA256Hex(knownSHA256) {
			return "", fmt.Errorf("malformed sha256 %q recorded in lock", knownSHA256)
		}
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
	resp, err := n.downloadClient.Do(req)
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
	var sha512h hash.Hash
	if sri != "" {
		sha512h = sha512.New()
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
