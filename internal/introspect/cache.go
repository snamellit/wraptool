package introspect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pti/wraptool/internal/paths"
)

// LoadCache loads a cached schema for a tool, returning nil if
// the cache doesn't exist or is expired.
func LoadCache(toolName string, ttl time.Duration) *Schema {
	path := paths.IntrospectionCacheFile(toolName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var schema Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil
	}

	if time.Since(schema.IntrospectedAt) > ttl {
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
