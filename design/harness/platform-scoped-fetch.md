# Design: Platform-scoped fetch for the harness pool

Status: proposed

Amends `design/harness/unified-harness-pool.md` §6.2 (Fetch), §8 (lock), §10
(`sync`), and §11.4 (realization). Nothing here changes the trust model: the
upstream integrity value stays the root every download is verified against.

## Problem

`Source.Fetch` downloads every artifact the lock entry lists, whatever the
platform being realized. For npm-sourced harnesses that list includes every
optional per-platform binary package. `@anthropic-ai/claude-code` 2.1.289
declares eight of them, at ~100 MB compressed (~240 MB unpacked) each:

```text
linux-x64  linux-arm64  linux-x64-musl  linux-arm64-musl
darwin-x64 darwin-arm64 win32-x64       win32-arm64
```

So `wraptool harness update` fetches about 860 MB (eight binaries plus the
glibc Node runtime for both architectures) to realize one generation that
uses about 130 MB of it. On a conference network (observed at ~0.85 MB/s)
that is 15–20 silent minutes, and it gets close to `downloadTimeout`'s
10-minute cap for any single object on a slower link. The archive source has
the same shape: `Archive.Fetch` downloads all four per-platform agy
artifacts.

Two classes of waste:

1. **Unrealizable anywhere.** `PlatformKey` is Linux-only (§5.1). The darwin
   and win32 packages can never be staged into any generation on any pool,
   yet every update downloads them. That is half the claude payload.
2. **Not needed yet.** The other platform keys' binaries are needed only if
   this pool ever realizes those keys, which is rare. `install` and `update`
   realize the host's own key; another key needs an explicit
   `sync --platform`.

Progress reporting (landed separately: `source.SetProgress`) makes the wait
visible but does not shorten it.

## What the lock already gives us

- The verbatim `package-lock.json` (`NPMLock.Lockfile`) carries each
  package's `os`, `cpu`, and (npm ≥ 10) `libc` gating, plus `optional`.
  The claude platform packages declare all three, e.g. `linux-x64-musl` is
  `os: [linux], cpu: [x64], libc: [musl]`. These map one-to-one onto
  `PlatformKey`'s (os, arch, libc) triple.
- `NPMLockPackage.SHA256` is a **cache address**, not a trust root. Every
  download is verified against the upstream sha512 SRI (`Integrity`) before
  it is admitted to `cache/`. Leaving `SHA256` empty loses no integrity, and
  no longer loses the cache hit either: the integrity index
  (`cache/sha256/.by-sha512/`, `internal/pool/source/cacheindex.go`) finds
  an already-cached object by its sha512 and re-verifies it before use.
- `Lock.Validate` does not require `NPMLockPackage.SHA256`. `gc`'s
  `retainedCacheDigests` already skips empty addresses.
  `LockArchiveArtifact.SHA256` is already `omitempty`.
- `Receipt.LockEntryHash` is recorded but never compared against the current
  lock, and `GenerationID` derives from the file manifest only. Filling in a
  cache address after the fact therefore invalidates no generation.
- Runtime artifacts (`LockRuntime.Artifacts`) record their sha256 from
  `SHASUMS256.txt` at resolve time, so per-platform scoping costs nothing
  for them: the cache-hit check works without a prior fetch.

## Design

### 1. Fetch takes its targets

```go
Fetch(ctx context.Context, h *LockHarness, cacheDir string, targets []PlatformKey) error
```

`Fetch` downloads an artifact only if some key in `targets` needs it:

- **npm package**: needed unless the lockfile entry is `optional: true` *and*
  its `os`/`cpu`/`libc` gating excludes every target key. A non-optional
  package is always fetched, whatever its gating; if it really excluded the
  target, `npm ci` would fail anyway and should fail loudly.
- **Runtime artifact**: needed iff its key is in `targets`.
- **Archive artifact**: needed iff its key is in `targets`.

Callers:

| Caller | `targets` |
|---|---|
| `UpdateHarnessLocked` (update, install) | the realize target (the host's key, as today) |
| `SyncHarnessLocked` | the `--platform` key |

The gating evaluation is a pure function next to `PlatformKey`:

```go
// MatchesNPMPlatform reports whether a package gated by npm's os/cpu/libc
// lists is installable on k, with npm's semantics: an absent or empty list
// matches everything; "!x" entries exclude; any positive entry restricts.
func (k PlatformKey) MatchesNPMPlatform(os, cpu, libc []string) bool
```

It needs the npm vocabulary for the triple: `linux`, `x64`/`arm64`, and
`glibc`/`musl`. For an entry with no `libc` field on Linux, both libcs match,
which is conservative: we fetch.

### 2. Empty `SHA256` means "not fetched on this pool"

§8 changes from "Fetch fills `sha256` for every package before the lock is
written" to:

> `sha256` is filled for every artifact needed by a platform this pool has
> fetched for. An empty `sha256` means the artifact has not been fetched
> here. It is filled in when a fetch first needs it, and is never cleared or
> changed once set.

`update` therefore writes a lock in which the darwin, win32, and other-key
packages have `integrity` but no `sha256`.

### 3. `sync` persists what it backfills

Today `SyncHarnessLocked` fetches into an in-memory copy and never writes the
`sha256` values back, which is harmless only because update already filled
them all. With scoped fetch, a `sync --platform linux-arm64-glibc` downloads
the arm64 binary. The integrity index already keeps a later sync from
downloading it again, but each such sync re-hashes the ~100 MB object to
re-verify the index hit, and the lock never records an address `gc` can
retain the object by. So sync should still record what it filled in.

Add `persistCacheAddresses(paths, harnessName, lh)`, shaped like
`persistGeneration`: re-read the lock under the held pool lock, and for the
same harness *and the same version and recipe_revision*, copy each non-empty
`sha256` into an entry that is still empty. Never overwrite a set value; a
mismatch is a hard error, since it would mean two different objects passed
the same SRI check. Run it after `Fetch` and before `RealizeGeneration`, so
the generation's `persistGeneration` sees the same entry.

`LockHarness.Hash` changes when addresses are filled in. Nothing compares
it across time (see above), so this is acceptable. The receipt records the
hash of the entry as it was when that generation was built.

### 4. Realize stages only what the target needs

`source.Realize` currently calls `verifyCacheObject` on, and hardlinks,
every package in `NPMLock.Packages`. It must apply the same predicate as
Fetch for its single `key`: skip optional packages gated away from `key`,
and require (with today's error) a cache object for everything else.

**This depends on one npm behavior that must be confirmed before
implementation** (step 0 below): `npm ci --offline` against a lockfile
listing optional platform packages, with only the target platform's tarball
in cache, must skip the others without trying to read them from the cache.
npm's arborist drops optional dependencies that fail `checkPlatform` before
reify, which suggests it does, but the realize path seeds the cache index
by hand (`seed_cache_index.js`), so it needs a direct test rather than an
assumption.

### 5. `gc`

No change. Empty addresses were already skipped, and a backfilled address is
retained by the same lock-reference rule as any other.

## Delivery

0. **Prototype check (blocking).** Run a realize of claude in the oci
   backend with only the `linux-x64` and runtime objects staged, and record
   the outcome in this note. If `npm ci --offline` insists on the other
   tarballs, steps 1 and 2 both fail as written (darwin is just another
   absent tarball), and stub tarballs are no way out because they cannot
   pass the SRI check. Re-scope this note before building anything.
1. **Drop unrealizable packages (no lock semantics change in practice).**
   Fetch and Realize skip optional packages that match *no* `PlatformKey`
   at all (`targets` = all four keys). This alone removes darwin/win32:
   ~400 MB of claude's ~860 MB. Small, low-risk, and it exercises the
   predicate, the empty-sha256 path, and the realize skip.
2. **Target-scoped fetch.** Pass the real target set; add
   `persistCacheAddresses` to sync; scope runtime and archive artifacts.
   Claude's update drops to ~130 MB.

## Tests

- `MatchesNPMPlatform`: table over the four keys × absent/positive/negated
  os, cpu, and libc lists, including a Linux entry with no `libc`.
- `NPM.Fetch` with a lockfile fixture containing optional linux-x64,
  linux-x64-musl, darwin-arm64 entries and one ungated package: only the
  matching and ungated packages are requested (reuse the
  `TestFetch_OnlyRequestsListedURLs` recorder), and the skipped ones keep an
  empty `SHA256`.
- A non-optional package gated away from the target is still fetched.
- `Archive.Fetch` and runtime artifacts: one request for one target.
- `SyncHarnessLocked`: a second sync for the same key makes zero requests
  (the backfilled address was persisted), and the lock entry's other fields
  are byte-identical.
- `persistCacheAddresses`: refuses a version or recipe mismatch, refuses to
  overwrite a set address with a different one, never clears one.
- `Realize`: succeeds with only the target's objects in cache; fails with
  the existing "missing cache object" error when the target's own package
  is absent.
- Integration (oci backend): the step-0 check, kept as a regression test.

## Non-goals

- **Lazy fetching at activation time.** The pool stays offline after fetch:
  realize and activate never touch the network (§6.2).
- **Resumable downloads / a stall detector.** Related, since a 100 MB object
  on a slow link can hit `downloadTimeout`, but independent: `npm.go`'s
  `downloadTimeout` comment already names a stall detector as the right fix.
  Track separately.
- **Changing what `update` resolves.** The lock still records every package
  npm resolved, for every platform, so `sync --platform` on another key
  needs no re-resolve.

## Follow-up: fetch before a realize-only update

Independent of scoping. When the lock already agrees (`DecisionRealize`, or
`DecisionKeepExisting` with no generation), `UpdateHarnessLocked` realizes
from the lock without calling `Fetch`. A cache object lost or damaged since
the lock was written (an interrupted `gc`, disk trouble, or the
since-fixed realize staging bug that truncated a tarball shared by two lock
entries) then fails every `update`/`install` with "cache object … is
empty" or "not found", and only `sync` repairs it, because `sync` fetches
first.

Calling `Fetch` before those realizes would heal the cache automatically.
With an intact cache it makes no requests (every object is found by its
recorded sha256). It does contradict an invariant three orchestrator tests
pin today (`TestUpdateHarnessIdempotentWhenLockAlreadyAgrees`,
`TestUpdateHarnessKeepsExistingLockWhenNothingEligible`,
`TestUpdateVsInstallAdvanceSemantics`): an agreeing lock makes no source
calls at all. Decide whether that invariant means "no network" (Fetch on an
intact cache satisfies it) or "no Source calls" (it does not) before
changing it.
