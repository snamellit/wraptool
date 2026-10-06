# Adding oh-my-pi (`omp`): a release-binary pool source

Status: **implemented** (WP0-WP4; open questions and deviations in §9 and §12)

Depends on `design/harness/platform-scoped-fetch.md` (see §5). Extends
`design/harness/unified-harness-pool.md` §6.2 (phase matrix), §8 (lock), §11.3
(Go seams), and §15 (gc retention) with a third source kind.

## 1. Problem

oh-my-pi (`omp`, [can1357/oh-my-pi](https://github.com/can1357/oh-my-pi), MIT)
is a terminal coding agent forked from pi. It adds LSP, debugger and subagent
support. It fits the harness pool's audience: a CLI that runs in the project
container. No existing pool source can acquire it:

- **npm is out.** `@oh-my-pi/pi-coding-agent` declares `engines: {bun: ">=1.3.14"}`
  and its `bin` is a Bun script. The pool bundles a Node runtime, not Bun.
- **The archive source is agy-specific.** `internal/pool/source/archive.go`
  speaks one channel format: a per-platform JSON manifest, a hex sha512, and
  a `.tar.gz` artifact. omp publishes none of those.

What omp does publish, per GitHub release (verified on `v18.6.1`, 2026-10-04):

| Asset | Pool platform |
|---|---|
| `omp-linux-x64` | `linux-amd64-glibc` |
| `omp-linux-arm64` | `linux-arm64-glibc` |
| `omp-linux-musl-x64` | `linux-amd64-musl` |
| `omp-linux-musl-arm64` | `linux-arm64-musl` |
| `SHA256SUMS.txt` | checksums for every asset, `<hex>  <name>` lines |

Each binary is a raw, uncompressed, Bun-compiled ELF of about 281 MB
(`omp-linux-x64`: 280,802,784 bytes). It is dynamically linked against glibc
only, at symbol version 2.17 or later, through `/lib64/ld-linux-x86-64.so.2`.
The musl builds cover Alpine. `omp --version` runs offline and writes nothing
to `HOME`. The downloaded `omp-linux-x64` matched its `SHA256SUMS.txt` entry.

The four assets map exactly onto the closed `PlatformKey` enum, so omp needs
no new platform handling, only a new way to acquire artifacts.

## 2. Decision

Add a third pool source kind, **`release`**: one binary per platform, attached
to a GitHub release and checksummed by a `SHA256SUMS.txt` asset in the same
release. Register `omp` as the first harness on it.

Do not generalize `Archive` into a configurable multi-channel source. The
formats share no wire shape. What they do share (download with progress,
streaming verification, cache admission, placing a file at `bin/<name>`) is
factored into helpers that both sources call.

The `release` source is deliberately generic over its descriptor. Go, Rust
and Bun-compiled agents commonly ship exactly this way, so the next such
harness is a descriptor entry, not a new source.

## 3. Pool changes (`internal/pool`)

1. **`SourceKind`.** Add `SourceRelease SourceKind = "release"`. Every
   exhaustive switch over `SourceKind` gains a case. golangci-lint's
   `exhaustive` check finds them. `recipeRevisionForSource` is the main one.
2. **Lock shape.** Add `LockHarness.Release *LockRelease`:

   ```go
   type LockRelease struct {
       Repo      string                                `json:"repo"` // "can1357/oh-my-pi"
       Tag       string                                `json:"tag"`  // "v18.6.1"
       Artifacts map[PlatformKey]LockReleaseArtifact   `json:"artifacts"`
   }
   type LockReleaseArtifact struct {
       URL    string `json:"url"`
       SHA256 string `json:"sha256"` // upstream checksum = pool cache address
   }
   ```

   Here the upstream integrity value and the pool's cache address are the same
   digest, so one field carries both. It is set at resolve time, unlike
   `LockArchiveArtifact.SHA256`, which stays empty until fetch. The field is
   additive and `omitempty`, so existing locks are unaffected.
3. **Validation.** `(*Lock).Validate` checks each artifact: a valid
   `PlatformKey`, an `https://` URL, and 64 lowercase hex characters for
   `SHA256`. It also checks a non-empty `Repo` and `Tag`.
4. **gc retention.** `retainedCacheDigests` must walk `Release.Artifacts`.
   This is called out explicitly because wave2-integration-fixes item 2 was
   exactly this bug for `Archive.Artifacts`: gc deleted a still-locked
   harness's cache object. Add a retention test in the same change.
5. **`recipe_revision`.** Add `releaseRecipeRevision`, a hash of the repo,
   the platform-to-asset mapping, the checksum asset name, and its own
   `releaseRecipeFormatVersion` (starting at 1, golden-value pinned). It is
   independent of npm's and archive's format versions. A mapping change in
   the descriptor then self-heals through the existing recipe-mismatch path.
6. **Eligibility.** No change is needed. Unlike agy, candidates carry a
   **real** `PublishedAt` (§4.1), so `minimum_release_age` and
   `minimum_observation_age` both apply, with no synthesized timestamp.

## 4. The `release` source (`internal/pool/source/release.go`)

It satisfies `pool.Source` structurally, like `NPM` and `Archive`. All four
methods are pure Go on the host. Like the archive source, it needs no backend
in any phase (§6.2), so `wraptool harness install omp` works on a host with
neither guix nor docker/podman.

### 4.1 Refresh: versions from the npm packument

The release lists no version history the pool can fetch cheaply, so versions
come from the npm packument of the descriptor's `VersionFeed` package
(`@oh-my-pi/pi-coding-agent`). Its `time` map gives every version a genuine
publish timestamp. This reuses the NPM source's packument fetch and parsing,
prerelease filtering included.

Alternatives considered:

- **The GitHub REST API.** Rejected: unauthenticated callers get 60 requests
  an hour, and that limit was hit while researching this note. The pool must
  not depend on a token.
- **`releases.atom`.** Rejected: it shows only the most recent releases and
  has no stable schema.

npm and the GitHub tag share one version line: npm `18.6.1` is release
`v18.6.1`. That was only checked for the latest release; see §9.

### 4.2 Resolve: the checksum file is the lock

`Resolve` takes `(pkg, version)`, ignores `b`, `workDir` and `nodeVersion`,
and does the following:

1. Fetches
   `https://github.com/<Repo>/releases/download/v<version>/SHA256SUMS.txt`.
2. Parses it strictly: `<64 lowercase hex><two spaces><name>` per line.
   A malformed line, or a duplicate name with different digests, fails.
3. Maps each `PlatformKey` to its asset name from the descriptor, looks up
   the digest, and builds the artifact URL from the same release.
4. Returns the lock entry with `Release` populated.

Two failure rules:

- **A missing platform is not an error.** If a platform's asset is absent
  from the checksum file, that platform is left out of `Artifacts`. This
  mirrors the archive source's mid-rollout ruling (§8.1 ruling 3).
- **No Linux platform at all is an error.** That includes a missing
  `SHA256SUMS.txt` (HTTP 404). npm can publish before the release assets are
  uploaded, so the message says the assets are not published yet and to
  retry later. It must not read as a corrupt release.

### 4.3 Fetch

Fetch streams each locked artifact into `cache/sha256/<digest>`, verifying
the digest as it downloads. An object already in the cache is re-hashed and
reused, as the other sources already do. It reports progress through
`SetProgress`, like the other sources.

### 4.4 Realize

There is no archive to unpack. `Realize` places the cache object at
`<staging>/bin/<binName>` with mode `0o755`, then writes a receipt with
backend `none`, exactly as `Archive.Realize` does. The rest is the existing
`RealizeGeneration` transaction: verify, rename into `runtime/generations/<id>`,
then activate.

Use `hardlinkOrCopy`. A hardlink saves 281 MB per generation, and gc
removing the cache object later leaves the generation intact (link count).
The hardlink also sets the cache object's mode to `0o755`. That is harmless,
because cache verification is content-based.

## 5. Fetch must be platform-scoped first

`Source.Fetch` currently downloads every artifact the lock names, whichever
platform is being realized; `Archive.Fetch` already does this for agy. For
omp that means four binaries of about 281 MB each: **about 1.1 GB per
`update`, of which a host uses one quarter.** At the ~0.85 MB/s observed on
a conference network, that is over 20 minutes.

So the `Fetch(targets)` interface change from
`design/harness/platform-scoped-fetch.md` is a **prerequisite** (WP0 below),
not a follow-up. The release source implements the scoped signature from its
first commit.

Related: an interrupted download currently restarts from zero. A connection
reset happened partway through the 281 MB download while researching this
note. Resumable downloads (HTTP `Range`) are a non-goal of the
platform-scoped-fetch note, and §9 asks whether omp changes that.

## 6. Harness wiring

### 6.1 Descriptor (`internal/harness`)

```go
"omp": {Name: "omp", Auth: AuthHeader, Bin: "omp", Render: renderOMP,
    PoolRelease: &ReleaseSpec{
        Repo:        "can1357/oh-my-pi",
        VersionFeed: "@oh-my-pi/pi-coding-agent",
        Checksums:   "SHA256SUMS.txt",
        Assets: map[string]string{ // keyed by pool.PlatformKey's string form
            "linux-amd64-glibc": "omp-linux-x64",
            "linux-arm64-glibc": "omp-linux-arm64",
            "linux-amd64-musl":  "omp-linux-musl-x64",
            "linux-arm64-musl":  "omp-linux-musl-arm64",
        },
    }},
```

`Assets` is keyed by string so `internal/harness` keeps no dependency on
`internal/pool`. The release source validates each key with
`pool.ParsePlatformKey`, and a registry test asserts every key parses.

Related changes:

- `PoolLockable()` becomes true for `PoolPackage`, `PoolArchive` or
  `PoolRelease`.
- `TestRegistryNeverSetsBothPoolPackageAndPoolArchive` becomes "at most one of
  the three".
- `PoolPackageID()` returns `Repo` for a release harness. That makes the
  observation and lock namespace `release/can1357/oh-my-pi/<version>`, so a
  repository move is a new identity and forces a fresh resolve.
- `Name == Bin` holds (`omp`), as the existing registry test requires.

### 6.2 `cmd/harness.go`

- `poolSourceKind` and `newSource` dispatch on `PoolRelease`.
- `harnessBackend` returns a nil backend for it, as it does for archive.
- `refreshNodeLineIfNPM` is already a no-op for any non-npm kind.

**omp is the first pool-only harness**, meaning it has no legacy `Install`
recipe. Today `maybeInstallLocked` calls `legacyInstallVersionLocked`, which
errors with "no automated installer" when `Install` is nil.
`warnInsteadOfFailForRealizedGeneration` would then downgrade that to a
warning on every install. Instead, a harness with no `Install` recipe whose
generation was realized and activated **skips the legacy step silently**.
If realize failed, the command fails; there is no legacy fallback to try.
This is the §16 Phase 4 end state, reached early for one harness.

### 6.3 MCP configuration

| | |
|---|---|
| Project file | `.omp/mcp.json` |
| Global file | `~/.omp/agent/mcp.json` (not the older `~/.omp/mcp.json`, which omp no longer reads) |
| Shape | `{"mcpServers": {"wraptool": {"type": "http", "url": ".../mcp?cwd=...", "headers": {"Authorization": "Bearer ..."}}}}` |

omp recommends `http` (Streamable HTTP) for new remote servers, so
`renderOMP` points at `/mcp` through `streamableURL`, as agy does. omp also
supports `sse`, but there is no reason to pick the legacy transport.

The matching `connect.sh` case is:

```sh
omp) EP="/mcp"; TOP="mcpServers"; URL_KEY="url"; TYPE='"type":"http",'; EXTRA=''; TARGET="${WT_CONFIG_OUT:-$PWD/.omp/mcp.json}" ;;
```

`TestHarnessParityWithConnectSh` gains the row, and
`TestFeatureEnumMatchesRegistry` forces the feature's `harness` enum to gain
`omp`. That bumps the feature to **1.2.0**. Adding an enum member is additive
under the compatibility promise.

omp expands `${VAR}` in `url` and `headers`. Both runtimes already export
`WRAPTOOL_TOKEN` and `WRAPTOOL_CWD` (`cmd/guixcontainer.go`, and the feature's
`containerEnv`/`remoteEnv`). So omp could get a token-free config. This note
keeps the existing convention (literal token, 0600 file, git-excluded) for
parity with every other harness. Moving all harnesses that support expansion
to token-free configs is a separate decision.

### 6.4 omp reads other tools' configuration

Project-level, omp reads `.omp/mcp.json` first, then the user file, then a
project-root `mcp.json` or `.mcp.json` as a fallback. A duplicate server name
resolves to the first definition. A project wired for claude therefore has
its `.mcp.json` `wraptool` entry shadowed by `.omp/mcp.json`, so there is no
double registration.

omp also imports rules, skills and MCP servers from `.claude`, `.cursor`,
`.codex` and similar directories. In a container, `HOME` is the shared pool
`home/`, so omp reads **other harnesses' state in the pool home**, not the
developer's host home. The pool home is already one trust domain across
harnesses and projects (`docs/getting-started.qmd`, "Harness HOME is shared
and writable"), so this is not a new boundary. omp exercises it actively rather
than incidentally, though, and the docs should say so where omp is listed.

## 7. Threat model delta

- **Credential isolation is unchanged.** omp runs in the same container,
  with the same mount namespace and the same mediation through wraptool's
  MCP tools.
- **Supply-chain trust is the same class as agy.** The integrity root is a
  checksum file fetched over HTTPS from the same origin as the binary. A
  compromised release can ship a matching binary and checksum, which no pool
  check detects. This is **mitigated, not prevented**, by quarantine: omp
  releases several times a day (660 npm versions since 2026-01-02), so the
  default `minimum_release_age` keeps the pool about a week behind upstream.
  That is the intended trade.
- **omp has a larger built-in tool surface** (browser automation, and
  `cloudflared` tunnel invocation appears in the binary). wraptool does not
  mediate container network egress. That is an existing limitation, not
  specific to omp, but omp makes it easier to reach.

## 8. Work packages

The packages are ordered by dependency. WP2 and WP3 touch disjoint files and
can run in parallel once WP1 lands.

| WP | Scope | Files |
|---|---|---|
| WP0 | `Fetch(targets)` from `platform-scoped-fetch.md`, at least the interface change plus the archive and runtime scoping | `internal/pool`, `internal/pool/source` |
| WP1 | `SourceRelease`, `LockRelease` and its validation, gc retention, `releaseRecipeRevision` (golden), exhaustive switches | `internal/pool` |
| WP2 | The `release` source: Refresh, Resolve, Fetch, Realize. Factor shared download, verify and bin-placement helpers out of `archive.go` | `internal/pool/source` |
| WP3 | `ReleaseSpec`, `PoolLockable`/`PoolPackageID`, registry invariants, `omp` descriptor, `renderOMP`, cmd dispatch, skipping legacy install for pool-only harnesses | `internal/harness`, `cmd` |
| WP4 | `connect.sh` case, feature enum, feature 1.2.0, selftest; docs (`mcp-integration`, `cli-reference`, `getting-started`, `terminology`, `harness-pool`); CHANGELOG; CLAUDE.md architecture entries for `internal/pool` and `cmd/harness.go` | `features`, `docs`, `CLAUDE.md` |

## 9. Open questions

1. **Self-update on a read-only mount.** Still open. The binary contains a
   `checkForUpdates` path. Confirm omp does not try to replace its own binary,
   and that a failed check on the read-only `runtime/` mount neither blocks
   startup nor prints an error each session. Find the setting or environment
   variable that disables it, and set it in the dispatcher or document it.
   The same risk was confirmed for `claude` on 2026-10-06. Its self-updater
   ran `npm install -g` into the node feature's writable prefix, and the new
   copy shadowed the pool binary on `PATH`. Both runtimes now set
   `DISABLE_AUTOUPDATER=1`, which covers `claude` only. omp needs its own
   switch, set the same way.
2. **Older versions.** Answered (verified 2026-10-06). `SHA256SUMS.txt` is
   published from v17.2.10 onward. All four Linux assets were present on every
   release checked: 17.2.10, 17.4.2, 18.0.0, 18.5.0, 18.6.0, 18.6.1. v17.2.9
   and earlier have binaries but no checksum file (HTTP 404), so the release
   source cannot lock them, and Resolve's error says so ("not published yet or
   predates checksum publication"). The checksum file also lists darwin,
   windows and `omp-browser-relay-extension.zip`, which the parser ignores.
3. **Signatures.** Still open: unverified, because the GitHub API was
   rate-limited during research.
4. **Resumable downloads.** Partly closed. The release source has a stall
   detector: a download aborts after 60 seconds without receiving any data
   (`downloadStallTimeout` in `internal/pool/source/release.go`), while the
   overall transfer time is unbounded, so a slow but moving link succeeds.
   HTTP `Range` resume is still not implemented: an interrupted or stalled
   download restarts from zero. The npm and archive sources still use a
   wall-clock timeout.
5. **Token-free configs.** Still open, out of scope here.

## 10. Non-goals

- macOS and Windows assets. Pool platforms are Linux-only by design (§5.1).
- The npm/Bun distribution of omp, or bundling a Bun runtime in the pool.
- User-defined release sources. `ReleaseSpec` is set by a descriptor in
  code, not in `manifest.yaml`.
- Hermes and OpenClaw. They were assessed alongside omp: Hermes needs a
  Python/uv source, and OpenClaw is not a coding harness.

## 11. Tests

Unit tests use no container runtime and a fake HTTP server:

- **Refresh:** a packument fixture gives candidates with real `PublishedAt`;
  prereleases are filtered.
- **Resolve:**
  - checksum parsing: a valid file, a malformed line, conflicting duplicates;
  - a missing platform is omitted;
  - zero Linux assets, or a 404 on `SHA256SUMS.txt`, gives the
    "not published yet" error;
  - artifact URLs are built from the repo and tag.
- **Fetch:** a digest mismatch is rejected and never admitted to the cache;
  a cached object is re-hashed and reused; only the target platform is
  downloaded (WP0).
- **Realize:** `bin/omp` exists with mode `0755`, the receipt is valid, and
  a zero-file generation is refused, as for every source.
- **Lock:** `Validate` accepts and rejects the shapes in §3.3, and the lock
  hash covers `Release`.
- **gc:** a locked release artifact's cache object survives `gc`. This is a
  regression test in the spirit of wave2 item 2.
- **Recipe revision:** the golden value is pinned, and the exhaustive switch
  has a case for `SourceRelease`.
- **Registry:** at most one source field is set; every `Assets` key parses;
  a pool-only install skips the legacy step without a warning.
- **Wiring:** connect.sh parity and the feature-enum test.

Manual checks:

- `wraptool harness install omp` on a host with no guix, docker or podman.
- `wraptool up --harness omp` on both the guix and devcontainer runtimes.
- `wraptool_info` is reachable from inside omp.

## 12. Implementation notes and deviations

- **WP0 scoped archive and runtime artifacts only.** npm package scoping is
  still blocked on step 0 of `platform-scoped-fetch.md`; npm packages are all
  fetched. `sync` persists backfilled cache addresses (`persistCacheAddresses`).
- **The shared download helper was not extracted** (§8, WP2). The release
  source has its own sha256 `downloadVerify`; `archive.go` is unchanged.
- **`recipe_revision` hashes more than §3.5 lists.** `releaseRecipeRevision`
  (format 1) also hashes the `VersionFeed` and the tag scheme (`v{version}`),
  besides the repo, asset mapping and checksum name.
- **A realize-only update does not fetch.** When the lock already agrees,
  `update` and `install` realize without calling `Fetch`. A pool shared by
  hosts of different architectures (for example over NFS) can therefore hold
  a lock whose artifact for the second host was never fetched; realize then
  fails with "cache object … not found", and `wraptool harness sync` repairs
  it. This is the follow-up "fetch before a realize-only update" in
  `platform-scoped-fetch.md`, accepted here as an edge case. For `omp` the
  command fails outright, because there is no legacy installer to fall back to.
