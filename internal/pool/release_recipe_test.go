package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestReleaseRecipeRevisionHandBuiltPayload(t *testing.T) {
	// Independent of the production struct: the exact JSON the golden
	// value must be the SHA-256 of.
	payload := `{"repo":"can1357/oh-my-pi","version_feed":"@oh-my-pi/pi-coding-agent","tag_scheme":"v{version}",` +
		`"assets":[{"platform":"linux-amd64-glibc","asset":"omp-linux-x64"},{"platform":"linux-arm64-glibc","asset":"omp-linux-arm64"}],` +
		`"checksums":"SHA256SUMS.txt","release_recipe_format_version":1}`
	sum := sha256.Sum256([]byte(payload))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if want != goldenReleaseRecipeRevision {
		t.Errorf("hand-built payload digest %s != golden %s", want, goldenReleaseRecipeRevision)
	}
	if releaseTagScheme != "v{version}" {
		t.Errorf("releaseTagScheme = %q", releaseTagScheme)
	}
}

func TestRecipeRevisionForSource_ReleaseRejectsIncompleteRecipe(t *testing.T) {
	tests := map[string]*ReleaseRecipe{
		"no assets":    {VersionFeed: releaseTestFeed, Checksums: "SHA256SUMS.txt"},
		"no checksums": {VersionFeed: releaseTestFeed, Assets: sampleReleaseAssets()},
		"no feed":      {Assets: sampleReleaseAssets(), Checksums: "SHA256SUMS.txt"},
	}
	for name, rr := range tests {
		if _, err := recipeRevisionForSource(SourceRelease, releaseTestRepo, "", "", rr); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
	full := &ReleaseRecipe{VersionFeed: releaseTestFeed, Assets: sampleReleaseAssets(), Checksums: "s"}
	if _, err := recipeRevisionForSource(SourceRelease, "", "", "", full); err == nil {
		t.Error("empty repo: want error, got nil")
	}
}

func TestUpdateHarnessLockedReleaseWithoutRecipeErrorsBeforeMutation(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	src := &fakeSource{}
	_, err := UpdateHarnessLocked(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: releaseTestRepo, Source: SourceRelease, Now: fixedNow,
	})
	if err == nil || !strings.Contains(err.Error(), "release recipe inputs") {
		t.Fatalf("err = %v, want missing release recipe inputs error", err)
	}
	if src.resolveCalls != 0 {
		t.Errorf("resolveCalls = %d, want 0", src.resolveCalls)
	}
	for _, f := range []string{paths.Lock, paths.Observations} {
		if _, statErr := os.Stat(f); !os.IsNotExist(statErr) {
			t.Errorf("%s exists after a failed call (stat err = %v)", f, statErr)
		}
	}
}
