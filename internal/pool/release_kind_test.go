package pool

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for the release-binary source kind and lock shape
// (design/harness/omp-release-source.md §3, §11).

const releaseTestRepo = "can1357/oh-my-pi"
const releaseTestFeed = "@oh-my-pi/pi-coding-agent"

func sampleReleaseAssets() map[PlatformKey]string {
	return map[PlatformKey]string{
		PlatformLinuxAMD64Glibc: "omp-linux-x64",
		PlatformLinuxARM64Glibc: "omp-linux-arm64",
	}
}

func sampleReleaseArtifacts() map[PlatformKey]LockReleaseArtifact {
	return map[PlatformKey]LockReleaseArtifact{
		PlatformLinuxAMD64Glibc: {
			URL:    "https://github.com/can1357/oh-my-pi/releases/download/v18.6.1/omp-linux-x64",
			SHA256: strings.Repeat("a", 64),
		},
		PlatformLinuxARM64Glibc: {
			URL:    "https://github.com/can1357/oh-my-pi/releases/download/v18.6.1/omp-linux-arm64",
			SHA256: strings.Repeat("b", 64),
		},
	}
}

func sampleLockWithRelease() *Lock {
	l := sampleLock()
	l.Harnesses["omp"] = LockHarness{
		Source:         releaseSourceName,
		Package:        releaseTestRepo,
		Version:        "18.6.1",
		PublishedAt:    "2026-09-01T12:00:00Z",
		FirstSeenAt:    "2026-09-01T12:10:00Z",
		RecipeRevision: "sha256:release-recipe",
		Release: &LockRelease{
			Repo:      releaseTestRepo,
			Tag:       "v18.6.1",
			Artifacts: sampleReleaseArtifacts(),
		},
	}
	return l
}

func TestLockValidate_ReleaseTable(t *testing.T) {
	setSHA := func(sha string) func(r *LockRelease) {
		return func(r *LockRelease) {
			a := r.Artifacts[PlatformLinuxAMD64Glibc]
			a.SHA256 = sha
			r.Artifacts[PlatformLinuxAMD64Glibc] = a
		}
	}
	tests := []struct {
		name    string
		mutate  func(r *LockRelease)
		wantErr string
	}{
		{"well-formed", func(r *LockRelease) {}, ""},
		{"no artifacts is shape-valid", func(r *LockRelease) { r.Artifacts = nil }, ""},
		{"empty repo", func(r *LockRelease) { r.Repo = "" }, "repo"},
		{"empty tag", func(r *LockRelease) { r.Tag = "" }, "tag"},
		{"unknown platform key", func(r *LockRelease) {
			r.Artifacts[PlatformKey("plan9-mips")] = LockReleaseArtifact{URL: "https://x/y", SHA256: strings.Repeat("a", 64)}
		}, "plan9-mips"},
		{"http url", func(r *LockRelease) {
			a := r.Artifacts[PlatformLinuxAMD64Glibc]
			a.URL = "http://github.com/x"
			r.Artifacts[PlatformLinuxAMD64Glibc] = a
		}, "https"},
		{"empty sha256", setSHA(""), "sha256"},
		{"uppercase sha256", setSHA(strings.Repeat("A", 64)), "sha256"},
		{"short sha256", setSHA(strings.Repeat("a", 63)), "sha256"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := sampleLockWithRelease()
			h := l.Harnesses["omp"]
			tt.mutate(h.Release)
			l.Harnesses["omp"] = h
			err := l.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLockRelease_RoundTripAndDeterministic(t *testing.T) {
	l := sampleLockWithRelease()
	data, err := l.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	again, err := sampleLockWithRelease().Marshal()
	if err != nil {
		t.Fatalf("Marshal (2): %v", err)
	}
	if !bytes.Equal(data, again) {
		t.Fatalf("Marshal not deterministic:\n%s\n%s", data, again)
	}
	got, err := UnmarshalLock(data)
	if err != nil {
		t.Fatalf("UnmarshalLock: %v", err)
	}
	r := got.Harnesses["omp"].Release
	if r == nil || r.Repo != releaseTestRepo || r.Tag != "v18.6.1" {
		t.Fatalf("round-tripped Release = %+v", r)
	}
	for key, want := range sampleReleaseArtifacts() {
		if r.Artifacts[key] != want {
			t.Errorf("artifact %s = %+v, want %+v", key, r.Artifacts[key], want)
		}
	}
	data2, err := got.Marshal()
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if !bytes.Equal(data, data2) {
		t.Fatalf("round-trip not byte-stable:\n%s\n%s", data, data2)
	}
}

func TestLockRelease_AbsentFromNonReleaseLocks(t *testing.T) {
	data, err := sampleLock().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"release"`)) {
		t.Errorf("a lock without a Release payload must not serialize the field:\n%s", data)
	}
}

func TestLockHarnessHash_CoversRelease(t *testing.T) {
	base := sampleLockWithRelease().Harnesses["omp"]
	baseHash, err := base.Hash()
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(h *LockHarness){
		"repo": func(h *LockHarness) { h.Release.Repo = "other/repo" },
		"tag":  func(h *LockHarness) { h.Release.Tag = "v18.6.2" },
		"url": func(h *LockHarness) {
			a := h.Release.Artifacts[PlatformLinuxAMD64Glibc]
			a.URL += "x"
			h.Release.Artifacts[PlatformLinuxAMD64Glibc] = a
		},
		"sha256": func(h *LockHarness) {
			a := h.Release.Artifacts[PlatformLinuxAMD64Glibc]
			a.SHA256 = strings.Repeat("c", 64)
			h.Release.Artifacts[PlatformLinuxAMD64Glibc] = a
		},
		"dropped payload": func(h *LockHarness) { h.Release = nil },
	}
	for name, mutate := range mutations {
		h := sampleLockWithRelease().Harnesses["omp"]
		mutate(&h)
		got, err := h.Hash()
		if err != nil {
			t.Fatal(err)
		}
		if got == baseHash {
			t.Errorf("Hash unchanged after mutating release %s", name)
		}
	}
	same := sampleLockWithRelease().Harnesses["omp"]
	if got, _ := same.Hash(); got != baseHash {
		t.Errorf("Hash differs for equal values: %s vs %s", got, baseHash)
	}
}

func TestGCReleaseArtifactCacheDigestIsRetained(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	if err := os.MkdirAll(paths.Cache, 0o755); err != nil {
		t.Fatal(err)
	}
	locked := strings.Repeat("e", 64)
	orphan := strings.Repeat("f", 64)
	for _, d := range []string{locked, orphan} {
		if err := os.WriteFile(filepath.Join(paths.Cache, d), []byte("omp binary"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gcWriteLock(t, paths, map[string]LockHarness{
		"omp": {
			Source: releaseSourceName, Package: releaseTestRepo, Version: "18.6.1",
			PublishedAt: FormatTimestamp(fixedNow), FirstSeenAt: FormatTimestamp(fixedNow),
			RecipeRevision: "sha256:" + strings.Repeat("1", 64),
			Release: &LockRelease{
				Repo: releaseTestRepo, Tag: "v18.6.1",
				Artifacts: map[PlatformKey]LockReleaseArtifact{
					gcTestKey: {URL: "https://example.invalid/omp", SHA256: locked},
				},
			},
		},
	})
	report, err := GCPoolLocked(paths, GCOptions{})
	if err != nil {
		t.Fatalf("GCPoolLocked: %v", err)
	}
	if report.RemovedCacheObjects != 1 || report.RetainedCacheObjects != 1 {
		t.Errorf("report = %+v, want 1 removed (orphan) / 1 retained (locked)", report)
	}
	if _, err := os.Stat(filepath.Join(paths.Cache, locked)); err != nil {
		t.Errorf("locked release artifact's cache object was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.Cache, orphan)); !os.IsNotExist(err) {
		t.Errorf("orphan cache object should be removed, stat err = %v", err)
	}
}

// goldenReleaseRecipeRevision pins releaseRecipeRevision for the sample
// inputs. If this fails, the recipe formula changed: bump
// releaseRecipeFormatVersion deliberately and re-pin.
const goldenReleaseRecipeRevision = "sha256:a43801a672dec07b5e0ffa92e2e1c1e09ddd1d070d2e08ed2737df6e0fd3660c"

func TestReleaseRecipeRevisionGoldenAndProperties(t *testing.T) {
	got, err := releaseRecipeRevision(releaseTestRepo, releaseTestFeed, sampleReleaseAssets(), "SHA256SUMS.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenReleaseRecipeRevision {
		t.Errorf("releaseRecipeRevision = %q, want golden %q", got, goldenReleaseRecipeRevision)
	}
	for range 20 {
		again, _ := releaseRecipeRevision(releaseTestRepo, releaseTestFeed, sampleReleaseAssets(), "SHA256SUMS.txt")
		if again != got {
			t.Fatalf("not stable across map iteration order: %s vs %s", again, got)
		}
	}

	changedAssets := sampleReleaseAssets()
	changedAssets[PlatformLinuxAMD64Glibc] = "omp-linux-x64-v2"
	variants := map[string]func() (string, error){
		"repo": func() (string, error) {
			return releaseRecipeRevision("a/b", releaseTestFeed, sampleReleaseAssets(), "SHA256SUMS.txt")
		},
		"feed": func() (string, error) {
			return releaseRecipeRevision(releaseTestRepo, "other-feed", sampleReleaseAssets(), "SHA256SUMS.txt")
		},
		"assets": func() (string, error) {
			return releaseRecipeRevision(releaseTestRepo, releaseTestFeed, changedAssets, "SHA256SUMS.txt")
		},
		"checksum": func() (string, error) {
			return releaseRecipeRevision(releaseTestRepo, releaseTestFeed, sampleReleaseAssets(), "sums.txt")
		},
	}
	for name, f := range variants {
		v, err := f()
		if err != nil {
			t.Fatal(err)
		}
		if v == got {
			t.Errorf("recipe revision unchanged after changing %s", name)
		}
	}
	for _, other := range []func() (string, error){
		func() (string, error) { return npmRecipeRevision(releaseTestRepo, "", "") },
		func() (string, error) { return archiveRecipeRevision(releaseTestRepo) },
	} {
		o, err := other()
		if err != nil {
			t.Fatal(err)
		}
		if o == got {
			t.Errorf("release recipe collides with another source's: %s", o)
		}
	}
}

func TestRecipeRevisionForSource_Release(t *testing.T) {
	want, err := releaseRecipeRevision(releaseTestRepo, releaseTestFeed, sampleReleaseAssets(), "SHA256SUMS.txt")
	if err != nil {
		t.Fatal(err)
	}
	rr := &ReleaseRecipe{VersionFeed: releaseTestFeed, Assets: sampleReleaseAssets(), Checksums: "SHA256SUMS.txt"}
	got, err := recipeRevisionForSource(SourceRelease, releaseTestRepo, "img", "22.1.0", rr)
	if err != nil || got != want {
		t.Errorf("recipeRevisionForSource(SourceRelease) = %q, %v, want %q", got, err, want)
	}
	if _, err := recipeRevisionForSource(SourceRelease, releaseTestRepo, "", "", nil); err == nil {
		t.Error("SourceRelease with nil release recipe inputs must error")
	}
	// Existing sources ignore the release inputs entirely.
	npmWith, _ := recipeRevisionForSource(SourceNPM, "p", "img", "22.1.0", rr)
	npmWithout, _ := recipeRevisionForSource(SourceNPM, "p", "img", "22.1.0", nil)
	if npmWith != npmWithout {
		t.Error("npm recipe must not depend on the release inputs")
	}
}

func TestCandidatesForPackage_ReleaseNamespace(t *testing.T) {
	published := FormatTimestamp(fixedNow.Add(-48 * time.Hour))
	seen := FormatTimestamp(fixedNow.Add(-24 * time.Hour))
	obs := Observations{}
	key := ObservationKey(string(SourceRelease), releaseTestRepo, "18.6.1")
	if key != "release/can1357/oh-my-pi/18.6.1" {
		t.Fatalf("ObservationKey = %q", key)
	}
	if err := obs.Observe(key, seen, published, false, false, false); err != nil {
		t.Fatal(err)
	}
	// The same package under another namespace must not leak in.
	if err := obs.Observe(ObservationKey(npmSourceName, releaseTestRepo, "9.9.9"), seen, published, false, false, false); err != nil {
		t.Fatal(err)
	}
	got, err := candidatesForPackage(obs, SourceRelease, releaseTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Version != "18.6.1" || got[0].PublishedAtSynthesized {
		t.Fatalf("candidates = %+v, want exactly release 18.6.1 with a real published_at", got)
	}
	if npm, _ := candidatesForPackage(obs, SourceNPM, releaseTestRepo); len(npm) != 1 || npm[0].Version != "9.9.9" {
		t.Errorf("npm namespace candidates = %+v", npm)
	}
}
