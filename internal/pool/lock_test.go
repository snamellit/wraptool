package pool

import (
	"bytes"
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
					Artifacts: map[string]LockRuntimeArtifact{
						"linux-amd64-glibc": {URL: "https://nodejs.org/dist/x", SHA256: "aaa"},
						"linux-arm64-glibc": {URL: "https://nodejs.org/dist/y", SHA256: "bbb"},
					},
				},
				NPMLock: &NPMLock{
					LockfileVersion: 3,
					Packages: map[string]NPMLockPackage{
						"node_modules/foo": {Version: "1.0.0", Resolved: "https://registry.npmjs.org/foo.tgz", Integrity: "sha512-xyz", SHA256: "ccc"},
						"node_modules/bar": {Version: "2.0.0", Resolved: "https://registry.npmjs.org/bar.tgz", Integrity: "sha512-abc", SHA256: "ddd"},
					},
				},
				Generations: map[string]string{
					"linux-amd64-glibc": "sha256:gen1",
					"linux-arm64-glibc": "sha256:gen2",
				},
			},
			"opencode": {
				Source:  "npm",
				Package: "opencode",
				Version: "1.0.0",
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
