package pool

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
}

// GCPool removes unreferenced generations, unreferenced cache objects, and
// every stale staging transaction directory from the pool at paths (§10,
// §15), under its own, self-contained pool lock acquisition. See
// GCPoolLocked for the retention rules; this wrapper exists for a caller
// that isn't already composing multiple orchestrator calls under one
// LockPool span. dryRun still acquires the lock — this work package's brief
// calls that out as a deliberate exception (no such wording appears in the
// design doc's own §10 gc entry, which doesn't mention --dry-run at all):
// dry-run wants a consistent view of what a real run would do, not a peek
// at a pool a concurrent mutation could be mid-changing.
func GCPool(ctx context.Context, paths PoolPaths, dryRun bool) (GCReport, error) {
	release, err := LockPool(ctx, paths)
	if err != nil {
		return GCReport{}, err
	}
	defer release()
	return GCPoolLocked(paths, dryRun)
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
// Legacy bin/+lib/ is never touched (Phase 4, §16).
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
func GCPoolLocked(paths PoolPaths, dryRun bool) (GCReport, error) {
	report := GCReport{DryRun: dryRun}

	retainedGenerations, err := retainedGenerationIDs(paths)
	if err != nil {
		return GCReport{}, err
	}
	if err := gcGenerations(paths, retainedGenerations, dryRun, &report); err != nil {
		return GCReport{}, err
	}

	retainedCache, err := retainedCacheDigests(paths)
	if err != nil {
		return GCReport{}, err
	}
	if err := gcCache(paths, retainedCache, dryRun, &report); err != nil {
		return GCReport{}, err
	}

	if err := gcStaging(paths, dryRun, &report); err != nil {
		return GCReport{}, err
	}

	if !dryRun {
		if err := AppendAudit(paths.Audit, "gc", map[string]any{
			"removed_generations":   report.RemovedGenerations,
			"removed_cache_objects": report.RemovedCacheObjects,
			"freed_bytes":           report.FreedBytes,
		}); err != nil {
			return GCReport{}, fmt.Errorf("recording gc audit event: %w", err)
		}
	}

	return report, nil
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
		sort.Slice(group, func(i, j int) bool { return group[i].modTime.After(group[j].modTime) })
		for i := 0; i < len(group) && i < 2; i++ {
			retained[group[i].id] = true
		}
	}

	return retained, nil
}

// retainedCacheDigests computes the set of cache/sha256/<digest> file names
// (bare hex, matching ReceiptFile.SHA256/NPMLockPackage.SHA256's own
// unprefixed form) that the current lock still references, across every
// harness's npm package cache addresses and runtime artifact digests.
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
