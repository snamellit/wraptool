# Changelog

Release tags (`wraptool-vX.Y.Z`) are the sole version source; binaries for
each tag are published by CI with SHA-256 checksums. Notes here cover what
changed and anything an upgrade should know.

## 0.5.0 — unreleased

Phase 3 of the unified harness pool: the OCI provisioner. `wraptool
harness install` now works on hosts without Guix — macOS, Windows, and
non-Guix Linux — through Docker or Podman.

### Added

- A pinned provisioning image (upstream `node:24-bookworm`, pinned by its
  multi-architecture manifest-list digest — amd64 and arm64, so Apple
  silicon works natively) drives npm resolve/realize on non-Guix hosts.
  The image digest is folded into every npm `recipe_revision`, so
  upgrading the image deliberately re-resolves locks.
- Backend auto-selection: Guix when available and the target matches the
  host architecture, otherwise Docker/Podman. Cross-architecture
  `harness sync --platform` routes through the OCI backend even on Guix
  hosts.
- Distinct, accurate errors when no backend exists — including the
  guix-present-but-cross-architecture case.
- `--backend auto|guix|oci` on `harness install`/`update`/`sync` overrides
  backend auto-selection; an explicit choice whose tooling is absent errors
  immediately.
- `wraptool harness gc --legacy` (§16 Phase 4 item 5): removes the pool's
  legacy `bin/`+`lib/` trees entirely, once they are no longer needed, and
  writes a marker recording that — so a later `wraptool up`/`list` no
  longer silently recreates them (`ensureHarnessPool` now checks it before
  creating `bin/`+`lib/`; `home/` and the new always-created `runtime/`
  directory, below, are unaffected by the marker either way). An explicit
  `wraptool harness install <name>` (or `update`) removes the marker again,
  deliberately un-gc'ing the pool. Explicit opt-in only — never automatic,
  and there is no `--force` escape hatch. Refused (an error, and nothing at
  all is removed — not even the ordinary generation/cache/staging cleanup)
  when any pool-lockable harness named in `manifest.yaml` has no genuinely
  realized, on-disk active generation for this host's own platform (a
  dangling pointer whose generation is gone from disk does not count as
  active); the error names every such harness, the fix
  (`wraptool harness install <name>`), and the `manifest.yaml` escape hatch
  for a harness no longer used. `--dry-run` reports what `--legacy` would
  remove without touching anything. `GCReport` gains `LegacyRemoved`
  (true only when at least one tree actually existed to remove); the `gc`
  audit event gains a `legacy_removed` field, recorded even if the legacy
  step itself later fails, so real deletions are never left unaudited.

### Changed

- On a Docker-only host, `harness install` for an npm harness realizes and
  activates the pool generation, then warns (instead of failing) that the
  host-side legacy binary was not installed — containers on such hosts get
  their harness via the wraptool devcontainer feature's `:0` (legacy,
  per-image provisioning) major; the new `:1` wiring-only major (below)
  never provisions a binary itself, on any host.
- Cross-architecture `sync` no longer runs the host's legacy installer as
  a side effect.
- The Guix container runtime tolerates a pool whose legacy `bin/`+`lib/`
  have already been removed via `gc --legacy`: an active generation mounts
  `runtime/` alone (the legacy expose is dropped, not attempted against a
  now-missing host path), and a harness with neither an active generation
  nor legacy `bin/`+`lib/` fails `wraptool up` with an actionable error —
  naming the actual paths and the real fix — instead of an opaque
  `guix shell --expose` failure. A pool with only ONE of the two legacy
  directories present (never `gc --legacy`'s own doing — it always removes
  both together) is treated as unusable rather than partially mounted, with
  a distinct error calling out the inconsistency and naming both expected
  paths.
- `ensureHarnessPool` now always creates the pool's `runtime/` directory (not
  just `bin/`+`lib/`+`home/`): a scaffolded devcontainer config's pool mount
  source needs to exist, developer-owned, before any container engine gets
  a chance to create it root-owned on first use.
- **Breaking (Dev Container feature):** published a new `wraptool` feature
  major, `1.0.0` (§12.2/§12.3 of the unified harness pool design). It removes
  the `provision` and `version` options entirely — the harness binary is no
  longer installed by the feature at image build time. Instead it comes from
  the host's shared harness pool `runtime/` tree, mounted read-only by
  `wraptool up` (or declared manually for a plain `devcontainer up`); `PATH`
  gains the mount's `bin/` via `containerEnv` (appended, never prepended).
  `connect.sh` now also checks that the selected harness resolves on `PATH`,
  naming `wraptool harness install <name>` as the fix: with `connect: true`
  (the default) a missing binary hard-fails postCreate; with `connect: false`
  the same message is a warning only, so a container staged ahead of running
  `harness install` isn't treated as broken. Consequence: `forge.snamellit.com/pti/wraptool/wraptool:latest` now
  resolves to this wiring-only major — pin `:1` for the new behavior or `:0`
  to keep the old per-image provisioning behavior; existing configs already
  pinned to `:0` are unaffected.

### Upgrade notes

- First `install`/`update` per npm harness re-resolves its lock once (the
  recipe now includes the provisioning-image identity) — the same one-shot
  self-heal pattern as previous upgrades, not an error.
- Dev Container feature consumers: if your `devcontainer.json` references
  `forge.snamellit.com/pti/wraptool/wraptool:latest` (or an unpinned major),
  it now resolves to the wiring-only `1.0.0` major and stops installing the
  harness binary at image build. Pin `:0` explicitly to keep the old
  provisioning behavior, or switch to `wraptool harness install <name>` +
  `wraptool up`'s pool mount (the supported path going forward).

## 0.4.0 — 2026-08-05

Phase 2 wave 2 of the unified harness pool: realized generations now
execute on the Guix runtime.

### Added

- Per-harness dispatcher scripts (`runtime/bin/<harness>`) and pointer-file
  activation: every successful realize activates the generation for the
  host platform.
- `wraptool harness rollback <name> [--to VERSION]` — switches the active
  pointer to a retained generation with no network access and no lock
  change. A later `sync` or `update` re-activates the locked generation.
- `wraptool harness gc [--dry-run]` — removes unreferenced generations and
  cache objects. Retention keeps everything pointer-referenced or
  lock-recorded plus the two most recent spares per harness/platform, so
  the previous version stays available for rollback.
- The Guix container runtime mounts the pool `runtime/` tree read-only
  whenever the selected harness has an active generation, with
  `runtime/bin` ahead of the legacy pool `bin/` on the (appended) PATH.
  Legacy `bin/` + `lib/` stay mounted during the migration.
- Archive (direct-download) source: `agy` is now pool-locked like the npm
  harnesses — per-platform manifest resolution, sha512-verified artifacts,
  and realization in pure Go with **no container backend required**, so it
  works day 0 on any host. Sources without publish history quarantine via
  `minimum_observation_age` instead of `minimum_release_age`.
- `harness list` gains a live ACTIVE column.

### Fixed

- A pool created by 0.3.x (generations realized before activation existed)
  self-heals: the first `harness install`/`update` per harness prints
  `activated <generation-id>` once and writes the missing pointer and
  dispatcher. This is the upgrade working as intended, not an error.
- `harness install`/`update` no longer abort the legacy install when
  generation realization fails — they warn and continue.
- Fresh-pool `install agy` no longer hits a 7-day quarantine wall.

### Upgrade notes

- No manual steps. Old locks and pools migrate in place; nothing needs
  deleting (`home/` — harness auth — is never touched).
- Generation binaries are FHS-linked and target the **container** platform:
  on a Guix host they are not directly executable outside
  `guix shell --container --emulate-fhs`. Do not patch them — a patched
  file breaks receipt verification.
- macOS: unchanged execution paths (provisioning still via the devcontainer
  feature); `harness install agy` now locks and realizes a generation there
  too, held for a later phase. Native npm-harness installs on macOS/Windows
  arrive with the OCI provisioner (Phase 3).

## 0.3.1 and earlier

Phase 2 wave 1 (0.3.x): immutable generations, offline npm realization
with the verbatim embedded lockfile, receipts, and the `REALIZED` list
column. Phase 1 (0.2.x): manifest/lock/quarantine around the legacy pool,
capability discovery, the request-review Web UI, and the `server start`
command family. See git history for details.
