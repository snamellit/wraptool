package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/pti/wraptool/internal/pool/backend"
)

// npmSourceName is the fixed Observations/Lock "source" identity for every
// Phase-1 harness (design/unified-harness-pool.md §16 Phase 1 only wires npm
// resolution, §8.1). It is not exported: callers never need to spell it, they
// just name a package.
const npmSourceName = "npm"

// defaultNodeVersion is the pinned Node runtime every Phase-1 npm harness
// resolves against, passed to Source.Resolve. §8.1 describes selecting this
// from "the descriptor-declared runtime requirement (node major)"; Phase 1's
// internal/harness.Descriptor carries no such field yet (this work package's
// brief scopes Descriptor's only addition to PoolPackage), and every current
// pool-lockable harness (claude, opencode, pi) is fine on one modern LTS, so
// a single constant is the Phase-1 answer. A later phase that needs
// per-harness runtime pinning promotes this to a descriptor or lock field.
const defaultNodeVersion = "22"

// recipeFormatVersion is bumped whenever this package's Phase-1 install
// recipe (the legacy npm-install-pinned-to-an-exact-version path, driven
// from cmd/harness.go) changes in a way that should invalidate a previously
// computed recipe_revision, so a resolved-and-locked generation can't be
// silently reused under changed realization assumptions (§8).
const recipeFormatVersion = 1

// npmRecipeRevision computes recipe_revision (§8: "sha256 over the
// descriptor's serialized install recipe + the backend provisioning-image
// digest (§11.2) + a recipe-format version constant") for one npm package.
// Phase 1 has no backend provisioning image to fold in — §16 Phase 1 keeps
// the actual harness install on the legacy, non-backend npm path, only
// resolve/lock/fetch move to the pool — so this is a deliberate
// simplification: package identity + recipeFormatVersion only. It still
// changes the moment either input does, which is the property recipe_revision
// exists to guarantee.
func npmRecipeRevision(pkg string) (string, error) {
	data, err := json.Marshal(struct {
		Package string `json:"package"`
		Format  int    `json:"recipe_format_version"`
	}{Package: pkg, Format: recipeFormatVersion})
	if err != nil {
		return "", fmt.Errorf("marshaling recipe for hashing: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Source is the local artifact-acquisition seam this package's command-level
// workflows call through (design/unified-harness-pool.md §11.3). Its method
// set is pinned to match internal/pool/source's npm Source implementation
// (a sibling work package built in parallel), so that concrete type satisfies
// this interface by Go's structural typing alone once both land — this file
// deliberately does not import internal/pool/source.
type Source interface {
	// Refresh lists candidate versions for pkg from registry metadata (host,
	// pure Go, no backend — §6.2). Every returned Candidate's FirstSeenAt is
	// the zero value; RefreshHarness fills it in from this pool's own
	// observations (§6.6: first_seen_at is pool-local, write-once state, not
	// something a registry can report).
	Refresh(ctx context.Context, pkg string) ([]Candidate, error)
	// Resolve produces the complete lock entry for one exact (pkg, version),
	// running inside b (registry-only network, §6.2/§8.1). workDir is a
	// scratch directory the Source may use for the synthesized package.json
	// / package-lock.json (§8.1 steps 1-3). nodeVersion and recipeRevision
	// are supplied by this package, not discovered by Source: nodeVersion is
	// the pinned Node runtime this harness resolves against (defaultNodeVersion,
	// a Phase-1 constant — see its doc comment), and recipeRevision is this
	// package's own recipe hash (npmRecipeRevision, §8's recipe_revision).
	Resolve(ctx context.Context, b backend.Backend, pkg, version, workDir, nodeVersion, recipeRevision string) (LockHarness, error)
	// Fetch downloads every artifact h describes into cacheDir, verifying
	// integrity before admission (host, pure Go, locked URLs only — §6.2).
	// It MUTATES h in place: each npm package entry's pool-local SHA256
	// cache-address field is filled (existing ones re-verified) as objects
	// are admitted to cache, so UpdateHarness calls Fetch before persisting
	// h into harness-lock.json, never after.
	Fetch(ctx context.Context, h *LockHarness, cacheDir string) error
}

// PoolPaths bundles the pool file locations one orchestrator command
// operates on. Production callers build one from the real pool root's
// layout accessors (Root, ManifestFile, LockFile, ...); tests build one
// from t.TempDir() so no orchestrator test depends on the developer's real
// $XDG_DATA_HOME — mirroring how the rest of this package's tests pass
// explicit paths rather than calling Root() (see layout_test.go's siblings).
type PoolPaths struct {
	Manifest     string
	Lock         string
	Observations string
	Audit        string
	AdvisoryLock string
	Cache        string
}

// UpdateOptions configures one resolve+lock+fetch pass (§9 phases 2-3),
// shared by the `update` and `install` commands (§10) — the only two
// commands allowed to advance a harness's locked version. `install`
// additionally writes the manifest constraint before calling UpdateHarness;
// that step lives in cmd, not here (§7's manifest edit is a cmd-owned
// yaml.Node editor, kept out of this package per this work package's brief).
type UpdateOptions struct {
	// Harness is the manifest/lock map key, e.g. "claude".
	Harness string
	// Package is the source package identity, e.g. "@anthropic-ai/claude-code".
	Package string
	// Now is the command's single clock read, threaded through instead of
	// calling time.Now() internally (§6.6, and this package's own test-
	// clock-injection convention).
	Now time.Time
	// WorkDir is passed through to Source.Resolve unchanged (§8.1 scratch dir).
	WorkDir string
	// AllowYoung bypasses the release/observation age gates (§9 "Emergency
	// security updates"). Requires a non-empty Reason.
	AllowYoung bool
	Reason     string
}

// RefreshHarness runs the metadata-only refresh phase (§9 phase 1, §6.2): it
// asks src for pkg's candidate versions, merges them into observations.json
// (first-seen stamped with now for keys observations.json has never seen
// before — MergeObservations/Observe's write-once rule, §6.6 — so a second
// refresh with a later now never moves an existing first_seen_at), and
// returns the candidates annotated with the FirstSeenAt now on record (which,
// for a version seen on a prior refresh, is that earlier timestamp, not
// now). Refresh never touches the lock; it takes the pool lock for its own
// full duration only (§6.5).
func RefreshHarness(ctx context.Context, src Source, paths PoolPaths, pkg string, now time.Time) ([]Candidate, error) {
	lock := NewPoolLock(paths.AdvisoryLock)
	if err := lock.Acquire(ctx); err != nil {
		return nil, err
	}
	defer lock.Release()

	candidates, err := src.Refresh(ctx, pkg)
	if err != nil {
		return nil, fmt.Errorf("refreshing %s: %w", pkg, err)
	}

	toMerge := make(Observations, len(candidates))
	for _, c := range candidates {
		firstSeen := c.FirstSeenAt
		if firstSeen.IsZero() {
			firstSeen = now
		}
		key := ObservationKey(npmSourceName, pkg, c.Version)
		if err := toMerge.Observe(key, FormatTimestamp(firstSeen), FormatTimestamp(c.PublishedAt), c.Prerelease, c.Yanked); err != nil {
			return nil, fmt.Errorf("recording observation for %s@%s: %w", pkg, c.Version, err)
		}
	}
	if err := MergeObservations(paths.Observations, toMerge); err != nil {
		return nil, fmt.Errorf("merging observations: %w", err)
	}

	// Re-load so the returned candidates carry the true (possibly older, per
	// the write-once rule above) first_seen_at rather than this call's now.
	stored, err := LoadObservations(paths.Observations)
	if err != nil {
		return nil, fmt.Errorf("reloading observations: %w", err)
	}
	out := make([]Candidate, len(candidates))
	for i, c := range candidates {
		key := ObservationKey(npmSourceName, pkg, c.Version)
		if obs, ok := stored[key]; ok {
			if fsa, err := ParseTimestamp(obs.FirstSeenAt); err == nil {
				c.FirstSeenAt = fsa
			}
		}
		out[i] = c
	}
	return out, nil
}

// UpdateHarness runs the resolve+lock+fetch phases (§9 phases 2-3, §6.2) for
// one harness: it loads the manifest strictly, resolves the effective policy,
// filters this pool's observed candidates for opts.Package down to those
// eligible under that policy (pool.Eligible) and matching the manifest's
// version constraint (Masterminds/semver), and picks the newest of those.
//
// If the pool's existing lock already pins that exact (package, version) for
// opts.Harness, UpdateHarness returns it unchanged and reports changed=false
// without calling src.Resolve or src.Fetch — the §10 idempotence contract
// ("install is idempotent when manifest, lock, ... agree. It no longer means
// 'run npm latest again.'"), which applies equally whether the caller is
// `install` (which writes manifest intent first, in cmd) or `update`.
//
// Otherwise it resolves the candidate via src.Resolve, stamps FirstSeenAt
// (pool-local, unknown to Source) onto the returned LockHarness, then fetches
// via src.Fetch BEFORE writing the lock — deliberately not §9's textual
// phase order. src.Fetch mutates its LockHarness argument in place, filling
// (and verifying) each artifact's pool-local sha256 cache-address field as it
// admits objects to cache (§8's "the pool's own sha256"); a lock persisted
// before Fetch runs would be missing those fields. Only once Fetch succeeds
// does UpdateHarness stamp PolicyHash/GeneratedAt onto the Lock, validate,
// and atomically write harness-lock.json — so a Fetch failure (including a
// partial one) leaves the previous lock file completely untouched, never a
// half-fetched resolution.
//
// opts.AllowYoung bypasses the release/observation age gates for candidate
// selection (§9's emergency bypass) and requires a non-empty opts.Reason; it
// always appends an audit.jsonl event when used, independent of whether the
// selected candidate actually needed the bypass. The whole call holds the
// pool lock for its full duration (§6.5); it must never be called while
// another PoolLock-holding call on the same pool is still in flight in the
// same process (that nests two flocks on one path and deadlocks) — callers
// composing refresh-then-update (e.g. `install`) call RefreshHarness and
// UpdateHarness sequentially, never nested.
func UpdateHarness(ctx context.Context, src Source, b backend.Backend, paths PoolPaths, opts UpdateOptions) (harness *LockHarness, changed bool, err error) {
	if opts.AllowYoung && opts.Reason == "" {
		return nil, false, fmt.Errorf("--allow-young requires a non-empty --reason")
	}

	lock := NewPoolLock(paths.AdvisoryLock)
	if err := lock.Acquire(ctx); err != nil {
		return nil, false, err
	}
	defer lock.Release()

	m, err := LoadManifest(paths.Manifest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, fmt.Errorf("no pool manifest yet; run `wraptool harness install %s` first", opts.Harness)
		}
		return nil, false, err
	}
	hm, ok := m.Harnesses[opts.Harness]
	if !ok {
		return nil, false, fmt.Errorf("harness %q has no manifest entry; run `wraptool harness install %s` first", opts.Harness, opts.Harness)
	}
	policy := m.EffectivePolicy(opts.Harness)

	obs, err := LoadObservations(paths.Observations)
	if err != nil {
		return nil, false, fmt.Errorf("loading observations: %w", err)
	}
	candidates, err := candidatesForPackage(obs, opts.Package)
	if err != nil {
		return nil, false, err
	}

	selectPolicy := policy
	if opts.AllowYoung {
		selectPolicy.MinimumReleaseAge = 0
		selectPolicy.MinimumObservationAge = 0
	}
	selected, err := selectNewestEligible(candidates, hm.Constraint, selectPolicy, opts.Now)
	if err != nil {
		return nil, false, fmt.Errorf("harness %q: %w", opts.Harness, err)
	}

	if opts.AllowYoung {
		remaining, _ := QuarantineRemaining(selected, policy, opts.Now)
		if err := AppendAudit(paths.Audit, "allow_young", map[string]any{
			"harness":              opts.Harness,
			"package":              opts.Package,
			"version":              selected.Version,
			"reason":               opts.Reason,
			"remaining_quarantine": remaining.String(),
		}); err != nil {
			return nil, false, fmt.Errorf("recording allow-young audit event: %w", err)
		}
	}

	recipeRevision, err := npmRecipeRevision(opts.Package)
	if err != nil {
		return nil, false, err
	}

	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		return nil, false, err
	}
	// §10 idempotence: manifest (already reflected in `selected`), lock, and
	// recipe must all agree — a recipe_revision mismatch (e.g. a wraptool
	// upgrade changed recipeFormatVersion) must NOT be treated as
	// already-satisfied, per §8's stated purpose for recipe_revision
	// ("changed wraptool installer logic cannot silently reuse a generation
	// built under different assumptions").
	if existing, ok := lockDoc.Harnesses[opts.Harness]; ok &&
		existing.Package == opts.Package && existing.Version == selected.Version &&
		existing.RecipeRevision == recipeRevision {
		out := existing
		return &out, false, nil
	}

	resolved, err := src.Resolve(ctx, b, opts.Package, selected.Version, opts.WorkDir, defaultNodeVersion, recipeRevision)
	if err != nil {
		return nil, false, fmt.Errorf("resolving %s@%s: %w", opts.Package, selected.Version, err)
	}
	resolved.FirstSeenAt = FormatTimestamp(selected.FirstSeenAt)

	// Fetch BEFORE writing the lock: it mutates resolved in place, filling
	// (and verifying) each artifact's pool-local sha256 cache-address field
	// as it admits objects to cache — a lock persisted before Fetch runs
	// would be missing the sha256 values §8 requires. A Fetch failure
	// (including a partial one, e.g. the second of three npm tarballs) must
	// leave the pool exactly as it was: since the previous lock file has not
	// been touched yet at this point, returning here does that for free —
	// no half-fetched resolution is ever persisted.
	if err := src.Fetch(ctx, &resolved, paths.Cache); err != nil {
		return nil, false, fmt.Errorf("fetching %s@%s: %w", opts.Package, selected.Version, err)
	}

	policyHash, err := PolicyHash(policy)
	if err != nil {
		return nil, false, err
	}
	lockDoc.SchemaVersion = LockSchemaVersion
	lockDoc.GeneratedAt = FormatTimestamp(opts.Now)
	lockDoc.PolicyHash = policyHash
	if lockDoc.Harnesses == nil {
		lockDoc.Harnesses = map[string]LockHarness{}
	}
	lockDoc.Harnesses[opts.Harness] = resolved
	if err := lockDoc.Validate(); err != nil {
		return nil, false, fmt.Errorf("validating lock: %w", err)
	}
	data, err := lockDoc.Marshal()
	if err != nil {
		return nil, false, err
	}
	if err := WriteFileAtomic(paths.Lock, data, 0o644); err != nil {
		return nil, false, err
	}

	out := resolved
	return &out, true, nil
}

// SyncHarness realizes the pool's exact existing lock entry for harnessName:
// it never calls Source.Resolve (§9's Resolve phase is skipped entirely —
// sync "resolves NOTHING"), only Source.Fetch, to backfill any cache objects
// missing since the lock was written (new workstation, cache loss, §10). It
// errors if no lock entry exists for harnessName; sync has nothing to
// realize from in that case. Holds the pool lock for its full duration (§6.5).
func SyncHarness(ctx context.Context, src Source, paths PoolPaths, harnessName string) (*LockHarness, error) {
	lock := NewPoolLock(paths.AdvisoryLock)
	if err := lock.Acquire(ctx); err != nil {
		return nil, err
	}
	defer lock.Release()

	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		return nil, err
	}
	lh, ok := lockDoc.Harnesses[harnessName]
	if !ok {
		return nil, fmt.Errorf("no lock entry for harness %q; run `wraptool harness install %s` first", harnessName, harnessName)
	}
	if err := src.Fetch(ctx, &lh, paths.Cache); err != nil {
		return nil, fmt.Errorf("fetching %s: %w", harnessName, err)
	}
	return &lh, nil
}

// HarnessStatus is one harness's row of `wraptool harness list` (§10): the
// developer's desired constraint, what's locked, the newest known version
// satisfying that constraint, the newest of those that's actually eligible
// today, and — when the newest known version isn't eligible yet purely
// because of the age gates — that version and how much longer it must wait.
type HarnessStatus struct {
	Harness    string
	Constraint string // "" if the harness has no manifest entry yet
	Locked     string // "" if never locked
	Latest     string // "" if no observed candidate satisfies the constraint
	Eligible   string // "" if no observed candidate is both eligible and satisfies the constraint

	// QuarantinedVersion is set when Latest is not yet eligible purely due to
	// the age gates (i.e. Latest != Eligible and QuarantineRemaining reports
	// a meaningful countdown for it); "" otherwise, including when Latest ==
	// Eligible (nothing is quarantined) or Latest is empty.
	QuarantinedVersion  string
	QuarantineRemaining time.Duration
	// QuarantineOK mirrors pool.QuarantineRemaining's ok: false means
	// QuarantinedVersion carries a timestamp that makes it unconditionally
	// ineligible (no countdown to render), true means QuarantineRemaining is
	// a meaningful duration.
	QuarantineOK bool
}

// StatusHarness computes one harness's list row (§10) from the pool's
// current manifest, lock, and observations — read-only, no pool lock taken
// (§6.5: "list takes no lock"). A missing manifest or lock file is treated
// as empty (nothing installed yet), not an error, so `list` works before any
// `install` has ever run.
func StatusHarness(paths PoolPaths, harnessName, pkg string, now time.Time) (HarnessStatus, error) {
	status := HarnessStatus{Harness: harnessName}

	m, err := loadManifestOrEmpty(paths.Manifest)
	if err != nil {
		return status, err
	}
	if hm, ok := m.Harnesses[harnessName]; ok {
		status.Constraint = hm.Constraint
	}
	policy := m.EffectivePolicy(harnessName)

	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		return status, err
	}
	if lh, ok := lockDoc.Harnesses[harnessName]; ok {
		status.Locked = lh.Version
	}

	obs, err := LoadObservations(paths.Observations)
	if err != nil {
		return status, err
	}
	candidates, err := candidatesForPackage(obs, pkg)
	if err != nil {
		return status, err
	}

	var constraintObj *semver.Constraints
	if status.Constraint != "" {
		constraintObj, err = semver.NewConstraint(status.Constraint)
		if err != nil {
			return status, fmt.Errorf("harness %q: invalid constraint %q: %w", harnessName, status.Constraint, err)
		}
	}

	var latest, eligibleBest *versionedCandidate
	for _, c := range candidates {
		v, verr := semver.NewVersion(c.Version)
		if verr != nil {
			continue
		}
		if constraintObj != nil && !constraintObj.Check(v) {
			continue
		}
		if c.Yanked {
			continue
		}
		if c.Prerelease && !policy.AllowPrereleases {
			continue
		}
		if latest == nil || v.GreaterThan(latest.ver) {
			latest = &versionedCandidate{c, v}
		}
		if Eligible(c, policy, now) && (eligibleBest == nil || v.GreaterThan(eligibleBest.ver)) {
			eligibleBest = &versionedCandidate{c, v}
		}
	}
	if latest != nil {
		status.Latest = latest.cand.Version
	}
	if eligibleBest != nil {
		status.Eligible = eligibleBest.cand.Version
	}
	if latest != nil && (eligibleBest == nil || latest.ver.GreaterThan(eligibleBest.ver)) {
		if remaining, ok := QuarantineRemaining(latest.cand, policy, now); ok {
			status.QuarantinedVersion = latest.cand.Version
			status.QuarantineRemaining = remaining
			status.QuarantineOK = true
		}
	}
	return status, nil
}

// versionedCandidate pairs a Candidate with its parsed semver.Version so
// StatusHarness's scan doesn't reparse Version on every comparison.
type versionedCandidate struct {
	cand Candidate
	ver  *semver.Version
}

// candidatesForPackage extracts every observed candidate for pkg (source
// "npm") out of the flat Observations map, parsing each "npm/<pkg>/<version>"
// key back into a Candidate ready for Eligible/QuarantineRemaining. Matching
// is a literal prefix check against "npm/<pkg>/", not a path split, so a
// scoped npm package name (which itself contains "/") is handled correctly.
func candidatesForPackage(obs Observations, pkg string) ([]Candidate, error) {
	prefix := npmSourceName + "/" + pkg + "/"
	var out []Candidate
	for key, o := range obs {
		version, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		publishedAt, err := ParseTimestamp(o.PublishedAt)
		if err != nil {
			return nil, fmt.Errorf("observation %q: published_at: %w", key, err)
		}
		firstSeenAt, err := ParseTimestamp(o.FirstSeenAt)
		if err != nil {
			return nil, fmt.Errorf("observation %q: first_seen_at: %w", key, err)
		}
		out = append(out, Candidate{
			Version:     version,
			PublishedAt: publishedAt,
			FirstSeenAt: firstSeenAt,
			Prerelease:  o.Prerelease,
			Yanked:      o.Yanked,
		})
	}
	return out, nil
}

// selectNewestEligible picks the newest candidate that both satisfies
// constraint (empty means "any version", per §7 "Empty constraint means the
// descriptor's stable channel") and is Eligible under policy at now. When no
// candidate qualifies, it returns an error naming the newest
// constraint-satisfying candidate and, when that candidate's ineligibility is
// purely a matter of waiting, how much longer via QuarantineRemaining (§9's
// requirement that a failed update names the quarantined candidate and its
// remaining time).
func selectNewestEligible(candidates []Candidate, constraintStr string, policy EffectivePolicy, now time.Time) (Candidate, error) {
	var constraintObj *semver.Constraints
	if constraintStr != "" {
		c, err := semver.NewConstraint(constraintStr)
		if err != nil {
			return Candidate{}, fmt.Errorf("invalid constraint %q: %w", constraintStr, err)
		}
		constraintObj = c
	}

	var newest, bestEligible *versionedCandidate
	for i := range candidates {
		cand := candidates[i]
		v, err := semver.NewVersion(cand.Version)
		if err != nil {
			continue // not a valid semver version; not selectable
		}
		if constraintObj != nil && !constraintObj.Check(v) {
			continue
		}
		if newest == nil || v.GreaterThan(newest.ver) {
			newest = &versionedCandidate{cand, v}
		}
		if Eligible(cand, policy, now) && (bestEligible == nil || v.GreaterThan(bestEligible.ver)) {
			bestEligible = &versionedCandidate{cand, v}
		}
	}

	if bestEligible != nil {
		return bestEligible.cand, nil
	}
	if newest == nil {
		if constraintStr == "" {
			return Candidate{}, fmt.Errorf("no observed candidate; run `wraptool harness refresh` first")
		}
		return Candidate{}, fmt.Errorf("no observed candidate satisfies constraint %q; run `wraptool harness refresh` first", constraintStr)
	}
	if remaining, ok := QuarantineRemaining(newest.cand, policy, now); ok {
		return Candidate{}, fmt.Errorf("no eligible candidate satisfies constraint %q: newest matching candidate %s is quarantined, %s remaining",
			constraintStr, newest.cand.Version, remaining)
	}
	return Candidate{}, fmt.Errorf("no eligible candidate satisfies constraint %q: newest matching candidate %s is not eligible (yanked, or a disallowed prerelease)",
		constraintStr, newest.cand.Version)
}

// loadManifestOrEmpty loads the manifest, treating a missing file as an
// empty one (no `install` has ever run) rather than an error — appropriate
// for read paths (StatusHarness) that must work before any pool mutation.
// Mutating paths that need a real, already-created manifest (UpdateHarness)
// use LoadManifest directly and surface its error.
func loadManifestOrEmpty(path string) (*Manifest, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return &Manifest{SchemaVersion: ManifestSchemaVersion, Harnesses: map[string]HarnessManifest{}}, nil
		}
		return nil, fmt.Errorf("checking manifest: %w", err)
	}
	return LoadManifest(path)
}

// loadLockOrEmpty reads and validates harness-lock.json, treating a missing
// file as an empty lock (nothing resolved yet) rather than an error.
func loadLockOrEmpty(path string) (*Lock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Lock{SchemaVersion: LockSchemaVersion, Harnesses: map[string]LockHarness{}}, nil
		}
		return nil, fmt.Errorf("reading lock: %w", err)
	}
	return UnmarshalLock(data)
}
