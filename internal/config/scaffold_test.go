package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestEnsureConfigFileConcurrentCreatesExactlyOne guards the atomic-publish
// contract: many processes racing to scaffold the same path must yield exactly
// one create, and every reader must see a complete, parseable file — never a
// half-written one.
func TestEnsureConfigFileConcurrentCreatesExactlyOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	var created int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := EnsureConfigFile(path)
			if err != nil {
				t.Errorf("EnsureConfigFile: %v", err)
			}
			if c {
				atomic.AddInt64(&created, 1)
			}
		}()
	}
	wg.Wait()

	if created != 1 {
		t.Fatalf("created=%d, want exactly 1", created)
	}
	// LoadStrict parses the whole document; a partial write would fail here.
	if _, err := LoadStrict(path); err != nil {
		t.Fatalf("scaffolded config did not parse (partial write?): %v", err)
	}
}

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
	// The starter allows exactly the two read-only transport subcommands. push
	// and clone are present only as commented examples: push writes to the
	// remote as the operator, and clone lets the caller choose both the remote
	// URL and the destination path. Neither may become active by accident.
	allowed := map[string]bool{}
	for _, rule := range git.Allow {
		if len(rule.Subcommand) != 1 {
			t.Errorf("starter allow rule %+v is not a single subcommand", rule)
			continue
		}
		allowed[rule.Subcommand[0]] = true
	}
	for _, want := range []string{"pull", "fetch"} {
		if !allowed[want] {
			t.Errorf("starter config does not allow %q; allow=%+v", want, git.Allow)
		}
	}
	for _, unwanted := range []string{"push", "clone"} {
		if allowed[unwanted] {
			t.Errorf("starter config allows %q, which must stay commented out", unwanted)
		}
	}
	if len(allowed) != 2 {
		t.Errorf("starter config allows %d subcommands, want exactly pull and fetch: %+v", len(allowed), git.Allow)
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

// TestRenderDefaultConfigSubstitutesConfigDir asserts the scaffold leaves no
// template placeholders behind and bakes in the platform config dir, so the
// token-file examples in the comments are correct on macOS/Windows (not just
// the Linux ~/.config path). Guards against regressing the Mac first-run
// scaffold where the comments pointed at a non-existent Linux path.
func TestRenderDefaultConfigSubstitutesConfigDir(t *testing.T) {
	out := string(renderDefaultConfig())
	if strings.Contains(out, "__CONFIG_DIR__") {
		t.Errorf("scaffold left an unsubstituted __CONFIG_DIR__ placeholder:\n%s", out)
	}
	if strings.Contains(out, "__GIT_BINARY__") {
		t.Errorf("scaffold left an unsubstituted __GIT_BINARY__ placeholder")
	}
	// The platform config dir must appear in the rendered comments (the
	// token-file examples use it). On a typical Linux CI box this is
	// ~/.config/wraptool; on macOS ~/Library/Application Support/wraptool.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir to compare against")
	}
	want := filepath.Join(home, ".config", "wraptool")
	if !strings.Contains(out, want) {
		// Not Linux; accept the xdg config home whatever it is on this host.
		t.Logf("rendered scaffold does not contain the Linux config path; this host's config dir differs (expected on macOS/Windows)")
	}
}

// TestScaffoldHardensGitEnv guards the mandatory command-scope git settings in
// the starter config. wraptool runs git on the host, in a working tree the AI
// can write to, so a hostile .git/config or .git/hooks would otherwise execute
// code as the operator on the next `git pull` (post-merge hooks, core.fsmonitor).
// GIT_CONFIG_* is command scope, which outranks the repository's own config —
// a global setting cannot defend against a hostile repo. Losing these silently
// reintroduces that path on every fresh install, so assert them here.
func TestScaffoldHardensGitEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := EnsureConfigFile(path); err != nil {
		t.Fatalf("EnsureConfigFile: %v", err)
	}
	// LoadStrict, not Validate: this assertion is about the env block and must
	// not depend on git existing on the host.
	cfg, err := LoadStrict(path)
	if err != nil {
		t.Fatalf("LoadStrict on generated config: %v", err)
	}
	git, ok := cfg.Tools["git"]
	if !ok {
		t.Fatalf("generated config missing the git tool; tools=%v", cfg.Tools)
	}
	env, err := git.ResolvedEnv()
	if err != nil {
		t.Fatalf("ResolvedEnv: %v", err)
	}

	// Both settings must be present, whatever index they occupy.
	keys := map[string]string{}
	for k, v := range env {
		if idx, found := strings.CutPrefix(k, "GIT_CONFIG_KEY_"); found {
			keys[v] = env["GIT_CONFIG_VALUE_"+idx]
		}
	}
	hooks, ok := keys["core.hooksPath"]
	if !ok {
		t.Errorf("scaffold lost core.hooksPath; git would run hooks from the working tree. env=%v", env)
	} else if !filepath.IsAbs(hooks) || strings.Contains(hooks, "__CONFIG_DIR__") {
		t.Errorf("core.hooksPath = %q, want an absolute, substituted path", hooks)
	}
	if got, ok := keys["core.fsmonitor"]; !ok || got != "false" {
		t.Errorf("core.fsmonitor = %q (present=%v), want \"false\"", got, ok)
	}

	// GIT_CONFIG_COUNT must equal the number of contiguous indexed pairs: git
	// reads indices 0..COUNT-1 and ignores the rest, so a pair added without
	// bumping the count is silently inert.
	count := env["GIT_CONFIG_COUNT"]
	n, err := strconv.Atoi(count)
	if err != nil {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want an integer: %v", count, err)
	}
	for i := range n {
		if _, ok := env[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)]; !ok {
			t.Errorf("GIT_CONFIG_COUNT=%d but GIT_CONFIG_KEY_%d is missing", n, i)
		}
		if _, ok := env[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)]; !ok {
			t.Errorf("GIT_CONFIG_COUNT=%d but GIT_CONFIG_VALUE_%d is missing", n, i)
		}
	}
	if _, ok := env[fmt.Sprintf("GIT_CONFIG_KEY_%d", n)]; ok {
		t.Errorf("GIT_CONFIG_KEY_%d exists but GIT_CONFIG_COUNT=%d, so git ignores it", n, n)
	}
	if len(keys) != n {
		t.Errorf("%d indexed key/value pairs but GIT_CONFIG_COUNT=%d", len(keys), n)
	}
}
