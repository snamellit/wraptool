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
// subcommand. SaveCache stamps every write with the CURRENT binary's
// schemaFormatVersion (the sole writer of truth — a caller-supplied value is
// always overwritten); LoadCache and LoadCacheStale both reject any Schema
// whose FormatVersion doesn't match, forcing a full re-introspection rather
// than serving a schema shaped like an older format. Bump again for any
// future change to Schema or Subcommand's JSON shape with the same property.
const schemaFormatVersion = 2

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
func LoadCacheStale(toolName string) (schema *Schema, ok bool) {
	return readCacheFile(toolName)
}

// readCacheFile is LoadCache and LoadCacheStale's shared read path: load,
// decode, and check FormatVersion. Neither caller-visible function does
// anything with the result but the two things they each document doing on
// top of this.
func readCacheFile(toolName string) (*Schema, bool) {
	data, err := os.ReadFile(paths.IntrospectionCacheFile(toolName))
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
// config hash it was built from, with no validity check — purely for reporting
// freshness (e.g. the wraptool_info tool). Returns ok=false when there is no
// readable cache (introspected fresh this boot, or introspection failed).
func CacheStamp(toolName string) (introspectedAt time.Time, configHash string, ok bool) {
	data, err := os.ReadFile(paths.IntrospectionCacheFile(toolName))
	if err != nil {
		return time.Time{}, "", false
	}
	var schema Schema
	if err := json.Unmarshal(data, &schema); err != nil {
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
