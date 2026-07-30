package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/uuid"

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
	// / package-lock.json (§8.1 steps 1-3) — always non-empty (UpdateHarness
	// creates one under staging/ when the caller didn't supply one; the
	// Source seam never sees ""). nodeVersion and recipeRevision are supplied
	// by this package, not discovered by Source: nodeVersion is the pinned
	// Node runtime this harness resolves against (defaultNodeVersion, a
	// Phase-1 constant — see its doc comment), and recipeRevision is this
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
	// Staging roots scratch work directories UpdateHarness creates itself
	// when a caller's UpdateOptions.WorkDir is empty (§5 layout:
	// staging/txn-<id>/, never mounted into project containers).
	Staging string
}

// LockPool acquires the pool's single exclusive advisory lock (§6.5) for
// paths and returns a release func the caller must defer-call exactly once
// when done. It exists so a cmd-level command spanning several orchestrator
// calls (e.g. `install`: write manifest intent, refresh, update, then run
// the legacy installer) acquires the pool lock ONCE for its whole span,
// rather than each call acquiring and releasing its own — critically,
// because github.com/gofrs/flock is not reentrant in-process: a second
// open-file-description flock on the same path from the same process blocks
// on the first, already-held one, so nesting two of this package's
// lock-acquiring calls deadlocks instead of composing. The exported
// RefreshHarness/UpdateHarness/SyncHarness each still acquire their own lock
// for a single, self-contained call; their *Locked siblings assume the
// caller already holds one obtained via LockPool and must never acquire
// another.
func LockPool(ctx context.Context, paths PoolPaths) (release func(), err error) {
	lock := NewPoolLock(paths.AdvisoryLock)
	if err := lock.Acquire(ctx); err != nil {
		return nil, err
	}
	return func() { _ = lock.Release() }, nil
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
	// WorkDir is passed through to Source.Resolve. Empty means
	// UpdateHarness creates and owns a scratch directory itself, under
	// paths.Staging, removed unconditionally when the call returns.
	WorkDir string
	// AllowYoung bypasses the release/observation age gates (§9 "Emergency
	// security updates"). Requires a non-empty Reason.
	AllowYoung bool
	Reason     string
	// Advance selects between install's and update's differing idempotence
	// semantics (§10: "update is the only normal command that advances a
	// floating constraint"). install passes false, update passes true.
	//
	//   - false (install): if the existing lock already matches
	//     opts.Package and the freshly computed recipe_revision, AND its
	//     version satisfies the manifest constraint, that is treated as
	//     fully realized intent — a pure no-op with NO candidate scan at
	//     all, even when a newer eligible version exists. Install's job is
	//     to realize existing intent, not to advance it.
	//   - true (update): always scans candidates first, regardless of
	//     whether the existing lock already matches. If the scan selects a
	//     version different from what's locked, that is an advance
	//     (resolve+fetch+lock). If it selects the SAME version as already
	//     locked (with matching package+recipe), that's a no-op for a
	//     different reason: nothing better to advance to. If nothing is
	//     eligible, the existing rules apply (keep an existing
	//     constraint-satisfying lock with a Note, or error if none exists).
	Advance bool
}

// UpdateResult is UpdateHarness's outcome.
type UpdateResult struct {
	// Harness is the harness's lock entry after the call — freshly resolved
	// when Changed, otherwise whatever was already locked (kept as-is).
	Harness *LockHarness
	// Changed is true only when Source.Resolve/Fetch actually ran and a new
	// lock entry was written.
	Changed bool
	// Note is a human-readable explanation for a changed=false result that
	// isn't the plain "everything already agrees" case — specifically, a
	// kept lock because no candidate is currently eligible (§9). Empty
	// otherwise.
	Note string
	// AllowYoungApplied is true when opts.AllowYoung actually changed which
	// candidate got selected — i.e. the selected candidate would not have
	// been Eligible under the real, non-bypassed policy. Requesting
	// --allow-young when it wasn't needed leaves this false: "only an
	// effective bypass gets audited" (§9).
	AllowYoungApplied bool
	// CandidateAge and CandidateIntegrity are populated only alongside
	// AllowYoungApplied — now minus the selected candidate's published_at,
	// and the resolved package's upstream integrity value when known — for
	// the caller to print per §9 ("prints candidate age and integrity").
	CandidateAge       time.Duration
	CandidateIntegrity string
}

// RefreshHarness runs the metadata-only refresh phase (§9 phase 1, §6.2)
// under its own, self-contained pool lock acquisition. See RefreshHarnessLocked
// for the behavior; this wrapper exists for a caller that isn't already
// composing multiple orchestrator calls under one LockPool span.
func RefreshHarness(ctx context.Context, src Source, paths PoolPaths, pkg string, now time.Time) ([]Candidate, error) {
	release, err := LockPool(ctx, paths)
	if err != nil {
		return nil, err
	}
	defer release()
	return RefreshHarnessLocked(ctx, src, paths, pkg, now)
}

// RefreshHarnessLocked is RefreshHarness's body, assuming the caller already
// holds the pool lock (via LockPool) — it never acquires one itself. It asks
// src for pkg's candidate versions, merges them into observations.json
// (first-seen stamped with now for keys observations.json has never seen
// before — MergeObservations/Observe's write-once rule, §6.6 — so a second
// refresh with a later now never moves an existing first_seen_at), and
// returns the candidates annotated with the FirstSeenAt now on record (which,
// for a version seen on a prior refresh, is that earlier timestamp, not
// now). Refresh never touches the lock, and never calls Source.Resolve or
// Source.Fetch.
func RefreshHarnessLocked(ctx context.Context, src Source, paths PoolPaths, pkg string, now time.Time) ([]Candidate, error) {
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
// one harness under its own, self-contained pool lock acquisition. See
// UpdateHarnessLocked for the behavior; this wrapper exists for a caller
// that isn't already composing multiple orchestrator calls under one
// LockPool span.
func UpdateHarness(ctx context.Context, src Source, b backend.Backend, paths PoolPaths, opts UpdateOptions) (*UpdateResult, error) {
	release, err := LockPool(ctx, paths)
	if err != nil {
		return nil, err
	}
	defer release()
	return UpdateHarnessLocked(ctx, src, b, paths, opts)
}

// UpdateHarnessLocked is UpdateHarness's body, assuming the caller already
// holds the pool lock (via LockPool) — it never acquires one itself.
//
// opts.Advance selects between install's and update's differing idempotence
// semantics (§10: "update is the only normal command that advances a
// floating constraint" — see UpdateOptions.Advance's doc comment):
//
//  1. install (Advance=false): if the pool's existing lock entry for
//     opts.Harness already matches opts.Package and the freshly computed
//     recipe_revision, AND its version still satisfies the manifest's
//     constraint, this is a pure no-op: changed=false, no candidates are
//     even scanned. This is §10's "install is idempotent when manifest,
//     lock, and recipe agree" — it does not care whether a NEWER version
//     would also be eligible; install realizes existing intent, it does not
//     go looking for something better.
//  2. update (Advance=true) ALWAYS scans first, so it notices a newer
//     eligible candidate or a widened constraint that now admits one. If the
//     scan selects a version different from what's locked (or the recipe
//     changed even at the same version), that's an advance: resolve, fetch,
//     write a new lock entry. If the scan selects the SAME version already
//     locked (package+recipe also unchanged), that's a no-op for a
//     different reason — changed=false, no Note (nothing to advance to).
//  3. In BOTH modes, when the scan finds nothing eligible: a lock that
//     already exists for this package and still satisfies the constraint is
//     kept rather than erroring (changed=false, with an informative Note
//     naming the newest quarantined candidate and its remaining wait, §9).
//     A hard error is returned only when there is no usable existing lock
//     AND nothing is eligible.
//
// When a fresh resolve does happen: it stamps FirstSeenAt (pool-local,
// unknown to Source) onto the resolved LockHarness, then calls Source.Fetch
// BEFORE writing the lock — deliberately not §9's textual phase order.
// Source.Fetch mutates its LockHarness argument in place, filling (and
// verifying) each artifact's pool-local sha256 cache-address field as it
// admits objects to cache (§8's "the pool's own sha256"); a lock persisted
// before Fetch runs would be missing those fields. Only once Fetch succeeds
// does UpdateHarnessLocked stamp PolicyHash/GeneratedAt onto the Lock,
// validate, and atomically write harness-lock.json — so a Fetch failure
// (including a partial one) leaves the previous lock file completely
// untouched, never a half-fetched resolution.
//
// opts.AllowYoung bypasses the release/observation age gates for candidate
// selection (§9's emergency bypass) and requires a non-empty opts.Reason.
// Only an "effective" bypass — one that actually changed which candidate got
// selected — is audited (appended to audit.jsonl, after the successful lock
// write) or reported via UpdateResult.AllowYoungApplied; requesting
// --allow-young when the normally-selected candidate was eligible anyway is
// a no-op bypass and neither prints nor audits anything extra.
//
// opts.WorkDir, when empty, is filled with a fresh staging/txn-<id> scratch
// directory that is always removed (success or failure) before returning —
// Source.Resolve is never called with an empty workDir.
func UpdateHarnessLocked(ctx context.Context, src Source, b backend.Backend, paths PoolPaths, opts UpdateOptions) (*UpdateResult, error) {
	if opts.AllowYoung && opts.Reason == "" {
		return nil, fmt.Errorf("--allow-young requires a non-empty --reason")
	}

	m, err := LoadManifest(paths.Manifest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no pool manifest yet; run `wraptool harness install %s` first", opts.Harness)
		}
		return nil, err
	}
	hm, ok := m.Harnesses[opts.Harness]
	if !ok {
		return nil, fmt.Errorf("harness %q has no manifest entry; run `wraptool harness install %s` first", opts.Harness, opts.Harness)
	}
	policy := m.EffectivePolicy(opts.Harness)

	var constraintObj *semver.Constraints
	if hm.Constraint != "" {
		constraintObj, err = semver.NewConstraint(hm.Constraint)
		if err != nil {
			return nil, fmt.Errorf("harness %q: invalid constraint %q: %w", opts.Harness, hm.Constraint, err)
		}
	}

	recipeRevision, err := npmRecipeRevision(opts.Package)
	if err != nil {
		return nil, err
	}

	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		return nil, err
	}
	existing, hasLock := lockDoc.Harnesses[opts.Harness]
	lockSatisfiesConstraint := false
	if hasLock {
		if v, verr := semver.NewVersion(existing.Version); verr == nil {
			lockSatisfiesConstraint = constraintObj == nil || constraintObj.Check(v)
		}
	}

	lockMatchesRecipe := hasLock && existing.Package == opts.Package && existing.RecipeRevision == recipeRevision

	// install's strict short-circuit (Advance=false only): skip scanning
	// entirely when intent is already fully realized, even if a newer
	// eligible candidate exists — install does not advance (§10).
	if !opts.Advance && lockMatchesRecipe && lockSatisfiesConstraint {
		out := existing
		return &UpdateResult{Harness: &out, Changed: false}, nil
	}

	obs, err := LoadObservations(paths.Observations)
	if err != nil {
		return nil, fmt.Errorf("loading observations: %w", err)
	}
	candidates, err := candidatesForPackage(obs, opts.Package)
	if err != nil {
		return nil, err
	}

	selectPolicy := policy
	if opts.AllowYoung {
		selectPolicy.MinimumReleaseAge = 0
		selectPolicy.MinimumObservationAge = 0
	}
	selected, selErr := selectNewestEligible(candidates, hm.Constraint, selectPolicy, opts.Now)
	if selErr != nil {
		// Nothing eligible right now (even with a bypass applied), in EITHER
		// mode. Keep the existing lock instead of erroring, as long as it's
		// for the same package and its version still satisfies the
		// constraint — a recipe mismatch doesn't matter here, since nothing
		// new is being resolved. Hard-error only when there is no usable
		// lock to fall back to.
		if hasLock && existing.Package == opts.Package && lockSatisfiesConstraint {
			note := fmt.Sprintf("keeping %s", existing.Version)
			if ver, remaining, ok := quarantinedCandidate(candidates, hm.Constraint, policy, opts.Now); ver != "" {
				if ok {
					note += fmt.Sprintf("; %s quarantined, %s remaining", ver, remaining)
				} else {
					note += fmt.Sprintf("; %s not eligible", ver)
				}
			} else {
				note += "; no eligible candidate"
			}
			out := existing
			return &UpdateResult{Harness: &out, Changed: false, Note: note}, nil
		}
		return nil, fmt.Errorf("harness %q: %w", opts.Harness, selErr)
	}

	// The scan succeeded. If it selected exactly what's already locked
	// (package+recipe unchanged too), this is a no-op for update — nothing
	// better to advance to — with no Note (unlike the scan-failure case
	// above, this isn't "held back", it's already current). For install,
	// reaching this point at all means the Advance=false short-circuit above
	// didn't fire, which only happens when something didn't already match
	// (no lock, package/recipe mismatch, or an unsatisfied constraint) — so
	// this branch mostly matters for update, but applies uniformly.
	if lockMatchesRecipe && existing.Version == selected.Version {
		out := existing
		return &UpdateResult{Harness: &out, Changed: false}, nil
	}

	// An "effective" bypass: opts.AllowYoung was set AND it's the reason
	// `selected` was reachable at all (it would not be Eligible under the
	// real, non-bypassed policy). Determined before Resolve/Fetch run, since
	// it only depends on `selected` and `policy`.
	allowYoungApplied := opts.AllowYoung && !Eligible(selected, policy, opts.Now)

	workDir := opts.WorkDir
	if workDir == "" {
		dir, cleanup, err := newScratchDir(paths.Staging)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		workDir = dir
	}

	resolved, err := src.Resolve(ctx, b, opts.Package, selected.Version, workDir, defaultNodeVersion, recipeRevision)
	if err != nil {
		return nil, fmt.Errorf("resolving %s@%s: %w", opts.Package, selected.Version, err)
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
		return nil, fmt.Errorf("fetching %s@%s: %w", opts.Package, selected.Version, err)
	}

	policyHash, err := PolicyHash(policy)
	if err != nil {
		return nil, err
	}
	lockDoc.SchemaVersion = LockSchemaVersion
	lockDoc.GeneratedAt = FormatTimestamp(opts.Now)
	lockDoc.PolicyHash = policyHash
	if lockDoc.Harnesses == nil {
		lockDoc.Harnesses = map[string]LockHarness{}
	}
	lockDoc.Harnesses[opts.Harness] = resolved
	if err := lockDoc.Validate(); err != nil {
		return nil, fmt.Errorf("validating lock: %w", err)
	}
	data, err := lockDoc.Marshal()
	if err != nil {
		return nil, err
	}
	if err := WriteFileAtomic(paths.Lock, data, 0o644); err != nil {
		return nil, err
	}

	out := resolved
	result := &UpdateResult{Harness: &out, Changed: true}

	// Only an effective bypass is audited — "only an effective bypass gets
	// audited" (§9 minor) — and only after the lock write above succeeded.
	if allowYoungApplied {
		age := opts.Now.Sub(selected.PublishedAt)
		integrity := ""
		if resolved.NPMLock != nil {
			if pkgEntry, ok := resolved.NPMLock.Packages[opts.Package]; ok {
				integrity = pkgEntry.Integrity
			}
		}
		if err := AppendAudit(paths.Audit, "allow_young", map[string]any{
			"harness":   opts.Harness,
			"package":   opts.Package,
			"version":   selected.Version,
			"reason":    opts.Reason,
			"age":       age.String(),
			"integrity": integrity,
		}); err != nil {
			return nil, fmt.Errorf("recording allow-young audit event: %w", err)
		}
		result.AllowYoungApplied = true
		result.CandidateAge = age
		result.CandidateIntegrity = integrity
	}

	return result, nil
}

// newScratchDir creates a fresh staging/txn-<id> scratch directory (§5
// layout) for Source.Resolve to use when UpdateOptions.WorkDir is empty. The
// returned cleanup func removes it unconditionally; the caller MUST defer it
// immediately so it runs on both the success and failure paths — a scratch
// dir is disposable, never part of the pool's persistent state, and never
// mounted into a project container.
func newScratchDir(stagingRoot string) (dir string, cleanup func(), err error) {
	dir = filepath.Join(stagingRoot, "txn-"+uuid.NewString())
	if err := os.MkdirAll(dir, poolDirPerm); err != nil {
		return "", nil, fmt.Errorf("creating scratch dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// SyncHarness realizes the pool's exact existing lock entry for harnessName
// under its own, self-contained pool lock acquisition. See SyncHarnessLocked
// for the behavior; this wrapper exists for a caller that isn't already
// composing multiple orchestrator calls under one LockPool span.
func SyncHarness(ctx context.Context, src Source, paths PoolPaths, harnessName string) (*LockHarness, error) {
	release, err := LockPool(ctx, paths)
	if err != nil {
		return nil, err
	}
	defer release()
	return SyncHarnessLocked(ctx, src, paths, harnessName)
}

// SyncHarnessLocked is SyncHarness's body, assuming the caller already holds
// the pool lock (via LockPool) — it never acquires one itself. It never
// calls Source.Resolve (§9's Resolve phase is skipped entirely — sync
// "resolves NOTHING"), only Source.Fetch, to backfill any cache objects
// missing since the lock was written (new workstation, cache loss, §10). It
// errors if no lock entry exists for harnessName; sync has nothing to
// realize from in that case.
func SyncHarnessLocked(ctx context.Context, src Source, paths PoolPaths, harnessName string) (*LockHarness, error) {
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

// ManifestHarnessNames returns the harness names currently listed in the
// pool manifest — i.e. those with recorded developer intent — in
// unspecified order. A missing manifest returns an empty slice, not an
// error (mirrors loadManifestOrEmpty's read-path semantics). Used by cmd's
// bare `refresh`/`update` to iterate only harnesses someone actually asked
// the pool to manage, rather than every registry-known lockable harness —
// which would otherwise needlessly seed observations.json for packages
// nobody has installed.
func ManifestHarnessNames(path string) ([]string, error) {
	m, err := loadManifestOrEmpty(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m.Harnesses))
	for name := range m.Harnesses {
		names = append(names, name)
	}
	return names, nil
}

// LockHarnessNames returns the harness names currently present in
// harness-lock.json, in unspecified order. A missing lock returns an empty
// slice, not an error. Used by cmd's bare `sync` to iterate only harnesses
// that actually have an existing lock entry to realize from.
func LockHarnessNames(path string) ([]string, error) {
	l, err := loadLockOrEmpty(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(l.Harnesses))
	for name := range l.Harnesses {
		names = append(names, name)
	}
	return names, nil
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
// repeated scans don't reparse Version on every comparison.
type versionedCandidate struct {
	cand Candidate
	ver  *semver.Version
}

// candidatesForPackage extracts every observed candidate for pkg (source
// "npm") out of the flat Observations map, parsing each "npm/<pkg>/<version>"
// key back into a Candidate ready for Eligible/QuarantineRemaining. Matching
// is a literal prefix check against "npm/<pkg>/" — the trailing slash is
// load-bearing: without it, pkg "foo" would also match a stored key for an
// unrelated package "foo-bar" ("npm/foo-bar/1.0.0" starts with "npm/foo" but
// not "npm/foo/"). This is a literal string-prefix check, not a path split,
// so a scoped npm package name (which itself contains "/") is handled
// correctly too.
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

// candidateScan is the result of scanning a candidate list against a version
// constraint: the best ELIGIBLE candidate under policy (if any), and,
// independently, the newest candidate satisfying the constraint regardless
// of eligibility — used to report what's currently being held back by
// quarantine.
type candidateScan struct {
	eligible *versionedCandidate
	newest   *versionedCandidate
}

func scanCandidatesForConstraint(candidates []Candidate, constraintStr string, policy EffectivePolicy, now time.Time) (candidateScan, error) {
	var constraintObj *semver.Constraints
	if constraintStr != "" {
		c, err := semver.NewConstraint(constraintStr)
		if err != nil {
			return candidateScan{}, fmt.Errorf("invalid constraint %q: %w", constraintStr, err)
		}
		constraintObj = c
	}

	var scan candidateScan
	for i := range candidates {
		cand := candidates[i]
		v, err := semver.NewVersion(cand.Version)
		if err != nil {
			continue // not a valid semver version; not selectable
		}
		if constraintObj != nil && !constraintObj.Check(v) {
			continue
		}
		if scan.newest == nil || v.GreaterThan(scan.newest.ver) {
			scan.newest = &versionedCandidate{cand, v}
		}
		if Eligible(cand, policy, now) && (scan.eligible == nil || v.GreaterThan(scan.eligible.ver)) {
			scan.eligible = &versionedCandidate{cand, v}
		}
	}
	return scan, nil
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
	scan, err := scanCandidatesForConstraint(candidates, constraintStr, policy, now)
	if err != nil {
		return Candidate{}, err
	}

	if scan.eligible != nil {
		return scan.eligible.cand, nil
	}
	if scan.newest == nil {
		if constraintStr == "" {
			return Candidate{}, fmt.Errorf("no observed candidate; run `wraptool harness refresh` first")
		}
		return Candidate{}, fmt.Errorf("no observed candidate satisfies constraint %q; run `wraptool harness refresh` first", constraintStr)
	}
	if remaining, ok := QuarantineRemaining(scan.newest.cand, policy, now); ok {
		return Candidate{}, fmt.Errorf("no eligible candidate satisfies constraint %q: newest matching candidate %s is quarantined, %s remaining",
			constraintStr, scan.newest.cand.Version, remaining)
	}
	return Candidate{}, fmt.Errorf("no eligible candidate satisfies constraint %q: newest matching candidate %s is not eligible (yanked, or a disallowed prerelease)",
		constraintStr, scan.newest.cand.Version)
}

// quarantinedCandidate reports the newest candidate satisfying constraint
// that is currently being held back from selection by policy. version==""
// means there is nothing to report: either no observed candidate satisfies
// constraint at all, or the newest one already IS the best eligible one
// (nothing is being held back). A constraint parse failure is swallowed to
// version=="" here — this helper only builds an informative message on an
// already-successful no-op path, where an invalid constraint would already
// have surfaced as a hard error earlier in the same call.
func quarantinedCandidate(candidates []Candidate, constraintStr string, policy EffectivePolicy, now time.Time) (version string, remaining time.Duration, ok bool) {
	scan, err := scanCandidatesForConstraint(candidates, constraintStr, policy, now)
	if err != nil || scan.newest == nil {
		return "", 0, false
	}
	if scan.eligible != nil && !scan.newest.ver.GreaterThan(scan.eligible.ver) {
		return "", 0, false
	}
	remaining, ok = QuarantineRemaining(scan.newest.cand, policy, now)
	return scan.newest.cand.Version, remaining, ok
}

// loadManifestOrEmpty loads the manifest, treating a missing file as an
// empty one (no `install` has ever run) rather than an error — appropriate
// for read paths (StatusHarness, ManifestHarnessNames) that must work
// before any pool mutation. Mutating paths that need a real, already-created
// manifest (UpdateHarnessLocked) use LoadManifest directly and surface its
// error.
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
