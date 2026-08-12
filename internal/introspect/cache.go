package introspect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pti/wraptool/internal/paths"
)

// schemaFormatVersion is bumped whenever Schema's on-disk shape changes in a
// way a pre-bump cache entry could be silently MISREAD under, rather than
// simply gaining an omittable field. Format 2 added Subcommand.Coverage and
// Subcommand.ParseOutcome (design/doctor/wraptool-doctor.md §4.2 items 4-5):
// a format-1 cache has no data for either, and json.Unmarshal would leave
// them at their Go zero values (0.0, ParseOutcomeUnknown) rather than
// erroring — indistinguishable from a genuinely-probed-and-found-nothing
// subcommand. Format 3 replaced Coverage (a precomputed float that read 1.0
// for both "verified, no flags" and "nothing to measure against" alike —
// exactly the false-confidence step function item 4 was meant to remove, not
// relocate into a new field) with CoverageTokens/CoverageFound, the raw
// counts a caller can reason about directly; a format-2 cache has no data
// for either int field either, same silent-zero-value risk. SaveCache stamps
// every write with the CURRENT binary's schemaFormatVersion (the sole writer
// of truth — a caller-supplied value is always overwritten); LoadCache and
// LoadCacheStale both reject any Schema whose FormatVersion doesn't match,
// forcing a full re-introspection rather than serving a schema shaped like
// an older format. Bump again for any future change to Schema or
// Subcommand's JSON shape with the same property.
const schemaFormatVersion = 3

// CacheValidity holds the current value of every input that determines a
// tool's introspected schema. A cached schema is reused only when all of them
// still match what produced it, so the cache self-invalidates on real drift
// instead of only after a TTL. See
// design/introspect/introspection-cache-invalidation.md.
//
// Exported (along with LoadCache) so a caller outside this package — doctor's
// drift group, which must read the cache without ever writing it (design
// §5.2) — can call LoadCache/LoadCacheStale directly instead of duplicating
// cache.go's format.
type CacheValidity struct {
	ConfigHash  string        // hash of the introspection-relevant config
	NewestInput time.Time     // latest mtime across the tool binary, the wraptool binary, and the overrides file
	TTL         time.Duration // loose backstop for cases the signals above can't see
}

// LoadCache loads a cached schema for a tool, returning nil if it is missing,
// unreadable, stale against v (config changed, an input binary/overrides file
// is newer than the cache, or the TTL lapsed), or written in an older
// FormatVersion (schemaFormatVersion).
func LoadCache(toolName string, v CacheValidity) *Schema {
	schema, ok := readCacheFile(toolName)
	if !ok {
		return nil
	}

	// Config edited since the cache was written (allow rules / overrides path).
	if schema.ConfigHash != v.ConfigHash {
		return nil
	}
	// The wrapped tool, wraptool itself (parser code), or the overrides file was
	// rebuilt/edited after the cache was written.
	if schema.IntrospectedAt.Before(v.NewestInput) {
		return nil
	}
	// Loose backstop (e.g. an in-place tool upgrade that kept the same mtime).
	if v.TTL > 0 && time.Since(schema.IntrospectedAt) > v.TTL {
		return nil
	}

	return schema
}

// LoadCacheStale reads a tool's cached schema from disk with NO validity
// check at all — not staleness against any CacheValidity, not the tool
// binary's existence. LoadCache (validity-gated) intentionally returns nil
// for a stale entry, which discards the one field
// drift.tool-version-changed (design/doctor/wraptool-doctor.md §7.6) needs
// from a STALE cache: VersionOutput, the version string BestParse saw last
// time. A tool upgrade bumps the binary's own mtime, which is exactly what
// makes LoadCache's validity check reject the entry — so a version-drift
// check built on LoadCache could never observe the very drift it exists to
// report.
//
// ok is false only for a missing, unreadable, corrupt, or wrong-FormatVersion
// cache file — never for staleness. A schema returned here may describe a
// since-upgraded binary: callers must not treat Subcommands/Available as
// reflecting the CURRENTLY installed tool for anything but VersionOutput and
// IntrospectedAt (when it was captured) — exactly what drift.tool-version-changed
// needs and nothing more.
//
// Resolves through THIS process's real paths.CacheDir(). doctor's drift
// checks must not call this: paths.CacheDir() is the real host's XDG cache
// dir regardless of what Input.Paths.CacheDir says, which made every doctor
// unit test that seeded a cache entry read/write the DEVELOPER's actual
// introspection cache (design/doctor/wraptool-doctor.md §7.6, RULING 1 —
// the F0 API gap: this whole package predates doctor's root-parameterized
// convention, internal/pool's ActiveGeneration/VerifyGenerationTree). Use
// LoadCacheStaleAt(in.Paths.CacheDir, toolName) instead; `serve`'s
// wraptool_discover/introspection-cache-freshness callers keep using this
// entry point since they always mean the real host.
func LoadCacheStale(toolName string) (schema *Schema, ok bool) {
	return LoadCacheStaleAt(paths.CacheDir(), toolName)
}

// LoadCacheStaleAt is LoadCacheStale, rooted at an explicit cache directory
// instead of the real host's paths.CacheDir() — the read-only counterpart of
// how internal/pool exposes ActiveGeneration/VerifyGenerationTree against an
// explicit root rather than its own zero-argument, host-rooted layout
// accessors. This is the entry point doctor's drift group must use: build the
// path from Input.Paths.CacheDir (cmd/doctor.go resolves that field from the
// real paths.CacheDir() once, up front — same value, but hermetically
// injectable), never from LoadCacheStale.
func LoadCacheStaleAt(cacheDir, toolName string) (schema *Schema, ok bool) {
	return readCacheFileAt(cacheFileAt(cacheDir, toolName))
}

// cacheFileAt mirrors paths.IntrospectionCacheFile's layout
// (cacheDir/introspection/<tool>.json) but rooted at an explicit cacheDir
// rather than resolving paths.CacheDir() itself.
func cacheFileAt(cacheDir, toolName string) string {
	return filepath.Join(cacheDir, "introspection", toolName+".json")
}

// readCacheFile is LoadCache's read path (via the real, host-rooted
// paths.IntrospectionCacheFile): load, decode, and check FormatVersion.
func readCacheFile(toolName string) (*Schema, bool) {
	return readCacheFileAt(paths.IntrospectionCacheFile(toolName))
}

// readCacheFileAt is the actual load-decode-validate primitive every reader
// in this file shares — LoadCache and CacheStamp via readCacheFile's
// host-rooted path, LoadCacheStaleAt via an explicit one.
func readCacheFileAt(path string) (*Schema, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}

	var schema Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, false
	}
	if schema.FormatVersion != schemaFormatVersion {
		return nil, false
	}

	return &schema, true
}

// CacheStamp reports when a tool's cached schema was last introspected and the
// config hash it was built from, with no validity check beyond FormatVersion
// — purely for reporting freshness (e.g. the wraptool_info tool). Returns
// ok=false when there is no readable cache (introspected fresh this boot,
// introspection failed) OR the cache is in an older FormatVersion — routed
// through the same readCacheFile LoadCache/LoadCacheStale use, rather than
// its own bare read-and-decode, so it can never report an introspected_at
// for an entry every OTHER reader in this package already treats as absent.
// A duplicated decode here previously skipped that check entirely.
func CacheStamp(toolName string) (introspectedAt time.Time, configHash string, ok bool) {
	schema, ok := readCacheFile(toolName)
	if !ok {
		return time.Time{}, "", false
	}
	return schema.IntrospectedAt, schema.ConfigHash, true
}

// SaveCache writes a schema to the cache, stamping it with the current
// binary's schemaFormatVersion regardless of what schema.FormatVersion held
// on entry — SaveCache is the one place that decides what format is being
// written.
func SaveCache(toolName string, schema *Schema) error {
	path := paths.IntrospectionCacheFile(toolName)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating cache directory: %w", err)
	}

	schema.FormatVersion = schemaFormatVersion
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling schema: %w", err)
	}

	return os.WriteFile(path, data, 0600)
}
