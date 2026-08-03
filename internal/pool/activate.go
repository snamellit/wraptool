package pool

import (
	"fmt"
	"os"
	"path/filepath"
)

// runtimeActiveDir, runtimeActivePlatformDir, and pointerFile mirror
// layout.go's Root()-rooted RuntimeActiveDir/RuntimeActivePlatformDir/
// PointerFile, parameterized on an explicit runtime root instead of calling
// Root() directly — the same reasoning as realize.go's
// runtimeGenerationsDir/runtimeGenerationDir and dispatcher.go's
// runtimeBinDir/runtimeBinFile: production wiring passes pool.RuntimeDir()
// (via PoolPaths.Runtime), tests pass a t.TempDir() subdirectory.
func runtimeActiveDir(runtimeRoot string) string {
	return filepath.Join(runtimeRoot, "active")
}

func runtimeActivePlatformDir(runtimeRoot string, key PlatformKey) string {
	return filepath.Join(runtimeActiveDir(runtimeRoot), string(key))
}

func pointerFile(runtimeRoot string, key PlatformKey, harnessName string) string {
	return filepath.Join(runtimeActivePlatformDir(runtimeRoot, key), harnessName+".gen")
}

// ActivateGeneration atomically switches harnessName's active pointer for
// key to generationID (§5.2) — the single point where the pool's runtime
// state actually changes what a project container executes. Must be called
// with the pool lock already held (§6.5), the same convention every other
// mutating entry point in this package follows.
//
// It first validates that generationID names a real, self-consistent
// generation: runtime/generations/<generationID>/receipt.json must exist,
// unmarshal (UnmarshalReceipt already calls Receipt.Validate), and its own
// Harness/Platform fields must agree with what's being activated — a defense
// against activating a generation id that was never actually published, or
// one built for a different harness/platform than the caller intends.
//
// Once validated: it MkdirAlls the active/<key> directory, WritePointers the
// new generation id (fsync+rename, §5.2 — readers never observe a partial
// pointer), regenerates the dispatcher (§5.3: "regenerated, not edited, on
// every activation" — harmless and idempotent, since dispatcher content
// depends only on harnessName, never on which generation is active), and
// appends an "activated" audit event. Every step after validation is
// host-local pure Go with no network and no backend — a failure partway
// through leaves the pointer either at its old value or the new one (rename
// atomicity), never a partial write, so there is no unrecoverable
// intermediate state to clean up.
//
// Rollback (RollbackHarness/RollbackHarnessLocked, orchestrator.go) is
// exactly one call to this function: switching the pointer to a previously
// realized generation is the entire rollback operation (§10, §13 "Default
// rollback switches the pointer only and preserves state") — there is no
// separate tree-surgery step.
func ActivateGeneration(paths PoolPaths, harnessName string, key PlatformKey, generationID string) error {
	genDir := runtimeGenerationDir(paths.Runtime, generationID)
	data, err := os.ReadFile(filepath.Join(genDir, "receipt.json"))
	if err != nil {
		return fmt.Errorf("activating %s for %s: reading generation %s's receipt: %w", harnessName, key, generationID, err)
	}
	receipt, err := UnmarshalReceipt(data)
	if err != nil {
		return fmt.Errorf("activating %s for %s: generation %s's receipt: %w", harnessName, key, generationID, err)
	}
	if receipt.Harness != harnessName || receipt.Platform != key {
		return fmt.Errorf("activating %s for %s: generation %s's receipt names %s/%s, which disagrees with the request",
			harnessName, key, generationID, receipt.Harness, receipt.Platform)
	}

	activeDir := runtimeActivePlatformDir(paths.Runtime, key)
	if err := os.MkdirAll(activeDir, poolDirPerm); err != nil {
		return fmt.Errorf("activating %s for %s: creating %s: %w", harnessName, key, activeDir, err)
	}

	if err := WritePointer(pointerFile(paths.Runtime, key, harnessName), generationID); err != nil {
		return fmt.Errorf("activating %s for %s: writing pointer: %w", harnessName, key, err)
	}

	if err := WriteDispatcher(paths, harnessName); err != nil {
		return fmt.Errorf("activating %s for %s: %w", harnessName, key, err)
	}

	if err := AppendAudit(paths.Audit, "activated", map[string]any{
		"harness": harnessName, "platform": string(key), "generation_id": generationID,
	}); err != nil {
		return fmt.Errorf("activating %s for %s: recording activated audit event: %w", harnessName, key, err)
	}
	return nil
}

// ActiveGeneration returns the generation id currently active for
// harnessName/key at paths, and whether one is set. It performs exactly one
// ReadPointer and tolerates every failure mode (no pointer file yet, a
// missing active/<key> directory, an unreadable pointer) as ok=false rather
// than an error — this is `list`'s and RollbackHarnessLocked's read path,
// and §6.5 requires `list` to take no lock and tolerate a concurrent
// transaction (a pointer rename is atomic: a concurrent reader either sees
// the old id, the new id, or — mid-MkdirAll on a never-yet-activated
// platform — a clean not-exist; there is no partial-content case to
// tolerate beyond what os.ReadFile already reports as an error).
func ActiveGeneration(paths PoolPaths, harnessName string, key PlatformKey) (generationID string, ok bool) {
	id, err := ReadPointer(pointerFile(paths.Runtime, key, harnessName))
	if err != nil {
		return "", false
	}
	return id, true
}

// activateIfNeeded activates generationID for harnessName/key unless the
// active pointer already names it exactly — RealizeGeneration's policy for
// its idempotent/existing-target paths (§11.4 step 6: "and also on the
// idempotent existing-target path when the pointer is missing or names a
// different generation"). A fresh publish always needs activation (the new
// generation id can never already be the active one), so RealizeGeneration's
// fresh-publish path calls this too rather than duplicating the logic —
// activateIfNeeded degrades to an unconditional ActivateGeneration call
// whenever there is nothing to compare against yet.
func activateIfNeeded(paths PoolPaths, harnessName string, key PlatformKey, generationID string) error {
	if current, ok := ActiveGeneration(paths, harnessName, key); ok && current == generationID {
		return nil
	}
	return ActivateGeneration(paths, harnessName, key, generationID)
}
