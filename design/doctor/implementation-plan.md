# Design: `wraptool doctor` implementation plan

Status: **proposed**
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

## 2. File map

```
internal/doctor/
  finding.go          A1  Finding, Outcome, Severity, Check interface, register()
  doctor.go           A1  engine: run order, selection, summary
  render_human.go     A1  human report
  render_json.go      A2  --json
  env.go              B1  env.*
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
| **A1** | Engine + CLI + human renderer + exit codes 0/1/2. Types: `Severity`, `Outcome`, `Finding`, `Check`, `register()`. One check: `env.container-backend` (info). Config is loaded leniently and passed to checks; a missing config is `info`, not an error. | `finding.go`, `doctor.go`, `render_human.go`, `cmd/doctor.go` | — | `wraptool doctor` prints one info line and exits 0 |
| **A2** | `--json`, `--only`, `--skip`, `--strict`, `--no-probe`, `--workspace` (via `os.Stat` walk, never `git rev-parse`). Summary arithmetic incl. `skipped`. | `render_json.go`, `doctor.go`, `cmd/doctor.go` | A1 | `wraptool doctor --json` emits the schema |

A1 is the only step whose review matters structurally — it fixes the seam every
later step plugs into. Review it properly; the rest are additive.

### Phase B — static checks (8 steps, all parallel after A2)

Every step here is: one new file, one test file, `init()` registration, a row
in the docs table. None touches another's file.

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

**Windows handling** lands with whichever of B3/B5/B6 goes first: a shared
`skipIfNoModeBits()` helper in that step's file, moved to `doctor.go` by the
second one that needs it. Small, deliberate duplication beats a blocking
dependency.

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
