package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/pool/backend"
)

// scopedArchiveSource wraps fakeSource to behave like an archive Source for
// platform-scoped Fetch: Resolve returns a four-platform archive lock with
// empty sha256 addresses, and Fetch "downloads" (counts, then fills the
// address of) only the targeted artifacts whose address is still empty.
type scopedArchiveSource struct {
	*fakeSource
	downloads int
}

func scopedAddr(key PlatformKey) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func scopedArchiveLock(version string) LockHarness {
	artifacts := map[PlatformKey]LockArchiveArtifact{}
	for _, key := range allPlatformKeys() {
		artifacts[key] = LockArchiveArtifact{
			URL:    "https://example.invalid/agy-" + string(key) + ".tar.gz",
			SHA512: strings.Repeat("a", 128),
		}
	}
	return LockHarness{
		Source: archiveSourceName, Package: "agy", Version: version,
		PublishedAt: FormatTimestamp(fixedNow), FirstSeenAt: FormatTimestamp(fixedNow),
		RecipeRevision: "sha256:" + strings.Repeat("1", 64),
		Archive:        &LockArchive{Artifacts: artifacts},
	}
}

func (s *scopedArchiveSource) Resolve(_ context.Context, _ backend.Backend, pkg, version, _, _, recipe string) (LockHarness, error) {
	s.resolveCalls++
	lh := scopedArchiveLock(version)
	lh.Package = pkg
	lh.RecipeRevision = recipe
	return lh, nil
}

func (s *scopedArchiveSource) Fetch(_ context.Context, h *LockHarness, _ string, targets []PlatformKey) error {
	s.fetchCalls++
	for key, a := range h.Archive.Artifacts {
		if !containsKey(targets, key) || a.SHA256 != "" {
			continue
		}
		s.downloads++
		a.SHA256 = scopedAddr(key)
		h.Archive.Artifacts[key] = a
	}
	return nil
}

func containsKey(keys []PlatformKey, k PlatformKey) bool {
	for _, c := range keys {
		if c == k {
			return true
		}
	}
	return false
}

func TestUpdateFetchesOnlyHostKeyAndLockValidatesAndGCWorks(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, `
schema_version: 1
harnesses:
  agy:
    constraint: ">=1 <2"
`)
	fs := &fakeSource{candidates: map[string][]Candidate{"agy": {{Version: "1.1.10"}}}}
	src := &scopedArchiveSource{fakeSource: fs}
	if _, err := RefreshHarnessLocked(ctxT(t), src, paths, SourceArchive, "agy", fixedNow); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := UpdateHarnessLocked(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "agy", Package: "agy", Source: SourceArchive, BinName: "agy", Now: fixedNow,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	host, err := HostPlatformKey()
	if err != nil {
		t.Skip(err)
	}
	if src.downloads != 1 {
		t.Errorf("downloads = %d, want 1 (the host key only)", src.downloads)
	}
	lock := loadLockFromDisk(t, paths)
	for key, a := range lock.Harnesses["agy"].Archive.Artifacts {
		if key == host && a.SHA256 == "" {
			t.Errorf("host key %s: sha256 not recorded", key)
		}
		if key != host && a.SHA256 != "" {
			t.Errorf("other key %s: sha256 = %q, want empty", key, a.SHA256)
		}
	}
	if _, err := GCPoolLocked(paths, GCOptions{}); err != nil {
		t.Fatalf("GCPoolLocked with empty addresses: %v", err)
	}
}

func loadLockFromDisk(t *testing.T, paths PoolPaths) *Lock {
	t.Helper()
	l, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		t.Fatalf("loading lock: %v", err)
	}
	if err := l.Validate(); err != nil {
		t.Fatalf("lock does not validate: %v", err)
	}
	return l
}

func seedScopedArchiveLock(t *testing.T, paths PoolPaths) {
	t.Helper()
	gcWriteLock(t, paths, map[string]LockHarness{"agy": scopedArchiveLock("1.1.10")})
}

// normalized returns h's JSON with the given key's address and the
// generations map cleared, so two entries can be compared on every OTHER field.
func normalized(t *testing.T, h LockHarness, key PlatformKey) string {
	t.Helper()
	c := h
	c.Generations = nil
	arts := map[PlatformKey]LockArchiveArtifact{}
	for k, a := range h.Archive.Artifacts {
		if k == key {
			a.SHA256 = ""
		}
		arts[k] = a
	}
	c.Archive = &LockArchive{Artifacts: arts}
	data, err := (&Lock{SchemaVersion: LockSchemaVersion, GeneratedAt: FormatTimestamp(fixedNow), PolicyHash: "sha256:" + strings.Repeat("0", 64),
		Harnesses: map[string]LockHarness{"agy": c}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSyncPersistsAddressSecondSyncMakesNoDownloads(t *testing.T) {
	paths := testPaths(t)
	seedScopedArchiveLock(t, paths)
	before := loadLockFromDisk(t, paths).Harnesses["agy"]
	src := &scopedArchiveSource{fakeSource: &fakeSource{}}
	key := PlatformLinuxARM64Glibc

	if _, err := SyncHarnessLocked(ctxT(t), src, fakeBackend{}, paths, "agy", "agy", key); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if src.downloads != 1 {
		t.Fatalf("first sync downloads = %d, want 1", src.downloads)
	}
	after := loadLockFromDisk(t, paths).Harnesses["agy"]
	if got := after.Archive.Artifacts[key].SHA256; got != scopedAddr(key) {
		t.Fatalf("persisted address = %q, want %q", got, scopedAddr(key))
	}
	if normalized(t, before, key) != normalized(t, after, key) {
		t.Errorf("lock entry changed beyond the address and generations")
	}

	if _, err := SyncHarnessLocked(ctxT(t), src, fakeBackend{}, paths, "agy", "agy", key); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if src.downloads != 1 {
		t.Errorf("downloads after second sync = %d, want still 1", src.downloads)
	}

	other := PlatformLinuxAMD64Glibc
	if _, err := SyncHarnessLocked(ctxT(t), src, fakeBackend{}, paths, "agy", "agy", other); err != nil {
		t.Fatalf("sync other key: %v", err)
	}
	final := loadLockFromDisk(t, paths).Harnesses["agy"]
	if src.downloads != 2 || final.Archive.Artifacts[other].SHA256 != scopedAddr(other) {
		t.Errorf("other key: downloads = %d, address = %q; want 2 and %q", src.downloads, final.Archive.Artifacts[other].SHA256, scopedAddr(other))
	}
	if final.Archive.Artifacts[key].SHA256 != scopedAddr(key) {
		t.Errorf("first key's address was lost")
	}
}

func TestPersistCacheAddresses(t *testing.T) {
	key := PlatformLinuxAMD64Glibc
	setup := func(t *testing.T) (PoolPaths, LockHarness) {
		paths := testPaths(t)
		seedScopedArchiveLock(t, paths)
		return paths, scopedArchiveLock("1.1.10")
	}
	setAddr := func(lh *LockHarness, k PlatformKey, addr string) {
		a := lh.Archive.Artifacts[k]
		a.SHA256 = addr
		lh.Archive.Artifacts[k] = a
	}

	t.Run("fills an empty address", func(t *testing.T) {
		paths, lh := setup(t)
		setAddr(&lh, key, scopedAddr(key))
		if err := persistCacheAddresses(paths, "agy", &lh); err != nil {
			t.Fatal(err)
		}
		if got := loadLockFromDisk(t, paths).Harnesses["agy"].Archive.Artifacts[key].SHA256; got != scopedAddr(key) {
			t.Errorf("address = %q", got)
		}
	})
	t.Run("refuses version mismatch", func(t *testing.T) {
		paths, lh := setup(t)
		lh.Version = "1.1.11"
		setAddr(&lh, key, scopedAddr(key))
		if err := persistCacheAddresses(paths, "agy", &lh); err == nil || !strings.Contains(err.Error(), "version") {
			t.Fatalf("err = %v, want version refusal", err)
		}
		if got := loadLockFromDisk(t, paths).Harnesses["agy"].Archive.Artifacts[key].SHA256; got != "" {
			t.Errorf("address written despite refusal: %q", got)
		}
	})
	t.Run("refuses recipe mismatch", func(t *testing.T) {
		paths, lh := setup(t)
		lh.RecipeRevision = "sha256:" + strings.Repeat("2", 64)
		setAddr(&lh, key, scopedAddr(key))
		if err := persistCacheAddresses(paths, "agy", &lh); err == nil || !strings.Contains(err.Error(), "recipe_revision") {
			t.Fatalf("err = %v, want recipe refusal", err)
		}
	})
	t.Run("refuses overwriting a set address and never clears one", func(t *testing.T) {
		paths := testPaths(t)
		disk := scopedArchiveLock("1.1.10")
		a := disk.Archive.Artifacts[key]
		a.SHA256 = scopedAddr(key)
		disk.Archive.Artifacts[key] = a
		gcWriteLock(t, paths, map[string]LockHarness{"agy": disk})

		lh := scopedArchiveLock("1.1.10")
		setAddr(&lh, key, strings.Repeat("f", 64))
		if err := persistCacheAddresses(paths, "agy", &lh); err == nil || !strings.Contains(err.Error(), "mismatch") {
			t.Fatalf("err = %v, want mismatch error", err)
		}
		// An empty address in the fetched copy leaves the set one alone.
		lh = scopedArchiveLock("1.1.10")
		if err := persistCacheAddresses(paths, "agy", &lh); err != nil {
			t.Fatal(err)
		}
		if got := loadLockFromDisk(t, paths).Harnesses["agy"].Archive.Artifacts[key].SHA256; got != scopedAddr(key) {
			t.Errorf("address = %q, want it untouched", got)
		}
	})
	t.Run("fills runtime and npm addresses too", func(t *testing.T) {
		paths := testPaths(t)
		base := LockHarness{
			Source: npmSourceName, Package: "p", Version: "1.0.0",
			PublishedAt: FormatTimestamp(fixedNow.Add(-time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-time.Hour)),
			RecipeRevision: "sha256:" + strings.Repeat("1", 64),
		}
		disk := base
		disk.NPMLock = &NPMLock{LockfileVersion: 3, Packages: map[string]NPMLockPackage{"node_modules/p": {Version: "1.0.0", Resolved: "https://r.invalid/p.tgz", Integrity: "sha512-x"}}}
		disk.Runtime = &LockRuntime{Name: "node", Version: "24.0.0", Artifacts: map[PlatformKey]LockRuntimeArtifact{key: {URL: "https://r.invalid/n.xz"}}}
		gcWriteLock(t, paths, map[string]LockHarness{"claude": disk})

		fetched := base
		fetched.NPMLock = &NPMLock{LockfileVersion: 3, Packages: map[string]NPMLockPackage{"node_modules/p": {Version: "1.0.0", Resolved: "https://r.invalid/p.tgz", Integrity: "sha512-x", SHA256: strings.Repeat("b", 64)}}}
		fetched.Runtime = &LockRuntime{Name: "node", Version: "24.0.0", Artifacts: map[PlatformKey]LockRuntimeArtifact{key: {URL: "https://r.invalid/n.xz", SHA256: strings.Repeat("c", 64)}}}
		if err := persistCacheAddresses(paths, "claude", &fetched); err != nil {
			t.Fatal(err)
		}
		got := loadLockFromDisk(t, paths).Harnesses["claude"]
		if got.NPMLock.Packages["node_modules/p"].SHA256 != strings.Repeat("b", 64) || got.Runtime.Artifacts[key].SHA256 != strings.Repeat("c", 64) {
			t.Errorf("addresses not persisted: %+v %+v", got.NPMLock.Packages, got.Runtime.Artifacts)
		}
		if !reflect.DeepEqual(got.Version, "1.0.0") {
			t.Errorf("version changed")
		}
	})
}
