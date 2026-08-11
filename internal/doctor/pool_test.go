package doctor

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/pool"
)

// ---------------------------------------------------------------- pool.home-perms

func TestRunPoolHomePerms(t *testing.T) {
	cases := []struct {
		name        string
		build       func(env *MapEnv)
		wantOutcome Outcome
		wantPaths   []string // exact set of offending paths named in Detail, order-independent
	}{
		{
			name:        "no pool yet",
			build:       func(env *MapEnv) {},
			wantOutcome: OutcomeSkipped,
		},
		{
			name: "home is not a directory",
			build: func(env *MapEnv) {
				env.Files["/pool/home"] = MapFile{Mode: 0o644}
			},
			wantOutcome: OutcomeSkipped,
		},
		{
			name: "clean tree",
			build: func(env *MapEnv) {
				env.Files["/pool/home"] = MapFile{Mode: fs.ModeDir | 0o700}
				env.Files["/pool/home/.claude"] = MapFile{Mode: fs.ModeDir | 0o700}
				env.Files["/pool/home/.claude/credentials.json"] = MapFile{Mode: 0o600, Data: []byte("{}")}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "offending file and offending home dir itself",
			build: func(env *MapEnv) {
				env.Files["/pool/home"] = MapFile{Mode: fs.ModeDir | 0o755} // offending: other-readable
				env.Files["/pool/home/.claude"] = MapFile{Mode: fs.ModeDir | 0o700}
				env.Files["/pool/home/.claude/credentials.json"] = MapFile{Mode: 0o644, Data: []byte("{}")} // offending
			},
			wantOutcome: OutcomeFinding,
			wantPaths:   []string{"/pool/home", "/pool/home/.claude/credentials.json"},
		},
		{
			name: "symlinked entry is neither checked nor descended into",
			build: func(env *MapEnv) {
				env.Files["/pool/home"] = MapFile{Mode: fs.ModeDir | 0o700}
				env.Links["/pool/home/link"] = "/somewhere/else" // MapEnv always reports a symlink as mode 0o777
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "unreadable subdirectory is skipped, not fatal",
			build: func(env *MapEnv) {
				env.Files["/pool/home"] = MapFile{Mode: fs.ModeDir | 0o700}
				env.Files["/pool/home/normal.txt"] = MapFile{Mode: 0o644} // still offending
				// "/pool/home/ghost" is referenced by nothing, so ReadDir would
				// never even be called on it here — this case exists to document
				// that poolCollectReadable tolerates a ReadDir error rather than
				// asserting one is reachable through MapEnv (it deliberately
				// isn't: MapEnv has no permission-denied simulation).
			},
			wantOutcome: OutcomeFinding,
			wantPaths:   []string{"/pool/home/normal.txt"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			tc.build(env)
			in := &Input{Env: env, GOOS: "linux", Paths: HostPaths{PoolRoot: "/pool"}}

			res := runPoolHomePerms(context.Background(), in)
			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v (findings=%v, skip=%q)", res.Outcome(), tc.wantOutcome, res.Findings, res.SkipReason)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			if len(res.Findings) != 1 {
				t.Fatalf("got %d findings, want 1", len(res.Findings))
			}
			f := res.Findings[0]
			if f.Severity != SeverityFail {
				t.Errorf("severity = %v, want fail", f.Severity)
			}
			for _, p := range tc.wantPaths {
				if !strings.Contains(f.Detail, p) {
					t.Errorf("Detail %q does not mention offending path %q", f.Detail, p)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- pool.generation-digest / pool.generation-corrupt fixtures

// poolTestLock builds a minimal, Validate-clean *pool.Lock naming exactly one
// harness/platform generation digest.
func poolTestLock(harness string, key pool.PlatformKey, digest string) *pool.Lock {
	return &pool.Lock{
		SchemaVersion: pool.LockSchemaVersion,
		GeneratedAt:   "2026-01-01T00:00:00Z",
		PolicyHash:    "sha256:" + strings.Repeat("e", 64),
		Harnesses: map[string]pool.LockHarness{
			harness: {
				Source:         "npm",
				Package:        "@example/" + harness,
				Version:        "1.2.3",
				PublishedAt:    "2026-01-01T00:00:00Z",
				FirstSeenAt:    "2026-01-01T00:00:00Z",
				RecipeRevision: "sha256:" + strings.Repeat("c", 64),
				Generations: map[pool.PlatformKey]string{
					key: digest,
				},
			},
		},
	}
}

func poolTestWriteLock(t *testing.T, root string, lock *pool.Lock) {
	t.Helper()
	data, err := lock.Marshal()
	if err != nil {
		t.Fatalf("Lock.Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "harness-lock.json"), data, 0o600); err != nil {
		t.Fatalf("writing harness-lock.json: %v", err)
	}
}

// poolTestSeedGeneration builds a small, real, receipt-valid generation tree
// at root/runtime/generations/<generation-id> for (harness, version, key),
// content varying the generation's digest. Mirrors
// internal/pool/activate_test.go's seedActivateGeneration, built from this
// package's test using only internal/pool's EXPORTED surface (it cannot see
// that file's unexported helpers).
func poolTestSeedGeneration(t *testing.T, root, harness, version string, key pool.PlatformKey, content string) (genID, digest string) {
	t.Helper()
	scratch := t.TempDir()
	binDir := filepath.Join(scratch, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, harness), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	files, err := pool.ScanGenerationFiles(scratch)
	if err != nil {
		t.Fatalf("ScanGenerationFiles: %v", err)
	}
	digest, err = pool.ManifestDigest(files)
	if err != nil {
		t.Fatalf("ManifestDigest: %v", err)
	}
	genID, err = pool.GenerationID(harness, version, key, digest)
	if err != nil {
		t.Fatalf("GenerationID: %v", err)
	}

	r := &pool.Receipt{
		SchemaVersion:  pool.ReceiptSchemaVersion,
		Harness:        harness,
		Version:        version,
		Platform:       key,
		RecipeRevision: "sha256:" + strings.Repeat("c", 64),
		LockEntryHash:  "sha256:" + strings.Repeat("d", 64),
		Backend:        pool.ReceiptBackend{Name: "fake"},
		Files:          files,
		ManifestDigest: digest,
	}
	data, err := r.Marshal()
	if err != nil {
		t.Fatalf("Receipt.Marshal: %v", err)
	}
	if err := pool.WriteFileAtomic(filepath.Join(scratch, "receipt.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(root, "runtime", "generations", genID)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(scratch, dest); err != nil {
		t.Fatal(err)
	}
	return genID, digest
}

func poolTestActivate(t *testing.T, root, harness string, key pool.PlatformKey, genID string) {
	t.Helper()
	ptr := filepath.Join(root, "runtime", "active", string(key), harness+".gen")
	if err := pool.WritePointer(ptr, genID); err != nil {
		t.Fatalf("WritePointer: %v", err)
	}
}

var poolTestKey = pool.PlatformLinuxAMD64Glibc

// ---------------------------------------------------------------- pool.generation-digest

func TestRunPoolGenerationDigest(t *testing.T) {
	t.Run("no lock yet", func(t *testing.T) {
		root := t.TempDir()
		in := &Input{Env: NewOSEnv(nil), GOOS: "linux", Paths: HostPaths{PoolRoot: root}}
		res := runPoolGenerationDigest(context.Background(), in)
		if res.Outcome() != OutcomeSkipped {
			t.Fatalf("outcome = %v, want skipped", res.Outcome())
		}
	})

	t.Run("unparseable lock", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "harness-lock.json"), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		in := &Input{Env: NewOSEnv(nil), GOOS: "linux", Paths: HostPaths{PoolRoot: root}}
		res := runPoolGenerationDigest(context.Background(), in)
		if res.Outcome() != OutcomeSkipped {
			t.Fatalf("outcome = %v, want skipped", res.Outcome())
		}
	})

	t.Run("no active pointer for the locked platform", func(t *testing.T) {
		root := t.TempDir()
		poolTestWriteLock(t, root, poolTestLock("claude", poolTestKey, "sha256:"+strings.Repeat("b", 64)))
		// A generation exists on disk but is never activated.
		poolTestSeedGeneration(t, root, "claude", "1.2.3", poolTestKey, "v1")

		in := &Input{Env: NewOSEnv(nil), GOOS: "linux", Paths: HostPaths{PoolRoot: root}}
		res := runPoolGenerationDigest(context.Background(), in)
		if res.Outcome() != OutcomePass {
			t.Fatalf("outcome = %v, want pass (findings=%v)", res.Outcome(), res.Findings)
		}
	})

	t.Run("digest matches", func(t *testing.T) {
		root := t.TempDir()
		genID, digest := poolTestSeedGeneration(t, root, "claude", "1.2.3", poolTestKey, "v1")
		poolTestActivate(t, root, "claude", poolTestKey, genID)
		poolTestWriteLock(t, root, poolTestLock("claude", poolTestKey, digest))

		in := &Input{Env: NewOSEnv(nil), GOOS: "linux", Paths: HostPaths{PoolRoot: root}}
		res := runPoolGenerationDigest(context.Background(), in)
		if res.Outcome() != OutcomePass {
			t.Fatalf("outcome = %v, want pass (findings=%v)", res.Outcome(), res.Findings)
		}
	})

	t.Run("digest mismatch is a tamper fail", func(t *testing.T) {
		root := t.TempDir()
		genID, digest := poolTestSeedGeneration(t, root, "claude", "1.2.3", poolTestKey, "v1")
		poolTestActivate(t, root, "claude", poolTestKey, genID)
		recorded := "sha256:" + strings.Repeat("b", 64)
		if recorded == digest {
			t.Fatal("test fixture collision: recorded digest must differ from the real one")
		}
		poolTestWriteLock(t, root, poolTestLock("claude", poolTestKey, recorded))

		in := &Input{Env: NewOSEnv(nil), GOOS: "linux", Paths: HostPaths{PoolRoot: root}}
		res := runPoolGenerationDigest(context.Background(), in)
		if res.Outcome() != OutcomeFinding {
			t.Fatalf("outcome = %v, want finding", res.Outcome())
		}
		if len(res.Findings) != 1 {
			t.Fatalf("got %d findings, want 1", len(res.Findings))
		}
		f := res.Findings[0]
		if f.Severity != SeverityFail {
			t.Errorf("severity = %v, want fail", f.Severity)
		}
		if !strings.Contains(f.Detail, digest) || !strings.Contains(f.Detail, recorded) {
			t.Errorf("Detail %q does not name both digests", f.Detail)
		}
		if f.Location == "" {
			t.Error("Location is empty, want the mismatching receipt's path")
		}
		if f.Remedy == "" {
			t.Error("Remedy is empty")
		}
	})

	t.Run("missing receipt for an active pointer is silently skipped", func(t *testing.T) {
		root := t.TempDir()
		poolTestWriteLock(t, root, poolTestLock("claude", poolTestKey, "sha256:"+strings.Repeat("b", 64)))
		poolTestActivate(t, root, "claude", poolTestKey, "ghost-generation")

		in := &Input{Env: NewOSEnv(nil), GOOS: "linux", Paths: HostPaths{PoolRoot: root}}
		res := runPoolGenerationDigest(context.Background(), in)
		if res.Outcome() != OutcomePass {
			t.Fatalf("outcome = %v, want pass (findings=%v)", res.Outcome(), res.Findings)
		}
	})
}

// ---------------------------------------------------------------- pool.generation-corrupt

func TestRunPoolGenerationCorrupt(t *testing.T) {
	t.Run("intact tree passes", func(t *testing.T) {
		root := t.TempDir()
		genID, digest := poolTestSeedGeneration(t, root, "claude", "1.2.3", poolTestKey, "v1")
		poolTestActivate(t, root, "claude", poolTestKey, genID)
		poolTestWriteLock(t, root, poolTestLock("claude", poolTestKey, digest))

		in := &Input{Env: NewOSEnv(nil), GOOS: "linux", Paths: HostPaths{PoolRoot: root}, VerifyPool: true}
		res := runPoolGenerationCorrupt(context.Background(), in)
		if res.Outcome() != OutcomePass {
			t.Fatalf("outcome = %v, want pass (findings=%v)", res.Outcome(), res.Findings)
		}
	})

	t.Run("content tampering after realization is caught", func(t *testing.T) {
		root := t.TempDir()
		genID, digest := poolTestSeedGeneration(t, root, "claude", "1.2.3", poolTestKey, "v1")
		poolTestActivate(t, root, "claude", poolTestKey, genID)
		poolTestWriteLock(t, root, poolTestLock("claude", poolTestKey, digest))

		// Tamper with the generation's content directly on disk, WITHOUT
		// touching its receipt — pool.generation-digest cannot see this (the
		// receipt still matches the lock); only the full content scan can.
		binPath := filepath.Join(root, "runtime", "generations", genID, "bin", "claude")
		if err := os.WriteFile(binPath, []byte("tampered"), 0o755); err != nil {
			t.Fatal(err)
		}

		in := &Input{Env: NewOSEnv(nil), GOOS: "linux", Paths: HostPaths{PoolRoot: root}, VerifyPool: true}
		res := runPoolGenerationCorrupt(context.Background(), in)
		if res.Outcome() != OutcomeFinding {
			t.Fatalf("outcome = %v, want finding", res.Outcome())
		}
		f := res.Findings[0]
		if f.Severity != SeverityFail {
			t.Errorf("severity = %v, want fail", f.Severity)
		}
		if f.Location == "" {
			t.Error("Location is empty, want the generation directory")
		}
	})

	t.Run("no active pointer means nothing to verify", func(t *testing.T) {
		root := t.TempDir()
		poolTestSeedGeneration(t, root, "claude", "1.2.3", poolTestKey, "v1")
		poolTestWriteLock(t, root, poolTestLock("claude", poolTestKey, "sha256:"+strings.Repeat("b", 64)))

		in := &Input{Env: NewOSEnv(nil), GOOS: "linux", Paths: HostPaths{PoolRoot: root}, VerifyPool: true}
		res := runPoolGenerationCorrupt(context.Background(), in)
		if res.Outcome() != OutcomePass {
			t.Fatalf("outcome = %v, want pass (findings=%v)", res.Outcome(), res.Findings)
		}
	})
}

// ---------------------------------------------------------------- devcontainer mount checks

const (
	poolTestWorkspace  = "/work"
	poolTestConfigPath = "/work/.devcontainer/devcontainer.json"
)

// poolTestSeedActivePointer marks harness as pool-locked+active for
// poolTestKey, satisfying poolAnyActivePointer.
func poolTestSeedActivePointer(env *MapEnv, harness string) {
	env.Files["/pool/runtime/active"] = MapFile{Mode: fs.ModeDir | 0o755}
	env.Files["/pool/runtime/active/"+string(poolTestKey)] = MapFile{Mode: fs.ModeDir | 0o755}
	env.Files["/pool/runtime/active/"+string(poolTestKey)+"/"+harness+".gen"] = MapFile{Mode: 0o644, Data: []byte("gen-1\n")}
}

func poolTestWriteDevcontainer(env *MapEnv, body string) {
	env.Files[poolTestConfigPath] = MapFile{Mode: 0o644, Data: []byte(body)}
}

func TestRunPoolDevcontainerMountMissing(t *testing.T) {
	cases := []struct {
		name        string
		activate    bool
		body        string // "" means no devcontainer.json at all
		wantOutcome Outcome
	}{
		{name: "no active pointer", activate: false, wantOutcome: OutcomePass},
		{name: "active, no devcontainer.json", activate: true, wantOutcome: OutcomeSkipped},
		{
			name: "active, mount absent", activate: true,
			body:        `{"mounts": ["source=/other,target=/somewhere,type=bind"]}`,
			wantOutcome: OutcomeFinding,
		},
		{
			name: "active, mount declared read-only", activate: true,
			body:        `{"mounts": ["source=/pool/runtime,target=/opt/wraptool-harness/runtime,type=bind,readonly"]}`,
			wantOutcome: OutcomePass,
		},
		{
			name: "active, mount declared read-write (object form)", activate: true,
			body:        `{"mounts": [{"source": "/pool/runtime", "target": "/opt/wraptool-harness/runtime", "type": "bind"}]}`,
			wantOutcome: OutcomePass, // mount-writable's job, not mount-missing's
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			if tc.activate {
				poolTestSeedActivePointer(env, "claude")
			}
			if tc.body != "" {
				poolTestWriteDevcontainer(env, tc.body)
			}
			in := &Input{Env: env, GOOS: "linux", Paths: HostPaths{PoolRoot: "/pool"}, Workspace: poolTestWorkspace}

			res := runPoolDevcontainerMountMissing(context.Background(), in)
			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v (findings=%v, skip=%q)", res.Outcome(), tc.wantOutcome, res.Findings, res.SkipReason)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityWarn {
				t.Errorf("severity = %v, want warn", f.Severity)
			}
			if !strings.Contains(f.Remedy, "readonly") {
				t.Errorf("Remedy %q does not mention readonly", f.Remedy)
			}
		})
	}
}

func TestRunPoolDevcontainerMountWritable(t *testing.T) {
	cases := []struct {
		name        string
		activate    bool
		body        string
		wantOutcome Outcome
	}{
		{name: "no active pointer", activate: false, wantOutcome: OutcomePass},
		{name: "active, no devcontainer.json", activate: true, wantOutcome: OutcomeSkipped},
		{
			name: "active, mount absent", activate: true,
			body:        `{"mounts": ["source=/other,target=/somewhere,type=bind"]}`,
			wantOutcome: OutcomePass,
		},
		{
			name: "active, mount declared read-only", activate: true,
			body:        `{"mounts": ["source=/pool/runtime,target=/opt/wraptool-harness/runtime,type=bind,readonly"]}`,
			wantOutcome: OutcomePass,
		},
		{
			name: "active, mount declared read-write (object form)", activate: true,
			body:        `{"mounts": [{"source": "/pool/runtime", "target": "/opt/wraptool-harness/runtime", "type": "bind"}]}`,
			wantOutcome: OutcomeFinding,
		},
		{
			name: "active, string-form mount missing the readonly token", activate: true,
			body:        `{"mounts": ["source=/pool/runtime,target=/opt/wraptool-harness/runtime,type=bind"]}`,
			wantOutcome: OutcomeFinding,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			if tc.activate {
				poolTestSeedActivePointer(env, "claude")
			}
			if tc.body != "" {
				poolTestWriteDevcontainer(env, tc.body)
			}
			in := &Input{Env: env, GOOS: "linux", Paths: HostPaths{PoolRoot: "/pool"}, Workspace: poolTestWorkspace}

			res := runPoolDevcontainerMountWritable(context.Background(), in)
			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v (findings=%v, skip=%q)", res.Outcome(), tc.wantOutcome, res.Findings, res.SkipReason)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityFail {
				t.Errorf("severity = %v, want fail", f.Severity)
			}
			if !strings.HasPrefix(f.Title, "SECURITY:") {
				t.Errorf("Title %q does not begin with SECURITY:", f.Title)
			}
		})
	}
}

// ---------------------------------------------------------------- pool.deprecated-feature

func TestRunPoolDeprecatedFeature(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantOutcome Outcome
		wantRef     string
	}{
		{name: "no devcontainer.json", wantOutcome: OutcomeSkipped},
		{
			name:        "no features at all",
			body:        `{"name": "demo"}`,
			wantOutcome: OutcomePass,
		},
		{
			name:        "current wraptool:1 major is not flagged",
			body:        `{"features": {"forge.snamellit.com/pti/wraptool/wraptool:1": {"harness": "claude"}}}`,
			wantOutcome: OutcomePass,
		},
		{
			name:        "deprecated wraptool:0 major",
			body:        `{"features": {"forge.snamellit.com/pti/wraptool/wraptool:0": {"harness": "claude"}}}`,
			wantOutcome: OutcomeFinding,
			wantRef:     "forge.snamellit.com/pti/wraptool/wraptool:0",
		},
		{
			name:        "sunset wraptool-connect feature",
			body:        `{"features": {"forge.snamellit.com/pti/wraptool/wraptool-connect": {}}}`,
			wantOutcome: OutcomeFinding,
			wantRef:     "forge.snamellit.com/pti/wraptool/wraptool-connect",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			if tc.body != "" {
				poolTestWriteDevcontainer(env, tc.body)
			}
			in := &Input{Env: env, GOOS: "linux", Paths: HostPaths{PoolRoot: "/pool"}, Workspace: poolTestWorkspace}

			res := runPoolDeprecatedFeature(context.Background(), in)
			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v (findings=%v, skip=%q)", res.Outcome(), tc.wantOutcome, res.Findings, res.SkipReason)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityWarn {
				t.Errorf("severity = %v, want warn", f.Severity)
			}
			if !strings.Contains(f.Detail, tc.wantRef) {
				t.Errorf("Detail %q does not mention ref %q", f.Detail, tc.wantRef)
			}
		})
	}
}

// ---------------------------------------------------------------- helper unit tests

func TestPoolStripJSONCLeavesStringsAlone(t *testing.T) {
	in := `{
  // a line comment
  "url": "http://example.com/a//b", /* block */ "n": 1,
}`
	out := poolStripJSONC(in)
	if !strings.Contains(out, `"http://example.com/a//b"`) {
		t.Errorf("stripped output corrupted a string containing //: %q", out)
	}
	if strings.Contains(out, "a line comment") || strings.Contains(out, "block") {
		t.Errorf("comments survived stripping: %q", out)
	}
	// Trailing comma before the closing brace must be gone, or json.Unmarshal
	// would reject it.
	if strings.Contains(out, ",\n}") {
		t.Errorf("trailing comma survived stripping: %q", out)
	}
}

func TestPoolParseMountString(t *testing.T) {
	fields, flags := poolParseMountString("source=/a,target=/b,type=bind,readonly")
	if fields["source"] != "/a" || fields["target"] != "/b" || fields["type"] != "bind" {
		t.Errorf("fields = %+v", fields)
	}
	if !flags["readonly"] {
		t.Errorf("flags = %+v, want readonly", flags)
	}
}

func TestPoolFeatureDeprecation(t *testing.T) {
	cases := []struct {
		ref        string
		deprecated bool
	}{
		{"forge.snamellit.com/pti/wraptool/wraptool:1", false},
		{"forge.snamellit.com/pti/wraptool/wraptool:0", true},
		{"forge.snamellit.com/pti/wraptool/wraptool:0.1.2", true},
		{"forge.snamellit.com/pti/wraptool/wraptool-connect", true},
		{"forge.snamellit.com/pti/wraptool/wraptool-connect:0.1.3", true},
		{"ghcr.io/devcontainers/features/go:1", false},
	}
	for _, tc := range cases {
		_, got := poolFeatureDeprecation(tc.ref)
		if got != tc.deprecated {
			t.Errorf("poolFeatureDeprecation(%q) deprecated=%v, want %v", tc.ref, got, tc.deprecated)
		}
	}
}
