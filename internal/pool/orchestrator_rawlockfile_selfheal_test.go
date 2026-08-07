package pool

import (
	"encoding/json"
	"testing"
	"time"
)

// This file is item (b) of the npm-raw-lockfile-backfill rework: a lock
// persisted by a pre-fix binary can carry a raw npm lockfile that is
// syntactically valid (loads fine — see lock_test.go's
// TestLockValidate_ToleratesRawEntryMissingIntegrity) but semantically
// incomplete (a raw entry with a resolved URL and no integrity, exactly
// @earendil-works/pi-tui's shape). This must self-heal on the very next
// install/update with zero manual steps: decision.go's
// DecisionInput.LockNPMRawIncomplete, folded into lockMatchesRecipe exactly
// like a recipe-format bump, forces UpdateHarnessLocked to call Source.Resolve
// again even though the package/version/recipe_revision all already agree —
// see that field's doc comment for the full mechanism.

// incompletePiShapedRawLockfile is the exact pi-coding-agent shape: a nested
// sibling entry with a registry resolved URL but no integrity, alongside a
// well-formed top-level entry.
const incompletePiShapedRawLockfile = `{
  "lockfileVersion": 3,
  "packages": {
    "": {"dependencies": {"@anthropic-ai/claude-code": "2.1.220"}},
    "node_modules/@anthropic-ai/claude-code": {
      "version": "2.1.220",
      "resolved": "https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-2.1.220.tgz",
      "integrity": "sha512-AAAA=="
    },
    "node_modules/@anthropic-ai/claude-code/node_modules/pi-sibling": {
      "version": "2.0.0",
      "resolved": "https://registry.npmjs.org/pi-sibling/-/pi-sibling-2.0.0.tgz"
    }
  }
}`

// patchedPiShapedRawLockfile is what a FIXED Source.Resolve would produce
// for the same package/version — the sibling's integrity backfilled and
// patched into the raw document (npm.go's Resolve/patchLockfileIntegrity in
// the sibling internal/pool/source package; reproduced by hand here since
// this test uses fakeSource, not the real npm Source). It also carries a
// bare "@anthropic-ai/claude-code" packages entry — not a shape any real
// npm lockfile has, but fakeSource.Fetch's OWN summary-map key is the bare
// package name (see its doc comment), and (*Lock).Validate's summary/raw
// consistency check requires every summary-map key to appear in the raw
// document too; this is a fakeSource-simulation artifact, not a production
// concern.
const patchedPiShapedRawLockfile = `{
  "lockfileVersion": 3,
  "packages": {
    "": {"dependencies": {"@anthropic-ai/claude-code": "2.1.220"}},
    "node_modules/@anthropic-ai/claude-code": {
      "version": "2.1.220",
      "resolved": "https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-2.1.220.tgz",
      "integrity": "sha512-AAAA=="
    },
    "node_modules/@anthropic-ai/claude-code/node_modules/pi-sibling": {
      "version": "2.0.0",
      "resolved": "https://registry.npmjs.org/pi-sibling/-/pi-sibling-2.0.0.tgz",
      "integrity": "sha512-BACKFILLED=="
    },
    "@anthropic-ai/claude-code": {
      "version": "2.1.220",
      "resolved": "https://registry.npmjs.org/x.tgz",
      "integrity": "sha512-fakeintegrity"
    }
  }
}`

// TestUpdateHarnessLockedSelfHealsIncompleteRawLockfile is item (b): install
// (Advance=false) on a pool whose existing lock already agrees on package,
// version, AND recipe_revision — the strict short-circuit's usual
// condition — but whose raw npm lockfile is incomplete. Without
// LockNPMRawIncomplete's fold-in, this would short-circuit straight to
// DecisionUpToDate/DecisionRealize and never call Resolve again, wedging the
// pool exactly like the field report described. With it, Resolve runs, and
// the persisted lock's raw lockfile is complete afterward.
func TestUpdateHarnessLockedSelfHealsIncompleteRawLockfile(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"

	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	wantRecipe, err := npmRecipeRevision(pkg, "")
	if err != nil {
		t.Fatal(err)
	}

	existingLockfile := json.RawMessage(incompletePiShapedRawLockfile)
	existingNPMLock := &NPMLock{
		LockfileVersion: 3,
		Lockfile:        existingLockfile,
		Packages: map[string]NPMLockPackage{
			"node_modules/" + pkg: {Version: "2.1.220", Resolved: "https://registry.npmjs.org/x.tgz", Integrity: "sha512-AAAA==", SHA256: "deadbeef"},
		},
	}
	if existingNPMLock.RawComplete() {
		t.Fatal("test setup bug: incompletePiShapedRawLockfile must be RawComplete()==false")
	}

	writeLock(t, paths, &Lock{
		SchemaVersion: LockSchemaVersion, GeneratedAt: FormatTimestamp(fixedNow.Add(-time.Hour)), PolicyHash: "sha256:x",
		Harnesses: map[string]LockHarness{
			"claude": {
				Source: npmSourceName, Package: pkg, Version: "2.1.220",
				PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
				RecipeRevision: wantRecipe, // package/version/recipe ALL already agree
				NPMLock:        existingNPMLock,
			},
		},
	})

	src := &fakeSource{resolveNPMLockfile: json.RawMessage(patchedPiShapedRawLockfile)}
	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, // install (Advance: false)
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected Changed=true: an incomplete raw lockfile must force a re-resolve even though package/version/recipe all already agree")
	}
	if src.resolveCalls != 1 {
		t.Errorf("resolveCalls = %d, want 1 — the strict short-circuit must NOT fire when the raw lockfile is incomplete", src.resolveCalls)
	}
	if src.realizeCalls != 1 {
		t.Errorf("realizeCalls = %d, want 1 — the fresh resolve must still realize", src.realizeCalls)
	}
	if res.Harness.Version != "2.1.220" {
		t.Errorf("Version = %q, want the self-heal to preserve the SAME version 2.1.220 (nothing new to advance to, only the raw lockfile needed patching)", res.Harness.Version)
	}

	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		t.Fatalf("loading persisted lock: %v", err)
	}
	lh, ok := lockDoc.Harnesses["claude"]
	if !ok {
		t.Fatal("no persisted lock entry for claude")
	}
	if lh.NPMLock == nil {
		t.Fatal("persisted entry has no NPMLock")
	}
	if !lh.NPMLock.RawComplete() {
		t.Error("persisted entry's raw lockfile is still incomplete after the self-heal — the fix did not take effect")
	}
	if err := lockDoc.Validate(); err != nil {
		t.Errorf("persisted lock fails Validate: %v", err)
	}
}

// TestUpdateHarnessLockedDoesNotResolveWhenRawLockfileIsComplete is
// TestUpdateHarnessLockedSelfHealsIncompleteRawLockfile's control: an
// otherwise-identical, fully agreeing lock whose raw lockfile IS complete
// must still take the ordinary strict short-circuit (no Resolve call) — the
// self-heal is specific to the raw-incomplete case, not a general regression
// that now always re-resolves on install.
func TestUpdateHarnessLockedDoesNotResolveWhenRawLockfileIsComplete(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"

	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	wantRecipe, err := npmRecipeRevision(pkg, "")
	if err != nil {
		t.Fatal(err)
	}

	completeNPMLock := &NPMLock{
		LockfileVersion: 3,
		Lockfile:        json.RawMessage(patchedPiShapedRawLockfile),
		Packages: map[string]NPMLockPackage{
			"node_modules/" + pkg: {Version: "2.1.220", Resolved: "https://registry.npmjs.org/x.tgz", Integrity: "sha512-AAAA==", SHA256: "deadbeef"},
		},
	}
	if !completeNPMLock.RawComplete() {
		t.Fatal("test setup bug: patchedPiShapedRawLockfile must be RawComplete()==true")
	}

	writeLock(t, paths, &Lock{
		SchemaVersion: LockSchemaVersion, GeneratedAt: FormatTimestamp(fixedNow.Add(-time.Hour)), PolicyHash: "sha256:x",
		Harnesses: map[string]LockHarness{
			"claude": {
				Source: npmSourceName, Package: pkg, Version: "2.1.220",
				PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
				RecipeRevision: wantRecipe,
				NPMLock:        completeNPMLock,
				Generations:    map[PlatformKey]string{},
			},
		},
	})

	src := &fakeSource{}
	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, // install (Advance: false)
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if src.resolveCalls != 0 {
		t.Errorf("resolveCalls = %d, want 0 — a complete raw lockfile must take the ordinary strict short-circuit", src.resolveCalls)
	}
	// A realize DOES still run (no generation recorded yet for this
	// platform), but on the EXISTING entry, never a freshly resolved one.
	if res.Changed {
		t.Error("Changed = true, want false — nothing on the resolve axis needed to change")
	}
}
