package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// poolDirPerm is used for every pool directory this package creates. 0o755,
// not 0o700: the files this package writes carry no secrets — harness
// auth/config/skills live only under home/, which this package never creates
// or writes — and runtime/ specifically must be traversable by the container
// UID that execs the dispatcher and generation binaries at container run
// time (§5, §12). A tighter mode on runtime/ would silently break every
// project container's read access to it.
const poolDirPerm = 0o755

// WriteFileAtomic writes data to path via the shared atomic-write pattern
// (§6.4): a temp file created in the destination's own directory (so the
// later rename stays on one volume), fsynced, then renamed over path. The
// parent directory is fsynced too, but only on POSIX — Windows has no
// directory-fsync equivalent and NTFS rename is durable without it. Every
// pool mutation (manifest, lock, observations, pointer files) goes through
// this one helper (or WritePointer, which shares its rename discipline),
// mirroring internal/config's writeAtomic.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return atomicWrite(path, ".wraptool-pool-*.tmp", data, perm)
}

// WritePointer atomically activates generationID for one platform/harness
// pair by writing it to the pointer file at path (§5.2): a single line, the
// generation id, LF-terminated, UTF-8, no other content. The temp file is
// named "<pointer-basename>.tmp-<random>" — the literal
// "<harness>.gen.tmp-<txn>" shape from §5.2 — rather than
// WriteFileAtomic's generic temp name, so an orphaned temp file surviving a
// crash under active/ is recognizable as pool cleanup work for a future `gc`
// (not implemented in this package) instead of an anonymous stray.
func WritePointer(path, generationID string) error {
	return atomicWrite(path, filepath.Base(path)+".tmp-*", []byte(generationID+"\n"), 0o644)
}

// atomicWrite is the shared fsync+rename implementation behind
// WriteFileAtomic and WritePointer; tmpPattern is an os.CreateTemp pattern
// controlling only the temp file's name.
func atomicWrite(path, tmpPattern string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, poolDirPerm); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return fmt.Errorf("setting permissions on temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsyncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp file over %s: %w", path, err)
	}

	// Parent-directory fsync makes the rename itself durable. Skipped on
	// Windows: there is no directory-fsync equivalent there (§6.4).
	if runtime.GOOS != "windows" {
		if dirFile, err := os.Open(dir); err == nil {
			_ = dirFile.Sync()
			_ = dirFile.Close()
		}
	}

	return nil
}

// ReadPointer reads a pointer file and returns the active generation id,
// trimming exactly one trailing newline (§5.2). A missing pointer file
// returns the underlying os error (test with os.IsNotExist / errors.Is) so
// callers can distinguish "not realized for this platform" from other
// failures.
func ReadPointer(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(data), "\n"), nil
}
