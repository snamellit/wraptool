# Design: tool descriptions come from `--help`, not config

Status: **decided — no change** (2026-07-10)
Scope: record why wraptool populates the MCP `description` field for CLI tools
purely from `--help` introspection, and deliberately does **not** add an
operator-authored `description` field to the config. No code change; this is a
decision record so the question isn't re-litigated.

## 1. Question

The MCP spec gives every tool a `description` the agent reads to decide when and
how to use it. Should the wraptool config let an operator author or override that
description per tool/subcommand — analogous to how a capability request carries
the agent's rationale for *why* it needs something?

## 2. What we already have

Tools are **not** description-less. `internal/mcptools/cli_source.go` builds each
exposed tool's `Description` from `--help` introspection —
`"<tool> <subcmd>: <parsed help text>"` — and flag descriptions come from the
same parse. When introspection is unavailable the fallback is the bare
`"<tool> <subcmd>"`. So descriptions exist, are accurate, and update themselves
as the underlying tool changes. In practice this works well with capable models.

## 3. Decision

Keep `--help` as the single source of truth for tool/flag descriptions. Do **not**
add an operator-authored `description` field to the config.

## 4. Rationale

- **Zero-configuration principle.** A tool becomes usable by adding an allow rule;
  it should not also require prose. `--help` already carries the tool's own,
  authoritative documentation — duplicating it in config is work with no gain.
- **Staleness.** A hand-written description drifts from the tool's real behavior
  as the tool evolves; the `--help` text does not. Config prose would be an
  unversioned second source that silently rots.
- **Don't prune exploration paths.** Terse, accurate descriptions let the model
  reason about the tool from its own knowledge and the flag schema. Over-specified
  operator prose ("use this for X") can *narrow* what the agent considers,
  discouraging valid uses we didn't anticipate. Less steering, more capability.
- **It empirically works without it.** The introspected descriptions plus the
  tool/flag names are sufficient today; there is no observed failure to fix.

## 5. Rejected alternative

An optional `description:` on the allow rule (augment/override the auto text).
Considered and declined for now. Its value is real but **narrow** — it concentrates
on cases `--help` can't cover:

- **Intent/policy `--help` can't express** — e.g. "this kubectl targets prod,
  read-only; mutations go through a change request."
- **Steering capability requests** — e.g. "`-n` is capped at 100; request a
  capability to raise it" sharpens the agent's request.
- **Cryptic or undocumented tools** — where `--help` is thin and the fallback
  string is bare.

For the common case (well-documented CLIs: git, kubectl, gcloud) it is redundant
and carries the staleness cost above. The value doesn't justify the config
surface, the maintenance, or the risk of pruning exploration.

## 6. If revisited

The escape hatch stays cheap to add later: an **optional, additive**
`description` on the allow rule that overrides/augments the introspected text when
present, and changes nothing when absent. Add it only if a specific tool is shown
to confuse the agent, or to encode environment/policy intent that `--help`
genuinely cannot — not as a blanket practice.
