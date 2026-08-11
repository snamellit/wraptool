package doctor

import (
	"io/fs"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

// MapEnv is a data-only Env. A Phase-B author populates maps and never writes a
// method — which is why adding an Env method never breaks an in-flight branch.
// It is production code, not a _test.go type: cmd/doctor_test.go needs it too
// (env.container-backend calls LookPath, so a cmd-level exit-code test against
// the real PATH would be non-deterministic across CI runners), and a _test.go
// type in package doctor is invisible from package cmd.
type MapEnv struct {
	PathBinaries map[string]string  // "docker" -> "/usr/bin/docker"; absent => exec.ErrNotFound
	Files        map[string]MapFile // absolute path -> content/mode/uid/gid (directories too)
	Links        map[string]string  // symlink path -> target, for Lstat/EvalSymlinks
	Vars         map[string]string
	Pids         map[int]bool
	UID, GID     int
	Clock        time.Time
}

// MapFile is one synthetic file or directory in a MapEnv. UID/GID are used
// verbatim (there is no per-entry "-1 unknown" default): a test that cares
// about ownership sets them explicitly.
type MapFile struct {
	Data     []byte
	Mode     fs.FileMode
	ModTime  time.Time
	UID, GID int
}

// NewMapEnv returns a MapEnv with initialized maps, UID/GID = -1 (unknown) and a
// fixed clock. ALWAYS use it: the zero MapEnv reports uid 0, i.e. root, which
// silently changes what every ownership check concludes.
func NewMapEnv() *MapEnv {
	return &MapEnv{
		PathBinaries: map[string]string{},
		Files:        map[string]MapFile{},
		Links:        map[string]string{},
		Vars:         map[string]string{},
		Pids:         map[int]bool{},
		UID:          -1,
		GID:          -1,
		Clock:        time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (m *MapEnv) LookPath(file string) (string, error) {
	if p, ok := m.PathBinaries[file]; ok {
		return p, nil
	}
	return "", exec.ErrNotFound
}

// resolve follows m.Links from path, bounded to 32 hops, and returns the final
// non-symlink path.
func (m *MapEnv) resolve(path string) (string, error) {
	cur := path
	for hops := 0; ; hops++ {
		target, ok := m.Links[cur]
		if !ok {
			return cur, nil
		}
		if hops >= 32 {
			return "", fs.ErrNotExist
		}
		cur = target
	}
}

func (m *MapEnv) Stat(path string) (FileInfo, error) {
	final, err := m.resolve(path)
	if err != nil {
		return FileInfo{}, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	mf, ok := m.Files[final]
	if !ok {
		return FileInfo{}, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	return toMapFileInfo(path, mf), nil
}

func (m *MapEnv) Lstat(path string) (FileInfo, error) {
	if target, ok := m.Links[path]; ok {
		return FileInfo{
			Path: path,
			Name: filepath.Base(path),
			Mode: fs.ModeSymlink | 0o777,
			Size: int64(len(target)),
			UID:  -1,
			GID:  -1,
		}, nil
	}
	mf, ok := m.Files[path]
	if !ok {
		return FileInfo{}, &fs.PathError{Op: "lstat", Path: path, Err: fs.ErrNotExist}
	}
	return toMapFileInfo(path, mf), nil
}

func (m *MapEnv) EvalSymlinks(path string) (string, error) {
	final, err := m.resolve(path)
	if err != nil {
		return "", &fs.PathError{Op: "realpath", Path: path, Err: err}
	}
	if _, ok := m.Files[final]; !ok {
		return "", &fs.PathError{Op: "realpath", Path: path, Err: fs.ErrNotExist}
	}
	return final, nil
}

func (m *MapEnv) ReadDir(path string) ([]FileInfo, error) {
	// Resolve the directory itself through Links, like Stat/EvalSymlinks/
	// ReadFile: a symlinked directory must be walkable the same way a
	// symlinked file is readable.
	final, err := m.resolve(path)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: path, Err: err}
	}
	dir, ok := m.Files[final]
	if !ok || !dir.Mode.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: path, Err: fs.ErrNotExist}
	}
	seen := map[string]bool{}
	var out []FileInfo
	collect := func(child string) {
		if seen[child] || filepath.Dir(child) != final {
			return
		}
		seen[child] = true
		if fi, err := m.Lstat(child); err == nil {
			// Re-root onto the REQUESTED path, not the resolved one: osEnv.ReadDir
			// (host.go) joins filepath.Join(path, e.Name()), so a caller walking a
			// symlinked directory must see entries rooted at the path it asked
			// for, not the link's target, or a finding's Location/Detail built
			// from fi.Path diverges between MapEnv and the real host.
			fi.Path = filepath.Join(path, filepath.Base(child))
			out = append(out, fi)
		}
	}
	for child := range m.Files {
		collect(child)
	}
	for child := range m.Links {
		collect(child)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReadFile follows m.Links like Stat/EvalSymlinks (and the real OS): a symlinked
// config file (B3's config.symlinked, C1's ConfigRaw read) must Stat and
// ReadFile through the same resolved path, or a fake built the "obvious" way —
// Stat succeeding, ReadFile returning fs.ErrNotExist — diverges from osEnv,
// whose os.ReadFile always follows symlinks.
func (m *MapEnv) ReadFile(path string) ([]byte, error) {
	final, err := m.resolve(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	mf, ok := m.Files[final]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), mf.Data...), nil
}

func (m *MapEnv) Getenv(key string) string { return m.Vars[key] }

func (m *MapEnv) Getuid() int { return m.UID }

func (m *MapEnv) Getgid() int { return m.GID }

func (m *MapEnv) ProcessAlive(pid int) bool { return m.Pids[pid] }

func (m *MapEnv) Now() time.Time { return m.Clock }

func toMapFileInfo(path string, mf MapFile) FileInfo {
	return FileInfo{
		Path:    path,
		Name:    filepath.Base(path),
		Mode:    mf.Mode,
		Size:    int64(len(mf.Data)),
		ModTime: mf.ModTime,
		UID:     mf.UID,
		GID:     mf.GID,
	}
}
