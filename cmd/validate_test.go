package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return p
}

// runValidate executes `config validate <args...>` through the root command,
// returning combined output and the command error.
func runValidate(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"config", "validate"}, args...))
	err := root.Execute()
	return buf.String(), err
}

const goodConfig = `
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

func TestConfigValidateAcceptsGoodConfig(t *testing.T) {
	out, err := runValidate(t, writeCfg(t, goodConfig))
	if err != nil {
		t.Fatalf("validate returned error on a good config: %v", err)
	}
	if !strings.Contains(out, "ok:") {
		t.Errorf("expected an ok message, got: %q", out)
	}
}

func TestConfigValidateRejectsBothArgAndFlag(t *testing.T) {
	p := writeCfg(t, goodConfig)
	// Both are individually valid; passing them together is the ambiguity we
	// reject so a --config value is never silently ignored behind a positional.
	_, err := runValidate(t, p, "--config", p)
	if err == nil {
		t.Fatal("expected an error when both a positional path and --config are given")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("expected a mutual-exclusivity error, got: %v", err)
	}
}

func TestConfigValidateRejectsUnknownKey(t *testing.T) {
	body := `
tools:
  git:
    binary: /usr/bin/git
    allow:
      - subcommand: [status]
        flag_constraint:
          --short: {type: boolean}
`
	_, err := runValidate(t, writeCfg(t, body))
	if err == nil {
		t.Fatal("expected an error for a typo'd key")
	}
	if !strings.Contains(err.Error(), "flag_constraint") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestConfigValidateRejectsBadSemantics(t *testing.T) {
	body := `
server:
  transport: carrier-pigeon
tools:
  git:
    binary: /usr/bin/git
    allow:
      - subcommand: [status]
`
	_, err := runValidate(t, writeCfg(t, body))
	if err == nil {
		t.Fatal("expected a validation error for a bad transport")
	}
	if !strings.Contains(err.Error(), "transport") {
		t.Errorf("expected a transport validation error, got: %v", err)
	}
}

func TestConfigValidateResolvesEnvVar(t *testing.T) {
	p := writeCfg(t, goodConfig)
	t.Setenv("WRAPTOOL_CONFIG", p)
	out, err := runValidate(t) // no positional arg or flag
	if err != nil {
		t.Fatalf("validate via WRAPTOOL_CONFIG errored: %v", err)
	}
	if !strings.Contains(out, "ok:") {
		t.Errorf("expected ok via WRAPTOOL_CONFIG, got: %q", out)
	}
}
