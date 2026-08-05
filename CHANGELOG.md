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

### Changed

- On a Docker-only host, `harness install` for an npm harness realizes and
  activates the pool generation, then warns (instead of failing) that the
  host-side legacy binary was not installed — containers on such hosts get
  their harness via the wraptool devcontainer feature's `:0` (legacy,
  per-image provisioning) major; the new `:1` wiring-only major (below)
  never provisions a binary itself, on any host.
- Cross-architecture `sync` no longer runs the host's legacy installer as
  a side effect.
- **Breaking (Dev Container feature):** published a new `wraptool` feature
  major, `1.0.0` (§12.2/§12.3 of the unified harness pool design). It removes
  the `provision` and `version` options entirely — the harness binary is no
  longer installed by the feature at image build time. Instead it comes from
  the host's shared harness pool `runtime/` tree, mounted read-only by
  `wraptool up` (or declared manually for a plain `devcontainer up`); `PATH`
  gains the mount's `bin/` via `containerEnv` (appended, never prepended).
  `connect.sh` now also fails postCreate loudly if the selected harness
  isn't resolvable on `PATH`, naming `wraptool harness install <name>` as the
  fix. Consequence: `forge.snamellit.com/pti/wraptool/wraptool:latest` now
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
