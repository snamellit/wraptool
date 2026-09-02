package harness

import (
	"os"
	"path/filepath"
	"strings"
)

// ensureGitExclude appends path's worktree-relative form to that repo's
// .git/info/exclude, so a generated harness config — which may carry a
// bearer token — is never accidentally `git add -A`ed into a commit. It is a
// silent no-op when path is not inside a git repo, and dedupes: a path
// already present (as its own line) is left alone.
func ensureGitExclude(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	excludeFile, root, ok, err := gitExcludePath(filepath.Dir(abs))
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)

	existing, err := os.ReadFile(excludeFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if line == rel {
			return nil // already excluded
		}
	}

	if err := os.MkdirAll(filepath.Dir(excludeFile), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(excludeFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	entry := rel + "\n"
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		entry = "\n" + entry
	}
	_, err = f.WriteString(entry)
	return err
}

// gitExcludePath locates the info/exclude file for the git repo containing
// dir, and the worktree root that exclude patterns (like .gitignore) are
// matched relative to. ok is false, with a nil error, when dir is not inside
// a git repo at all.
func gitExcludePath(dir string) (excludeFile, worktreeRoot string, ok bool, err error) {
	root, gitEntry, err := findGitEntry(dir)
	if err != nil {
		return "", "", false, err
	}
	if gitEntry == "" {
		return "", "", false, nil
	}
	commonDir, err := resolveCommonDir(gitEntry)
	if err != nil {
		return "", "", false, err
	}
	return filepath.Join(commonDir, "info", "exclude"), root, true, nil
}

// findGitEntry walks up from dir looking for a ".git" entry (directory or
// file), the way git itself locates a repo. It returns the directory it was
// found in (the worktree root) and the absolute path to the entry. A nil
// error with an empty gitEntry means dir is not inside a git repo.
func findGitEntry(dir string) (root, gitEntry string, err error) {
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	for {
		candidate := filepath.Join(dir, ".git")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return dir, candidate, nil
		} else if !os.IsNotExist(statErr) {
			return "", "", statErr
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", nil // reached the filesystem root
		}
		dir = parent
	}
}

// resolveCommonDir returns the git dir that info/exclude actually lives in
// for gitEntry (a repo's ".git" path). For an ordinary repo that is gitEntry
// itself. For a linked worktree or a submodule, ".git" is a file containing
// "gitdir: <path>" pointing at a per-worktree directory that itself holds a
// "commondir" file naming the shared repo dir (where info/exclude lives,
// shared across all worktrees) — see gitrepository-layout(5).
func resolveCommonDir(gitEntry string) (string, error) {
	info, err := os.Stat(gitEntry)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return gitEntry, nil
	}

	data, err := os.ReadFile(gitEntry)
	if err != nil {
		return "", err
	}
	const prefix = "gitdir:"
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, prefix) {
		return "", &os.PathError{Op: "parse", Path: gitEntry, Err: os.ErrInvalid}
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(gitEntry), target)
	}
	target = filepath.Clean(target)

	commonData, err := os.ReadFile(filepath.Join(target, "commondir"))
	if err != nil {
		if os.IsNotExist(err) {
			return target, nil
		}
		return "", err
	}
	common := strings.TrimSpace(string(commonData))
	if !filepath.IsAbs(common) {
		common = filepath.Join(target, common)
	}
	return filepath.Clean(common), nil
}
