package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestObservationKeyFormat(t *testing.T) {
	got := ObservationKey("npm", "@anthropic-ai/claude-code", "2.1.220")
	want := "npm/@anthropic-ai/claude-code/2.1.220"
	if got != want {
		t.Errorf("ObservationKey() = %q, want %q", got, want)
	}
}

func TestObservationsFirstSeenNeverMoves(t *testing.T) {
	o := Observations{}
	key := ObservationKey("npm", "claude", "2.1.220")

	if err := o.Observe(key, "2026-07-29T12:10:00Z", "2026-07-29T12:00:00Z", false, false, false); err != nil {
		t.Fatalf("Observe (initial): %v", err)
	}
	if got := o[key].FirstSeenAt; got != "2026-07-29T12:10:00Z" {
		t.Fatalf("initial FirstSeenAt = %q, want 2026-07-29T12:10:00Z", got)
	}

	// A later merge with an earlier claimed first-seen time must not move it
	// backward...
	if err := o.Observe(key, "2020-01-01T00:00:00Z", "2026-07-29T12:00:00Z", false, false, false); err != nil {
		t.Fatalf("Observe (earlier claim): %v", err)
	}
	if got := o[key].FirstSeenAt; got != "2026-07-29T12:10:00Z" {
		t.Fatalf("FirstSeenAt moved backward: %q, want unchanged 2026-07-29T12:10:00Z", got)
	}

	// ...nor forward.
	if err := o.Observe(key, "2030-01-01T00:00:00Z", "2026-07-29T12:00:00Z", false, false, false); err != nil {
		t.Fatalf("Observe (later claim): %v", err)
	}
	if got := o[key].FirstSeenAt; got != "2026-07-29T12:10:00Z" {
		t.Fatalf("FirstSeenAt moved forward: %q, want unchanged 2026-07-29T12:10:00Z", got)
	}
}

func TestObservationsUpdatesMutableFields(t *testing.T) {
	o := Observations{}
	key := ObservationKey("npm", "claude", "2.1.220")

	if err := o.Observe(key, "2026-07-29T12:10:00Z", "2026-07-29T12:00:00Z", false, false, false); err != nil {
		t.Fatalf("Observe (initial): %v", err)
	}
	if o[key].Yanked {
		t.Fatal("expected not yanked initially")
	}

	// A later observation can flip yanked/prerelease and update published_at,
	// but first_seen_at must still be preserved. The firstSeenAt argument
	// here is a real, valid timestamp (Observe validates it even though it's
	// discarded for an existing key) but deliberately different from the
	// stored one, to prove it's ignored rather than adopted.
	if err := o.Observe(key, "2099-01-01T00:00:00Z", "2026-07-29T12:00:00Z", true, true, false); err != nil {
		t.Fatalf("Observe (update): %v", err)
	}
	got := o[key]
	if !got.Yanked {
		t.Error("expected Yanked to update to true on a later merge")
	}
	if !got.Prerelease {
		t.Error("expected Prerelease to update to true on a later merge")
	}
	if got.FirstSeenAt != "2026-07-29T12:10:00Z" {
		t.Errorf("FirstSeenAt = %q, want preserved 2026-07-29T12:10:00Z", got.FirstSeenAt)
	}
}

// TestObserveRejectsMalformedTimestamps covers N1: Observe validates both
// timestamp arguments on the write path and leaves the map unchanged on
// failure, mirroring LoadObservations' read-path check.
func TestObserveRejectsMalformedTimestamps(t *testing.T) {
	o := Observations{}

	if err := o.Observe("k", "not-a-timestamp", "2026-07-29T12:00:00Z", false, false, false); err == nil {
		t.Error("expected an error for a malformed first_seen_at, got nil")
	}
	if err := o.Observe("k", "2026-07-29T12:10:00Z", "not-a-timestamp", false, false, false); err == nil {
		t.Error("expected an error for a malformed published_at, got nil")
	}
	if len(o) != 0 {
		t.Errorf("a rejected Observe must not store anything, got %d entries: %+v", len(o), o)
	}
}

func TestLoadObservationsMissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	o, err := LoadObservations(filepath.Join(dir, "does-not-exist.json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(o) != 0 {
		t.Errorf("expected empty map for a missing file, got %d entries", len(o))
	}
}

func TestObservationsSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observations.json")

	o := Observations{}
	key := ObservationKey("npm", "claude", "2.1.220")
	if err := o.Observe(key, "2026-07-29T12:10:00Z", "2026-07-29T12:00:00Z", false, false, false); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	if err := o.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadObservations(path)
	if err != nil {
		t.Fatalf("LoadObservations: %v", err)
	}
	if got[key] != o[key] {
		t.Errorf("round-tripped observation = %+v, want %+v", got[key], o[key])
	}
}

func TestObservationsSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observations.json")

	o := Observations{}
	if err := o.Observe("k", "2026-07-29T12:10:00Z", "2026-07-29T12:00:00Z", false, false, false); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if err := o.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected exactly the final file after Save, found %v", entries)
	}
}

// TestLoadObservationsCorruptJSONNamesPathAndRemedy: a malformed
// observations.json (not valid JSON at all, distinct from a validly-shaped
// file with a bad timestamp) must name the file path and tell the caller how
// to recover — deleting it is always safe since observations.json is
// disposable pool-local cache state (§9), not the manifest or lock.
func TestLoadObservationsCorruptJSONNamesPathAndRemedy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observations.json")
	if err := os.WriteFile(path, []byte(`{not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadObservations(path)
	if err == nil {
		t.Fatal("expected an error for corrupt JSON, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name the file path %q", err.Error(), path)
	}
	if !strings.Contains(err.Error(), "delete it") {
		t.Errorf("error = %q, want it to suggest deleting the file as the remedy", err.Error())
	}
}

func TestLoadObservationsRejectsUnparsableTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observations.json")
	if err := os.WriteFile(path, []byte(`{"npm/claude/1.0.0":{"first_seen_at":"not-a-timestamp","published_at":"2026-07-29T12:00:00Z"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadObservations(path); err == nil {
		t.Fatal("expected an error for an unparsable first_seen_at, got nil")
	}
}

func TestLoadObservationsRejectsNonUTCTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observations.json")
	if err := os.WriteFile(path, []byte(`{"npm/claude/1.0.0":{"first_seen_at":"2026-07-29T12:00:00+02:00","published_at":"2026-07-29T12:00:00Z"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadObservations(path); err == nil {
		t.Fatal("expected an error for a non-UTC first_seen_at, got nil")
	}
}

// TestMergeObservationsPreservesUntouchedKeys covers MINOR 4: a caller that
// only has this refresh's candidates (a partial map) must not clobber keys
// from a previous refresh that this one didn't see.
func TestMergeObservationsPreservesUntouchedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observations.json")

	existing := Observations{}
	if err := existing.Observe("npm/claude/1.0.0", "2026-01-01T00:00:00Z", "2025-12-31T00:00:00Z", false, false, false); err != nil {
		t.Fatalf("Observe (seed): %v", err)
	}
	if err := existing.Save(path); err != nil {
		t.Fatalf("Save (seed): %v", err)
	}

	partial := Observations{}
	if err := partial.Observe("npm/claude/2.0.0", "2026-07-29T12:10:00Z", "2026-07-29T12:00:00Z", false, false, false); err != nil {
		t.Fatalf("Observe (partial): %v", err)
	}
	if err := MergeObservations(path, partial); err != nil {
		t.Fatalf("MergeObservations: %v", err)
	}

	got, err := LoadObservations(path)
	if err != nil {
		t.Fatalf("LoadObservations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both the untouched and the merged key to survive, got %d entries: %+v", len(got), got)
	}
	if got["npm/claude/1.0.0"].FirstSeenAt != "2026-01-01T00:00:00Z" {
		t.Errorf("untouched key's first_seen_at changed: got %+v", got["npm/claude/1.0.0"])
	}
	if got["npm/claude/2.0.0"].FirstSeenAt != "2026-07-29T12:10:00Z" {
		t.Errorf("merged key's first_seen_at wrong: got %+v", got["npm/claude/2.0.0"])
	}
}

// TestMergeObservationsPreservesFirstSeenOfKnownKey covers MINOR 4 + §6.6
// together: merging a partial map that re-observes an already-known key must
// still never move that key's first_seen_at.
func TestMergeObservationsPreservesFirstSeenOfKnownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observations.json")

	existing := Observations{}
	if err := existing.Observe("npm/claude/1.0.0", "2026-01-01T00:00:00Z", "2025-12-31T00:00:00Z", false, false, false); err != nil {
		t.Fatalf("Observe (seed): %v", err)
	}
	if err := existing.Save(path); err != nil {
		t.Fatalf("Save (seed): %v", err)
	}

	partial := Observations{}
	// A later refresh re-observes the same version; a naive implementation
	// might claim "first seen now".
	if err := partial.Observe("npm/claude/1.0.0", "2027-01-01T00:00:00Z", "2025-12-31T00:00:00Z", false, true, false); err != nil {
		t.Fatalf("Observe (partial): %v", err)
	}
	if err := MergeObservations(path, partial); err != nil {
		t.Fatalf("MergeObservations: %v", err)
	}

	got, err := LoadObservations(path)
	if err != nil {
		t.Fatalf("LoadObservations: %v", err)
	}
	if got["npm/claude/1.0.0"].FirstSeenAt != "2026-01-01T00:00:00Z" {
		t.Errorf("first_seen_at moved via MergeObservations: got %+v", got["npm/claude/1.0.0"])
	}
	if !got["npm/claude/1.0.0"].Yanked {
		t.Error("expected Yanked to update to true via MergeObservations")
	}
}

func TestMergeObservationsOnMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observations.json")

	partial := Observations{}
	if err := partial.Observe("npm/claude/1.0.0", "2026-07-29T12:10:00Z", "2026-07-29T12:00:00Z", false, false, false); err != nil {
		t.Fatalf("Observe (partial): %v", err)
	}
	if err := MergeObservations(path, partial); err != nil {
		t.Fatalf("MergeObservations on a missing file: %v", err)
	}

	got, err := LoadObservations(path)
	if err != nil {
		t.Fatalf("LoadObservations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
}

// TestMergeObservationsRejectsMalformedTimestampLeavesFileUntouched covers
// N1's required test directly: a single bad entry in the incoming partial
// map must not brick observations.json for every subsequent caller — the
// existing file must be left exactly as it was, not partially merged or
// wiped.
func TestMergeObservationsRejectsMalformedTimestampLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observations.json")

	seed := Observations{}
	if err := seed.Observe("npm/claude/1.0.0", "2026-01-01T00:00:00Z", "2025-12-31T00:00:00Z", false, false, false); err != nil {
		t.Fatalf("Observe (seed): %v", err)
	}
	if err := seed.Save(path); err != nil {
		t.Fatalf("Save (seed): %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	bad := Observations{"npm/claude/2.0.0": Observation{
		FirstSeenAt: "not-a-timestamp",
		PublishedAt: "2026-07-29T12:00:00Z",
	}}
	if err := MergeObservations(path, bad); err == nil {
		t.Fatal("expected an error for a malformed incoming timestamp, got nil")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("file changed after a rejected merge:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	got, err := LoadObservations(path)
	if err != nil {
		t.Fatalf("LoadObservations after rejected merge: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected only the seed entry to survive, got %d: %+v", len(got), got)
	}
}
