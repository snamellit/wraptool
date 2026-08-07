package pool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// LockSchemaVersion is the schema_version this package writes and
// understands for harness-lock.json (§8).
const LockSchemaVersion = 1

// Lock is the generated, exact resolution of every harness the manifest
// selects (§8) — harness-lock.json. It contains everything realization needs
// without consulting a mutable version selector: exact version, integrity for
// every artifact, the complete transitive dependency graph (npm sources), and
// a receipt digest per realized platform.
//
// This package defines the shape and (de)serialization only. Producing a Lock
// (resolve mechanics, §8.1) is a later work package's Source implementation.
type Lock struct {
	SchemaVersion int                    `json:"schema_version"`
	GeneratedAt   string                 `json:"generated_at"` // UTC RFC3339
	PolicyHash    string                 `json:"policy_hash"`  // "sha256:<hex>"
	Harnesses     map[string]LockHarness `json:"harnesses"`
}

// LockHarness is one harness's exact resolution.
type LockHarness struct {
	Source         string       `json:"source"`  // e.g. "npm"
	Package        string       `json:"package"` // e.g. "@anthropic-ai/claude-code"
	Version        string       `json:"version"`
	PublishedAt    string       `json:"published_at"`    // UTC RFC3339
	FirstSeenAt    string       `json:"first_seen_at"`   // UTC RFC3339
	RecipeRevision string       `json:"recipe_revision"` // "sha256:<hex>"
	Runtime        *LockRuntime `json:"runtime,omitempty"`
	NPMLock        *NPMLock     `json:"npm_lock,omitempty"`
	// Archive pins a direct-download ("archive") source's per-platform
	// artifact (design/harness/unified-harness-pool.md §6.2 phase table: an
	// archive source resolves and realizes in pure Go, no backend). Mutually
	// exclusive with NPMLock/Runtime in practice (Source is either "npm" or
	// "archive" for a given harness), but nothing here enforces that — the
	// two fields simply describe different sources' additive lock shapes.
	Archive *LockArchive `json:"archive,omitempty"`
	// Generations maps a platform key to its receipt digest ("sha256:<hex>"),
	// present once that platform has been realized (§11.5). Keyed by the
	// closed PlatformKey enum (not a bare string) so a lock read back from
	// disk cannot silently carry an unrecognized platform; encoding/json
	// still sorts these by their string form, so Marshal stays deterministic.
	Generations map[PlatformKey]string `json:"generations,omitempty"`
}

// LockRuntime pins the bundled language runtime (e.g. Node) a harness needs,
// per platform.
type LockRuntime struct {
	Name      string                              `json:"name"`
	Version   string                              `json:"version"`
	Artifacts map[PlatformKey]LockRuntimeArtifact `json:"artifacts"` // platform key -> artifact
}

// LockRuntimeArtifact is one platform's downloadable runtime artifact.
type LockRuntimeArtifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// LockArchive is a direct-download ("archive") source's locked artifacts,
// one per realized platform (§6.2, §11.3: the archive Source implementation
// lives in internal/pool/source). Unlike NPMLock (a full transitive
// dependency graph), an archive artifact is a single downloadable file per
// platform — there is no dependency resolution to lock, only the exact
// artifact URL and its integrity.
type LockArchive struct {
	Artifacts map[PlatformKey]LockArchiveArtifact `json:"artifacts"`
}

// LockArchiveArtifact is one platform's locked, direct-download artifact.
// SHA512 is the upstream release channel's own integrity value for the
// artifact (hex-encoded, unlike npm's base64 SRI strings — see the archive
// Source's parsing) — the trust root Fetch verifies the download against,
// mirroring NPMLockPackage.Integrity. SHA256 is the pool's own cache
// address (cache/sha256/<digest>), empty until Fetch downloads and admits
// the artifact, mirroring NPMLockPackage.SHA256's split between upstream
// integrity and pool-local addressing.
type LockArchiveArtifact struct {
	URL    string `json:"url"`
	SHA512 string `json:"sha512"`
	SHA256 string `json:"sha256,omitempty"`
}

// NPMLock embeds the resolved npm dependency graph for one harness (§8.1):
// exact versions, resolved tarball URLs, and both the upstream sha512 SRI
// integrity value and the pool's own sha256 cache address.
type NPMLock struct {
	LockfileVersion int                       `json:"lockfile_version"`
	Packages        map[string]NPMLockPackage `json:"packages"`
	// Lockfile is the verbatim package-lock.json bytes Resolve read back from
	// the resolve container (compacted once via json.Compact at Resolve time
	// — see Hash's doc comment for why a canonical, whitespace-normalized
	// form matters). This is the only faithful realization source: a
	// hand-reconstructed lockfile built from Packages alone (name, version,
	// resolved, integrity — no more) provably loses information npm's own
	// installer depends on — optional/os/cpu gating (an os-mismatched
	// optionalDependency with no recorded "os" field becomes
	// unconditionally required on every platform) and nested
	// version-conflict edges (a package installed under another package's
	// own node_modules/, reachable only through that parent's own
	// "dependencies" entry, never from the root). Packages is NOT
	// superseded by this field: it remains Fetch's cache-addressing source
	// of truth (resolved URL, upstream integrity, pool sha256) and
	// Realize's cache-object verification list; Lockfile is realization's
	// materialization source, Packages is fetch/cache's. Empty on a lock
	// produced before this field existed — Realize must re-resolve rather
	// than degrade silently (see internal/pool/source's Realize).
	//
	// When Resolve had to backfill a missing "integrity" (a bundled
	// npm-shrinkwrap.json workspace-sibling entry, see npm.go's
	// backfillIntegrity), that value is ALSO patched into this raw document
	// — a realize-side `npm ci --offline` matches its offline cache by
	// integrity, so a raw entry missing it would pass Fetch/cache-object
	// verification (which checks Packages, not this field) and then fail
	// realize with an opaque ENOTCACHED. The patch is a generic
	// map[string]any round trip (json.Unmarshal into map[string]any, set
	// the one field, json.Marshal back), never a round trip through the
	// typed npmLockFile struct — that struct only knows
	// version/resolved/integrity/link and would silently drop every other
	// field (os/cpu/optional/dependencies/engines/license, ...) a real
	// lockfile entry carries. This means a backfilled lock's Lockfile is
	// only SEMANTICALLY verbatim (same fields, same values, npm-Marshal's
	// own key ordering/number formatting) rather than byte-verbatim from
	// npm's own output — still deterministic (encoding/json sorts
	// map[string]any keys), just not the identical byte sequence npm wrote.
	// A lock with no backfilled entries keeps npm's original bytes exactly.
	Lockfile json.RawMessage `json:"lockfile,omitempty"`
	// HasShrinkwrap is true when ANY raw "packages" entry carries npm's own
	// "hasShrinkwrap" marker (e.g. @earendil-works/pi-coding-agent). A real
	// npm install reifies a shrinkwrapped package's own dependency subtree
	// from the npm-shrinkwrap.json embedded in its extracted tarball, not
	// from this outer lockfile's "packages" map; those nested fetches go
	// through npm's URL-indexed HTTP cache (make-fetch-happen) rather than
	// the plain content-addressed shortcut a normal lockfile-driven install
	// takes. NPM.Realize (internal/pool/source/realize.go) handles this by
	// seeding that URL index from this same lock's already-verified
	// (resolved URL, integrity) pairs before running `npm ci --offline`
	// (seedShrinkwrapCacheIndex) — this field is what gates that extra,
	// npm-internals-coupled step to the harnesses that actually need it,
	// rather than paying its cost (and its npm-version-fragility) on every
	// realize. It used to also gate a hard realize-time refusal (a
	// currently-unfixable defect class, at the time); that refusal is gone
	// now that seeding lets a shrinkwrapped harness realize like any other.
	// If seeding one day stops working (an npm internals change), the
	// ordinary realize-failure path still applies: non-fatal RealizeErr warn
	// on install/update, hard error on sync — nothing here special-cases
	// that outcome. omitempty since false is the overwhelmingly common case
	// and this field postdates many existing locks.
	HasShrinkwrap bool `json:"has_shrinkwrap,omitempty"`
}

// NPMLockPackage is one entry of the npm dependency graph.
type NPMLockPackage struct {
	Version   string `json:"version"`
	Resolved  string `json:"resolved"`
	Integrity string `json:"integrity"` // upstream sha512 SRI
	SHA256    string `json:"sha256"`    // pool cache address (cache/sha256/<digest>)
}

// Marshal serializes the lock deterministically. encoding/json already sorts
// map keys (string-kind, which includes PlatformKey) and struct field order
// is fixed by the type definition, so repeated calls on an equal value
// produce byte-identical output (§17 "deterministic lock serialization").
// SetEscapeHTML(false) avoids the encoder silently rewriting characters like
// "<" (present in semver constraints and npm scoped package names) into
// <, which would otherwise make the lock harder to read and diff.
func (l *Lock) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(l); err != nil {
		return nil, fmt.Errorf("marshaling lock: %w", err)
	}
	// json.Encoder.Encode appends a trailing newline; strip it so Marshal's
	// output shape matches a plain json.MarshalIndent caller would expect.
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// UnmarshalLock parses lock JSON produced by Marshal and validates it via
// Validate: every platform key (in Generations and runtime Artifacts) must
// be one of the closed PlatformKey values, and every timestamp must be a
// non-zero UTC RFC3339 string (§6.6). A lock that fails either check is
// rejected outright rather than silently accepted with a value that later
// code would mishandle.
func UnmarshalLock(data []byte) (*Lock, error) {
	var l Lock
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("parsing lock: %w", err)
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return &l, nil
}

// Validate checks generated_at, every harness's published_at/first_seen_at,
// and every platform key referenced in Generations/runtime Artifacts.
// Exported so a caller assembling a Lock in memory (Wave 2's resolve/writer,
// which produces a Lock without ever round-tripping it through JSON) can
// validate it before persisting, not only after reading it back via
// UnmarshalLock (which also calls Validate).
func (l *Lock) Validate() error {
	if err := validateLockTimestamp("generated_at", l.GeneratedAt); err != nil {
		return err
	}
	for name, h := range l.Harnesses {
		if err := validateLockTimestamp(fmt.Sprintf("harness %q: published_at", name), h.PublishedAt); err != nil {
			return err
		}
		if err := validateLockTimestamp(fmt.Sprintf("harness %q: first_seen_at", name), h.FirstSeenAt); err != nil {
			return err
		}
		for key := range h.Generations {
			if _, err := ParsePlatformKey(string(key)); err != nil {
				return fmt.Errorf("harness %q: generations: %w", name, err)
			}
		}
		if h.Runtime != nil {
			for key := range h.Runtime.Artifacts {
				if _, err := ParsePlatformKey(string(key)); err != nil {
					return fmt.Errorf("harness %q: runtime artifacts: %w", name, err)
				}
			}
		}
		if h.NPMLock != nil {
			if err := validateNPMLockfile(h.NPMLock); err != nil {
				return fmt.Errorf("harness %q: %w", name, err)
			}
		}
		if h.Archive != nil {
			for key, artifact := range h.Archive.Artifacts {
				if _, err := ParsePlatformKey(string(key)); err != nil {
					return fmt.Errorf("harness %q: archive artifacts: %w", name, err)
				}
				if err := validateArchiveArtifact(key, artifact); err != nil {
					return fmt.Errorf("harness %q: %w", name, err)
				}
			}
		}
	}
	return nil
}

// validateArchiveArtifact checks one LockArchiveArtifact's shape (§8 ruling:
// "SHA512 = upstream manifest integrity, hex, validated shape; SHA256 =
// pool cache address"): URL must be https, SHA512 must be exactly 128
// lowercase hex characters (a raw SHA-512 digest, hex-encoded — unlike
// npm's base64 SRI strings), and SHA256, when present, must be exactly 64
// lowercase hex characters (empty is expected and valid before Fetch has
// run).
func validateArchiveArtifact(key PlatformKey, a LockArchiveArtifact) error {
	if !strings.HasPrefix(a.URL, "https://") {
		return fmt.Errorf("archive artifact %s: url %q must be https", key, a.URL)
	}
	if !isLowerHexOfLen(a.SHA512, 128) {
		return fmt.Errorf("archive artifact %s: sha512 must be 128 lowercase hex characters", key)
	}
	if a.SHA256 != "" && !isLowerHexOfLen(a.SHA256, 64) {
		return fmt.Errorf("archive artifact %s: sha256 must be empty or 64 lowercase hex characters", key)
	}
	return nil
}

// isLowerHexOfLen reports whether s is exactly n lowercase hexadecimal
// characters. Kept local to this file (a separate check from receipt.go's
// isLowerHex64, which is fixed at 64 characters) rather than generalizing
// that helper — receipt.go is a sibling work package's file this package
// does not touch.
func isLowerHexOfLen(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// npmRawLockfilePackageEntry is the subset of one raw "packages" map entry
// validateNPMLockfile/RawComplete read — deliberately not the source
// package's npmLockPackageEntry (this package does not import
// internal/pool/source, and importing it here would be the wrong direction
// anyway: source depends on pool, not the reverse).
type npmRawLockfilePackageEntry struct {
	Resolved  string `json:"resolved"`
	Integrity string `json:"integrity"`
	Link      bool   `json:"link"`
}

// validateNPMLockfile checks n.Lockfile, when present: it must be valid
// JSON, "lockfileVersion" 3, and every key in n.Packages (the summary map)
// must appear in its own "packages" map — a consistency check between the
// two, not a full structural validation of arbitrary npm lockfile content.
//
// Deliberately does NOT reject a raw entry with a resolved URL but no
// integrity (npm-raw-lockfile-backfill field lesson): a lock persisted by a
// pre-fix binary can carry exactly that shape (@earendil-works/pi-tui's
// workspace-sibling entries), and this function runs on every lock LOAD
// (UnmarshalLock), including the load `wraptool harness update` itself does
// to find the very lock it needs to replace. Rejecting it here would wedge
// that pool permanently — the "re-run wraptool harness update" advice would
// be circular, since update can't even load the lock to act on it. See
// RawComplete for the (advisory, load-tolerant) check that flags this shape
// instead of failing on it, and orchestrator.go's use of it to force a
// self-healing re-resolve. A no-op when Lockfile is empty (§ "empty on a
// lock produced before this field existed", NPMLock.Lockfile's doc comment).
func validateNPMLockfile(n *NPMLock) error {
	if len(n.Lockfile) == 0 {
		return nil
	}
	var raw struct {
		LockfileVersion int                        `json:"lockfileVersion"`
		Packages        map[string]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(n.Lockfile, &raw); err != nil {
		return fmt.Errorf("npm_lock.lockfile: invalid JSON: %w", err)
	}
	if raw.LockfileVersion != 3 {
		return fmt.Errorf("npm_lock.lockfile: lockfileVersion %d, want 3", raw.LockfileVersion)
	}

	names := make([]string, 0, len(n.Packages))
	for name := range n.Packages {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic "first missing key" in the error below
	for _, name := range names {
		if _, ok := raw.Packages[name]; !ok {
			return fmt.Errorf("npm_lock.lockfile: missing packages entry %q, which is present in the summary map", name)
		}
	}
	return nil
}

// RawComplete reports whether n.Lockfile (when present) has every
// non-root, non-link raw "packages" entry's integrity already filled in —
// the invariant Resolve's raw-lockfile patch establishes going forward (see
// npm.go's Resolve and NPMLock.Lockfile's own doc comment on backfilling).
// A lock persisted by a pre-fix binary can be syntactically valid
// (validateNPMLockfile accepts it — lock LOADING must tolerate this old
// shape, see that function's doc comment) but semantically incomplete: a
// raw entry with a resolved URL and no integrity passes Fetch/cache-object
// verification (which checks the summary map, NPMLockPackage.Integrity, not
// this raw document) and then fails realize-side `npm ci --offline` with an
// opaque ENOTCACHED, since npm matches its offline cache by integrity.
//
// RawComplete is the self-heal trigger orchestrator.go's UpdateHarnessLocked
// consults (folded into lockMatchesRecipe, the same seam a recipe-format
// bump uses to force a stale lock to re-resolve): an incomplete raw
// lockfile is treated as NOT matching the recipe, so Decide routes
// DecisionResolveVersion and a fresh Source.Resolve rewrites the entry with
// the patched raw lockfile — the pool self-heals on its very next
// install/update, with no manual step and no failed lock load. It is also
// NPM.Realize's own preflight backstop for the one path that never calls
// Resolve at all (`sync`, which realizes straight from an existing lock
// entry — see that function's doc comment).
//
// Returns true (nothing to heal) when n is nil or n.Lockfile is empty,
// mirroring validateNPMLockfile's own "empty is a no-op" contract, and when
// n.Lockfile fails to parse as JSON at all — an unparseable Lockfile is
// caught by validateNPMLockfile (a hard load error) long before any caller
// reaches RawComplete, so treating that case as "nothing to heal" here
// never masks it; it just keeps this function total rather than needing its
// own error return for a case its caller has already ruled out.
func (n *NPMLock) RawComplete() bool {
	if n == nil || len(n.Lockfile) == 0 {
		return true
	}
	var raw struct {
		Packages map[string]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(n.Lockfile, &raw); err != nil {
		return true
	}
	for name, rawEntry := range raw.Packages {
		if name == "" {
			// The synthesized project root has no resolved/integrity by
			// construction (see Resolve's own "" handling).
			continue
		}
		var entry npmRawLockfilePackageEntry
		if err := json.Unmarshal(rawEntry, &entry); err != nil {
			continue
		}
		if entry.Link {
			continue
		}
		if entry.Resolved != "" && entry.Integrity == "" {
			return false
		}
	}
	return true
}

// validateLockTimestamp parses s as a UTC RFC3339 timestamp and additionally
// rejects the zero time (year 1) — FormatTimestamp(time.Time{}) produces the
// syntactically valid string "0001-01-01T00:00:00Z", which ParseTimestamp
// alone would accept, but an unset time must never survive into a persisted
// lock (N5).
func validateLockTimestamp(label, s string) error {
	t, err := ParseTimestamp(s)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if t.IsZero() {
		return fmt.Errorf("%s: timestamp is unset (zero value)", label)
	}
	return nil
}

// Hash returns "sha256:<hex>" over h's plain json.Marshal encoding (default
// HTML escaping left on, unlike Lock.Marshal's SetEscapeHTML(false) — this
// value is only ever hashed, never written to disk or diffed, so the
// escaping is immaterial as long as it is consistent). That encoding is
// still deterministic across calls on an equal value: encoding/json sorts
// h.Generations' and h.NPMLock.Packages' map keys, and struct field order is
// fixed by the type. It is the receipt's lock_entry_hash input (§11.5): a
// generation's receipt records exactly which lock entry produced it, so two
// lock entries that are equal in every field hash equal, and changing any
// field changes the hash. h must be non-nil: json.Marshal(nil) encodes to
// the 4 bytes "null" without error, which would otherwise let a nil
// *LockHarness hash silently instead of failing.
func (h *LockHarness) Hash() (string, error) {
	if h == nil {
		return "", fmt.Errorf("hashing lock harness entry: receiver must not be nil")
	}
	data, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("marshaling lock harness entry for hashing: %w", err)
	}
	sum := sha256.Sum256(data)
	return sha256DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// policyHashPayload is the canonical, minimal representation of an
// EffectivePolicy hashed by PolicyHash. Keeping it separate from
// EffectivePolicy insulates the hash from unrelated field additions to the
// policy struct that don't change quarantine semantics.
type policyHashPayload struct {
	MinimumReleaseAge     string `json:"minimum_release_age"`
	MinimumObservationAge string `json:"minimum_observation_age"`
	AllowPrereleases      bool   `json:"allow_prereleases"`
}

// PolicyHash returns "sha256:<hex>" over the canonical serialization of an
// effective policy (§8 "policy_hash", §9 "Effective policy and its hash are
// recorded in lock and audit output"). Two EffectivePolicy values compare
// equal under PolicyHash iff their three fields are equal.
func PolicyHash(p EffectivePolicy) (string, error) {
	data, err := json.Marshal(policyHashPayload{
		MinimumReleaseAge:     p.MinimumReleaseAge.String(),
		MinimumObservationAge: p.MinimumObservationAge.String(),
		AllowPrereleases:      p.AllowPrereleases,
	})
	if err != nil {
		return "", fmt.Errorf("marshaling policy for hashing: %w", err)
	}
	sum := sha256.Sum256(data)
	return sha256DigestPrefix + hex.EncodeToString(sum[:]), nil
}
