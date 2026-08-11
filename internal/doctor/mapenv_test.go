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
