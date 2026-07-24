package config

import (
	_ "embed"
	"errors"
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
// on PATH and made absolute (LookPath can return a PATH-relative path, which the
// whitelist would reject); if git is absent we fall back to a conventional
// location and let config validation surface the bad path rather than guessing
// silently.
func renderDefaultConfig() []byte {
	gitPath, err := exec.LookPath("git")
	if err == nil {
		if abs, aerr := filepath.Abs(gitPath); aerr == nil {
			gitPath = abs
		}
	} else {
		gitPath = "/usr/bin/git"
	}
	return []byte(strings.ReplaceAll(defaultConfigTemplate, "__GIT_BINARY__", gitPath))
}

// EnsureConfigFile writes the default config to path when no file exists there
// yet, creating the parent directory (0700). It never overwrites an existing
// file; created reports whether a new file was written, so it is safe to call on
// every startup.
//
// The write is atomic: the content is written to a temp file in the same
// directory and published with os.Link, which fails if the target already
// exists. A concurrent reader therefore sees either no config or the complete
// config (never a half-written file), a racing writer that loses the Link gets
// created=false rather than clobbering, and a crash mid-write leaves only the
// temp behind — never a truncated config that would poison every later startup.
func EnsureConfigFile(path string) (created bool, err error) {
	path = expandPath(path)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("creating config dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp") // 0600 by default
	if err != nil {
		return false, fmt.Errorf("creating temp config: %w", err)
	}
	tmpName := tmp.Name()
	// Remove the temp name on every path: it is a no-op on the target once
	// os.Link succeeds (the content lives on under path), and cleanup otherwise.
	defer os.Remove(tmpName)

	if _, werr := tmp.Write(renderDefaultConfig()); werr != nil {
		tmp.Close()
		return false, fmt.Errorf("writing default config: %w", werr)
	}
	// Check Close: a buffered write can surface ENOSPC/quota only on close.
	if cerr := tmp.Close(); cerr != nil {
		return false, fmt.Errorf("writing default config: %w", cerr)
	}

	if lerr := os.Link(tmpName, path); lerr != nil {
		if errors.Is(lerr, os.ErrExist) {
			return false, nil // already present — never clobber
		}
		return false, fmt.Errorf("publishing config %q: %w", path, lerr)
	}
	return true, nil
}
