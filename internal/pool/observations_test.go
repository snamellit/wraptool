package pool

import (
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
