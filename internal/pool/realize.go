package pool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pti/wraptool/internal/pool/backend"
)

// RealizeGeneration builds (or, when one is already published, verifies and
// records) one platform generation for harnessName/key from h's locked
// artifacts (design/harness/unified-harness-pool.md §11.4 step 6, §16 Phase
// 2). It must be called with the pool lock already held (§6.5) — the same
// LockPool span the caller's *Locked orchestrator function assumes, never a
// second acquisition.
//
// Idempotence (§10's "install is idempotent...", extended to the realize
// axis): if h.Generations[key] already names a digest AND that generation's
// directory still exists on disk with a receipt that agrees, this is a
// no-op — src.Realize is never called. A recorded digest whose directory
// has vanished (lost cache, manual deletion, a fresh workstation seeded from
// a copied lock file) falls through to a fresh realize instead of silently
// reporting success — the "repair" case (§15/§17: "interrupted build leaves
// prior state usable" cuts both ways, a build must also be retried when
// prior state is simply gone).
//
// A fresh realize runs src.Realize inside a staging/txn-<id> scratch
// directory (paths.Staging — same volume as runtime/, §6.4), REFUSES to
// publish a generation whose receipt has zero files (almost certainly a
// Realize that unpacked into the wrong directory — an empty tree and an
// empty receipt would otherwise verify "clean" purely because there is
// nothing on either side to disagree with), host-verifies the tree against
// its own receipt (VerifyGenerationTree), and is renamed into
// runtime/generations/<generation-id>/ only after every check passes. Any
// failure before that rename leaves the txn directory cleaned up (the
// deferred newScratchDir cleanup) and the pool's lock/runtime completely
// untouched — whatever generation was active before this call stays usable
// (§17).
//
// On a fresh publish (or a byte-identical one landing on an already-existing
// target — see the "target already exists" check below): h.Generations[key]
// is updated in memory, harness-lock.json is persisted with the same
// update, and (fresh publish only) a "generation_realized" audit event is
// appended.
//
// Every success path — the idempotent fast path, the crash-recovery
// "target already exists" path, and a fresh publish — ends by calling
// activateIfNeeded (activate.go) for (harnessName, key) with the resolved
// generation id (§11.4 step 6: "…renames the tree into runtime/generations/,
// and activates"; wave 2 activation layer). This is host-local pure Go with
// no network and no backend, so a realize that itself succeeded but then
// fails to activate returns that activation error as-is: there is no
// partial-state hazard to paper over, since WritePointer's rename atomicity
// means the pointer is either the old generation or the new one, never a
// partial write.
func RealizeGeneration(ctx context.Context, src Source, b backend.Backend, paths PoolPaths, harnessName, binName string, h *LockHarness, key PlatformKey) (*Receipt, error) {
	// binName ends up as a wrapper filename inside the generation tree
	// (receipt.go/§11.4's generateHarnessWrapper), the same kind of
	// caller-supplied value GenerationID's harness/version arguments already
	// guard (validatePathSegment, receipt.go) — validated here, before any
	// use, so a malformed value (empty, a path separator, "." or "..") fails
	// loudly instead of silently escaping the intended directory.
	if err := validatePathSegment("binName", binName); err != nil {
		return nil, fmt.Errorf("realizing %s for %s: %w", harnessName, key, err)
	}

	// Idempotent fast path: the lock already claims a digest for this
	// platform. If the generation it names is still on disk and its own
	// receipt agrees, there is nothing to build — src.Realize is never
	// called.
	if existingDigest, ok := h.Generations[key]; ok && existingDigest != "" {
		genID, err := GenerationID(harnessName, h.Version, key, existingDigest)
		if err != nil {
			return nil, fmt.Errorf("computing generation id for %s/%s from the locked digest: %w", harnessName, key, err)
		}
		genDir := runtimeGenerationDir(paths.Runtime, genID)
		if _, statErr := os.Stat(genDir); statErr == nil {
			onDisk, err := readGenerationReceipt(genDir)
			if err != nil {
				return nil, fmt.Errorf("reading already-published generation %s: %w", genID, err)
			}
			if onDisk.ManifestDigest != existingDigest {
				return nil, fmt.Errorf("generation %s already exists but its receipt records manifest_digest %q, not the locked %q for %s/%s: immutability violation, refusing to overwrite",
					genID, onDisk.ManifestDigest, existingDigest, harnessName, key)
			}
			if err := persistGeneration(paths, harnessName, key, existingDigest); err != nil {
				return nil, err
			}
			// §11.4 step 6's "renames the tree ... and activates" applies to
			// the idempotent path too, per this work package's brief: also
			// activate here when the pointer is missing or names a different
			// generation (a repair, distinct from the directory-vanished
			// repair above — here the generation itself is fine, only the
			// pointer disagrees or is unset). activateIfNeeded no-ops when
			// the pointer already names genID.
			if err := activateIfNeeded(paths, harnessName, key, genID); err != nil {
				// Not re-wrapped: ActivateGeneration's own error already
				// names harnessName/key.
				return nil, err
			}
			return onDisk, nil
		}
		// Recorded but gone: fall through and rebuild (§15/§17 repair case).
	}

	txnDir, cleanup, err := newScratchDir(paths.Staging)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	receipt, err := src.Realize(ctx, b, harnessName, binName, h, key, paths.Cache, txnDir)
	if err != nil {
		return nil, fmt.Errorf("realizing %s for %s: %w", harnessName, key, err)
	}
	if len(receipt.Files) == 0 {
		return nil, fmt.Errorf("realizing %s for %s produced an empty generation (0 files in the receipt); refusing to publish it (§6.4/§11.4)", harnessName, key)
	}
	// Validate BEFORE publish: a malformed receipt (e.g. a zero/unsupported
	// SchemaVersion, or a Files list that doesn't sort/hash-check) must
	// never reach disk. Publishing one anyway would permanently wedge the
	// idempotent fast path above — every later command hits
	// UnmarshalReceipt's error on the very digest the lock now records,
	// with no way to recover short of hand-editing the lock or the
	// generation tree.
	if err := receipt.Validate(); err != nil {
		return nil, fmt.Errorf("realizing %s for %s: Source.Realize returned an invalid receipt: %w", harnessName, key, err)
	}
	// The receipt must describe exactly what was asked for. Two different
	// code paths derive a generation id from a LockHarness: this function's
	// publish path uses the RECEIPT's own Harness/Version/Platform fields,
	// while the idempotent fast path above (and persistGeneration) uses
	// harnessName/h.Version/key from the CALLER. A Source that normalized
	// or mis-set these fields would make the two paths compute different
	// generation ids for what should be the same realize, and — since the
	// fast path would then never recognize its own prior publish — would
	// republish forever instead of erroring once, here.
	if receipt.Harness != harnessName || receipt.Version != h.Version || receipt.Platform != key {
		return nil, fmt.Errorf("realizing %s for %s: Source.Realize returned a receipt for %s/%s/%s, which disagrees with the request; refusing to publish it",
			harnessName, key, receipt.Harness, receipt.Version, receipt.Platform)
	}

	if err := VerifyGenerationTree(txnDir, receipt); err != nil {
		return nil, fmt.Errorf("verifying realized generation for %s/%s: %w", harnessName, key, err)
	}

	genID, err := GenerationID(receipt.Harness, receipt.Version, key, receipt.ManifestDigest)
	if err != nil {
		return nil, fmt.Errorf("computing generation id for %s/%s: %w", harnessName, key, err)
	}

	receiptData, err := receipt.Marshal()
	if err != nil {
		return nil, err
	}
	// Written INTO the staged tree, after verification: ScanGenerationFiles
	// (which VerifyGenerationTree above used) excludes the top-level
	// receipt.json from the digest, so writing it here doesn't change what
	// was just verified.
	if err := WriteFileAtomic(filepath.Join(txnDir, "receipt.json"), receiptData, 0o644); err != nil {
		return nil, err
	}

	genDir := runtimeGenerationDir(paths.Runtime, genID)
	if _, statErr := os.Stat(genDir); statErr == nil {
		// The target already exists — a concurrent or repeated realize
		// landed first (or, for a first-time realize, an extremely
		// unlikely digest8 collision). Compare, don't blindly overwrite: a
		// generation directory is immutable once published (§5).
		onDisk, err := readGenerationReceipt(genDir)
		if err != nil {
			return nil, fmt.Errorf("reading existing generation %s: %w", genID, err)
		}
		if onDisk.ManifestDigest != receipt.ManifestDigest {
			return nil, fmt.Errorf("generation %s already exists but its receipt records manifest_digest %q, not this realize's %q: immutability violation, refusing to overwrite",
				genID, onDisk.ManifestDigest, receipt.ManifestDigest)
		}
		if h.Generations == nil {
			h.Generations = map[PlatformKey]string{}
		}
		h.Generations[key] = receipt.ManifestDigest
		if err := persistGeneration(paths, harnessName, key, receipt.ManifestDigest); err != nil {
			return nil, err
		}
		if err := activateIfNeeded(paths, harnessName, key, genID); err != nil {
			// Not re-wrapped: ActivateGeneration's own error already names
			// harnessName/key.
			return nil, err
		}
		return onDisk, nil
	}

	if err := os.MkdirAll(filepath.Dir(genDir), poolDirPerm); err != nil {
		return nil, fmt.Errorf("creating generations directory: %w", err)
	}
	if err := os.Rename(txnDir, genDir); err != nil {
		return nil, fmt.Errorf("publishing generation %s: %w", genID, err)
	}

	if h.Generations == nil {
		h.Generations = map[PlatformKey]string{}
	}
	h.Generations[key] = receipt.ManifestDigest

	if err := persistGeneration(paths, harnessName, key, receipt.ManifestDigest); err != nil {
		return nil, err
	}
	if err := AppendAudit(paths.Audit, "generation_realized", map[string]any{
		"harness": harnessName, "version": receipt.Version, "platform": string(key), "generation_id": genID,
	}); err != nil {
		return nil, fmt.Errorf("recording generation_realized audit event: %w", err)
	}

	// §11.4 step 6: "…renames the tree into runtime/generations/, and
	// activates." activateIfNeeded degrades to an unconditional
	// ActivateGeneration here (a freshly published generation id can never
	// already be the active one) — see its own doc comment for why this
	// fresh-publish path shares the helper instead of calling
	// ActivateGeneration directly.
	if err := activateIfNeeded(paths, harnessName, key, genID); err != nil {
		// Not re-wrapped: ActivateGeneration's own error already names
		// harnessName/key.
		return nil, err
	}

	return receipt, nil
}

// GenerationVersion resolves generationID's version by reading its own
// receipt.json — the authoritative source (not generation-id string
// parsing: a harness name or a semver prerelease identifier can itself
// contain hyphens, which would make splitting "<harness>-<version>-<platform-
// key>-<digest8>" back apart ambiguous). Used by cmd/harness.go's `list` to
// render the ACTIVE column's version from a HarnessStatus.ActiveGenerations
// entry.
func GenerationVersion(paths PoolPaths, generationID string) (string, error) {
	r, err := readGenerationReceipt(runtimeGenerationDir(paths.Runtime, generationID))
	if err != nil {
		return "", fmt.Errorf("resolving version for generation %s: %w", generationID, err)
	}
	return r.Version, nil
}

// readGenerationReceipt reads and validates (via UnmarshalReceipt)
// receipt.json from an existing generation directory.
func readGenerationReceipt(genDir string) (*Receipt, error) {
	data, err := os.ReadFile(filepath.Join(genDir, "receipt.json"))
	if err != nil {
		return nil, err
	}
	return UnmarshalReceipt(data)
}

// persistGeneration records digest as harnessName's realized generation for
// key in the on-disk harness-lock.json, leaving every other field (and
// every other harness entry) untouched. It re-reads the lock fresh rather
// than trusting an in-memory copy: by the time RealizeGeneration runs, the
// caller's own resolve+fetch+lock write (or an earlier command entirely)
// has already persisted the harness entry this call is only adding one
// Generations key to. A no-op when the digest is already recorded, so the
// idempotent fast path above doesn't rewrite the lock file on every call.
func persistGeneration(paths PoolPaths, harnessName string, key PlatformKey, digest string) error {
	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		return err
	}
	lh, ok := lockDoc.Harnesses[harnessName]
	if !ok {
		return fmt.Errorf("no lock entry for harness %q to record generation %s", harnessName, key)
	}
	if lh.Generations != nil && lh.Generations[key] == digest {
		return nil
	}
	if lh.Generations == nil {
		lh.Generations = map[PlatformKey]string{}
	}
	lh.Generations[key] = digest
	lockDoc.Harnesses[harnessName] = lh

	if err := lockDoc.Validate(); err != nil {
		return fmt.Errorf("validating lock after recording generation: %w", err)
	}
	data, err := lockDoc.Marshal()
	if err != nil {
		return err
	}
	return WriteFileAtomic(paths.Lock, data, 0o644)
}

// runtimeGenerationsDir and runtimeGenerationDir mirror layout.go's
// Root()-rooted RuntimeGenerationsDir/RuntimeGenerationDir, but parameterized
// on an explicit runtime root (PoolPaths.Runtime) instead of calling Root()
// directly — the same reasoning as PoolPaths.Cache/Staging: production
// wiring passes pool.RuntimeDir(), tests pass a t.TempDir() subdirectory, and
// nothing in this package ever touches the developer's real pool by
// accident.
func runtimeGenerationsDir(runtimeRoot string) string {
	return filepath.Join(runtimeRoot, "generations")
}

func runtimeGenerationDir(runtimeRoot, generationID string) string {
	return filepath.Join(runtimeGenerationsDir(runtimeRoot), generationID)
}
