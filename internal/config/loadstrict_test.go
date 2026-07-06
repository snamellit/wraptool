package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return p
}

const validBody = `
server:
  transport: stdio
tools:
  git:
    binary: /usr/bin/git
    allow:
      - subcommand: [status]
        flags: [--short]
        flag_constraints:
          --short:
            type: boolean
`

func TestLoadStrictAcceptsValidConfig(t *testing.T) {
	cfg, err := LoadStrict(writeConfig(t, validBody))
	if err != nil {
		t.Fatalf("LoadStrict rejected a valid config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate failed on a valid config: %v", err)
	}
	if cfg.Server.Transport != "stdio" {
		t.Errorf("defaults not applied: transport = %q", cfg.Server.Transport)
	}
}

func TestLoadStrictRejectsUnknownKey(t *testing.T) {
	// flag_constraint (singular) is a typo for flag_constraints; the lenient
	// loader silently ignores it, which is exactly the bug `config validate`
	// exists to catch.
	body := `
tools:
  git:
    binary: /usr/bin/git
    allow:
      - subcommand: [status]
        flag_constraint:
          --short: {type: boolean}
`
	p := writeConfig(t, body)

	_, err := LoadStrict(p)
	if err == nil {
		t.Fatal("LoadStrict accepted an unknown key (flag_constraint)")
	}
	if !strings.Contains(err.Error(), "flag_constraint") {
		t.Errorf("error should name the offending key, got: %v", err)
	}

	// The lenient Load must still tolerate it (forward-compat behaviour).
	if _, err := Load(p); err != nil {
		t.Errorf("lenient Load should ignore the unknown key, got: %v", err)
	}
}

func TestLoadStrictEmptyFileIsZeroConfig(t *testing.T) {
	// An empty document must not be a parse error; it yields a zero config with
	// defaults applied (matching the previous yaml.Unmarshal behaviour).
	cfg, err := LoadStrict(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("LoadStrict errored on an empty file: %v", err)
	}
	if cfg.Server.Transport != "stdio" {
		t.Errorf("defaults not applied to empty config: transport = %q", cfg.Server.Transport)
	}
}

func TestLoadStrictReportsMissingFile(t *testing.T) {
	_, err := LoadStrict(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
