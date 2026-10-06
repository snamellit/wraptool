package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
)

const (
	testReleaseRepo = "can1357/oh-my-pi"
	testReleaseFeed = "@oh-my-pi/pi-coding-agent"
	testReleaseSums = "SHA256SUMS.txt"
)

func sha256HexOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func testReleaseConfig() ReleaseConfig {
	return ReleaseConfig{
		Repo:        testReleaseRepo,
		VersionFeed: testReleaseFeed,
		Checksums:   testReleaseSums,
		Assets: map[pool.PlatformKey]string{
			pool.PlatformLinuxAMD64Glibc: "omp-linux-x64",
			pool.PlatformLinuxARM64Glibc: "omp-linux-arm64",
		},
	}
}

// requestCounter records every request path a test server sees.
type requestCounter struct {
	mu    sync.Mutex
	paths []string
}

func (c *requestCounter) add(p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths = append(c.paths, p)
}

func (c *requestCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.paths)
}

// newReleaseUnderTest builds a Release whose GitHub host is the given TLS
// test server (Resolve locks https URLs only, so the clients trust its
// certificate) and whose registry is registryURL.
func newReleaseUnderTest(t *testing.T, gh *httptest.Server, registryURL string) *Release {
	t.Helper()
	r, err := NewRelease(testReleaseConfig(), gh.URL, registryURL)
	if err != nil {
		t.Fatalf("NewRelease: %v", err)
	}
	r.metadataClient = gh.Client()
	r.metadataClient.Timeout = metadataTimeout
	r.downloadClient = gh.Client()
	r.downloadClient.Timeout = downloadTimeout
	return r
}

// newSumsServer serves sums at /<repo>/releases/download/v<version>/SHA256SUMS.txt
// and assets (name -> bytes) next to it; everything else 404s.
func newSumsServer(t *testing.T, version, sums string, assets map[string][]byte, counter *requestCounter) *httptest.Server {
	t.Helper()
	prefix := "/" + testReleaseRepo + "/releases/download/v" + version + "/"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if counter != nil {
			counter.add(r.URL.Path)
		}
		name, ok := strings.CutPrefix(r.URL.Path, prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if name == testReleaseSums {
			if sums == "" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(sums))
			return
		}
		if body, ok := assets[name]; ok {
			w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNewRelease_Validation(t *testing.T) {
	bad := map[string]func(*ReleaseConfig){
		"empty repo":          func(c *ReleaseConfig) { c.Repo = "" },
		"repo without slash":  func(c *ReleaseConfig) { c.Repo = "oh-my-pi" },
		"empty feed":          func(c *ReleaseConfig) { c.VersionFeed = "" },
		"empty checksums":     func(c *ReleaseConfig) { c.Checksums = "" },
		"no assets":           func(c *ReleaseConfig) { c.Assets = nil },
		"bad platform key":    func(c *ReleaseConfig) { c.Assets = map[pool.PlatformKey]string{"darwin-arm64": "x"} },
		"empty asset name":    func(c *ReleaseConfig) { c.Assets = map[pool.PlatformKey]string{pool.PlatformLinuxAMD64Glibc: ""} },
		"repo with extra dir": func(c *ReleaseConfig) { c.Repo = "a/b/c" },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			cfg := testReleaseConfig()
			mutate(&cfg)
			if _, err := NewRelease(cfg, "", ""); err == nil {
				t.Fatal("NewRelease succeeded, want error")
			}
		})
	}
	if _, err := NewRelease(testReleaseConfig(), "", ""); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestRelease_Refresh(t *testing.T) {
	reg := newPackumentServer(t, map[string]string{
		"created":     "2026-01-01T00:00:00.000Z",
		"17.2.10":     "2026-03-04T05:06:07.000Z",
		"18.0.0-rc.1": "2026-03-05T00:00:00.000Z",
	}, []string{"17.2.10", "18.0.0-rc.1"})
	defer reg.Close()
	gh := newSumsServer(t, "x", "", nil, nil)
	r := newReleaseUnderTest(t, gh, reg.URL)

	cands, err := r.Refresh(context.Background(), testReleaseRepo)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	byVersion := map[string]pool.Candidate{}
	for _, c := range cands {
		byVersion[c.Version] = c
	}
	c, ok := byVersion["17.2.10"]
	if !ok || c.PublishedAt.IsZero() || c.Prerelease {
		t.Errorf("17.2.10 = %+v, ok=%v, want real PublishedAt and not prerelease", c, ok)
	}
	if got := c.PublishedAt.UTC().Format("2006-01-02T15:04:05"); got != "2026-03-04T05:06:07" {
		t.Errorf("PublishedAt = %s, want the packument's time", got)
	}
	if rc, ok := byVersion["18.0.0-rc.1"]; !ok || !rc.Prerelease {
		t.Errorf("18.0.0-rc.1 = %+v, want flagged Prerelease", rc)
	}

	if _, err := r.Refresh(context.Background(), "other/repo"); err == nil {
		t.Error("Refresh with pkg != Repo succeeded, want error")
	}
}

func goodSums(ampDigest, armDigest string) string {
	return ampDigest + "  omp-linux-x64\n" +
		armDigest + "  omp-linux-arm64\n" +
		strings.Repeat("a", 64) + "  omp-darwin-arm64\n" +
		strings.Repeat("b", 64) + "  omp-windows-x64.exe\n" +
		strings.Repeat("c", 64) + "  extension.zip\n"
}

func TestRelease_Resolve_Valid(t *testing.T) {
	amd, arm := strings.Repeat("1", 64), strings.Repeat("2", 64)
	gh := newSumsServer(t, "17.2.10", goodSums(amd, arm), nil, nil)
	r := newReleaseUnderTest(t, gh, "")

	lh, err := r.Resolve(context.Background(), nil, testReleaseRepo, "17.2.10", "", "", "sha256:recipe")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lh.Source != "release" || lh.Package != testReleaseRepo || lh.Version != "17.2.10" || lh.RecipeRevision != "sha256:recipe" {
		t.Errorf("LockHarness header = %+v", lh)
	}
	if lh.Release == nil || lh.Release.Repo != testReleaseRepo || lh.Release.Tag != "v17.2.10" {
		t.Fatalf("Release = %+v, want repo and tag v17.2.10", lh.Release)
	}
	wantBase := gh.URL + "/" + testReleaseRepo + "/releases/download/v17.2.10/"
	got := lh.Release.Artifacts
	if len(got) != 2 {
		t.Fatalf("artifacts = %+v, want exactly the two mapped platforms (unmapped names ignored)", got)
	}
	if a := got[pool.PlatformLinuxAMD64Glibc]; a.URL != wantBase+"omp-linux-x64" || a.SHA256 != amd {
		t.Errorf("amd64 artifact = %+v", a)
	}
	if a := got[pool.PlatformLinuxARM64Glibc]; a.URL != wantBase+"omp-linux-arm64" || a.SHA256 != arm {
		t.Errorf("arm64 artifact = %+v", a)
	}
}

func TestRelease_Resolve_ChecksumParsing(t *testing.T) {
	d1, d2 := strings.Repeat("1", 64), strings.Repeat("2", 64)
	cases := map[string]struct {
		sums    string
		wantErr string // "" = success
	}{
		"identical duplicate ok":     {d1 + "  omp-linux-x64\n" + d1 + "  omp-linux-x64\n", ""},
		"blank lines ok":             {"\n" + d1 + "  omp-linux-x64\n\n", ""},
		"conflicting duplicate":      {d1 + "  omp-linux-x64\n" + d2 + "  omp-linux-x64\n", "twice with different digests"},
		"single space separator":     {d1 + " omp-linux-x64\n", "malformed"},
		"uppercase hex":              {strings.ToUpper(d1[:63]+"a") + "  omp-linux-x64\n", "malformed"},
		"short digest":               {d1[:63] + "  omp-linux-x64\n", "malformed"},
		"binary-mode star separator": {d1 + " *omp-linux-x64\n", "malformed"},
		"missing name":               {d1 + "  \n", "malformed"},
		"garbage line":               {"hello\n", "malformed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gh := newSumsServer(t, "1.0.0", tc.sums, nil, nil)
			r := newReleaseUnderTest(t, gh, "")
			_, err := r.Resolve(context.Background(), nil, testReleaseRepo, "1.0.0", "", "", "rr")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Resolve err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestRelease_Resolve_MissingPlatformOmitted(t *testing.T) {
	d := strings.Repeat("1", 64)
	gh := newSumsServer(t, "1.0.0", d+"  omp-linux-x64\n", nil, nil)
	r := newReleaseUnderTest(t, gh, "")
	lh, err := r.Resolve(context.Background(), nil, testReleaseRepo, "1.0.0", "", "", "rr")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, ok := lh.Release.Artifacts[pool.PlatformLinuxARM64Glibc]; ok || len(lh.Release.Artifacts) != 1 {
		t.Errorf("artifacts = %+v, want only linux-amd64-glibc", lh.Release.Artifacts)
	}
}

func TestRelease_Resolve_NotPublishedErrors(t *testing.T) {
	d := strings.Repeat("1", 64)
	cases := map[string]string{
		"404 on checksum file":  "",
		"zero mapped platforms": d + "  omp-darwin-arm64\n",
	}
	for name, sums := range cases {
		t.Run(name, func(t *testing.T) {
			gh := newSumsServer(t, "1.0.0", sums, nil, nil)
			r := newReleaseUnderTest(t, gh, "")
			_, err := r.Resolve(context.Background(), nil, testReleaseRepo, "1.0.0", "", "", "rr")
			if err == nil {
				t.Fatal("Resolve succeeded, want error")
			}
			for _, want := range []string{"may not be published yet", "retry later", "predates"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
		})
	}
}

func TestRelease_Resolve_NeverTouchesBackend(t *testing.T) {
	d := strings.Repeat("1", 64)
	gh := newSumsServer(t, "1.0.0", d+"  omp-linux-x64\n", nil, nil)
	r := newReleaseUnderTest(t, gh, "")
	var b backend.Backend // nil: any dereference would panic
	if _, err := r.Resolve(context.Background(), b, testReleaseRepo, "1.0.0", "ignored", "ignored", "rr"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func releaseLock(url string, key pool.PlatformKey, digest string) *pool.LockHarness {
	return &pool.LockHarness{
		Source: "release", Package: testReleaseRepo, Version: "1.0.0", RecipeRevision: "rr",
		Release: &pool.LockRelease{
			Repo: testReleaseRepo, Tag: "v1.0.0",
			Artifacts: map[pool.PlatformKey]pool.LockReleaseArtifact{key: {URL: url, SHA256: digest}},
		},
	}
}

func TestRelease_Fetch(t *testing.T) {
	body := []byte("#!/bin/sh\necho omp\n")
	digest := sha256HexOf(body)
	ctx := context.Background()

	t.Run("admits verified download and reuses it with zero requests", func(t *testing.T) {
		var c requestCounter
		gh := newSumsServer(t, "1.0.0", "", map[string][]byte{"omp-linux-x64": body}, &c)
		r := newReleaseUnderTest(t, gh, "")
		url := gh.URL + "/" + testReleaseRepo + "/releases/download/v1.0.0/omp-linux-x64"
		h := releaseLock(url, pool.PlatformLinuxAMD64Glibc, digest)
		cache := t.TempDir()
		targets := []pool.PlatformKey{pool.PlatformLinuxAMD64Glibc}

		if err := r.Fetch(ctx, h, cache, targets); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(cache, digest))
		if err != nil || string(got) != string(body) {
			t.Fatalf("cache object = %q, %v", got, err)
		}
		if c.count() != 1 {
			t.Fatalf("requests after first Fetch = %d, want 1", c.count())
		}
		if err := r.Fetch(ctx, h, cache, targets); err != nil {
			t.Fatalf("second Fetch: %v", err)
		}
		if c.count() != 1 {
			t.Errorf("requests after cached Fetch = %d, want still 1", c.count())
		}
	})

	t.Run("digest mismatch rejected and not admitted", func(t *testing.T) {
		gh := newSumsServer(t, "1.0.0", "", map[string][]byte{"omp-linux-x64": []byte("tampered")}, nil)
		r := newReleaseUnderTest(t, gh, "")
		url := gh.URL + "/" + testReleaseRepo + "/releases/download/v1.0.0/omp-linux-x64"
		h := releaseLock(url, pool.PlatformLinuxAMD64Glibc, digest)
		cache := t.TempDir()
		err := r.Fetch(ctx, h, cache, []pool.PlatformKey{pool.PlatformLinuxAMD64Glibc})
		if err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
			t.Fatalf("Fetch err = %v, want integrity mismatch", err)
		}
		entries, _ := os.ReadDir(cache)
		if len(entries) != 0 {
			t.Errorf("cache dir not empty after rejected download: %v", entries)
		}
	})

	t.Run("corrupt cache object is replaced", func(t *testing.T) {
		gh := newSumsServer(t, "1.0.0", "", map[string][]byte{"omp-linux-x64": body}, nil)
		r := newReleaseUnderTest(t, gh, "")
		url := gh.URL + "/" + testReleaseRepo + "/releases/download/v1.0.0/omp-linux-x64"
		h := releaseLock(url, pool.PlatformLinuxAMD64Glibc, digest)
		cache := t.TempDir()
		if err := os.WriteFile(filepath.Join(cache, digest), []byte("corrupt"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := r.Fetch(ctx, h, cache, []pool.PlatformKey{pool.PlatformLinuxAMD64Glibc}); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if got, _ := os.ReadFile(filepath.Join(cache, digest)); string(got) != string(body) {
			t.Errorf("cache object = %q, want the verified download", got)
		}
	})

	t.Run("only target platform requested", func(t *testing.T) {
		var c requestCounter
		gh := newSumsServer(t, "1.0.0", "", map[string][]byte{"omp-linux-x64": body, "omp-linux-arm64": []byte("arm")}, &c)
		r := newReleaseUnderTest(t, gh, "")
		base := gh.URL + "/" + testReleaseRepo + "/releases/download/v1.0.0/"
		h := releaseLock(base+"omp-linux-x64", pool.PlatformLinuxAMD64Glibc, digest)
		h.Release.Artifacts[pool.PlatformLinuxARM64Glibc] = pool.LockReleaseArtifact{URL: base + "omp-linux-arm64", SHA256: sha256HexOf([]byte("arm"))}
		if err := r.Fetch(ctx, h, t.TempDir(), []pool.PlatformKey{pool.PlatformLinuxAMD64Glibc}); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if len(c.paths) != 1 || !strings.HasSuffix(c.paths[0], "omp-linux-x64") {
			t.Errorf("requested paths = %v, want only omp-linux-x64", c.paths)
		}
	})

	t.Run("empty targets rejected", func(t *testing.T) {
		gh := newSumsServer(t, "1.0.0", "", nil, nil)
		r := newReleaseUnderTest(t, gh, "")
		h := releaseLock(gh.URL+"/x", pool.PlatformLinuxAMD64Glibc, digest)
		if err := r.Fetch(ctx, h, t.TempDir(), nil); err == nil {
			t.Error("Fetch with no targets succeeded, want error")
		}
	})

	t.Run("lock without release rejected", func(t *testing.T) {
		gh := newSumsServer(t, "1.0.0", "", nil, nil)
		r := newReleaseUnderTest(t, gh, "")
		err := r.Fetch(ctx, &pool.LockHarness{Package: "p"}, t.TempDir(), []pool.PlatformKey{pool.PlatformLinuxAMD64Glibc})
		if err == nil {
			t.Error("Fetch of non-release lock succeeded, want error")
		}
	})
}

func TestRelease_Realize(t *testing.T) {
	body := []byte("#!/bin/sh\necho omp\n")
	digest := sha256HexOf(body)
	gh := newSumsServer(t, "1.0.0", "", nil, nil)
	r := newReleaseUnderTest(t, gh, "")
	h := releaseLock("https://example.invalid/omp", pool.PlatformLinuxAMD64Glibc, digest)

	t.Run("places bin with mode 0755 and a valid receipt", func(t *testing.T) {
		cache := t.TempDir()
		if err := os.WriteFile(filepath.Join(cache, digest), body, 0o644); err != nil {
			t.Fatal(err)
		}
		staging := filepath.Join(t.TempDir(), "gen")
		rcpt, err := r.Realize(context.Background(), nil, "omp", "omp", h, pool.PlatformLinuxAMD64Glibc, cache, staging)
		if err != nil {
			t.Fatalf("Realize: %v", err)
		}
		fi, err := os.Stat(filepath.Join(staging, "bin", "omp"))
		if err != nil {
			t.Fatalf("bin/omp: %v", err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("mode = %v, want 0755", fi.Mode().Perm())
		}
		if got, _ := os.ReadFile(filepath.Join(staging, "bin", "omp")); string(got) != string(body) {
			t.Errorf("content = %q", got)
		}
		if err := rcpt.Validate(); err != nil {
			t.Errorf("receipt invalid: %v", err)
		}
		if rcpt.Backend.Name != "none" || rcpt.Platform != pool.PlatformLinuxAMD64Glibc || rcpt.Version != "1.0.0" {
			t.Errorf("receipt = %+v", rcpt)
		}
	})

	t.Run("key not in artifacts", func(t *testing.T) {
		_, err := r.Realize(context.Background(), nil, "omp", "omp", h, pool.PlatformLinuxARM64Glibc, t.TempDir(), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "no artifact recorded") {
			t.Errorf("err = %v, want no-artifact error", err)
		}
	})

	t.Run("missing cache object names the sync remedy", func(t *testing.T) {
		_, err := r.Realize(context.Background(), nil, "omp", "omp", h, pool.PlatformLinuxAMD64Glibc, t.TempDir(), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "harness sync") {
			t.Errorf("err = %v, want the harness sync remedy", err)
		}
	})

	t.Run("binName must be a bare name", func(t *testing.T) {
		_, err := r.Realize(context.Background(), nil, "omp", "../omp", h, pool.PlatformLinuxAMD64Glibc, t.TempDir(), t.TempDir())
		if err == nil {
			t.Error("path-ish binName accepted")
		}
	})
}
