package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
)

// --- Refresh -----------------------------------------------------------

// newPackumentServer serves a canned npm registry packument (the full
// document, not abbreviated) at every path, so the test package name is
// irrelevant to routing.
func newPackumentServer(t *testing.T, timeMap map[string]string, versions []string) *httptest.Server {
	t.Helper()
	versionsMap := make(map[string]json.RawMessage, len(versions))
	for _, v := range versions {
		versionsMap[v] = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(map[string]interface{}{
		"time":     timeMap,
		"versions": versionsMap,
	})
	if err != nil {
		t.Fatalf("marshaling fixture packument: %v", err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
}

// newRawPackumentServer serves body verbatim, for fixtures that need a
// "time" map with non-string values (npm's "unpublished" key) that
// newPackumentServer's map[string]string signature cannot express.
func newRawPackumentServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
}

func candidateByVersion(t *testing.T, cs []pool.Candidate, version string) pool.Candidate {
	t.Helper()
	for _, c := range cs {
		if c.Version == version {
			return c
		}
	}
	t.Fatalf("no candidate for version %q among %d candidates", version, len(cs))
	return pool.Candidate{}
}

func TestRefresh(t *testing.T) {
	srv := newPackumentServer(t, map[string]string{
		"created":      "2019-01-01T00:00:00.000Z",
		"modified":     "2024-01-01T00:00:00.000Z",
		"1.0.0":        "2020-01-01T00:00:00.000Z",
		"1.1.0":        "2020-06-01T00:00:00.000Z",
		"2.0.0-beta.1": "2021-01-01T00:00:00.000Z",
		"1.2.0":        "2022-01-01T00:00:00.000Z", // unpublished: absent from "versions" below
	}, []string{"1.0.0", "1.1.0", "2.0.0-beta.1", "1.3.0"}) // 1.3.0: no time entry
	defer srv.Close()

	n := NewNPM(srv.URL)
	candidates, err := n.Refresh(context.Background(), "example-pkg")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(candidates) != 5 {
		t.Fatalf("got %d candidates, want 5: %+v", len(candidates), candidates)
	}

	for _, c := range candidates {
		if !c.FirstSeenAt.IsZero() {
			t.Errorf("candidate %s: FirstSeenAt = %v, want zero (orchestrator fills it)", c.Version, c.FirstSeenAt)
		}
	}

	t.Run("normal version", func(t *testing.T) {
		c := candidateByVersion(t, candidates, "1.0.0")
		want := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		if !c.PublishedAt.Equal(want) {
			t.Errorf("PublishedAt = %v, want %v", c.PublishedAt, want)
		}
		if c.Prerelease {
			t.Error("Prerelease = true, want false")
		}
		if c.Yanked {
			t.Error("Yanked = true, want false")
		}
	})

	t.Run("prerelease", func(t *testing.T) {
		c := candidateByVersion(t, candidates, "2.0.0-beta.1")
		if !c.Prerelease {
			t.Error("Prerelease = false, want true")
		}
		if c.Yanked {
			t.Error("Yanked = true, want false")
		}
	})

	t.Run("unpublished (yanked)", func(t *testing.T) {
		c := candidateByVersion(t, candidates, "1.2.0")
		if !c.Yanked {
			t.Error("Yanked = false, want true (present in time, absent from versions)")
		}
		want := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
		if !c.PublishedAt.Equal(want) {
			t.Errorf("PublishedAt = %v, want %v (yanked candidates still carry their publish time)", c.PublishedAt, want)
		}
	})

	t.Run("missing time entry", func(t *testing.T) {
		c := candidateByVersion(t, candidates, "1.3.0")
		if !c.PublishedAt.IsZero() {
			t.Errorf("PublishedAt = %v, want zero (no time entry — must not invent one)", c.PublishedAt)
		}
		if c.Yanked {
			t.Error("Yanked = true, want false (present in versions, just missing a time entry)")
		}
	})
}

// TestRefresh_SkipsNonStringTimeEntries locks in that a packument whose
// "time" map contains npm's nested "unpublished" object (not a string) does
// not fail the whole Refresh — only that one non-version key is skipped.
func TestRefresh_SkipsNonStringTimeEntries(t *testing.T) {
	body := []byte(`{
		"time": {
			"created": "2019-01-01T00:00:00.000Z",
			"modified": "2024-01-01T00:00:00.000Z",
			"unpublished": {
				"maintainers": [],
				"name": "example-pkg",
				"time": "2024-01-01T00:00:00.000Z",
				"versions": ["9.9.9"]
			},
			"1.0.0": "2020-01-01T00:00:00.000Z"
		},
		"versions": {
			"1.0.0": {}
		}
	}`)
	srv := newRawPackumentServer(t, body)
	defer srv.Close()

	n := NewNPM(srv.URL)
	candidates, err := n.Refresh(context.Background(), "example-pkg")
	if err != nil {
		t.Fatalf("Refresh: %v (a nested non-string time entry must not fail the whole packument decode)", err)
	}
	if len(candidates) != 1 {
		t.Fatalf(`got %d candidates, want 1 ("unpublished" is not a version): %+v`, len(candidates), candidates)
	}
	if candidates[0].Version != "1.0.0" {
		t.Errorf("Version = %q, want 1.0.0", candidates[0].Version)
	}
}

// --- Resolve -------------------------------------------------------------

// fakeBackend is a backend.Backend test double: it records every RunSpec it
// receives and, if lockJSON is set, writes it as package-lock.json into the
// first mount's host directory — standing in for what a real `npm install
// --package-lock-only` would produce.
type fakeBackend struct {
	lockJSON []byte
	runErr   error
	calls    []backend.RunSpec
}

func (f *fakeBackend) Name() string    { return "fake" }
func (f *fakeBackend) Available() bool { return true }

func (f *fakeBackend) Run(_ context.Context, spec backend.RunSpec) error {
	f.calls = append(f.calls, spec)
	if f.runErr != nil {
		return f.runErr
	}
	if f.lockJSON == nil {
		return nil
	}
	if len(spec.Mounts) == 0 {
		return fmt.Errorf("fakeBackend: RunSpec has no mounts")
	}
	return os.WriteFile(filepath.Join(spec.Mounts[0].Host, "package-lock.json"), f.lockJSON, 0o644)
}

func newNodeDistServer(t *testing.T, shasums string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "SHASUMS256.txt") {
			w.Write([]byte(shasums))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

const canonicalPackageLock = `{
  "name": "resolve-scratch",
  "version": "0.0.0",
  "lockfileVersion": 3,
  "packages": {
    "": {"name": "resolve-scratch", "version": "0.0.0"},
    "node_modules/@anthropic-ai/claude-code": {
      "version": "2.1.220",
      "resolved": "https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-2.1.220.tgz",
      "integrity": "sha512-AAAA=="
    },
    "node_modules/leftpad": {
      "version": "1.0.0",
      "resolved": "https://registry.npmjs.org/leftpad/-/leftpad-1.0.0.tgz",
      "integrity": "sha512-BBBB=="
    }
  }
}`

const shasums = "abc1111111111111111111111111111111111111111111111111111111111111  node-v24.18.1-linux-x64.tar.xz\ndef2222222222222222222222222222222222222222222222222222222222222  node-v24.18.1-linux-arm64.tar.xz\n"

func TestResolve(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{
		"2.1.220": "2026-07-29T12:00:00.000Z",
	}, []string{"2.1.220"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL

	fb := &fakeBackend{lockJSON: []byte(canonicalPackageLock)}
	workDir := t.TempDir()

	lh, err := n.Resolve(context.Background(), fb, "@anthropic-ai/claude-code", "2.1.220", workDir, "24.18.1", "sha256:recipe-rev")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	t.Run("RunSpec", func(t *testing.T) {
		if len(fb.calls) != 1 {
			t.Fatalf("got %d backend.Run calls, want 1", len(fb.calls))
		}
		spec := fb.calls[0]
		wantArgv := []string{"npm", "install", "--package-lock-only", "--ignore-scripts"}
		if len(spec.Argv) != len(wantArgv) {
			t.Fatalf("Argv = %v, want %v", spec.Argv, wantArgv)
		}
		for i := range wantArgv {
			if spec.Argv[i] != wantArgv[i] {
				t.Fatalf("Argv = %v, want %v", spec.Argv, wantArgv)
			}
		}
		if !spec.Network {
			t.Error("Network = false, want true (resolve needs registry access, §6.2)")
		}
		if len(spec.Mounts) != 1 {
			t.Fatalf("got %d mounts, want 1", len(spec.Mounts))
		}
		if spec.Mounts[0].ReadOnly {
			t.Error("scratch mount is read-only, want read-write")
		}
		if spec.Mounts[0].Host == "" || spec.Mounts[0].Container == "" {
			t.Errorf("mount has empty Host/Container: %+v", spec.Mounts[0])
		}
	})

	t.Run("LockHarness fields", func(t *testing.T) {
		if lh.Source != "npm" {
			t.Errorf("Source = %q, want npm", lh.Source)
		}
		if lh.Package != "@anthropic-ai/claude-code" {
			t.Errorf("Package = %q", lh.Package)
		}
		if lh.Version != "2.1.220" {
			t.Errorf("Version = %q", lh.Version)
		}
		wantPublished := pool.FormatTimestamp(time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
		if lh.PublishedAt != wantPublished {
			t.Errorf("PublishedAt = %q, want %q", lh.PublishedAt, wantPublished)
		}
		wantZero := pool.FormatTimestamp(time.Time{})
		if lh.FirstSeenAt != wantZero {
			t.Errorf("FirstSeenAt = %q, want zero-formatted %q (orchestrator fills it)", lh.FirstSeenAt, wantZero)
		}
		if lh.RecipeRevision != "sha256:recipe-rev" {
			t.Errorf("RecipeRevision = %q", lh.RecipeRevision)
		}
	})

	t.Run("Runtime artifacts", func(t *testing.T) {
		if lh.Runtime == nil {
			t.Fatal("Runtime is nil")
		}
		if lh.Runtime.Name != "node" || lh.Runtime.Version != "24.18.1" {
			t.Errorf("Runtime = %+v", lh.Runtime)
		}
		if len(lh.Runtime.Artifacts) != 2 {
			t.Fatalf("got %d artifacts, want 2 (glibc only, musl deferred): %+v", len(lh.Runtime.Artifacts), lh.Runtime.Artifacts)
		}
		amd64, ok := lh.Runtime.Artifacts[pool.PlatformLinuxAMD64Glibc]
		if !ok {
			t.Fatal("missing linux-amd64-glibc artifact")
		}
		if !strings.HasPrefix(amd64.SHA256, "abc1111") {
			t.Errorf("amd64 sha256 = %q, want it to start with abc1111", amd64.SHA256)
		}
		if !strings.Contains(amd64.URL, "node-v24.18.1-linux-x64.tar.xz") {
			t.Errorf("amd64 URL = %q", amd64.URL)
		}
		arm64, ok := lh.Runtime.Artifacts[pool.PlatformLinuxARM64Glibc]
		if !ok {
			t.Fatal("missing linux-arm64-glibc artifact")
		}
		if !strings.HasPrefix(arm64.SHA256, "def2222") {
			t.Errorf("arm64 sha256 = %q, want it to start with def2222", arm64.SHA256)
		}
		if _, ok := lh.Runtime.Artifacts[pool.PlatformLinuxAMD64Musl]; ok {
			t.Error("linux-amd64-musl artifact present, want absent (musl deferred, §5.1)")
		}
	})

	t.Run("NPMLock conversion", func(t *testing.T) {
		if lh.NPMLock == nil {
			t.Fatal("NPMLock is nil")
		}
		if lh.NPMLock.LockfileVersion != 3 {
			t.Errorf("LockfileVersion = %d, want 3", lh.NPMLock.LockfileVersion)
		}
		if _, ok := lh.NPMLock.Packages[""]; ok {
			t.Error(`Packages contains "" (the synthesized root entry), want it excluded`)
		}
		if len(lh.NPMLock.Packages) != 2 {
			t.Fatalf("got %d packages, want 2: %+v", len(lh.NPMLock.Packages), lh.NPMLock.Packages)
		}
		claude, ok := lh.NPMLock.Packages["node_modules/@anthropic-ai/claude-code"]
		if !ok {
			t.Fatal("missing claude-code package entry")
		}
		if claude.Resolved != "https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-2.1.220.tgz" {
			t.Errorf("Resolved = %q", claude.Resolved)
		}
		if claude.Integrity != "sha512-AAAA==" {
			t.Errorf("Integrity = %q", claude.Integrity)
		}
		if claude.SHA256 != "" {
			t.Errorf("SHA256 = %q, want empty (Fetch fills it later)", claude.SHA256)
		}
	})

	t.Run("Lockfile is the verbatim, compacted package-lock.json", func(t *testing.T) {
		if len(lh.NPMLock.Lockfile) == 0 {
			t.Fatal("Lockfile is empty, want the verbatim package-lock.json bytes")
		}
		var compacted bytes.Buffer
		if err := json.Compact(&compacted, []byte(canonicalPackageLock)); err != nil {
			t.Fatalf("compacting fixture for comparison: %v", err)
		}
		if string(lh.NPMLock.Lockfile) != compacted.String() {
			t.Errorf("Lockfile = %s, want the compacted fixture %s", lh.NPMLock.Lockfile, compacted.String())
		}
		if bytes.ContainsAny(lh.NPMLock.Lockfile, "\n\t") {
			t.Errorf("Lockfile contains whitespace, want a compacted single-line form: %s", lh.NPMLock.Lockfile)
		}
	})
}

// resolveFixture builds a minimal lockJSON with one top-level entry matching
// pkg/version (so the top-level cross-check passes) plus whatever extra
// packages entries the caller supplies as raw JSON snippets.
func resolveFixture(pkg, version string, extraPackagesJSON string) string {
	topKey := "node_modules/" + pkg
	return fmt.Sprintf(`{
  "lockfileVersion": 3,
  "packages": {
    "": {"name": "resolve-scratch"},
    %q: {"version": %q, "resolved": "https://registry.npmjs.org/%s/-/%s-%s.tgz", "integrity": "sha512-AAAA=="}
    %s
  }
}`, topKey, version, pkg, pkg, version, extraPackagesJSON)
}

func TestResolve_RejectsGitDependency(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{"1.0.0": "2026-01-01T00:00:00.000Z"}, []string{"1.0.0"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	// A git dependency's package-lock.json entry carries "resolved" (a
	// git+https URL) but no "integrity". Its resolved URL is not on the
	// configured registry, so the integrity backfill must refuse it rather
	// than try to look it up.
	lockJSON := resolveFixture("example", "1.0.0", `,
    "node_modules/git-dep": {"version": "1.0.0", "resolved": "git+https://github.com/foo/bar.git#abcdef0"}`)

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want one naming the unlockable entry")
	}
	if !strings.Contains(err.Error(), "node_modules/git-dep") {
		t.Errorf("error = %q, want it to name node_modules/git-dep", err.Error())
	}
	// Pinning the exact message (not just the entry name) is what makes this
	// test fail if the non-registry guard in backfillIntegrity were dropped —
	// the git+https URL would then be sent to the registry as a packument
	// request and fail with an unrelated fetch error.
	if !strings.Contains(err.Error(), "but no integrity (git or non-registry dependency cannot be locked)") {
		t.Errorf("error = %q, want the git/non-registry no-integrity message", err.Error())
	}
}

// TestResolve_RejectsIntegrityWithoutResolved covers an entry that has
// Integrity but no Resolved. This shape doesn't occur naturally in a real
// package-lock.json (no known npm dependency kind produces it), but a
// mutation that deleted the Resolved check would pass every other test in
// this file while silently accepting an entry like this one — so it is
// exercised directly, on the belt-and-suspenders principle, rather than left
// to only ever be caught in production.
func TestResolve_RejectsIntegrityWithoutResolved(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{"1.0.0": "2026-01-01T00:00:00.000Z"}, []string{"1.0.0"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	lockJSON := resolveFixture("example", "1.0.0", `,
    "node_modules/no-resolved": {"version": "1.0.0", "integrity": "sha512-AAAA=="}`)

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want one naming the entry missing resolved")
	}
	if !strings.Contains(err.Error(), "node_modules/no-resolved") {
		t.Errorf("error = %q, want it to name node_modules/no-resolved", err.Error())
	}
	if !strings.Contains(err.Error(), "has no resolved URL") {
		t.Errorf("error = %q, want it to contain \"has no resolved URL\"", err.Error())
	}
}

// newRoutedRegistryServer serves per-package packument bodies keyed by
// package name, for tests whose backfill path must fetch a different
// packument than the top-level package's. Go's http server decodes the %2f
// that registryURL percent-encodes into scoped package names, so the routing
// key is the decoded path without its leading slash.
func newRoutedRegistryServer(t *testing.T, packuments map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := packuments[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
}

// TestResolve_BackfillsMissingIntegrityFromRegistry covers the
// shrinkwrap-published shape (@earendil-works/pi-coding-agent and its
// workspace siblings): a nested entry with a registry resolved URL but no
// integrity must be locked with the registry's own dist.integrity for that
// exact name@version, not rejected.
func TestResolve_BackfillsMissingIntegrityFromRegistry(t *testing.T) {
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	const siblingIntegrity = "sha512-CCCC=="
	registrySrv := newRoutedRegistryServer(t, map[string]string{
		"example": `{
			"time": {"1.0.0": "2026-01-01T00:00:00.000Z"},
			"versions": {"1.0.0": {}}
		}`,
		"@scope/sibling": fmt.Sprintf(`{
			"time": {"2.0.0": "2026-01-01T00:00:00.000Z"},
			"versions": {"2.0.0": {"dist": {"integrity": %q}}}
		}`, siblingIntegrity),
	})
	defer registrySrv.Close()

	// The sibling entry mirrors what npm writes when honoring a bundled
	// npm-shrinkwrap.json: nested under the shipping package, resolved on the
	// registry, no integrity.
	lockJSON := resolveFixture("example", "1.0.0", fmt.Sprintf(`,
    "node_modules/example/node_modules/@scope/sibling": {"version": "2.0.0", "resolved": "%s/@scope/sibling/-/sibling-2.0.0.tgz"}`,
		registrySrv.URL))

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	lh, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err != nil {
		t.Fatalf("Resolve: %v (a registry-resolved entry missing integrity must be backfilled, not rejected)", err)
	}
	got, ok := lh.NPMLock.Packages["node_modules/example/node_modules/@scope/sibling"]
	if !ok {
		t.Fatal("backfilled entry missing from NPMLock.Packages")
	}
	if got.Integrity != siblingIntegrity {
		t.Errorf("Integrity = %q, want %q (backfilled from the registry's dist.integrity)", got.Integrity, siblingIntegrity)
	}
}

// TestResolve_BackfillFailsWhenRegistryLacksIntegrity locks in that the
// backfill never invents an integrity: a registry-resolved entry whose
// packument version carries no dist.integrity stays a hard, entry-naming
// error.
func TestResolve_BackfillFailsWhenRegistryLacksIntegrity(t *testing.T) {
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	registrySrv := newRoutedRegistryServer(t, map[string]string{
		"example": `{
			"time": {"1.0.0": "2026-01-01T00:00:00.000Z"},
			"versions": {"1.0.0": {}}
		}`,
		"bare-dist": `{
			"time": {"2.0.0": "2026-01-01T00:00:00.000Z"},
			"versions": {"2.0.0": {"dist": {"shasum": "c32c9ea53b34df5eec69186cc9795c1ce122f308"}}}
		}`,
	})
	defer registrySrv.Close()

	lockJSON := resolveFixture("example", "1.0.0", fmt.Sprintf(`,
    "node_modules/bare-dist": {"version": "2.0.0", "resolved": "%s/bare-dist/-/bare-dist-2.0.0.tgz"}`,
		registrySrv.URL))

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want backfill failure when the registry has no dist.integrity")
	}
	if !strings.Contains(err.Error(), "node_modules/bare-dist") {
		t.Errorf("error = %q, want it to name node_modules/bare-dist", err.Error())
	}
	if !strings.Contains(err.Error(), "no integrity") {
		t.Errorf("error = %q, want it to mention the missing integrity", err.Error())
	}
}

// piSiblingVersion is the sibling entry's version in piShapedLockJSON —
// deliberately distinct from the top-level package's own version, and
// matching the registry fixture packument's "1.5.0" version key in the tests
// below.
const piSiblingVersion = "1.5.0"

// piShapedLockJSON builds a resolveFixture-style package-lock.json shaped
// like the live @earendil-works/pi-tui bug: a nested entry with a resolved
// URL but NO integrity, plus engines/dependencies/license — fields the typed
// npmLockFile struct doesn't know about — alongside a sibling entry that
// ALREADY has integrity and carries os/cpu/optional (also unknown to
// npmLockFile). registryURL is embedded in the sibling's "resolved" so
// backfillIntegrity's registry-host check passes.
func piShapedLockJSON(pkg, version, registryURL string) string {
	return resolveFixture(pkg, version, fmt.Sprintf(`,
    "node_modules/%[1]s/node_modules/@parent/native": {
      "version": "9.9.9",
      "resolved": "%[2]s/@parent/native/-/native-9.9.9.tgz",
      "integrity": "sha512-PARENTINTEGRITY==",
      "os": ["linux"],
      "cpu": ["x64"],
      "optional": true
    },
    "node_modules/%[1]s/node_modules/@earendil-works/pi-tui": {
      "version": %[3]q,
      "resolved": "%[2]s/@earendil-works/pi-tui/-/pi-tui-%[3]s.tgz",
      "engines": {"node": ">=18"},
      "dependencies": {"leftpad": "^1.0.0"},
      "license": "MIT"
    }`, pkg, registryURL, piSiblingVersion))
}

// TestResolve_BackfillPatchesRawLockfilePreservingUnknownFields is the
// pi-coding-agent regression test (npm-raw-lockfile-backfill work package): a
// nested entry shaped like pi-tui's shrinkwrap-copied sibling package
// (resolved, engines, dependencies, license — NO integrity) must have its
// backfilled integrity land in the RETURNED NPMLock.Lockfile itself, not
// just the NPMLock.Packages summary map — Realize materializes Lockfile
// verbatim as package-lock.json, and a realize-side `npm ci --offline`
// matches its offline cache by integrity, so a raw entry still missing it
// fails realize with an opaque ENOTCACHED even though Fetch/the summary map
// were already correct. This also asserts the sibling entry's own
// unknown-to-npmLockFile fields (engines/dependencies/license) AND an
// unrelated parent entry's unknown fields (os/cpu/optional) survive the
// patch untouched — proving the patch is a generic, single-field
// map[string]any edit, never a round trip through the typed struct.
func TestResolve_BackfillPatchesRawLockfilePreservingUnknownFields(t *testing.T) {
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	const siblingIntegrity = "sha512-PITUIINTEGRITY=="
	registrySrv := newRoutedRegistryServer(t, map[string]string{
		"example": `{
			"time": {"1.0.0": "2026-01-01T00:00:00.000Z"},
			"versions": {"1.0.0": {}}
		}`,
		"@earendil-works/pi-tui": fmt.Sprintf(`{
			"time": {"1.5.0": "2026-01-01T00:00:00.000Z"},
			"versions": {"1.5.0": {"dist": {"integrity": %q}}}
		}`, siblingIntegrity),
	})
	defer registrySrv.Close()

	lockJSON := piShapedLockJSON("example", "1.0.0", registrySrv.URL)
	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	lh, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	const siblingKey = "node_modules/example/node_modules/@earendil-works/pi-tui"
	const parentKey = "node_modules/example/node_modules/@parent/native"

	// --- summary map carries the backfilled integrity ---
	sibling, ok := lh.NPMLock.Packages[siblingKey]
	if !ok {
		t.Fatal("backfilled sibling entry missing from NPMLock.Packages")
	}
	if sibling.Integrity != siblingIntegrity {
		t.Errorf("summary Integrity = %q, want %q", sibling.Integrity, siblingIntegrity)
	}

	// --- the RETURNED raw Lockfile parses and carries the same integrity ---
	var raw struct {
		LockfileVersion int                        `json:"lockfileVersion"`
		Packages        map[string]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(lh.NPMLock.Lockfile, &raw); err != nil {
		t.Fatalf("parsing returned Lockfile: %v", err)
	}
	if raw.LockfileVersion != 3 {
		t.Errorf("raw Lockfile lockfileVersion = %d, want 3", raw.LockfileVersion)
	}

	rawSiblingBytes, ok := raw.Packages[siblingKey]
	if !ok {
		t.Fatal("raw Lockfile missing the sibling entry entirely")
	}
	var rawSibling struct {
		Version      string            `json:"version"`
		Resolved     string            `json:"resolved"`
		Integrity    string            `json:"integrity"`
		Engines      map[string]string `json:"engines"`
		Dependencies map[string]string `json:"dependencies"`
		License      string            `json:"license"`
	}
	if err := json.Unmarshal(rawSiblingBytes, &rawSibling); err != nil {
		t.Fatalf("parsing raw sibling entry: %v", err)
	}
	if rawSibling.Integrity != siblingIntegrity {
		t.Errorf("raw Lockfile sibling integrity = %q, want %q (the ENOTCACHED bug: raw document never patched)", rawSibling.Integrity, siblingIntegrity)
	}
	if rawSibling.Version != "1.5.0" {
		t.Errorf("raw sibling version = %q, want 1.5.0 (unknown field lost across the patch)", rawSibling.Version)
	}
	if rawSibling.Engines["node"] != ">=18" {
		t.Errorf("raw sibling engines = %+v, want node >=18 (unknown field lost across the patch)", rawSibling.Engines)
	}
	if rawSibling.Dependencies["leftpad"] != "^1.0.0" {
		t.Errorf("raw sibling dependencies = %+v, want leftpad ^1.0.0 (unknown field lost across the patch)", rawSibling.Dependencies)
	}
	if rawSibling.License != "MIT" {
		t.Errorf("raw sibling license = %q, want MIT (unknown field lost across the patch)", rawSibling.License)
	}

	// --- the UNRELATED parent entry's unknown fields survive untouched ---
	rawParentBytes, ok := raw.Packages[parentKey]
	if !ok {
		t.Fatal("raw Lockfile missing the parent entry entirely")
	}
	var rawParent struct {
		Integrity string   `json:"integrity"`
		OS        []string `json:"os"`
		CPU       []string `json:"cpu"`
		Optional  bool     `json:"optional"`
	}
	if err := json.Unmarshal(rawParentBytes, &rawParent); err != nil {
		t.Fatalf("parsing raw parent entry: %v", err)
	}
	if rawParent.Integrity != "sha512-PARENTINTEGRITY==" {
		t.Errorf("raw parent integrity = %q, want unchanged sha512-PARENTINTEGRITY==", rawParent.Integrity)
	}
	if len(rawParent.OS) != 1 || rawParent.OS[0] != "linux" {
		t.Errorf("raw parent os = %v, want [linux] (unknown field lost/mutated)", rawParent.OS)
	}
	if len(rawParent.CPU) != 1 || rawParent.CPU[0] != "x64" {
		t.Errorf("raw parent cpu = %v, want [x64] (unknown field lost/mutated)", rawParent.CPU)
	}
	if !rawParent.Optional {
		t.Error("raw parent optional = false, want true (unknown field lost/mutated)")
	}

	// --- summary map and raw document agree on the backfilled entry ---
	if sibling.Integrity != rawSibling.Integrity {
		t.Errorf("summary Integrity %q != raw Lockfile integrity %q", sibling.Integrity, rawSibling.Integrity)
	}
}

// TestResolve_BackfilledLockfileIsDeterministic covers the determinism
// requirement on a backfilled lock specifically (TestResolve's own "Lockfile
// is the verbatim, compacted package-lock.json" subtest already covers the
// unpatched path): two Resolve calls against the identical fixture must
// produce byte-identical NPMLock.Lockfile output, since (*LockHarness).Hash
// depends on it (§17 "deterministic lock serialization").
func TestResolve_BackfilledLockfileIsDeterministic(t *testing.T) {
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	const siblingIntegrity = "sha512-PITUIINTEGRITY=="
	registrySrv := newRoutedRegistryServer(t, map[string]string{
		"example": `{
			"time": {"1.0.0": "2026-01-01T00:00:00.000Z"},
			"versions": {"1.0.0": {}}
		}`,
		"@earendil-works/pi-tui": fmt.Sprintf(`{
			"time": {"1.5.0": "2026-01-01T00:00:00.000Z"},
			"versions": {"1.5.0": {"dist": {"integrity": %q}}}
		}`, siblingIntegrity),
	})
	defer registrySrv.Close()

	lockJSON := piShapedLockJSON("example", "1.0.0", registrySrv.URL)
	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL

	var lockfiles [][]byte
	for i := 0; i < 3; i++ {
		fb := &fakeBackend{lockJSON: []byte(lockJSON)}
		lh, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
		if err != nil {
			t.Fatalf("Resolve run %d: %v", i, err)
		}
		lockfiles = append(lockfiles, lh.NPMLock.Lockfile)
	}
	for i := 1; i < len(lockfiles); i++ {
		if !bytes.Equal(lockfiles[0], lockfiles[i]) {
			t.Errorf("Lockfile run %d differs from run 0:\n%s\nvs\n%s", i, lockfiles[i], lockfiles[0])
		}
	}
}

// TestPatchLockfileIntegrity_PreservesLargeNumbersAndDecimals is the
// UseNumber() regression test: an untouched numeric field elsewhere in the
// raw document — one beyond float64's exact-integer range (2^53), and a
// decimal — must survive patchLockfileIntegrity byte-for-byte. Before
// UseNumber(), decoding into map[string]any would silently convert both to
// float64 and re-Marshal could reformat them (dropping precision on the
// large integer, or changing the decimal's textual shape), which would also
// break Lockfile determinism across two Resolve calls whose registry
// response happened to arrive with different (but numerically equal)
// formatting.
func TestPatchLockfileIntegrity_PreservesLargeNumbersAndDecimals(t *testing.T) {
	const largeInt = "9007199254740993" // 2^53 + 1: not exactly representable as float64
	const decimal = "1.50000"           // a decimal whose trailing zeros a float64 round trip would drop
	raw := []byte(fmt.Sprintf(`{
  "lockfileVersion": 3,
  "packages": {
    "": {"dependencies": {}},
    "node_modules/untouched": {
      "version": "1.0.0",
      "resolved": "https://registry.npmjs.org/untouched.tgz",
      "integrity": "sha512-UNTOUCHED==",
      "someLargeCount": %s,
      "someDecimal": %s
    },
    "node_modules/to-patch": {
      "version": "1.0.0",
      "resolved": "https://registry.npmjs.org/to-patch.tgz"
    }
  }
}`, largeInt, decimal))

	patched, err := patchLockfileIntegrity(raw, map[string]string{
		"node_modules/to-patch": "sha512-PATCHED==",
	})
	if err != nil {
		t.Fatalf("patchLockfileIntegrity: %v", err)
	}

	var doc struct {
		Packages map[string]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(patched, &doc); err != nil {
		t.Fatalf("parsing patched lockfile: %v", err)
	}

	// The patched entry got its integrity.
	var patchedEntry struct {
		Integrity string `json:"integrity"`
	}
	if err := json.Unmarshal(doc.Packages["node_modules/to-patch"], &patchedEntry); err != nil {
		t.Fatalf("parsing patched entry: %v", err)
	}
	if patchedEntry.Integrity != "sha512-PATCHED==" {
		t.Errorf("patched entry Integrity = %q, want sha512-PATCHED==", patchedEntry.Integrity)
	}

	// The UNTOUCHED entry's numeric fields survive with their exact original
	// text — decoded via json.Number, not float64, so this string comparison
	// is meaningful (a float64 round trip could still coincidentally print
	// the same text for small numbers, but not for largeInt or a
	// trailing-zero decimal like decimal above).
	var untouched struct {
		SomeLargeCount json.Number `json:"someLargeCount"`
		SomeDecimal    json.Number `json:"someDecimal"`
	}
	if err := json.Unmarshal(doc.Packages["node_modules/untouched"], &untouched); err != nil {
		t.Fatalf("parsing untouched entry: %v", err)
	}
	if untouched.SomeLargeCount.String() != largeInt {
		t.Errorf("someLargeCount = %s, want %s byte-preserved (2^53+1 is not exactly representable as float64)", untouched.SomeLargeCount, largeInt)
	}
	if untouched.SomeDecimal.String() != decimal {
		t.Errorf("someDecimal = %s, want %s byte-preserved (trailing zeros)", untouched.SomeDecimal, decimal)
	}
}

// TestResolve_RecordsHasShrinkwrapWhenAnyEntryCarriesIt covers the
// pi-coding-agent root-cause finding: npm marks a package that bundles its
// own npm-shrinkwrap.json with "hasShrinkwrap": true on the CONSUMER
// lockfile's entry for it. Resolve must succeed (this is a realize
// limitation, not a resolve one — see NPMLock.HasShrinkwrap's doc comment)
// and record NPMLock.HasShrinkwrap true, surviving a real persisted-lock
// Marshal/UnmarshalLock round trip.
func TestResolve_RecordsHasShrinkwrapWhenAnyEntryCarriesIt(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{"1.0.0": "2026-01-01T00:00:00.000Z"}, []string{"1.0.0"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	lockJSON := resolveFixture("example", "1.0.0", `,
    "node_modules/example/node_modules/shrinkwrapped-dep": {"version": "3.0.0", "resolved": "https://registry.npmjs.org/shrinkwrapped-dep/-/shrinkwrapped-dep-3.0.0.tgz", "integrity": "sha512-CCCC==", "hasShrinkwrap": true}`)

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	lh, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err != nil {
		t.Fatalf("Resolve: %v (a shrinkwrapped entry must not fail Resolve — this is a realize limitation)", err)
	}
	if !lh.NPMLock.HasShrinkwrap {
		t.Fatal("NPMLock.HasShrinkwrap = false, want true (an entry in the lockfile carries hasShrinkwrap)")
	}

	// Persisted round trip: the flag must survive a real Lock
	// Marshal/UnmarshalLock cycle, not just live on the in-memory
	// LockHarness Resolve returns. FirstSeenAt is stamped here since Resolve
	// deliberately leaves it zero-formatted (the orchestrator's job,
	// per Resolve's own doc comment) and Validate rejects a zero timestamp.
	lh.FirstSeenAt = pool.FormatTimestamp(time.Now().UTC())
	lockDoc := &pool.Lock{
		SchemaVersion: pool.LockSchemaVersion,
		GeneratedAt:   pool.FormatTimestamp(time.Now().UTC()),
		PolicyHash:    "sha256:" + strings.Repeat("a", 64),
		Harnesses:     map[string]pool.LockHarness{"example": lh},
	}
	data, err := lockDoc.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := pool.UnmarshalLock(data)
	if err != nil {
		t.Fatalf("UnmarshalLock: %v", err)
	}
	if !got.Harnesses["example"].NPMLock.HasShrinkwrap {
		t.Error("round-tripped NPMLock.HasShrinkwrap = false, want true")
	}
}

// TestResolve_HasShrinkwrapFalseWhenNoEntryCarriesIt is the control: an
// ordinary lockfile with no shrinkwrapped entries must leave
// NPMLock.HasShrinkwrap false.
func TestResolve_HasShrinkwrapFalseWhenNoEntryCarriesIt(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{
		"2.1.220": "2026-07-29T12:00:00.000Z",
	}, []string{"2.1.220"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(canonicalPackageLock)}

	lh, err := n.Resolve(context.Background(), fb, "@anthropic-ai/claude-code", "2.1.220", t.TempDir(), "24.18.1", "sha256:recipe-rev")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lh.NPMLock.HasShrinkwrap {
		t.Error("NPMLock.HasShrinkwrap = true, want false (no entry in this fixture carries hasShrinkwrap)")
	}
}

// TestResolve_ReportsFirstUnlockableEntryDeterministically locks in the
// sorted-key iteration: with several unlockable entries, the error must name
// the lexicographically first one on every run, not whichever Go's random
// map order visits first. Repeated runs make a regression to map-order
// iteration overwhelmingly likely to surface.
func TestResolve_ReportsFirstUnlockableEntryDeterministically(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{"1.0.0": "2026-01-01T00:00:00.000Z"}, []string{"1.0.0"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	lockJSON := resolveFixture("example", "1.0.0", `,
    "node_modules/aa-git-dep": {"version": "1.0.0", "resolved": "git+https://github.com/foo/aa.git#abcdef0"},
    "node_modules/zz-git-dep": {"version": "1.0.0", "resolved": "git+https://github.com/foo/zz.git#abcdef0"}`)

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	for i := 0; i < 8; i++ {
		_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
		if err == nil {
			t.Fatal("Resolve: got nil error, want one naming an unlockable entry")
		}
		if !strings.Contains(err.Error(), "node_modules/aa-git-dep") {
			t.Fatalf("run %d: error = %q, want it to name node_modules/aa-git-dep (the lexicographically first unlockable entry)", i, err.Error())
		}
	}
}

func TestResolve_RejectsWorkspaceLink(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{"1.0.0": "2026-01-01T00:00:00.000Z"}, []string{"1.0.0"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	// A workspace link entry carries "resolved" (a relative workspace path)
	// and "link": true, and no "integrity" — reject it explicitly on the
	// link marker, not just on missing integrity.
	lockJSON := resolveFixture("example", "1.0.0", `,
    "node_modules/workspace-pkg": {"resolved": "../workspace-pkg", "link": true}`)

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want one naming the workspace link entry")
	}
	if !strings.Contains(err.Error(), "node_modules/workspace-pkg") {
		t.Errorf("error = %q, want it to name node_modules/workspace-pkg", err.Error())
	}
	// Pinning the exact message is what makes this test fail if the
	// dedicated Link check were deleted — this fixture's entry has a
	// non-empty Resolved and an empty Integrity, so without the Link check
	// it would still be caught by the resolved-or-integrity check, just with
	// a different, misleading message.
	if !strings.Contains(err.Error(), "is a workspace link") {
		t.Errorf("error = %q, want it to contain \"is a workspace link\"", err.Error())
	}
}

func TestResolve_RejectsSHA1Integrity(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{"1.0.0": "2026-01-01T00:00:00.000Z"}, []string{"1.0.0"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	lockJSON := resolveFixture("example", "1.0.0", `,
    "node_modules/old-pkg": {"version": "1.0.0", "resolved": "https://registry.npmjs.org/old-pkg/-/old-pkg-1.0.0.tgz", "integrity": "sha1-AAAAAAAAAAAAAAAAAAAAAAAAAAA="}`)

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want rejection of sha1 integrity")
	}
	if !strings.Contains(err.Error(), "node_modules/old-pkg") || !strings.Contains(err.Error(), "sha1") {
		t.Errorf("error = %q, want it to name node_modules/old-pkg and mention sha1", err.Error())
	}
}

func TestResolve_RejectsMultiHashIntegrity(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{"1.0.0": "2026-01-01T00:00:00.000Z"}, []string{"1.0.0"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	lockJSON := resolveFixture("example", "1.0.0", `,
    "node_modules/multi-hash": {"version": "1.0.0", "resolved": "https://registry.npmjs.org/multi-hash/-/multi-hash-1.0.0.tgz", "integrity": "sha512-AAAA== sha256-BBBB=="}`)

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want rejection of multi-hash SRI")
	}
	if !strings.Contains(err.Error(), "multi-hash") {
		t.Errorf("error = %q, want it to mention multi-hash", err.Error())
	}
}

func TestResolve_RejectsWrongLockfileVersion(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{"1.0.0": "2026-01-01T00:00:00.000Z"}, []string{"1.0.0"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	lockJSON := `{"lockfileVersion": 2, "packages": {}}`
	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want one rejecting lockfileVersion 2")
	}
	if !strings.Contains(err.Error(), "lockfileVersion") {
		t.Errorf("error = %q, want it to mention lockfileVersion", err.Error())
	}
}

func TestResolve_RequiresTopLevelEntryMatchingRequestedVersion(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{"1.0.0": "2026-01-01T00:00:00.000Z"}, []string{"1.0.0"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	// No "node_modules/example" entry at all.
	lockJSON := `{
  "lockfileVersion": 3,
  "packages": {
    "": {"name": "resolve-scratch"},
    "node_modules/other": {"version": "1.0.0", "resolved": "https://registry.npmjs.org/other/-/other-1.0.0.tgz", "integrity": "sha512-AAAA=="}
  }
}`
	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want rejection when the top-level package is missing from the lockfile")
	}
	if !strings.Contains(err.Error(), "node_modules/example") {
		t.Errorf("error = %q, want it to name node_modules/example", err.Error())
	}
}

func TestResolve_RequiresPublishedAt(t *testing.T) {
	// The registry reports 1.0.0 in "versions" but has no "time" entry for
	// it at all.
	registrySrv := newPackumentServer(t, map[string]string{}, []string{"1.0.0"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	lockJSON := resolveFixture("example", "1.0.0", "")
	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want rejection when the registry has no time entry for the resolved version")
	}
	if !strings.Contains(err.Error(), "no time entry") {
		t.Errorf("error = %q, want it to mention the missing time entry", err.Error())
	}
}

func TestResolve_RejectsMalformedNodeDigest(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{"1.0.0": "2026-01-01T00:00:00.000Z"}, []string{"1.0.0"})
	defer registrySrv.Close()
	badShasums := "../../etc/passwd  node-v24.18.1-linux-x64.tar.xz\n" +
		"def2222222222222222222222222222222222222222222222222222222222222  node-v24.18.1-linux-arm64.tar.xz\n"
	nodeSrv := newNodeDistServer(t, badShasums)
	defer nodeSrv.Close()

	lockJSON := resolveFixture("example", "1.0.0", "")
	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want rejection of a malformed SHASUMS256.txt digest")
	}
	if !strings.Contains(err.Error(), "malformed digest") {
		t.Errorf("error = %q, want it to mention a malformed digest", err.Error())
	}
}

func TestResolve_StripsLeadingVFromNodeVersion(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{"1.0.0": "2026-01-01T00:00:00.000Z"}, []string{"1.0.0"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	lockJSON := resolveFixture("example", "1.0.0", "")
	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	lh, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "v24.18.1", "sha256:rev")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lh.Runtime.Version != "24.18.1" {
		t.Errorf("Runtime.Version = %q, want 24.18.1 (leading v stripped)", lh.Runtime.Version)
	}
	amd64 := lh.Runtime.Artifacts[pool.PlatformLinuxAMD64Glibc]
	if strings.Contains(amd64.URL, "vv24.18.1") {
		t.Errorf("URL = %q contains a doubled v prefix", amd64.URL)
	}
	if !strings.Contains(amd64.URL, "v24.18.1") {
		t.Errorf("URL = %q, want it to contain v24.18.1", amd64.URL)
	}
}

// newPackumentServerWithVersionBody serves a fixed packument whose "time"
// map has one entry for version (an arbitrary, always-parsable timestamp —
// these tests are about engines.node, not published_at) and whose
// "versions" map has version -> versionBody verbatim — checkEnginesNode's
// test fixture, since newPackumentServer's map[string]string signature
// cannot express a non-trivial version manifest (an "engines" object).
func newPackumentServerWithVersionBody(t *testing.T, version, versionBody string) *httptest.Server {
	t.Helper()
	body := fmt.Sprintf(`{"time":{%q:"2026-01-01T00:00:00.000Z"},"versions":{%q:%s}}`, version, version, versionBody)
	return newRawPackumentServer(t, []byte(body))
}

// TestResolve_FetchesPackumentOnlyOnce is review NIT 8's required test:
// versionPublishedAt and checkEnginesNode both need pkg's packument, but
// share the SAME per-Resolve-call cache (the packuments map already
// threaded through backfillIntegrity) — one Resolve call must hit the
// registry's packument endpoint exactly once per package, not once per
// caller.
func TestResolve_FetchesPackumentOnlyOnce(t *testing.T) {
	var requests int32
	body := []byte(`{"time":{"1.0.0":"2026-01-01T00:00:00.000Z"},"versions":{"1.0.0":{"engines":{"node":">=20"}}}}`)
	registrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	lockJSON := resolveFixture("example", "1.0.0", "")
	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	if _, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("packument endpoint hit %d times, want exactly 1 (versionPublishedAt and checkEnginesNode must share one cache)", got)
	}
}

// TestResolve_EnginesNodeSatisfiedPasses is deliverable-required test 1
// (design/harness/node-runtime-tracking.md WP2 item 5): a package version
// whose engines.node is satisfied by the resolved node version must not be
// rejected.
func TestResolve_EnginesNodeSatisfiedPasses(t *testing.T) {
	registrySrv := newPackumentServerWithVersionBody(t, "1.0.0", `{"engines":{"node":">=20"}}`)
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	lockJSON := resolveFixture("example", "1.0.0", "")
	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	if _, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev"); err != nil {
		t.Fatalf("Resolve: %v, want success (node 24.18.1 satisfies >=20)", err)
	}
}

// TestResolve_EnginesNodeViolatedFailsActionably is deliverable-required
// test 2: a package version whose engines.node the resolved node version
// violates must fail Resolve, naming the package version, its engines
// constraint, the resolved node version, and the Node line it resolved on
// (design doc item 5's exact wording) — worded as a wraptool code change,
// never an instruction to edit a Go field name (review NIT 7).
func TestResolve_EnginesNodeViolatedFailsActionably(t *testing.T) {
	registrySrv := newPackumentServerWithVersionBody(t, "1.0.0", `{"engines":{"node":">=99"}}`)
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	lockJSON := resolveFixture("example", "1.0.0", "")
	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want rejection when node fails the package's engines.node constraint")
	}
	for _, want := range []string{"example@1.0.0", ">=99", "24.18.1", "line 24", "wraptool code change"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	}
}

// TestResolve_EnginesNodeMissingOrGarbageIsNonEvent is deliverable-required
// test 3: a missing engines field, an engines object with no "node" key, or
// an engines.node value this package's semver dialect cannot parse must
// never fail Resolve — the design doc's "A missing or unparsable engines
// field is a non-event."
func TestResolve_EnginesNodeMissingOrGarbageIsNonEvent(t *testing.T) {
	cases := []struct {
		name        string
		versionBody string
	}{
		{"no engines field at all", `{}`},
		{"engines present but no node key", `{"engines":{"npm":">=8"}}`},
		{"engines.node is not valid semver-range syntax", `{"engines":{"node":"lol not a range"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registrySrv := newPackumentServerWithVersionBody(t, "1.0.0", tc.versionBody)
			defer registrySrv.Close()
			nodeSrv := newNodeDistServer(t, shasums)
			defer nodeSrv.Close()

			lockJSON := resolveFixture("example", "1.0.0", "")
			n := NewNPM(registrySrv.URL)
			n.nodeDistBaseURL = nodeSrv.URL
			fb := &fakeBackend{lockJSON: []byte(lockJSON)}

			if _, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev"); err != nil {
				t.Fatalf("Resolve: %v, want success (missing/unparsable engines.node must be a non-event)", err)
			}
		})
	}
}

// --- Fetch -----------------------------------------------------------------

func sriFor(content []byte) string {
	sum := sha512.Sum512(content)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func TestFetch_NPMPackage(t *testing.T) {
	content := []byte("tarball bytes for the npm package")
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Write(content)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	lh := &pool.LockHarness{
		NPMLock: &pool.NPMLock{
			LockfileVersion: 3,
			Packages: map[string]pool.NPMLockPackage{
				"node_modules/pkg": {
					Version:   "1.0.0",
					Resolved:  srv.URL + "/pkg.tgz",
					Integrity: sriFor(content),
				},
			},
		},
	}

	n := NewNPM("")
	if err := n.Fetch(context.Background(), lh, cacheDir, allTargets); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}

	wantDigest := sha256Hex(content)
	got := lh.NPMLock.Packages["node_modules/pkg"]
	if got.SHA256 != wantDigest {
		t.Errorf("Sha256 = %q, want %q (filled in from empty)", got.SHA256, wantDigest)
	}

	cached, err := os.ReadFile(filepath.Join(cacheDir, wantDigest))
	if err != nil {
		t.Fatalf("reading cached object: %v", err)
	}
	if !bytes.Equal(cached, content) {
		t.Error("cached object content does not match downloaded content")
	}
}

func TestFetch_SRIMismatchAdmitsNothing(t *testing.T) {
	content := []byte("actual bytes served")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	lh := &pool.LockHarness{
		NPMLock: &pool.NPMLock{
			Packages: map[string]pool.NPMLockPackage{
				"node_modules/pkg": {
					Resolved:  srv.URL + "/pkg.tgz",
					Integrity: sriFor([]byte("different bytes entirely")),
				},
			},
		},
	}

	n := NewNPM("")
	err := n.Fetch(context.Background(), lh, cacheDir, allTargets)
	if err == nil {
		t.Fatal("Fetch: got nil error, want SRI mismatch")
	}
	if !strings.Contains(err.Error(), "node_modules/pkg") {
		t.Errorf("error = %q, want it to name node_modules/pkg", err.Error())
	}

	entries, rerr := os.ReadDir(cacheDir)
	if rerr != nil {
		t.Fatalf("reading cache dir: %v", rerr)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".wraptool-pool-fetch-") {
			t.Errorf("cache dir contains %q after a mismatch — nothing should be admitted", e.Name())
		}
	}
	// The mismatched digest must specifically not exist in the cache.
	if _, err := os.Stat(filepath.Join(cacheDir, sha256Hex(content))); err == nil {
		t.Error("mismatched object's digest exists in cache, want absent")
	}
}

func TestFetch_SHA256VerifyAndMismatch(t *testing.T) {
	content := []byte("runtime artifact bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()
	digest := sha256Hex(content)

	t.Run("matches recorded sha256", func(t *testing.T) {
		cacheDir := t.TempDir()
		lh := &pool.LockHarness{
			Runtime: &pool.LockRuntime{
				Artifacts: map[pool.PlatformKey]pool.LockRuntimeArtifact{
					pool.PlatformLinuxAMD64Glibc: {URL: srv.URL + "/node.tar.xz", SHA256: digest},
				},
			},
		}
		n := NewNPM("")
		if err := n.Fetch(context.Background(), lh, cacheDir, allTargets); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
	})

	t.Run("mismatches recorded sha256", func(t *testing.T) {
		cacheDir := t.TempDir()
		lh := &pool.LockHarness{
			Runtime: &pool.LockRuntime{
				Artifacts: map[pool.PlatformKey]pool.LockRuntimeArtifact{
					pool.PlatformLinuxAMD64Glibc: {URL: srv.URL + "/node.tar.xz", SHA256: "0000000000000000000000000000000000000000000000000000000000000000"},
				},
			},
		}
		n := NewNPM("")
		err := n.Fetch(context.Background(), lh, cacheDir, allTargets)
		if err == nil {
			t.Fatal("Fetch: got nil error, want sha256 mismatch")
		}
		if !strings.Contains(err.Error(), string(pool.PlatformLinuxAMD64Glibc)) {
			t.Errorf("error = %q, want it to name the platform key", err.Error())
		}
	})
}

func TestFetch_RejectsMalformedSHA256(t *testing.T) {
	lh := &pool.LockHarness{
		NPMLock: &pool.NPMLock{
			Packages: map[string]pool.NPMLockPackage{
				"node_modules/pkg": {
					Resolved:  "http://example.invalid/pkg.tgz",
					Integrity: "sha512-AAAA==",
					SHA256:    "../../etc/passwd",
				},
			},
		},
	}

	n := NewNPM("")
	err := n.Fetch(context.Background(), lh, t.TempDir(), allTargets)
	if err == nil {
		t.Fatal("Fetch: got nil error, want rejection of a malformed sha256 before any request")
	}
	if !strings.Contains(err.Error(), "malformed sha256") {
		t.Errorf("error = %q, want it to mention a malformed sha256", err.Error())
	}
}

func TestFetch_RejectsUnverifiableNPMEntry(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Write([]byte("should never be fetched"))
	}))
	defer srv.Close()

	lh := &pool.LockHarness{
		NPMLock: &pool.NPMLock{
			Packages: map[string]pool.NPMLockPackage{
				// Neither Integrity nor SHA256 set: there is nothing to
				// verify the download against, so Fetch must refuse before
				// making any request — independent of whatever Resolve's
				// own validation already guarantees (defence in depth).
				"node_modules/unverifiable": {Resolved: srv.URL + "/pkg.tgz"},
			},
		},
	}

	n := NewNPM("")
	err := n.Fetch(context.Background(), lh, t.TempDir(), allTargets)
	if err == nil {
		t.Fatal("Fetch: got nil error, want rejection of an entry with neither integrity nor sha256")
	}
	if !strings.Contains(err.Error(), "node_modules/unverifiable") {
		t.Errorf("error = %q, want it to name node_modules/unverifiable", err.Error())
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0 (must reject before any download)", requests)
	}
}

func TestFetch_RejectsRuntimeArtifactWithoutSHA256(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Write([]byte("should never be fetched"))
	}))
	defer srv.Close()

	lh := &pool.LockHarness{
		Runtime: &pool.LockRuntime{
			Artifacts: map[pool.PlatformKey]pool.LockRuntimeArtifact{
				pool.PlatformLinuxAMD64Glibc: {URL: srv.URL + "/node.tar.xz"},
			},
		},
	}

	n := NewNPM("")
	err := n.Fetch(context.Background(), lh, t.TempDir(), allTargets)
	if err == nil {
		t.Fatal("Fetch: got nil error, want rejection of a runtime artifact with no recorded sha256")
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0 (must reject before any download)", requests)
	}
}

func TestFetch_CacheHitSkipsDownload(t *testing.T) {
	content := []byte("already cached content")
	digest := sha256Hex(content)
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Write(content)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, digest), content, 0o644); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	lh := &pool.LockHarness{
		NPMLock: &pool.NPMLock{
			Packages: map[string]pool.NPMLockPackage{
				"node_modules/pkg": {
					Resolved:  srv.URL + "/pkg.tgz",
					Integrity: sriFor(content),
					SHA256:    digest,
				},
			},
		},
	}

	n := NewNPM("")
	if err := n.Fetch(context.Background(), lh, cacheDir, allTargets); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0 (cache hit must skip the download)", requests)
	}
}

// TestFetch_OnlyRequestsListedURLs asserts Fetch never issues a request for
// anything beyond the URLs already recorded on the LockHarness (§6.2: "A
// fetch of an unlisted URL is a bug, not a fallback"). Every request that
// reaches the server — including one for a path neither handler
// registers — is recorded by a catch-all wrapper *before* dispatch, so an
// unlisted request would show up in the recorded set even though the
// underlying mux would 404 it (a plain mux-registered-paths-only recorder
// would never see that request at all).
func TestFetch_OnlyRequestsListedURLs(t *testing.T) {
	pkgContent := []byte("pkg tarball")
	runtimeContent := []byte("node runtime tarball")

	var mu sync.Mutex
	var requested []string

	mux := http.NewServeMux()
	mux.HandleFunc("/pkg.tgz", func(w http.ResponseWriter, r *http.Request) {
		w.Write(pkgContent)
	})
	mux.HandleFunc("/node.tar.xz", func(w http.ResponseWriter, r *http.Request) {
		w.Write(runtimeContent)
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requested = append(requested, r.URL.Path)
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	lh := &pool.LockHarness{
		NPMLock: &pool.NPMLock{
			Packages: map[string]pool.NPMLockPackage{
				"node_modules/pkg": {Resolved: srv.URL + "/pkg.tgz", Integrity: sriFor(pkgContent)},
			},
		},
		Runtime: &pool.LockRuntime{
			Artifacts: map[pool.PlatformKey]pool.LockRuntimeArtifact{
				pool.PlatformLinuxAMD64Glibc: {URL: srv.URL + "/node.tar.xz", SHA256: sha256Hex(runtimeContent)},
			},
		},
	}

	n := NewNPM("")
	if err := n.Fetch(context.Background(), lh, t.TempDir(), allTargets); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), requested...)
	mu.Unlock()

	sort.Strings(got)
	want := []string{"/node.tar.xz", "/pkg.tgz"}
	if len(got) != len(want) {
		t.Fatalf("requested paths = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("requested paths = %v, want exactly %v", got, want)
		}
	}
}
