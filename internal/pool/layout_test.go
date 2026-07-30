package pool

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLayoutAccessorsAreRootPrefixed asserts every layout accessor returns a
// path under Root() — the invariant §6.3 depends on ("Host paths are built
// with path/filepath, rooted at paths.DataDir()"). A stray accessor that
// forgot to join against Root() would otherwise silently escape the pool
// tree.
func TestLayoutAccessorsAreRootPrefixed(t *testing.T) {
	root := Root()
	cases := map[string]string{
		"ManifestFile":             ManifestFile(),
		"LockFile":                 LockFile(),
		"ObservationsFile":         ObservationsFile(),
		"AuditFile":                AuditFile(),
		"CacheDir":                 CacheDir(),
		"CacheSHA256Dir":           CacheSHA256Dir(),
		"CacheObjectPath":          CacheObjectPath("deadbeef"),
		"RuntimeDir":               RuntimeDir(),
		"RuntimeBinDir":            RuntimeBinDir(),
		"RuntimeBinFile":           RuntimeBinFile("claude"),
		"RuntimeActiveDir":         RuntimeActiveDir(),
		"RuntimeActivePlatformDir": RuntimeActivePlatformDir(PlatformLinuxAMD64Glibc),
		"PointerFile":              PointerFile(PlatformLinuxAMD64Glibc, "claude"),
		"RuntimeGenerationsDir":    RuntimeGenerationsDir(),
		"RuntimeGenerationDir":     RuntimeGenerationDir("claude-2.1.220-linux-amd64-glibc-a1b2c3d4"),
		"HomeDir":                  HomeDir(),
		"StagingDir":               StagingDir(),
		"StagingTxnDir":            StagingTxnDir("abc123"),
		"AdvisoryLockFile":         AdvisoryLockFile(),
	}
	for name, path := range cases {
		if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
			t.Errorf("%s() = %q, want a path equal to or rooted under %q", name, path, root)
		}
	}
}

// TestLayoutTreeShape pins the concrete tree shape from §5 so an accidental
// rename of a path segment (e.g. "harness-lock.json" -> "lock.json") is
// caught here rather than only when a later package's tests fail obscurely.
func TestLayoutTreeShape(t *testing.T) {
	root := Root()
	want := map[string]string{
		"ManifestFile":          filepath.Join(root, "manifest.yaml"),
		"LockFile":              filepath.Join(root, "harness-lock.json"),
		"ObservationsFile":      filepath.Join(root, "observations.json"),
		"AuditFile":             filepath.Join(root, "audit.jsonl"),
		"CacheDir":              filepath.Join(root, "cache"),
		"CacheSHA256Dir":        filepath.Join(root, "cache", "sha256"),
		"RuntimeDir":            filepath.Join(root, "runtime"),
		"RuntimeBinDir":         filepath.Join(root, "runtime", "bin"),
		"RuntimeActiveDir":      filepath.Join(root, "runtime", "active"),
		"RuntimeGenerationsDir": filepath.Join(root, "runtime", "generations"),
		"HomeDir":               filepath.Join(root, "home"),
		"StagingDir":            filepath.Join(root, "staging"),
		"AdvisoryLockFile":      filepath.Join(root, "lock"),
	}
	got := map[string]string{
		"ManifestFile":          ManifestFile(),
		"LockFile":              LockFile(),
		"ObservationsFile":      ObservationsFile(),
		"AuditFile":             AuditFile(),
		"CacheDir":              CacheDir(),
		"CacheSHA256Dir":        CacheSHA256Dir(),
		"RuntimeDir":            RuntimeDir(),
		"RuntimeBinDir":         RuntimeBinDir(),
		"RuntimeActiveDir":      RuntimeActiveDir(),
		"RuntimeGenerationsDir": RuntimeGenerationsDir(),
		"HomeDir":               HomeDir(),
		"StagingDir":            StagingDir(),
		"AdvisoryLockFile":      AdvisoryLockFile(),
	}
	for name, wantPath := range want {
		if got[name] != wantPath {
			t.Errorf("%s() = %q, want %q", name, got[name], wantPath)
		}
	}

	// Platform-scoped and parameterized accessors.
	if got := RuntimeActivePlatformDir(PlatformLinuxARM64Musl); got != filepath.Join(root, "runtime", "active", "linux-arm64-musl") {
		t.Errorf("RuntimeActivePlatformDir() = %q, want the platform-key-named subdirectory", got)
	}
	if got := PointerFile(PlatformLinuxAMD64Glibc, "claude"); got != filepath.Join(root, "runtime", "active", "linux-amd64-glibc", "claude.gen") {
		t.Errorf("PointerFile() = %q, want <active>/<platform>/<harness>.gen", got)
	}
	if got := RuntimeGenerationDir("gen-1"); got != filepath.Join(root, "runtime", "generations", "gen-1") {
		t.Errorf("RuntimeGenerationDir() = %q, want <generations>/<id>", got)
	}
	if got := StagingTxnDir("txn-1"); got != filepath.Join(root, "staging", "txn-txn-1") {
		t.Errorf("StagingTxnDir() = %q, want staging/txn-<id>", got)
	}
	if got := CacheObjectPath("abc"); got != filepath.Join(root, "cache", "sha256", "abc") {
		t.Errorf("CacheObjectPath() = %q, want cache/sha256/<digest>", got)
	}
}
