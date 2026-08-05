package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/pool"
)

// TestIntegration_RealArchiveSourceThroughRealOrchestrator drives the REAL
// archive Source (this package's Archive: Refresh -> Resolve -> Fetch ->
// Realize) through the REAL command-level orchestrator
// (pool.RefreshHarness, pool.UpdateHarness) on a t.TempDir pool — the
// archive-source counterpart of npm_integration_test.go's
// TestIntegration_RealNPMSourceThroughRealOrchestrator. No real network
// request ever leaves this test (the manifest server is a local
// httptest.Server, the artifact server a local httptest.NewTLSServer whose
// certificate this test's Archive is pointed at); no container backend is
// involved at all — this call passes a nil backend.Backend all the way
// through UpdateHarness, proving (alongside archive_test.go's own
// fatalBackend-based unit tests) that an archive-sourced install genuinely
// never dereferences one (§6.2, this work package's headline capability:
// `wraptool harness install agy` reaches realize/activate with no
// guix/OCI backend present at all).
//
// Regression coverage this test is uniquely positioned for (unlike
// archive_test.go's own Source-level tests, which never touch
// observations.json/harness-lock.json/runtime/ at all): the PublishedAt
// synthesis (RefreshHarnessLocked/UpdateHarnessLocked, internal/pool/
// orchestrator.go) that lets an archive candidate — whose Refresh/Resolve
// always return a zero PublishedAt — actually become eligible and get
// persisted into a lock that (*pool.Lock).Validate accepts.
func TestIntegration_RealArchiveSourceThroughRealOrchestrator(t *testing.T) {
	const version = "1.1.10"
	const harnessName = "agy"
	scriptContent := "#!/bin/sh\necho hello from fake agy\n"
	tarGZ := buildAgyTarGZ(t, "", map[string]string{"agy": scriptContent}) // no wrapping dir — "the flat native binary"

	// httptest.NewTLSServer, not NewServer: Resolve rejects a non-https
	// manifest-supplied artifact URL before it is ever locked (see
	// archive_integration_test.go's matching comment on the same choice).
	artifactSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarGZ)
	}))
	defer artifactSrv.Close()

	artifactSHA512 := sha512Hex(tarGZ)
	manifestSrv := newManifestServer(t, map[string]archiveManifest{
		"linux_amd64": {Version: version, URL: artifactSrv.URL + "/agy-linux-amd64.tar.gz", SHA512: artifactSHA512},
		"linux_arm64": {Version: version, URL: artifactSrv.URL + "/agy-linux-arm64.tar.gz", SHA512: artifactSHA512},
	})
	defer manifestSrv.Close()

	a := NewArchive(manifestSrv.URL)
	// Trust the TLS test server's self-signed certificate for Fetch's
	// downloads; production code never overrides this client (only this
	// package's own tests, which construct an Archive literal directly,
	// can reach the unexported field — see archive_integration_test.go's
	// identical setup).
	a.downloadClient = artifactSrv.Client()
	a.downloadClient.Timeout = downloadTimeout

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
	// DEFAULT manifest policy (168h minimum_release_age, 0h
	// minimum_observation_age) — no policy override at all — is deliberate
	// here (wave2-integration-fixes item 1: this test previously carried a
	// `minimum_release_age: 0h` override that papered over the very
	// regression it exists to catch). An archive candidate's PublishedAt is
	// synthesized as FirstSeenAt (RefreshHarnessLocked) and marked
	// PublishedAtSynthesized, which makes Eligible skip the
	// minimum_release_age check entirely for it (RULING,
	// Candidate.PublishedAtSynthesized's doc comment) — so this
	// single-pass refresh-then-install sequence must succeed at DAY 0 under
	// the real default policy, not just under a relaxed one.
	manifestYAML := "schema_version: 1\nharnesses:\n  agy: {}\n"
	if err := os.MkdirAll(filepath.Dir(paths.Manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Manifest, []byte(manifestYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	// --- Refresh (real Source.Refresh, four manifest GETs — two reachable) ---
	if _, err := pool.RefreshHarness(ctx, a, paths, pool.SourceArchive, harnessName, now); err != nil {
		t.Fatalf("RefreshHarness: %v", err)
	}

	// --- Install (real Source.Resolve/Fetch/Realize, nil backend) ---
	result, err := pool.UpdateHarness(ctx, a, nil, paths, pool.UpdateOptions{
		Harness: harnessName, Package: harnessName, Source: pool.SourceArchive, BinName: "agy", Now: now, Advance: false,
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if !result.Changed || result.Harness.Version != version {
		t.Fatalf("UpdateResult = %+v, want Changed=true Version=%s", result, version)
	}
	if result.RealizeErr != nil {
		t.Fatalf("RealizeErr = %v, want nil (realize must succeed with no backend at all)", result.RealizeErr)
	}
	if result.RealizedGenerationID == "" {
		t.Fatal("expected UpdateResult.RealizedGenerationID to be set")
	}

	// --- Assertion: lock exists and validates. ---
	lockBytes, err := os.ReadFile(paths.Lock)
	if err != nil {
		t.Fatalf("reading harness-lock.json: %v", err)
	}
	lockDoc, err := pool.UnmarshalLock(lockBytes)
	if err != nil {
		t.Fatalf("harness-lock.json failed to validate: %v", err)
	}
	lh, ok := lockDoc.Harnesses[harnessName]
	if !ok || lh.Source != "archive" || lh.Version != version {
		t.Fatalf("lock entry = %+v, ok=%v, want source=archive version=%s", lh, ok, version)
	}

	// --- Assertion: the generation directory exists with a receipt. ---
	genDir := filepath.Join(paths.Runtime, "generations", result.RealizedGenerationID)
	if _, err := os.Stat(filepath.Join(genDir, "receipt.json")); err != nil {
		t.Errorf("expected a receipt.json in the published generation %s: %v", genDir, err)
	}
	if _, err := os.Stat(filepath.Join(genDir, "bin", "agy")); err != nil {
		t.Errorf("expected the generation's own bin/agy to exist: %v", err)
	}

	// --- Assertion: the pointer file is live for the host's own platform. ---
	hostKey, err := pool.HostPlatformKey()
	if err != nil {
		t.Fatalf("HostPlatformKey: %v", err)
	}
	activeID, ok := pool.ActiveGeneration(paths, harnessName, hostKey)
	if !ok || activeID != result.RealizedGenerationID {
		t.Errorf("ActiveGeneration = %q, %v, want %q, true", activeID, ok, result.RealizedGenerationID)
	}

	// --- Assertion: the dispatcher script was generated. ---
	if _, err := os.Stat(filepath.Join(paths.Runtime, "bin", "agy")); err != nil {
		t.Errorf("expected a generated dispatcher at runtime/bin/agy: %v", err)
	}

	// --- Assertion: `list`'s ACTIVE column source (StatusHarness +
	// GenerationVersion) reports exactly this version, the same computation
	// cmd/harness.go's runHarnessList performs. ---
	status, err := pool.StatusHarness(paths, harnessName, pool.SourceArchive, harnessName, now)
	if err != nil {
		t.Fatalf("StatusHarness: %v", err)
	}
	genID, ok := status.ActiveGenerations[hostKey]
	if !ok {
		t.Fatalf("StatusHarness: no ActiveGenerations entry for %s", hostKey)
	}
	activeVersion, err := pool.GenerationVersion(paths, genID)
	if err != nil {
		t.Fatalf("GenerationVersion: %v", err)
	}
	if activeVersion != version {
		t.Errorf("ACTIVE version = %q, want %q", activeVersion, version)
	}

	// --- Assertion: observations.json carries no zero PublishedAt for this
	// version (the PublishedAt-synthesis regression this whole test exists
	// to cover). ---
	obsBytes, err := os.ReadFile(paths.Observations)
	if err != nil {
		t.Fatalf("reading observations.json: %v", err)
	}
	if strings.Contains(string(obsBytes), "0001-01-01") {
		t.Errorf("observations.json contains a zero timestamp:\n%s", obsBytes)
	}
}
