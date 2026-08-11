package doctor

import (
	"errors"
	"io/fs"
	"os/exec"
	"testing"
)

func TestNewMapEnvDefaults(t *testing.T) {
	m := NewMapEnv()
	if m.Getuid() != -1 || m.Getgid() != -1 {
		t.Errorf("NewMapEnv() must default UID/GID to -1 (unknown), got %d/%d", m.Getuid(), m.Getgid())
	}
	if m.Files == nil || m.Links == nil || m.Vars == nil || m.Pids == nil || m.PathBinaries == nil {
		t.Error("NewMapEnv() must initialize every map")
	}
}

func TestMapEnvLookPath(t *testing.T) {
	m := NewMapEnv()
	m.PathBinaries["docker"] = "/usr/bin/docker"

	if p, err := m.LookPath("docker"); err != nil || p != "/usr/bin/docker" {
		t.Errorf("LookPath(docker): got (%q, %v)", p, err)
	}
	if _, err := m.LookPath("podman"); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("LookPath(podman): got %v, want exec.ErrNotFound", err)
	}
}

func TestMapEnvStatFollowsSymlinkChain(t *testing.T) {
	m := NewMapEnv()
	m.Files["/real/target"] = MapFile{Mode: 0o644, Data: []byte("hi")}
	m.Links["/link/a"] = "/link/b"
	m.Links["/link/b"] = "/real/target"

	fi, err := m.Stat("/link/a")
	if err != nil {
		t.Fatalf("Stat through a 2-hop symlink chain: %v", err)
	}
	if fi.Size != 2 {
		t.Errorf("Stat resolved to wrong target: size %d, want 2", fi.Size)
	}
	if fi.IsSymlink() {
		t.Error("Stat must report the RESOLVED file's mode, not a symlink")
	}
}

func TestMapEnvLstatDoesNotFollow(t *testing.T) {
	m := NewMapEnv()
	m.Files["/real/target"] = MapFile{Mode: 0o644}
	m.Links["/link/a"] = "/real/target"

	fi, err := m.Lstat("/link/a")
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if !fi.IsSymlink() {
		t.Error("Lstat on a Links entry must report ModeSymlink")
	}
}

func TestMapEnvSymlinkLoopIsBounded(t *testing.T) {
	m := NewMapEnv()
	m.Links["/a"] = "/b"
	m.Links["/b"] = "/a"

	if _, err := m.Stat("/a"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat on a symlink loop: got %v, want fs.ErrNotExist", err)
	}
	if _, err := m.EvalSymlinks("/a"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("EvalSymlinks on a symlink loop: got %v, want fs.ErrNotExist", err)
	}
}

func TestMapEnvMissingPathIsErrNotExist(t *testing.T) {
	m := NewMapEnv()
	if _, err := m.Stat("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(missing): got %v, want fs.ErrNotExist", err)
	}
	if _, err := m.Lstat("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Lstat(missing): got %v, want fs.ErrNotExist", err)
	}
	if _, err := m.ReadFile("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadFile(missing): got %v, want fs.ErrNotExist", err)
	}
	if _, err := m.ReadDir("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadDir(missing): got %v, want fs.ErrNotExist", err)
	}
}

func TestMapEnvReadDir(t *testing.T) {
	m := NewMapEnv()
	m.Files["/dir"] = MapFile{Mode: fs.ModeDir | 0o755}
	m.Files["/dir/b.txt"] = MapFile{Mode: 0o644, Data: []byte("b")}
	m.Files["/dir/a.txt"] = MapFile{Mode: 0o644, Data: []byte("a")}
	m.Links["/dir/c.link"] = "/dir/a.txt"
	// Not a child of /dir — must not appear.
	m.Files["/dir/sub/nested.txt"] = MapFile{Mode: 0o644}

	entries, err := m.ReadDir("/dir")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}
	// Sorted by name: a.txt, b.txt, c.link
	if entries[0].Name != "a.txt" || entries[1].Name != "b.txt" || entries[2].Name != "c.link" {
		t.Errorf("unexpected order: %v, %v, %v", entries[0].Name, entries[1].Name, entries[2].Name)
	}
	if !entries[2].IsSymlink() {
		t.Error("c.link must report as a symlink (Lstat-shaped)")
	}
}

// TestMapEnvReadFileFollowsSymlink pins ReadFile to the same link-following
// semantics as Stat/EvalSymlinks (and the real OS's os.ReadFile): a fake that
// Stats a symlinked path successfully but fails to ReadFile it diverges from
// osEnv and would surface as a mysterious failure inside a Phase-B branch that
// cannot edit this shared file (e.g. B3's config.symlinked).
func TestMapEnvReadFileFollowsSymlink(t *testing.T) {
	m := NewMapEnv()
	m.Files["/real/config.yaml"] = MapFile{Data: []byte("mcp: {}\n"), Mode: 0o600}
	m.Links["/home/user/.config/wraptool/config.yaml"] = "/real/config.yaml"

	got, err := m.ReadFile("/home/user/.config/wraptool/config.yaml")
	if err != nil {
		t.Fatalf("ReadFile through a symlink: %v", err)
	}
	if string(got) != "mcp: {}\n" {
		t.Errorf("ReadFile through a symlink: got %q, want the target's content", got)
	}
}

// TestMapEnvReadDirFollowsSymlinkedDirectory pins ReadDir's own directory
// lookup to the same link-following semantics, symmetric with ReadFile.
func TestMapEnvReadDirFollowsSymlinkedDirectory(t *testing.T) {
	m := NewMapEnv()
	m.Files["/real/dir"] = MapFile{Mode: fs.ModeDir | 0o755}
	m.Files["/real/dir/a.txt"] = MapFile{Mode: 0o644, Data: []byte("a")}
	m.Links["/link/dir"] = "/real/dir"

	entries, err := m.ReadDir("/link/dir")
	if err != nil {
		t.Fatalf("ReadDir through a symlinked directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Fatalf("ReadDir through a symlinked directory: got %+v, want [a.txt]", entries)
	}
	// Path must be rooted at the REQUESTED path (/link/dir), matching
	// osEnv.ReadDir — not at the link's target (/real/dir), which is what
	// collect() returns before re-rooting.
	if entries[0].Path != "/link/dir/a.txt" {
		t.Errorf("ReadDir through a symlinked directory: entries[0].Path = %q, want %q", entries[0].Path, "/link/dir/a.txt")
	}
	// ...and a path MapEnv hands out must be one MapEnv can take back, which
	// needs resolve to follow symlinked ancestors the way a real host does.
	if _, err := m.Stat(entries[0].Path); err != nil {
		t.Errorf("Stat of a path ReadDir returned: %v", err)
	}
	if _, err := m.ReadFile(entries[0].Path); err != nil {
		t.Errorf("ReadFile of a path ReadDir returned: %v", err)
	}
}

// TestMapEnvReadDirOnANonDirectoryIsNotErrNotExist pins the distinction a
// check relies on when it walks to the nearest existing ancestor: a real host
// answers ENOTDIR for a path that exists but is a file, never ErrNotExist, so
// a check branching on errors.Is(err, fs.ErrNotExist) to mean "absent" must
// not take that branch against the fake either.
func TestMapEnvReadDirOnANonDirectoryIsNotErrNotExist(t *testing.T) {
	m := NewMapEnv()
	m.Files["/a/f.txt"] = MapFile{Mode: 0o644}

	_, err := m.ReadDir("/a/f.txt")
	if err == nil {
		t.Fatal("ReadDir of a regular file: got nil error, want ENOTDIR-shaped error")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadDir of a regular file: %v satisfies fs.ErrNotExist; a real host returns ENOTDIR", err)
	}
}

func TestMapEnvGetenvGetuidNowProcessAlive(t *testing.T) {
	m := NewMapEnv()
	m.Vars["FOO"] = "bar"
	m.UID, m.GID = 42, 43
	m.Pids[123] = true

	if got := m.Getenv("FOO"); got != "bar" {
		t.Errorf("Getenv: got %q", got)
	}
	if got := m.Getenv("MISSING"); got != "" {
		t.Errorf("Getenv(missing): got %q, want empty", got)
	}
	if m.Getuid() != 42 || m.Getgid() != 43 {
		t.Error("Getuid/Getgid did not reflect UID/GID fields")
	}
	if !m.ProcessAlive(123) {
		t.Error("ProcessAlive(123) should be true")
	}
	if m.ProcessAlive(999) {
		t.Error("ProcessAlive(999) should default to false")
	}
	if !m.Now().Equal(m.Clock) {
		t.Error("Now() must return Clock")
	}
}
