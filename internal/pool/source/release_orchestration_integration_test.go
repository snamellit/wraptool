package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/pool"
)

// TestIntegration_RealReleaseSourceThroughRealOrchestrator drives the REAL
// release Source through pool.RefreshHarnessLocked and
// pool.UpdateHarnessLocked on a t.TempDir pool, against httptest servers
// (npm packument over plain HTTP, GitHub release over TLS because the lock
// accepts https URLs only) and no backend at all — the release counterpart
// of archive_orchestration_integration_test.go (omp-release-source.md §11).
func TestIntegration_RealReleaseSourceThroughRealOrchestrator(t *testing.T) {
	const version = "17.2.10"
	const harnessName = "omp"
	hostKey, err := pool.HostPlatformKey()
	if err != nil {
		t.Skipf("host platform has no pool key: %v", err)
	}
	otherKey := pool.PlatformLinuxARM64Glibc
	if hostKey == otherKey {
		otherKey = pool.PlatformLinuxAMD64Glibc
	}

	hostBody := []byte("#!/bin/sh\necho hello from fake omp\n")
	otherBody := []byte("other platform binary")
	sums := sha256HexOf(hostBody) + "  omp-host\n" + sha256HexOf(otherBody) + "  omp-other\n"
	var counter requestCounter
	gh := newSumsServer(t, version, sums, map[string][]byte{"omp-host": hostBody, "omp-other": otherBody}, &counter)

	reg := newPackumentServer(t, map[string]string{
		"created": "2026-01-01T00:00:00.000Z",
		version:   "2026-02-01T00:00:00.000Z",
	}, []string{version})
	defer reg.Close()

	cfg := ReleaseConfig{
		Repo: testReleaseRepo, VersionFeed: testReleaseFeed, Checksums: testReleaseSums,
		Assets: map[pool.PlatformKey]string{hostKey: "omp-host", otherKey: "omp-other"},
	}
	src, err := NewRelease(cfg, gh.URL, reg.URL)
	if err != nil {
		t.Fatalf("NewRelease: %v", err)
	}
	src.metadataClient = gh.Client()
	src.downloadClient = gh.Client()

	root := t.TempDir()
	paths := pool.PoolPaths{
		Manifest:     filepath.Join(root, "manifest.yaml"),
		Lock:         filepath.Join(root, "harness-lock.json"),
		Observations: filepath.Join(root, "observations.json"),
		Audit:        filepath.Join(root, "audit.jsonl"),
		AdvisoryLock: filepath.Join(root, "lock"),
		Cache:        filepath.Join(root, "cache", "sha256"),
		Staging:      filepath.Join(root, "staging"),
		Runtime:      filepath.Join(root, "runtime"),
	}
	if err := os.WriteFile(paths.Manifest, []byte("schema_version: 1\nharnesses:\n  omp: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	recipe := &pool.ReleaseRecipe{VersionFeed: cfg.VersionFeed, Assets: cfg.Assets, Checksums: cfg.Checksums}
	opts := pool.UpdateOptions{
		Harness: harnessName, Package: cfg.Repo, Source: pool.SourceRelease,
		BinName: "omp", Release: recipe, Now: now,
	}

	cands, err := pool.RefreshHarness(ctx, src, paths, pool.SourceRelease, cfg.Repo, now)
	if err != nil {
		t.Fatalf("RefreshHarness: %v", err)
	}
	if len(cands) != 1 || cands[0].PublishedAt.IsZero() {
		t.Fatalf("candidates = %+v, want one with a real PublishedAt", cands)
	}

	result, err := pool.UpdateHarness(ctx, src, nil, paths, opts)
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if !result.Changed || result.Harness.Version != version {
		t.Fatalf("UpdateResult = %+v, want Changed with version %s", result, version)
	}
	if result.RealizeErr != nil || result.RealizedGenerationID == "" {
		t.Fatalf("RealizeErr = %v, generation = %q", result.RealizeErr, result.RealizedGenerationID)
	}

	lockBytes, err := os.ReadFile(paths.Lock)
	if err != nil {
		t.Fatal(err)
	}
	lockDoc, err := pool.UnmarshalLock(lockBytes)
	if err != nil {
		t.Fatalf("lock failed to validate: %v", err)
	}
	lh := lockDoc.Harnesses[harnessName]
	if lh.Source != "release" || lh.Release == nil || lh.Release.Tag != "v"+version {
		t.Fatalf("lock entry = %+v", lh)
	}

	genDir := filepath.Join(paths.Runtime, "generations", result.RealizedGenerationID)
	got, err := os.ReadFile(filepath.Join(genDir, "bin", "omp"))
	if err != nil || string(got) != string(hostBody) {
		t.Fatalf("generation bin/omp = %q, %v; want served bytes", got, err)
	}
	if id, ok := pool.ActiveGeneration(paths, harnessName, hostKey); !ok || id != result.RealizedGenerationID {
		t.Errorf("ActiveGeneration = %q, %v", id, ok)
	}

	// Platform-scoped: the other platform's binary was never downloaded.
	for _, p := range counter.paths {
		if strings.HasSuffix(p, "omp-other") {
			t.Errorf("non-target platform asset requested: %s", p)
		}
	}

	// A second update downloads no artifact: the cache object is reused.
	downloads := func() int {
		n := 0
		for _, p := range counter.paths {
			if strings.HasSuffix(p, "omp-host") || strings.HasSuffix(p, "omp-other") {
				n++
			}
		}
		return n
	}
	before := downloads()
	if before != 1 {
		t.Fatalf("artifact downloads after first update = %d, want 1", before)
	}
	if _, err := pool.UpdateHarness(ctx, src, nil, paths, opts); err != nil {
		t.Fatalf("second UpdateHarness: %v", err)
	}
	if after := downloads(); after != before {
		t.Errorf("artifact downloads after second update = %d, want %d", after, before)
	}
}
