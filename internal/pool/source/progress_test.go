package source

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/pool"
)

func TestFormatBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{107274873, "102.3 MiB"},
		{3 << 30, "3.0 GiB"},
	} {
		if got := formatBytes(tc.n); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestDownloadProgress_ThrottlesByInterval drives the reporter with a fake
// clock: writes inside one progressInterval add no lines, and the first
// write past it reports the running byte count, percentage, and rate.
func TestDownloadProgress_ThrottlesByInterval(t *testing.T) {
	var out bytes.Buffer
	clock := time.Unix(1000, 0)
	p := startDownloadProgress(&out, "pkg", 4<<20, func() time.Time { return clock })
	out.Reset() // drop the start line; this test is about the progress lines

	p.Write(make([]byte, 1<<20))
	clock = clock.Add(progressInterval - time.Millisecond)
	p.Write(make([]byte, 1<<20))
	if out.Len() != 0 {
		t.Fatalf("reported before progressInterval elapsed: %q", out.String())
	}

	clock = clock.Add(time.Millisecond)
	p.Write(make([]byte, 0))
	want := "  pkg: 2.0 MiB / 4.0 MiB (50%, 409.6 KiB/s)\n"
	if got := out.String(); got != want {
		t.Errorf("progress line = %q, want %q", got, want)
	}
}

func TestDownloadProgress_NilOutIsSilent(t *testing.T) {
	if w := newDownloadProgress(nil, "pkg", 10); w != io.Discard {
		t.Errorf("newDownloadProgress(nil, ...) = %T, want io.Discard", w)
	}
}

// TestFetch_ReportsProgress checks SetProgress end to end: a download prints
// a start line naming the package (node_modules/ prefix stripped) and its
// Content-Length, and a cache hit prints nothing.
func TestFetch_ReportsProgress(t *testing.T) {
	content := []byte("tarball bytes for the npm package")
	srv, _ := countingServer(t, content)

	newLock := func() *pool.LockHarness {
		return &pool.LockHarness{NPMLock: &pool.NPMLock{Packages: map[string]pool.NPMLockPackage{
			"node_modules/@scope/pkg": {Resolved: srv.URL + "/pkg.tgz", Integrity: sriFor(content)},
		}}}
	}

	var out bytes.Buffer
	n := NewNPM("")
	n.SetProgress(&out)
	cacheDir := t.TempDir()
	lh := newLock()
	if err := n.Fetch(context.Background(), lh, cacheDir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if want := "fetching @scope/pkg (33 B)\n"; out.String() != want {
		t.Errorf("progress output = %q, want %q", out.String(), want)
	}

	out.Reset()
	if err := n.Fetch(context.Background(), lh, cacheDir); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("cache hit printed %q, want nothing", out.String())
	}
	if _, err := os.Stat(filepath.Join(cacheDir, lh.NPMLock.Packages["node_modules/@scope/pkg"].SHA256)); err != nil {
		t.Errorf("cached object missing: %v", err)
	}
}

func TestArchiveFetch_ReportsProgress(t *testing.T) {
	content := []byte("archive bytes")
	srv, _ := countingServer(t, content)

	lh := &pool.LockHarness{Package: "agy", Archive: &pool.LockArchive{Artifacts: map[pool.PlatformKey]pool.LockArchiveArtifact{
		pool.PlatformLinuxAMD64Glibc: {URL: srv.URL + "/agy.tar.gz", SHA512: sha512Hex(content)},
	}}}

	var out bytes.Buffer
	a := NewArchive("")
	a.SetProgress(&out)
	if err := a.Fetch(context.Background(), lh, t.TempDir()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.HasPrefix(out.String(), "fetching agy (") || !strings.HasSuffix(out.String(), ") (13 B)\n") {
		t.Errorf("progress output = %q, want a start line naming agy, its platform, and 13 B", out.String())
	}
}
