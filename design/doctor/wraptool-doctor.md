# Design: `wraptool doctor`

Status: **proposed**
Scope: a single operator-facing preflight command that answers "is this
installation sane, is my policy as tight as I think it is, and does it still
match the tools actually installed?" It checks host dependencies, strict config
validity, deployment posture, config-vs-running divergence, dangerous policy
content, and drift between the policy and the installed tool versions. It never
edits config, never starts or contacts the server, and never mutates the
harness pool.

This automates the manual checklist in
`design/policy/security-audit-policy-enforcement.md` §"Policy-content
self-audit checklist", which today exists only as prose telling the operator to
grep their own config by hand.

## 1. Problem

wraptool's failure modes are spread across layers that no single command
covers:

| Layer | Today's coverage | What goes unnoticed |
|---|---|---|
| Host dependencies | none — discovered at use time | `guix`/`docker`/`podman`/`devcontainer` absent; `wraptool up` fails deep inside a subcommand |
| Config syntax & semantics | `wraptool config validate` | nothing (this layer is solved) |
| Deployment posture | partly in `Config.Validate` | world-readable token file, world-writable `config.yaml`, group-readable config holding inline `env:` secrets, `allowed_cwd_roots` unset (audit F5) |
| Config vs running server | **nothing** | `up` rewrites the bind to `0.0.0.0` in memory only; a failed reload leaves the server on an older, weaker policy |
| Policy content | **nothing** — prose checklist only | value flags with no `pattern`, `.*` patterns that are patterns in name only, `kubectl exec`-class subcommands, mutating verbs with no deny |
| Policy vs installed tools | **nothing** | subcommands and flags that the installed tool no longer has (or never had — a typo is silently inert), orphaned `flag_constraints` |

The last three rows are the expensive ones.

A policy that *looks* restrictive but allows `git -c core.sshCommand=… pull` is
worse than no wraptool at all, because it carries the credibility of a
whitelist. `Config.Validate` cannot catch this class: every such config is
structurally valid.

Drift is the quiet counterpart. A rule naming a subcommand the tool dropped, or
a flag misspelled at authoring time, is not an error anywhere — it registers,
it validates, and it simply never matches. The operator believes a capability
is granted; it is dead. Nothing in the system reports this today, and the data
needed to detect it (`introspect.Available`, per-subcommand parsed flags) is
already computed and cached at every server start.

Divergence is the sharpest of the three: `adjustServerBindForRuntime`
(`cmd/up.go:215-231`) rewrites a loopback bind to `0.0.0.0` and defaults in a
token for the devcontainer runtime, passes it to `serve` as overrides
(`cmd/up.go:326-329`), and never writes it back to disk. A file-only audit
reports "loopback, ok" while the server is listening off-host.

## 2. Goals and non-goals

**Goals**

- One command that an operator runs after install, after editing config, and in
  CI.
- Report *every* problem in one pass, not the first one. `Config.Validate`
  short-circuits on its first error by design; doctor must not.
- Findings are actionable: each names the location **in the file**, what is
  wrong, and the specific change to make.
- Deterministic: identical inputs give identical output, so reports diff
  cleanly between runs.
- Machine-readable (`--json`) with stable check IDs.

**Non-goals**

- Not a fixer. doctor never writes config; it prints the change to make. An
  `--fix` mode is deferred (§12).
- Not AI-facing. doctor is an operator CLI command only, never an MCP tool. Its
  output names host paths and describes precisely where the policy is weakest,
  which is the reconnaissance an untrusted harness must not get.
  `wraptool_discover` remains the AI-facing, policy-respecting counterpart.
- Not a liveness check. doctor reads the server's *recorded* state from disk; it
  never opens a connection, performs an MCP handshake, or requires the server
  to be running.
- Not a replacement for `wraptool config validate`, which stays as the narrow,
  scriptable primitive the audit doc's preflight instruction points at.

## 3. Command surface

```
wraptool doctor [--config PATH] [--root DIR] [--json] [--strict]
                [--skip CHECK,...] [--only CHECK,...] [--no-probe]
```

- `--config PATH` — same resolution order as `config validate`: flag, then
  `$WRAPTOOL_CONFIG`, then `paths.DefaultConfigFile()`.
- `--root DIR` — project root for the runtime-selection checks (`manifest.scm`
  detection, `.devcontainer/` inspection). Defaults to the same repo detection
  `up` performs; when there is no project root, those checks report `info: not
  in a project`.
- `--json` — emit the machine-readable report (§8) on stdout, nothing else.
- `--strict` — promote every `warn` to `fail` for exit-code purposes. Interactive
  and one-off use; see §8.1 for why it is a poor CI gate.
- `--skip` / `--only` — check-ID selection, accepting a group prefix
  (`--only policy`, `--skip drift`).
- `--no-probe` — skip every check that executes a tool binary (§4). Makes the
  run fully static at the cost of the `drift.*` group.

### 3.1 Exit codes

| Code | Meaning |
|---|---|
| 0 | No `fail` findings (`warn` findings may be present, unless `--strict`) |
| 1 | At least one `fail` finding |
| 2 | doctor could not complete — config unreadable or unparseable, or an internal error |

Exit 2 is distinct on purpose: "your setup is broken" and "I could not tell"
must not be the same signal in a pipeline. A config that fails strict decode
produces the `config.parse` finding *and* exits 2, because every downstream
check depends on a loaded config; the report still contains the full `env.*`
group, which needs no config at all.

A **missing** config is not an error. Post-install, before the first
`serve`/`up`, no config exists yet — `config.EnsureConfigFile` scaffolds it on
first run. doctor reports `info: no config yet` and runs the `env.*` group, so
the very first thing a new user can run is `wraptool doctor`.

## 4. Probe policy

doctor performs **inert local execution** of configured tool binaries, and
nothing else:

- **Allowed**: `os.Stat`/`Lstat`, reading config, override, pool, and
  server-state files, `exec.LookPath`, and executing a configured tool binary
  with introspection's help/version argv (`-h`, `--help`, `help <subcommand>`,
  `--version`) — including the config-authored subcommand tokens, since that is
  the only way to learn which flags a subcommand actually has.
- **Forbidden**: any network call, any container-engine invocation (`docker
  run`, `guix shell`), any connection to the running server, any argv token
  that did not come from either introspection's fixed help vocabulary or an
  operator-authored config subcommand, and any mutation of config, pool, or
  request log.

Probing is what makes the `drift.*` group possible, and drift is the check
class with the most direct operator payoff — it is the one that tells you which
of your rules are dead. Three constraints keep it honest:

1. **Scrubbed child environment.** `introspect.runTool` sets no `cmd.Env`
   (`internal/introspect/introspect.go:203`), so it inherits the caller's full
   environment. Inside `serve` that is the service environment; inside doctor it
   would be the operator's interactive shell, typically carrying `GH_TOKEN`,
   `SSH_AUTH_SOCK`, and cloud credentials. doctor's probe uses
   `executor.ChildEnv` instead — the same minimal environment the AI path
   builds. This is both the safer choice and the more *accurate* one: doctor
   then observes the tool exactly as wraptool will actually invoke it.
2. **Ordering.** `tools.binary-executable` and `tools.binary-writable` are hard
   preconditions, evaluated before any probe of that binary. doctor is the
   command an operator runs to *learn* their tool binary is attacker-writable;
   it must not execute it first and report afterwards.
3. **Respect explicit help revocation.** A rule whose `deny_flags` removes
   `-h`/`--help` (`SeedHelpFlags`, `internal/config/config.go:234-255`) is not
   probed with them — that is the operator saying "do not run help here", and is
   audit M2's shape.

`--version` is part of the probe vocabulary because `Introspect` already runs it
(`introspect.go:108,186`) and its output is the cache's version-drift signal.

### 4.1 Cache interaction

Probing through `introspect` writes the cache (`SaveCache`,
`introspect.go:150`). That is not a security concern — the argv is
operator-authored, the binary is operator-named, and `serve` already probes the
same binaries the same way at every start. It is an *ownership* concern: if
`serve` runs as a dedicated service user, a cache file written by the operator
can block the server's later write.

Therefore: doctor writes the cache when its uid matches the server's recorded
uid (or when no server state exists, i.e. same-user deployment), and otherwise
probes with the cache in read-only mode and emits `info: cache not written
(server runs as uid N)`. The uid comes from the `runtime.*` group (§6.5).

### 4.2 Required upstream API

The design depends on introspection surface that is currently unexported. This
is part of the work, not an assumption:

- A read-only introspection entry point that returns a `*Schema` without
  writing the cache, and reports whether the cached entry is still valid.
  Today `LoadCache`, `configHash`, and `newestInputMtime` are unexported and
  only `CacheStamp` is exported, which reports `introspectedAt` + `configHash`
  with no validity verdict (`internal/introspect/cache.go:59-69`).
- A probe entry point accepting an injected environment, so `executor.ChildEnv`
  can be supplied.
- A way to distinguish *parse failure* from *genuinely no flags*. Today a help
  failure stores an empty `Subcommand` (`introspect.go:129-135`), which is
  indistinguishable from a successful parse of a flagless subcommand. Without
  this distinction the `drift.*` group cannot be implemented safely — see §6.6.

## 5. Check model

Every check produces zero or more findings:

```
ID        string   // stable, dotted: "policy.unpatterned-value-flag"
Severity  fail | warn | info
Title     string   // one line
Detail    string   // what was found, with the concrete value
Location  string   // file:line for config findings (§5.1), path, or ""
Remedy    string   // the specific change to make
DocRef    string   // e.g. "docs/configuration.qmd#flag-constraints"
```

Severity semantics, applied consistently:

- **fail** — wraptool is broken, or the credential boundary is provably not
  what the config claims. No judgement call involved.
- **warn** — a heuristic, a default-open setting, or a shape that is dangerous
  in most deployments but legitimately intended in some.
- **info** — state worth knowing, never a problem.

A check that cannot run (dependency missing, file unreadable, help parse failed)
emits its own finding rather than being silently skipped. Silence must mean
"checked, clean", never "did not look".

### 5.1 Locations must be file positions

Findings for config content are located by **file and line**, resolved through
`internal/yamledit`'s node tree — not by index into the in-memory `Config`.

This is a correctness requirement, not a presentation preference.
`applyDefaults` seeds `-h`/`--help` into every rule's `Flags`
(`internal/config/config.go:296-298`), so `tools.git.allow[1].flags[3]` in the
loaded struct corresponds to no node in the file at all, and a lint that
reported it would send operators hunting for text that is not there. Linting
against the node tree also makes the seeded help flags naturally invisible to
the policy checks, since they do not appear in the file.

`yamledit` already provides the node-tree primitives (`MapGet`, `EnsureMapChild`)
that `config.editConfig` and `cmd/harness.go` share.

### 5.2 Suppression

Per-site suppression ships with the lint groups; it is not an open question.
Without it, the first operator annoyed by a heuristic disables the entire group
and never sees a real finding again.

A YAML comment marks an accepted finding:

```yaml
tools:
  git:
    allow:
      - subcommand: [pull]
        # doctor:ignore policy.dangerous-flag -- reviewed 2026-08, pattern lives in the wrapper
        flags: ["-c"]
```

The comment is read from the node tree (the same mechanism that preserves
comments through `config.editConfig`) and applies to findings at or below that
node. A suppressed finding is not printed, but is counted, and `--json` includes
it with `"suppressed": true` plus the comment's rationale text — so an auditor
can see what was waived and why. Suppression applies to `warn` findings only;
a `fail` is never suppressible.

## 6. Check catalog

Grouped by prefix. The ID surface is additive after the first release.

### 6.1 `env.*` — host dependencies

| ID | Sev | Check |
|---|---|---|
| `env.container-backend` | info | Which of `guix`, `docker`, `podman` are on PATH. `info`, not `warn`: a host-only deployment serving MCP to a separately managed container is fully supported (`docs/isolated-environments.md`) and must not be nagged. |
| `env.devcontainer-cli` | info | `devcontainer` present; required only for `--runtime=devcontainer`. |
| `env.guix-manifest` | info | With `--root`: `guix` on PATH **and** `manifest.scm` present = the guix runtime auto-selects. Mirrors `hasGuix` (`cmd/guixcontainer.go:56`). |
| `env.no-backend-with-project` | warn | With `--root` naming a project that has `.devcontainer/` or `manifest.scm`, but no usable backend on PATH. Here the operator has demonstrably opted into `up`, so absence is a real problem. |
| `env.path-differs-from-server` | info | doctor's `PATH` differs from the server's recorded `PATH`. Tool runtime deps resolve against `executor.childPath`, which reads the **serve** process's environment (`internal/executor/executor.go:28-38`) — doctor cannot check the server's PATH from its own shell, so it reports the divergence rather than a false verdict. |
| `env.xdg-writable` | fail | `paths.DataDir()`, the config dir, and the runtime dir are writable — testing the nearest **existing** ancestor, since these are created lazily by first run (`internal/config/scaffold.go:58`) and `RuntimeDir()` falls back to `CacheDir()/run` when `XDG_RUNTIME_DIR` is unset (`internal/paths/paths.go:48-53`). |

### 6.2 `config.*` — integrity

| ID | Sev | Check |
|---|---|---|
| `config.parse` | fail | `config.LoadStrict` succeeds — unknown/misspelled keys reported (audit F1's class). Aborts with exit 2. |
| `config.validate` | fail | `Config.Validate()` passes; one finding per error surfaced. |
| `config.file-perms` | fail | `config.yaml` is not writable by group or other. A writable config is a policy bypass: whoever can write it grants themselves any tool. |
| `config.file-readable-secrets` | fail | Config is group/other-**readable** *and* contains inline `env:` entries under `tools.*` or `mcp_servers.*` (`internal/config/config.go:50,67`). Reports the key names, never the values. |
| `config.dir-perms` | fail | The config directory is not group/other-writable. Same severity as `config.file-perms` by the same argument: rename-in-place substitution defeats a locked-down file. |

`config.parse` and `config.validate` call the same code path as
`wraptool config validate` rather than reimplementing it.

### 6.3 `exposure.*` — deployment posture and secrets

| ID | Sev | Check |
|---|---|---|
| `exposure.transport-not-supported` | warn | `mcp.transport` is not `sse` or `unix`. Keyed on the supported set, not on naming the test-only transport; the passing form prints nothing. |
| `exposure.sse-non-loopback` | warn | **`sse` only** — `mcp.listen` is non-loopback. `Config.Validate` already fails the unauthenticated case (`internal/config/validate.go:43-46`); this warns on the authenticated one. Gated on transport because `ListenIsLoopback` returns false for a `unix` socket path (`internal/config/listen.go:11-22`), which would otherwise fire on every unix deployment. |
| `exposure.admin-non-loopback` | fail | `admin.listen` is non-loopback. The review UI is the approval authority. |
| `exposure.admin-container-reachable` | warn | `admin.listen` is loopback **and** the guix runtime applies for `--root`. `guix shell --container --network` shares the host network namespace (`cmd/guixcontainer.go:96`), so an in-container harness reaches `127.0.0.1:8718`. Loopback is not a boundary against the thing wraptool is isolating. |
| `exposure.token-perms` | fail | Every `auth_token_file` has no group/other bits. |
| `exposure.token-owner` | warn | Token file owner differs from the server's recorded uid (or from doctor's, when no server state exists). `warn`, not `fail`: a service-user deployment legitimately has files doctor does not own (§6.5). |
| `exposure.token-weak` | warn | Token is empty, shorter than 32 bytes, or a known placeholder. Reports length and shape only — never the value. |
| `exposure.listeners-collide` | fail | `mcp.listen` equals `admin.listen`. `Config.Validate` compares only the two token files (`validate.go:66-68`); identical addresses pass validation and fail at bind. |
| `exposure.unix-socket-dir` | fail | For `unix` transport, the nearest existing ancestor of the socket path is not group/other-writable. Existence is not required — `serve` creates it at bind. |
| `exposure.cwd-roots-unset` | warn | `mcp.allowed_cwd_roots` is empty — the documented default-open case (audit F5). |
| `exposure.request-log-writable` | fail | The nearest existing ancestor of `mcp.request_log` is writable. The directory itself is created lazily (`internal/requestlog/store.go:47`), so requiring it would fail every fresh install. |
| `exposure.request-log-perms` | fail | An existing request log has no group/other bits. `Store.append` creates it `0600` (`store.go:50`) but never repairs a pre-existing permissive file, and it records every tool call. |

### 6.4 `tools.*` — wiring

| ID | Sev | Check |
|---|---|---|
| `tools.binary-executable` | fail | Each `binary` is executable. `validateTool` checks absolute/exists/regular (`validate.go:158-203`), not the mode bits. |
| `tools.binary-writable` | warn | The resolved binary, or a path segment leading to it, is group/other-writable — arbitrary code inside the credential boundary. `warn` rather than `fail` because Intel-macOS Homebrew makes `/usr/local` group-writable by design and the repo now ships a tap; the detail names the offending segment and its group. |
| `tools.working-dir` | fail | `working_dir`, when set, is an existing absolute directory. Validated nowhere today; a bad value fails every call at exec time. |
| `tools.env-file-missing` | fail | Every `env_files` entry (on `tools.*` **and** `mcp_servers.*`) exists and is readable. |
| `tools.env-file-perms` | fail | Every `env_files` entry has no group/other read bits — these hold credentials by definition. |
| `tools.mcp-server-command` | fail | Every `mcp_servers.*.command` is absolute and exists. `validateMCPServers` checks non-emptiness only (`validate.go:122-124`), so `command: npx` resolves from the server's PATH at spawn — a hijack vector with no equivalent on the `tools.*` side, where `binary` must be absolute. |
| `tools.inert` | info | A tool with zero `allow` rules registers no MCP tools. `info`: this is the deliberate bootstrap state `wraptool_discover` exists to serve, and `validateTool` permits it explicitly. |
| `tools.mcp-server-inert` | warn | An `mcp_servers` entry with zero allow rules still spawns the upstream child (audit M1). |
| `tools.overrides-missing` | fail | A configured `introspection.overrides_file` does not exist. |
| `tools.overrides-invalid` | fail | The overrides file does not parse. `mergeOverrides` failures are swallowed with a `log.Printf` (`introspect.go:143-147`), silently reverting flag typing to regex inference and weakening constraints exactly as audit F2 describes. |

### 6.5 `runtime.*` — config vs running server

Reads the pidfile and `serve.state` (`cmd/server.go:139-160`, written at
`cmd/serve.go:199`). All inert file reads; no connection, and every check
degrades to `info: server not running` when there is no state.

| ID | Sev | Check |
|---|---|---|
| `runtime.bind-divergence` | fail | The server's effective listen address differs from the config's. Catches `adjustServerBindForRuntime` widening a loopback bind to `0.0.0.0` (`cmd/up.go:215-231`) — invisible to any file-only audit. |
| `runtime.stale-policy` | warn | Config mtime is newer than the server's last successful load. Reload keeps the **old** policy on any parse or validate failure, so a clean file can coexist with a weaker policy in memory. Remedy: check the server log, then `SIGHUP`. |
| `runtime.uid` | info | The uid the server runs as, when recorded. Feeds the ownership checks (§6.3) and the cache-write decision (§4.1); its absence is why those are `warn`, not `fail`. |
| `runtime.stale-pidfile` | warn | A pidfile exists but names no live process. |

### 6.6 `drift.*` — policy vs installed tools

Requires probing; skipped entirely under `--no-probe`, with one `info` saying
so. Every finding here is gated on parse quality, because the failure mode of a
wrong drift finding is an operator deleting a working rule.

| ID | Sev | Check |
|---|---|---|
| `drift.unknown-subcommand` | warn | An allowed subcommand is absent from the tool's advertised catalog (`introspect.Available`, parsed from top-level help by `BestParseSubcommands`). Either the tool dropped it, or it was always a typo — inert since the day it was written. |
| `drift.unknown-flag` | warn | An allowed flag is absent from that subcommand's parsed flags. |
| `drift.constraint-orphan` | warn | A `flag_constraints` key names a flag not present in the rule's `flags`. Pure config-internal, needs no probe, and is always dead weight — plus `required: true` on such a key makes every call to that rule deny. |
| `drift.tool-version-changed` | info | The cached `VersionOutput` differs from the installed binary's. Explains why other drift findings appeared. |
| `drift.unverifiable` | info | Help output could not be obtained or parsed with sufficient confidence for a tool or subcommand. This is the honest output whenever the checks above cannot run. |

Gating rules, all mandatory:

- A finding fires **only** when the top-level catalog parsed non-empty and
  `BestParse` reported confidence above a threshold for that subcommand.
- Absence is never asserted from an empty parse result. The current
  representation cannot distinguish "help failed" from "no flags"
  (`introspect.go:129-135`); until §4.2's distinction exists, an empty result is
  `drift.unverifiable`, never `drift.unknown-flag`.
- Placeholder-less help (glab is the known case — see
  `design/policy/flag-type-inference.md`) parses weakly by construction; those
  tools land in `drift.unverifiable` rather than producing confident nonsense.

The remedy text never says "delete this rule". It says the flag was not found
in the installed tool's help, names the version probed, and suggests verifying
by hand.

### 6.7 `policy.*` — content lint (heuristic)

This group automates the audit checklist. Every finding is a `warn`: the checks
encode per-tool knowledge and will produce false positives on a policy whose
author knows better. Suppression (§5.2) is what makes that acceptable.

| ID | Sev | Check |
|---|---|---|
| `policy.unpatterned-value-flag` | warn | An allowed value-taking flag has neither a `pattern` nor a non-string type. Free-form value = audit F2's residue. |
| `policy.dangerous-flag` | warn | An allowed flag is on the execute-or-redirect list for its tool and lacks a `pattern`: git `--upload-pack`/`--receive-pack`/`--exec`/`-c`/`--config`; kubectl `--kubeconfig`/`--token`/`--server`/`--as`; gcloud `--impersonate-service-account`/`--flags-file`/`--configuration`; plus generic `--*-file`/`--*-cmd` shapes. |
| `policy.overbroad-pattern` | warn | A `pattern` that constrains nothing: `.*`, `.+`, `.*?`, `(?s).*`, empty. Patterns are auto-anchored since audit F6 (`internal/whitelist/constraints.go:74`), so *unanchored* is correctly not a finding — overbroad is. |
| `policy.uncompilable-pattern` | fail | A `pattern` that does not compile. `regexp.MatchString` runs at call time (`constraints.go:75-78`); the rule fails closed and is permanently uncallable, and nothing reports it. This is the one `fail` in the group — it is not a judgement call. |
| `policy.shell-equivalent-subcommand` | warn | An allowed subcommand is shell-equivalent for its tool: `kubectl exec`/`cp`/`port-forward`/`proxy`, `gcloud compute ssh`. |
| `policy.mutating-subcommand` | warn | An allowed subcommand mutates remote state without capability gating: `kubectl delete/apply/edit/patch/replace/scale/drain`, `gcloud … delete/set-iam-policy/add-iam-policy-binding`, `terraform apply/destroy/import`. |
| `policy.unbounded-positional-count` | warn | An allow rule permits positional args with no `arg_constraints.positional_max`. Note this bounds **count only** — `ArgConstraints` has no content field (`internal/config/config.go:176`); audit F3 (content constraints) remains open upstream, and the finding text must say so rather than implying `arg_constraints` closes it. |
| `policy.deny-prefix-gap` | warn | A deny rule whose subcommand is a strict prefix of a longer allowed subcommand. Deny matches exact length (audit F4). |
| `policy.force-push-allowed` | warn | `git push` allowed with no `deny_flags` covering `--force`/`--force-with-lease`/`-f`. |

**Exemptions, mandatory.** A flag is exempt from `unpatterned-value-flag` and
`dangerous-flag` when it is already neutralized: listed in the rule's
`deny_flags` (checked before the allowlist, `internal/whitelist/policy.go:63-73`)
or carrying `flag_constraints[f].deny: true`
(`internal/whitelist/constraints.go:19-23`). A hardened policy that explicitly
denies `git -c` must not be warned at for doing so.

**What counts as value-taking.** `internal/whitelist` has no such notion —
`validateValue` branches only on whether the constraint type is valueless
(`constraints.go:44-53`) and passes an unconstrained flag's value through
regardless. The source is `mcptools.IsBooleanFlag` (`internal/mcptools/schema.go:78-99`)
plus introspected flag types, with introspection's placeholder-aware default
(unknown type → string) from `design/policy/flag-type-inference.md`. The lint
and the enforcer therefore do **not** agree today, which is itself the point:
the lint flags exactly the gap.

Per-tool knowledge (dangerous flags, shell-equivalent subcommands, mutating
verbs) lives in one in-tree table keyed by tool basename. An unknown tool gets
the generic checks only, plus `info: no per-tool ruleset`.

### 6.8 `pool.*` — harness pool

Deliberately small: `wraptool harness list` already renders ACTIVE/LOCKED/
REALIZED per harness without a backend and without the pool lock. Only what
that table does not show belongs here.

| ID | Sev | Check |
|---|---|---|
| `pool.home-perms` | fail | The pool's `home/` tree is not group/other-readable. It holds every harness's auth state and is shared read-write into every container — the pool's real credential boundary. |
| `pool.generation-integrity` | fail | The active generation's on-disk digest matches its receipt, via `VerifyGenerationTree`/`ScanGenerationFiles` (`internal/pool/receipt.go`) — pure, local, inert. A mismatch is tampering or corruption in the tree that gets mounted into every container. |
| `pool.devcontainer-mount` | warn | With `--root`: an active pointer exists but `.devcontainer/devcontainer.json` declares no read-only runtime mount. `up` already detects this and prints the snippet (`cmd/up.go:793-838`); doctor reports it without needing a container start. |
| `pool.deprecated-feature` | warn | With `--root`: `.devcontainer/devcontainer.json` pins the deprecated `wraptool:0` feature or the sunset `wraptool-connect` feature. A pure file read, and the most operator-visible deprecation in the tree. |

Dropped from the earlier draft: `pool.orphan-generations` (it would have to
duplicate `gc`'s three retention rules — including the "2 most recent per
(harness, platform)" rule at `internal/pool/gc.go:518-547` — and `GCPool` takes
the pool lock even for `--dry-run`, which doctor must not do), and
`pool.lock-recipe-drift` (`RecipeRevision` is a hash with the format version
folded *into* the payload, `internal/pool/orchestrator.go:119,131-142`, so drift
is not readable without recomputing every harness's expected revision through
unexported helpers).

doctor never takes the pool lock: it must work while `up` holds it. A torn read
is an `info` inconsistency, not a reason to block.

## 7. Report shape

Human output is grouped by prefix, findings ordered `fail` → `warn` → `info`,
then by ID, then by location — a total order, with map iteration sorted, so two
runs diff cleanly:

```
wraptool doctor — /home/user/.config/wraptool/config.yaml
server: running as uid 998, sse 0.0.0.0:8717   (config says 127.0.0.1:8717)

environment
  ok    container backend: guix, podman

configuration
  FAIL  config.file-perms: config.yaml is mode 0664 (group-writable).
        Anyone in group `users` can grant themselves any tool.
        → chmod 600 /home/user/.config/wraptool/config.yaml

runtime
  FAIL  runtime.bind-divergence: server is listening on 0.0.0.0:8717 but the
        config says 127.0.0.1:8717. `wraptool up` widens the bind for the
        devcontainer runtime; it is reachable from your network right now.

drift
  warn  drift.unknown-flag: config.yaml:41 — `--recurse-submodules` was not
        found in `git pull --help` (git version 2.45.2).
        → verify by hand; if the flag is gone, remove it from the rule.

policy
  warn  policy.dangerous-flag: config.yaml:37 — `-c` is allowed with no pattern.
        `git -c core.sshCommand=…` executes an arbitrary command.
        → add flag_constraints["-c"].pattern, or deny the flag.
        docs/configuration.qmd#flag-constraints

2 failed, 6 warnings (1 suppressed), 24 passed.
```

## 8. JSON output

```json
{
  "schema": 1,
  "config_path": "/home/user/.config/wraptool/config.yaml",
  "wraptool_version": "0.5.1",
  "probed": true,
  "summary": {"fail": 2, "warn": 6, "suppressed": 1, "info": 4, "pass": 24},
  "findings": [
    {
      "id": "policy.dangerous-flag",
      "severity": "warn",
      "title": "Allowed flag can execute an arbitrary command",
      "detail": "flag `-c` is allowed with no pattern constraint",
      "location": "/home/user/.config/wraptool/config.yaml:37",
      "remedy": "add flag_constraints[\"-c\"].pattern, or deny the flag",
      "doc_ref": "docs/configuration.qmd#flag-constraints",
      "suppressed": false
    }
  ]
}
```

`schema` is versioned independently of the wraptool version.

### 8.1 ID stability and CI gating

Check IDs are a compatibility surface: an ID may be added, and its severity may
be lowered, but renaming or repurposing one is a breaking change.

`--strict` is explicitly **not** a stable CI gate: promoting every `warn` to
`fail` means any release that adds one heuristic check breaks every pinned
pipeline. CI should gate on `--only <groups>` with the deterministic groups
(`config`, `exposure`, `tools`, `runtime`), or on specific IDs from `--json`.
New checks land as `warn` in their first release, so a non-strict gate absorbs
them.

## 9. Implementation sketch

- `internal/doctor/` — check engine and catalog. Takes a loaded config, the
  config's yaml node tree, and injected `LookPath`/`Stat`/`ReadFile`/probe/clock
  interfaces; returns `[]Finding`. No printing.
- `internal/doctor/toolknowledge.go` — the §6.7 per-tool table, data only.
- `cmd/doctor.go` — flags, engine invocation, rendering. Thin.
- Reuses: `config.LoadStrict`/`Validate`, `yamledit` node-tree primitives,
  `paths`, `introspect` (with §4.2's new read-only + injected-env entry points),
  `executor.ChildEnv`, `pool`'s lockless accessors, `mcptools.IsBooleanFlag`.

## 10. Security invariants

1. **Never print a secret.** Token files and `env_files` are checked for
   existence, mode, ownership, and length; contents never appear in the report,
   in `--json`, or in an error. Inline `env:` findings name keys, never values.
   A doctor report is a document operators paste into issues.
2. **Never AI-facing.** No MCP tool, no unauthenticated Web UI surface. The
   report is a map of where the policy is weakest.
3. **No AI-supplied argv.** Probe argv is introspection's help vocabulary plus
   operator-authored config subcommands. Nothing from an MCP client ever
   reaches it.
4. **Scrubbed probe environment** (§4) — `executor.ChildEnv`, matching the AI
   path.
5. **No mutation of config, pool, or request log.** The introspection cache is
   the sole exception, under §4.1's uid rule.
6. **Unknown is a finding.** Where doctor cannot determine something, it emits a
   finding naming what it could not check.

## 11. Test plan

- Table-driven per check, against injected `Stat`/`LookPath`/probe fakes.
- Checks that necessarily touch the real filesystem — `Config.Validate` itself
  stats token files and binaries (`validate.go:49,57,62,168,173`),
  `pool.ActiveGeneration` reads a receipt, `CacheStamp` reads the cache file —
  are tested against a temp-dir fixture tree. The "no real filesystem" claim is
  scoped to doctor's own logic, not to its dependencies.
- Golden human report and `--json` for a fixture carrying one finding of each
  severity, plus a suppressed one.
- A **clean** fixture config asserting an empty finding set — the guard against
  false positives creeping into `policy.*` and `drift.*`.
- Drift-specific: a fixture whose help output fails to parse must produce
  `drift.unverifiable` and **never** `drift.unknown-flag`.
- Exit-code matrix: clean, warn-only, warn-only-with-`--strict`, fail,
  unparseable, config-absent.
- Platform: every mode-bit and ownership check is skipped on Windows with one
  `info` naming the limitation. `paths` supports Windows
  (`internal/paths/paths.go:23`) and there is a `docs/platform-windows.qmd`;
  POSIX bits there would otherwise produce uniform spurious passes.
- Every catalog ID is asserted reachable by at least one test, so a check
  cannot silently become dead.

## 12. Open questions

1. **`--fix` mode.** `config.ApplyRequest`/`AddRule` already do
   comment-preserving atomic edits, so mechanical fixes (`chmod` a token file,
   add a missing `deny_flags`) are within reach. Deferred: a fixer that is wrong
   about policy intent is worse than a report that is.
2. **Per-tool knowledge freshness.** Does the §6.7 table ship in-tree only, or
   is it overridable by an operator file the way introspection overrides are?
3. **Web UI panel.** The same engine could render a health panel on the admin
   listener. Deferred, but it is why the engine returns findings rather than
   printing them.
4. **Drift confidence threshold.** The concrete `BestParse` confidence value
   above which a drift finding may fire needs calibrating against the captured
   `testdata/` help corpus before the group ships.

## 13. Prior art

`brew doctor`, `flutter doctor`, `terraform validate`, `git fsck`. The shape
borrowed: no arguments, everything at once, actionable remedies, CI-usable exit
code. The deliberate divergence is `policy.*` and `drift.*` — those tools check
their own installation, whereas wraptool's most valuable checks are about the
security policy the operator wrote and whether it still matches reality.
