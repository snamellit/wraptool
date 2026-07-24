package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestEnsureConfigFileCreatesWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")

	created, err := EnsureConfigFile(path)
	if err != nil {
		t.Fatalf("EnsureConfigFile: %v", err)
	}
	if !created {
		t.Fatal("expected created=true for a missing file")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perms = %o, want 600", perm)
	}
}

func TestEnsureConfigFileProducesValidConfig(t *testing.T) {
	// Validate() stats the starter tool's git binary, so this test only holds
	// where the rendered path exists. Skip rather than fail on hosts without git
	// on PATH and without the /usr/bin/git fallback — that is host state, not a
	// defect in the code under test.
	if _, err := exec.LookPath("git"); err != nil {
		if _, statErr := os.Stat("/usr/bin/git"); statErr != nil {
			t.Skip("git unavailable; scaffold binary path would not validate here")
		}
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := EnsureConfigFile(path); err != nil {
		t.Fatalf("EnsureConfigFile: %v", err)
	}

	// The generated default must survive the strict, security-critical load path
	// and validation — a broken default would break first-run `serve`.
	cfg, err := LoadStrict(path)
	if err != nil {
		t.Fatalf("LoadStrict on generated config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate on generated config: %v", err)
	}
	// The starter exposes exactly the privileged git tool (git pull) — nothing
	// else — and its binary path was resolved to an absolute path.
	if len(cfg.Tools) != 1 {
		t.Fatalf("generated config exposes %d tools, want 1 (git)", len(cfg.Tools))
	}
	git, ok := cfg.Tools["git"]
	if !ok {
		t.Fatalf("generated config missing the git tool; tools=%v", cfg.Tools)
	}
	if !filepath.IsAbs(git.Binary) {
		t.Errorf("git binary %q is not absolute", git.Binary)
	}
	if len(git.Allow) != 1 || len(git.Allow[0].Subcommand) != 1 || git.Allow[0].Subcommand[0] != "pull" {
		t.Errorf("git allow rules = %+v, want a single [pull]", git.Allow)
	}
}

func TestEnsureConfigFileNeverClobbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	const existing = "mcp:\n  transport: stdio\n  log_level: warn\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	created, err := EnsureConfigFile(path)
	if err != nil {
		t.Fatalf("EnsureConfigFile: %v", err)
	}
	if created {
		t.Error("expected created=false for an existing file")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != existing {
		t.Errorf("existing config was modified:\n%s", got)
	}
}
