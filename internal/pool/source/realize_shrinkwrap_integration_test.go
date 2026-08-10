package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
)

// TestIntegration_ShrinkwrapSeeding is the "money test" for
// seedShrinkwrapCacheIndex (realize.go): a pi-coding-agent-shaped fixture —
// a harness package that bundles its own npm-shrinkwrap.json, whose
// embedded entry for a workspace-sibling dependency carries a resolved URL
// but NO integrity (the exact, empirically-diagnosed defect a real
// @earendil-works/pi-coding-agent tarball has for its own sibling packages;
// see seedShrinkwrapCacheIndex's doc comment) — realized via the REAL npm
// Source against the REAL npm/tar on the host (hostExecBackend, no
// container runtime, following realize_integration_test.go's own pattern).
//
// Without seeding, `npm ci --offline` fails ENOTCACHED trying to fetch the
// sibling (proven by the "seeding skipped" subtest below, which realizes
// the identical fixture with HasShrinkwrap forced false — the sole gate on
// whether seedShrinkwrapCacheIndex runs at all). With seeding (the
// production path, HasShrinkwrap true), the same fixture realizes cleanly,
// fully offline. Skips if npm is not on PATH.
func TestIntegration_ShrinkwrapSeeding(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH")
	}

	const closedPort = "http://127.0.0.1:1"
	// The sibling URL deliberately carries a query string: make-fetch-happen
	// keys its cache index on the normalized URL WITH search kept
	// (seed_cache_index.js's format options), so a registry serving tarballs
	// via pre-signed or proxied URLs only hits the seeded entry if the query
	// survives normalization — dropping `search: true` there must fail this
	// test, not just exotic registries in the field.
	siblingURL := closedPort + "/tiny-shrink-sibling-1.0.0.tgz?x=1"
	harnessURL := closedPort + "/tiny-shrink-harness-1.0.0.tgz"

	siblingTarball := buildNPMTarballGZ(t, map[string]string{
		"package.json": `{"name":"tiny-shrink-sibling","version":"1.0.0"}`,
	})

	// The harness bundles its OWN npm-shrinkwrap.json (package content,
	// entirely independent of the outer lock this test also builds below).
	// Its entry for the sibling has a resolved URL but deliberately NO
	// integrity — the real, empirically-diagnosed defect: a workspace
	// sibling's entry in a real npm-shrinkwrap.json can lack integrity, and
	// that lockfile is frozen inside the already-published tarball, forever
	// out of wraptool's own backfillIntegrity's reach.
	shrinkwrapJSON := `{
  "lockfileVersion": 3,
  "packages": {
    "": {"dependencies": {"tiny-shrink-sibling": "^1.0.0"}},
    "node_modules/tiny-shrink-sibling": {
      "version": "1.0.0",
      "resolved": ` + `"` + siblingURL + `"` + `
    }
  }
}`
	harnessTarball := buildNPMTarballGZ(t, map[string]string{
		"package.json":        `{"name":"tiny-shrink-harness","version":"1.0.0","bin":{"tiny-shrink-harness":"bin/cli.js"},"dependencies":{"tiny-shrink-sibling":"^1.0.0"}}`,
		"bin/cli.js":          "#!/usr/bin/env node\nconsole.log('hello from tiny-shrink-harness');\n",
		"npm-shrinkwrap.json": shrinkwrapJSON,
	})

	nodeTarXZ, ok := buildFakeNodeTarXZ(t, "node-v99.0.0-linux-x64")
	if !ok {
		t.Skip("host tar cannot produce .tar.xz output")
	}

	siblingDigest := sha256Hex(siblingTarball)
	harnessDigest := sha256Hex(harnessTarball)
	nodeDigest := sha256Hex(nodeTarXZ)

	cacheDir := t.TempDir()
	for digest, content := range map[string][]byte{
		siblingDigest: siblingTarball,
		harnessDigest: harnessTarball,
		nodeDigest:    nodeTarXZ,
	} {
		if err := os.WriteFile(filepath.Join(cacheDir, digest), content, 0o644); err != nil {
			t.Fatalf("seeding cache object %s: %v", digest, err)
		}
	}

	// The OUTER lockfile (this test's own — never mutated inside the
	// tarball) mirrors reality: it HAS an entry for the nested sibling, with
	// integrity (as wraptool's own Resolve/backfillIntegrity would produce),
	// even though the harness package that owns that subtree is marked
	// hasShrinkwrap. Realize materializes this outer document verbatim
	// (ruling 1) — it is the embedded shrinkwrap, not this file, that lacks
	// integrity for the nested fetch.
	rawLockfile := buildRawLockfile(t,
		map[string]string{"tiny-shrink-harness": "1.0.0"},
		map[string]fixtureLockPackage{
			"node_modules/tiny-shrink-harness": {
				Version: "1.0.0", Resolved: harnessURL, Integrity: sriFor(harnessTarball),
				HasShrinkwrap: true,
				Dependencies:  map[string]string{"tiny-shrink-sibling": "^1.0.0"},
			},
			"node_modules/tiny-shrink-harness/node_modules/tiny-shrink-sibling": {
				Version: "1.0.0", Resolved: siblingURL, Integrity: sriFor(siblingTarball),
			},
		})

	h := &pool.LockHarness{
		Source:         "npm",
		Package:        "tiny-shrink-harness",
		Version:        "1.0.0",
		PublishedAt:    pool.FormatTimestamp(realizeFixtureNow),
		FirstSeenAt:    pool.FormatTimestamp(realizeFixtureNow),
		RecipeRevision: "sha256:" + strings.Repeat("a", 64),
		Runtime: &pool.LockRuntime{
			Name:    "node",
			Version: "99.0.0",
			Artifacts: map[pool.PlatformKey]pool.LockRuntimeArtifact{
				pool.PlatformLinuxAMD64Glibc: {URL: closedPort + "/node.tar.xz", SHA256: nodeDigest},
			},
		},
		NPMLock: &pool.NPMLock{
			LockfileVersion: 3,
			Packages: map[string]pool.NPMLockPackage{
				"node_modules/tiny-shrink-harness": {
					Version: "1.0.0", Resolved: harnessURL,
					Integrity: sriFor(harnessTarball), SHA256: harnessDigest,
				},
				"node_modules/tiny-shrink-harness/node_modules/tiny-shrink-sibling": {
					Version: "1.0.0", Resolved: siblingURL,
					Integrity: sriFor(siblingTarball), SHA256: siblingDigest,
				},
			},
			Lockfile:      rawLockfile,
			HasShrinkwrap: true,
		},
	}

	n := NewNPM("")
	b := hostExecBackend{}
	ctx := context.Background()

	t.Run("seeded: realizes fully offline", func(t *testing.T) {
		staging := filepath.Join(t.TempDir(), "gen")
		receipt, err := n.Realize(ctx, b, "tiny-shrink-harness", "tiny-shrink-harness", h, pool.PlatformLinuxAMD64Glibc, cacheDir, staging)
		if err != nil {
			t.Fatalf("Realize: %v", err)
		}
		if err := pool.VerifyGenerationTree(staging, receipt); err != nil {
			t.Errorf("VerifyGenerationTree: %v", err)
		}
		siblingInstalled := filepath.Join(staging, "lib", "node_modules", "tiny-shrink-harness", "node_modules", "tiny-shrink-sibling", "package.json")
		if _, err := os.Stat(siblingInstalled); err != nil {
			t.Errorf("shrinkwrapped sibling was not installed nested under the harness: %v", err)
		}
		wrapperPath := filepath.Join(staging, "bin", "tiny-shrink-harness")
		if _, err := os.Stat(wrapperPath); err != nil {
			t.Errorf("expected the generation's own bin/tiny-shrink-harness to exist: %v", err)
		}
	})

	// The field case that motivated needsShrinkwrapSeeding (a real pi
	// 0.83.0 lock on a real pool): the lock entry was resolved by a
	// wraptool predating the summary has_shrinkwrap flag, so the flag is
	// false — but the verbatim embedded package-lock.json still carries
	// npm's own "hasShrinkwrap" marker. Nothing forces such a lock to
	// re-resolve (version current, recipe_revision unchanged), so realize
	// must derive the gate from the raw document and seed anyway.
	t.Run("stale lock: raw marker only, summary flag false — still seeds", func(t *testing.T) {
		stale := *h.NPMLock
		stale.HasShrinkwrap = false
		hStale := *h
		hStale.NPMLock = &stale

		staging := filepath.Join(t.TempDir(), "gen-stale")
		receipt, err := n.Realize(ctx, b, "tiny-shrink-harness", "tiny-shrink-harness", &hStale, pool.PlatformLinuxAMD64Glibc, cacheDir, staging)
		if err != nil {
			t.Fatalf("Realize: %v", err)
		}
		if err := pool.VerifyGenerationTree(staging, receipt); err != nil {
			t.Errorf("VerifyGenerationTree: %v", err)
		}
	})

	// Mutation: run the IDENTICAL lock through a backend that silently
	// swallows the seed invocation — the lock data cannot be used to force
	// the gate false anymore, because stripping the raw "hasShrinkwrap"
	// marker changes npm's OWN behavior (without the marker in the outer
	// lockfile, arborist installs the sibling from the outer lock's
	// integrity entry and never consults the embedded shrinkwrap — the
	// original forced-flag-false technique only worked while the flag was
	// the sole gate). Dropping the seed run while leaving everything else
	// untouched keeps npm on the shrinkwrap path: `npm ci --offline` must
	// then fail ENOTCACHED fetching the sibling exactly as the live bug
	// did — proving the seeding step (not some other difference) is what
	// makes the subtests above succeed.
	t.Run("seeding skipped: fails ENOTCACHED-class", func(t *testing.T) {
		staging := filepath.Join(t.TempDir(), "gen-unseeded")
		_, err := n.Realize(ctx, seedSwallowingBackend{b}, "tiny-shrink-harness", "tiny-shrink-harness", h, pool.PlatformLinuxAMD64Glibc, cacheDir, staging)
		if err == nil {
			t.Fatal("Realize: got nil error with seeding swallowed, want the ENOTCACHED-class npm ci failure")
		}
		if !strings.Contains(err.Error(), "ENOTCACHED") && !strings.Contains(err.Error(), "only-if-cached") {
			t.Errorf("Realize error = %v, want an ENOTCACHED-class npm cache-miss failure", err)
		}
	})
}

// seedSwallowingBackend passes every backend.Run through except the
// shrinkwrap cache-seed invocation (recognized by the seed script's path in
// argv), which it silently skips — the narrowest possible "seeding did not
// happen" mutation, leaving the lock and npm's own shrinkwrap handling
// untouched.
type seedSwallowingBackend struct {
	inner hostExecBackend
}

func (s seedSwallowingBackend) Name() string    { return s.inner.Name() }
func (s seedSwallowingBackend) Available() bool { return s.inner.Available() }

func (s seedSwallowingBackend) Run(ctx context.Context, spec backend.RunSpec) error {
	for _, a := range spec.Argv {
		if strings.HasSuffix(a, "seed-cache-index.js") {
			return nil
		}
	}
	return s.inner.Run(ctx, spec)
}
