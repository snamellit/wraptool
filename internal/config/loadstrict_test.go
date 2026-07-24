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

// validBody is a config that must pass Validate(), so its tool binary is a real
// executable created for the test rather than a hardcoded path that may not
// exist on the host (e.g. /usr/bin/git is absent on Guix).
func validBody(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing dummy binary: %v", err)
	}
	return `
mcp:
  transport: stdio
tools:
  git:
    binary: ` + bin + `
    allow:
      - subcommand: [status]
        flags: [--short]
        flag_constraints:
          --short:
            type: boolean
`
}

func TestLoadStrictAcceptsValidConfig(t *testing.T) {
	cfg, err := LoadStrict(writeConfig(t, validBody(t)))
	if err != nil {
		t.Fatalf("LoadStrict rejected a valid config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate failed on a valid config: %v", err)
	}
	if cfg.MCP.Transport != "stdio" {
		t.Errorf("defaults not applied: transport = %q", cfg.MCP.Transport)
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

func TestLoadStrictRejectsLegacyServerSection(t *testing.T) {
	p := writeConfig(t, `
server:
  transport: sse
  listen: 127.0.0.1:8717
tools:
  git:
    binary: /usr/bin/git
    allow:
      - subcommand: [status]
`)

	_, err := LoadStrict(p)
	if err == nil || !strings.Contains(err.Error(), "field server not found") {
		t.Fatalf("legacy server section error = %v", err)
	}
}

func TestLoadStrictEmptyFileIsZeroConfig(t *testing.T) {
	// An empty document must not be a parse error; it yields a zero config with
	// defaults applied (matching the previous yaml.Unmarshal behaviour).
	cfg, err := LoadStrict(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("LoadStrict errored on an empty file: %v", err)
	}
	if cfg.MCP.Transport != "sse" || cfg.MCP.Listen != "127.0.0.1:8717" {
		t.Errorf("defaults not applied to empty config: transport=%q listen=%q", cfg.MCP.Transport, cfg.MCP.Listen)
	}
}

func TestLoadStrictReportsMissingFile(t *testing.T) {
	_, err := LoadStrict(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestConstraintTypeRejectsUnknownAtDecode(t *testing.T) {
	// A misspelled constraint type must fail when the config is parsed — via the
	// closed ConstraintType enum's UnmarshalYAML — not silently at call time.
	// This holds for the lenient Load too (an invalid value is not an unknown
	// key), so a broken constraint can never load.
	body := `
tools:
  git:
    binary: /usr/bin/git
    allow:
      - subcommand: [status]
        flags: [--fmt]
        flag_constraints:
          --fmt:
            type: bolean
`
	p := writeConfig(t, body)

	for _, load := range []struct {
		name string
		fn   func(string) (*Config, error)
	}{{"Load", Load}, {"LoadStrict", LoadStrict}} {
		_, err := load.fn(p)
		if err == nil {
			t.Errorf("%s accepted an unknown constraint type", load.name)
			continue
		}
		if !strings.Contains(err.Error(), "bolean") {
			t.Errorf("%s: error should name the offending type, got: %v", load.name, err)
		}
	}
}

func TestConstraintTypeAcceptsKnown(t *testing.T) {
	for _, ct := range []string{"string", "integer", "boolean", ""} {
		body := "tools:\n  git:\n    binary: /usr/bin/git\n    allow:\n" +
			"      - subcommand: [status]\n        flags: [--x]\n" +
			"        flag_constraints:\n          --x:\n            type: " + ct + "\n"
		if _, err := LoadStrict(writeConfig(t, body)); err != nil {
			t.Errorf("type %q should load, got: %v", ct, err)
		}
	}
}

func TestConstraintTypeRejectsNonScalar(t *testing.T) {
	// A YAML sequence/mapping for `type` has an empty scalar value; without a
	// node-kind check it would slip through as ConstraintUntyped.
	for _, bad := range []string{"[boolean]", "[string, integer]", "{a: b}"} {
		body := "tools:\n  git:\n    binary: /usr/bin/git\n    allow:\n" +
			"      - subcommand: [status]\n        flags: [--x]\n" +
			"        flag_constraints:\n          --x:\n            type: " + bad + "\n"
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("type: %s should be rejected (non-scalar)", bad)
		}
	}
}

func TestConstraintKind(t *testing.T) {
	cases := []struct {
		ct        ConstraintType
		schema    string
		valueless bool
		numeric   bool
	}{
		{ConstraintUntyped, "string", false, false},
		{ConstraintString, "string", false, false},
		{ConstraintInteger, "integer", false, true},
		{ConstraintBoolean, "boolean", true, false},
	}
	for _, c := range cases {
		k, ok := c.ct.Kind()
		if !ok {
			t.Errorf("%q: ok=false, want a kind", c.ct)
			continue
		}
		if k.JSONSchema != c.schema || k.Valueless != c.valueless || k.Numeric != c.numeric {
			t.Errorf("%q: got %+v, want {schema:%s valueless:%v numeric:%v}", c.ct, k, c.schema, c.valueless, c.numeric)
		}
	}
	if _, ok := ConstraintType("float").Kind(); ok {
		t.Error("a value outside the enum should return ok=false")
	}
}
