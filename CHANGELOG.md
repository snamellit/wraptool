# Changelog

Release tags (`wraptool-vX.Y.Z`) are the sole version source; binaries for
each tag are published by CI with SHA-256 checksums. Notes here cover what
changed and anything an upgrade should know.

## 0.4.0 — unreleased

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
