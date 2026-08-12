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
| F5 | 🔴 High | Client-supplied `cwd` is used unvalidated → allowed commands run in an attacker-chosen dir (git repo-local config/hook execution) | 🟨 partially fixed `32903be`; default-open and TOCTOU remain | `context.go:31-32`, `executor.go:95-98` |
| F3 | 🟠 Medium | Positional args have no *content* constraint, only a max count | ⬜ open | `policy.go:98` |
| F6 | 🟠 Medium | Flag-value regex patterns are unanchored (substring match) — constraints weaker than authored | ✅ fixed `1adb80b` | `constraints.go:69` |
| F4 | 🟡 Low | Deny matches exact subcommand length, not prefix | ⬜ open | `policy.go:119-129` |
| M1 | 🟡 Low | Upstream MCP child is spawned even with zero allowed tools | ⬜ open | `mcpproxy/source.go:61-73,83` |
| M2 | 🟡 Low | Introspection runs `--help`/`--version` unsanitized and outside flag-level policy | ⬜ open | `introspect.go:72-99` |

> Validation pass (2026-07-08): F1–F4 re-verified against code, cites confirmed.
> F5 and F6 were found during that review and added below.
>
> Remediation (2026-07-08): F1, F2, and F6 fixed; F5 phase 1 added optional
> containment on branch `security-policy-hardening` (commits `1adb80b`,
> `32903be`); build/vet/test green. F3, F4, F5 default-open/token-scope/TOCTOU,
> M1, and M2 remain open. The evidence line-cites above describe the *pre-fix*
> code — see the commits for the fixed state.

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

## Policy-content self-audit checklist — now automated by `wraptool doctor`

This section used to tell the operator to audit `config.yaml` by hand,
grepping for the four shapes below. It predates `wraptool doctor`
(`design/doctor/wraptool-doctor.md`), whose `config.*` and `policy.*` check
groups (§7.2, §7.7; implementation `internal/doctor`) now run these checks
for you. Run `wraptool doctor` — or `wraptool doctor --only config,policy`
to scope it to this section's concerns — instead of a manual pass. What
follows maps each original item to the check(s) that now cover it and says
what, if anything, still needs a human.

1. **Every value-taking flag that can execute or redirect must carry a
   `pattern` constraint.** Two checks split this:
   - `policy.unpatterned-value-flag` (warn) fires on an allowed flag whose
     `flag_constraints` entry CONFIRMS a value-taking type
     (`type: string`) and has no `pattern`. It deliberately does **not**
     fire on an untyped flag: `internal/whitelist` has no notion of
     "value-taking" independent of the constraint type, and most flags in a
     real config are untyped (`flag_constraints` is written only when a
     capability request supplied `flag_types`) — treating "untyped" as
     "value-taking" produced nine false positives against a plausible
     `git status/log/diff` policy during implementation (bare switches like
     `--porcelain`, `--oneline`) and was reverted (`internal/doctor/policy_flags.go`).
     So an unconstrained, untyped value flag is still a manual review item.
   - `policy.dangerous-flag` (warn) fires on an allowed flag that is on its
     tool's own execute-or-redirect list, or matches the generic
     `--*-file`/`--*-cmd` shape, with no `pattern` — on any constraint type,
     not just string. The per-tool lists are git
     `--upload-pack`/`--receive-pack`/`--exec`/`-c`/`--config`; kubectl
     `--kubeconfig`/`--token`/`--server`/`--as`; gcloud
     `--impersonate-service-account`/`--flags-file`/`--configuration`
     (`internal/doctor/toolknowledge.go`, matching
     `design/doctor/wraptool-doctor.md` §7.7). **Correction to this item's
     git list:** an earlier draft of this checklist also named `-o`. git has
     no top-level `-o` flag at all; per-subcommand it means things far from
     "execute or redirect" (`clone -o <name>` names a remote,
     `format-patch -o <dir>` names an output directory) or, at most,
     `push -o`/`--push-option`, which hands an opaque string to a
     *server-side* hook rather than causing the local git process to
     execute or read anything — not the local exec/redirect shape this item
     is about, and not part of the F5 repo-local-config exec vector either
     (`core.fsmonitor`/`core.pager`/`core.sshCommand`/hooks, none of which
     `-o` touches). `internal/doctor/toolknowledge.go` and
     `design/doctor/wraptool-doctor.md` §7.7 both omit it; this note is now
     amended to agree with the code rather than the other way around.
   - Manual residue beyond the untyped-flag gap above: the per-tool list
     only covers git/kubectl/gcloud/terraform
     (`internal/doctor/toolknowledge.go`'s `toolKnowledgeTable`) — a tool
     with no entry (e.g. `glab`) gets the generic `--*-file`/`--*-cmd` check
     only, never a per-tool one, and doctor never guesses one.
   - The **anchoring** half of this item is now moot, not manual: patterns
     are auto-anchored (`^(?:…)$`) since F6, so an unanchored pattern can no
     longer under-match. What replaces it is a pattern that's anchored but
     constrains nothing (`.*`, `.+`, `.*?`, `(?s).*`) — that's
     `policy.overbroad-pattern` (warn), a distinct check. An **empty**
     pattern is deliberately not one of them: `FlagConstraint.Pattern`'s zero
     value cannot distinguish `pattern: ""` from no `pattern` key, so
     counting it fired on every constraint that simply has none — including
     `docs/getting-started.qmd`'s own `{type: integer, max: 100}`, whose
     value `validateValue` really does range-check. The no-pattern case
     belongs to `policy.unpatterned-value-flag`. `design/doctor/wraptool-doctor.md`
     §7.7's row still lists the empty pattern and is stale on this point; the
     code is authoritative.
2. **Mutating subcommands.** `policy.mutating-subcommand` (warn) fires when
   an allowed subcommand's *last* segment is one of a tool's mutating verbs:
   kubectl `delete/apply/edit/patch/replace/scale/drain`, gcloud
   `delete/set-iam-policy/add-iam-policy-binding`, terraform
   `apply/destroy/import`. `policy.force-push-allowed` (warn) fires when
   `git push` is allowed with no `deny_flags` covering
   `--force`/`--force-with-lease`/`-f`. Note kubectl's `exec`/`cp`/
   `port-forward` — lumped in with "mutating" by this item's original
   wording — are covered by item 3's check instead, not this one (they
   aren't in kubectl's `MutatingVerbs` set). Manual residue: both checks
   only *warn*, they don't deny or gate anything, and the verb tables are
   scoped to git/kubectl/gcloud/terraform — any other mutating tool/verb is
   still a human's job.
3. **Treat `kubectl exec`/`cp`/`port-forward`/`proxy` as shell-equivalent.**
   `policy.shell-equivalent-subcommand` (warn) covers exactly this list, plus
   `gcloud compute ssh` (not in this item's original wording, but in the
   design catalog and the same tool-knowledge table). Manual residue: same
   table-scoping limit as item 2, and F3 remains open — a flagged
   subcommand's positional payload is still not content-constrained, only
   count-bounded, so a `kubectl exec` allow rule still needs a human to
   judge what it can actually run even after doctor flags it.
4. ~~Run `wraptool config validate` as a mandatory pre-flight until F1 is
   fixed.~~ Superseded, not merely automated: F1 itself is fixed (`serve` and
   reload now hard-fail on `LoadStrict`, see "Proposed remediation order"
   below), so a typo'd restrictive key no longer runs silently even without
   this checklist. `wraptool doctor` additionally runs the same
   `config.LoadStrict` / `Config.Validate()` pair itself, as `config.parse`
   and `config.validate` (both `fail` severity, `internal/doctor/config_load.go`)
   — an explicit, scriptable preflight (`wraptool doctor --only config.parse`)
   with the same JSON/CI-gating story as every other check (§9.1 there).
   Manual residue: `Config.Validate` still stops at its first error
   (documented gap, both there and here), so a config with several problems
   surfaces them one fix at a time.

---

## Proposed remediation order

1. ~~**F1** — `serve` + reload use `LoadStrict`.~~ ✅ done (`1adb80b`) — chose hard-fail over warn-only; lenient `Load` kept for read-only consumers.
2. ~~**F2** — executor rejects option-like (`-`-leading) flag values.~~ ✅ done (`1adb80b`) — `SanitizeFlagValues`.
3. ~~**F6** — auto-anchor constraint patterns.~~ ✅ done (`1adb80b`) — `^(?:…)$` in `validateValue`.
4. ~~**F5 phase 1** — contain `cwd` to allowed roots; reject escapes.~~ ✅ done (`32903be`) — `mcp.allowed_cwd_roots`, warn-only when unset (non-breaking). Default-open behavior, server-wide token scope, and path TOCTOU remain open as M5 in `kimi-audit.md`; proposed replacement is server-issued project grants in `project-isolation.md`.
5. **F3** — optional per-positional pattern in `ArgConstraints`. Larger; enables real least-privilege on positionals. *(open)*
6. **F4 / M1 / M2** — prefix-deny (or doc), skip zero-tool spawns, sanitize introspection args. *(open)*
