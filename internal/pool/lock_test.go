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

// sampleLockNPMLockfile is a raw package-lock.json consistent with
// sampleLock's "claude" harness Packages summary map (node_modules/foo,
// node_modules/bar) — used by the WithNPMLockfile variants below to exercise
// Marshal/Hash with the json.RawMessage field actually populated, since
// sampleLock's own NPMLock.Lockfile is left nil (a lock predating the
// embedded-lockfile schema is exactly what most of this file tests).
const sampleLockNPMLockfile = `{"lockfileVersion":3,"packages":{"":{"dependencies":{"@anthropic-ai/claude-code":"2.1.220"}},"node_modules/bar":{"version":"2.0.0","resolved":"https://registry.npmjs.org/bar.tgz","integrity":"sha512-abc"},"node_modules/foo":{"version":"1.0.0","resolved":"https://registry.npmjs.org/foo.tgz","integrity":"sha512-xyz"}}}`

// sampleLockWithNPMLockfile returns sampleLock with the "claude" harness's
// NPMLock.Lockfile populated — the tripwire fixture for anyone touching the
// json.RawMessage handling in Marshal/UnmarshalLock/Hash: RawMessage is NOT
// an ordinary Go value (its own MarshalJSON just re-emits its stored bytes
// verbatim), so it is exactly the kind of field a naive refactor could
// silently break determinism or round-tripping for without any of the
// OTHER fields' tests noticing.
func sampleLockWithNPMLockfile() *Lock {
	l := sampleLock()
	h := l.Harnesses["claude"]
	h.NPMLock.Lockfile = json.RawMessage(sampleLockNPMLockfile)
	l.Harnesses["claude"] = h
	return l
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

// TestLockMarshalDeterministic_WithNPMLockfile is TestLockMarshalDeterministic's
// counterpart for a Lock whose NPMLock.Lockfile is populated (finding C):
// json.RawMessage's own MarshalJSON re-emits its stored bytes as-is, so
// Marshal's determinism claim needs its own proof once that field is in play,
// not just an inference from the (Lockfile-less) fixture every other test
// here uses.
func TestLockMarshalDeterministic_WithNPMLockfile(t *testing.T) {
	l := sampleLockWithNPMLockfile()

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

	l2 := sampleLockWithNPMLockfile()
	c, err := l2.Marshal()
	if err != nil {
		t.Fatalf("Marshal (3): %v", err)
	}
	if !bytes.Equal(a, c) {
		t.Fatalf("Marshal differs across equal values:\n--- a ---\n%s\n--- c ---\n%s", a, c)
	}
}

// TestLockMarshalUnmarshalRoundTrip_WithNPMLockfile is
// TestLockMarshalUnmarshalRoundTrip's counterpart with NPMLock.Lockfile
// populated: persist -> read back -> re-persist must be byte-equal, and the
// raw lockfile bytes themselves must survive the round trip unchanged.
func TestLockMarshalUnmarshalRoundTrip_WithNPMLockfile(t *testing.T) {
	l := sampleLockWithNPMLockfile()
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

	gotLockfile := got.Harnesses["claude"].NPMLock.Lockfile
	var wantCompact, gotCompact bytes.Buffer
	if err := json.Compact(&wantCompact, []byte(sampleLockNPMLockfile)); err != nil {
		t.Fatalf("compacting fixture: %v", err)
	}
	if err := json.Compact(&gotCompact, gotLockfile); err != nil {
		t.Fatalf("compacting round-tripped Lockfile: %v", err)
	}
	if wantCompact.String() != gotCompact.String() {
		t.Errorf("round-tripped NPMLock.Lockfile = %s, want (compacted) %s", gotCompact.String(), wantCompact.String())
	}
}

// TestLockHarnessHash_StableWithNPMLockfile: Hash must be stable across
// repeated calls, and across a fresh value built independently (not read
// back through JSON at all), once NPMLock.Lockfile is populated.
func TestLockHarnessHash_StableWithNPMLockfile(t *testing.T) {
	h1 := sampleLockWithNPMLockfile().Harnesses["claude"]
	h2 := sampleLockWithNPMLockfile().Harnesses["claude"]

	hash1, err := h1.Hash()
	if err != nil {
		t.Fatalf("Hash (1): %v", err)
	}
	hash1Again, err := h1.Hash()
	if err != nil {
		t.Fatalf("Hash (1, again): %v", err)
	}
	if hash1 != hash1Again {
		t.Errorf("Hash is not stable across repeated calls on the same value: %q vs %q", hash1, hash1Again)
	}

	hash2, err := h2.Hash()
	if err != nil {
		t.Fatalf("Hash (2): %v", err)
	}
	if hash1 != hash2 {
		t.Errorf("Hash differs across two independently-built, logically-equal values: %q vs %q", hash1, hash2)
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

// --- LockArchive (archive-source additive schema) -----------------------

// sampleArchiveArtifacts returns a well-formed Artifacts map for two
// platforms, reused by the tests below.
func sampleArchiveArtifacts() map[PlatformKey]LockArchiveArtifact {
	return map[PlatformKey]LockArchiveArtifact{
		PlatformLinuxAMD64Glibc: {
			URL:    "https://storage.googleapis.com/example/agy-amd64.tar.gz",
			SHA512: strings.Repeat("a", 128),
			SHA256: strings.Repeat("1", 64),
		},
		PlatformLinuxARM64Glibc: {
			URL:    "https://storage.googleapis.com/example/agy-arm64.tar.gz",
			SHA512: strings.Repeat("b", 128),
			// SHA256 intentionally empty here — not yet Fetched.
		},
	}
}

// sampleLockWithArchive returns sampleLock with an additional "agy" harness
// whose LockHarness.Archive is populated — the archive-source counterpart of
// sampleLockWithNPMLockfile, exercising Marshal/Validate/round-trip with the
// new field set (§8 archive-source additive schema).
func sampleLockWithArchive() *Lock {
	l := sampleLock()
	l.Harnesses["agy"] = LockHarness{
		Source:         "archive",
		Package:        "agy",
		Version:        "1.1.10",
		PublishedAt:    "2026-07-29T12:00:00Z",
		FirstSeenAt:    "2026-07-29T12:10:00Z",
		RecipeRevision: "sha256:archive-recipe",
		Archive:        &LockArchive{Artifacts: sampleArchiveArtifacts()},
	}
	return l
}

func TestLockMarshalDeterministic_WithArchive(t *testing.T) {
	l := sampleLockWithArchive()

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

	l2 := sampleLockWithArchive()
	c, err := l2.Marshal()
	if err != nil {
		t.Fatalf("Marshal (3): %v", err)
	}
	if !bytes.Equal(a, c) {
		t.Fatalf("Marshal differs across equal values:\n--- a ---\n%s\n--- c ---\n%s", a, c)
	}
}

func TestLockMarshalUnmarshalRoundTrip_WithArchive(t *testing.T) {
	l := sampleLockWithArchive()
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

	gotArchive := got.Harnesses["agy"].Archive
	if gotArchive == nil {
		t.Fatal("round-tripped lock has no Archive field for \"agy\"")
	}
	want := sampleArchiveArtifacts()
	if len(gotArchive.Artifacts) != len(want) {
		t.Fatalf("got %d archive artifacts, want %d", len(gotArchive.Artifacts), len(want))
	}
	for key, wantArtifact := range want {
		got, ok := gotArchive.Artifacts[key]
		if !ok {
			t.Fatalf("missing archive artifact for platform %s", key)
		}
		if got != wantArtifact {
			t.Errorf("archive artifact %s = %+v, want %+v", key, got, wantArtifact)
		}
	}
}

func TestLockHarnessHash_StableWithArchive(t *testing.T) {
	h1 := sampleLockWithArchive().Harnesses["agy"]
	h2 := sampleLockWithArchive().Harnesses["agy"]

	hash1, err := h1.Hash()
	if err != nil {
		t.Fatalf("Hash (1): %v", err)
	}
	hash1Again, err := h1.Hash()
	if err != nil {
		t.Fatalf("Hash (1, again): %v", err)
	}
	if hash1 != hash1Again {
		t.Errorf("Hash is not stable across repeated calls on the same value: %q vs %q", hash1, hash1Again)
	}

	hash2, err := h2.Hash()
	if err != nil {
		t.Fatalf("Hash (2): %v", err)
	}
	if hash1 != hash2 {
		t.Errorf("Hash differs across two independently-built, logically-equal values: %q vs %q", hash1, hash2)
	}
}

func TestLockValidate_ArchiveWellFormed(t *testing.T) {
	l := sampleLockWithArchive()
	if err := l.Validate(); err != nil {
		t.Fatalf("Validate() with a well-formed Archive field: %v", err)
	}
}

func TestLockValidate_ArchiveRejectsNonHTTPSURL(t *testing.T) {
	l := sampleLockWithArchive()
	h := l.Harnesses["agy"]
	artifacts := sampleArchiveArtifacts()
	a := artifacts[PlatformLinuxAMD64Glibc]
	a.URL = "http://storage.googleapis.com/example/agy-amd64.tar.gz"
	artifacts[PlatformLinuxAMD64Glibc] = a
	h.Archive = &LockArchive{Artifacts: artifacts}
	l.Harnesses["agy"] = h

	if err := l.Validate(); err == nil {
		t.Fatal("Validate: got nil error, want rejection of a non-https artifact URL")
	}
}

func TestLockValidate_ArchiveRejectsMalformedSHA512(t *testing.T) {
	l := sampleLockWithArchive()
	h := l.Harnesses["agy"]
	artifacts := sampleArchiveArtifacts()
	a := artifacts[PlatformLinuxAMD64Glibc]
	a.SHA512 = "not-hex-and-wrong-length"
	artifacts[PlatformLinuxAMD64Glibc] = a
	h.Archive = &LockArchive{Artifacts: artifacts}
	l.Harnesses["agy"] = h

	if err := l.Validate(); err == nil {
		t.Fatal("Validate: got nil error, want rejection of a malformed sha512")
	}
}

func TestLockValidate_ArchiveRejectsMalformedSHA256WhenPresent(t *testing.T) {
	l := sampleLockWithArchive()
	h := l.Harnesses["agy"]
	artifacts := sampleArchiveArtifacts()
	a := artifacts[PlatformLinuxAMD64Glibc]
	a.SHA256 = "too-short"
	artifacts[PlatformLinuxAMD64Glibc] = a
	h.Archive = &LockArchive{Artifacts: artifacts}
	l.Harnesses["agy"] = h

	if err := l.Validate(); err == nil {
		t.Fatal("Validate: got nil error, want rejection of a malformed (non-empty) sha256")
	}
}

func TestLockValidate_ArchiveAllowsEmptySHA256(t *testing.T) {
	// sampleArchiveArtifacts already leaves linux-arm64-glibc's SHA256 empty
	// (not yet Fetched) — Validate must accept that, not require it.
	l := sampleLockWithArchive()
	if err := l.Validate(); err != nil {
		t.Fatalf("Validate() with an empty (not-yet-fetched) archive sha256: %v", err)
	}
}

func TestLockValidate_ArchiveRejectsUnknownPlatformKey(t *testing.T) {
	data := []byte(`{
		"schema_version": 1,
		"generated_at": "2026-07-30T09:00:00Z",
		"policy_hash": "sha256:abc",
		"harnesses": {
			"agy": {
				"source": "archive",
				"package": "agy",
				"version": "1.1.10",
				"published_at": "2026-07-29T12:00:00Z",
				"first_seen_at": "2026-07-29T12:10:00Z",
				"archive": {
					"artifacts": {
						"darwin-arm64": {"url": "https://example/x.tar.gz", "sha512": "` + strings.Repeat("a", 128) + `"}
					}
				}
			}
		}
	}`)
	if _, err := UnmarshalLock(data); err == nil {
		t.Fatal("expected an error for an unrecognized platform key in archive artifacts, got nil")
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
