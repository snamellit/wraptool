// Package pool implements the developer-scoped harness pool: layout, manifest
// and lock types, atomic file/pointer writes, the advisory pool lock, release
// eligibility, and audit logging. See design/harness/unified-harness-pool.md.
//
// This package owns pure path/type/policy logic only. It does not know how to
// download, resolve, or realize a harness (internal/pool/source and
// internal/pool/backend, a later work package) and has no cmd/ wiring.
package pool

import (
	"path/filepath"

	"github.com/pti/wraptool/internal/paths"
)

// Root returns the pool root directory: <paths.DataDir()>/harness-pool. Every
// other accessor in this file is rooted here — never a hardcoded path (§6.3).
func Root() string {
	return filepath.Join(paths.DataDir(), "harness-pool")
}

// ManifestFile returns the path to the developer-owned intent/policy file
// (§7).
func ManifestFile() string {
	return filepath.Join(Root(), "manifest.yaml")
}

// LockFile returns the path to the generated exact-resolution lock (§8).
func LockFile() string {
	return filepath.Join(Root(), "harness-lock.json")
}

// ObservationsFile returns the path to the first-seen candidate metadata
// store (§8.1).
func ObservationsFile() string {
	return filepath.Join(Root(), "observations.json")
}

// AuditFile returns the path to the append-only pool event log (§5, §9).
func AuditFile() string {
	return filepath.Join(Root(), "audit.jsonl")
}

// LegacyRemovedMarkerFile returns the path of the marker
// `wraptool harness gc --legacy` writes, after removing the pool's legacy
// bin/+lib/ trees, at the developer's real pool root (§16 Phase 4 item 5).
// Its presence is what stops `ensureHarnessPool` (cmd/guixcontainer.go)
// from silently recreating bin/+lib/ on the very next `wraptool up` —
// without it, a gc'd pool would resurrect itself the moment a developer
// re-entered a container. An explicit reinstall (`wraptool harness install
// <name>`, cmd/harness.go's legacy install path) removes this marker again,
// deliberately un-gc'ing the pool. A production-only convenience wrapper
// around LegacyRemovedMarkerFileAt(Root()) — every caller that must work
// against a test-rooted pool (internal/pool.PoolPaths.LegacyRemovedMarker,
// or a cmd-level fake pool root) uses that parameterized form instead.
func LegacyRemovedMarkerFile() string {
	return LegacyRemovedMarkerFileAt(Root())
}

// LegacyRemovedMarkerFileAt is LegacyRemovedMarkerFile parameterized on an
// explicit pool root, so a caller that already has one in hand (a fake
// t.TempDir() root in a test, or cmd/guixcontainer.go's own harnessPoolDir())
// never needs to reconstruct or duplicate the "legacy-removed" filename
// itself.
func LegacyRemovedMarkerFileAt(root string) string {
	return filepath.Join(root, "legacy-removed")
}

// CacheDir returns the pool's immutable downloaded-artifact cache root.
func CacheDir() string {
	return filepath.Join(Root(), "cache")
}

// CacheSHA256Dir returns the flat, content-addressed artifact cache
// (cache/sha256/<digest>).
func CacheSHA256Dir() string {
	return filepath.Join(CacheDir(), "sha256")
}

// CacheObjectPath returns the path of the cached artifact for the given
// sha256 hex digest.
func CacheObjectPath(digest string) string {
	return filepath.Join(CacheSHA256Dir(), digest)
}

// RuntimeDir returns the read-only tree exposed to project containers (§5):
// only this subtree is ever mounted into an untrusted project.
func RuntimeDir() string {
	return filepath.Join(Root(), "runtime")
}

// RuntimeBinDir returns the directory of generated dispatcher scripts
// (runtime/bin/<harness>).
func RuntimeBinDir() string {
	return filepath.Join(RuntimeDir(), "bin")
}

// RuntimeBinFile returns the dispatcher script path for one harness.
func RuntimeBinFile(harness string) string {
	return filepath.Join(RuntimeBinDir(), harness)
}

// RuntimeActiveDir returns the directory holding per-platform pointer files
// (runtime/active/<platform-key>/).
func RuntimeActiveDir() string {
	return filepath.Join(RuntimeDir(), "active")
}

// RuntimeActivePlatformDir returns the pointer-file directory for one
// platform key.
func RuntimeActivePlatformDir(key PlatformKey) string {
	return filepath.Join(RuntimeActiveDir(), string(key))
}

// PointerFile returns the path of the pointer file that activates harness
// under platform key (§5.2): runtime/active/<platform-key>/<harness>.gen.
func PointerFile(key PlatformKey, harness string) string {
	return filepath.Join(RuntimeActivePlatformDir(key), harness+".gen")
}

// RuntimeGenerationsDir returns the directory holding immutable generation
// trees (runtime/generations/<generation-id>/).
func RuntimeGenerationsDir() string {
	return filepath.Join(RuntimeDir(), "generations")
}

// RuntimeGenerationDir returns the tree for one generation id.
func RuntimeGenerationDir(generationID string) string {
	return filepath.Join(RuntimeGenerationsDir(), generationID)
}

// HomeDir returns the shared, read-write harness HOME (auth/config/skills),
// mounted read-write into every project container.
func HomeDir() string {
	return filepath.Join(Root(), "home")
}

// StagingDir returns the root of in-flight transactions. Never mounted into
// project containers (§5).
func StagingDir() string {
	return filepath.Join(Root(), "staging")
}

// StagingTxnDir returns the directory for one in-flight transaction.
func StagingTxnDir(txnID string) string {
	return filepath.Join(StagingDir(), "txn-"+txnID)
}

// AdvisoryLockFile returns the path of the pool's exclusive advisory
// mutation lock (§6.5) — distinct from LockFile, which is the generated
// harness-lock.json resolution artifact.
func AdvisoryLockFile() string {
	return filepath.Join(Root(), "lock")
}
