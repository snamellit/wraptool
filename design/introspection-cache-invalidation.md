# Design: introspection cache invalidates on its real inputs

Status: **implemented**
Scope: make the per-tool `--help` introspection cache
(`internal/introspect`) regenerate when anything that determines its content
changes — the config, the wrapped tool binary, or wraptool's own parser code —
instead of only after a time-to-live.

## 1. Problem

`LoadCache` reused a cached schema based on **the TTL alone** (24h default). The
`Schema` even stored the tool's `--version` output, but nothing ever compared it.
So a change to any real input kept serving a stale schema for up to a day:

- **Config** — adding an allowed subcommand didn't re-introspect it (a cache hit
  returns the whole schema), and an edited `overrides_file` was ignored.
- **The wrapped tool** — upgrading `glab` didn't refresh its flags.
- **wraptool's parser code** — the one that bit us: shipping the placeholder-aware
  flag-type fix (`design/flag-type-inference.md`) did nothing, because the cached
  glab schema still typed `--reviewer` boolean. Cache invalidation, on brand.

The workaround was `rm -rf ~/.cache/wraptool/introspection`. That shouldn't be a
step.

## 2. Design — key the cache on its inputs

A cached schema is reused only when every input that produced it still matches.
Three cheap signals, no subprocess on a cache hit:

| Input that drives the schema | Signal |
|---|---|
| **config** (binary path, allowed subcommands, overrides path) | `configHash` — a stable SHA-256 stored on the `Schema` and compared on load |
| **the wrapped tool** (upgraded binary) | its file **mtime** |
| **wraptool's parser code** (rebuilt binary) | the wraptool executable's **mtime** |
| **overrides file** (edited) | its file **mtime** |

`LoadCache(toolName, cacheValidity{ConfigHash, NewestInput, TTL})` invalidates
when the stored `ConfigHash` differs, when the cache's `IntrospectedAt` predates
`NewestInput` (the latest mtime across the three files), or when the TTL lapses.
The TTL stays only as a loose backstop for cases the signals can't see (an
in-place tool upgrade that preserves mtime).

**Why mtime, not `--version`.** Comparing the tool's `--version` on every load
would spawn a subprocess per tool at each startup. An mtime is a `stat`; it also
catches a rebuilt *wraptool* (a `--version` string that never bumps between
builds would not), which is exactly the parser-code case.

**`configHash` excludes flags.** Introspection parses a subcommand's entire
`--help` regardless of which flags a rule allows, so flag edits don't change the
schema and are left out of the hash — only the binary, the sorted set of
subcommands, and the overrides path go in. Subcommand order doesn't matter (sorted).

**Missing tool binary → meaningful error.** Computing `NewestInput` stats the
tool binary; if it's gone, introspection returns "tool binary … (removed or
renamed?)" instead of silently caching an empty schema. `config.Validate`
normally catches a missing binary at load first; this is the backstop.

## 3. Result

The cache self-heals on every real drift source: edit the config, upgrade a tool,
or rebuild wraptool, and the next start re-introspects the affected tool. No
manual cache clearing.
