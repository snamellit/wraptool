package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/pool"

	"gopkg.in/yaml.v3"
)

// runHarnessCmd executes `harness <args...>` through the root command,
// returning combined output and the command error. Mirrors runValidate's
// pattern in cmd/validate_test.go.
func runHarnessCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"harness"}, args...))
	err := root.Execute()
	return buf.String(), err
}

// TestHarnessCommandArgValidation table-tests every subcommand's argument
// validation. Every case here must fail before touching the filesystem (no
// pool paths, no Source, no backend) — each one returns from its RunE before
// reaching newHarnessSource()/resolveBackend()/setHarnessConstraint, so none
// of them can write into the real developer pool ($XDG_DATA_HOME).
func TestHarnessCommandArgValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"install: unknown harness", []string{"install", "bogus"}, "unknown harness"},
		{"install: host IDE harness is not a container CLI", []string{"install", "cursor"}, "host-side IDE"},
		{"install: --allow-young without --reason", []string{"install", "claude", "--allow-young"}, "--reason"},
		{"refresh: unknown harness", []string{"refresh", "bogus"}, "unknown harness"},
		{"refresh: legacy (non-lockable) harness", []string{"refresh", "agy"}, "not pool-lockable"},
		{"update: unknown harness", []string{"update", "bogus"}, "unknown harness"},
		{"update: legacy (non-lockable) harness", []string{"update", "gemini"}, "not pool-lockable"},
		{"update: --allow-young without --reason", []string{"update", "claude", "--allow-young"}, "--reason"},
		{"sync: unknown harness", []string{"sync", "bogus"}, "unknown harness"},
		{"sync: legacy (non-lockable) harness", []string{"sync", "roo"}, "not pool-lockable"},
		{"policy set: no flags given", []string{"policy", "set"}, "specify at least one of"},
		{"policy set: unknown --harness", []string{"policy", "set", "--harness", "bogus", "--minimum-release-age", "1h"}, "unknown harness"},
		{"policy set: negative --minimum-release-age", []string{"policy", "set", "--minimum-release-age", "-1h"}, "must not be negative"},
		{"policy set: negative --minimum-observation-age", []string{"policy", "set", "--minimum-observation-age", "-1h"}, "must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runHarnessCmd(t, tc.args...)
			if err == nil {
				t.Fatalf("expected an error, got none; output:\n%s", out)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestHarnessInstallTooManyArgs and TestHarnessUpdateTooManyArgs pin cobra's
// own arity enforcement (ExactArgs(1) for install, MaximumNArgs(1) for the
// optional-harness commands) so a future edit can't accidentally loosen it.
func TestHarnessInstallRequiresExactlyOneArg(t *testing.T) {
	if _, err := runHarnessCmd(t, "install"); err == nil {
		t.Error("expected an error for `install` with no harness name")
	}
	if _, err := runHarnessCmd(t, "install", "claude", "extra"); err == nil {
		t.Error("expected an error for `install` with more than one argument")
	}
}

func TestHarnessUpdateAcceptsAtMostOneArg(t *testing.T) {
	if _, err := runHarnessCmd(t, "update", "claude", "extra"); err == nil {
		t.Error("expected an error for `update` with more than one argument")
	}
}

// --- list rendering (§10 column shapes) ---

func testHarnessPaths(t *testing.T) pool.PoolPaths {
	t.Helper()
	dir := t.TempDir()
	return pool.PoolPaths{
		Manifest:     filepath.Join(dir, "manifest.yaml"),
		Lock:         filepath.Join(dir, "harness-lock.json"),
		Observations: filepath.Join(dir, "observations.json"),
		Audit:        filepath.Join(dir, "audit.jsonl"),
		AdvisoryLock: filepath.Join(dir, "lock"),
		Cache:        filepath.Join(dir, "cache"),
	}
}

func TestRunHarnessListRendersColumnsAndQuarantineCountdown(t *testing.T) {
	paths := testHarnessPaths(t)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	pkg := "@anthropic-ai/claude-code"

	if err := os.WriteFile(paths.Manifest, []byte("schema_version: 1\npolicy:\n  minimum_release_age: 168h\nharnesses:\n  claude:\n    constraint: \">=2 <3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	obs := pool.Observations{}
	if err := obs.Observe(pool.ObservationKey("npm", pkg, "2.1.220"),
		pool.FormatTimestamp(now.Add(-200*time.Hour)), pool.FormatTimestamp(now.Add(-200*time.Hour)), false, false); err != nil {
		t.Fatal(err)
	}
	// Published exactly 165h ago: 168h - 165h = 3h remaining... use days instead.
	quarantinedPublished := now.Add(-(168*time.Hour - 3*24*time.Hour))
	if err := obs.Observe(pool.ObservationKey("npm", pkg, "2.1.224"),
		pool.FormatTimestamp(quarantinedPublished), pool.FormatTimestamp(quarantinedPublished), false, false); err != nil {
		t.Fatal(err)
	}
	if err := obs.Save(paths.Observations); err != nil {
		t.Fatal(err)
	}

	lock := &pool.Lock{
		SchemaVersion: pool.LockSchemaVersion, GeneratedAt: pool.FormatTimestamp(now), PolicyHash: "sha256:x",
		Harnesses: map[string]pool.LockHarness{
			"claude": {
				Source: "npm", Package: pkg, Version: "2.1.220",
				PublishedAt: pool.FormatTimestamp(now.Add(-200 * time.Hour)),
				FirstSeenAt: pool.FormatTimestamp(now.Add(-200 * time.Hour)),
			},
		},
	}
	data, err := lock.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteFileAtomic(paths.Lock, data, 0o644); err != nil {
		t.Fatal(err)
	}

	poolBin := t.TempDir() // no binaries present -> ACTIVE = "no" for CLI harnesses

	var buf bytes.Buffer
	if err := runHarnessList(paths, poolBin, now, &buf); err != nil {
		t.Fatalf("runHarnessList: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "HARNESS") || !strings.Contains(out, "CONSTRAINT") || !strings.Contains(out, "QUARANTINED") {
		t.Fatalf("missing expected header columns, got:\n%s", out)
	}
	if !strings.Contains(out, "2.1.224 (3d left)") {
		t.Errorf("expected the quarantined-with-remaining cell '2.1.224 (3d left)', got:\n%s", out)
	}
	if !strings.Contains(out, ">=2 <3") {
		t.Errorf("expected the claude row's constraint, got:\n%s", out)
	}
	if !strings.Contains(out, "legacy (not lockable)") {
		t.Errorf("expected a legacy-harness row (e.g. agy/gemini/cursor/roo), got:\n%s", out)
	}
}

// --- editManifest / setHarnessConstraint ---

func TestEditManifestPreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	original := "# developer intent — do not remove this comment\n" +
		"schema_version: 1\n" +
		"policy:\n" +
		"  minimum_release_age: 168h # seven days\n" +
		"harnesses:\n" +
		"  claude:\n" +
		"    constraint: \">=2 <3\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	err := editManifest(path, func(doc *yaml.Node) error { return nil })
	if err != nil {
		t.Fatalf("editManifest (no-op mutate): %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "developer intent — do not remove this comment") {
		t.Errorf("top-level comment was dropped; got:\n%s", got)
	}
	if !strings.Contains(string(got), "seven days") {
		t.Errorf("inline comment was dropped; got:\n%s", got)
	}
}

func TestSetHarnessConstraintCreatesAndUpdates(t *testing.T) {
	paths := testHarnessPaths(t)

	if err := setHarnessConstraint(paths, "claude", ">=2 <3"); err != nil {
		t.Fatalf("first setHarnessConstraint: %v", err)
	}
	m, err := pool.LoadManifest(paths.Manifest)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got := m.Harnesses["claude"].Constraint; got != ">=2 <3" {
		t.Errorf("Constraint = %q, want >=2 <3", got)
	}

	// An empty constraint on a later install must not clear the existing one.
	if err := setHarnessConstraint(paths, "claude", ""); err != nil {
		t.Fatalf("second setHarnessConstraint (empty): %v", err)
	}
	m2, err := pool.LoadManifest(paths.Manifest)
	if err != nil {
		t.Fatalf("LoadManifest after empty constraint: %v", err)
	}
	if got := m2.Harnesses["claude"].Constraint; got != ">=2 <3" {
		t.Errorf("Constraint after empty-constraint install = %q, want unchanged >=2 <3", got)
	}
}

// --- policy set round-trip ---

func TestHarnessPolicySetRoundTripsCommentedManifest(t *testing.T) {
	paths := testHarnessPaths(t)
	original := "# do not remove\nschema_version: 1\npolicy:\n  minimum_release_age: 168h\n"
	if err := os.WriteFile(paths.Manifest, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	lock := pool.NewPoolLock(paths.AdvisoryLock)
	ctx := t.Context()
	if err := lock.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	err := editManifest(paths.Manifest, func(doc *yaml.Node) error {
		target := ensureMapChild(doc, "policy")
		mapSetNode(target, "minimum_observation_age", scalarNode((24 * time.Hour).String()))
		return nil
	})
	lock.Release()
	if err != nil {
		t.Fatalf("editManifest: %v", err)
	}

	got, err := os.ReadFile(paths.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "do not remove") {
		t.Errorf("comment was dropped; got:\n%s", got)
	}
	m, err := pool.LoadManifest(paths.Manifest)
	if err != nil {
		t.Fatalf("LoadManifest after policy set: %v", err)
	}
	if m.Policy.MinimumObservationAge == nil || *m.Policy.MinimumObservationAge != 24*time.Hour {
		t.Errorf("MinimumObservationAge = %v, want 24h", m.Policy.MinimumObservationAge)
	}
	if m.Policy.MinimumReleaseAge == nil || *m.Policy.MinimumReleaseAge != 168*time.Hour {
		t.Errorf("MinimumReleaseAge = %v, want the original 168h preserved", m.Policy.MinimumReleaseAge)
	}
}

// --- pinInstallVersion ---

func TestPinInstallVersion(t *testing.T) {
	argv := []string{"guix", "shell", "--container", "--network", "--emulate-fhs",
		"--share={prefix}={prefix}", "node", "--", "npm", "install", "-g", "@anthropic-ai/claude-code", "--prefix", "{prefix}"}
	pinned, err := pinInstallVersion(argv, "@anthropic-ai/claude-code", "2.1.220")
	if err != nil {
		t.Fatalf("pinInstallVersion: %v", err)
	}
	found := false
	for _, a := range pinned {
		if a == "@anthropic-ai/claude-code@2.1.220" {
			found = true
		}
		if a == "@anthropic-ai/claude-code" {
			t.Errorf("floating package argument %q survived pinning", a)
		}
	}
	if !found {
		t.Errorf("pinned argv %v does not contain the pinned package@version", pinned)
	}
	// The original argv must not be mutated (pinInstallVersion copies).
	stillFloating := false
	for _, a := range argv {
		if a == "@anthropic-ai/claude-code" {
			stillFloating = true
		}
	}
	if !stillFloating {
		t.Errorf("pinInstallVersion mutated its input argv in place: %v", argv)
	}
}

func TestPinInstallVersionErrorsWhenPackageNotFound(t *testing.T) {
	argv := []string{"guix", "shell", "node", "--", "npm", "install", "-g", "some-other-pkg"}
	if _, err := pinInstallVersion(argv, "@anthropic-ai/claude-code", "2.1.220"); err == nil {
		t.Fatal("expected an error when the package is not present in argv")
	}
}
