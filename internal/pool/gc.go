package pool

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
// LockPool span. dryRun still acquires the lock (§10: "a deliberate
// exception" — dry-run wants a consistent view of what a real run would do)
// but removes nothing.
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
// Retention invariants (§17):
//
//   - A generation under runtime/generations/ is RETAINED iff (a) some
//     platform's active pointer (runtime/active/<key>/<harness>.gen, any
//     harness) currently references it, OR (b) its receipt digest is
//     recorded in the CURRENT harness-lock.json's Generations map for some
//     harness/platform. Everything else under runtime/generations/ is
//     removed. An active generation is therefore never collected (rule a),
//     matching §15's "an active generation is never garbage-collected" —
//     even one no longer reachable from the lock (e.g. after a manual lock
//     edit) stays retained as long as some pointer still names it.
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

// retainedGenerationIDs computes the set of generation ids that must be
// kept: every id any platform's active pointer currently names, unioned
// with every id the current lock's Generations map resolves to (via
// GenerationID, using each harness's own locked version — receipts, not
// generation-id string parsing, remain the per-generation authority
// elsewhere in this package; here we only need the id strings themselves to
// match against runtime/generations/ directory names).
func retainedGenerationIDs(paths PoolPaths) (map[string]bool, error) {
	retained := map[string]bool{}

	matches, err := filepath.Glob(filepath.Join(runtimeActiveDir(paths.Runtime), "*", "*.gen"))
	if err != nil {
		return nil, fmt.Errorf("globbing active pointers: %w", err)
	}
	for _, ptr := range matches {
		id, err := ReadPointer(ptr)
		if err != nil {
			// An unreadable or mid-rename pointer retains nothing extra;
			// harmless to skip (§6.5's lockless-read tolerance applies here
			// too, even though GC itself holds the lock — a pointer can
			// still be genuinely absent between MkdirAll and the first
			// WritePointer of a platform that has never been activated).
			continue
		}
		if id != "" {
			retained[id] = true
		}
	}

	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		return nil, err
	}
	for harnessName, h := range lockDoc.Harnesses {
		for key, digest := range h.Generations {
			id, err := GenerationID(harnessName, h.Version, key, digest)
			if err != nil {
				// A malformed recorded digest can't be resolved to a
				// generation id at all; skip rather than fail GC entirely —
				// there is nothing on disk this could even be retaining.
				continue
			}
			retained[id] = true
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
