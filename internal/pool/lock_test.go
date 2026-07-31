package pool

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampleLock() *Lock {
	return &Lock{
		SchemaVersion: LockSchemaVersion,
		GeneratedAt:   "2026-07-30T09:00:00Z",
		PolicyHash:    "sha256:abc123",
		Harnesses: map[string]LockHarness{
			"claude": {
				Source:         "npm",
				Package:        "@anthropic-ai/claude-code",
				Version:        "2.1.220",
				PublishedAt:    "2026-07-29T12:00:00Z",
				FirstSeenAt:    "2026-07-29T12:10:00Z",
				RecipeRevision: "sha256:def456",
				Runtime: &LockRuntime{
					Name:    "node",
					Version: "24.18.1",
					Artifacts: map[PlatformKey]LockRuntimeArtifact{
						PlatformLinuxAMD64Glibc: {URL: "https://nodejs.org/dist/x", SHA256: "aaa"},
						PlatformLinuxARM64Glibc: {URL: "https://nodejs.org/dist/y", SHA256: "bbb"},
					},
				},
				NPMLock: &NPMLock{
					LockfileVersion: 3,
					Packages: map[string]NPMLockPackage{
						"node_modules/foo": {Version: "1.0.0", Resolved: "https://registry.npmjs.org/foo.tgz", Integrity: "sha512-xyz", SHA256: "ccc"},
						"node_modules/bar": {Version: "2.0.0", Resolved: "https://registry.npmjs.org/bar.tgz", Integrity: "sha512-abc", SHA256: "ddd"},
					},
				},
				Generations: map[PlatformKey]string{
					PlatformLinuxAMD64Glibc: "sha256:gen1",
					PlatformLinuxARM64Glibc: "sha256:gen2",
				},
			},
			"opencode": {
				Source:      "npm",
				Package:     "opencode",
				Version:     "1.0.0",
				PublishedAt: "2026-06-01T00:00:00Z",
				FirstSeenAt: "2026-06-01T00:10:00Z",
			},
		},
	}
}

func TestLockMarshalDeterministic(t *testing.T) {
	l := sampleLock()

	a, err := l.Marshal()
	if err != nil {
		t.Fatalf("Marshal (1): %v", err)
	}
	b, err := l.Marshal()
	if err != nil {
		t.Fatalf("Marshal (2): %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("Marshal is not deterministic:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}

	// A freshly built, logically-equal value (maps populated in different
	// insertion order) must serialize identically too.
	l2 := sampleLock()
	c, err := l2.Marshal()
	if err != nil {
		t.Fatalf("Marshal (3): %v", err)
	}
	if !bytes.Equal(a, c) {
		t.Fatalf("Marshal differs across equal values:\n--- a ---\n%s\n--- c ---\n%s", a, c)
	}
}

func TestLockMarshalUnmarshalRoundTrip(t *testing.T) {
	l := sampleLock()
	data, err := l.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := UnmarshalLock(data)
	if err != nil {
		t.Fatalf("UnmarshalLock: %v", err)
	}
	data2, err := got.Marshal()
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if !bytes.Equal(data, data2) {
		t.Fatalf("round-trip is not byte-stable:\n--- original ---\n%s\n--- re-marshaled ---\n%s", data, data2)
	}
}

func TestUnmarshalLockRejectsGarbage(t *testing.T) {
	if _, err := UnmarshalLock([]byte("not json")); err == nil {
		t.Fatal("expected an error for invalid JSON, got nil")
	}
}

// TestUnmarshalLockRejectsUnknownPlatformKey covers MINOR 5: a generations or
// runtime-artifacts key that isn't one of the closed PlatformKey values must
// be rejected, not silently accepted as an opaque string.
func TestUnmarshalLockRejectsUnknownPlatformKey(t *testing.T) {
	data := []byte(`{
		"schema_version": 1,
		"generated_at": "2026-07-30T09:00:00Z",
		"policy_hash": "sha256:abc",
		"harnesses": {
			"claude": {
				"source": "npm",
				"package": "claude",
				"version": "2.1.220",
				"published_at": "2026-07-29T12:00:00Z",
				"first_seen_at": "2026-07-29T12:10:00Z",
				"generations": {"darwin-arm64": "sha256:gen1"}
			}
		}
	}`)
	if _, err := UnmarshalLock(data); err == nil {
		t.Fatal("expected an error for an unrecognized platform key, got nil")
	}
}

// TestUnmarshalLockRejectsBadTimestamps covers MINOR 5's timestamp
// validation on the read path.
func TestUnmarshalLockRejectsBadTimestamps(t *testing.T) {
	t.Run("unparsable generated_at", func(t *testing.T) {
		l := sampleLock()
		l.GeneratedAt = "not-a-timestamp"
		data, err := l.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if _, err := UnmarshalLock(data); err == nil {
			t.Fatal("expected an error for an unparsable generated_at, got nil")
		}
	})
	t.Run("non-UTC published_at", func(t *testing.T) {
		l := sampleLock()
		h := l.Harnesses["claude"]
		h.PublishedAt = "2026-07-29T12:00:00+02:00"
		l.Harnesses["claude"] = h
		data, err := l.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if _, err := UnmarshalLock(data); err == nil {
			t.Fatal("expected an error for a non-UTC published_at, got nil")
		}
	})
}

// TestLockValidateIsExportedAndUsableBeforePersisting covers N5: a caller
// that assembled a Lock in memory (never round-tripped through JSON) must be
// able to validate it directly via Lock.Validate, without going through
// Marshal/UnmarshalLock.
func TestLockValidateIsExportedAndUsableBeforePersisting(t *testing.T) {
	l := sampleLock()
	if err := l.Validate(); err != nil {
		t.Fatalf("Validate() on a well-formed in-memory lock: %v", err)
	}
}

// TestLockValidateRejectsZeroTimestamp covers N5: FormatTimestamp(time.Time{})
// yields the syntactically valid RFC3339 string "0001-01-01T00:00:00Z",
// which ParseTimestamp alone accepts — Validate must additionally reject it,
// since an unset time must never survive into a persisted lock.
func TestLockValidateRejectsZeroTimestamp(t *testing.T) {
	zero := FormatTimestamp(time.Time{})
	if zero != "0001-01-01T00:00:00Z" {
		t.Fatalf("test assumption broken: FormatTimestamp(time.Time{}) = %q", zero)
	}

	t.Run("generated_at", func(t *testing.T) {
		l := sampleLock()
		l.GeneratedAt = zero
		if err := l.Validate(); err == nil {
			t.Fatal("expected an error for a zero generated_at, got nil")
		}
	})
	t.Run("harness published_at", func(t *testing.T) {
		l := sampleLock()
		h := l.Harnesses["claude"]
		h.PublishedAt = zero
		l.Harnesses["claude"] = h
		if err := l.Validate(); err == nil {
			t.Fatal("expected an error for a zero published_at, got nil")
		}
	})
	t.Run("harness first_seen_at", func(t *testing.T) {
		l := sampleLock()
		h := l.Harnesses["claude"]
		h.FirstSeenAt = zero
		l.Harnesses["claude"] = h
		if err := l.Validate(); err == nil {
			t.Fatal("expected an error for a zero first_seen_at, got nil")
		}
	})
}

// TestUnmarshalLockRejectsZeroTimestamp confirms UnmarshalLock's call to
// Validate catches the same zero-timestamp case on the read path, not only
// via direct Lock.Validate calls.
func TestUnmarshalLockRejectsZeroTimestamp(t *testing.T) {
	l := sampleLock()
	l.GeneratedAt = FormatTimestamp(time.Time{})
	data, err := l.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := UnmarshalLock(data); err == nil {
		t.Fatal("expected an error for a zero generated_at, got nil")
	}
}

// --- NPMLock.Lockfile consistency (Validate) --------------------------

// npmLockfileFixtureFor builds a minimal, valid raw package-lock.json (as
// json.RawMessage) whose "packages" map contains exactly names.
func npmLockfileFixtureFor(names ...string) json.RawMessage {
	pkgs := make(map[string]any, len(names))
	pkgs[""] = map[string]any{"dependencies": map[string]string{}}
	for _, n := range names {
		pkgs[n] = map[string]any{"version": "1.0.0"}
	}
	data, err := json.Marshal(map[string]any{
		"lockfileVersion": 3,
		"packages":        pkgs,
	})
	if err != nil {
		panic(err) // fixture construction, never expected to fail
	}
	return data
}

func TestLockValidate_NPMLockfileConsistency(t *testing.T) {
	l := sampleLock()
	h := l.Harnesses["claude"]
	h.NPMLock.Lockfile = npmLockfileFixtureFor("node_modules/foo", "node_modules/bar")
	l.Harnesses["claude"] = h

	if err := l.Validate(); err != nil {
		t.Fatalf("Validate() with a consistent Lockfile: %v", err)
	}
}

func TestLockValidate_NoLockfileIsANoOp(t *testing.T) {
	// sampleLock's NPMLock.Lockfile is unset (nil) by default — Validate
	// must not require it (old locks, or a non-npm source, have none).
	l := sampleLock()
	if l.Harnesses["claude"].NPMLock.Lockfile != nil {
		t.Fatal("test assumption broken: sampleLock already sets Lockfile")
	}
	if err := l.Validate(); err != nil {
		t.Fatalf("Validate() with no Lockfile: %v", err)
	}
}

func TestLockValidate_RejectsInvalidJSONLockfile(t *testing.T) {
	l := sampleLock()
	h := l.Harnesses["claude"]
	h.NPMLock.Lockfile = json.RawMessage(`not json`)
	l.Harnesses["claude"] = h

	err := l.Validate()
	if err == nil {
		t.Fatal("Validate: got nil error, want rejection of invalid JSON in Lockfile")
	}
	if !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("error = %q, want it to mention invalid JSON", err.Error())
	}
}

func TestLockValidate_RejectsWrongLockfileVersionInLockfile(t *testing.T) {
	l := sampleLock()
	h := l.Harnesses["claude"]
	h.NPMLock.Lockfile = json.RawMessage(`{"lockfileVersion":2,"packages":{}}`)
	l.Harnesses["claude"] = h

	err := l.Validate()
	if err == nil {
		t.Fatal("Validate: got nil error, want rejection of lockfileVersion != 3")
	}
	if !strings.Contains(err.Error(), "lockfileVersion") {
		t.Errorf("error = %q, want it to mention lockfileVersion", err.Error())
	}
}

func TestLockValidate_RejectsLockfileMissingASummaryMapEntry(t *testing.T) {
	l := sampleLock()
	h := l.Harnesses["claude"]
	// Lockfile only carries "node_modules/foo"; the summary map (from
	// sampleLock) also has "node_modules/bar".
	h.NPMLock.Lockfile = npmLockfileFixtureFor("node_modules/foo")
	l.Harnesses["claude"] = h

	err := l.Validate()
	if err == nil {
		t.Fatal("Validate: got nil error, want rejection of a summary-map entry missing from Lockfile")
	}
	if !strings.Contains(err.Error(), "node_modules/bar") {
		t.Errorf("error = %q, want it to name node_modules/bar", err.Error())
	}
}

func TestPolicyHashStableAndDistinguishing(t *testing.T) {
	p1 := EffectivePolicy{MinimumReleaseAge: 168 * time.Hour, MinimumObservationAge: 0, AllowPrereleases: false}
	p2 := EffectivePolicy{MinimumReleaseAge: 168 * time.Hour, MinimumObservationAge: 0, AllowPrereleases: false}
	p3 := EffectivePolicy{MinimumReleaseAge: 72 * time.Hour, MinimumObservationAge: 0, AllowPrereleases: false}

	h1, err := PolicyHash(p1)
	if err != nil {
		t.Fatalf("PolicyHash(p1): %v", err)
	}
	h2, err := PolicyHash(p2)
	if err != nil {
		t.Fatalf("PolicyHash(p2): %v", err)
	}
	h3, err := PolicyHash(p3)
	if err != nil {
		t.Fatalf("PolicyHash(p3): %v", err)
	}

	if h1 != h2 {
		t.Errorf("equal policies produced different hashes: %q vs %q", h1, h2)
	}
	if h1 == h3 {
		t.Errorf("different policies produced the same hash: %q", h1)
	}
	if h1[:7] != "sha256:" {
		t.Errorf("PolicyHash() = %q, want sha256:<hex> prefix", h1)
	}
}
