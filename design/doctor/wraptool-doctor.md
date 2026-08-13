# Design: `wraptool doctor`

Status: **implemented**
Scope: a single operator-facing preflight command that answers "is this
installation sane, is my policy as tight as I think it is, and does it still
match the tools actually installed?" It checks host dependencies, strict config
validity, deployment posture, config-vs-running divergence, dangerous policy
content, and drift between the policy and the installed tools. It never edits
config, never starts or contacts the server, and never mutates the harness
pool.

The policy-content group automates the manual checklist in
`design/policy/security-audit-policy-enforcement.md` §"Policy-content
self-audit checklist", which today exists only as prose telling the operator to
grep their own config by hand.

**Delivery is phased** (§3). Phase 1 is everything implementable against
today's data. Phase 2 depends on named changes in `introspect` and
`cmd/server.go` (§4) and cannot start before them.

## 1. Problem

wraptool's failure modes are spread across layers that no single command
covers:

| Layer | Today's coverage | What goes unnoticed |
|---|---|---|
| Host dependencies | none — discovered at use time | `guix`/`docker`/`podman`/`devcontainer` absent; `wraptool up` fails deep inside a subcommand |
| Config syntax & semantics | `wraptool config validate` | nothing (this layer is solved) |
| Deployment posture | partly in `Config.Validate` | world-readable token file, world-writable `config.yaml`, group-readable config holding inline `env:` secrets, `allowed_cwd_roots` unset (audit F5), a symlinked config that silently disables hot-reload |
| Config vs running server | **nothing** | `up` rewrites the bind to `0.0.0.0` in memory only; a failed reload leaves the server on an older, weaker policy |
| Policy content | **nothing** — prose checklist only | value flags with no `pattern`, `.*` patterns that are patterns in name only, `kubectl exec`-class subcommands, `mcp_servers` args that fetch unpinned remote code at every start |
| Policy vs installed tools | **nothing** | subcommands and flags the installed tool does not have (or never had — a typo is silently inert), orphaned `flag_constraints` |

The last three rows are the expensive ones.

A policy that *looks* restrictive but allows `git -c core.sshCommand=… pull` is
worse than no wraptool at all, because it carries the credibility of a
whitelist. `Config.Validate` cannot catch this class: every such config is
structurally valid.

Drift is the quiet counterpart. A rule naming a subcommand the tool dropped, or
a flag misspelled at authoring time, is not an error anywhere — it registers, it
validates, and it never matches. In the **deny** direction the same typo is
fail-open: `deny: [remote, set-utl]` reads as a denial and enforces nothing.

Divergence is the sharpest: `adjustServerBindForRuntime` (`cmd/up.go:215-232`)
rewrites a loopback bind to `0.0.0.0` and forces a token for the devcontainer
runtime, passes it to `serve` as overrides, and never writes it back to disk. A
file-only audit reports "loopback, ok" while the server listens off-host.

## 2. Goals and non-goals

**Goals**

- One command an operator runs after install, after editing config, and in CI.
- Report as many independent problems as possible in one pass. (`Config.Validate`
  returns a single `error` and stops at the first failure — see §4.1; until that
  changes, config-semantic problems surface one at a time and doctor says so.)
- Findings are actionable: each names the location **in the file**, what is
  wrong, and the change to make.
- Deterministic: identical inputs give identical output, so reports diff
  cleanly. The introspection cache is an *input*, never written by default
  (§5.2).
- Machine-readable (`--json`) with stable check IDs.

**Non-goals**

- Not a fixer. doctor never writes config; it prints the change to make.
  `--fix` is deferred (§13).
- Not AI-facing. Never an MCP tool, never on an unauthenticated Web UI surface.
  Its output is a map of exactly where the policy is weakest.
  `wraptool_discover` remains the AI-facing, policy-respecting counterpart.
- Not a liveness check. doctor reads the server's *recorded* state from disk; it
  never opens a connection or requires the server to be running.
- Not a replacement for `wraptool config validate`, which stays as the narrow,
  scriptable primitive the audit doc's preflight instruction points at.

## 3. Phases

Phase 1 ships against today's code. Phase 2 is blocked on §4.

| Phase | Groups | Blocked on |
|---|---|---|
| 1 | `env.*`, `config.*`, `exposure.*`, `tools.*`, `policy.*`, `pool.*`, and `runtime.bind-divergence` | nothing |
| 2 | the rest of `runtime.*` | §4.1 serve-state extension |
| 2 | `drift.*` | §4.2 introspection changes |

Phase 1 is independently useful: it covers every posture, permission, and
policy-content check, which is the bulk of the catalog. Phase 2 adds the two
groups that require other packages to record data they do not record today.

## 4. Prerequisite work in other packages

This is the honest cost of the design. None of it is speculative — each item is
a specific gap found while checking the catalog against the source.

### 4.1 `cmd/server.go` — extend the serve state file

`serve.state` today is two fields:

```go
type serveState struct {   // cmd/server.go:148-151
    Listen string
    Auth   bool
}
```

written as `listen=<addr>\nauth=<bool>\n` (`cmd/server.go:159`) from
`cmd/serve.go:199`. It records no uid, no config path, no load timestamp, no
transport, and no admin listener — so every `runtime.*` check except
`bind-divergence`, plus the uid-dependent ownership and cache rules, has no data
source.

Scoped below. It is small, self-contained, and useful independently of doctor
(`server status` gains the same information).

**Fields.** Every one is `effective` state — what this process is actually
doing — never what the config file says.

| Key | Type | Source | Consumer |
|---|---|---|---|
| `listen` | string | `cfg.MCP.Listen` post-override | existing (`runningMatches`) |
| `auth` | bool | `cfg.MCP.AuthTokenFile != ""` | existing |
| `transport` | string | `cfg.MCP.Transport` | `runtime.*` gating |
| `admin_listen` | string | `cfg.Admin.Listen` | `exposure.admin-*` effective check |
| `uid` | int | `os.Getuid()` | `runtime.uid`, `exposure.token-owner`, `--write-cache` gate |
| `config_path` | string | `EvalSymlinks(Abs(configPath))` | gates every `runtime.*` check |
| `path` | string | `os.Getenv("PATH")` | `env.path-differs-from-server` |
| `started_at` | RFC3339 | process start | report header |
| `config_loaded_at` | RFC3339 | last **successful** load or reload | `runtime.stale-policy` |

**Write sites.** Two, both already single-writer within the serve process:

1. `cmd/serve.go:199` — initial write, unchanged in placement.
2. `cmd/reload.go:71` `reloadConfig`, on the success path only. Both reload
   triggers funnel through it (file watch at `reload.go:60`, SIGHUP at
   `cmd/platform_unix.go:40`), so it is the single hook. A **failed** reload must
   not touch the file — that is precisely the state `runtime.stale-policy`
   detects.

The reload write updates `config_loaded_at` and nothing else. The process does
not rebind on reload, so `listen`/`auth`/`transport`/`admin_listen` must keep
their start-time values even if the new config changes them; refreshing them
from the reloaded config would report a bind that is not in effect — inverting
the check's purpose.

**Presence versus zero value.** `uid` cannot be decoded into a plain `int`:
`0` is root, a legitimate value, and also the zero value that an absent key
yields. The reader must track which keys were present — parse into a
`map[string]string` and expose typed accessors returning `(value, ok)`. Every
consumer treats absent as *unknown* and skips, never as a value.

**Wire format.** Keep `key=value` lines; the existing reader already ignores
unknown keys and unparseable lines (`cmd/server.go:173-188`), so an older
wraptool reads a newer file without error. `strings.Cut` on the first `=` keeps
values containing `=` intact (PATH entries can). Newlines are the one hazard —
a POSIX filename may contain one, so `config_path` could corrupt the file.
Escape `\n` and `\\` on write, unescape on read. Do **not** switch to JSON: an
older `readServeState` would fail to parse it, `runningMatches` would return
false, and every `up` against a running server would needlessly restart it.

**Atomicity.** `writeServeState` currently uses `os.WriteFile`
(`cmd/server.go:160`). Once the file is rewritten on every reload while doctor
and `up` may be reading it, a torn read becomes reachable. Switch to
write-temp-then-rename in the same directory. `internal/pool` already has this
helper shape; either lift it or write the four-line local version.

**Compatibility.** `runningMatches` compares named fields explicitly
(`cmd/up.go:350-357`), not struct equality, and every construction site uses
field names (`cmd/up.go:280`, `cmd/serve.go:199`, `cmd/server_test.go`), so
adding fields breaks nothing. It must keep comparing only `listen` and `auth`:
adding `config_path` or `uid` to that comparison would make `up` restart a
perfectly good server whenever it was started from a different config path.

**Liveness.** A state file survives an uncatchable exit — removal is a `defer`
plus the SIGINT/SIGTERM handler (`cmd/serve.go:202,209`), neither of which runs
on SIGKILL, OOM-kill, or power loss. Every `runtime.*` check is gated on
`processAlive(pid)` (already in `cmd/server.go`, used by `server status`) **and**
on `config_path` matching the config being audited. Without both gates doctor
would report a dead server's bind as live.

**Reachability.** The file lives in `paths.RuntimeDir()` (`cmd/server.go:153`),
which is per-process XDG (`internal/paths/paths.go:48-53`). A server running as
a service user writes somewhere the operator cannot read, so doctor sees no
state and skips every `runtime.*` check — correct behaviour, and the reason
§5.2 refuses to infer "same user" from an absent file.

**Security.** No secrets are added: `auth` stays a bool, never the token or its
path contents. `path` and `config_path` disclose filesystem layout, which is
already the case for the pidfile's directory; the file stays `0600` inside a
`0700` runtime dir.

**Windows.** `os.Getuid()` returns `-1`; omit the key rather than writing it,
and the ownership checks stay `skipped` there (§12).

**Tests.** Extend `cmd/server_test.go`: round-trip all fields; an old-format
file (only `listen`/`auth`) reads with the rest absent-not-zero, including
`uid` distinguishable from root; a value containing `=`, and one containing a
newline, survive; `runningMatches` still matches on `listen`+`auth` alone when
the new fields differ; a failed reload leaves `config_loaded_at` unchanged
while a successful one advances it; a concurrent read during rewrite never
observes a partial file.

**Not in scope.** `server status` could print the effective state instead of
re-probing — a natural follow-up, but it changes user-visible output and is not
needed by doctor.

---

`config.Validate` returning a single error (it returns at the first failure at
every step, `internal/config/validate.go:26-97`) is a related but separate gap:
a multi-error variant would let §2's one-pass goal apply to config semantics
too. Not required for Phase 1; `config.validate` reports one finding until it
exists, and says so.

### 4.2 `internal/introspect` — what `drift.*` needs

1. **A callable read-only cache path.** `LoadCache` is exported
   (`internal/introspect/cache.go:26`) but its parameter type `cacheValidity`
   (`cache.go:17`) is not, so it cannot be called from outside the package.
   Export a validity constructor or type.
2. **A stale-tolerant read exposing `VersionOutput`.** `LoadCache` returns nil
   for a stale entry (`cache.go:43`), and a tool upgrade bumps the binary mtime —
   so the upgrade discards the very cached version string
   `drift.tool-version-changed` needs. `CacheStamp` (`cache.go:59-69`) returns
   only `introspectedAt` and `configHash`.
3. **A probe entry point taking an injected environment**, so doctor can supply
   `executor.ChildEnv(binary, nil)` (§5.1).
4. **Parse-quality signal persisted per subcommand.** `Introspect` discards
   `result.Confidence` (`introspect.go:135-139`) and `type Subcommand`
   (`internal/introspect/types.go:26-29`) has no field for it. This is a cache
   format change.
5. **A positive-parse signal.** Three states are currently indistinguishable:
   help succeeded and the subcommand has no flags; help failed (an empty
   `Subcommand` is stored, `introspect.go:129-132`); and help "succeeded" with
   garbage — `git bogus -h` prints `git: 'bogus' is not a git command.`, which
   clears `getHelp`'s 20-byte threshold (`introspect.go:163`) and is then parsed
   as if it were help. Absence may never be asserted from any of these.
6. **Per-tool catalog probes.** See §5.3 — `-h` is not a catalog.

## 5. Probe policy

doctor performs **inert local execution** of configured tool binaries, and
nothing else:

- **Allowed**: `os.Stat`/`Lstat`, reading config, override, pool, and
  server-state files, `exec.LookPath`, and executing a configured tool binary
  with introspection's help/version vocabulary (`-h`, `--help`,
  `help <subcommand>`, `--version`) plus the per-tool catalog argv of §5.3 —
  including operator-authored subcommand tokens, since that is the only way to
  learn which flags a subcommand has.
- **Forbidden**: any network call, any container-engine invocation, any
  connection to the running server, any argv token not from that vocabulary or
  from operator-authored config, and any mutation of config, pool, or request
  log.

`--no-probe` disables all execution. Project-root detection must therefore not
shell out: `up` derives the root with `exec.Command("git", "rev-parse",
"--show-toplevel")` (`cmd/up.go:1259`), which doctor may not do. doctor walks
upward for `.git`/`manifest.scm` with `os.Stat` instead. The flag is spelled
`--workspace`, matching `up` (`cmd/up.go:73`).

### 5.1 Probe environment

`introspect.runTool` sets no `cmd.Env` (`internal/introspect/introspect.go:203`),
inheriting the caller's full environment. In `serve` that is the service
environment; in doctor it would be the operator's interactive shell, typically
carrying `GH_TOKEN`, `SSH_AUTH_SOCK`, and cloud credentials. doctor probes with
`executor.ChildEnv(binary, nil)` — **explicitly nil `resolved`**, since
`ChildEnv`'s second parameter is the tool's own resolved env
(`internal/executor/executor.go:56`), i.e. exactly the `env`/`env_files`
credentials that must not be handed to a help probe.

This means the probe environment is *not* identical to the AI path's, and the
design makes no such claim: the AI path passes the tool's credentials, doctor
does not.

One consequence must be stated rather than buried: probing operator-authored
subcommand tokens can execute a **plugin** binary. `kubectl <unknown-sub>`
resolves `kubectl-<sub>` from `PATH`, and `childPath` builds that PATH from
doctor's own environment (`executor.go:29`), which may differ from the
server's. This is the same execution surface `serve` already has — `Introspect`
probes every allowed subcommand at each start — but doctor exercises it from a
different PATH, so it can execute a different binary than the server would.
`env.path-differs-from-server` (Phase 2) exists to surface that.

### 5.2 Cache interaction

Probing through `introspect` writes the cache (`SaveCache`,
`introspect.go:150`). This is not a security concern — argv is
operator-authored, the binary is operator-named, and `serve` probes the same
binaries the same way at every start. It is an *ownership* concern: a cache
file written by the operator can block a service-user server's later write.

**doctor does not write the cache by default.** Writing requires
`--write-cache`, which is refused unless the serve state positively proves a
matching uid. The earlier "no state file implies same-user" inference is wrong
in exactly the case it was meant to protect: `paths.RuntimeDir()` is per-process
XDG (`internal/paths/paths.go:48-53`), so a service-user server writes state
into a directory the operator cannot read — doctor sees nothing, would conclude
"same user", and would cause the collision. Absence of state proves nothing.

Not writing also preserves determinism (§2): a cache write would make
`drift.tool-version-changed` report on the first run and not the second.

### 5.3 Subcommand catalogs

`introspect.Available` is not an exhaustive catalog. `getHelp` tries `-h` first
and returns it once output exceeds 20 bytes (`introspect.go:161-165`), and
`git -h` prints only the *common* commands — verified on git 2.54: `stash`,
`config`, `remote`, `worktree`, `rev-parse`, `cherry-pick`, `submodule` are all
absent. Treating that list as a catalog produces a false
`drift.unknown-subcommand` for `git config`, and §6.7's "catalog parsed
non-empty" gate does not help, because git's catalog *is* non-empty.

Therefore the §6.8 per-tool knowledge table gains a **catalog argv** column
(`git help -a`, `kubectl --help`, `gcloud help`, `terraform --help`). A tool
with no catalog argv in the table never produces a subcommand-drift finding —
it reports `drift.unverifiable`. `git help -a` is a MUCH better catalog than
`git -h`, but §7.6's own investigation later found it is still not exhaustive
(missing deprecated spellings and remote-helper plumbing `git`'s own
`--list-cmds` enumerates) — see §7.6's "Named prerequisite: git eligibility"
for the evidence; this is why git carries a catalog argv in the knowledge
table (§6.8) but stays out of `driftEligibleCatalogTools`.

Multi-level subcommands are a second, independent limit: schema keys are
`strings.Join(rule.Subcommand, " ")` (`introspect.go:123`) while catalog entries
are single tokens (`subcommandRe`, `internal/introspect/parser_git.go:23`). So
`subcommand: [compute, instances, list]` — wraptool's own documented gcloud
example (`docs/configuration.qmd:109`) — can never match. Matching is
**first-segment only**; deeper levels report `drift.unverifiable`.

## 6. Check model

Every check yields an outcome plus zero or more findings:

```
Outcome   pass | finding | skipped   // skipped carries a reason
ID        string   // stable, dotted: "policy.unpatterned-value-flag"
Severity  fail | warn | info
Title     string
Detail    string   // what was found, with the concrete value
Location  string   // file:line for config findings (§6.1), a path, or ""
Remedy    string
DocRef    string   // e.g. "docs/configuration.qmd#flag-constraints"
```

A distinct `skipped` outcome is required so a passing check is never confused
with one that did not run. Summary arithmetic: `pass + findings + skipped` =
total checks; `suppressed` is a **subset** of `warn`, reported separately.

Severity:

- **fail** — wraptool is broken, or the credential boundary is provably not what
  the config claims. Not a judgement call, and never fires on a configuration
  that is working as designed.
- **warn** — a heuristic, a default-open setting, or a shape dangerous in most
  deployments but legitimately intended in some.
- **info** — state worth knowing, never a problem.

Catalog rows below are phrased as **the condition that fires the finding**, so
polarity is unambiguous.

A check that cannot run emits `skipped` with a reason. Silence means "checked,
clean", never "did not look" — and §12 lists what is deliberately not covered.

### 6.1 Locations are file positions, keyed by value

Config findings are located by file and line through the `yaml.Node` tree, and
the struct→file mapping key is **the flag or subcommand value, never a slice
index**.

`SeedHelpFlags` (`internal/config/config.go:234-255`) both *appends* `-h`/
`--help` and *drops* any flag listed in `deny_flags`. So `flags: ["-c",
"--depth"]` with `deny_flags: ["-c"]` loads as `["--depth", "-h", "--help"]`:
struct index 0 is `--depth`, file index 0 is `-c`. Any index-based mapping
reports the wrong line. Reading `flags` from the node tree also makes the seeded
help flags naturally invisible to the policy lint, since they do not appear in
the file.

`internal/yamledit` provides `MapGet`/`MapSet`/`EnsureMapChild`/`ScalarNode`/
`StrNode` only — there is no comment accessor and no sequence-index helper, and
its comment "preservation" is preservation-by-not-replacing
(`internal/yamledit/node.go:47-62`), not a read path. doctor reads
`yaml.Node.HeadComment`/`LineComment` directly and adds the sequence-traversal
helpers it needs; whether those land in `yamledit` or in `internal/doctor` is an
implementation call.

### 6.2 Suppression

Per-site suppression ships with the lint groups. Without it, the first operator
annoyed by a heuristic disables the whole group and never sees a real finding
again.

```yaml
tools:
  git:
    allow:
      # doctor:ignore policy.dangerous-flag -- reviewed 2026-08, value is fixed by the wrapper
      - subcommand: [pull]
        flags: ["-c"]
```

Two placements are supported, and they differ in scope because `yaml.v3`
attaches comments differently:

- A comment **before a sequence item** (`- subcommand:`) attaches to the item's
  mapping node and suppresses matching findings anywhere in that rule.
- A comment **before a mapping key** attaches as that key's `HeadComment` — on
  the *key scalar*, not the value — and suppresses matching findings within that
  key's value subtree. Associating key→value is the implementation's job; the
  earlier draft's claim that the comment lands on the enclosing node was wrong.

A suppressed finding is not printed but is counted, and `--json` includes it
with `"suppressed": true` and `"suppression_reason"` carrying the comment's
text after `--`, so an auditor sees what was waived and why.

**Only `warn` is suppressible.** That constraint is only tenable because every
`fail` in the catalog was re-checked against §6's definition; where a `fail`
could fire on a working deployment, it was re-severitied instead (see
`runtime.bind-divergence`).

## 7. Check catalog

### 7.1 `env.*` — host dependencies

| ID | Sev | Fires when |
|---|---|---|
| `env.container-backend` | info | Always — reports which of `guix`/`docker`/`podman` are on PATH. `info`, not `warn`: a host-only deployment serving MCP to a separately managed container is fully supported (`docs/isolated-environments.md`) and must not be nagged. |
| `env.devcontainer-cli` | info | Always — reports whether `devcontainer` is present. |
| `env.no-backend-with-project` | warn | `--workspace` names a project with `.devcontainer/` or `manifest.scm`, but no backend is on PATH. The operator has demonstrably opted into `up`, so absence is a real problem here. |
| `env.guix-runtime-selected` | info | `guix` is on PATH and the workspace has `manifest.scm` — the guix runtime will auto-select (`cmd/guixcontainer.go:56`). |
| `env.xdg-writable` | fail | The nearest **existing** ancestor of the data dir, config dir, or runtime dir is not writable. Existence is not required: these are created lazily (`internal/config/scaffold.go:58`) and `RuntimeDir()` falls back to `CacheDir()/run` when `XDG_RUNTIME_DIR` is unset (`internal/paths/paths.go:48-53`). |
| `env.path-differs-from-server` | info | *(Phase 2)* doctor's `PATH` differs from the server's recorded `PATH`. Tool runtime deps resolve against `executor.childPath`, which reads the **serve** process's environment — doctor cannot verify the server's PATH from its own shell, so it reports divergence rather than a false verdict. Also explains why a probe may hit a different plugin binary (§5.1). |

### 7.2 `config.*` — integrity

| ID | Sev | Fires when |
|---|---|---|
| `config.parse` | fail | `config.LoadStrict` fails — unknown or misspelled keys (audit F1's class). Aborts with exit 2 after the `env.*` group. |
| `config.validate` | fail | `Config.Validate()` fails. One finding per call; the detail notes that validation stops at the first error until §4.1's multi-error variant exists. |
| `config.file-perms` | fail | `config.yaml` (symlinks resolved) is group- or other-writable. Whoever can write it grants themselves any tool. |
| `config.dir-perms` | fail | The directory containing the resolved config is group- or other-writable — rename-in-place substitution defeats a locked-down file. Same severity as `config.file-perms` by the same argument. **Not** when the directory has the sticky bit set (e.g. mode 1777, `/tmp`-style): sticky is exactly the mechanism that blocks a non-owner from renaming/unlinking an entry they don't own, so a sticky world-writable directory is a configuration working as designed, not a fail. The printed mode includes the type and sticky bits (e.g. `1777 (drwxrwxrwt)`), matching what `ls -ld` shows on the same path. |
| `config.file-readable-secrets` | fail | The config is group/other-readable **and** contains an inline `env:` entry, under `tools.*` or `mcp_servers.*` (`internal/config/config.go:50,67`), whose **key name is credential-shaped**: it matches `TOKEN`, `SECRET`, `PASSWORD`, `PASSWD`, `CREDENTIAL(S)`, `API_KEY`, `APIKEY`, `PRIVATE_KEY`, `ACCESS_KEY`, `SESSION_KEY`, or `AUTH`, case-insensitively, as a whole `_`-delimited word or adjacent word-sequence — never a loose substring. A readable config whose inline env: names nothing credential-shaped (e.g. `CLOUDSDK_CORE_PROJECT`, an inline `HOME` override — both prescribed by `docs/configuration.qmd`) gets **no finding at all**, not even a warn. Reports key names only, never values. |
| `config.symlinked` | warn | The config path is a symlink. `watchConfigFile` uses `filepath.Abs`, not `EvalSymlinks`, then watches the *link's* directory (`cmd/reload.go:17-35`) — so with the common dotfiles layout (`~/.config/wraptool/config.yaml` → `~/dotfiles/wraptool.yaml`) hot-reload never fires and edits apply only on SIGHUP or restart. |
| `config.constraint-orphan` | warn | A `flag_constraints` key names a flag not present in the rule's `flags`. Always dead weight, and `required: true` on such a key makes every call to that rule deny. Static — deliberately here rather than in `drift.*` so it runs under `--no-probe` and in the §9.1 CI gate. |

### 7.3 `exposure.*` — deployment posture and secrets

| ID | Sev | Fires when |
|---|---|---|
| `exposure.transport-unsupported` | warn | `mcp.transport` is not `sse` or `unix`. Keyed on the supported set; the passing form prints nothing. |
| `exposure.sse-non-loopback` | warn | Transport is **`sse`** and `mcp.listen` is non-loopback. `Config.Validate` already fails the unauthenticated case (`internal/config/validate.go:43-46`); this warns on the authenticated one. Gated on transport because `ListenIsLoopback` returns false for a unix socket path (`internal/config/listen.go:10-22`). |
| `exposure.admin-non-loopback` | fail | `admin.listen` is non-loopback. The review UI is the approval authority. |
| `exposure.admin-container-reachable` | warn | `admin.listen` is set and `guix` is on PATH. `guix shell --container --network` shares the host netns (`cmd/guixcontainer.go:96`), so an in-container harness reaches `127.0.0.1:8718`. Gated on the backend, not on `--workspace`: the risk is a property of the server config, not of doctor's cwd. |
| `exposure.token-perms` | fail | An `auth_token_file` has group or other bits. |
| `exposure.token-owner` | warn | *(Phase 2)* Token file owner differs from the server's recorded uid. `warn`: a service-user deployment legitimately has files doctor does not own. |
| `exposure.token-weak` | warn | Token is empty, under 32 bytes, or a known placeholder. Reports length and shape only. |
| `exposure.listeners-collide` | fail | `mcp.listen` and `admin.listen` share a port AND their expanded bind sets have a non-empty intersection — `0.0.0.0:8717` vs `127.0.0.1:8717` is unequal as a string but collides at bind. Each host expands to the addresses it actually binds (`""`/`0.0.0.0`/`::` are wildcards overlapping everything, `localhost` expands to `{127.0.0.1, ::1}`, anything else is its parsed `net.IP` or, for a non-IP hostname, a case-insensitive literal match) — NOT a bare loopback-vs-loopback predicate, which would wrongly fail two distinct loopback IPs (e.g. `127.0.0.1:8717` and `127.0.0.2:8717`) that both bind and run correctly. |
| `exposure.unix-socket-dir` | fail | For `unix` transport, the nearest existing ancestor of the socket path is group/other-writable. Existence is not required — `serve` creates it at bind. |
| `exposure.cwd-roots-unset` | warn | `mcp.allowed_cwd_roots` is empty — the documented default-open case (audit F5). |
| `exposure.request-log-writable` | fail | The nearest existing ancestor of `mcp.request_log` is not writable. The directory is created lazily (`internal/requestlog/store.go:47`). |
| `exposure.request-log-perms` | fail | An existing request log has group or other bits. `Store.append` creates it `0600` (`store.go:50`) but never repairs a pre-existing permissive file, and it records every tool call. |

### 7.4 `tools.*` — wiring

| ID | Sev | Fires when |
|---|---|---|
| `tools.binary-executable` | fail | A `binary` is not executable. `validateTool` checks absolute/exists/regular (`internal/config/validate.go:158-203`), not the mode bits. |
| `tools.binary-writable` | warn | The resolved binary or a path segment leading to it is group/other-writable — arbitrary code inside the credential boundary. `warn` because Intel-macOS Homebrew makes `/usr/local` group-writable by design and the repo ships a tap; the detail names the segment and its group. |
| `tools.working-dir` | warn | `working_dir` is set and is not an existing absolute directory. Validated nowhere today. **Amended (B6):** `working_dir` is only the LAST fallback in `paths.ResolveCWD` (`internal/paths/context.go`) — an explicit client `?cwd=` wins first, a unix-socket peer PID's `/proc/<pid>/cwd` second — and every harness config wraptool generates appends `?cwd=` unconditionally unless `--global` (`internal/harness/harness.go`'s `appendCWD`; `cmd/serve.go`'s `cwdContextFunc` injects it), so under `wraptool up`/`wraptool init` the configured value is never consulted at all. `warn`, not `fail`: the check cannot know whether a given deployment ever reaches the fallback. The detail names what IS knowable — this is the fallback used only when the client supplies no `?cwd=` and no peer-PID cwd resolves, and a bad value fails that call when it is reached. |
| `tools.env-file-missing` | fail | An `env_files` entry (on `tools.*` **or** `mcp_servers.*`) is missing or unreadable. |
| `tools.env-file-perms` | fail | An `env_files` entry is group/other-readable — these hold credentials by definition. |
| `tools.mcp-server-command` | fail | An `mcp_servers.*.command` is not absolute or does not exist. `validateMCPServers` checks non-emptiness only (`validate.go:122-124`), so `command: npx` resolves from the server's PATH at spawn — a hijack vector with no equivalent on the `tools.*` side. |
| `tools.mcp-server-args-unpinned` | fail | ANY `npx`/`uvx` invocation (regardless of `-y`/`--yes`) whose resolved package spec is a `@latest`/dist-tag or otherwise unpinned version. Args are passed verbatim to the child (`internal/mcpproxy/client.go:135`) and validated nowhere, so an absolute, existing `command` still executes unpinned remote code at every serve start — with `env_files` credentials in its environment. `tools.mcp-server-command` alone would give false assurance here. **Amended (B7):** an earlier revision gated npx on `-y`/`--yes` on the premise that npx without it "would block on a confirmation prompt" — false when measured against the spawned-child shape (`internal/mcpproxy`'s stdio child has no TTY, so npm's exec confirmation proceeds non-interactively regardless of `-y`); the Detail says so. The package-spec is read conservatively: an explicit `-p`/`--package`(npx)/`--from`(uvx) flag's value wins, else the first bare positional IF every argument ahead of it is a recognized runner switch or a joined `--flag=value` — an unrecognized value-taking flag (`--python`, `--with`, uv's own `-p`/`--python`) ahead of the positional makes the spec unidentifiable and the check emits nothing rather than mistake that flag's value for the spec. The scan is a single left-to-right pass that STOPS at the first bare positional, because everything after the spec belongs to the spawned server: an unbounded scan reads the CHILD's flags as the runner's. An npm range or wildcard (`@*`, `@^1.2`, `@>=1.0`, `@1.x`) is not a pin — it resolves to whatever the registry serves at that moment, the same moving target `@latest` names. A local spec (`./server`, `/opt/server`) fetches nothing and is exempt. The runner is matched with its Windows executable suffix stripped (`npx.cmd`, `uvx.exe`), or the check would silently pass on the platform where those ARE the runner names. |
| `tools.mcp-server-inert` | warn | An `mcp_servers` entry has zero allow rules but still spawns the upstream child (audit M1). |
| `tools.inert` | info | A tool has zero `allow` rules. `info`: this is the deliberate bootstrap state `wraptool_discover` exists to serve, and `validateTool` permits it explicitly. |
| `tools.overrides-missing` | fail | A configured `introspection.overrides_file` does not exist. |
| `tools.overrides-invalid` | fail | The overrides file does not parse. `mergeOverrides` failures are swallowed with a `log.Printf` (`introspect.go:143-147`), silently reverting flag typing to regex inference and weakening constraints exactly as audit F2 describes. |

### 7.5 `runtime.*` — config vs running server

Every check here is gated on **a live pid** and on the state file's
`config_path` matching the config being audited; otherwise `skipped`. The state
file survives SIGKILL (§4.1), so its presence alone proves nothing.

| ID | Sev | Fires when | Phase |
|---|---|---|---|
| `runtime.stale-pidfile` | warn | A pidfile exists naming no live process. | 1 |
| `runtime.bind-divergence` | warn / fail | The server's effective listen address differs from the config's. **`warn`** when auth is enabled and the difference matches `adjustServerBindForRuntime`'s exact transformation (`cmd/up.go:215-232`) — that is `up` working as designed for the devcontainer runtime, and a `fail` there would be permanently red in every such deployment's CI. **`fail`** for any other divergence, or any unauthenticated widening. | 1 |
| `runtime.stale-policy` | warn | Config mtime is newer than `config_loaded_at`. Reload keeps the **old** policy on any parse or validate failure, so a clean file can coexist with a weaker in-memory policy. Remedy: check the server log, then SIGHUP. | 2 |
| `runtime.uid` | info | Always — reports the uid the server runs as. Feeds the ownership checks and the `--write-cache` gate. | 2 |

### 7.6 `drift.*` — policy vs installed tools *(Phase 2)*

Requires probing; `skipped` under `--no-probe`. The failure mode of a wrong
drift finding is an operator deleting a working rule, so every rule below is
mandatory.

| ID | Sev | Fires when |
|---|---|---|
| `drift.unknown-subcommand` | warn (allow) / **fail** (deny) | A rule's first subcommand segment is absent from the tool's catalog (§5.3). Deny rules are `fail`: a typo'd deny is fail-open, silently removing a denial the operator believes is enforced — and `docs/examples.qmd:35-37` is built on deny rules. `Config.Validate` checks only non-emptiness there (`validate.go:196-200`). |
| `drift.unknown-flag` | warn | An allowed flag is absent from that subcommand's parsed flags **and** the parse cleared the coverage bar below. |
| `drift.tool-version-changed` | info | The cached `VersionOutput` differs from the installed binary's. Explains why other drift findings appeared. |
| `drift.unverifiable` | info | Anything could not be confirmed: no catalog argv for the tool, a multi-level subcommand, a failed or low-coverage parse. This is the expected output for many tools, not an edge case. |

**Gating rules.**

- Subcommand drift requires a per-tool catalog argv (§5.3). No catalog, no
  finding.
- Only the **first** subcommand segment is matched; deeper levels are
  `unverifiable`.
- Flag drift is gated on a **coverage** signal, not on parse confidence.
  Confidence is a four-value step function of *parser identity*, not parse
  quality: 0.3 fallback (`parser_fallback.go:54-57`), 0.6/0.7 GNU
  (`parser_gnu.go:69-74`), 0.8/0.9 git (`parser_git.go:172-177`), where the only
  gradation is `matchCount > 5`. Measured: `git log -h` parses 7 flags at 0.90 —
  the maximum — while missing `--oneline`, `--graph`, `--pretty`, `--since`,
  `--author`. Thresholding that produces maximum-confidence false positives.
  The usable signal is the fraction of the rule's own flags found, plus a
  per-tool "help is exhaustive" assertion in the §6.8 table; anything short of
  both is `unverifiable`.
- Absence is never asserted from an empty or unvalidated parse — §4.2 item 5's
  positive-parse signal is a hard prerequisite. A non-empty result is not proof:
  `git bogus -h` yields a 53-byte error message that `getHelp` returns as
  success.
- A flag that drift could not confirm must not then be counted as
  unconstrained by `policy.unpatterned-value-flag` — an unparsed flag has no
  introspected type and would otherwise generate a second, compounding false
  positive through the string default (§7.7).

Remedy text never says "delete this rule". It says the flag or subcommand was
not found in the installed tool's help, names the version probed, and asks the
operator to verify.

#### Rulings taken while F1 shipped

Six decisions came out of F1's review rounds. The code cites them by number, so
they are recorded here rather than living only in a review thread.

1. **The cache read is root-parameterized.** `introspect.LoadCacheStale`
   resolves through `paths.CacheDir()`, so a check calling it reads the
   developer's real cache even under a `MapEnv` unit test — measured, and it
   made the registry-wide invariants non-hermetic. `LoadCacheStaleAt` takes an
   explicit directory and is the only way a check may reach the cache;
   `invariantsForbiddenHostSelectors` names the host-rooted entry points so the
   fix cannot be reverted silently.
2. **The shared fixture never executes host binaries.** Once `NeedsProbe` was
   satisfiable, `invariantsFullyPopulatedInput`'s `/usr/bin/*` paths made
   `go test ./internal/doctor` spawn real processes. Its binaries point at a
   guaranteed-absent directory, so a probe fails deterministically and every
   check reports its honest cannot-verify.
3. **Silence must not imply verifiable.** `drift.unverifiable` reads a
   stale-tolerant cache, so staying quiet about a rule claimed it was checked
   against the installed tool. It now reports when the cached schema was built
   from a different binary path, or predates the binary's mtime.
4. **The missing half of the flag gate is named.** §7.6 gates a flag claim on
   coverage *and* a per-tool "help is exhaustive" assertion. That assertion
   exists for no tool, so no flag-level absence can be confirmed today — and a
   full-coverage rule read as flag-verifiable until the check said so out loud.
5. **Deny rules get their honest counterpart before F2.** F2 makes a typo'd
   deny rule the `fail`-severity case because it is fail-open; its "could not
   tell" partner ships first, and findings say `allow` or `deny`.
6. **A reason with no text produces no finding.** A reason missing from the
   order or text table was silently dropped, yielding a causeless
   "could not verify". The reasons are an iota enum with a completeness test,
   and a finding whose sentences came out empty is refused.

#### Rulings taken while F2 shipped

Three decisions came out of F2's first review round, all sharing one
structural cause: `driftPluginRiskyTools` was a DENYLIST, so any tool with a
`CatalogArgv` could assert an absence by default — inverted from the design's
intent exactly. git, the only tool this step actually measured, was
excluded, while gcloud and terraform — verified on NO host — were the only
tools that COULD produce an absence claim. The code cites these by number.

1. **Eligibility for a subcommand-absence claim is an ALLOWLIST, not a
   denylist.** `driftEligibleCatalogTools` (`internal/doctor/drift.go`)
   replaces `driftPluginRiskyTools`: a tool's basename is a key IFF its
   `CatalogArgv` has been run against a REAL, installed binary and confirmed
   to enumerate everything the tool will dispatch — built-ins, aliases, and
   external/plugin commands — with the version measured and what was checked
   recorded alongside (`VersionMeasured`/`Evidence`). A tool absent from the
   map never produces a `drift.unknown-subcommand` finding; it falls through
   to `drift.unverifiable` instead, whose per-rule reason
   (`driftReasonCatalogNotVetted`) says its catalog has not been vetted for
   completeness. A reviewer downloaded real terraform (1.9.8 and 1.15.8) and
   demonstrated the consequence of the denylist end to end: `terraform env`
   is a working, undocumented built-in subcommand absent from
   `terraform --help`, so the denylist reported it as not existing. The
   allowlist is expected to be EMPTY or nearly so — shipping a check that
   fires rarely and truthfully beats one that fires often and sometimes
   lies.
2. **A sentinel guards against a truncated or reshaped catalog.**
   `CatalogProbe.Exhaustive` is derived from exit status, output shape, and
   non-emptiness alone — none of those can see CONTENT truncation: a probe
   that prints only the first N lines of a real tool's help and exits 0
   still reports `Exhaustive == true`. Each `driftEligibleCatalogTools` entry
   therefore also records a `Sentinel` — a subcommand certain to be present
   in the vetted catalog (`terraform init`, `gcloud version`, `git commit`).
   If a probed catalog does not contain it, the probe result does not match
   what was vetted: no absence claim is made this run
   (`driftCatalogEligibilityReason`), and `drift.unverifiable` reports the
   mismatch instead. This is a BOUND on the probe result, not an enumeration
   of truncation shapes.
3. **The eligible set is pinned by a test, not left incidental.**
   `TestDriftCatalogEligibilityIsExplicit` (`drift_test.go`) walks
   `toolKnowledgeTable` and requires every entry with a non-empty
   `CatalogArgv` to be accounted for in EXACTLY ONE of
   `driftEligibleCatalogTools` (with evidence) or `driftCatalogExcludedTools`
   (with a reason) — so a later `CatalogArgv` addition can never silently
   grant absence-claim authority by omission; the reviewed decision is
   visible in the test, not implied by what a map happens to leave out.

**Named prerequisite: git eligibility — RESOLVED (the parser gap), but git
STAYS EXCLUDED (a second, independent reason). SUPERSEDED — the conclusion
below stands, but for a third and decisive reason: see "Phase F closes as
informational-only" at the end of this section. Nothing here needs acting on;
it is kept for the measurements.** The parser gap this section
originally recorded is fixed: `internal/introspect/parser_git.go`'s
`ParseSubcommands` now recognizes a bare, description-less catalog line (git
2.54.0's `git help -a`, with a `git-foo` script placed on `PATH`, lists it
under a genuine "External commands" section with no second, description-
carrying column at all — `cat -A`: `   foo$`, three spaces, the name, nothing
after it, verified directly). The fix is gated, not a blanket relaxation of
`subcommandRe`: a bare line is trusted as a catalog entry only when the
WHOLE help text's 2-or-more-space-indented content sits at one single,
uniform indentation width, and that width already carries at least one
genuine two-column entry — see `ParseSubcommands`' own doc comment for why a
looser bound (e.g. "matches any width already used by a two-column entry
anywhere in the text") is unsafe: measured directly against every fixture in
`testdata/` and this host's own installed tools, several REAL help texts
(a subcommand-specific git help page's own flag-value enumeration; a live
`pi --help` capture's "Examples:" section, which uses the identical 2-space
indentation as its real subcommand list) contain an indented bare word that
is prose, not a listing entry, and would have become a false catalog entry
under a width-matching-alone rule. git's own built-ins and `git config
alias.*` entries are unaffected by any of this (verified separately: `git
help -a` lists a "Command aliases" section in the SAME two-column,
WITH-description shape, e.g. `   st                      status`, which
`subcommandRe` already matched correctly before this fix).

Fixing the parser was NOT enough to make git eligible. Verifying it exposed
an independent, more fundamental problem: `git help -a`'s own printed catalog
is not exhaustive even read by a perfect parser. Measured, on git 2.54.0:
`git --list-cmds=main,others,alias` — git's OWN internal command-enumeration
mechanism, confirmed to correctly include the PATH-installed external command
and gitconfig aliases used above — lists at least 24 names `git help -a`
never prints at all: deprecated spellings (`init-db`, `fsck-objects`) and
remote-helper/protocol plumbing (`upload-pack`, `receive-pack`,
`http-fetch`, `http-push`, `remote-https`, `remote-http`, `remote-ftp`,
`remote-ftps`, `remote-ext`, `remote-fd`, `shell`, `stage`, `subtree`,
`pickaxe`, `merge-recursive` and its `-ours`/`-recursive-ours`/
`-recursive-theirs`/`-resolve`/`-subtree`/`-octopus` siblings,
`upload-archive`). Every one of these DISPATCHES: `git init-db` and
`git fsck-objects` were run directly (created a repository; ran a
connectivity check, exit 0); the rest were confirmed via their own real
usage/error output (`git upload-pack` prints its own
"usage: git-upload-pack ..." line; `git shell` reports its own "Interactive
git shell is not enabled" — never git's `'X' is not a git command` message,
which is what a unique-prefix abbreviation (`git stat`), a case variant
(`git Status`/`git STATUS`), and an ambiguous prefix (`git chec`) all
produce instead — so abbreviation and case are confirmed NOT a second gap).
The eligibility bar (RULING 1 above) is enumerating EVERYTHING the tool
dispatches; `git help -a` provably does not, independent of the parser that
reads it. git therefore remains in `driftCatalogExcludedTools`, with this
finding recorded as its reason (`internal/doctor/drift.go`) — see
`internal/introspect/introspect_test.go`'s
`TestGitParser_ParseSubcommands_NameOnlyExternalCommands` and
`TestGitParser_ParseSubcommands_NameOnlyRequiresUniformIndentation` for the
parser fix's own regression coverage, and
`testdata/git_help_a_external.txt` for the captured fixture (git 2.54.0,
external commands + aliases).

#### Phase F closes as informational-only

**Decision: `drift.unknown-subcommand` never asserts that a subcommand is
absent. The eligibility allowlist stays permanently empty, and F3
(deny-rule subcommand drift at `fail`) and F4 (`drift.unknown-flag`) are not
built.** What the drift group ships is what it can support: which rules it
could not verify, and precisely why.

This supersedes the two reasons recorded above. Both were true and neither was
the real one. The parser gap was fixable, and was fixed. `git help -a`'s
incompleteness was real, and `git --list-cmds=main,others,alias` answers it —
it enumerates built-ins, PATH-installed externals and aliases, omits a typo,
and unlike probing a subcommand it does not execute an alias body (measured: a
repo whose `.git/config` defines `alias.wtexec = !script` runs the script under
`git wtexec -h` and does not under `--list-cmds`). Two rounds of review then
found the actual obstacle, and it disqualifies the whole class of claim.

**A tool's dispatchable set depends on ambient configuration, and doctor probes
from outside it.** Every axis below was measured, each in a separate review
round, each after the previous one was closed:

| axis | probe sees | execution sees | detectable? |
|---|---|---|---|
| working directory | `Input.Workspace` | `paths.ResolveCWD`: the CLIENT's cwd, per call | no — see below |
| declared `env` / `env_files` | withheld (`ChildEnv(binary, nil)`) | the tool's own resolved env | yes, gate on it |
| `PATH` | doctor's | serve's | only when `serve.state` records one |
| `working_dir` | ignored | `ResolveCWD`'s fallback | yes |
| `HOME` | doctor's | serve's | **no** — `serve.state` records no `home` |

The working-directory row is decisive on its own. `ResolveCWD`
(`internal/paths/context.go`) prefers a client-supplied `?cwd=` **per MCP
call**, then a Unix-socket peer's `/proc/<pid>/cwd`, and only then the
configured `working_dir`. git resolves repo-local aliases from the working
directory's `.git/config`, so a git tool's dispatchable set genuinely differs
between two calls made seconds apart. There is no single environment doctor
could probe that answers for all of them. The strongest statement any probe
supports is "absent in the environment I probed", and that is not "absent".

`HOME` shows the same thing from the other side: a global alias in
`$HOME/.gitconfig` appears under one `HOME` and not another (measured), and
`serve.state` records no `home` key — so the gate cannot even detect the
difference, let alone close it.

Five axes were found across five rounds, each round closing the named ones and
uncovering more. That is the signature of enumerating cases where a bound was
needed, and the bound, once stated, rules the claim out rather than repairing
it: **an absence claim about a tool whose dispatch depends on ambient
configuration is not soundly derivable from an out-of-band probe.** Loading
enough environment to make it sound is not available either — closing the env
axis honestly would mean a catalog probe reading the tool's `env_files`, and a
probe that reads credentials to check for typos is a worse trade than not
checking.

Two approaches were built and abandoned in reaching this, both recorded because
the reasons generalize:

- **Per-subcommand rejection probes** (`drift.subcommand-rejected`, branch
  `impl/doctor-rejection`, abandoned). git states plainly that it does not have
  a subcommand — `git zwitch -h` prints `'zwitch' is not a git command` and even
  names the correction — which is stronger evidence than any catalog listing,
  and sidesteps completeness entirely. It was abandoned because probing an
  operator-authored token EXECUTES a shell-bodied git alias: a reviewer
  demonstrated `wraptool doctor` creating a side-effect file. A tool that audits
  a repository must not execute what it finds there.
- **`--list-cmds` as an exhaustive catalog** (branch `impl/git-listcmds`,
  abandoned). Complete and side-effect free, and still defeated by the
  environment axes above.

What an operator gets instead, and it is not nothing: `drift.unverifiable`
names every rule it could not confirm and the reason (no cached schema, a cache
from a different build or binary, a help page that did not parse, a
multi-segment subcommand, an unvetted catalog). `drift.tool-version-changed`
explains when the tool moved underneath the cache. A typo like `git zwitch`
surfaces as a rule whose help output the tool did not recognise — honest,
caveated, and correct — with the authoritative answer arriving where it always
was: the call fails at execution.

Revisit if `serve.state` ever records the environment a call actually ran in,
or if a tool appears whose dispatchable set is a pure function of its binary.
Neither is true of git today.

### 7.7 `policy.*` — content lint (heuristic)

Every finding is `warn` except the one that is not a judgement call.
Suppression (§6.2) is what makes the heuristics acceptable.

| ID | Sev | Fires when |
|---|---|---|
| `policy.unpatterned-value-flag` | warn | An allowed value-taking flag has neither a `pattern` nor a non-string type — audit F2's residue. |
| `policy.dangerous-flag` | warn | An allowed flag is on the execute-or-redirect list for its tool and lacks a `pattern`: git `--upload-pack`/`--receive-pack`/`--exec`/`-c`/`--config`; kubectl `--kubeconfig`/`--token`/`--server`/`--as`; gcloud `--impersonate-service-account`/`--flags-file`/`--configuration`; plus generic `--*-file`/`--*-cmd` shapes. |
| `policy.overbroad-pattern` | warn | A `pattern` constrains nothing: `.*`, `.+`, `.*?`, `(?s).*`. An EMPTY pattern is deliberately excluded — it is indistinguishable from an absent one, so counting it fired on every constraint without a pattern; that case belongs to `policy.unpatterned-value-flag`. Patterns are auto-anchored since audit F6 (`internal/whitelist/constraints.go:74`), so *unanchored* is correctly not a finding. |
| `policy.uncompilable-pattern` | fail | A `pattern` does not compile. `regexp.MatchString` runs at call time (`constraints.go:75-78`); the rule fails closed and is permanently uncallable, and nothing reports it. |
| `policy.shell-equivalent-subcommand` | warn | An allowed subcommand is shell-equivalent for its tool: `kubectl exec`/`cp`/`port-forward`/`proxy`, `gcloud compute ssh`. |
| `policy.mutating-subcommand` | warn | An allowed subcommand mutates remote state without capability gating: `kubectl delete/apply/edit/patch/replace/scale/drain`, `gcloud … delete/set-iam-policy/add-iam-policy-binding`, `terraform apply/destroy/import`. |
| `policy.unbounded-positional-count` | warn | An allow rule permits positionals with no `arg_constraints.positional_max`. This bounds **count only** — `ArgConstraints` has no content field (`internal/config/config.go:176`); audit F3 remains open upstream and the finding text must say so rather than implying `arg_constraints` closes it. |
| `policy.deny-prefix-gap` | warn | A deny rule's subcommand is a strict prefix of a longer allowed subcommand. Deny matches exact length (audit F4). |
| `policy.force-push-allowed` | warn | `git push` is allowed with no `deny_flags` covering `--force`/`--force-with-lease`/`-f`. |
| `policy.clone-allowed` | warn | **Added (post-D3):** a git tool allows `clone` at all. Unlike the other §7.7 rows this questions the capability rather than its flags: wraptool's own workflow never needs it (`wraptool up` derives its workspace from an already-checked-out repository, via `cmd/up.go`'s `gitToplevel`), while allowing it grants a client-chosen URL fetched with the host's git credentials, a working tree written at a client-chosen path (unconstrained whenever `mcp.allowed_cwd_roots` is empty, audit F5's default-open case), and a new repository whose own hooks and config later wrapped calls run. The remedy is to clone on the host, where the intent is explicit. |
| `policy.mcp-dangerous-tool` | warn | An `mcp_servers.*.allow` entry names an upstream tool matching an execute-or-write shape (`*execute*`, `*command*`, `*shell*`, `*write_file*`, `*delete*`). `MCPAllowRule`/`MCPDenyRule` are bare `{Tool string}` (`internal/config/config.go:80-88`) — the entire surface is name-matched, with no subcommand or flag structure to constrain, so a name heuristic is the only lint available. Without it the CLI side gets nine heuristics and the MCP side none. |

**Exemptions, mandatory.** A flag is exempt from `unpatterned-value-flag` and
`dangerous-flag` when already neutralized: listed in the rule's `deny_flags`
(checked before the allowlist, `internal/whitelist/policy.go:64-75`) or carrying
`flag_constraints[f].deny: true` (`internal/whitelist/constraints.go:19-23`). A
hardened policy that explicitly denies `git -c` must not be warned at for doing
so.

**What counts as value-taking.** `internal/whitelist` has no such notion —
`validateValue` branches only on whether the constraint type is valueless
(`constraints.go:44-53`) and passes an unconstrained flag's value through
regardless. The source is `mcptools.IsBooleanFlag`
(`internal/mcptools/schema.go:78-99`) plus introspected flag types, with the
placeholder-aware default (unknown → string) from
`design/policy/flag-type-inference.md`. The lint and the enforcer therefore do
**not** agree today; that gap is what the lint reports.

### 7.8 Per-tool knowledge table

One in-tree table keyed by tool basename, holding: catalog argv (§5.3), whether
help is exhaustive enough for flag drift, dangerous flags, shell-equivalent
subcommands, mutating verbs. An unknown tool gets the generic checks only, plus
`info: no per-tool ruleset`. Whether operators may extend it with their own file
is §13.2.

### 7.9 `pool.*` — harness pool

Deliberately small: `wraptool harness list` already renders ACTIVE/LOCKED/
REALIZED without a backend or the pool lock. Only what it does not show belongs
here. doctor never takes the pool lock — it must work while `up` holds it.

| ID | Sev | Fires when |
|---|---|---|
| `pool.home-perms` | fail | The pool's `home/` tree is group/other-readable. It holds every harness's auth state and is shared read-write into every container. |
| `pool.generation-digest` | fail | The active generation's receipt `ManifestDigest` does not match `harness-lock.json`'s recorded digest for that (harness, platform) — the same external anchor `gc` uses (`internal/pool/gc.go:513`). Cheap, and it is the check that detects *tampering*: the receipt lives inside the tree it attests (`internal/pool/activate.go:65`) and `ScanGenerationFiles` excludes it (`internal/pool/receipt.go:75`), so tree-vs-receipt verification alone cannot catch an attacker who rewrites both. |
| `pool.generation-corrupt` | fail | *(opt-in, `--verify-pool`)* `VerifyGenerationTree` reports a content mismatch. Not default: `ScanGenerationFiles` sha256-hashes every file in the tree (`internal/pool/receipt.go:165` `hashFile`), which for a node+npm generation is 10k+ files and hundreds of MB on every run. |
| `pool.devcontainer-mount-missing` | warn | `--workspace` has an active pointer but `.devcontainer/devcontainer.json` declares no runtime mount. `up` already detects this and prints the snippet (`cmd/up.go:803`). |
| `pool.devcontainer-mount-writable` | fail | The mount is declared **without** `,readonly`. `up`'s own notice for this case begins `SECURITY:` (`cmd/up.go:823`): the pool tree whose dispatchers every project's container executes would be writable from inside one project's container. Splitting this from the missing case is why the earlier single `warn` row was wrong. |
| `pool.deprecated-feature` | warn | `.devcontainer/devcontainer.json` pins the deprecated `wraptool:0` feature or the sunset `wraptool-connect` feature. |

Dropped: `pool.orphan-generations` (would duplicate `gc`'s three retention
rules, including "2 most recent per (harness, platform)",
`internal/pool/gc.go:518-547`, and `GCPool` takes the pool lock even for
`--dry-run`) and `pool.lock-recipe-drift` (`RecipeRevision` folds the format
version *into* a hash, `internal/pool/orchestrator.go:119,131-142`, so drift is
unreadable without recomputing every harness's expected revision through
unexported helpers).

## 8. Report shape

Findings order `fail` → `warn` → `info`, then ID, then location — a total
order, with map iteration sorted, so runs diff cleanly.

```
wraptool doctor — /home/user/.config/wraptool/config.yaml
server: pid 4412 live, sse 0.0.0.0:8717 (config says 127.0.0.1:8717, auth on)

configuration
  FAIL  config.file-perms: config.yaml is mode 0664 (group-writable).
        Anyone in group `users` can grant themselves any tool.
        → chmod 600 /home/user/.config/wraptool/config.yaml

  warn  config.symlinked: config.yaml → ~/dotfiles/wraptool.yaml. Hot-reload
        watches the link's directory, so edits to the target never trigger a
        reload. → send SIGHUP after editing, or store the file directly.

tools
  FAIL  tools.mcp-server-args-unpinned: config.yaml:88 — mcp_servers.fetch runs
        `/usr/bin/npx -y @modelcontextprotocol/server-fetch@latest`, fetching
        and executing unpinned remote code at every server start, with
        env_files credentials in its environment.
        → pin an exact version, or vendor the server locally.

runtime
  warn  runtime.bind-divergence: server listens on 0.0.0.0:8717, config says
        127.0.0.1:8717. This matches `up`'s devcontainer bind adjustment and
        the listener is authenticated — expected if you started it with
        `wraptool up`.

drift
  warn  drift.unknown-subcommand: config.yaml:52 — deny rule [remote, set-utl]
        is not a `git remote` subcommand (git 2.54.0). A typo'd deny enforces
        nothing. → did you mean `set-url`?
  info  drift.unverifiable: gcloud — no catalog probe for this tool; and
        `compute instances list` is multi-level, which cannot be matched.

policy
  warn  policy.dangerous-flag: config.yaml:37 — `-c` is allowed with no
        pattern. `git -c core.sshCommand=…` executes an arbitrary command.
        → add flag_constraints["-c"].pattern, or deny the flag.
        docs/configuration.qmd#flag-constraints

2 failed, 5 warnings (1 suppressed), 3 info, 22 passed, 6 skipped.
```

## 9. JSON output

```json
{
  "schema": 1,
  "config_path": "/home/user/.config/wraptool/config.yaml",
  "wraptool_version": "0.5.1",
  "probed": true,
  "incomplete": false,
  "incomplete_reason": "",
  "config_state": "loaded",
  "summary": {"fail": 2, "warn": 5, "suppressed": 1, "info": 3, "pass": 22, "skipped": 6},
  "findings": [
    {
      "id": "policy.dangerous-flag",
      "severity": "warn",
      "title": "Allowed flag can execute an arbitrary command",
      "detail": "flag `-c` is allowed with no pattern constraint",
      "location": "/home/user/.config/wraptool/config.yaml:37",
      "remedy": "add flag_constraints[\"-c\"].pattern, or deny the flag",
      "doc_ref": "docs/configuration.qmd#flag-constraints",
      "suppressed": false,
      "suppression_reason": ""
    }
  ],
  "skipped": [{"id": "drift.unknown-flag", "reason": "--no-probe"}]
}
```

`suppressed` counts a subset of `warn`. `schema` is versioned independently of
the wraptool version.

`incomplete`/`incomplete_reason`/`config_state` are additive (wave-1 followups
item 9) and do not bump `schema`: without them, a run that could not complete
(an unknown `--only` selector, an unreadable config, an internal error) still
wrote a structurally clean, all-zero report to stdout, with the cause named
only on stderr and the exit code — a CI consumer that parses stdout without
also checking the process exit code would read `wraptool doctor --only bogus
--json` as a passing run. `incomplete` mirrors `Report.Incomplete`,
`incomplete_reason` mirrors `Report.IncompleteReason` (empty when
`incomplete` is false), and `config_state` is one of `"loaded"`, `"absent"`,
or `"unreadable"` (§10.1's three states — `"absent"` is not an error).

### 9.1 ID stability and CI gating

An ID may be added and its severity lowered; renaming or repurposing one is a
breaking change.

`--strict` is explicitly **not** a stable CI gate: promoting every `warn` to
`fail` means any release adding one heuristic breaks every pinned pipeline. CI
gates on `--only <groups>` over the deterministic groups (`config`, `exposure`,
`tools`, `runtime`), or on specific IDs from `--json`. New checks land as `warn`
in their first release so a non-strict gate absorbs them.

## 10. Command surface

```
wraptool doctor [--config PATH] [--workspace DIR] [--json] [--strict]
                [--skip CHECK,...] [--only CHECK,...]
                [--no-probe] [--write-cache] [--verify-pool]
```

`--config` resolves as `config validate` does. `--workspace` defaults to an
`os.Stat` walk for `.git`/`manifest.scm` (§5); with no project root, the
workspace-dependent checks are `skipped`.

### 10.1 Exit codes

| Code | Meaning |
|---|---|
| 0 | No `fail` findings (`warn` may be present, unless `--strict`) |
| 1 | At least one `fail` finding |
| 2 | doctor could not complete — config unreadable or unparseable, or an internal error |

Exit 2 is distinct on purpose: "your setup is broken" and "I could not tell"
must not be one signal. A config failing strict decode produces `config.parse`
*and* exits 2, after emitting the full `env.*` group, which needs no config.

A **missing** config is not an error: post-install, before the first
`serve`/`up`, none exists (`config.EnsureConfigFile` scaffolds it on first run).
doctor reports `info: no config yet` and runs `env.*`, so `wraptool doctor` is
safe as the very first command a new user runs.

## 11. Implementation sketch

- `internal/doctor/` — engine and catalog. Takes a loaded config, the config's
  yaml node tree, and injected `LookPath`/`Stat`/`ReadFile`/probe/clock
  interfaces; returns outcomes and findings. No printing.
- `internal/doctor/toolknowledge.go` — the §7.8 table, data only.
- `cmd/doctor.go` — flags, engine invocation, rendering. Thin.
- Reuses: `config.LoadStrict`/`Validate`, `yamledit` primitives, `paths`,
  `introspect` (with §4.2's changes), `executor.ChildEnv`, `pool`'s lock-free
  accessors (`PoolPaths`, `ActiveGeneration`, `VerifyGenerationTree` are all
  exported and root-parameterized), `mcptools.IsBooleanFlag`.

The injected seams cover **doctor's own** checks. `config.LoadStrict`/`Validate`
stat token files and binaries directly (`internal/config/validate.go:49,57,62,
168,173`), and `CacheStamp` reads the cache file — those bypass injection and
are fixture-tested (§12).

## 12. Test plan

- Table-driven per check against injected `Stat`/`LookPath`/probe fakes.
- Checks whose dependencies touch the real filesystem (`config.Validate`,
  `pool.ActiveGeneration`, `CacheStamp`) are tested against a temp-dir fixture
  tree. The "no real filesystem" property is scoped to doctor's own logic.
- Golden human report and `--json` for a fixture with one finding of each
  severity plus a suppressed one.
- A **clean** fixture asserting an empty finding set — the guard against false
  positives creeping into `policy.*` and `drift.*`.
- Drift-specific, all mandatory: a failed help parse must yield
  `drift.unverifiable`, never `drift.unknown-flag`; a *garbage* parse
  (`git bogus -h`) likewise; a multi-level subcommand likewise; a tool with no
  catalog argv likewise; and `git config` against a `git -h` catalog must **not**
  produce a finding.
- Suppression: both comment placements of §6.2, asserted against real
  `yaml.v3` attachment behaviour rather than assumed.
- Exit-code matrix: clean, warn-only, warn-only-with-`--strict`, fail,
  unparseable, config-absent.
- Platform: every mode-bit and ownership check is `skipped` on Windows with a
  reason. `paths` supports Windows (`internal/paths/paths.go:23`) and there is a
  `docs/platform-windows.qmd`; POSIX bits there would produce spurious passes.
- Every catalog ID is asserted reachable by at least one test.

### 12.1 Drift calibration corpus

`testdata/` today holds 6 files covering two tools, and `git_log_help.txt` is
the 2943-line `--help` man page — while `getHelp` returns the terse `git log -h`
form first. Calibrating against the corpus would measure the rich form and ship
against the terse one. Before `drift.*` ships, capture `-h` output and catalog
output for every tool in the §7.8 table.

## 13. Not covered, and why

Stated explicitly so silence keeps meaning "checked, clean" (§6):

- **Credential scrubbing** — there is no config surface for it today, so there
  is nothing for doctor to check. If scrubbing patterns become configurable,
  they belong here.
- **Request-log integrity** — `requestlog` folds state over an append-only
  JSONL stream; a truncated or corrupt line is discovered at read time. doctor
  checks the log's permissions and writability, not its content.
- **Web UI auth surface beyond the token file** — session cookie and login-form
  behaviour is code, not configuration.
- **`introspection.cache_ttl`** — a very large value makes the cache
  effectively permanent, but the invalidation inputs (binary mtime, wraptool
  mtime, overrides mtime) still fire, so it is not obviously wrong.

## 14. Open questions

1. **`--fix` mode.** `config.ApplyRequest`/`AddRule` already do
   comment-preserving atomic edits, so mechanical fixes (`chmod` a token file,
   add a missing `deny_flags`) are within reach. Deferred: a fixer wrong about
   policy intent is worse than a report that is.
2. **Per-tool knowledge freshness.** In-tree only, or operator-overridable the
   way introspection overrides are?
3. **Web UI panel.** The engine returns findings rather than printing them
   partly so an admin-listener health panel stays possible.
4. **Coverage bar for flag drift.** The concrete fraction-of-flags-found
   threshold needs calibrating against §12.1's corpus.

## 15. Prior art

`brew doctor`, `flutter doctor`, `terraform validate`, `git fsck`. Borrowed: no
arguments, everything at once, actionable remedies, CI-usable exit code. The
divergence is `policy.*` and `drift.*` — those tools check their own
installation, whereas wraptool's most valuable checks are about the security
policy the operator wrote and whether it still matches reality.
