# Design: flag value types come from the capability author, not a regex

Status: **implemented** (parser default + request-supplied types); Web UI type
field is a follow-up — sequenced in `design/webui-management-completion.md`.
Scope: fix how a wrapped CLI flag's value type (`string` / `integer` /
`boolean`) is determined, so a value-taking flag is never silently rendered as
an uncarriable boolean. Two changes: a safer introspection default, and a path
for the intelligent capability author (developer, or the AI) to supply the
authoritative type.

## 1. Problem

Flag types are guessed by parsing `--help` output (`internal/introspect`). The
parsers are purely **syntactic**: they look for a value placeholder token
(`--data <data>`, `-n NUM`) next to the flag. No placeholder → the flag was
typed **`boolean`**.

That breaks on CLIs whose help renderer omits placeholders entirely. `glab mr
update --help` is the motivating case — *every* flag renders as bare
`--flag   description`, so a value flag is indistinguishable from a switch:

```
    --draft                 Mark merge request as a draft.
    --reviewer              Request review from users by their usernames. ...
```

Both parse identically → both typed `boolean`. The consequence is asymmetric and
severe: **a value flag mistyped `boolean` is uncarriable** — the MCP tool schema
exposes a bare switch, so there is no way to pass the value (`glab_mr_update`
could not set a reviewer, target-branch, milestone, …). A switch mistyped the
other way is merely cosmetic.

The information to type `--reviewer` correctly *is present* — in the
**description** ("by their usernames", "comma-separated", "repeating the flag").
A regex throws it away; any reader (human or LLM) types it instantly.

## 2. Change A — a safer, placeholder-aware default

The parser's fallback is flipped from "assert `boolean`" to "admit **unknown**":

- When the parsed help shows **no value placeholder on any flag**, the CLI's
  renderer gives no type signal, so valueless flags are marked **unknown**
  (`Flag.Type == ""`) rather than `boolean`
  (`introspect.demoteBooleansIfNoPlaceholders`, applied by `BestParse` to the
  **winning** parser's result — which parser wins is data-dependent, e.g. the git
  parser also handles cobra-style help and wins for glab, so demoting per-parser
  silently missed it). The schema builder then defaults
  an unknown flag to **string** (`mcptools.buildCLITools` → `IsBooleanFlag`
  heuristic, whose default is string) — the recoverable direction: a switch
  rendered as a string param still works as a bare flag (`buildArgs` emits just
  `--flag` for an empty value), while a value flag stays carriable.
- When the help **does** use placeholders somewhere (curl, git), the absence of
  one is a reliable `boolean`, so those are left exactly as before. No regression
  for well-formed CLIs.
- The `wellKnownBooleanFlags` allowlist (`--yes/--force/--dry-run/--quiet/
  --verbose/--help/-h`) still names genuine switches, so common booleans stay
  boolean even under the unknown default.

This is a **default**, not the answer — the parser is a dumb bootstrap.

## 3. Change B — the author supplies the authoritative type

Capabilities are not born from the parser. They are authored by an intelligent,
accountable actor who has read (or can read) the help:

- **Developer** — edits `flag_constraints` in the config (or, later, the Web UI
  rule editor). Trusted, authoritative.
- **AI in the container** — calls `wraptool_request_capability`. It already
  reasons about which flags it needs; it now also passes an optional
  `flag_types` map (`{"--reviewer": "string"}`), inferred from the tool's
  `--help`. On approval, `config.ApplyRequest` writes these as `flag_constraints`
  on the allow rule. Operator-gated: the AI *proposes* a type, the human ratifies
  it on `requests approve --apply`.

Precedence already favours this — `mcptools.buildCLITools` resolves a flag's type
as **explicit `flag_constraint` > introspection > name heuristic**. So an
author-supplied constraint overrides whatever the parser guessed. `applyFlagTypes`
never clobbers an existing constraint (operator authority wins over a later
request), and an invalid type is rejected by the config's closed
`ConstraintType` enum on the round-trip validation, so a bad request can't
corrupt the policy.

### The self-correcting loop

Once a help-enabled server build is deployed (`-h`/`--help` seeded into every
allow rule, `SeedHelpFlags`), the two changes compose:

> AI calls `glab_mr_update`, sees `reviewer` is boolean → calls `glab mr update
> --help` (now permitted) → reads "…usernames" → requests the capability with
> `--reviewer: string` + rationale → operator approves → `flag_constraints`
> written → schema updates → AI sets the reviewer.

## 4. Why this shape

A regex was never going to out-reason two intelligences already in the loop. The
parser should be the cheap default; the curator supplies truth. Modelling the
type as a **policy decision made at authoring time** (and gated by the existing
request/approve machinery) keeps it accurate *and* accountable, and reuses
`flag_constraints` — the single source of truth for flag types
(`design/flag-constraint-type-single-source.md`) — rather than inventing a new
channel.
