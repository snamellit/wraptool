package pool

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// GCReport summarizes one GCPool/GCPoolLocked call (§10, §17): how many
// generations/cache objects were removed vs. retained, how many stale
// staging transaction directories were cleared, and the total bytes freed.
// A DryRun report describes what WOULD be removed; nothing is actually
// deleted in that case (GCPoolLocked still measures and counts everything
// exactly as a real run would, so the report is accurate either way).
type GCReport struct {
	DryRun bool

	RemovedGenerations   int
	RetainedGenerations  int
	RemovedCacheObjects  int
	RetainedCacheObjects int
	RemovedStagingDirs   int
	FreedBytes           int64

	// LegacyRemoved is true iff GCOptions.Legacy was set, the refusal check
	// (gcRefuseLegacyIfUnrealized) passed, AND at least one of the legacy
	// bin/+lib/ trees actually existed to remove (§16 Phase 4 item 5). It is
	// true on a dry run too ("would remove", like every other Removed*
	// field). False whenever GCOptions.Legacy was false, OR Legacy was true
	// but BOTH trees were already absent (nothing there to remove — the
	// report reflects reality, not merely that --legacy was requested; a
	// repeat `gc --legacy` on an already-gc'd pool still succeeds and still
	// (re)writes the legacy-removed marker, but reports nothing removed).
	// A true Legacy request that fails the refusal check never reaches a
	// GCReport at all (GCPoolLocked returns an error instead, before doing
	// any work).
	LegacyRemoved bool
}

// GCOptions configures one GCPool/GCPoolLocked call (§10, §16 Phase 4 item
// 5). The zero value (DryRun false, Legacy false) is a normal, mutating,
// legacy-untouched gc — every pre-existing caller before this work package
// (a bare `dryRun bool` parameter) is expressed as GCOptions{DryRun: dryRun}.
type GCOptions struct {
	// DryRun measures and reports what a real run would do without mutating
	// anything — see GCPool's own doc comment for why dry-run still takes
	// the pool lock.
	DryRun bool
	// Legacy additionally removes the pool's legacy bin/+lib/ trees IN
	// THEIR ENTIRETY (§16 Phase 4 item 5) — unlike generations/cache, which
	// are individually retained-or-removed by reference, the legacy trees
	// are deprecated wholesale: once safe to drop, every file in them goes.
	// RULING (this work package's brief): never automatic — a developer
	// whose containers still run the old devcontainer feature major, or a
	// guix host still relying on the mixed-state fallback
	// (cmd/guixcontainer.go, §12.1), depends on legacy bin/+lib/ staying
	// present, so this is opt-in only, one flag, no "--force" escape hatch.
	// Refused (error, before any removal work at all — see
	// gcRefuseLegacyIfUnrealized) when the manifest names a pool-lockable
	// harness with no active generation for HostPlatformKey.
	Legacy bool
	// LegacyBin, LegacyLib are the pool's legacy bin/+lib/ directories
	// (cmd/guixcontainer.go's ensureHarnessPool) — required when Legacy is
	// true, unused otherwise. This package never derives them itself: it
	// has no opinion on the legacy layout beyond "two directories the
	// caller names," keeping that layout decision in cmd, where
	// ensureHarnessPool already owns it.
	LegacyBin string
	LegacyLib string
	// IsPoolLockable classifies a manifest-declared harness name as
	// pool-lockable (internal/harness.Descriptor.PoolLockable()) or not.
	// Required when Legacy is true, unused otherwise — this package never
	// imports internal/harness (see this file's own package doc comment),
	// so the caller (cmd/harness.go) supplies the classification. A
	// harness the manifest names that this rejects is skipped by the
	// refusal check entirely: a legacy-only harness has no runtime/ path to
	// ever be "active" on, so requiring one would make --legacy
	// permanently unusable for any pool with even one such harness
	// declared.
	IsPoolLockable func(name string) bool
	// HostPlatformKey is the platform the refusal check's "has an active
	// generation" test runs against — normally pool.HostPlatformKey()'s
	// result, threaded through explicitly (like this package's Now/WorkDir
	// conventions elsewhere) rather than called internally, so a test can
	// pin an arbitrary key with no dependency on the actual host
	// architecture. Required when Legacy is true, unused otherwise.
	HostPlatformKey PlatformKey
	// Now is this call's single clock read (this package's own test-clock-
	// injection convention, e.g. UpdateOptions.Now, orchestrator.go),
	// stamped into the legacy-removed marker's timestamp line. Required
	// when Legacy is true, unused otherwise.
	Now time.Time
}

// GCPool removes unreferenced generations, unreferenced cache objects, and
// every stale staging transaction directory from the pool at paths (§10,
// §15), under its own, self-contained pool lock acquisition. See
// GCPoolLocked for the retention rules; this wrapper exists for a caller
// that isn't already composing multiple orchestrator calls under one
// LockPool span. opts.DryRun still acquires the lock — this work package's
// brief calls that out as a deliberate exception (no such wording appears in
// the design doc's own §10 gc entry, which doesn't mention --dry-run at
// all): dry-run wants a consistent view of what a real run would do, not a
// peek at a pool a concurrent mutation could be mid-changing.
func GCPool(ctx context.Context, paths PoolPaths, opts GCOptions) (GCReport, error) {
	release, err := LockPool(ctx, paths)
	if err != nil {
		return GCReport{}, err
	}
	defer release()
	return GCPoolLocked(paths, opts)
}

// GCPoolLocked is GCPool's body, assuming the caller already holds the pool
// lock (via LockPool) — it never acquires one itself.
//
// Retention invariants (§15, §17) — a generation under runtime/generations/
// is RETAINED iff ANY of:
//
//   - (a) some platform's active pointer (runtime/active/<key>/<harness>.gen,
//     any harness) currently references it — read directly from the
//     pointer's own content (a bare id-string comparison), never by
//     resolving a generation id and comparing on-disk digests, so this rule
//     needs no receipt at all;
//   - (b) its OWN receipt's ManifestDigest matches the CURRENT
//     harness-lock.json's recorded digest for that receipt's OWN
//     (Harness, Platform) — read per generation via its own receipt.json
//     (the scanRollbackCandidates pattern, orchestrator.go), never
//     reconstructed by recomputing a generation id from
//     (harnessName, lock's current top-level Version, digest): a platform
//     realized at an OLDER version than the lock's current top-level
//     Version would otherwise silently lose retention, because the
//     reconstructed id would embed the wrong version and never match the
//     real, on-disk directory name (this was review finding 2's arm64
//     scenario — imagine claude locked at v2 for amd64 but still only ever
//     realized at v1 for arm64: reconstructing amd64's v2 onto arm64's
//     recorded digest computes an id nothing on disk has);
//   - (c) among everything NOT already retained by (a)/(b), it is one of the
//     2 most-recently-created generations (directory ModTime — the same
//     ordering RollbackHarnessLocked uses to pick a rollback target) sharing
//     its (Harness, Platform) — §15 "Default GC retains at least two
//     previous generations per harness/platform". This is what makes an
//     immediately-previous version still rollback-able after an update: an
//     update's fresh realize overwrites the lock's Generations[key] entry
//     with the NEW digest only, so the old generation survives purely
//     through this rule until a second update (or gc, twice) ages it out of
//     the top 2.
//
// Everything else under runtime/generations/ is removed.
//
//   - A cache object under cache/sha256/ is RETAINED iff its digest (the
//     file's own name) is referenced by the current lock: any npm package's
//     pool-local SHA256 or any runtime artifact's SHA256, across every
//     harness. Everything else is removed.
//   - Every directory under staging/ is removed unconditionally: GC runs
//     under the pool's single exclusive lock (§6.5), so no other command
//     can be mid-transaction — any staging/txn-* directory still present is
//     necessarily debris from an interrupted prior run, never a directory
//     someone else currently owns (§15).
//
// Legacy bin/+lib/ is untouched by the rules above, regardless of opts.Legacy
// (§16 Phase 4 item 5) — GCOptions.Legacy is a wholly separate, wholesale
// removal step (gcLegacy), not an extra retention rule folded into the
// generation/cache scan.
//
// An existing-but-unreadable/corrupt POINTER file is a hard error naming the
// pointer's path (not a skip): GC holds the exclusive pool lock, so this can
// only be corruption, never a benign race with a concurrent activation —
// and silently continuing risks deleting the very generation that pointer
// references. The error suggests `wraptool harness rollback <name> --to
// <version>` or re-syncing the affected platform to repair it. A generation
// directory whose OWN receipt.json is unreadable/unparseable is instead
// simply excluded from rules (b)/(c) (it can never be the CURRENTLY active
// one — ActivateGeneration validates a receipt before ever pointing at it —
// so it is safe to let it fall through to removal rather than aborting the
// whole run over it).
//
// A non-dry-run call appends one "gc" audit event after every removal
// succeeds; a dry-run call appends nothing (it made no mutation to record).
//
// GC-under-live-mount (wave2-integration-fixes item 5): a container started
// before this call may hold a bind mount of runtime/ resolved at
// container-start time, including a generation this call is about to
// remove — GC has no way to know about, let alone wait for, an
// already-running container's mount table. If a harness misbehaves
// immediately after a gc, restart the container (`wraptool up`) so it
// re-resolves the mount against gc's retained set.
//
// opts.Legacy (§16 Phase 4 item 5): validated and checked FIRST, before any
// of the generation/cache/staging work above runs at all —
// gcRefuseLegacyIfUnrealized either returns nil (safe to proceed, and the
// rest of this call including legacy removal itself all still happen) or an
// error (nothing in this call touches the pool at all, not even the
// ordinary non-legacy cleanup — a refused `--legacy` invocation is a pure
// no-op, not a partial one). Once past that guard, legacy removal (gcLegacy)
// runs LAST, after the ordinary generation/cache/staging steps.
//
// Audit resilience: the "gc" audit event is appended after gcLegacy runs
// REGARDLESS of whether gcLegacy itself errored — report already reflects
// every deletion gcGenerations/gcCache/gcStaging (and any partial gcLegacy
// progress before its own error) actually performed, and those deletions
// must never go unrecorded just because the legacy step failed partway
// through. A gcLegacy error is still returned to the caller (wrapped with
// an audit-failure note if the audit append ALSO failed), after the audit
// append has been attempted.
func GCPoolLocked(paths PoolPaths, opts GCOptions) (GCReport, error) {
	if opts.Legacy {
		if opts.IsPoolLockable == nil {
			return GCReport{}, fmt.Errorf("internal error: GCOptions.Legacy requires IsPoolLockable")
		}
		if opts.HostPlatformKey == "" {
			return GCReport{}, fmt.Errorf("internal error: GCOptions.Legacy requires HostPlatformKey")
		}
		if opts.LegacyBin == "" || opts.LegacyLib == "" {
			return GCReport{}, fmt.Errorf("internal error: GCOptions.Legacy requires LegacyBin and LegacyLib")
		}
		if opts.Now.IsZero() {
			return GCReport{}, fmt.Errorf("internal error: GCOptions.Legacy requires Now")
		}
		if paths.LegacyRemovedMarker == "" {
			return GCReport{}, fmt.Errorf("internal error: GCOptions.Legacy requires paths.LegacyRemovedMarker")
		}
		if err := gcRefuseLegacyIfUnrealized(paths, opts.HostPlatformKey, opts.IsPoolLockable); err != nil {
			return GCReport{}, err
		}
	}

	report := GCReport{DryRun: opts.DryRun}

	retainedGenerations, err := retainedGenerationIDs(paths)
	if err != nil {
		return GCReport{}, err
	}
	if err := gcGenerations(paths, retainedGenerations, opts.DryRun, &report); err != nil {
		return GCReport{}, err
	}

	retainedCache, err := retainedCacheDigests(paths)
	if err != nil {
		return GCReport{}, err
	}
	if err := gcCache(paths, retainedCache, opts.DryRun, &report); err != nil {
		return GCReport{}, err
	}

	if err := gcStaging(paths, opts.DryRun, &report); err != nil {
		return GCReport{}, err
	}

	var legacyErr error
	if opts.Legacy {
		legacyErr = gcLegacy(paths, opts, &report)
	}

	if !opts.DryRun {
		if auditErr := AppendAudit(paths.Audit, "gc", map[string]any{
			"removed_generations":   report.RemovedGenerations,
			"removed_cache_objects": report.RemovedCacheObjects,
			"freed_bytes":           report.FreedBytes,
			"legacy_removed":        report.LegacyRemoved,
		}); auditErr != nil {
			if legacyErr != nil {
				return report, fmt.Errorf("%w (additionally failed to record the gc audit event: %v)", legacyErr, auditErr)
			}
			return GCReport{}, fmt.Errorf("recording gc audit event: %w", auditErr)
		}
	}

	if legacyErr != nil {
		return report, legacyErr
	}

	return report, nil
}

// gcRefuseLegacyIfUnrealized implements GCOptions.Legacy's RULING (this work
// package's brief): --legacy is refused — an error, never a warning — when
// the manifest at paths.Manifest names ANY pool-lockable harness (per
// isPoolLockable; see GCOptions.IsPoolLockable's own doc comment) with no
// GENUINELY REALIZED, on-disk active generation for key on THIS pool
// (harnessActiveOnDisk: pointer content PLUS the generation it names still
// existing on disk, receipt and all — the same standard resolveGuixPoolPlan,
// cmd/guixcontainer.go, and GenerationVersion apply elsewhere; a bare
// pointer-content read, pool.ActiveGeneration alone, is NOT enough here — a
// dangling pointer left behind by an earlier/concurrent gc must not read as
// "safe"). This is the strongest cheap proxy available for "every machine
// sharing this pool has actually cut over": it cannot see another machine's
// pool at all, but a name failing THIS pool's own check is conclusive proof
// legacy removal is unsafe here.
//
// loadManifestOrEmpty (orchestrator.go) already gives a missing manifest.yaml
// the same "not bootstrapped yet" tolerance loadLockOrEmpty gives a missing
// lock, reused here as-is: a missing manifest.yaml (never bootstrapped — no
// harness intent recorded at all) has nothing to check and passes trivially;
// refusing --legacy on a from-scratch pool with zero declared harnesses
// would make the flag permanently unusable there.
//
// The error names EVERY unrealized harness (not just the first found,
// sorted for deterministic output) and the fix for each (`wraptool harness
// install <name>`), so one --legacy attempt surfaces the whole remaining
// checklist rather than a discover-one-at-a-time loop — plus the escape
// hatch for a harness the developer no longer actually uses: dropping its
// manifest.yaml entry removes it from this check entirely.
func gcRefuseLegacyIfUnrealized(paths PoolPaths, key PlatformKey, isPoolLockable func(name string) bool) error {
	m, err := loadManifestOrEmpty(paths.Manifest)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(m.Harnesses))
	for name := range m.Harnesses {
		if !isPoolLockable(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var unrealized []string
	for _, name := range names {
		if !harnessActiveOnDisk(paths, name, key) {
			unrealized = append(unrealized, name)
		}
	}
	if len(unrealized) == 0 {
		return nil
	}

	fixes := make([]string, len(unrealized))
	for i, name := range unrealized {
		fixes[i] = fmt.Sprintf("wraptool harness install %s", name)
	}
	return fmt.Errorf("refusing --legacy: %s has no active, on-disk generation for %s on this pool; "+
		"every harness must show ACTIVE in `wraptool harness list` on every machine sharing this pool "+
		"before legacy removal is safe — run: %s (no longer using one of these? remove its entry from "+
		"manifest.yaml instead)",
		strings.Join(unrealized, ", "), key, strings.Join(fixes, "; "))
}

// harnessActiveOnDisk reports whether harnessName has a genuinely realized
// active generation for key — not merely a pointer that RESOLVES to an id
// (pool.ActiveGeneration alone), but one whose runtime/generations/ tree and
// receipt.json still exist. A dangling pointer (its generation removed by an
// earlier or concurrent gc, §15 — the pointer itself is untouched by GC's
// own retention rules, so it can easily outlive the generation it names)
// must not count as "active" here: gcRefuseLegacyIfUnrealized's whole
// purpose is proving a harness will keep working once legacy bin/+lib/ are
// gone, and a dangling pointer proves the opposite.
func harnessActiveOnDisk(paths PoolPaths, harnessName string, key PlatformKey) bool {
	genID, ok := ActiveGeneration(paths, harnessName, key)
	if !ok || genID == "" {
		return false
	}
	_, err := GenerationVersion(paths, genID)
	return err == nil
}

// gcLegacy implements GCOptions.Legacy's removal step, called only after
// gcRefuseLegacyIfUnrealized has already passed: wholesale-removes the
// pool's ENTIRE legacy bin/+lib/ trees (never selectively pruned, unlike
// generations/cache — see GCOptions.Legacy's own doc comment), then writes
// the legacy-removed marker (paths.LegacyRemovedMarker,
// pool.LegacyRemovedMarkerFile's own doc comment) so `ensureHarnessPool`
// (cmd/guixcontainer.go) stops recreating them on the next `wraptool up`.
//
// report.LegacyRemoved reflects reality, not merely that Legacy was
// requested: true iff at least one of the two directories actually existed
// (dry run: "would remove" — same convention as every other Removed* field).
// Both already being absent (idempotent repeat, or manual removal ahead of
// gc) is not an error and is not reported as a removal, but the marker is
// still (re)written on a non-dry run either way — the pool's "legacy
// removed" state should hold even when this call found nothing left to
// delete itself.
//
// The marker is skipped entirely on a dry run (no mutation at all). A
// partial failure (e.g. bin/ removed, then lib/'s os.RemoveAll errors)
// returns before writing the marker — report.FreedBytes and .LegacyRemoved
// already reflect whatever WAS removed before the error, and the caller
// (GCPoolLocked) still records that partial progress in the "gc" audit
// event rather than losing it.
func gcLegacy(paths PoolPaths, opts GCOptions, report *GCReport) error {
	removedAny := false
	for _, dir := range []string{opts.LegacyBin, opts.LegacyLib} {
		if _, err := os.Stat(dir); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("checking legacy dir %s: %w", dir, err)
		}
		size, err := dirSize(dir)
		if err != nil {
			return fmt.Errorf("measuring legacy dir %s: %w", dir, err)
		}
		report.FreedBytes += size
		removedAny = true
		if !opts.DryRun {
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("removing legacy dir %s: %w", dir, err)
			}
		}
	}
	report.LegacyRemoved = removedAny

	if opts.DryRun {
		return nil
	}
	marker := opts.Now.UTC().Format(time.RFC3339) + "\n" +
		"removed by `wraptool harness gc --legacy`\n"
	if err := WriteFileAtomic(paths.LegacyRemovedMarker, []byte(marker), 0o644); err != nil {
		return fmt.Errorf("writing legacy-removed marker: %w", err)
	}
	return nil
}

// gcGenerationRecord is one generation directory's identity as read from its
// OWN receipt.json — never reconstructed from a harness's current lock
// entry (see GCPoolLocked's doc comment, rule (b)).
type gcGenerationRecord struct {
	id       string
	harness  string
	platform PlatformKey
	digest   string
	modTime  time.Time
}

// scanGenerationRecords reads every directory under runtime/generations/ and
// returns a record for each one whose receipt.json parses successfully,
// silently excluding (not erroring on) any that doesn't — the same
// tolerance scanRollbackCandidates (orchestrator.go) applies to its own
// generation scan, and safe for the same reason: a generation whose receipt
// can't be parsed could never have been legitimately activated in the first
// place (ActivateGeneration validates a receipt before ever pointing at
// one), so it can only ever be retained via rule (a) (a bare pointer-content
// comparison, which needs no receipt at all) — never via (b) or (c), both of
// which this function's records feed.
func scanGenerationRecords(paths PoolPaths) ([]gcGenerationRecord, error) {
	entries, err := os.ReadDir(runtimeGenerationsDir(paths.Runtime))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing generations: %w", err)
	}
	var out []gcGenerationRecord
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		genDir := runtimeGenerationDir(paths.Runtime, entry.Name())
		info, err := os.Stat(genDir)
		if err != nil {
			continue
		}
		r, err := readGenerationReceipt(genDir)
		if err != nil {
			continue
		}
		out = append(out, gcGenerationRecord{
			id: entry.Name(), harness: r.Harness, platform: r.Platform, digest: r.ManifestDigest, modTime: info.ModTime(),
		})
	}
	return out, nil
}

// retainedGenerationIDs computes the set of generation ids GC must keep —
// see GCPoolLocked's doc comment for the three retention rules this
// implements. Pointer files are read STRICTLY: an existing pointer that
// fails to read is a hard error (ruling: GC holds the exclusive pool lock,
// so this is corruption, not a benign race), naming the pointer path and
// suggesting the host-side repair command. A pointer file that simply
// doesn't exist (that platform was never activated) is not iterated at all
// by the glob below, so it never reaches this check.
func retainedGenerationIDs(paths PoolPaths) (map[string]bool, error) {
	retained := map[string]bool{}

	matches, err := filepath.Glob(filepath.Join(runtimeActiveDir(paths.Runtime), "*", "*.gen"))
	if err != nil {
		return nil, fmt.Errorf("globbing active pointers: %w", err)
	}
	for _, ptr := range matches {
		id, err := ReadPointer(ptr)
		if err != nil {
			return nil, fmt.Errorf("gc: pointer %s is unreadable (%w); this pointer's generation cannot be safely identified for retention — "+
				"repair it first, e.g. `wraptool harness rollback <name> --to <version>` or `wraptool harness sync --platform <key>`, then re-run gc", ptr, err)
		}
		if id != "" {
			retained[id] = true
		}
	}

	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		return nil, err
	}

	records, err := scanGenerationRecords(paths)
	if err != nil {
		return nil, err
	}

	// Rule (b): match by the GENERATION's own (harness, platform, digest) —
	// never by reconstructing a generation id from the lock's current
	// top-level Version (review finding 2).
	for _, rec := range records {
		if retained[rec.id] {
			continue
		}
		if h, ok := lockDoc.Harnesses[rec.harness]; ok && h.Generations[rec.platform] == rec.digest {
			retained[rec.id] = true
		}
	}

	// Rule (c): among records not yet retained by (a)/(b), keep the 2
	// most-recently-created (by directory ModTime) per (harness, platform).
	type groupKey struct {
		harness  string
		platform PlatformKey
	}
	spares := map[groupKey][]gcGenerationRecord{}
	for _, rec := range records {
		if retained[rec.id] {
			continue
		}
		k := groupKey{rec.harness, rec.platform}
		spares[k] = append(spares[k], rec)
	}
	for _, group := range spares {
		// SliceStable + an explicit generation-id tiebreak (rather than
		// relying on scanGenerationRecords' incoming os.ReadDir order,
		// which happens to be name-sorted too, but that's an accident of
		// the stdlib, not a documented guarantee this package should lean
		// on): coarse-grained filesystem mtimes (e.g. 1s resolution on some
		// hosts) can make two generations compare equal, and GC's output
		// must not depend on directory-iteration order in that case.
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].modTime.Equal(group[j].modTime) {
				return group[i].id < group[j].id
			}
			return group[i].modTime.After(group[j].modTime)
		})
		for i := 0; i < len(group) && i < 2; i++ {
			retained[group[i].id] = true
		}
	}

	return retained, nil
}

// retainedCacheDigests computes the set of cache/sha256/<digest> file names
// (bare hex, matching ReceiptFile.SHA256/NPMLockPackage.SHA256's own
// unprefixed form) that the current lock still references, across every
// harness's npm package cache addresses, runtime artifact digests, and
// archive artifact digests (lock.go's LockHarness.Archive — wave2-
// integration-fixes item 2: this walk originally covered only
// NPMLock.Packages and Runtime.Artifacts, so GC deleted a still-locked
// archive-sourced harness's (e.g. agy) cache object out from under it).
func retainedCacheDigests(paths PoolPaths) (map[string]bool, error) {
	retained := map[string]bool{}

	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		return nil, err
	}
	for _, h := range lockDoc.Harnesses {
		if h.NPMLock != nil {
			for _, pkg := range h.NPMLock.Packages {
				if pkg.SHA256 != "" {
					retained[pkg.SHA256] = true
				}
			}
		}
		if h.Runtime != nil {
			for _, artifact := range h.Runtime.Artifacts {
				if artifact.SHA256 != "" {
					retained[artifact.SHA256] = true
				}
			}
		}
		if h.Archive != nil {
			for _, artifact := range h.Archive.Artifacts {
				if artifact.SHA256 != "" {
					retained[artifact.SHA256] = true
				}
			}
		}
	}
	return retained, nil
}

// gcGenerations removes every directory under runtime/generations/ that is
// not in retained, counting and sizing every one (removed or kept) into
// report. A missing runtime/generations/ directory (nothing ever realized)
// is a silent no-op, not an error.
func gcGenerations(paths PoolPaths, retained map[string]bool, dryRun bool, report *GCReport) error {
	entries, err := os.ReadDir(runtimeGenerationsDir(paths.Runtime))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("listing generations: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if retained[entry.Name()] {
			report.RetainedGenerations++
			continue
		}
		genDir := runtimeGenerationDir(paths.Runtime, entry.Name())
		size, err := dirSize(genDir)
		if err != nil {
			return fmt.Errorf("measuring generation %s: %w", entry.Name(), err)
		}
		report.RemovedGenerations++
		report.FreedBytes += size
		if !dryRun {
			if err := os.RemoveAll(genDir); err != nil {
				return fmt.Errorf("removing generation %s: %w", entry.Name(), err)
			}
		}
	}
	return nil
}

// gcCache removes every file under cache/sha256/ that is not in retained,
// counting and sizing every one (removed or kept) into report. A missing
// cache directory (nothing ever fetched) is a silent no-op, not an error.
func gcCache(paths PoolPaths, retained map[string]bool, dryRun bool, report *GCReport) error {
	entries, err := os.ReadDir(paths.Cache)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("listing cache: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if retained[entry.Name()] {
			report.RetainedCacheObjects++
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat cache object %s: %w", entry.Name(), err)
		}
		report.RemovedCacheObjects++
		report.FreedBytes += info.Size()
		if !dryRun {
			if err := os.Remove(filepath.Join(paths.Cache, entry.Name())); err != nil {
				return fmt.Errorf("removing cache object %s: %w", entry.Name(), err)
			}
		}
	}
	return nil
}

// gcStaging removes every directory under staging/ unconditionally (§15,
// this file's own doc comment on GCPoolLocked explains why that is safe
// under the pool lock). A missing staging directory is a silent no-op.
func gcStaging(paths PoolPaths, dryRun bool, report *GCReport) error {
	entries, err := os.ReadDir(paths.Staging)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("listing staging: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		report.RemovedStagingDirs++
		if !dryRun {
			if err := os.RemoveAll(filepath.Join(paths.Staging, entry.Name())); err != nil {
				return fmt.Errorf("removing staging dir %s: %w", entry.Name(), err)
			}
		}
	}
	return nil
}

// dirSize returns the total size, in bytes, of every regular file under
// root (recursively) — used to report FreedBytes for a directory GC is
// about to remove (or, on a dry run, would remove).
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measuring %s: %w", root, err)
	}
	return total, nil
}
