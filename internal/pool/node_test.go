package pool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// nodeIndexServer starts an httptest.Server serving a fixed nodejs.org/dist/
// index.json body (entries: version -> date, both plain strings) at
// "/index.json" — hermetic, so RefreshNodeLine's real HTTP fetch never
// leaves this test (design/harness/node-runtime-tracking.md WP1; "internal/
// pool tests use hermetic HTTP test servers — no test may hit the real
// nodejs.org or npm registry").
func nodeIndexServer(t *testing.T, entries map[string]string) *httptest.Server {
	t.Helper()
	var body []map[string]string
	for version, date := range entries {
		body = append(body, map[string]string{"version": version, "date": date})
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshaling fixture node index: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRefreshNodeLineQuarantinesYoungReleases is deliverable-required test 1:
// a node patch younger than minimum_release_age is not selected by
// selectNodeVersion; an older one on the same line is.
func TestRefreshNodeLineQuarantinesYoungReleases(t *testing.T) {
	paths := testPaths(t)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	srv := nodeIndexServer(t, map[string]string{
		"v22.20.0": "2026-08-19", // published 1 day ago: too young
		"v22.18.0": "2026-01-01", // published well over 168h ago: eligible
		"v20.10.0": "2026-01-01", // a different line entirely: must be ignored
	})

	if _, err := RefreshNodeLine(ctxT(t), paths, NodeOptions{BaseURL: srv.URL}, "22", now); err != nil {
		t.Fatalf("RefreshNodeLine: %v", err)
	}

	obs, err := LoadObservations(paths.Observations)
	if err != nil {
		t.Fatalf("LoadObservations: %v", err)
	}
	policy := EffectivePolicy{MinimumReleaseAge: 168 * time.Hour}

	version, reason, err := selectNodeVersion(obs, "22", policy, now)
	if err != nil {
		t.Fatalf("selectNodeVersion: %v", err)
	}
	if version != "22.18.0" {
		t.Errorf("selected version = %q, want 22.18.0 (22.20.0 is quarantined, 20.10.0 is a different line)", version)
	}
	if reason == "" {
		t.Error("expected a non-empty selection reason")
	}
}

// TestRefreshNodeLineFirstSeenWriteOnceAcrossRefreshes is deliverable-
// required test 2: first_seen_at is write-once across two refreshes;
// published_at updates on re-observation.
func TestRefreshNodeLineFirstSeenWriteOnceAcrossRefreshes(t *testing.T) {
	paths := testPaths(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(72 * time.Hour)

	srv1 := nodeIndexServer(t, map[string]string{"v22.18.0": "2025-12-01"})
	if _, err := RefreshNodeLine(ctxT(t), paths, NodeOptions{BaseURL: srv1.URL}, "22", t0); err != nil {
		t.Fatalf("first RefreshNodeLine: %v", err)
	}

	// Re-observed at t1 with a DIFFERENT reported release date — published_at
	// must move to the new value (Observe overwrites it on every merge, by
	// design), but first_seen_at must stay t0.
	srv2 := nodeIndexServer(t, map[string]string{"v22.18.0": "2025-12-15"})
	if _, err := RefreshNodeLine(ctxT(t), paths, NodeOptions{BaseURL: srv2.URL}, "22", t1); err != nil {
		t.Fatalf("second RefreshNodeLine: %v", err)
	}

	obs, err := LoadObservations(paths.Observations)
	if err != nil {
		t.Fatalf("LoadObservations: %v", err)
	}
	key := nodeObservationKey("22", "22.18.0")
	got, ok := obs[key]
	if !ok {
		t.Fatalf("no observation recorded for %s", key)
	}
	wantFirstSeen := FormatTimestamp(t0)
	if got.FirstSeenAt != wantFirstSeen {
		t.Errorf("FirstSeenAt = %q, want %q (write-once — must not move to the second refresh's clock)", got.FirstSeenAt, wantFirstSeen)
	}
	wantPublished := FormatTimestamp(time.Date(2025, 12, 15, 0, 0, 0, 0, time.UTC))
	if got.PublishedAt != wantPublished {
		t.Errorf("PublishedAt = %q, want %q (published_at is NOT write-once — must reflect the re-observed date)", got.PublishedAt, wantPublished)
	}
}

// TestSelectNodeVersionFallsBackToNewestObservedWhenNoneEligible is
// deliverable 3's fallback rule: with observations present but none
// eligible (all quarantined), selectNodeVersion falls back to the newest
// OBSERVED patch rather than erroring.
func TestSelectNodeVersionFallsBackToNewestObservedWhenNoneEligible(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	obs := Observations{}
	// Both candidates published too recently under a 168h policy.
	if err := obs.Observe(nodeObservationKey("22", "22.20.0"), FormatTimestamp(now.Add(-2*time.Hour)), FormatTimestamp(now.Add(-2*time.Hour)), false, false, false); err != nil {
		t.Fatal(err)
	}
	if err := obs.Observe(nodeObservationKey("22", "22.19.0"), FormatTimestamp(now.Add(-1*time.Hour)), FormatTimestamp(now.Add(-1*time.Hour)), false, false, false); err != nil {
		t.Fatal(err)
	}
	policy := EffectivePolicy{MinimumReleaseAge: 168 * time.Hour}

	version, reason, err := selectNodeVersion(obs, "22", policy, now)
	if err != nil {
		t.Fatalf("selectNodeVersion: %v", err)
	}
	if version != "22.20.0" {
		t.Errorf("selected version = %q, want the newest OBSERVED 22.20.0 (nothing eligible)", version)
	}
	if reason == "" {
		t.Error("expected a non-empty fallback reason")
	}
}

// TestSelectNodeVersionErrorsWhenNothingObserved is deliverable 3's error
// path: a fresh pool (or refresh never run) has no observations for the
// line at all, so selectNodeVersion must error, actionably naming
// `wraptool harness refresh` — never silently pin an old hardcoded version
// (the MAJOR-2 guarantee: what reaches Source.Resolve is always an exact
// published version or an explicit error, never a bare major/fallback
// constant).
func TestSelectNodeVersionErrorsWhenNothingObserved(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	_, _, err := selectNodeVersion(Observations{}, "22", EffectivePolicy{}, now)
	if err == nil {
		t.Fatal("expected an error when nothing has ever been observed for the line")
	}
	if got := err.Error(); !strings.Contains(got, "22") || !strings.Contains(got, "wraptool harness refresh") {
		t.Errorf("error = %q, want it to name the line and `wraptool harness refresh`", got)
	}
}

// TestRefreshNodeLineIgnoresOtherLines proves RefreshNodeLine only records
// candidates on the requested line — a sibling major present in the same
// index.json response must not pollute this line's observations (and, by
// extension, must never influence selectNodeVersion for it).
func TestRefreshNodeLineIgnoresOtherLines(t *testing.T) {
	paths := testPaths(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	srv := nodeIndexServer(t, map[string]string{
		"v22.18.0": "2025-12-01",
		"v24.1.0":  "2025-12-01",
	})
	if _, err := RefreshNodeLine(ctxT(t), paths, NodeOptions{BaseURL: srv.URL}, "22", now); err != nil {
		t.Fatalf("RefreshNodeLine: %v", err)
	}
	obs, err := LoadObservations(paths.Observations)
	if err != nil {
		t.Fatalf("LoadObservations: %v", err)
	}
	if _, ok := obs[nodeObservationKey("22", "22.18.0")]; !ok {
		t.Error("expected line 22's own candidate to be recorded")
	}
	if _, ok := obs[nodeObservationKey("22", "24.1.0")]; ok {
		t.Error("line 24's candidate must not be recorded under line 22's refresh")
	}
	if _, ok := obs[nodeObservationKey("24", "24.1.0")]; ok {
		t.Error("a refresh scoped to line 22 must not record line 24's candidate at all")
	}
}

// TestDefaultNodeLineUsedWhenUpdateOptionsNodeLineEmpty confirms
// UpdateOptions.NodeLine's documented default: an empty NodeLine resolves
// against defaultNodeLine, not an unresolved/empty line.
func TestDefaultNodeLineUsedWhenUpdateOptionsNodeLineEmpty(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	src := &fakeSource{}
	// UpdateOptions.NodeLine deliberately left empty.
	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected a fresh resolve")
	}
	if src.lastResolve.nodeVersion != testNodeVersion {
		t.Errorf("nodeVersion passed to Resolve = %q, want seedObservations' default-line candidate %q", src.lastResolve.nodeVersion, testNodeVersion)
	}
}
