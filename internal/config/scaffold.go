package config

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// defaultConfigTemplate is the commented starter config written by
// EnsureConfigFile when no config exists yet. It exposes a single privileged
// starter tool (git pull) so first-run `serve` both comes up and demonstrates
// what wraptool is for. __GIT_BINARY__ is substituted with git's resolved
// absolute path, since the whitelist requires an absolute, existing binary.
//
//go:embed default_config.yaml
var defaultConfigTemplate string

// renderDefaultConfig fills in the starter tool's binary path. git is resolved
// on PATH; if it is absent we fall back to a conventional location and let
// config validation surface the bad path rather than guessing silently.
func renderDefaultConfig() []byte {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		gitPath = "/usr/bin/git"
	}
	return []byte(strings.ReplaceAll(defaultConfigTemplate, "__GIT_BINARY__", gitPath))
}

// EnsureConfigFile writes the default config to path when no file exists there
// yet, creating the parent directory (0700) and the file (0600). It never
// overwrites an existing file, so it is safe to call on every startup; created
// reports whether a new file was written.
func EnsureConfigFile(path string) (created bool, err error) {
	path = expandPath(path)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("creating config dir: %w", err)
	}

	// O_EXCL is the authoritative, race-free existence check: the file is
	// created only if it does not already exist, so an existing config is never
	// clobbered (os.IsExist → created=false) — no separate Stat, no TOCTOU gap.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("creating config %q: %w", path, err)
	}
	defer f.Close()

	if _, err := f.Write(renderDefaultConfig()); err != nil {
		return false, fmt.Errorf("writing default config: %w", err)
	}
	return true, nil
}
