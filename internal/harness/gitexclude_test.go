package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initGitDir makes dir look like a git repo root (just the ".git/" directory
// info/exclude cares about — no real git binary involved).
func initGitDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
}

func TestWriteWritesMode0600(t *testing.T) {
	dir := t.TempDir()
	path, err := Write("claude", Params{Transport: "sse", URL: "http://127.0.0.1:8717/sse", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("mode = %o, want 0600", perm)
	}
}

func TestWriteRefusesOnMergeFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".mcp.json")
	bad := []byte("not json")
	if err := os.WriteFile(target, bad, 0600); err != nil {
		t.Fatal(err)
	}

	_, err := Write("claude", Params{Transport: "sse", URL: "http://127.0.0.1:8717/sse", Dir: dir})
	if err == nil {
		t.Fatal("expected an error on merge failure, got nil")
	}
	if !strings.Contains(err.Error(), target) {
		t.Errorf("error %q does not name the file %q", err, target)
	}

	got, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(bad) {
		t.Errorf("existing file was modified: got %q, want %q", got, bad)
	}
}

func TestWriteExcludesFromGit(t *testing.T) {
	dir := t.TempDir()
	initGitDir(t, dir)

	path, err := Write("claude", Params{Transport: "sse", URL: "http://127.0.0.1:8717/sse", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	exclude := filepath.Join(dir, ".git", "info", "exclude")
	b, rerr := os.ReadFile(exclude)
	if rerr != nil {
		t.Fatalf("reading exclude file: %v", rerr)
	}
	rel, _ := filepath.Rel(dir, path)
	rel = filepath.ToSlash(rel)
	if !containsLine(string(b), rel) {
		t.Errorf("exclude file %q does not contain %q", b, rel)
	}

	// Second run: not duplicated.
	if _, err := Write("claude", Params{Transport: "sse", URL: "http://127.0.0.1:8717/sse", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	b2, rerr := os.ReadFile(exclude)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if n := strings.Count(string(b2), rel+"\n"); n != 1 {
		t.Errorf("exclude entry appeared %d times, want 1:\n%s", n, b2)
	}
}

func TestWriteSkipsExcludeOutsideGitRepo(t *testing.T) {
	dir := t.TempDir() // no .git anywhere above a TempDir in CI/dev boxes' tmp
	path, err := Write("claude", Params{Transport: "sse", URL: "http://127.0.0.1:8717/sse", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("test invariant broken: %q unexpectedly has a .git entry", dir)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}
}

// TestGitExcludeResolvesLinkedWorktree covers the ".git is a file" case
// (linked worktree / submodule): info/exclude lives in the shared common
// dir, named by a "commondir" file, not next to the per-worktree gitdir.
func TestGitExcludeResolvesLinkedWorktree(t *testing.T) {
	root := t.TempDir()
	mainGit := filepath.Join(root, "main", ".git")
	worktree := filepath.Join(root, "wt")
	worktreeGitDir := filepath.Join(mainGit, "worktrees", "wt")

	if err := os.MkdirAll(mainGit, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreeGitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeGitDir, "commondir"), []byte("../..\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+worktreeGitDir+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	excludeFile, wtRoot, ok, err := gitExcludePath(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected worktree to be recognized as a git repo")
	}
	if wtRoot != worktree {
		t.Errorf("worktreeRoot = %q, want %q", wtRoot, worktree)
	}
	if want := filepath.Join(mainGit, "info", "exclude"); excludeFile != want {
		t.Errorf("excludeFile = %q, want %q (shared common dir, not the per-worktree dir)", excludeFile, want)
	}
}

func containsLine(text, line string) bool {
	for _, l := range strings.Split(text, "\n") {
		if l == line {
			return true
		}
	}
	return false
}
