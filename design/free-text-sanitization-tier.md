# Design: a free-text sanitization tier for flag values

Status: **proposal**
Scope: let an operator mark specific flags of a wrapped tool as carrying
**free text** (MR descriptions, issue bodies, commit messages, comments), so
their *values* accept prose — newlines, punctuation, Markdown — while every
other argument keeps today's strict sanitization. Extends the
`flag_constraints` type vocabulary from
[`flag-constraint-type-single-source.md`](flag-constraint-type-single-source.md);
changes nothing for configs that don't opt in.

## 1. Problem — evidence from MR !59

`executor.SanitizeArgs` rejects any argument containing a character from

```go
const shellMetachars = ";|&$`(){}\\<>\n\r"   // internal/executor/sanitize.go
```

applied uniformly to **every** argv token of **every** wrapped tool. This is
deliberate defense-in-depth: wraptool never invokes a shell
(`exec.CommandContext` with explicit argv), so these characters are inert in
wraptool's own execution — the ban guards against the *wrapped tool* or its
children re-interpreting them.

The cost surfaced the first time an agent shipped real work through the
policed path. Creating GitLab MR !59 (`glab mr create --description …`) took
four attempts, each rejected on a different character — `(`, then `\n`, then
`;` — and the surviving description is denatured prose: no Markdown structure,
no bullet list line breaks, clauses reworded to dodge punctuation. The same
wall blocks, today, for every configured forge/tracker tool:

- `glab mr create / mr note / issue create` — descriptions and comments;
- `acli jira workitem comment / create` — comment and description bodies;
- any future `tea pr create`, `git commit -m`, changelog or release-note flag.

These flags are *documented free text* on their tools. A sanitizer that
forbids newlines in an MR description isn't adding safety; it is making the
capability half-usable and teaching agents to strip structure from prose.

## 2. Threat model — what the strict tier actually protects

Naming the protections tells us which ones a text tier may relax and which it
must keep:

| Protection | Mechanism | Still needed for text? |
|---|---|---|
| Shell injection in wraptool | none needed — no shell, argv arrays | n/a (structural) |
| Wrapped tool shelling out with the value embedded (hooks, `$EDITOR`, `--exec`-style flags) | strict charset | **Operator judgment.** The text tier is opt-in *per flag*; marking `--description` asserts "this tool treats this value as data". Flags that feed sub-execution must never be marked text. |
| Option injection (`--description "-rf"` becoming a flag) | leading-`-` guard (`SanitizeFlagValues`) | **Yes — unchanged.** Orthogonal to charset. |
| Flag smuggling via positionals | positional leading-`-` guard | **Yes — text never applies to positionals** (§4.3). |
| Terminal/log injection at the operator's console | `\n\r` ban + control chars implicitly | **Partially.** Text allows `\n`/`\t` but must keep rejecting all other control characters, especially ESC (ANSI injection) and NUL. |

Conclusion: free text can safely relax the *punctuation* class
(`;|&$(){}\<>` + backtick + backslash) and permit `\n`/`\t`, provided the
option-injection guard, the positional rules, and a strict control-character
ban remain. The relaxation is a per-flag, operator-granted property — never a
tool-wide or global mode.

## 3. Design

### 3.1 A new constraint type: `text`

Extend the (single-source) constraint enum with one member:

```yaml
tools:
  glab:
    subcommands:
      - name: [mr, create]
        flags: ["--title", "--description", "--reviewer", "--yes", ...]
        flag_constraints:
          "--description": { type: text }
          "--title":       { type: text }
```

Semantics table entry (the §4.3 table of
`flag-constraint-type-single-source.md`):

| axis | value |
|---|---|
| JSONSchema (presentation to the AI) | `string` |
| Valueless | false |
| Value validation | `pattern` still applies if present |
| **Sanitization class** | **relaxed** (§3.2) — the new axis this proposal adds to the table |

Every existing type (`""`, `string`, `integer`, `boolean`) maps to
sanitization class **strict**, i.e. exactly today's behavior. The
sanitization class becomes part of the one semantics table both `mcptools`
and `whitelist` read — *not* a new hand-rolled switch; the `type: boolean`
incident (two consumers disagreeing about an unknown member) is the direct
cautionary tale, and the seam/contract test from that design must cover
`text` on day one.

### 3.2 The relaxed rule

For a value whose flag is `type: text`:

- **Reject** NUL and every control character **except** `\n` (LF) and `\t`.
  That includes ESC (`0x1b`, ANSI escape injection) and `\r` (CR) — reject
  CR rather than normalizing CRLF; reject-don't-rewrite is the sanitizer's
  established stance (env keys, mount paths), and the calling agent can
  trivially send LF.
- **Reject** values beginning with `-` — the existing `SanitizeFlagValues`
  option-injection guard runs unchanged, *before* the charset check.
- **Allow** everything else, including `;|&$(){}\<>`, backtick, backslash,
  quotes, and Unicode.
- **Cap length** at 64 KiB. Free text is the one tier where an agent could
  pump megabytes through an argument; the cap bounds argv size, audit-log
  entries (`Handler.logCall`), and downstream API payloads. Fixed constant
  now; a per-constraint `max_length` knob only if a real need appears.

Everything not marked `text` — all positionals, all other flag values, the
flag names themselves — keeps the strict rule, byte-for-byte unchanged.

### 3.3 Plumbing: the executor must learn which tokens are text

Today `executor` receives a finished `[]string` argv and sanitizes it
uniformly; the flag→constraint knowledge lives upstream where `buildArgs`
renders the call. Two possible seams:

- **(a) Sanitize before assembly** *(recommended)*: `whitelist`/`mcptools`
  validate each flag value against its constraint's sanitization class at the
  same place `pattern`/`integer` checks already run, then mark the executor
  request as "argument classes already enforced" per token — concretely,
  `executor` takes `[]Arg{Value string; Relaxed bool}` (or an index set)
  instead of bare `[]string`, and applies strict/relaxed per token.
- **(b) Re-derive in the executor**: pass the constraint map down and let the
  executor re-match tokens to flags. Rejected — it duplicates flag-matching
  logic in a second place, the exact drift shape this codebase keeps paying
  for.

With (a), the executor keeps a hard invariant as backstop: any token not
explicitly marked relaxed gets the strict rule, and a relaxed token still
gets the §3.2 control-character/NUL/length checks. A bug upstream can
therefore at worst *under*-relax, never over-relax.

### 3.4 Capability-request and discovery integration

- `wraptool_request_capability`'s `flag_types` map accepts `text` alongside
  `string`/`integer`/`boolean`; `config.ApplyRequest` writes it as a
  `flag_constraints` entry, same as today. An agent hitting the wall (as in
  MR !59) can request precisely: *tool `glab`, subcommand `mr create`, flag
  `--description`, type `text`* — and the operator approves a scoped,
  reviewable relaxation rather than a policy hole.
- `wraptool_discover` and the generated MCP schema surface nothing new: a
  `text` flag renders as a plain string parameter. (Optionally the flag's
  description gains a "free text; newlines allowed" hint so agents stop
  pre-flattening prose.)
- The Web UI rule editor lists `text` in the type dropdown; the §2 table's
  "flags that feed sub-execution must never be text" warning belongs in that
  UI text and in the docs recipe.

### 3.5 Explicit non-goals

- **No global or per-tool relaxation.** The unit of trust is one flag on one
  subcommand of one tool, granted in config by the operator.
- **No escaping/rewriting.** Values are accepted or rejected as sent
  (consistent with env-key and mount-path handling in the pool backends).
- **No relaxation for positionals.** A tool whose free text arrives
  positionally (e.g. `git commit -m` is fine — `-m` is a flag — but
  `somecli comment <body>`) is out of scope until a concrete need exists;
  the flag-scoped design deliberately cannot express it.
- **Output is unaffected.** Credential scrubbing of tool output is a separate
  layer and unchanged.

## 4. Security review checklist (for the implementing MR)

- [ ] Strict tier byte-identical for non-text flags, flag names, positionals.
- [ ] Relaxed tier rejects: NUL, ESC, CR, all control chars except LF/TAB,
      leading `-`, over-length.
- [ ] Executor backstop: unmarked tokens strict even if upstream forgets;
      marked tokens still control-char-checked.
- [ ] Seam/contract test covers `text` across config decode, mcptools
      rendering, whitelist enforcement, and executor sanitization — one test
      that imports all layers (the `type: boolean` lesson).
- [ ] `deny` rules still evaluated before any of this — a denied subcommand
      never reaches sanitization.
- [ ] Audit log entries with 64 KiB values remain parseable (truncate in the
      log record if needed, marked as truncated).
- [ ] Docs: forge-CLI recipe shows `text` on exactly `--title`,
      `--description`, `--message`-class flags, with the sub-execution
      warning.

## 5. Migration

1. Land the constraint-type single-source refactor first (or together) — the
   enum + semantics table is where `text` must live; adding it to three
   hand-rolled switches would recreate the drift bug on day one.
2. Add `text` + relaxed sanitizer + executor plumbing (§3.2–3.3), with the
   checklist tests.
3. Extend `flag_types` acceptance in the capability-request path and the Web
   UI dropdown.
4. Ship the docs recipe for `glab`/`acli`/`tea` free-text flags; update the
   operator's config for this repo so the next MR description ships as real
   Markdown.

## 6. Result

An agent creating an MR writes a normal multi-paragraph Markdown description;
the operator granted that capability one flag at a time and can read exactly
what was granted in the config; nothing else about wraptool's argument
handling changed. The MR !59 experience — four rejections and a flattened
description — becomes a one-time capability request instead of a permanent
tax on every free-text interaction.
