# Design: documentation and onboarding improvements

Status: **proposed**

Scope: close the gap between what wraptool does at 0.5.0 and what its
documentation says, with a bias toward the first-run experience. Every finding
below was verified against the code or the rendered docs at
`79713f2` (tag `wraptool-v0.5.0`); each names the file and line so the fix is
mechanical rather than a re-investigation.

This is a documentation plan. It proposes no behavior change except where a
help string is itself the defect (§4.2, §6.3).

## 1. Why now

Three things landed close together and moved the docs out of date in different
directions:

- **Phase 3 (OCI provisioner, 0.5.0)** made `wraptool harness install` work on
  hosts with no Guix — the entire reason macOS and Windows users can now use
  the pool at all. No user-facing page mentions it.
- **Phase 5** added `docs/harness-pool.qmd`, which is good and covers
  quarantine plus scheduled refresh well. It is deliberately narrow: it does
  not cover the pool's command surface, its on-disk state, or provisioning.
- **A real macOS run** (Docker Desktop, 2026-08-05) surfaced two onboarding
  traps that only appear off-Guix, one of which was a genuine defect (fixed in
  `ac38efb`) and one of which is working-as-intended but reads as failure.

The docs' structure is sound — per-platform setup pages, a `wraptool up`-first
getting-started, a CLI reference validated against the Cobra tree. The problem
is coverage and a handful of specific false statements, not organization.

## 2. P0 — statements that are factually wrong

### 2.1 `up` does not require a pre-existing config

`docs/getting-started.qmd:29-32` says:

> `up` needs **one thing up front**: an existing config. It calls `config.Load`
> before it does anything else, so with no config file it fails immediately —
> create one first.

`cmd/up.go:102-107` calls `config.EnsureConfigFile` **before** `config.Load`,
with a comment stating the intent outright ("First run: write a
deny-by-default starter config if none exists"). `cmd/serve.go:120` does the
same. A first-time user is being told to hand-write YAML before the
one-command path works, when `wraptool up` in a git repo already works from
nothing.

This is the single highest-value fix in this plan: it turns the documented
happy path from *build → write config → start server → wire harness → up* into
*install → `wraptool up`*.

**Fix:** rewrite `getting-started.qmd`'s lead so the scaffold is the default
path, and demote the manual sections to what they already are — the
from-scratch explanation. Keep §2's policy example; it is what a reader edits
*after* first run, not a prerequisite.

### 2.2 `CHANGELOG.md` says 0.5.0 is unreleased

`CHANGELOG.md:7` reads `## 0.5.0 — unreleased`, but `wraptool-v0.5.0` is
tagged at `79713f2`. Same class of slip as the one already noted for 0.4.0
during the Mac work. **Fix:** stamp the release date, and add a release-checklist
line to `docs/releases.qmd` so the changelog heading is updated as part of
tagging rather than after it.

### 2.3 The CLI reference documents 2 of 8 `harness` subcommands

`docs/cli-reference.qmd:106-114` lists only `list` and `install`. The binary
has `gc`, `install`, `list`, `policy`, `refresh`, `rollback`, `sync`, and
`update`. Undocumented flags include `--backend`, `--platform`, `--constraint`,
`--allow-young`/`--reason`, `--to`, `--dry-run`, and `--legacy`.

The page opens by claiming it "is validated against the Cobra command tree in
`cmd/`", which makes the omission worse than a plain gap — it asserts a
completeness it does not have.

**Fix:** expand the section to the full surface, in the style already used for
`up`/`init`/`server` (a `text` block per command plus prose on the
non-obvious semantics). Note explicitly that `gc --legacy` is refused when any
pool-lockable harness lacks a realized active generation, since that error is
otherwise surprising.

## 3. P1 — 0.5.0's headline feature is undocumented

Grepping `docs/` for `podman`, `Docker Desktop`, `OCI`, `provisioning image`,
and `node:24` finds **no page** that explains harness provisioning without
Guix. Yet both non-Guix platform pages already instruct the reader to use it:
`docs/platform-macos.qmd:131` (`wraptool harness install agy`) and
`docs/platform-windows.qmd:82` (`wraptool harness install claude`).

Those two commands have materially different prerequisites, which nothing
states:

| Harness | Source | Needs a container backend? |
|---|---|---|
| `agy` | archive (`PoolArchive`, `internal/harness/harness.go:160`) | **No** — resolve/realize are pure Go |
| `claude`, `opencode`, `pi` | npm (`PoolPackage`, `harness.go:162,164,167`) | **Yes** — Guix, or Docker/Podman |
| `cursor`, `gemini`, `roo` | legacy | Not pool-lockable |

So the Windows page's `harness install claude` silently requires Docker Desktop
to be *running*, and the macOS page's `harness install agy` requires nothing at
all. A reader has no way to know which situation they are in.

**Fix:** add a short "Provisioning a harness off Guix" section to
`docs/harness-pool.qmd` (it is the natural home and already in the nav),
carrying the table above, the `--backend auto|guix|oci` override, and the
auto-selection rule from `defaultResolveBackend` — Guix when present *and* the
target arch matches the host, else OCI. Link it from both platform pages at the
`harness install` step.

### 3.1 Successful installs print warnings that read as failures

On a Docker-only host an npm harness install ends with a stderr warning that
the host-side legacy binary was not installed. That is the **expected shape of
success** (`cmd/harness.go`'s `warnInsteadOfFailForRealizedGeneration`), not a
problem — the pool generation was built and activated, and the container gets
the harness from the pool mount. Separately, the first `install`/`update` per
npm harness re-resolves its lock once because the provisioning-image digest is
folded into `recipe_revision`.

Both were confusing enough during the Mac run to need the source read to
interpret. **Fix:** an "Expected warnings" subsection listing each, what
triggers it, and why it is benign. This is cheap to write and directly reduces
support load.

## 4. P2 — the pool's state model has no user-facing documentation

### 4.1 On-disk state

`manifest.yaml` and `harness-lock.json` appear in **no** page under `docs/`
(verified by grep). A user is expected to run `harness policy set`, which edits
`manifest.yaml`, without ever being told the file exists or what the difference
is between recorded intent (manifest) and resolved fact (lock).

**Fix:** a "What the pool stores" section in `docs/harness-pool.qmd`: the four
things (`manifest.yaml` = intent, `harness-lock.json` = resolved versions,
`runtime/generations/` + `active/` pointers = realized state, `home/` = shared
harness HOME), one sentence each, with the per-OS root path. Deliberately keep
the receipt/digest mechanics in `design/harness/unified-harness-pool.md`.

### 4.2 `harness list` column semantics

The table prints HARNESS / CONSTRAINT / LOCKED / ACTIVE / LATEST / ELIGIBLE /
QUARANTINED / REALIZED. Only the quarantine countdown is explained anywhere
(`docs/harness-pool.qmd`). **Fix:** one definition list; it is the command a
user runs most and the widest single source of "what does this mean".

## 5. P3 — pre-existing gaps, unchanged by 0.5.0

- **`wraptool_discover` and `wraptool_info` are effectively undiscoverable.**
  `mcp-integration.qmd` has no section on the MCP tools wraptool exposes;
  `wraptool_discover` is mentioned only in passing in `hot-reload.qmd` and
  `wraptool_info` only in `releases.qmd`. `wraptool_discover` exists to break
  a bootstrap deadlock (a tool with no allow rules registers nothing, so the
  agent cannot name a subcommand to request) — worth surfacing where readers look
  for what the agent can see.
- **`flag_types` on `wraptool_request_capability` is undocumented** anywhere in
  `docs/`, despite overriding introspection
  (`design/policy/flag-type-inference.md`).
- **The `guix` CI runner has no documented contract.** `docs/contributing.qmd:7`
  names the label and nothing states what the host must provide. A runner
  mis-registration in July took every workflow down for ~12 commits and was not
  diagnosable from the repo. A short "CI runner requirements" section plus a
  fail-fast `command -v guix` guard step would have named the cause immediately.
- **`publish-pages.yml:30,34` is unpinned.** It calls bare `guix shell
  quarto-bin` / `guix shell bash coreutils ripgrep` with no `time-machine -C
  channels.scm`, so the docs build depends on the runner's ambient channel
  state — contradicting the single-source-of-truth rule in `CLAUDE.md`.
  Understandable given `design/policy/opt-in-documentation-tooling.md`
  deliberately keeps Quarto out of `manifest.scm`, but it should be pinned to a
  channel revision rather than left to host state.
- **`design/README.md`'s index is missing four notes**:
  `credential-webui-unix-hardening.md`, `free-text-sanitization-tier.md`,
  `git-execution-hardening.md`, `project-isolation.md`. The index is supposed
  to mirror every note's `Status:`.

## 6. Deliberately out of scope

- **`design/project-isolation.md` stays undocumented for users.** It proposes
  replacing shared mutable harness state with a read-only seed plus per-project
  copy-on-write, which would contradict what `getting-started.qmd:301-315`
  currently (correctly) tells readers about the shared, writable harness HOME.
  Documenting a proposal as behavior would be worse than the gap. Revisit when
  it ships.
- **Phase 5 item 4** (upstream signature/transparency adapters) is
  deliberately unimplemented and `docs/harness-pool.qmd:210` already says so.
- **`HostPlatformKey` naming.** On macOS the "host's own platform" resolves to
  `linux-arm64-glibc` — host *architecture*, container *OS*. The doc comment at
  `internal/pool/platform.go:44-49` is correct, but the identifier and the
  `--platform` help text ("default: this host's own platform") both read as
  false on a Mac. This misled a reader during the Mac run. It is a code/help
  change rather than a docs change, so it is filed here as a cross-reference
  only; the docs fix is to explain the Linux-target rule in §3's new section.

## 7. Suggested order

1. §2.1 (`up` scaffolds) — highest onboarding value, smallest diff.
2. §2.2 (changelog) and §2.3 (CLI reference) — correctness of existing claims.
3. §3 + §3.1 (off-Guix provisioning, expected warnings) — unblocks macOS and
   Windows users on 0.5.0's headline feature.
4. §4 (pool state model, `list` columns).
5. §5 (standing gaps), taking the CI-runner item early if another outage would
   be costly.

## 8. Verification

Documentation changes are verified by rendering, not by inspection:

```bash
guix time-machine -C channels.scm -- shell quarto-bin -- quarto render docs
```

Then confirm: every command block in the expanded `cli-reference.qmd` matches
`go run . <command> --help` for the same build; the new nav entries resolve;
and the harness-pool prerequisite table matches `internal/harness/harness.go`'s
registry (`PoolArchive` vs `PoolPackage` vs neither). The last one is worth a
test rather than a habit — `internal/harness/harness_test.go` already pins the
registry's source-kind invariants, so a doc table that drifts is detectable.
