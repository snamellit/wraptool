package doctor

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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
		wantCount   int      // exact offending count, when wantOutcome == OutcomeFinding
		wantPaths   []string // paths that MUST be named in Detail
		wantAbsent  []string // paths that must NOT be named in Detail
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
				env.Files["/pool/home/.claude/.credentials.json"] = MapFile{Mode: 0o600, Data: []byte("{}")}
			},
			wantOutcome: OutcomePass,
		},
		{
			// BLOCKER 1: cmd/guixcontainer.go's ensureHarnessPool creates
			// home/ with os.MkdirAll(home, 0o755) on every host — that alone
			// must never be a finding, or this check fails on every pool
			// wraptool itself created (SeverityFail's own doc comment: "never
			// fires on a configuration working as designed").
			name: "home/ itself is 0755 (as ensureHarnessPool creates it), no credential files exposed",
			build: func(env *MapEnv) {
				env.Files["/pool/home"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.claude"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.claude/.credentials.json"] = MapFile{Mode: 0o600, Data: []byte("{}")}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "a world-readable credential file is offending, the home dir itself is not",
			build: func(env *MapEnv) {
				env.Files["/pool/home"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.claude"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.claude/.credentials.json"] = MapFile{Mode: 0o644, Data: []byte("{}")}
			},
			wantOutcome: OutcomeFinding,
			wantCount:   1,
			wantPaths:   []string{"/pool/home/.claude/.credentials.json"},
			wantAbsent:  []string{"/pool/home (mode", "/pool/home/.claude (mode"},
		},
		{
			name: "a world-readable non-credential file under a scoped dir is not flagged",
			build: func(env *MapEnv) {
				env.Files["/pool/home"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.claude"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.claude/settings.json"] = MapFile{Mode: 0o644, Data: []byte("{}")}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "cache files under .npm/.cache are never flagged, even when world-readable",
			build: func(env *MapEnv) {
				env.Files["/pool/home"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.npm"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.npm/_cacache"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.npm/_cacache/index-v5"] = MapFile{Mode: 0o644, Data: []byte("x")}
				env.Files["/pool/home/.cache"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.cache/some-file"] = MapFile{Mode: 0o644, Data: []byte("x")}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "a token/session-named file anywhere in the tree is flagged",
			build: func(env *MapEnv) {
				env.Files["/pool/home"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.opencode"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.opencode/session.json"] = MapFile{Mode: 0o644, Data: []byte("{}")}
			},
			wantOutcome: OutcomeFinding,
			wantCount:   1,
			wantPaths:   []string{"/pool/home/.opencode/session.json"},
		},
		{
			name: ".config/*/auth* is flagged regardless of nesting depth beneath it",
			build: func(env *MapEnv) {
				env.Files["/pool/home"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.config"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.config/opencode"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/pool/home/.config/opencode/auth"] = MapFile{Mode: fs.ModeDir | 0o700} // itself not offending
				env.Files["/pool/home/.config/opencode/auth/store.json"] = MapFile{Mode: 0o644, Data: []byte("{}")}
			},
			wantOutcome: OutcomeFinding,
			wantCount:   1,
			wantPaths:   []string{"/pool/home/.config/opencode/auth/store.json"},
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
				env.Files["/pool/home/.credentials.json"] = MapFile{Mode: 0o644} // still offending
				// "/pool/home/ghost" is referenced by nothing, so ReadDir would
				// never even be called on it here — this case exists to document
				// that poolCollectReadable tolerates a ReadDir error rather than
				// asserting one is reachable through MapEnv (it deliberately
				// isn't: MapEnv has no permission-denied simulation).
			},
			wantOutcome: OutcomeFinding,
			wantCount:   1,
			wantPaths:   []string{"/pool/home/.credentials.json"},
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
			if f.Severity != SeverityWarn {
				t.Errorf("severity = %v, want warn", f.Severity)
			}
			if tc.wantCount > 0 && !strings.Contains(f.Detail, strconv.Itoa(tc.wantCount)) {
				t.Errorf("Detail %q does not name the count %d", f.Detail, tc.wantCount)
			}
			if strings.Contains(f.Remedy, "chmod -R go-rwx /pool/home") {
				t.Errorf("Remedy %q recommends a recursive chmod on the whole home/ tree", f.Remedy)
			}
			for _, p := range tc.wantPaths {
				if !strings.Contains(f.Detail, p) {
					t.Errorf("Detail %q does not mention offending path %q", f.Detail, p)
				}
			}
			for _, p := range tc.wantAbsent {
				if strings.Contains(f.Detail, p) {
					t.Errorf("Detail %q unexpectedly mentions %q", f.Detail, p)
				}
			}
		})
	}
}

// TestRunPoolHomePermsCapsEnumeration is BLOCKER 3: a real pool's home/ tree
// can hold thousands of offending entries, and joining every one of them
// into a single Finding.Detail line makes the count — the actually
// actionable fact — impossible to find. The listed entries are capped at
// poolMaxOffendingListed, with the remainder folded into "and N more", and
// the full count is always reported.
func TestRunPoolHomePermsCapsEnumeration(t *testing.T) {
	env := NewMapEnv()
	env.Files["/pool/home"] = MapFile{Mode: fs.ModeDir | 0o755}
	const total = poolMaxOffendingListed + 7
	for i := range total {
		d := filepath.Join("/pool/home", "h"+string(rune('a'+i)))
		env.Files[d] = MapFile{Mode: fs.ModeDir | 0o755}
		env.Files[filepath.Join(d, ".credentials.json")] = MapFile{Mode: 0o644, Data: []byte("{}")}
	}
	in := &Input{Env: env, GOOS: "linux", Paths: HostPaths{PoolRoot: "/pool"}}

	res := runPoolHomePerms(context.Background(), in)
	if res.Outcome() != OutcomeFinding {
		t.Fatalf("outcome = %v, want finding", res.Outcome())
	}
	f := res.Findings[0]
	if !strings.Contains(f.Detail, strconv.Itoa(total)) {
		t.Errorf("Detail %q does not name the full count %d", f.Detail, total)
	}
	more := total - poolMaxOffendingListed
	if !strings.Contains(f.Detail, "and "+strconv.Itoa(more)+" more") {
		t.Errorf("Detail %q does not fold the remainder into \"and %d more\"", f.Detail, more)
	}
}

// TestPoolIsCredentialPath pins the scoped predicate BLOCKER 1 introduced:
// harness auth state, never every readable file under home/.
func TestPoolIsCredentialPath(t *testing.T) {
	cases := []struct {
		rel  string
		want bool
	}{
		{".credentials.json", true},
		{".claude/.credentials.json", true},
		{".opencode/session.json", true},
		{".opencode/token", true},
		{"some/deep/path/my-credentials-backup", true},
		{".config/opencode/auth.json", true},
		{".config/opencode/auth/store.json", true},
		{".config/opencode/auth/nested/deep/store.json", true},
		{".claude/settings.json", false},
		{".npm/_cacache/index-v5/aa", false},
		{".cache/some-file", false},
		{".config/opencode/config.json", false},
		{"skills/some-skill/SKILL.md", false},
		// Observed on a real pool: every one of these was flagged by the
		// original substring rule, and not one is a credential.
		{".claude/plugins/marketplaces/x/plugins/learning-output-style/hooks-handlers/session-start.sh", false},
		{".claude/plugins/marketplaces/x/plugins/security-guidance/hooks/session_state.py", false},
		{".claude/plugins/marketplaces/x/plugins/session-report", false},
		{".config/go/telemetry/local/upload.token", false},
		// ...while the genuine articles still match.
		{".gemini/antigravity-cli/antigravity-oauth-token", true},
		{".ssh/id_ed25519", true},
	}
	for _, tc := range cases {
		if got := poolIsCredentialPath(tc.rel); got != tc.want {
			t.Errorf("poolIsCredentialPath(%q) = %v, want %v", tc.rel, got, tc.want)
		}
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

	// BLOCKER 2: after a genuine `wraptool harness rollback`, the active
	// generation's digest legitimately differs from the lock's CURRENTLY
	// recorded one, but the generation's own content still matches its own
	// receipt (its directory name still encodes exactly this digest) — the
	// architect-ruled discriminator. That must be reported as info about a
	// rollback, never a fail whose remedy would silently undo it.
	t.Run("digest differs from the lock but matches the generation's own name: a rollback, reported as info", func(t *testing.T) {
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
		if f.Severity != SeverityInfo {
			t.Errorf("severity = %v, want info", f.Severity)
		}
		if !strings.Contains(f.Detail, digest) || !strings.Contains(f.Detail, recorded) {
			t.Errorf("Detail %q does not name both digests", f.Detail)
		}
		if !strings.Contains(f.Detail, "rollback") {
			t.Errorf("Detail %q does not say this looks like a rollback", f.Detail)
		}
		if f.Remedy != "" {
			t.Errorf("Remedy = %q, want empty — a rollback info finding must not tell the operator to undo it", f.Remedy)
		}
	})

	// BLOCKER 2's genuine-tampering side: receipt.json rewritten to describe
	// DIFFERENT, self-consistent content (a valid digest for its own Files,
	// so it passes UnmarshalReceipt's Validate) WITHOUT renaming the
	// generation directory to match. The directory name still encodes the
	// ORIGINAL digest, so recomputing the generation id from the rewritten
	// receipt no longer matches it — this is what the directory-name
	// cross-check exists to catch.
	t.Run("receipt rewritten without a matching rename is a tamper fail", func(t *testing.T) {
		root := t.TempDir()
		genID, digest := poolTestSeedGeneration(t, root, "claude", "1.2.3", poolTestKey, "v1")
		poolTestActivate(t, root, "claude", poolTestKey, genID)
		poolTestWriteLock(t, root, poolTestLock("claude", poolTestKey, digest))

		fakeFiles := []pool.ReceiptFile{{Path: "bin/other", Executable: true, Size: 4, SHA256: strings.Repeat("1", 64)}}
		fakeDigest, err := pool.ManifestDigest(fakeFiles)
		if err != nil {
			t.Fatalf("pool.ManifestDigest: %v", err)
		}
		if fakeDigest == digest {
			t.Fatal("test fixture collision: fabricated digest must differ from the real one")
		}
		tampered := &pool.Receipt{
			SchemaVersion:  pool.ReceiptSchemaVersion,
			Harness:        "claude",
			Version:        "1.2.3",
			Platform:       poolTestKey,
			RecipeRevision: "sha256:" + strings.Repeat("c", 64),
			LockEntryHash:  "sha256:" + strings.Repeat("d", 64),
			Backend:        pool.ReceiptBackend{Name: "fake"},
			Files:          fakeFiles,
			ManifestDigest: fakeDigest,
		}
		data, err := tampered.Marshal()
		if err != nil {
			t.Fatalf("Receipt.Marshal: %v", err)
		}
		receiptPath := filepath.Join(root, "runtime", "generations", genID, "receipt.json")
		if err := os.WriteFile(receiptPath, data, 0o644); err != nil {
			t.Fatal(err)
		}

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

	// MINOR: doctor never takes the pool lock (design §7.9), so a concurrent
	// `wraptool harness gc` can remove the generation's tree between reading
	// its receipt and scanning its content. That is a race, not corruption,
	// and must not surface as a fail whose remedy tells the operator to
	// rebuild a perfectly healthy pool.
	t.Run("generation directory removed mid-scan (concurrent gc) is silently skipped, not a fail", func(t *testing.T) {
		root := t.TempDir()
		genID, digest := poolTestSeedGeneration(t, root, "claude", "1.2.3", poolTestKey, "v1")
		poolTestActivate(t, root, "claude", poolTestKey, genID)
		poolTestWriteLock(t, root, poolTestLock("claude", poolTestKey, digest))

		genDir := filepath.Join(root, "runtime", "generations", genID)
		receiptPath := filepath.Join(genDir, "receipt.json")
		receiptData, err := os.ReadFile(receiptPath)
		if err != nil {
			t.Fatal(err)
		}
		// Simulate the exact race window: receipt.json was already read
		// successfully (cached below, standing in for that earlier read),
		// but by the time pool.VerifyGenerationTree scans genDir's content —
		// a concurrent `harness gc` — the whole tree is gone.
		if err := os.RemoveAll(genDir); err != nil {
			t.Fatal(err)
		}

		in := &Input{
			Env:   poolTestCachedReadFileEnv{Env: NewOSEnv(nil), path: receiptPath, data: receiptData},
			GOOS:  "linux",
			Paths: HostPaths{PoolRoot: root}, VerifyPool: true,
		}
		res := runPoolGenerationCorrupt(context.Background(), in)
		if res.Outcome() != OutcomePass {
			t.Fatalf("outcome = %v, want pass (findings=%v)", res.Outcome(), res.Findings)
		}
	})
}

// poolTestCachedReadFileEnv wraps a real Env but serves ONE path's content
// from an in-memory cache instead of the real filesystem — the seam this
// package's race-window tests need: a caller that reads a file via Env
// successfully, after which the real, on-disk tree it names is removed
// (mirroring a concurrent `harness gc`) before a SIBLING call that touches
// the real filesystem directly (pool.VerifyGenerationTree, which is not
// Env-mediated — see host.go's Env doc comment on that sanctioned exception).
type poolTestCachedReadFileEnv struct {
	Env
	path string
	data []byte
}

func (e poolTestCachedReadFileEnv) ReadFile(p string) ([]byte, error) {
	if p == e.path {
		return append([]byte(nil), e.data...), nil
	}
	return e.Env.ReadFile(p)
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
		// §6: silence means checked-and-clean. With no active pointer there
		// is no pool-locked runtime for the mount to carry — nothing was
		// checked, so this is Skipped, not Pass.
		{name: "no active pointer", activate: false, wantOutcome: OutcomeSkipped},
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
			// MINOR: name WHICH file was read (both .devcontainer/devcontainer.json
			// and .devcontainer.json are accepted).
			if f.Location != poolTestConfigPath {
				t.Errorf("Location = %q, want %q", f.Location, poolTestConfigPath)
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
		{name: "no active pointer", activate: false, wantOutcome: OutcomeSkipped},
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
			if f.Location != poolTestConfigPath {
				t.Errorf("Location = %q, want %q", f.Location, poolTestConfigPath)
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
			if f.Location != poolTestConfigPath {
				t.Errorf("Location = %q, want %q", f.Location, poolTestConfigPath)
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

// TestPoolRuntimeMountSnippetRejectsUnsafeChars is the MINOR fix porting
// cmd/up.go's devcontainerRuntimeMountSnippet unsafe-character guard: a pool
// root path containing a comma, quote, or backslash cannot be embedded in
// the comma-separated mounts string form without corrupting it, so the
// snippet must refuse to render one rather than emit a broken suggestion.
func TestPoolRuntimeMountSnippetRejectsUnsafeChars(t *testing.T) {
	got := poolRuntimeMountSnippet("/pool, evil/runtime")
	if strings.Contains(got, "target=") {
		t.Errorf("snippet %q was rendered despite an unsafe source path", got)
	}
	if !strings.Contains(got, "cannot render") {
		t.Errorf("snippet %q does not explain the escaping problem", got)
	}

	safe := poolRuntimeMountSnippet("/pool/runtime")
	if !strings.HasPrefix(safe, "source=/pool/runtime,target=") {
		t.Errorf("safe snippet = %q, want a normal mounts entry", safe)
	}
}

// ---------------------------------------------------------------- Meta (Needs) pinning

// TestPoolCheckMeta pins every check's ID and Needs against the SAME Meta
// literal init() registers (poolHomePermsMeta etc.), never through
// Registered() (plan §1 rule 8): once this package's eight sibling Phase-B
// files have all merged, Registered() carries every one of THEIR checks too,
// so an assertion against it would not be testing THIS file's own behaviour
// in isolation. Without this, e.g. dropping NeedsPoolVerify from
// pool.generation-corrupt (making it hash the whole pool tree on every
// default run) would leave every other test in this file green.
func TestPoolCheckMeta(t *testing.T) {
	cases := []struct {
		meta      Meta
		wantID    CheckID
		wantNeeds Requirement
	}{
		{poolHomePermsMeta, poolHomePerms, NeedsModeBits},
		{poolGenerationDigestMeta, poolGenerationDigest, NeedsNothing},
		{poolGenerationCorruptMeta, poolGenerationCorrupt, NeedsPoolVerify},
		{poolDevcontainerMountMissingMeta, poolDevcontainerMountMissing, NeedsWorkspace},
		{poolDevcontainerMountWritableMeta, poolDevcontainerMountWritable, NeedsWorkspace},
		{poolDeprecatedFeatureMeta, poolDeprecatedFeature, NeedsWorkspace},
	}
	for _, tc := range cases {
		t.Run(string(tc.wantID), func(t *testing.T) {
			if tc.meta.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", tc.meta.ID, tc.wantID)
			}
			if tc.meta.Needs != tc.wantNeeds {
				t.Errorf("Needs = %v, want %v", tc.meta.Needs, tc.wantNeeds)
			}
		})
	}
}
