package introspect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pti/wraptool/internal/paths"
)

// cacheValidity holds the current value of every input that determines a tool's
// introspected schema. A cached schema is reused only when all of them still
// match what produced it, so the cache self-invalidates on real drift instead of
// only after a TTL. See design/introspection-cache-invalidation.md.
type cacheValidity struct {
	ConfigHash  string        // hash of the introspection-relevant config
	NewestInput time.Time     // latest mtime across the tool binary, the wraptool binary, and the overrides file
	TTL         time.Duration // loose backstop for cases the signals above can't see
}

// LoadCache loads a cached schema for a tool, returning nil if it is missing,
// unreadable, or stale against v (config changed, an input binary/overrides file
// is newer than the cache, or the TTL lapsed).
func LoadCache(toolName string, v cacheValidity) *Schema {
	path := paths.IntrospectionCacheFile(toolName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var schema Schema
	if err := json.Unmarshal(data, &schema); err != nil {
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

	return &schema
}

// SaveCache writes a schema to the cache.
func SaveCache(toolName string, schema *Schema) error {
	path := paths.IntrospectionCacheFile(toolName)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating cache directory: %w", err)
	}

	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling schema: %w", err)
	}

	return os.WriteFile(path, data, 0600)
}
