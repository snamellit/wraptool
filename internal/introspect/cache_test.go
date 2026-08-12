package introspect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/paths"
)

func isolateCacheDir(t *testing.T) {
	t.Helper()
	old, had := os.LookupEnv("XDG_CACHE_HOME")
	if err := os.Setenv("XDG_CACHE_HOME", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	xdg.Reload()
	t.Cleanup(func() {
		if had {
			os.Setenv("XDG_CACHE_HOME", old)
		} else {
			os.Unsetenv("XDG_CACHE_HOME")
		}
		xdg.Reload()
	})
}

func TestLoadCache_Invalidation(t *testing.T) {
	isolateCacheDir(t)

	now := time.Now().UTC()
	save := func(introspectedAt time.Time) {
		if err := SaveCache("git", &Schema{
			Binary:         "/x",
			ConfigHash:     "H",
			IntrospectedAt: introspectedAt,
			Subcommands:    map[string]Subcommand{},
		}); err != nil {
			t.Fatal(err)
		}
	}

	save(now)
	inputsOld := now.Add(-time.Hour)

	if LoadCache("git", CacheValidity{ConfigHash: "H", NewestInput: inputsOld, TTL: time.Hour}) == nil {
		t.Error("matching config hash + older inputs + within TTL should hit")
	}
	if LoadCache("git", CacheValidity{ConfigHash: "DIFFERENT", NewestInput: inputsOld, TTL: time.Hour}) != nil {
		t.Error("config hash change should invalidate")
	}
	if LoadCache("git", CacheValidity{ConfigHash: "H", NewestInput: now.Add(time.Minute), TTL: time.Hour}) != nil {
		t.Error("an input newer than the cache should invalidate (rebuilt binary / edited overrides)")
	}

	// TTL backstop: cache written two hours ago, TTL one hour → stale.
	save(now.Add(-2 * time.Hour))
	if LoadCache("git", CacheValidity{ConfigHash: "H", NewestInput: now.Add(-3 * time.Hour), TTL: time.Hour}) != nil {
		t.Error("TTL lapse should invalidate")
	}
}

func TestConfigHash(t *testing.T) {
	a := &config.ToolConfig{Binary: "/b", Allow: []config.AllowRule{{Subcommand: []string{"commit"}}}}
	if h1, h2 := configHash(a), configHash(a); h1 != h2 {
		t.Errorf("configHash must be stable for the same input: %q != %q", h1, h2)
	}
	// Adding a subcommand changes what gets introspected → hash must change.
	b := &config.ToolConfig{Binary: "/b", Allow: []config.AllowRule{
		{Subcommand: []string{"commit"}}, {Subcommand: []string{"status"}},
	}}
	if configHash(a) == configHash(b) {
		t.Error("adding an allowed subcommand should change the hash")
	}
	// Flags don't affect introspection (whole --help is parsed regardless).
	c := &config.ToolConfig{Binary: "/b", Allow: []config.AllowRule{{Subcommand: []string{"commit"}, Flags: []string{"--x"}}}}
	if configHash(a) != configHash(c) {
		t.Error("flag changes must NOT affect the config hash")
	}
	// Order of allow rules must not matter (sorted).
	d := &config.ToolConfig{Binary: "/b", Allow: []config.AllowRule{
		{Subcommand: []string{"status"}}, {Subcommand: []string{"commit"}},
	}}
	if configHash(b) != configHash(d) {
		t.Error("subcommand order must not affect the hash")
	}
}

func TestCacheStamp(t *testing.T) {
	isolateCacheDir(t)

	if _, _, ok := CacheStamp("git"); ok {
		t.Error("CacheStamp should report ok=false when no cache exists")
	}

	at := time.Now().UTC().Truncate(time.Second)
	if err := SaveCache("git", &Schema{ConfigHash: "H", IntrospectedAt: at, Subcommands: map[string]Subcommand{}}); err != nil {
		t.Fatal(err)
	}
	gotAt, gotHash, ok := CacheStamp("git")
	if !ok {
		t.Fatal("CacheStamp ok=false after SaveCache")
	}
	if !gotAt.Equal(at) {
		t.Errorf("introspectedAt = %v, want %v", gotAt, at)
	}
	if gotHash != "H" {
		t.Errorf("configHash = %q, want H", gotHash)
	}
}

func TestNewestInputMtime_MissingBinary(t *testing.T) {
	if _, err := newestInputMtime(&config.ToolConfig{Binary: "/no/such/tool/binary"}); err == nil {
		t.Error("a missing tool binary should surface as an error (removed or renamed?)")
	}
}

// TestLoadCacheStale_SurvivesInvalidity covers item 2's whole point: a cache
// entry LoadCache correctly rejects (config changed, or an input is newer —
// the shape of a tool upgrade) still returns its VersionOutput through
// LoadCacheStale.
func TestLoadCacheStale_SurvivesInvalidity(t *testing.T) {
	isolateCacheDir(t)

	at := time.Now().UTC().Truncate(time.Second)
	if err := SaveCache("git", &Schema{
		ConfigHash:     "H",
		VersionOutput:  "git version 2.40.0",
		IntrospectedAt: at,
		Subcommands:    map[string]Subcommand{},
	}); err != nil {
		t.Fatal(err)
	}

	// A validity check that would reject this entry outright (wrong config
	// hash, and an input newer than the cache — exactly a binary upgrade).
	if got := LoadCache("git", CacheValidity{ConfigHash: "DIFFERENT", NewestInput: at.Add(time.Hour)}); got != nil {
		t.Fatal("test setup bug: LoadCache should have rejected this entry")
	}

	schema, ok := LoadCacheStale("git")
	if !ok {
		t.Fatal("LoadCacheStale ok=false for a genuinely stale-but-present cache")
	}
	if schema.VersionOutput != "git version 2.40.0" {
		t.Errorf("VersionOutput = %q, want the stale cached version string", schema.VersionOutput)
	}
	if !schema.IntrospectedAt.Equal(at) {
		t.Errorf("IntrospectedAt = %v, want %v", schema.IntrospectedAt, at)
	}
}

func TestLoadCacheStale_Missing(t *testing.T) {
	isolateCacheDir(t)
	if _, ok := LoadCacheStale("git"); ok {
		t.Error("LoadCacheStale should report ok=false when no cache exists")
	}
}

// TestLoadCacheStaleAt_Hermetic is the RULING 1 regression test
// (design/doctor/wraptool-doctor.md §7.6): LoadCacheStaleAt must read from an
// EXPLICIT cacheDir, never from paths.CacheDir(), so a caller outside this
// package (doctor's drift group) never touches the real host's introspection
// cache. Deliberately does NOT call isolateCacheDir — no XDG_CACHE_HOME
// redirection at all — and writes directly under two independent
// t.TempDir()s to prove the read is rooted at the argument, not at any
// process-global state.
func TestLoadCacheStaleAt_Hermetic(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "cache-a")
	dirB := filepath.Join(t.TempDir(), "cache-b")

	if _, ok := LoadCacheStaleAt(dirA, "git"); ok {
		t.Fatal("LoadCacheStaleAt should report ok=false when no cache exists at dirA")
	}

	at := time.Now().UTC().Truncate(time.Second)
	data, err := json.Marshal(&Schema{
		Binary:         "/usr/bin/git",
		VersionOutput:  "git version 2.44.0",
		IntrospectedAt: at,
		Subcommands:    map[string]Subcommand{},
		FormatVersion:  schemaFormatVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dirA, "introspection", "git.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}

	schema, ok := LoadCacheStaleAt(dirA, "git")
	if !ok {
		t.Fatal("LoadCacheStaleAt ok=false after writing directly under dirA")
	}
	if schema.VersionOutput != "git version 2.44.0" {
		t.Errorf("VersionOutput = %q, want the seeded version", schema.VersionOutput)
	}

	// dirB never had anything written to it — reading it must never fall back
	// to dirA, or to the real host's paths.CacheDir().
	if _, ok := LoadCacheStaleAt(dirB, "git"); ok {
		t.Error("LoadCacheStaleAt(dirB, ...) should report ok=false — nothing was ever written there")
	}
}

// TestLoadCacheStale_DelegatesToLoadCacheStaleAt pins LoadCacheStale as
// exactly LoadCacheStaleAt(paths.CacheDir(), toolName) — the real-host
// entry point `serve`'s own callers keep using, now expressed as a thin
// wrapper rather than its own read path.
func TestLoadCacheStale_DelegatesToLoadCacheStaleAt(t *testing.T) {
	isolateCacheDir(t)

	at := time.Now().UTC().Truncate(time.Second)
	if err := SaveCache("git", &Schema{VersionOutput: "v1", IntrospectedAt: at, Subcommands: map[string]Subcommand{}}); err != nil {
		t.Fatal(err)
	}

	viaStale, okStale := LoadCacheStale("git")
	viaAt, okAt := LoadCacheStaleAt(paths.CacheDir(), "git")
	if okStale != okAt {
		t.Fatalf("ok mismatch: LoadCacheStale=%v, LoadCacheStaleAt(paths.CacheDir(), ...)=%v", okStale, okAt)
	}
	if viaStale.VersionOutput != viaAt.VersionOutput || !viaStale.IntrospectedAt.Equal(viaAt.IntrospectedAt) {
		t.Errorf("LoadCacheStale and LoadCacheStaleAt(paths.CacheDir(), ...) disagree: %+v vs %+v", viaStale, viaAt)
	}
}

// TestSchemaFormatVersion_DiscardsOlderFormat covers item 4's cache-format
// bump: a cache entry written in an older FormatVersion must be treated as
// absent by BOTH LoadCache and LoadCacheStale, never decoded and served with
// Coverage/ParseOutcome silently at their Go zero values.
func TestSchemaFormatVersion_DiscardsOlderFormat(t *testing.T) {
	isolateCacheDir(t)

	at := time.Now().UTC().Truncate(time.Second)
	if err := SaveCache("git", &Schema{
		ConfigHash:     "H",
		IntrospectedAt: at,
		Subcommands:    map[string]Subcommand{},
	}); err != nil {
		t.Fatal(err)
	}

	// Rewrite the just-saved cache file with an older FormatVersion, as if
	// written by a pre-format-2 binary.
	path := paths.IntrospectionCacheFile("git")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var schema Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	schema.FormatVersion = schemaFormatVersion - 1
	old, err := json.Marshal(&schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}

	if got := LoadCache("git", CacheValidity{ConfigHash: "H", NewestInput: at.Add(-time.Hour)}); got != nil {
		t.Error("LoadCache must discard an older-FormatVersion entry rather than serve it")
	}
	if _, ok := LoadCacheStale("git"); ok {
		t.Error("LoadCacheStale must discard an older-FormatVersion entry rather than serve it")
	}
	// Ruling 5: CacheStamp must agree with LoadCache/LoadCacheStale about
	// what counts as present. Before this fix CacheStamp did its own bare
	// read-and-decode with no FormatVersion check, so it could report an
	// introspected_at for an entry every other reader in this package
	// already treats as absent (wraptool_info claiming freshness for a
	// schema LoadCache would silently discard and re-introspect).
	if _, _, ok := CacheStamp("git"); ok {
		t.Error("CacheStamp must discard an older-FormatVersion entry rather than serve it")
	}
}

// TestSaveCache_StampsFormatVersion covers SaveCache being the sole writer of
// FormatVersion: a caller-supplied value (here, deliberately wrong) must be
// overwritten on save, not trusted.
func TestSaveCache_StampsFormatVersion(t *testing.T) {
	isolateCacheDir(t)

	if err := SaveCache("git", &Schema{
		ConfigHash:     "H",
		IntrospectedAt: time.Now().UTC(),
		Subcommands:    map[string]Subcommand{},
		FormatVersion:  999, // must not survive the save
	}); err != nil {
		t.Fatal(err)
	}

	schema, ok := LoadCacheStale("git")
	if !ok {
		t.Fatal("LoadCacheStale ok=false right after SaveCache")
	}
	if schema.FormatVersion != schemaFormatVersion {
		t.Errorf("FormatVersion = %d, want %d (SaveCache must stamp its own)", schema.FormatVersion, schemaFormatVersion)
	}
}
