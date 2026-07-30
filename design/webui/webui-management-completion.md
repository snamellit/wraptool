# Design: complete the operator Web UI for flag typing and policy management

Status: **planned**. Backend for request-supplied flag types is implemented
(`design/policy/flag-type-inference.md`); the Web UI does not yet surface, edit, or
fully manage those types. This note sequences the remaining work as four
independent, individually-shippable stages.

Scope: close the loop between what the AI *proposes*
(`wraptool_request_capability` → `flag_types`) and what the operator can *see,
correct, and author* in the Web UI (`internal/webui`), then round out the UI so
policy management no longer forces a drop to hand-edited YAML.

## 0. Where we are

The `flag_types` value flows end to end at the data layer but dead-ends at the
templates:

| Layer | Where | State |
|-------|-------|-------|
| MCP request tool accepts `flag_types` | `internal/mcptools/request_tool.go` | done |
| Event carries `FlagTypes map[string]string` | `internal/requestlog/types.go` | done |
| `ApplyRequest` writes `flag_constraints` (never clobbers) | `internal/config/edit.go` (`applyFlagTypes`) | done |
| Closed `ConstraintType` enum validates on load | `internal/config/config.go` | done |
| Request review **shows** proposed types | `internal/webui/templates/list.html` | **missing** |
| Operator **overrides** types before apply | `webui.go` `handleApprove` + template | **missing** |
| Rules editor authors `flag_constraints` inline | `internal/webui/templates/rules.html` | **missing** (punts to YAML) |
| UI covers whole-config management | `internal/webui` | **partial** (requests + basic allow/deny rules only) |

`requestView` already embeds the full `CapabilityRequest` (so `.FlagTypes` is in
scope in the template); `ruleView.Advanced` already flags rules that have
constraints but sends the operator to the YAML. The work is UI + a small amount
of handler/config plumbing, not new data modelling.

## Stage 1 — show proposed types in request review (read-only)

Operator approving a capability sees the flags but not the types the AI
proposed, so `apply to config` writes constraints the operator never saw.

- `list.html`: render `.FlagTypes` in each pending card — e.g. a small table
  `--reviewer: string`, `--draft: boolean`. Only when non-empty.
- No handler change: data is already on `requestView`. Template loops
  `{{range $flag, $type := .FlagTypes}}`.
- Test: `webui` render test asserts the type mapping appears for a request that
  carries `FlagTypes`, and is absent otherwise.

Smallest chunk; removes the "approve blind" gap. Independently shippable.

## Stage 2 — operator overrides types before apply

The AI *proposes*; the human should be able to *correct* before ratifying,
without a post-approval YAML edit. Design note §3 already frames the type as an
operator-gated policy decision — this is that gate in the UI.

- `list.html`: in the pending card's approve form, render one `<select>` per
  proposed flag (`""`/`string`/`integer`/`boolean`) pre-filled from
  `.FlagTypes`. Options mirror the closed `ConstraintType` enum.
- `handleApprove`: read the submitted per-flag types, merge over `req.FlagTypes`,
  pass the result into `config.ApplyRequest`. Reuse the existing
  `flag_types`→`flag_constraints` path; no new config API.
- Unknown/invalid types can't slip through — the config's `ConstraintType`
  round-trip validation already rejects them on write. Surface the error via the
  existing flash.
- Test: submitting an override changes the written constraint; an omitted select
  falls back to the proposed type.

## Stage 3 — author `flag_constraints` in the rules editor

Today a rule with constraints is tagged `adv` and the operator is told to edit
YAML. This is the developer-authoring path from design note §3 ("or, later, the
Web UI rule editor").

- `rules.html`: for each allow rule, an expandable per-flag constraint editor —
  type `<select>`, optional `min`/`max` (integer), `pattern`, `required`,
  `deny`. Mirrors `config.FlagConstraint`.
- New config edit surface: extend the `rules.go` node-tree editors (or add
  `SetFlagConstraint`/`DeleteFlagConstraint`) so a constraint can be written
  through the same comment-preserving, round-trip-validated, atomic-write path as
  `AddRule`/`UpdateRule`. Keep optimistic concurrency (`expect`).
- Drop the "edit YAML directly" punt for the covered fields; `arg_constraints`
  may remain YAML-only until Stage 4.
- Test: set/clear a constraint via the handler; assert YAML round-trips and
  comments survive.

## Stage 4 — new management tools: make the UI feature-complete

Status: **done**. After Stages 1–3 the UI reviews requests and authors
allow/deny rules with flag constraints, but full policy management still
required YAML for several things. This stage added the missing management
surfaces so the operator never has to leave the UI. Each is an independent
tool/page sharing the existing `editConfig` node-tree editor
(comment-preserving, round-trip-validated, atomic write, hot-reload):

- **Whole-tool CRUD** ✅ — add/remove a wrapped tool (name + binary) from the
  UI. Also relaxed validation so a rule-less tool is a valid (inert) state,
  enabling a create-then-add-rules bootstrap.
- **`arg_constraints` editor** ✅ — positional-args limit; retired the `adv` tag.
- **`deny_flags` editor** ✅ — per-rule denied flags, edited in the rule form.
- **Revoke an approved capability** ✅ — rule/tool deletes append a
  `capability_revoked` audit event (loud on failure), shown on the Calls page.
- **Environment / tool metadata** ✅ — working_dir, timeout, and **env_files
  paths only**. Inline `env` *values* are deliberately **not** editable: secrets
  belong in a per-folder `.env`/`.envrc` referenced by `env_files`, so the
  shared config never becomes a plaintext secret store (the Web UI reproduces
  the per-directory env a developer gets on the CLI). See the note in
  `internal/config/tools.go` (`SetToolMetadata`).

Whether any of these is *also* exposed as an MCP tool (AI-facing) vs. Web-UI-only
(operator-facing) follows the existing boundary: authoring/policy mutation is
operator-gated and stays in the Web UI (bound to localhost, not AI-facing);
the AI's only write path remains `wraptool_request_capability` (propose, not
apply). Stage 4 does **not** hand the AI a config-write tool.

Sequencing within Stage 4 is by frequency of the YAML-drop it removes; each
lands as its own page + `editConfig`-based edit function + test, so the stage can
ship incrementally.

## Why this shape

- **Reuse the single source of truth.** Every stage writes `flag_constraints` /
  rule nodes through `editConfig`; no new type channel, no parallel policy store.
  Same round-trip validation and hot-reload as the CLI and `ApplyRequest`.
- **Operator authority, AI proposal.** The AI proposes types; the human sees
  (Stage 1), corrects (Stage 2), and authors (Stages 3–4). No stage gives the AI
  a direct config-write path — the request/approve gate stays the only bridge.
- **Independently shippable.** Stage 1 is a template-only change behind one MR;
  each later stage adds one edit surface + test. Value lands early; scope grows
  only as far as the operator's YAML-drop pain justifies.
