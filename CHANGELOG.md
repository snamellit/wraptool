# Changelog

Release tags (`wraptool-vX.Y.Z`) are the sole version source; binaries for
each tag are published by CI with SHA-256 checksums. Notes here cover what
changed and anything an upgrade should know.

## 0.9.0 — 2026-10-08

A third, experimental runtime (Docker Sandboxes), the `omp` harness, and a
smaller harness set: every remaining harness is a CLI from the harness pool.

### Fixed

- **Realize no longer empties a shared npm tarball.** When two lock entries
  named the same tarball (pi 0.87.1 vendors `agent-base` twice), realize
  truncated the pool's downloaded copy to zero bytes, and `npm cache add`
  failed with `ENODATA: Cache input stream was empty`. **Upgrade note:** a
  pool already hit by this keeps failing until the file is downloaded
  again — run `wraptool harness sync <harness>` once after upgrading.
- **A pool-locked `claude` no longer updates itself in a container.** Claude
  Code's self-updater ran `npm install -g`, which put a newer, unlocked and
  unquarantined copy in any writable npm prefix in the image (the node
  feature's, for example). That copy came first on `PATH`, ahead of the pool
  binary. The `wraptool` Dev Container feature and the Guix runtime now set
  `DISABLE_AUTOUPDATER=1`. **Upgrade note:** in an existing container,
  remove the stray copy (`npm uninstall -g @anthropic-ai/claude-code`) and
  rebuild the container with feature 1.2.0.
- **The feature's MCP config is owner-only.** `connect.sh` wrote
  `.mcp.json` (and the other harnesses' config files), which carry the bearer
  token, world-readable. It now writes them `0600`, as `wraptool init` does,
  and tightens an existing file on the next run.
- **`wraptool up` no longer prints the admin token.** Its review UI line was
  a login URL with the token in the query, which ended up in terminal
  scrollback, captured logs and pasted transcripts. It now prints
  `review UI: wraptool admin open`, as `wraptool server status` does.
  **Upgrade note:** if `up` output was saved or shared anywhere, rotate the
  token: write a new random value to the file `admin.auth_token_file` names
  and restart the server.
- **`wraptool up` no longer mistakes `/dev/null` for a terminal.** Its
  terminal check accepted any character device, so with stdin on
  `/dev/null`, `--shell=auto` opened a shell on every runtime, and
  `--shell=always` on the Docker Sandboxes runtime failed with
  `inspect exec: context deadline exceeded`. It now asks the terminal for its
  window size.

### Added

- **The Docker Sandboxes runtime, experimental.** `wraptool up --runtime=sbx`
  runs the harness in a Docker Sandboxes microVM instead of a container. It
  is opt-in only: `--runtime=auto` never selects it. The pool's `runtime/`
  is mounted read-only and its `home/` read-write, and the sandbox reaches
  the server through the sandbox proxy on one allowed loopback port.
  `wraptool down` stops the sandbox, and `down --rm` removes it.
  - Needs `sbx` 0.47.0 or later, `sbx login`, a network policy
    (`sbx policy init balanced`), and SSH agent forwarding off host-wide:
    `sbx settings set ssh.agentForwardingEnabled false && sbx daemon restart`.
    The setting also applies to your other sandboxes.
  - `up` refuses to start while sbx could hand the sandbox a credential that
    bypasses wraptool's policy: SSH agent forwarding is on, an agent is
    reachable in the sandbox, sbx has host MCP servers registered, or a
    global sbx secret exists that is not a model-provider key.
    `--allow-sbx-secrets` overrides only the last check.
  - `--sandbox-template` (or `WRAPTOOL_SANDBOX_TEMPLATE`) picks the OCI image
    the sandbox starts from.
  - **Known limitation:** one runtime per worktree is not enforced yet.
    `wraptool down` stops every runtime it finds for the worktree, including
    a Dev Container that another session is using, and
    `up --runtime=sbx` points the worktree's `.mcp.json` at the sandbox, which
    a Dev Container on the same worktree cannot reach. Test it in a scratch
    repository. Design: `design/harness/docker-sandbox-runtime.md`; the
    fix: `design/harness/one-runtime-per-worktree.md`.
- **The `omp` (Oh My Pi) harness**, from a new pool source kind, `release`.
  The pool downloads one `omp` binary per platform from a GitHub release and
  locks it by the release's `SHA256SUMS.txt`. Versions and publish times come
  from the npm packument, so `minimum_release_age` applies. It needs no guix,
  docker, or podman, and no Node runtime.
  - `omp` is pool-only: run `wraptool harness install omp` before
    `wraptool up --harness omp`. `install`, `update`, and `sync` skip the
    legacy step without a warning.
  - `init --harness omp` writes `.omp/mcp.json` (global:
    `~/.omp/agent/mcp.json`) with Streamable HTTP at `/mcp`. The `wraptool`
    Dev Container feature is 1.2.0 and accepts `omp` as its `harness`.
  - Releases before 17.2.10 publish no checksum file, so the pool cannot lock
    them.
  - `omp` reads other harnesses' configuration (`.claude`, `.cursor`,
    `.codex`) from the shared pool HOME.
- Downloads are platform-scoped: archive and Node runtime artifacts are fetched
  only for the platform being realized, and `sync` records the cache addresses
  it backfills. npm packages are still fetched for every platform.
- `harness install`, `update`, and `sync` report each download on stderr:
  its size, then progress every 5 seconds. A claude update downloads several
  hundred megabytes, which used to look like a hang on a slow network.
- An interrupted `install` or `update` resumes: downloads that already
  finished are found again by their upstream integrity hash and re-verified
  instead of downloaded again.

### Removed

- **The `roo`, `cursor`, and `gemini` harnesses.** Every remaining harness
  (`agy`, `claude`, `opencode`, `pi`) is a CLI that runs in the container
  from the harness pool.
  - Roo Code was shut down upstream on 15 May 2026. Its SSE bug dropped the
    `Authorization` header, so `init` had skipped it since the listener
    began requiring auth.
  - Google retired Gemini CLI for consumer accounts on 18 June 2026 in favor
    of Antigravity (`agy`). It was never pool-lockable, so no container could
    get its binary.
  - Cursor is a GUI editor, not a container CLI.

  `init --harness` with any of the three now fails as an unknown harness.
  The `wraptool` Dev Container feature moves to `1.1.0`, and its `harness`
  option no longer accepts `gemini`. **Upgrade notes:** delete any
  `.roo/mcp.json`, `.cursor/mcp.json`, or `.gemini/settings.json` an earlier
  `init` wrote (they hold the MCP token). A `devcontainer.json` that sets
  `"harness": "gemini"` must switch to `agy`, or pin the feature to `:1.0`.

## 0.8.0 — 2026-09-04

The 1.0 release-candidate line: the MCP listener is authenticated by default,
the bundled Node runtime becomes a tracked input, and the compatibility
promise is published.

### Security

- **The sse MCP listener is authenticated by default.** `serve`, `up`, and
  `init` share one token provisioner: an unset `mcp.auth_token_file` is
  auto-provisioned (0600, default path under the config dir) instead of left
  tokenless, on both the guix and devcontainer runtimes. The listener also
  rejects browser requests whose `Origin` is not loopback and requests with
  an unrecognized `Host`, on `/sse`, `/mcp`, and `/message` — closing a
  drive-by tool-execution path from ordinary web pages. **Upgrade note:** a
  harness config generated against a tokenless listener now gets 401 — re-run
  `wraptool init` (or `wraptool up`) once to embed the token.
- Generated harness configs are written 0600 (and re-tightened on rewrite),
  refuse to overwrite an existing file wraptool cannot merge, and are
  appended to `.git/info/exclude` so a token-bearing file can't ride along
  with `git add -A`.
- Credential scrubbing redacts JSON notation (`"token":"..."`), which
  previously passed through unredacted, and no longer mangles
  already-redacted markers.

### Added

- **Node runtime tracking** (#38): the node version each npm harness bundles
  is now an observed, quarantined, locked input. `harness refresh` observes
  the nodejs.org release index per pinned line under the same
  `minimum_release_age`/`minimum_observation_age` quarantine as package
  versions (no bypass); `update` resolves the newest eligible patch, locks
  it, folds it into `recipe_revision` (format 5) so a new eligible node
  release alone self-heals every pool; the package's `engines.node` is
  checked at resolve time with an actionable error. Routine node bumps need
  no wraptool release; moving a major line stays a code change.
- The compatibility promise: `docs/releases.qmd` now enumerates the frozen
  surfaces, the excluded ones, the version-bump rules, and the reserved
  security exception that 1.0 will stand on.
- Upstream MCP tool names are sanitized for model APIs (a namespaced
  upstream tool no longer breaks the whole session on strict clients).

### Changed

- `wraptool init` scaffolds the configuration when absent (as `serve` and
  `up` do) and fails loudly on an unreadable one instead of silently writing
  a config that cannot authenticate; `--harness all` validates every target
  before writing any and skips auth-unverified harnesses with a notice.
- The stdio server transport no longer appears in any user-visible surface
  (help, examples, error strings); it remains tests-only and unsupported.
- Documentation accuracy pass over every claim of protection: scrubbing is
  described as the heuristic it is, the review UI as token-gated rather than
  unreachable, deny rules as exact-match (with the `doctor` check that flags
  prefix gaps), and the flagship isolation example now teaches the hardened
  allow/deny split.

## 0.7.2 — 2026-09-04

- Bumped the pool's bundled Node runtime to 22.23.2 (from 22.11.0, which
  predates node's zstd API — new pi releases crashed on start, #38) and
  bumped the lock recipe format so every existing pool self-heals on its
  next `harness update`.

## 0.7.1 — 2026-09-02

First published 0.7 binaries. Also fixes the CI toolchain: golangci-lint
2.13.2 (built with go1.27) and a serve-state test that broke the release
pipeline on the runner.

## 0.7.0 — 2026-08-13 (tag only — the release pipeline failed; no binaries were published. 0.7.1 is the first installable 0.7)

`wraptool doctor`: an operator preflight that checks the installation, the
configuration's posture, and the policy's own content. It automates the manual
checklist in `design/policy/security-audit-policy-enforcement.md`, which until
now told operators to grep their own configuration by hand.

### Added

- `wraptool doctor` — 55 checks across eight groups: host dependencies
  (`env.*`), configuration integrity and permissions (`config.*`), listener and
  credential-file posture (`exposure.*`), tool and MCP-server wiring
  (`tools.*`), the running server versus the configuration on disk
  (`runtime.*`), the harness pool (`pool.*`), a heuristic lint over the policy
  itself (`policy.*`), and policy-versus-installed-tools drift (`drift.*`, see
  Known limits). Safe as the very first command after install: a missing
  configuration is reported as information, not an error.
- Findings carry a `file:line` location resolved against the operator's own
  YAML, so a policy finding names the line to change.
- `# doctor:ignore <check-id> -- reason` suppresses a warning per site. Only
  warnings are suppressible; a suppressed finding is still counted, and
  `--json` carries it with its reason so an auditor sees what was waived.
- `--json` (stable check IDs, schema 1), `--only`/`--skip` by check or group,
  `--strict`, `--no-probe`, `--workspace`, `--verify-pool`. Exit `0` clean,
  `1` on a failure, `2` when doctor could not complete — a distinct signal,
  because "your setup is broken" and "I could not tell" are different answers.
- `docs/doctor.qmd`, a `cli-reference` section, and troubleshooting entries for
  the findings an operator meets first.
- `serve.state` records the server's effective runtime state — transport, admin
  listen, uid, config path, PATH, start time and last successful config load —
  so `runtime.*` can report the config on disk against the policy actually in
  force. A failed reload deliberately leaves the load timestamp untouched;
  that is what makes a stale policy detectable.

### Changed

- **wraptool refuses to run as root.** Every wrapped tool inherits the server's
  uid, so a root server makes a policy that allows `git pull` allow it against
  any file on the host, and the configuration whose permissions decide who may
  grant capabilities becomes writable by the process reading it. Run wraptool
  as the user that owns the credentials — a dedicated service user, or
  yourself. The guard keys on the effective uid, so a setuid binary is caught
  too, and it is a no-op on Windows.
- `mcp.allowed_cwd_roots` now expands a leading `~`, like every other path in
  the configuration. `~/src` previously failed with "must be an absolute path",
  which was true and read as a rule against home-relative paths.
- Introspection builds its child environment with `executor.ChildEnv`, matching
  the tool-execution and MCP-server paths. It was the one spawn in the codebase
  that inherited the caller's whole environment. `PATH` and `HOME` survive, so
  plugin-derived subcommands still enumerate; ambient shell state no longer
  reaches a help probe.
- The introspection cache records a format version and per-subcommand parse
  quality. An older cache is discarded rather than misread, and doctor reports
  *which* of "never introspected", "written by a different build" and "corrupt"
  it found, because the remedies differ.

### Known limits

- The `drift.*` group is informational. It reports which policy rules it could
  not verify against the installed tools, and why, but never claims a
  subcommand or flag is absent. Five measured environment axes — the
  client-supplied working directory (per MCP call), a tool's declared `env` and
  `env_files`, `PATH`, `working_dir` and `HOME` — mean a probe run outside the
  execution environment cannot support that claim; see
  `design/doctor/wraptool-doctor.md` §7.6. A rule naming a subcommand the tool
  does not have surfaces as one whose help output was not recognised, and the
  authoritative answer arrives when the call fails.
- The `policy.*` group is heuristic by construction, which is what
  `doctor:ignore` is for. Its per-tool knowledge covers git, kubectl, gcloud
  and terraform; any other tool gets the tool-independent checks only, and
  doctor never guesses per-tool knowledge it does not have.

## 0.6.0 — 2026-08-10

Distribution and onboarding.

### Added

- A Homebrew tap for wraptool itself, published by the release workflow. The
  formula is attached to each release, so the tap can also be updated by hand
  when the push secret is absent.
- A comparison page for wraptool against Docker Sandboxes, and a design note
  investigating microVMs as a harder runtime boundary.

### Changed

- Onboarding documentation reworked around the first-run path.
- Documentation adopted a written style contract and a terminology table:
  one name per concept, with `agent`, `harness`, and `review UI` as the
  preferred terms.

### Fixed

- The shrinkwrap-seeding gate derives from the embedded raw lockfile rather
  than a reconstructed summary, so a harness shipping its own
  `npm-shrinkwrap.json` realizes from what was actually locked.
- Homebrew formula style, instance URLs, and the Linux dependency note.

## 0.5.0 — 2026-08-08

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
- A harness whose npm package ships its own `npm-shrinkwrap.json` (`pi`,
  `@earendil-works/pi-coding-agent`) now realizes into the pool like any
  other npm harness. npm reifies that package's dependency subtree from the
  shrinkwrap embedded in its extracted tarball at install time, not from
  the outer lockfile, and those nested fetches need npm's URL-indexed HTTP
  cache — which offline pool realization's `npm cache add` (content-only,
  keyed by integrity) never populates. Realize now seeds that URL index
  itself, from the same lock's already-verified (resolved URL, integrity)
  pairs, before running `npm ci --offline` — no network access, no change
  to what gets trusted. Pool realization of a shrinkwrapped harness was not
  possible before this release (0.4.0 surfaced it as an opaque npm
  `ENOTCACHED` failure, falling back to the legacy install). The seeding
  gate reads npm's own `hasShrinkwrap` markers from the verbatim embedded
  lockfile, not only the lock's summary flag — a lock entry resolved by an
  earlier wraptool carries the markers but not the flag, and nothing ever
  forces such a still-current lock to re-resolve, so trusting the flag
  alone left exactly the pre-existing locks this feature targets failing
  with the same `ENOTCACHED` (found in the field on a real pi 0.83.0
  pool).
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
- Docs: [Harness pool: quarantine and scheduled refresh](https://pti.pages.snamellit.com/wraptool/harness-pool.html)
  (§16 Phase 5 item 1) explains why a periodic `wraptool harness refresh` is
  what makes `minimum_observation_age` actually count — a pool that never
  refreshes never records `first_seen_at`, so the observation window never
  starts — and gives a systemd user timer and a macOS launchd plist to
  schedule it. It also states plainly which Phase-5 items already shipped
  earlier (local observations and release-age eligibility in Phase 1;
  `--allow-young` bypass audit events and `harness list`'s quarantine
  countdown, also Phase 1) and that upstream signature/transparency
  adapters (Phase 5 item 4) remain deliberately unimplemented.
- `wraptool up` prints a one-line stderr notice when the running binary was
  built from a different commit than the wraptool source checkout it's
  launched in (`go.mod`'s module line is exactly `module
  github.com/pti/wraptool`), comparing `buildinfo`'s stamped VCS revision
  against `git rev-parse HEAD`. Silent for a binary with no stamped VCS
  revision, in any other project, or whenever git can't answer — never
  blocks `up`. A tagged release binary does carry a stamped revision, so
  running it inside a wraptool checkout that has moved past the tag prints
  the notice too — that reader is developing wraptool, and the hint is
  deliberate. Catches the
  "built a fix, forgot to reinstall, harness keeps exercising yesterday's
  code" self-hosted-development trap.

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
  the host's shared harness pool `runtime/` tree, which must be DECLARED as a
  read-only mount in `devcontainer.json` — the devcontainer CLI's `--mount`
  flag has no read-only field at all, so `wraptool up` cannot inject this
  mount itself and only verifies it (`wraptool init devcontainer` scaffolds
  the declaration for new projects; see the `up` entry below); `PATH` gains
  the mount's `bin/` via `containerEnv` (appended, never prepended).
  `connect.sh` now also checks that the selected harness resolves on `PATH`,
  naming `wraptool harness install <name>` as the fix: with `connect: true`
  (the default) a missing binary hard-fails postCreate; with `connect: false`
  the same message is a warning only, so a container staged ahead of running
  `harness install` isn't treated as broken. Consequence: `forge.snamellit.com/pti/wraptool/wraptool:latest` now
  resolves to this wiring-only major — pin `:1` for the new behavior or `:0`
  to keep the old per-image provisioning behavior; existing configs already
  pinned to `:0` are unaffected.
- `wraptool init devcontainer` scaffolds the `:1` wiring-only feature and both
  pool mounts (`home/` read-write, `runtime/` read-only, the latter as the
  verbatim `,readonly` string form the devcontainer CLI actually enforces — it
  refuses to scaffold a runtime mount whose source path would corrupt that
  form, e.g. one containing a comma). It no longer auto-adds the `node`
  feature for npm harnesses (the pool runtime bundles its own) or scaffolds a
  `provision`/`version` option for any harness. `gemini` (not pool-lockable)
  gets accurate next-steps — bake its binary into the image yourself — instead
  of a dead-end `harness install gemini` suggestion. This repo's own
  `.devcontainer/devcontainer.json` and `examples/devcontainer/` move to the
  same shape.
- `wraptool up --runtime=devcontainer` verifies the pool runtime mount instead
  of injecting it (§12.2): a pool-locked harness with the mount already
  declared read-only prints an info line; a declared mount that the
  devcontainer CLI will NOT actually enforce as read-only (object-form
  entries, or a string-form entry missing `readonly`/`ro`) gets an unmissable
  SECURITY notice, since a writable pool `runtime/` would let this project's
  container rewrite every other project's dispatcher scripts; a missing
  declaration prints the exact `mounts` string entry to add. The home mount
  stays `up`-injected as before, and a declared-but-host-missing source (the
  mixed-OS team case) warns for both mounts rather than failing. `up`'s own
  interactive shell entry-wraps `PATH` with the pool `runtime/bin` dir for
  legacy-feature containers that don't yet have it from `containerEnv`.
  `--harness` is now validated against the descriptor registry before the
  shared server is started, not after, so an unknown harness fails fast
  instead of leaving a server spawned for nothing.
- **Sunset: the older `wraptool-connect` Dev Container feature.** Its source
  (`features/src/wraptool-connect/`, `features/test/wraptool-connect/`) is
  deleted and it is no longer published by CI. Already-published registry
  artifacts (`:0`, `0.1.x`) keep working for existing configs indefinitely —
  only the source is retired. New setups use the merged `wraptool` (`:1`)
  feature above.

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
capability discovery, the request-review UI, and the `server start`
command family. See git history for details.
