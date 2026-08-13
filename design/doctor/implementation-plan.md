# Design: `wraptool doctor` implementation plan

Status: **implemented**
Scope: an itemized, incremental execution plan for
`design/doctor/wraptool-doctor.md`. Every step after the foundation ships a
`wraptool doctor` that runs and covers slightly more than the step before it.
No step leaves the command broken, and no step is large enough to need a long
review.

## 1. Working rules

1. **Every step is independently mergeable.** Own branch off `main`, own MR,
   green gates (`gofmt`, `go vet`, `golangci-lint run ./...`, `go test ./...`).
2. **Every step after A2 adds visible coverage.** The acceptance test is: run
   `wraptool doctor` before and after, and the report has checks it did not
   have before.
3. **Steps are sized in tens of minutes, not hours.** If a step grows past
   roughly 300 lines of non-test diff, split it.
4. **Checks self-register.** Each check file calls `register(...)` from its own
   `init()`, so two parallel steps never edit the same list. This is the seam
   that makes the parallelism real — a central `[]Check` literal in `doctor.go`
   would put every step in conflict with every other.
5. **One file per group.** The file map in §2 is the conflict-avoidance
   contract; a step touches its own file plus its own test file, and nothing
   else.
6. **A step that discovers a design error stops and reports it** rather than
   widening. The design note is wrong often enough (two adversarial passes
   found real breakage) that this will happen.
7. **Prefix every unexported identifier — in your group file AND its own
   `_test.go`, no exceptions — with your file's own group prefix**
   (`internal/doctor/env.go`'s `envBackends`, `envDescribeBackends`,
   `runEnvContainerBackend`, and `env_test.go`'s `envSplitBackendDetail` —
   never a bare `describeBackends` or `splitBackendDetail`). A package-level
   name collision between two concurrently authored files is invisible to git
   and merges cleanly; it breaks only on the merge commit, with a
   redeclaration error that names neither author — and the package's shared
   `_test.go` namespace is where a generic helper name (`check`, `virginInput`)
   is likeliest of all to collide, since it is the one file every one of the
   nine Phase-B authors is guaranteed to write. The prefix is your file's own
   name with `.go`/`_test.go` stripped and `_` separators removed —
   `env.go`/`env_test.go` -> `env`, `config_load.go` -> `configload`,
   `exposure_listen.go` -> `exposurelisten` — not the report `Group`. It is the
   whole file name rather than its first segment on purpose: `exposure_listen.go`
   and `exposure_files.go` share a `Group` AND a parallel wave, so a coarse
   `exposure` prefix would let both authors write `exposureInput()`, each branch
   green and the merge commit broken. Requiring `exposureListen*`/`exposureFiles*`
   rejects that on each author's own branch instead. If the mechanical prefix does
   not fit your file's natural, idiomatic naming (a plural file name over singular
   helpers, a compound name whose real group is its shorter head noun),
   `internal/doctor/invariants_test.go`'s `invariantsFilePrefixOverrides` table
   already names the prefix to use for every file in §2's map — it exists before
   your branch does, so you never edit that test file yourself to add an entry.
   `TestGroupFileIdentifiersArePrefixedWithFileGroup` enforces this over every
   `.go` file in the package INCLUDING `_test.go` ones (identifier contains one of
   its file's allowed prefixes, case-insensitively, so `run<Group>...` check
   functions pass too), except the seam/foundation/data-only files named in
   `invariantsFoundationFiles` (`finding.go`, `doctor.go`, `render_human.go`,
   `render_json.go`, `host.go`, `host_stat_unix.go`, `host_stat_other.go`,
   `mapenv.go`, `toolknowledge.go` — never a check group, so never scanned,
   and their own `_test.go` files are exempt with them).
8. **A check's `RunFunc` is a named, package-level function** (`runFoo`, per
   rule 7), tested by calling it directly with a hand-built `*Input` — never a
   closure defined inside `init()`, and no test asserts over the global
   `Registered()`/registry for a specific check's OWN behaviour (the registry
   is for registry-WIDE invariants: `internal/doctor/invariants_test.go`).
   This is what stops one author's tests from breaking when another author's
   file merges: a test written against `Registered()` sees every OTHER
   already-merged check too.
9. **Never set `Finding.Location` for a config finding — set `Finding.Path`.**
   `Location` is engine-owned for config findings (RULING R5): the engine
   fills it from `Path` via the `Locator`, and C1 is a zero-line edit per group
   file only because no group file writes `Location` for a config subject
   itself. Hand-rolling `Location` as `in.ConfigPath` (or a guessed
   `path:line`) looks like it works before C1 lands (`nullLocator` never
   overwrites a check-set `Location`), but silently defeats C1's retrofit for
   that finding once it does — the exact regression
   `TestFindingsAreWellFormed` in `internal/doctor/invariants_test.go` rejects
   (a `Location` with `in.ConfigPath` as a prefix, `Path` empty).
   `Finding.Location`'s own doc comment (`internal/doctor/finding.go`) states
   this rule too.
10. **A step that believes it needs a new `Requirement` bit or `Env` method
    stops and reports** rather than widening `internal/doctor/finding.go`'s
    `Requirement` vocabulary or `internal/doctor/host.go`'s `Env` interface.
    Both are CLOSED in A1 on purpose (see their own doc comments): either one
    is a file shared by all nine Phase-B branches, so an author who adds a bit
    or a method to unblock their own check puts every other branch in
    conflict with theirs. This is rule 6 applied to the two shared vocabulary
    types specifically. A check is not limited to `Env`, though: `host.go`'s
    `Env` doc comment names the sanctioned sibling-package escape hatches
    (`config.Load`/`LoadStrict`/`Validate`, `introspect.CacheStamp`,
    `requestlog`'s readers) that touch the real filesystem themselves — a
    check using one of these is tested against a `t.TempDir()` fixture tree,
    never a `MapEnv` (design §12).

## 2. File map

```
internal/doctor/
  finding.go          A1  Finding, Outcome, Severity, Check interface, register()
  doctor.go           A1  engine: run order, selection, summary
  render_human.go     A1  human report
  render_json.go      A2  --json
  host.go             A1  Env interface (CLOSED, rule 10), osEnv, FileInfo
  mapenv.go           A1  MapEnv: the fake every Phase-B/C/D unit test builds
  invariants_test.go  A1  registry-wide invariants every check is covered by
                          automatically (naming, determinism, well-formed
                          findings, no direct host access) — read, never
                          edited, by B1..B9/C1/C2/D0..D4
  env.go              A1 (env.container-backend), extended by B1 with env.*
  config_load.go      B2  config.parse, config.validate
  config_perms.go     B3  config.file-perms, dir-perms, symlinked, file-readable-secrets
  exposure_listen.go  B4  listener/bind checks
  exposure_files.go   B5  token, socket, request-log checks
  tools.go            B6  tools.* for CLI tools
  mcpservers.go       B7  tools.mcp-server-*
  pool.go             B8  pool.*
  runtime.go          B9  runtime.*, extended in E1/E2
  locate.go           C1  yaml node tree → file:line
  suppress.go         C2  # doctor:ignore
  toolknowledge.go    D0  per-tool data table
  policy_generic.go   D1  tool-independent lints
  policy_flags.go     D2  flag lints + exemptions
  policy_subcmd.go    D3  per-tool subcommand lints
  policy_mcp.go       D4  policy.mcp-dangerous-tool
  drift.go            F1+ drift.*
cmd/doctor.go         A1  CLI wiring (A2 adds flags)
```

## 3. Steps

Dependency column names the step that must land first. "Parallel" names steps
that may run at the same time because their file sets are disjoint.

### Phase A — foundation (serial, 2 steps)

| Step | Lands | Files | Depends | Demo |
|---|---|---|---|---|
| **A1** | Engine + CLI + human renderer + exit codes 0/1/2. Types: `Severity`, `Outcome`, `Finding`, `Check`, `register()`. The `Env`/`FileInfo` host seam (real `osEnv` + the `MapEnv` fake every later step's unit tests build) and the registry-wide invariant tests every check is covered by automatically. One check: `env.container-backend` (info). Config is loaded leniently and passed to checks; a missing config is `info`, not an error. | `finding.go`, `doctor.go`, `render_human.go`, `host.go`, `host_stat_unix.go`, `host_stat_other.go`, `mapenv.go`, `env.go`, `invariants_test.go`, `cmd/doctor.go`, `cmd/exit.go`, `main.go` (exit-code plumbing) | — | `wraptool doctor` prints one info line and exits 0 |
| **A2** | `--json`, `--only`, `--skip`, `--strict`, `--no-probe`, `--workspace` (via `os.Stat` walk, never `git rev-parse`). Summary arithmetic incl. `skipped`. | `render_json.go`, `doctor.go`, `cmd/doctor.go` | A1 | `wraptool doctor --json` emits the schema |

A1 is the only step whose review matters structurally — it fixes the seam every
later step plugs into. Review it properly; the rest are additive.

### Phase B — static checks (9 steps, all parallel after A2)

Every step here is: one new file, one test file, `init()` registration, a row
in the docs table. None touches another's file. **§1 rules 7–10 are the
concurrency contract for this phase specifically** — read them before writing
a check: rule 7 (naming) and rule 9 (`Path` not `Location`) are the two whose
violation either merges cleanly and breaks a later merge commit, or silently
defeats a later step (C1) with no error at all.

| Step | Lands | Depends | Parallel with |
|---|---|---|---|
| **B1** | `env.devcontainer-cli`, `env.no-backend-with-project`, `env.guix-runtime-selected`, `env.xdg-writable` | A2 | B2–B9 |
| **B2** | `config.parse` (LoadStrict), `config.validate`, and the exit-2 abort path with `env.*` still reported | A2 | B1, B4–B9 |
| **B3** | `config.file-perms`, `config.dir-perms`, `config.symlinked`, `config.file-readable-secrets` | B2 | B1, B4–B9 |
| **B4** | `exposure.transport-unsupported`, `sse-non-loopback`, `admin-non-loopback`, `admin-container-reachable`, `listeners-collide` | B2 | all other B |
| **B5** | `exposure.token-perms`, `token-weak`, `unix-socket-dir`, `cwd-roots-unset`, `request-log-writable`, `request-log-perms` | B2 | all other B |
| **B6** | `tools.binary-executable`, `binary-writable`, `working-dir`, `env-file-missing`, `env-file-perms`, `tools.inert` | B2 | all other B |
| **B7** | `tools.mcp-server-command`, `mcp-server-args-unpinned`, `mcp-server-inert`, `tools.overrides-missing`, `overrides-invalid` | B2 | all other B |
| **B8** | `pool.home-perms`, `generation-digest`, `devcontainer-mount-missing`, `devcontainer-mount-writable`, `deprecated-feature`, `--verify-pool` for `generation-corrupt` | A2 | all other B |
| **B9** | `runtime.stale-pidfile`, `runtime.bind-divergence` — both work against **today's** `serve.state`, which already records `listen`+`auth`. Gated on `processAlive`. | B2 | all other B |

B9 is deliberately here, not in Phase E: the design's most valuable single
check needs no prerequisite at all.

**Windows handling** needs no per-step helper and no `doctor.go` edit: a
permission-bit check declares `Needs: NeedsModeBits` in its `Meta`, and the
engine skips it with the canned reason ("file permission bits are not
meaningful on this platform", `finding.go`'s `requirementReasons`) whenever
`Input.GOOS == "windows"` (`provides()`, `doctor.go`). This bit already exists
in A1 for exactly this reason — B3/B5/B6 each just set it, with no
coordination between them needed at all.

### Phase C — locations and suppression (2 steps, serial)

| Step | Lands | Files | Depends | Demo |
|---|---|---|---|---|
| **C1** | `yaml.Node` traversal → `file:line` on every config finding, keyed by **value not index** (`SeedHelpFlags` both appends and drops). Retrofits B2–B7 findings. | `locate.go` + one-line edits per group file | B3, B6, B7 | findings gain `config.yaml:37` |
| **C2** | `# doctor:ignore <id>` in both supported comment placements, `suppressed`/`suppression_reason` in JSON, suppressed counted not printed | `suppress.go`, `render_*.go` | C1 | a suppressed warn disappears from the report but stays in `--json` |

C1 touches every group file with a one-line change each, so it is the one step
that must not run concurrently with Phase B. Land it after B, before D.

### Phase D — policy lint (5 steps, mostly parallel)

| Step | Lands | Depends | Parallel with |
|---|---|---|---|
| **D0** | `toolknowledge.go`: table shape + git/kubectl/gcloud/terraform data, no checks yet. Pure data + a lookup test. | C2 | — |
| **D1** | `policy.uncompilable-pattern` (fail), `policy.overbroad-pattern`, `config.constraint-orphan`. Tool-independent, no table needed. | C2 | D0, D2 |
| **D2** | `policy.unpatterned-value-flag`, `policy.dangerous-flag`, plus the mandatory `deny_flags` / `deny: true` exemptions | D0 | D1, D3, D4 |
| **D3** | `policy.shell-equivalent-subcommand`, `mutating-subcommand`, `force-push-allowed` | D0 | D2, D4 |
| **D4** | `policy.unbounded-positional-count`, `deny-prefix-gap`, `policy.mcp-dangerous-tool` | D0 | D2, D3 |

D1 can start immediately after C2 — it needs no table.

### Phase E — runtime, after the serve.state prerequisite

| Step | Lands | Depends |
|---|---|---|
| **E0** | The serve.state extension itself, scoped in `wraptool-doctor.md` §4.1. Ships alone, in `cmd/`, with no doctor changes. Reviewable on its own merits; `server status` keeps working. | — (can run in parallel with **all** of Phase B) |
| **E1** | `runtime.uid`, `exposure.token-owner`, `env.path-differs-from-server` | E0, B5, B9 |
| **E2** | `runtime.stale-policy` (needs `config_loaded_at`, i.e. E0's reload hook) | E0, B9 |

E0 is the single best candidate to run concurrently with Phase B: different
package, no shared files, and it unblocks E1/E2 by the time B finishes.

### Phase F — drift, after the introspect prerequisites

Each F0 step is its own MR in `internal/introspect`, independently useful and
independently reviewable.

| Step | Lands | Depends |
|---|---|---|
| **F0a** | Exported validity type/constructor so the already-exported `LoadCache` is callable from outside the package | — |
| **F0b** | Stale-tolerant read exposing `VersionOutput` (a tool upgrade currently discards the cached version string the check needs) | F0a |
| **F0c** | Probe entry point taking an injected environment, so doctor can pass `ChildEnv(binary, nil)` | — |
| **F0d** | Persist per-subcommand parse **coverage** in `Schema` (cache format change; bump the cache's own version) | — |
| **F0e** | Positive-parse signal distinguishing "no flags" from "help failed" from "garbage parsed as help" (`git bogus -h` returns 53 bytes of error text that clears the 20-byte threshold) | F0d |
| **F0f** | Per-tool catalog argv probe (`git help -a`, `kubectl --help`, `gcloud help`) + capture `-h` fixtures into `testdata/` for every tool in the D0 table | F0c |
| **F1** | `drift.unverifiable` + `drift.tool-version-changed`. **Never asserts absence** — ships the honest half first, so the group is useful and harmless before any absence claim exists. | F0b, F0c, D0 |
| **F2** | `drift.unknown-subcommand` for **allow** rules: first-segment matching, catalog-gated, everything else `unverifiable` | F0f, F1 |
| **F3** | `drift.unknown-subcommand` for **deny** rules at `fail` severity (a typo'd deny is fail-open) | F2 |
| **F4** | `drift.unknown-flag`, gated on the coverage signal, with the rule that an unconfirmed flag is exempted from `policy.unpatterned-value-flag` so one miss cannot compound into two findings | F0e, F2, D2 |

F1 before F2 is the important ordering: it proves the probe path, the cache
path, and the report plumbing while the group still cannot produce a false
"this does not exist".

**OUTCOME: F0a–F0f and F1 shipped. F2 shipped inert; F3 and F4 were not
built.** `drift.unknown-subcommand` exists with a permanently empty
eligibility allowlist and never asserts absence — see
`design/doctor/wraptool-doctor.md` §7.6, "Phase F closes as
informational-only", for the five measured environment axes that rule the
claim out, and for the two approaches built and abandoned on the way there.

That ordering decision is the one part of this phase worth reusing. Shipping
the half that cannot assert absence first meant every later round tested a
real probe path against a real report, and the four rounds that followed each
found a false-absence case in a working configuration. Had F2 gone first,
those would have been support reports rather than review findings.

### Phase G — documentation

| Step | Lands | Depends |
|---|---|---|
| **G1** | `docs/doctor.qmd` covering the command, the groups shipped so far, exit codes, and suppression; `_quarto.yml` nav entry; `docs/cli-reference.qmd` row | C2 |
| **G2** | Update `design/policy/security-audit-policy-enforcement.md` §"Policy-content self-audit checklist" to point at `wraptool doctor` instead of prose steps, and mark the checklist items doctor now automates | D4 |
| **G3** | `docs/troubleshooting.qmd` entries for the findings most likely to be hit first (`config.symlinked`, `runtime.bind-divergence`, `drift.unverifiable`) | F2 |

## 4. Suggested execution order

Three tracks that can run at once once A2 is in:

```
track 1 (doctor checks):  A1 → A2 → B1..B9 → C1 → C2 → D0/D1 → D2/D3/D4 → G1
track 2 (cmd package):    E0 ......................→ E1 → E2
track 3 (introspect):     F0a/F0c/F0d → F0b/F0e/F0f ....... → F1 → F2 → F3 → F4
```

Track 2 and track 3 touch packages doctor does not, so they can start on day
one. Within Phase B, the nine steps are mutually independent — that is the
widest parallel point in the plan.

## 5. Risks this ordering manages

- **Seam risk** is front-loaded into A1 and paid once. Everything after it is
  additive registration.
- **Merge risk** is bounded by the one-file-per-group rule plus `init()`
  self-registration. The only cross-cutting step is C1, which is scheduled
  alone.
- **False-positive risk** is deferred: the heuristic groups (D, F) land after
  the deterministic ones, and within F the absence-asserting checks land last.
- **Prerequisite risk** is isolated: E0 and every F0 step is a normal,
  reviewable change to its own package, valuable even if doctor slipped.
- **Design-error risk** stays real. Two adversarial passes each found checks
  that could not be built as specified; a third class will surface during
  implementation. Rule 6 of §1 exists for that.
