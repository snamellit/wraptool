package pool

import (
	"os"
	"path/filepath"
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

	o.Observe(key, "2026-07-29T12:10:00Z", "2026-07-29T12:00:00Z", false, false)
	if got := o[key].FirstSeenAt; got != "2026-07-29T12:10:00Z" {
		t.Fatalf("initial FirstSeenAt = %q, want 2026-07-29T12:10:00Z", got)
	}

	// A later merge with an earlier claimed first-seen time must not move it
	// backward...
	o.Observe(key, "2020-01-01T00:00:00Z", "2026-07-29T12:00:00Z", false, false)
	if got := o[key].FirstSeenAt; got != "2026-07-29T12:10:00Z" {
		t.Fatalf("FirstSeenAt moved backward: %q, want unchanged 2026-07-29T12:10:00Z", got)
	}

	// ...nor forward.
	o.Observe(key, "2030-01-01T00:00:00Z", "2026-07-29T12:00:00Z", false, false)
	if got := o[key].FirstSeenAt; got != "2026-07-29T12:10:00Z" {
		t.Fatalf("FirstSeenAt moved forward: %q, want unchanged 2026-07-29T12:10:00Z", got)
	}
}

func TestObservationsUpdatesMutableFields(t *testing.T) {
	o := Observations{}
	key := ObservationKey("npm", "claude", "2.1.220")

	o.Observe(key, "2026-07-29T12:10:00Z", "2026-07-29T12:00:00Z", false, false)
	if o[key].Yanked {
		t.Fatal("expected not yanked initially")
	}

	// A later observation can flip yanked/prerelease and update published_at,
	// but first_seen_at must still be preserved.
	o.Observe(key, "irrelevant", "2026-07-29T12:00:00Z", true, true)
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
	o.Observe(key, "2026-07-29T12:10:00Z", "2026-07-29T12:00:00Z", false, false)

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
	o.Observe("k", "t1", "t2", false, false)
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
	existing.Observe("npm/claude/1.0.0", "2026-01-01T00:00:00Z", "2025-12-31T00:00:00Z", false, false)
	if err := existing.Save(path); err != nil {
		t.Fatalf("Save (seed): %v", err)
	}

	partial := Observations{}
	partial.Observe("npm/claude/2.0.0", "2026-07-29T12:10:00Z", "2026-07-29T12:00:00Z", false, false)
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
	existing.Observe("npm/claude/1.0.0", "2026-01-01T00:00:00Z", "2025-12-31T00:00:00Z", false, false)
	if err := existing.Save(path); err != nil {
		t.Fatalf("Save (seed): %v", err)
	}

	partial := Observations{}
	// A later refresh re-observes the same version; a naive implementation
	// might claim "first seen now".
	partial.Observe("npm/claude/1.0.0", "2027-01-01T00:00:00Z", "2025-12-31T00:00:00Z", false, true)
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
	partial.Observe("npm/claude/1.0.0", "2026-07-29T12:10:00Z", "2026-07-29T12:00:00Z", false, false)
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
