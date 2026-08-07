package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/pool"
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
	siblingURL := closedPort + "/tiny-shrink-sibling-1.0.0.tgz"
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

	// Mutation: force HasShrinkwrap false on an otherwise-identical lock —
	// the sole gate seedShrinkwrapCacheIndex checks — so the seeding step
	// never runs. Everything else (the harness/sibling tarballs, the
	// embedded shrinkwrap with its missing-integrity sibling entry, the
	// outer lockfile) is unchanged. `npm ci --offline` must then fail
	// ENOTCACHED trying to fetch the sibling exactly as the live bug did —
	// proving the seeding step (not some other difference) is what makes
	// the first subtest succeed.
	t.Run("seeding skipped: fails ENOTCACHED-class", func(t *testing.T) {
		unseeded := *h.NPMLock
		unseeded.HasShrinkwrap = false
		hUnseeded := *h
		hUnseeded.NPMLock = &unseeded

		staging := filepath.Join(t.TempDir(), "gen-unseeded")
		_, err := n.Realize(ctx, b, "tiny-shrink-harness", "tiny-shrink-harness", &hUnseeded, pool.PlatformLinuxAMD64Glibc, cacheDir, staging)
		if err == nil {
			t.Fatal("Realize: got nil error with seeding skipped, want the ENOTCACHED-class npm ci failure")
		}
		if !strings.Contains(err.Error(), "ENOTCACHED") && !strings.Contains(err.Error(), "only-if-cached") {
			t.Errorf("Realize error = %v, want an ENOTCACHED-class npm cache-miss failure", err)
		}
	})
}
