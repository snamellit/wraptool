package doctor

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Env is the injected host environment: every syscall-shaped thing a check may
// do DIRECTLY. A check that calls os.Stat / os.Getenv / exec.LookPath /
// time.Now / filepath.EvalSymlinks / paths.* directly (bypassing Env) is a
// bug, and TestChecksDoNotTouchTheRealHost fails the build for it.
//
// Sanctioned exception (design §11): a check MAY call a sibling package's own
// host-touching function instead of going through Env —
// config.Load/LoadStrict/Validate (which os.Stat token files and tool
// binaries directly, internal/config/validate.go), introspect.CacheStamp, and
// requestlog's readers. TestChecksDoNotTouchTheRealHost cannot see through a
// call like that, so a check using one of these is untestable against a
// MapEnv and MUST be tested against a t.TempDir() fixture tree instead (see
// plan §1 rule 10) — never asserted "no real filesystem" the way every other
// check is.
//
// Every method is total and cheap. Nothing here EXECUTES a program: probing is
// Phase F and arrives as an Input field, deliberately NOT as an Exec method on
// Env, so "no shell execution, fixed help vocabulary" (§5, CLAUDE.md) stays
// enforceable by type rather than by review. A1 executes nothing at all.
//
// This interface is CLOSED in A1 (plan §1 rule 10). Adding a method breaks no
// Phase-B fake — every Phase-B fake is a MapEnv literal, which is data — but it
// does edit host.go and mapenv.go, two files shared by all nine Phase-B
// branches. A step that believes it needs a new method stops and reports rather
// than widening it.
type Env interface {
	// LookPath resolves a binary on PATH. Returns exec.ErrNotFound when absent.
	// A1's env.container-backend; B1.
	LookPath(file string) (string, error)
	// Stat follows symlinks. B3/B5/B6/B8 permission checks, nearest-existing-
	// ancestor walks (env.xdg-writable, exposure.unix-socket-dir).
	Stat(path string) (FileInfo, error)
	// Lstat does not follow symlinks. B3's config.symlinked.
	Lstat(path string) (FileInfo, error)
	// EvalSymlinks resolves a path fully. B3's config.file-perms explicitly
	// specifies "symlinks resolved" (§7.2).
	EvalSymlinks(path string) (string, error)
	// ReadDir returns Lstat-shaped entries with Path joined. B8's pool home/
	// tree walk.
	ReadDir(path string) ([]FileInfo, error)
	// ReadFile: token file, overrides file, devcontainer.json, serve.state,
	// and the config bytes C1 needs.
	ReadFile(path string) ([]byte, error)
	// Getenv. E1's env.path-differs-from-server.
	Getenv(key string) string
	// Getuid/Getgid return -1 when unknown (Windows). B1, B6, E1.
	Getuid() int
	Getgid() int
	// ProcessAlive gates B9's runtime.* checks. Implemented by passing cmd's
	// own platform-split processAlive into NewOSEnv — see below.
	ProcessAlive(pid int) bool
	// Now. E2's runtime.stale-policy and the report header.
	Now() time.Time
}

// FileInfo is doctor's OWN stat result, deliberately not fs.FileInfo: uid/gid
// are only reachable through fs.FileInfo.Sys().(*syscall.Stat_t), which returns
// nil from any hand-written fake and is platform-specific. An fs.FileInfo-based
// Env would make every ownership check untestable (tools.binary-writable,
// env.xdg-writable, exposure.token-owner) and force `syscall` into check files.
type FileInfo struct {
	Path    string      // absolute path as requested (or joined, from ReadDir)
	Name    string      // base name
	Mode    fs.FileMode // type bits + permission bits
	Size    int64
	ModTime time.Time
	UID     int // -1 unknown
	GID     int // -1 unknown
}

func (fi FileInfo) IsDir() bool       { return fi.Mode.IsDir() }
func (fi FileInfo) IsSymlink() bool   { return fi.Mode&fs.ModeSymlink != 0 }
func (fi FileInfo) Perm() fs.FileMode { return fi.Mode.Perm() }
func (fi FileInfo) OwnerKnown() bool  { return fi.UID >= 0 }

// GroupOrOtherWritable reports whether group or other has any write bit set.
func (fi FileInfo) GroupOrOtherWritable() bool { return fi.Perm()&0o022 != 0 }

// GroupOrOtherReadable reports whether group or other has any read bit set.
func (fi FileInfo) GroupOrOtherReadable() bool { return fi.Perm()&0o044 != 0 }

// osEnv is the real, OS-backed Env. processAlive is injected (see NewOSEnv)
// rather than implemented here so this package does not need its own
// platform-split process-liveness pair.
type osEnv struct {
	processAlive func(pid int) bool
}

// NewOSEnv returns an Env backed by the real OS.
//
// processAlive is a PARAMETER, not an internal implementation, because
// cmd/platform_unix.go:24 and cmd/platform_windows.go:34 already carry a
// correct, platform-split implementation and `cmd` imports `doctor`, so the
// dependency only runs one way. Passing cmd's own function avoids a duplicate
// host_unix.go/host_windows.go pair inside internal/doctor and means B9 — which
// sits in the parallel wave and therefore cannot widen Env — needs no seam
// change at all. A nil argument yields a predicate that always reports false.
func NewOSEnv(processAlive func(pid int) bool) Env {
	if processAlive == nil {
		processAlive = func(int) bool { return false }
	}
	return osEnv{processAlive: processAlive}
}

func (osEnv) LookPath(file string) (string, error) { return exec.LookPath(file) }

func (osEnv) Stat(path string) (FileInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return FileInfo{}, err
	}
	return toFileInfo(path, fi), nil
}

func (osEnv) Lstat(path string) (FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return FileInfo{}, err
	}
	return toFileInfo(path, fi), nil
}

func (osEnv) EvalSymlinks(path string) (string, error) { return filepath.EvalSymlinks(path) }

func (osEnv) ReadDir(path string) ([]FileInfo, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, toFileInfo(filepath.Join(path, e.Name()), info))
	}
	return out, nil
}

func (osEnv) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (osEnv) Getenv(key string) string { return os.Getenv(key) }

func (osEnv) Getuid() int { return os.Getuid() }

func (osEnv) Getgid() int { return os.Getgid() }

func (e osEnv) ProcessAlive(pid int) bool { return e.processAlive(pid) }

func (osEnv) Now() time.Time { return time.Now() }

// toFileInfo converts a standard library fs.FileInfo into doctor's own
// FileInfo, filling UID/GID via the platform-specific statOwnership (see
// host_stat_unix.go / host_stat_other.go — the only files in this package
// that touch syscall).
func toFileInfo(path string, fi fs.FileInfo) FileInfo {
	uid, gid := statOwnership(fi)
	return FileInfo{
		Path:    path,
		Name:    fi.Name(),
		Mode:    fi.Mode(),
		Size:    fi.Size(),
		ModTime: fi.ModTime(),
		UID:     uid,
		GID:     gid,
	}
}
