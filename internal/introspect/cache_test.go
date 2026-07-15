package introspect

import (
	"os"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/pti/wraptool/internal/config"
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

	if LoadCache("git", cacheValidity{ConfigHash: "H", NewestInput: inputsOld, TTL: time.Hour}) == nil {
		t.Error("matching config hash + older inputs + within TTL should hit")
	}
	if LoadCache("git", cacheValidity{ConfigHash: "DIFFERENT", NewestInput: inputsOld, TTL: time.Hour}) != nil {
		t.Error("config hash change should invalidate")
	}
	if LoadCache("git", cacheValidity{ConfigHash: "H", NewestInput: now.Add(time.Minute), TTL: time.Hour}) != nil {
		t.Error("an input newer than the cache should invalidate (rebuilt binary / edited overrides)")
	}

	// TTL backstop: cache written two hours ago, TTL one hour → stale.
	save(now.Add(-2 * time.Hour))
	if LoadCache("git", cacheValidity{ConfigHash: "H", NewestInput: now.Add(-3 * time.Hour), TTL: time.Hour}) != nil {
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
