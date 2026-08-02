# Design: one version-locked harness pool for Guix and Dev Containers

Status: **proposed (rev 2 — elaborated implementation specification)**
Scope: replace Dev Container per-image harness provisioning with the same
developer-owned pool and `wraptool harness` commands used by the Guix runtime.
This supersedes the split provisioning model in
[`harness-descriptor.md` §7a](harness-descriptor.md#7a-provisioning-on-a-devcontainer-host-no-guix)
and the pool-mount proposal in
[`harness-pool-devcontainer.md`](harness-pool-devcontainer.md) (its mixed-OS
analysis in §3/§5 is carried forward here; its `ConfigHomeEnv` alternative is
**not** adopted — the shipped feature's `HOME=/home/wraptool-harness` invariant
stays).

> **Rev 2 changes.** Rev 1 stated intent; rev 2 makes the spec implementable
> without further design decisions, with platform independence as a normative
> contract (§6). Concretely: activation switches from symlinks to atomically
> renamed pointer files (§5.2) so it works on NTFS and case-insensitive hosts;
> generations are required to be **symlink-free and position-independent**
> (§6.4) so a tree realized in a Linux container runs from any mount path on
> any host filesystem; every update phase is classified by where it may run and
> whether it may touch the network (§6.2); the Go interfaces workers implement
> are specified (§11.3); npm lock production, offline realization, and wrapper
> entrypoints are specified (§8.1, §11.4); §12.3's rev-1 claim that a committed
> `devcontainer.json` can carry the runtime mount portably is corrected; legacy
> `bin/`+`lib/` migration is specified (§16).

## 1. Problem

Harnesses are developer tools, not project dependencies. Current runtimes do not
model that consistently:

| Concern | Guix runtime | Dev Container runtime |
|---|---|---|
| Harness binary | Developer pool, shared by every project | Installed into each project image |
| Harness version owner | Developer (`wraptool harness install`) | Project (`devcontainer.json` feature option) |
| Auth/config/skills | Shared pool `home/` | Shared pool `home/` |
| Binary mount | Read-only | Not mounted |
| Upgrade scope | All projects | One rebuilt image |

The Dev Container model has two problems:

1. Projects can pin different harness versions while those versions mutate one
   shared `HOME`. The executables do not overwrite each other, but a newer
   harness can migrate shared state that an older harness can no longer read.
2. A Dev Container Feature version locks only the Feature archive. An installer
   inside that archive can still fetch an unpinned `latest` npm package, script,
   transitive dependency, or native artifact. Rebuilding without Docker cache
   does not make that reproducible.

The Guix model has the right ownership and mount boundary, but its installer is
also incomplete: npm resolves `latest`, no pool manifest records intent, no lock
records exact artifacts, and reinstalling immediately consumes a newly
published release. It also leans on the container's `node` from `manifest.scm`
(npm shims use `#!/usr/bin/env node`), so the pool binary silently depends on
the *project* toolchain — exactly the coupling the pool exists to remove.

Finally, the pool must work from **any host OS** (Linux, macOS, Windows —
wraptool already documents Windows pool mounts), while every execution target
is a **Linux container**. Rev 1 under-specified that split.

## 2. Decision

Use one developer-scoped harness pool on every runtime:

```text
<paths.DataDir()>/harness-pool/
```

(`$XDG_DATA_HOME/wraptool/harness-pool` on Linux; the macOS/Windows equivalents
come from the same `paths.DataDir()` call — never a hardcoded path.)

The pool owns:

- selected harnesses and version constraints;
- exact resolved versions and artifact integrity;
- self-contained, platform-specific runtime generations;
- shared harness `HOME` state;
- release-age policy and locally observed candidates.

Projects select a **harness name**, never its version. `wraptool harness`
commands are the only supported way to install or upgrade pool executables.
Guix containers and Dev Containers mount the selected pool `runtime/` subtree
read-only and mount pool `home/` read-write.

Dev Container Features stop installing harness software. They retain only MCP
wiring and fixed container environment integration. `wraptool up` and generated
Dev Container configurations provide pool mounts.

## 3. Goals

- Same `wraptool harness` workflow with or without Guix, on Linux, macOS, and
  Windows hosts, producing byte-identical Linux-target generations.
- One harness version per developer and target platform, shared by all projects.
- Exact, inspectable version and integrity lock.
- Offline realization from a complete lock and artifact cache.
- No package downloads before configured release-age threshold.
- Atomic upgrade and rollback on every supported host filesystem.
- Read-only executable surface inside untrusted project containers.
- No dependency on project Node, npm, libc layout, or language Features where a
  self-contained harness runtime is practical.
- Preserve shared login, skills, and harness configuration across projects.

## 4. Non-goals

- Packaging harnesses as wraptool or Guix packages.
- Mirroring entire npm or OCI registries.
- Running harnesses natively on the host OS. Generations target Linux
  containers only; there are no `darwin-*` or `windows-*` platform keys.
- Windows containers. Docker on Windows is used in Linux-container mode only.
- Claiming a time delay prevents all supply-chain attacks. It mitigates short
  lived, newly published malicious releases; it does not make an old malicious
  release safe.
- Protecting writable harness configuration from the harness itself. Shared
  `home/` remains an accepted cross-project state channel.
- Allowing projects to request arbitrary package sources or installer scripts.
  Source recipes remain trusted wraptool code.

## 5. Pool layout

```text
harness-pool/
├── manifest.yaml                 # developer intent and update policy
├── harness-lock.json             # exact generated resolution
├── observations.json             # first-seen candidate metadata
├── audit.jsonl                   # append-only pool events (bypass, activate, rollback)
├── cache/
│   └── sha256/<digest>           # immutable downloaded artifacts (flat, content-addressed)
├── runtime/                      # ONLY this tree is exposed read-only to project containers
│   ├── bin/
│   │   ├── claude                # dispatcher script (POSIX sh, generated, §5.3)
│   │   └── opencode
│   ├── active/
│   │   ├── linux-amd64-glibc/
│   │   │   ├── claude.gen        # pointer file: one line, a generation id (§5.2)
│   │   │   └── opencode.gen
│   │   └── linux-arm64-glibc/
│   └── generations/
│       └── <generation-id>/      # e.g. claude-2.1.220-linux-amd64-glibc-a1b2c3d4
│           ├── bin/              # wrapper entrypoints, $0-relative (§11.4)
│           ├── lib/              # e.g. node_modules tree
│           ├── runtime/          # pinned Node or other required runtime
│           └── receipt.json
├── home/                         # shared rw auth/config/skills (unchanged location)
├── staging/                      # never mounted into project containers
│   └── txn-<id>/                 # one in-flight transaction per directory
└── lock                          # advisory mutation lock (§6.5)
```

`runtime/` contains no credentials and is safe to expose read-only. `cache/`,
`staging/`, `audit.jsonl`, manifests, and lockfiles are not mounted into
project containers. A project can execute active tools but cannot replace
them, alter update policy, or inspect partially downloaded content.

A generation is immutable after publication. Its directory name is its
**generation id**: `<harness>-<version>-<platform-key>-<digest8>`, where
`digest8` is the first 8 hex chars of the receipt digest (§11.5). Upgrade
creates and verifies a new generation under `staging/`, renames it into
`runtime/generations/`, then atomically switches the applicable pointer file.
Existing processes keep running old mapped files; new processes use the new
generation. Old generations remain available for rollback until explicit
garbage collection.

### 5.1 Platform keys

Dev Containers are Linux even when Docker runs on macOS or Windows. Native
artifacts therefore target the **container** platform, never the host OS.
`PlatformKey` is a typed string constant set in Go (exhaustive-lint enforced,
per `design/policy/flag-constraint-type-single-source.md` convention):

```text
linux-amd64-glibc
linux-arm64-glibc
linux-amd64-musl
linux-arm64-musl
```

Guix uses glibc/FHS emulation. Most generated Dev Containers use glibc. Musl is
resolved separately rather than assuming a glibc binary works on Alpine.

**Musl is defined but deferred.** Development containers are overwhelmingly
glibc (the `devcontainers/*` base images are Debian/Ubuntu; the Guix runtime is
glibc by construction); Alpine's niche is deployment images, not interactive
dev environments. So the musl keys exist in the enum and the dispatcher's
detection from day one (churn-free later), but no phase auto-realizes them:
glibc is the only default-realized libc, and musl realization happens only on
an explicit `wraptool harness sync --platform linux-<arch>-musl`. The
dispatcher's precise "not realized for linux-…-musl; run: …" error (§5.3) is
deliberately the demand probe — occurrences of it in the wild are the signal
to promote musl to a default target.

**Host-side target selection** (what `wraptool harness install/sync` realizes
and what `up` verifies):

- Guix runtime: arch = host arch (the container shares it), libc = glibc
  (always, `--emulate-fhs`).
- Dev Container runtime: arch = the Docker daemon's default platform
  (`docker info` / `uname -m` of the daemon, not the client — on Apple silicon
  Docker Desktop this is `arm64`); libc defaults to **glibc**. There is no
  reliable static way to know an arbitrary image's libc, so: `wraptool up
  --platform-libc=musl` (or `harness.platform_libc` in wraptool config)
  overrides — expected to be rarely used, see the musl deferral below — and
  the in-container dispatcher independently detects the *actual*
  libc at exec time (§5.3), so a mismatch fails with a precise message naming
  the missing key rather than a loader error.

### 5.2 Activation: pointer files, not symlinks

Rev 1 activated via `active/<platform>/<name>` symlinks. Symlinks are not
portable pool primitives: the pool lives on the **host** filesystem, and on
Windows (NTFS) symlink creation needs privilege/developer mode, while Docker
Desktop bind mounts translate them inconsistently. Activation is therefore a
**pointer file**:

- `runtime/active/<platform-key>/<harness>.gen` contains exactly one line: the
  active generation id, LF-terminated, UTF-8, no other content.
- Activation = write `<harness>.gen.tmp-<txn>` in the same directory, fsync,
  rename over `<harness>.gen`. Rename-over-existing is atomic on POSIX and on
  NTFS via the Go `os.Rename` path used by the existing atomic-write helpers
  (`internal/config` pattern); reuse one shared helper.
- The dispatcher (§5.3) reads the pointer at exec time. Readers never see a
  partial pointer (rename atomicity); a missing pointer means "not realized for
  this platform" and produces the §12.2 error.
- Rollback = write a previous generation id into the pointer. No tree surgery.

### 5.3 Dispatcher specification (normative)

`runtime/bin/<harness>` is a generated **POSIX sh** script (`#!/bin/sh`, no
bashisms — it must run on debian, alpine/busybox, and the Guix FHS container).
It may use only: `sh` builtins, `uname`, `cat`. Algorithm:

```sh
#!/bin/sh
# generated by wraptool — do not edit
set -eu
name=claude
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)   # .../runtime
m=$(uname -m)
case "$m" in
  x86_64)          arch=amd64 ;;
  aarch64|arm64)   arch=arm64 ;;
  *) echo "wraptool: unsupported architecture $m for $name" >&2; exit 69 ;;
esac
libc=glibc
[ -e "/lib/ld-musl-$m.so.1" ] && libc=musl
key=${WRAPTOOL_PLATFORM:-linux-$arch-$libc}
ptr="$root/active/$key/$name.gen"
if [ ! -f "$ptr" ]; then
  echo "wraptool: $name is not realized for $key;" >&2
  echo "run on the host: wraptool harness sync --platform $key" >&2
  exit 69
fi
gen=$(cat "$ptr")
exec "$root/generations/$gen/bin/$name" "$@"
```

Normative points:

- `$0`-relative resolution: the dispatcher works wherever `runtime/` is
  mounted (`/opt/wraptool-harness/runtime`, a Guix same-path mount, a test
  tempdir). It must never contain an absolute host path.
- libc detection is the musl loader probe shown above; glibc is the default
  when the probe misses. `WRAPTOOL_PLATFORM` overrides detection (tests,
  unusual images).
- Exit code 69 (EX_UNAVAILABLE) for "no realization", with the exact host
  command to fix it.
- The dispatcher is regenerated (not edited) on every activation; a golden-file
  test pins its content per harness name (§17).

## 6. Platform-independence contract (normative)

This section is the portability rulebook. Every implementation PR is checked
against it; workers should treat violations as bugs even when a feature "works
on my machine".

### 6.1 Two independent platform axes

| Axis | Values | Decides |
|---|---|---|
| **Host OS** — where wraptool and the pool live | linux, darwin, windows | pool paths (`paths.DataDir()`), file locking, atomic rename, mount-source syntax |
| **Target platform** — where generations execute | the four `PlatformKey`s (§5.1) | artifact selection, realization backend output, dispatcher routing |

No code may conflate them. `runtime.GOOS` decides host behavior only;
`PlatformKey` decides target behavior only. A generation never encodes
anything about the host that produced it — the §17 cross-backend test compares
receipts produced on different hosts for equality.

### 6.2 Phase × capability matrix

The five update phases (§9) have fixed execution environments:

| Phase | Runs | Network | Needs Linux backend | Implementation |
|---|---|---|---|---|
| Refresh | host, pure Go | yes (registry metadata over HTTPS) | no | Go `net/http` against registry JSON APIs |
| Resolve | Linux backend container | yes (registry metadata only) | **yes** (npm sources; archive sources resolve in pure Go) | §8.1 |
| Fetch | host, pure Go | yes (locked URLs only) | no | Go download + integrity verify into `cache/` |
| Realize | Linux backend container | **no** | **yes** | §11.4 |
| Activate | host, pure Go | no | no | §5.2 pointer rename |

Consequences workers must preserve:

- Refresh, fetch, activate, `list`, `rollback`, and `gc` work on **every** host
  with no container runtime present.
- Resolve and realize require a backend (§11): Guix where available, otherwise
  OCI (Docker/Podman). This is what makes Windows/macOS hosts first-class —
  they produce Linux artifacts through the same backend interface.
- Fetch downloads only URLs already recorded in the proposed lock, verifies
  integrity **before** the object is renamed into `cache/`, and never consults
  a version selector. A fetch of an unlisted URL is a bug, not a fallback.

### 6.3 Path discipline

- **Host paths** are built with `path/filepath`, rooted at `paths.DataDir()`.
  Never hardcode `~/.local/share`, `/home`, or drive letters.
- **Container paths** are Linux paths built with `path` (forward slashes),
  from named constants: `devcontainerPoolHome = /home/wraptool-harness`
  (exists, keep) and new `devcontainerPoolRuntime = /opt/wraptool-harness/runtime`.
- Every mount is a `(HostPath, ContainerPath, ReadOnly)` triple. When a host
  path is serialized into a mount string, Docker CLI argument, or
  `devcontainer.json`, apply `filepath.ToSlash` (established precedent: the
  pool-mount leak check and Windows docs, commits `f6bca00`/`73d1565`).
  Windows drive-letter sources (`C:\Users\...` → `C:/Users/...`) are what
  Docker Desktop expects.
- Nothing inside `runtime/` may contain an absolute path — not the dispatcher,
  not generation wrappers, not receipts' file lists (receipt paths are
  generation-relative). The pool must survive being mounted at a different
  path on every runtime, and `paths.DataDir()` differs per developer and OS.

### 6.4 Filesystem portability rules

The pool lives on ext4, APFS (default case-insensitive), and NTFS. Therefore:

- **Generations are symlink-free.** npm layouts contain symlinks
  (`bin/x → ../lib/node_modules/...`, `node_modules/.bin/*`); realization
  replaces them with wrapper scripts or file copies (§11.4) and the receipt
  step **fails** if a symlink survives (`Lstat` scan). Rationale: symlinks on
  NTFS-hosted pools are unreliable, and relative symlinks silently break when
  trees are copied by tools that follow links.
- **No two paths in a generation may differ only by case** (receipt validation
  compares the case-folded file list for collisions). APFS/NTFS would merge
  them.
- **Atomic replace** uses the single shared helper: temp file/dir in the same
  directory tree as the destination (same volume — staging lives under the
  pool root for exactly this reason), fsync file, rename, fsync parent dir on
  POSIX (skip on Windows). Directory renames (staging → `runtime/generations/`)
  follow the same same-volume rule.
- **Execute bits**: realization sets `0755` on `bin/*` and the bundled runtime
  binaries. POSIX hosts preserve them; on Windows hosts Go's chmod is a no-op,
  and Docker Desktop presents bind-mounted files as executable inside the
  container, so exec still works. The receipt records an `executable: true`
  flag per entry (not the raw mode), so receipts hash identically across
  hosts.
- **File content is the identity.** Receipt digests hash the sorted
  (generation-relative path, executable flag, size, sha256) tuples — never
  mtimes, uids, or host paths.

### 6.5 Locking

All mutating commands (`install`, `update`, `sync`, `rollback`, `gc`,
`refresh` — it writes `observations.json`, `policy set`) take one exclusive
advisory lock on `<pool>/lock` before reading pool state, released on exit.
Implementation: `github.com/gofrs/flock` (cross-platform: `flock(2)` on
POSIX, `LockFileEx` on Windows) — the existing `cmd/up.go` flock is
Linux-oriented and stays where it is; the pool gets the portable dependency.
`list` takes no lock; it must tolerate a concurrent transaction (staging dirs
present, pointer mid-flight) and reports in-progress staging distinctly.

### 6.6 Time

- All persisted timestamps are UTC RFC 3339.
- `first_seen_at` is written once per (source, package, version) and never
  updated — repeated refreshes must not move it in either direction.
- Eligibility (§9) compares registry-supplied `published_at` and pool-local
  `first_seen_at` against the host clock. Clock skew is accepted risk; no
  monotonic-clock persistence.

### 6.7 New dependencies

Two additions to the direct-dependency list (update `CLAUDE.md` when they
land): `github.com/Masterminds/semver/v3` (npm-style range constraints —
`golang.org/x/mod/semver` has no ranges) and `github.com/gofrs/flock` (§6.5).
No language runtimes; resolution/realization use node/npm **inside backends
only**, never on the host.

## 7. Manifest: developer intent

`manifest.yaml` is user-owned, comment-friendly, and strict-decoded (the
`KnownFields` pattern from `internal/config`):

```yaml
schema_version: 1

policy:
  minimum_release_age: 168h       # seven days
  minimum_observation_age: 0h     # optional stronger local quarantine
  allow_prereleases: false

harnesses:
  claude:
    constraint: ">=2 <3"
  opencode:
    constraint: ">=1 <2"
    minimum_release_age: 72h      # optional per-harness override
```

Both quarantine durations are configurable developer policy. A duration of
`0h` disables that delay. Per-harness values override global policy; omitted
values inherit it. Project files and environment variables cannot weaken these
settings — the policy belongs to the developer pool. (`WRAPTOOL_PLATFORM` in
§5.3 selects *which* realized generation runs; it cannot cause installation.)

Manifest names map to descriptors in `internal/harness`. Descriptors own package
identity, source type, executable name, runtime requirements, and install
recipe. A repository cannot substitute `claude` with a different npm package.

Version constraints express developer policy, not a project requirement. Empty
constraint means the descriptor's stable channel. Exact versions remain
supported for developers who want manual upgrades only.

Writes use the shared atomic pattern (§6.4) under the pool lock (§6.5).
Unknown keys and invalid constraints fail before pool mutation.

## 8. Lock: exact resolution

`harness-lock.json` is generated and must contain enough information to realize
a generation without consulting mutable version selectors:

```json
{
  "schema_version": 1,
  "generated_at": "2026-07-30T09:00:00Z",
  "policy_hash": "sha256:...",
  "harnesses": {
    "claude": {
      "source": "npm",
      "package": "@anthropic-ai/claude-code",
      "version": "2.1.220",
      "published_at": "2026-07-29T12:00:00Z",
      "first_seen_at": "2026-07-29T12:10:00Z",
      "recipe_revision": "sha256:...",
      "runtime": {
        "name": "node",
        "version": "24.18.1",
        "artifacts": {
          "linux-amd64-glibc": {
            "url": "https://nodejs.org/dist/...",
            "sha256": "..."
          }
        }
      },
      "npm_lock": {
        "lockfile_version": 3,
        "packages": {
          "...": {
            "version": "...",
            "resolved": "https://registry.npmjs.org/...tgz",
            "integrity": "sha512-...",
            "sha256": "..."
          }
        }
      },
      "generations": {
        "linux-amd64-glibc": "sha256:..."
      }
    }
  }
}
```

Required properties:

- exact top-level harness version;
- publication and first-observation times;
- integrity for every downloaded artifact — the upstream integrity value
  (npm's sha512 SRI) **and** the pool's own sha256 (the `cache/` address), both
  verified at fetch;
- exact transitive dependency graph;
- pinned runtime and platform artifact digest;
- install-recipe revision, so changed wraptool installer logic cannot silently
  reuse a generation built under different assumptions. `recipe_revision` =
  sha256 over the descriptor's serialized install recipe + the backend
  provisioning-image digest (§11.2) + a recipe-format version constant bumped
  when realization logic changes;
- generation receipt digest for each realized platform.

> **Known shape tension (Phase 1 implementation note).** `policy_hash` is a
> document-level field, but effective policy is resolved **per harness**
> (§7's per-harness overrides), so a multi-harness `update` persists
> whichever harness's hash was computed last. Phase 2 moves `policy_hash`
> into each harness entry (the hash of *that* harness's effective policy);
> the document-level field then either holds the global-policy hash or is
> dropped. Until then the field is advisory only — nothing may make
> decisions from it.

A simple `package@version` is insufficient. npm can resolve changed transitive
dependencies, and lifecycle scripts can fetch additional mutable content. npm
sources therefore use a complete lockfile and integrity-check every tarball.
Direct-download sources record final URLs and cryptographic digests. Existing
`curl | bash` recipes (agy today) are not eligible for locked mode and must be
replaced by artifact-specific adapters before migration; until then those
harnesses stay on the legacy path with a warning in `harness list`.

### 8.1 How the lock is produced (resolve mechanics)

npm source, inside the backend container (network: registry only):

1. Synthesize `package.json` in a scratch dir depending on the chosen exact
   version: `{"dependencies": {"@anthropic-ai/claude-code": "2.1.220"}}`.
2. Run `npm install --package-lock-only --ignore-scripts` (npm ≥ 9,
   lockfileVersion 3, npm pinned by the backend image/manifest).
3. Read back `package-lock.json`; embed its `packages` map (version, resolved,
   integrity) into the lock. Reject entries lacking `resolved`+`integrity`
   (git deps, workspace links) — a harness with such deps cannot be locked and
   fails resolution loudly. One exception: an entry with a registry `resolved`
   URL but no `integrity` — the shape npm writes when honoring a dependency's
   bundled `npm-shrinkwrap.json` whose workspace siblings were published
   without one (pi ships exactly this) — has its integrity backfilled from
   the registry's own `dist.integrity` for that exact name@version, the same
   trust root every other entry's integrity came from; Fetch still verifies
   the downloaded tarball against it.
4. Record the descriptor-declared runtime requirement (node major) and select
   the pinned runtime version + per-platform nodejs.org artifacts (URL +
   published SHASUMS digest) — fetched metadata, no payloads.

Archive/direct-download source (pure Go, no backend): resolve version from the
adapter's release metadata endpoint; record per-platform final URL + sha256.

`observations.json` is a flat map `"<source>/<package>/<version>" →
{first_seen_at, published_at, prerelease, yanked}`, merged write-once per key
(§6.6).

## 9. Quarantine and delayed download

Default update policy selects the newest release satisfying both:

```text
now - published_at  >= minimum_release_age
now - first_seen_at >= minimum_observation_age
```

`minimum_release_age` defaults to seven days but is configurable globally or
per harness in pool `manifest.yaml`. Registry publication timestamps are
suitable for mitigating compromised maintainer credentials publishing a new
drive-by version: a release removed or reported during quarantine is never
downloaded.

Examples:

```yaml
# Disable release quarantine globally.
policy:
  minimum_release_age: 0h

# Or retain seven days globally but fast-track one harness.
policy:
  minimum_release_age: 168h
harnesses:
  claude:
    constraint: ">=2 <3"
    minimum_release_age: 24h
```

Changing policy does not itself download or activate anything. A subsequent
`refresh`/`update` evaluates candidates under new effective policy. Effective
policy and its hash are recorded in lock and audit output.

`minimum_observation_age` defaults to zero because requiring every developer to
observe an already-old release and wait again creates severe first-install
friction. Security-sensitive users can set it to, for example, `24h`. Candidate
metadata is then recorded locally by `refresh`, but package payloads are not
downloaded until a later invocation after the observation window.

Update phases are intentionally separate (execution environments in §6.2):

1. **Refresh** — fetch metadata only; record candidate versions and timestamps.
2. **Resolve** — choose newest eligible candidate and write proposed lock data.
3. **Fetch** — download exact locked artifacts after quarantine.
4. **Realize** — build generation without network access.
5. **Activate** — atomically switch the active pointer.

No payload download occurs during refresh or before eligibility. If a candidate
is yanked before eligibility, resolution ignores it. Previously cached content
is never activated unless its version satisfies current policy and lock.

Emergency security updates can bypass quarantine explicitly:

```bash
wraptool harness update claude --allow-young --reason 'CVE-YYYY-NNNN'
```

Bypass requires a non-empty reason, prints candidate age and integrity, and
appends an event to the pool-local `audit.jsonl` (append-only, same event-log
style as `requestlog`). It is never implied by `--force`.

Time delay complements, not replaces:

- digest and signature verification where upstream supplies signatures;
- exact dependency locking;
- no-network realization;
- review of recipe changes;
- read-only runtime mounts;
- rollback.

## 10. Commands

Commands behave identically on Linux, macOS, and Windows hosts, with or
without Guix (backend availability aside, §6.2):

```bash
# Add intent, choose newest eligible version, lock, fetch, realize, activate.
wraptool harness install claude

# Exact developer pin.
wraptool harness install claude --constraint '=2.1.220'

# Configure pool-wide quarantine (0h disables it).
wraptool harness policy set --minimum-release-age 168h

# Optional harness-specific override.
wraptool harness policy set --harness claude --minimum-release-age 24h

# Metadata only; safe for a timer and required by observation-age policy.
wraptool harness refresh [claude]

# Update lock and active generation to newest eligible release.
wraptool harness update [claude]

# Realize the exact existing lock — new workstation, cache loss, or a platform
# the lock knows but this pool hasn't built yet.
wraptool harness sync [--platform linux-arm64-glibc]

# Show desired, locked, active, latest, eligible, and quarantined versions.
wraptool harness list

# Switch the pointer to a retained generation without network access.
wraptool harness rollback claude [--to 2.1.220]

# Remove unreferenced generations and cached artifacts (and legacy bin/+lib/, §16).
wraptool harness gc

# Backend selection for resolve/realize (default: guix if available, else oci).
wraptool harness install claude --backend=oci
```

`install` is idempotent when manifest, lock, recipe, platform, and active
receipt agree. It no longer means "run npm latest again." `update` is the only
normal command that advances a floating constraint.

Example `list` output:

```text
HARNESS  CONSTRAINT  LOCKED   ACTIVE   LATEST   ELIGIBLE  QUARANTINED
claude   >=2 <3      2.1.220  2.1.220  2.1.224  2.1.220   2.1.224 (3d left)
```

## 11. Provisioning backends

Resolution and lock semantics are backend-independent. A backend only executes
Linux containers for the two phases that need them (§6.2); everything else is
pure Go on the host.

### 11.1 Guix backend

Use `guix shell --container --network --emulate-fhs` only for resolve (registry
metadata). Realization uses a separate `--container --emulate-fhs` invocation
**without** `--network`, exposing `cache/` read-only and the transaction's
staging dir read-write. Host `$HOME`, workspace, credentials, and pool `home/`
are absent from these containers.

### 11.2 OCI backend for non-Guix hosts

When Guix is unavailable (Arch, macOS, Windows), use a small pinned
provisioning OCI image through Docker or Podman (auto-detected, `--backend`
overrides). The image contains only resolver/build prerequisites (node+npm,
tar, coreutils). Its containers get:

- no project workspace;
- no host home;
- no Docker socket;
- verified cache mounted read-only during realization;
- pool staging mounted read-write;
- network only during resolve;
- explicit target platform (`--platform linux/amd64|arm64` to the runtime).

The provisioning image reference **and digest** are lock inputs via
`recipe_revision` (§8). Upgrading the image invalidates recipe revisions and
requires a new generation; it cannot silently change existing generations.

This solves host portability: macOS and Windows users create Linux artifacts
for Linux Dev Containers instead of installing host-native binaries into the
pool. Mount sources passed to `docker run` are `ToSlash`-converted host paths
(§6.3).

### 11.3 Go seams (what workers implement)

New package `internal/pool` (layout, manifest/lock/observations types, atomic
ops, locking, pointer activation, receipts, GC) with subpackages
`internal/pool/source` and `internal/pool/backend`. `cmd/harness.go` stays a
thin cobra layer. The two seams:

```go
// A Backend runs one Linux container step. It is the ONLY place that knows
// how to invoke guix / docker / podman.
type Backend interface {
    Name() string            // "guix" | "oci"
    Available() bool
    // Run executes argv in a Linux container per spec. Network is off unless
    // spec.Network. Mounts are (HostPath, ContainerPath, ReadOnly) triples.
    Run(ctx context.Context, spec RunSpec) error
}

// A Source implements one artifact-acquisition family ("npm", "archive").
type Source interface {
    // Refresh lists candidates from registry metadata. Pure Go, host-side.
    Refresh(ctx context.Context, d harness.Descriptor) ([]Candidate, error)
    // Resolve produces the complete lock entry for one exact candidate.
    // May call b.Run (npm needs a backend); archive sources must not.
    Resolve(ctx context.Context, d harness.Descriptor, c Candidate, b Backend) (LockEntry, error)
    // Fetch downloads every artifact in e into cache, verifying integrity
    // before admission. Pure Go, host-side, locked URLs only.
    Fetch(ctx context.Context, e LockEntry, cache *Cache) error
    // Realize builds one platform generation in stagingDir via b.Run with
    // Network=false, then post-processes to satisfy §6.4 invariants.
    Realize(ctx context.Context, e LockEntry, p PlatformKey, b Backend, stagingDir string) (Receipt, error)
}
```

Orchestration (phase ordering, quarantine eligibility, locking, activation,
rollback, GC) lives once in `internal/pool` and is unit-testable with fake
`Backend`/`Source` implementations — no container runtime in unit tests.

### 11.4 Realization of an npm generation (normative)

Inside the backend container, network off, staging rw, cache ro:

1. Materialize `package.json` + the locked `package-lock.json` in the staging
   generation root.
2. Seed npm's cache offline: `npm cache add <cache/sha256/…>` for every locked
   tarball (cache objects are the verified tarballs; npm re-verifies its
   sha512 SRI on add).
3. `npm ci --offline --foreground-scripts` — lifecycle scripts run with no
   network; a script that needs one fails the realization (correct: that
   dependency was not locked).
4. Unpack the pinned node runtime artifact into `runtime/node/`.
5. **De-symlink and de-shebang** (§6.4): for each declared harness executable,
   replace npm's `bin/<name>` symlink+`#!/usr/bin/env node` shim with a
   generated `$0`-relative wrapper:

   ```sh
   #!/bin/sh
   d=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
   exec "$d/runtime/node/bin/node" "$d/lib/node_modules/@anthropic-ai/claude-code/cli.js" "$@"
   ```

   Remaining `node_modules/.bin` symlinks are replaced with equivalent
   wrappers or copies; the symlink scan then enforces zero symlinks.
6. Write `receipt.json`; the host verifies it (§11.5), renames the tree into
   `runtime/generations/<generation-id>/`, and activates (§5.2).

The generation therefore runs with **its own** node, on any image with
`/bin/sh` — the "no dependency on project Node" goal made mechanical. Archive
sources skip steps 1–3/5 and just unpack + wrap.

### 11.5 Receipt

`receipt.json` records: schema version, harness, version, platform key,
`recipe_revision`, lock-entry hash, backend name + image digest (oci) or guix
channel commit (best effort), and the file manifest — sorted
(relative path, executable flag, size, sha256) — plus the digest over that
manifest, which is the generation digest referenced by the lock and the
generation id. Identical lock + recipe on different hosts/backends must yield
identical file-manifest digests (§17 cross-backend test); the
environment-identity fields sit outside the digested manifest so they don't
break that equality.

## 12. Container integration

### 12.1 Guix

`wraptool up --runtime=guix` resolves the current platform key, then mounts
(replacing today's `bin/`+`lib/` exposes, preserving their trust boundary):

```text
pool/runtime  -> same host path, read-only   (--expose)
pool/home     -> same host path, read-write  (--share)
```

It **appends** `<pool>/runtime/bin` to `PATH` (append, never prepend — pool
tools must not shadow project `git`/`go`/`bash`) and sets `HOME` to pool home,
as today. No project manifest contains harness package versions.

### 12.2 Dev Containers driven by `wraptool up`

`wraptool up --runtime=devcontainer` adds:

```text
pool/runtime  -> /opt/wraptool-harness/runtime, read-only  (bind, ro)
pool/home     -> /home/wraptool-harness,        read-write (bind)
```

Environment (home mount + `HOME` pinning unchanged from the shipped feature):

```text
HOME=/home/wraptool-harness            # feature containerEnv invariant, unchanged
PATH=<existing PATH>:/opt/wraptool-harness/runtime/bin
```

Mount sources are `ToSlash`-converted `paths.DataDir()` results (§6.3). As
today, `up` skips injecting a mount the config already declares (duplicate
bind targets are a Docker error) — the existing `devcontainerDeclaresPoolMount`
check extends to both targets, and `up` warns when a declared source does not
exist on this host (the mixed-OS case, §12.3).

Before container creation, `up` verifies the selected harness has an active
pointer for the target platform key. A missing realization fails with:

```text
claude is locked but not realized for linux-arm64-glibc;
run: wraptool harness sync --platform linux-arm64-glibc
```

It never installs from inside a project container.

### 12.3 Standalone editor / `devcontainer up`

Rev 1 claimed `wraptool init devcontainer` "emits read-only runtime and
writable home mounts" as if that were fully portable. It is not, and the
limits must be explicit ([`harness-pool-devcontainer.md` §3]: Dev Container
variable substitution has no XDG/known-folder resolver and no conditionals, so
no committed mount source is correct on every host OS):

- The scaffolder emits both mounts using the existing `poolHomeMountSource`
  strategy (`cmd/init_devcontainer.go`): source rewritten to
  `${localEnv:HOME}/<ToSlash rel>` when the data dir is under the home dir
  (true for the defaults on all three OSes; on Windows `HOME` must be set —
  already documented), else the literal host path. The runtime mount adds
  `,readonly`.
- A committed config is therefore correct for **the OS family of the developer
  who scaffolded it**. On a mixed-OS team, teammates on another OS use
  `wraptool up` (which injects OS-correct mounts and skips declared-but-alien
  ones) or adjust the committed sources. This degradation is documented, not
  papered over.
- Raw `devcontainer up` without wraptool: pool semantics work when the
  developer previously ran `wraptool harness install <name>`; the feature's
  post-create check fails with exactly that command when the selected
  executable is absent from the mounted runtime.

Feature changes (new major of `features/src/wraptool`):

- remove `provision` and `version` from the new major Feature schema;
- retain `harness` for MCP config shape;
- retain `connect`, `port`, and `url`;
- keep the `HOME` containerEnv invariant; append the dispatcher path to PATH;
- perform no network access and install no harness package;
- fail clearly at post-create when the selected executable is absent.

The old Feature major remains available for existing configurations. Scaffolder
and docs move to the new major only after pool commands support OCI
provisioning.

## 13. Shared state compatibility

One active version per harness prevents simultaneous project-selected versions
from fighting over shared state. Rollback can still expose state written by a
newer version to an older binary. Before activation:

1. snapshot or copy harness-owned state where practical;
2. record snapshot identifier in generation receipt;
3. run descriptor-specific compatibility checks, if known.

Rollback warns when state changed after newer generation activation. Automatic
state rollback is unsafe because `home/` may contain user conversations and
edits created after upgrade. Default rollback switches the pointer only and
preserves state; restoring a snapshot requires a separate explicit command.

State remains harness-specific logically even though many harnesses expect one
`HOME`. Descriptors declare owned paths for backup and diagnostics. They must
never classify unrelated paths as safe to delete.

## 14. Threat model

### Mitigated

- **Project changes harness version:** project no longer owns version.
- **Compromised project poisons executable:** runtime mount is read-only and
  staging/manifest/cache are not mounted.
- **Mutable `latest`:** lock contains exact versions and integrity.
- **Changed transitive dependency:** complete dependency lock detects it.
- **Drive-by new release:** payload remains undownloaded during quarantine.
- **Installer downloads arbitrary current content:** realization has no network;
  all inputs must exist in lock/cache.
- **Partial upgrade:** immutable generations plus atomic pointer activation.

### Residual

- A malicious release surviving quarantine can still be installed.
- Registry metadata can lie if the registry itself is compromised. Signatures
  and independent transparency data improve this where available.
- First install trusts descriptor source identity and the provisioning image.
- Shared writable `home/` lets one project alter harness config used by another.
  Hooks/plugins in that state can execute later. This is existing accepted risk
  and must remain prominent in docs.
- Developer account compromise can alter pool files directly.
- Harness service credentials are intentionally available inside every project
  using shared `home/`.

## 15. Concurrency and failure handling

All mutating commands take the single advisory pool lock (§6.5). Read-only
`list` runs concurrently and reports in-progress staging separately.

Failures before activation leave the active pointer untouched. Staging
directories carry transaction IDs and are cleaned on the next locked command
after validation. Cache objects are written to temporary names, verified, then
renamed (§6.4).

An active generation is never garbage-collected. Default GC retains at least
two previous generations per harness/platform and all generations referenced
by lock or rollback metadata.

## 16. Migration

The existing pool (`bin/`, `lib/`, `home/` from `ensureHarnessPool`) keeps
working until Phase 4; `home/` is never touched by migration. The new tree
grows alongside it; runtimes prefer `runtime/` whenever an active pointer
exists for the needed platform, else fall back to legacy `bin/`+`lib/` with a
deprecation notice.

### Phase 1: manifest and lock around the current Guix pool

1. Add strict manifest/lock/observation types, `internal/pool`, the portable
   lock (§6.5), and the shared atomic-write helper.
2. Add exact npm version and dependency resolution (§8.1).
3. Change `install` semantics; add `refresh`, `update`, `sync`, `rollback`,
   `policy set`.
4. Import existing installations by inspecting installed npm package metadata
   and writing an explicit lock, or require a clean reinstall when integrity
   cannot be proven. Legacy `bin/`+`lib/` remain the executed path.

No Dev Container behavior changes yet.

### Phase 2: immutable generations and offline realization

1. Add cache, receipts, dispatchers, pointer activation, symlink-free
   post-processing (§11.4), and receipt validation (§11.5).
2. Bundle pinned runtimes.
3. Replace `curl | bash` descriptors (agy) with lockable artifact adapters.
4. Mount `runtime/` read-only in Guix containers (§12.1); legacy fallback
   retained.

### Phase 3: OCI provisioner

1. Build and pin the minimal provisioning image.
2. Implement Docker and Podman `Backend`s behind the §11.3 interface.
3. Test Linux artifacts produced from Linux, macOS, and Windows hosts
   (ToSlash mount sources, no-symlink invariant on NTFS).
4. Add arch/libc target selection and diagnostics (§5.1). Both arches realize
   by default; **glibc only** — musl realization exists solely behind an
   explicit `sync --platform linux-<arch>-musl` (deferred target, §5.1).
5. Retire the guix-absent error in `harness install` that points at the
   provisioning feature (`cmd/harness.go`) — the command now works everywhere.

### Phase 4: Dev Container cutover

1. Publish the new wiring-only wraptool Feature major.
2. Update `wraptool init devcontainer` mounts (§12.3) and remove the project
   version option.
3. Update `wraptool up` to inject runtime (ro) + home (rw) mounts.
4. Migrate examples and this repository.
5. Deprecate the per-image provisioning Feature major; retain it for
   compatibility. `gc` learns to remove legacy `bin/`+`lib/`.

### Phase 5: quarantine automation

1. Metadata-only scheduled refresh guidance.
2. Local observations and release-age eligibility.
3. Bypass audit events and candidate warnings.
4. Optional upstream signature/transparency adapters.

## 17. Tests

### Unit (no container runtime required — fake Backend/Source)

- strict manifest parsing and version constraints;
- deterministic lock serialization (byte-stable round trip);
- eligibility boundaries (`<`, `==`, and `>` minimum age);
- global policy, per-harness override, inheritance, and `0h` disable semantics;
- project configuration and environment cannot weaken pool policy;
- first-seen timestamp never moves on repeated refresh;
- yanked and prerelease filtering;
- integrity mismatch rejection (both sha512 SRI and sha256 cache address);
- recipe/provisioner revision invalidation;
- dispatcher golden file per harness (content of §5.3, arch/libc cases via
  `WRAPTOOL_PLATFORM`);
- pointer-file activation: concurrent reader never sees partial content;
  rollback rewrites pointer only;
- receipt digest identical for identical trees regardless of host mtimes;
  case-collision detection; symlink scan failure;
- mount-spec generation on a faked Windows path (`C:\Users\...` →
  `C:/Users/...`), and `${localEnv:HOME}` rewriting for both mounts;
- lock contention: second mutating command blocks/fails cleanly;
- GC retention invariants;
- exhaustive source/platform enum switches (the `exhaustive` linter enforces).

### Integration

- install the same locked Claude generation through Guix and OCI backends;
  compare receipt file-manifest digests for equality and the reported harness
  version;
- realize from cache with network disabled (backend run asserts no egress);
- prove no payload request occurs for a quarantined candidate;
- prove a project container cannot write runtime, manifest, lock, cache, or
  staging, and **can** write pool home;
- a generation relocates: realize, copy the pool elsewhere, dispatcher still
  execs (position independence, §6.3/§11.4);
- run the generation's entrypoint on a node-less image (`debian:bookworm-slim`
  without the node feature) — bundled runtime suffices;
- two projects see the same active harness version and shared state; upgrade
  once, recreate both, both see the new version; rollback once, both see the
  retained previous version;
- macOS/Windows host provisioner emits a Linux target artifact;
- interrupted fetch/build leaves the prior active generation usable.

## 18. Documentation changes

Terminology becomes precise:

- **harness pool**: manifest, lock, immutable executable generations, and state;
- **runtime**: read-only executable/runtime subtree;
- **harness home**: writable shared auth/config/skills subtree;
- **project toolchain**: Guix manifest or Dev Container language environment,
  excluding harnesses.

Dev Container docs must stop recommending project-owned harness versions.
Security docs must explain quarantine limitations and shared-home residual
risk. Windows docs extend the existing "HOME must be set" note to both pool
mounts. `wraptool harness list` becomes the primary diagnostic for both
runtimes. `CLAUDE.md`'s dependency list gains the §6.7 additions when they
land.

## 19. Result

After migration, a developer performs one workflow on any supported host:

```bash
wraptool harness install claude
wraptool up --harness claude
```

Every project uses the same locked, quarantined, read-only harness generation
and the same writable harness state — regardless of whether the host is a Guix
box, a Linux laptop with Docker, or a macOS/Windows machine driving Docker
Desktop. Projects choose which harness to run, but cannot silently choose what
code that harness name executes.
