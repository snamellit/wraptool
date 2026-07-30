# Security Audit: policy-enforcement perimeter & config fail-open surface

Status: **findings — open**
Date: 2026-07-08
Scope: whether `config.yaml` is the single, sufficient control point for what an
AI harness can do through wraptool — i.e. (a) can any subprocess be reached with
AI-supplied arguments *without* a whitelist decision, and (b) can a malformed or
adversarial config silently *weaken* the policy (fail-open) rather than
fail-closed. Motivation: in a supply-chain-hostile world we want auditing
`config.yaml` to have maximal effect, which requires there to be no escape
hatches around it.

Method: manual read of the policy engine + executor + handler, plus two
read-only sweeps — (1) every `exec.*`/`.Run()`/`.Output()` site and its gating
status, (2) config decode/validate/reload fail-open analysis.

---

## Perimeter verdict

`config.yaml` **is** the single control point for AI-driven commands. Every
AI-reachable command flows `Handler.Handle → Policy.Evaluate → executor.Run`
(`internal/mcptools/handler.go:68,82`); there is no shell (`exec.CommandContext`
with an argv array, `internal/executor/executor.go:88`); the AI cannot mutate
policy (`wraptool_request_capability` only *appends an audit record* —
`internal/mcptools/request_tool.go`; approval is a host-side operator action via
CLI or the 127.0.0.1 web UI); and no AI tool can trigger a config reload.

Two code regions run subprocesses **without** a policy check, but in both the
argv is **config-derived, never AI-supplied**, and both run at registration/reload
time, not per AI call:

- **Introspection** (`internal/introspect/introspect.go:116-117`) executes
  `<allowed-subcommand> -h|--help|help|--version` to build tool schemas. Args are
  `rule.Subcommand` from the config allow-set; no shell; no injection surface.
- **Upstream MCP child spawn** (`internal/mcpproxy/client.go:187`) launches a
  configured stdio MCP server; command/args come from `MCPServerConfig`.

The engine is **fail-closed where it counts** (see inventory below). The findings
are escape hatches *around* the policy that blunt an audit, not holes in the
allow/deny logic itself.

---

## Findings

| # | Severity | Finding | Status | Evidence |
|---|---|---|---|---|
| F1 | 🔴 High | Typo'd restrictive keys are silently dropped (lenient decode); serve & reload fail-open | ✅ fixed `1adb80b` | `config.go:201`, `reload.go:84`, `validate.go` |
| F2 | 🔴 High | Option injection via unchecked flag *values* (esp. valueless flags) | ✅ fixed `1adb80b` | `sanitize.go:35-45`, `executor.go:132-137`, `constraints.go:50-53`, `handler.go:53-64` |
| F5 | 🔴 High | Client-supplied `cwd` is used unvalidated → allowed commands run in an attacker-chosen dir (git repo-local config/hook execution) | ✅ fixed `32903be` | `context.go:31-32`, `executor.go:95-98` |
| F3 | 🟠 Medium | Positional args have no *content* constraint, only a max count | ⬜ open | `policy.go:98` |
| F6 | 🟠 Medium | Flag-value regex patterns are unanchored (substring match) — constraints weaker than authored | ✅ fixed `1adb80b` | `constraints.go:69` |
| F4 | 🟡 Low | Deny matches exact subcommand length, not prefix | ⬜ open | `policy.go:119-129` |
| M1 | 🟡 Low | Upstream MCP child is spawned even with zero allowed tools | ⬜ open | `mcpproxy/source.go:61-73,83` |
| M2 | 🟡 Low | Introspection runs `--help`/`--version` unsanitized and outside flag-level policy | ⬜ open | `introspect.go:72-99` |

> Validation pass (2026-07-08): F1–F4 re-verified against code, cites confirmed.
> F5 and F6 were found during that review and added below.
>
> Remediation (2026-07-08): F1, F2, F5, F6 fixed on branch
> `security-policy-hardening` (commits `1adb80b`, `32903be`); build/vet/test
> green. F3, F4, M1, M2 remain open. The evidence line-cites above describe the
> *pre-fix* code — see the commits for the fixed state.

### F1 — Typo'd security keys are silently dropped (FAIL-OPEN) 🔴

The runtime load is **lenient** — `yaml.NewDecoder(...).KnownFields(false)`
(`internal/config/config.go:201`, via `Load → load(path, false)`) — and
`Validate()` does not check for unknown keys. Misspelling a *restrictive* key
therefore does not error; it silently vanishes and `serve` still starts:

- `dney:` for `deny:` → the deny rule is gone; a surviving allow rule now permits
  what it was meant to block. Passes `Validate()` (no rule requires a deny).
- `deny_flags:`, `flag_constraints:`, `arg_constraints:` misspelled → the
  flag/positional becomes **unconstrained**, silently.

Asymmetry: a typo in `allow:` fails *closed* (`validateTool` requires ≥1 allow
rule — `validate.go:150-152`), but a typo in any deny/constraint key fails *open*.

`wraptool config validate` *would* catch these — it alone uses `LoadStrict`
(`KnownFields(true)`, `cmd/validate.go:65`) — but it is **manual and advisory**;
nothing forces it before `serve`. The **hot-reload path is also lenient**
(`cmd/reload.go:84`), so a live edit can replace the good policy with a weakened
one and log `config reloaded successfully`.

This is the finding that most undermines "auditing `config.yaml` has the most
effect": a single typo silently defeats the audit.

**Fix:** make `serve` and reload use `LoadStrict` (or at minimum emit a loud
warning on unknown keys). Small, high-leverage.

### F2 — Option injection via unchecked flag values 🔴

Positional args are well-defended: placed after a `--` separator
(`executor.go:139-141`) **and** rejected if they start with `-`
(`sanitize.go:40`). Flag **values** get neither protection. `buildArgs` emits each
flag as `[flag, value]` (`executor.go:132-137`); the only filter on `value` is the
shell-metachar denylist (`sanitize.go:11`), which does **not** include `-`, `=`,
or `/`.

Compounding factors:
- `handler.go:53-64` accepts a **string for any parameter** — a boolean/valueless
  flag advertised as `boolean` in the schema can still be handed a string by a
  malicious client (which is the threat model). No server-side type enforcement.
- `validateValue` **skips value checks for valueless flags** (`constraints.go:50-53`),
  and `checkConstraints` skips any flag with no constraint entry at all
  (`constraints.go:28-30`).

Result:

> An allowlisted boolean flag (e.g. `--verbose` on `git clone`) fed the value
> `--upload-pack=/tmp/x` → argv becomes `git clone --verbose --upload-pack=/tmp/x …`
> → arbitrary local exec. The injected token is never checked for being
> option-like.

No shell is involved, so this is argument-semantic (tool-interprets-value-as-flag)
risk, not command injection — but it is a real bypass of the flag allowlist for
any tool that reparses the token as an option. Severity depends partly on whether
the MCP transport validates arg types upstream; relying on that for a security
boundary is the fragility to avoid.

**Fix:** in the executor, reject flag values beginning with `-` (mirror the
positional check), and/or drop the value entirely for known-valueless flags.
Defense-in-depth, independent of the transport library.

### F5 — Client-supplied `cwd` is unvalidated 🔴

`ResolveCWD` returns the client's `?cwd=` query param **verbatim**, with no
allowlist or containment check (`internal/paths/context.go:31-32`), and the
executor sets it as the child's working directory (`executor.go:95-98`). Because
the server runs host-side as the developer's user (full filesystem access), a
malicious harness can run any *allowed* command in **any host directory** —
`/root`, another project, anywhere.

The sharp exploitation is git. git honours **repo-local** `.git/config` for the
repo it runs in (`safe.directory` does not help — the server and the workspace
are the same host user), and several config keys execute programs:
`core.fsmonitor`, `core.pager`, `core.sshCommand`, plus `.git/hooks`. The shared
workspace is writable (rw mount), so an attacker plants a repo containing a
malicious `.git/config`, sets `cwd` to it, and issues a fully-allowed `git`
command → arbitrary code execution on the host, entirely inside policy. The class
generalises to any tool that reads cwd-relative config or plugins.

Note this partially overlaps `design/git-hardening.md` (global-config lockdown);
the wraptool-side amplifier is that `cwd` itself is attacker-chosen and
unconstrained.

**Fix:** constrain `cwd` to an allowlisted root (e.g. the configured workspace /
worktree), reject paths outside it, and reject symlink escapes. Pair with the git
hardening for repo-local config.

### F3 — Positional args: content unconstrained, only count 🟠

`ArgConstraints` carries only `PositionalMax` (`policy.go:98`); there is no pattern
option for positional *values*, and if `arg_constraints:` is absent (or typo'd
away per F1) the count is unbounded too. So where the dangerous part is a
positional — `kubectl exec POD -- cat /etc/secret`, resource names, refs — the
policy can restrict *how many* but not *what*. A genuine least-privilege ceiling,
not just a config mistake.

**Fix:** add an optional per-positional pattern/allowlist to `ArgConstraints`.

### F6 — Flag-value regex patterns are unanchored 🟠

`validateValue` matches with `regexp.MatchString(c.Pattern, value)`
(`constraints.go:69`), which succeeds if the pattern occurs **anywhere** in the
value. An operator writing `pattern: "main"` to permit the value `main` actually
permits `main-evil`, `xmainx`, `release/main`, etc. — a substring allowlist, not
an exact one. This silently weakens every value constraint, and it is the exact
mechanism the F2 remediation and self-audit checklist item 1 rely on to bound
dangerous flag values.

**Fix:** anchor patterns automatically (compile as `^(?:` + pattern + `)$`), or
validate at load time that each pattern is fully anchored and reject those that
aren't. Auto-anchoring is safer — it can't be forgotten per-rule.

### F4 — Deny matches exact length, not prefix 🟡

`subcommandMatches` requires `len(rule) == len(actual)` (`policy.go:128`). So
`deny: [remote]` does **not** block `remote add`; a deny authored at one
granularity silently won't cover deeper subcommands. Easy to write a deny that
doesn't do what the author thinks.

**Fix:** prefix-match denies (deny `[remote]` ⇒ blocks all `remote …`), or
document the exact-length semantics prominently in config docs.

### M1 — Upstream MCP child spawned with zero allowed tools 🟡

`Source.Tools()` (`internal/mcpproxy/source.go:61-73`) spawns the configured
upstream MCP server before the per-tool policy filter at `source.go:83`. An
upstream server whose tools are all denied is still launched. Args are
config-derived (not AI-influenced), so this is a hardening nicety, not a bypass.

**Fix:** skip the spawn when no tool of that server survives the policy filter.

### M2 — Introspection unsanitized & outside flag-level policy 🟡

Introspection runs `<subcommand> --help/-h/help/--version` (`introspect.go:72-99`)
without calling `SanitizeArgs`, and can invoke `--help` on an allowed subcommand
even when `--help` is not an allowed flag. Args are trusted config + no shell, so
harmless today — noted for strict defense-in-depth.

---

## Fail-closed inventory (verified correct — do not regress)

- **Unknown tool / no matching allow / empty allow list → deny** (`policy.go:40-46,110-114`; MCP mirror `mcp_policy.go`).
- **Deny before allow**, deny returns immediately on match (`policy.go:48-56`).
- **Constraint & regex parse errors → deny**, not allow (`constraints.go:55-75`, surfaced at `policy.go:89-95`). An invalid `pattern` denies rather than opens.
- **Empty / missing config → refuse to start** (`validate.go:46-48`, "no tools configured").
- **Reload keeps the old policy** on any parse or validate failure (`reload.go:82-100`); atomic swap under lock (`policy.go:29-33`), no empty-policy window.
- **No shell** anywhere on the AI path; `exec.CommandContext` with argv arrays.
- **AI cannot self-approve** capabilities or write/reload config (`request_tool.go` appends an audit record only).
- **Introspection is server-only**, config-bounded, `--help`-inert.

---

## Policy-content self-audit checklist (for the host config, not readable here)

Because `config.yaml` is host-isolated from the harness by design, audit it
host-side against:

1. **Every value-taking flag that can execute or redirect must carry a `pattern`
   constraint** — git `--upload-pack`/`--receive-pack`/`--exec`/`-c`/`--config`/`-o`;
   kubectl `--kubeconfig`/`--token`/`--server`/`--as`; gcloud
   `--impersonate-service-account`/`--flags-file`/`--configuration`; any `--*-file`.
   An allowlisted value flag with no pattern = free-form value (see F2). Also
   ensure each `pattern` is anchored (`^…$`) — unanchored patterns are substring
   matches (see F6).
2. **Mutating subcommands** — prefer deny or capability-gating: `kubectl
   delete/apply/edit/patch/replace/scale/drain/exec/cp/port-forward`, `gcloud …
   delete/set-iam-policy/add-iam-policy-binding`, `terraform apply/destroy/import`,
   `git push --force*` (as `deny_flags`).
3. **Treat `kubectl exec`/`cp`/`port-forward`/`proxy` as shell-equivalent** — deny
   unless truly required (and note F3: their positional payloads aren't
   content-constrained).
4. **Run `wraptool config validate` as a mandatory pre-flight** until F1 is fixed —
   it is currently the only defense against the silent-typo class.

---

## Proposed remediation order

1. ~~**F1** — `serve` + reload use `LoadStrict`.~~ ✅ done (`1adb80b`) — chose hard-fail over warn-only; lenient `Load` kept for read-only consumers.
2. ~~**F2** — executor rejects option-like (`-`-leading) flag values.~~ ✅ done (`1adb80b`) — `SanitizeFlagValues`.
3. ~~**F6** — auto-anchor constraint patterns.~~ ✅ done (`1adb80b`) — `^(?:…)$` in `validateValue`.
4. ~~**F5** — contain `cwd` to allowed roots; reject escapes.~~ ✅ done (`32903be`) — `mcp.allowed_cwd_roots`, warn-only when unset (non-breaking).
5. **F3** — optional per-positional pattern in `ArgConstraints`. Larger; enables real least-privilege on positionals. *(open)*
6. **F4 / M1 / M2** — prefix-deny (or doc), skip zero-tool spawns, sanitize introspection args. *(open)*
