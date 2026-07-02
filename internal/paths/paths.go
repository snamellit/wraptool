package paths

import (
	"path/filepath"

	"github.com/adrg/xdg"
)

const appName = "wraptool"

// ConfigDir returns the platform-appropriate config directory.
//   - Linux:   $XDG_CONFIG_HOME/wraptool  (~/.config/wraptool)
//   - macOS:   ~/Library/Application Support/wraptool
//   - Windows: %LOCALAPPDATA%/wraptool
func ConfigDir() string {
	return filepath.Join(xdg.ConfigHome, appName)
}

// DataDir returns the platform-appropriate data directory.
//   - Linux:   $XDG_DATA_HOME/wraptool  (~/.local/share/wraptool)
//   - macOS:   ~/Library/Application Support/wraptool
//   - Windows: %LOCALAPPDATA%/wraptool
func DataDir() string {
	return filepath.Join(xdg.DataHome, appName)
}

// CacheDir returns the platform-appropriate cache directory.
//   - Linux:   $XDG_CACHE_HOME/wraptool  (~/.cache/wraptool)
//   - macOS:   ~/Library/Caches/wraptool
//   - Windows: %LOCALAPPDATA%/wraptool/cache
func CacheDir() string {
	return filepath.Join(xdg.CacheHome, appName)
}

// DefaultConfigFile returns the default config file path.
func DefaultConfigFile() string {
	return filepath.Join(ConfigDir(), "config.yaml")
}

// DefaultRequestLog returns the default request log path.
func DefaultRequestLog() string {
	return filepath.Join(DataDir(), "requests.jsonl")
}

// RuntimeDir returns a per-user runtime directory for transient state such as
// pidfiles and locks (Linux: $XDG_RUNTIME_DIR/wraptool). Falls back to the
// cache dir when no runtime dir is defined (e.g. macOS).
func RuntimeDir() string {
	if xdg.RuntimeDir != "" {
		return filepath.Join(xdg.RuntimeDir, appName)
	}
	return filepath.Join(CacheDir(), "run")
}

// IntrospectionCacheFile returns the cache file path for a tool's introspection data.
func IntrospectionCacheFile(toolName string) string {
	return filepath.Join(CacheDir(), "introspection", toolName+".json")
}
