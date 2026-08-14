# design/

Design notes and audits for wraptool. New here? Read `mcp-server-wrapping.md`
first (the foundational wrapping model), then browse by area below.

## Organization

- Subdirectories cluster notes by subsystem: `harness/`, `webui/`, `policy/`,
  `introspect/`, `doctor/`, `audit/`.
- The foundational wrapping model stays at the root: `mcp-server-wrapping.md`.
- Each note carries a `Status:` line near the top (`implemented` / `proposed`
  / `planned` / `superseded` / `findings` / `decided`); the index below mirrors
  it. Supersession is tracked by cross-links (e.g. `harness-pool-devcontainer.md`
  → `unified-harness-pool.md`).
- We deliberately do **not** use ADR numbering. These are living narratives and
  audits, not signed-off immutable decisions; the `Status:` field plus
  supersession links carry the lifecycle. Date-prefixed filenames are avoided
  too — the topic is what you grep for, not the date.

## Index

| Path | Area | Status | One line |
|------|------|--------|----------|
| `mcp-server-wrapping.md` | core | draft | Wrapping local stdio MCP servers (the foundational model) |
| `documentation-onboarding.md` | docs | proposed | Close the 0.5.0 docs gap; make first-run the documented path |
| `harness/acp-bridge.md` | harness | proposed | ACP editors (Zed, Neovim, Emacs) driving a harness inside a container — a transparent pipe and documentation, not a mediating bridge |
| `harness/guix-container-runtime.md` | harness | implemented | Guix-container runtime for `wraptool up` |
| `harness/harness-descriptor.md` | harness | implemented | A shared harness descriptor registry |
| `harness/harness-pool-devcontainer.md` | harness | superseded | Shared harness pool for the devcontainer runtime → `unified-harness-pool.md` |
| `harness/microvm-sandbox-runtime.md` | harness | proposed | Investigate Docker Sandboxes microVMs as a harder container boundary |
| `harness/unified-harness-pool.md` | harness | proposed | One version-locked harness pool for Guix and Dev Containers |
| `webui/webui-management-completion.md` | webui | planned | Complete the operator Web UI for flag typing and policy management |
| `webui/webui-accessibility-reachability.md` | webui | proposed | Accessibility and reachability of the operator review UI |
| `webui/webauthn-admin-auth.md` | webui | proposed | WebAuthn admin authentication |
| `policy/flag-constraint-type-single-source.md` | policy | proposal | Single source of truth for flag-constraint types |
| `policy/flag-type-inference.md` | policy | implemented | Flag value types come from the capability author, not a regex |
| `policy/security-audit-policy-enforcement.md` | policy | findings | Policy-enforcement perimeter & config fail-open surface (open) |
| `policy/opt-in-documentation-tooling.md` | policy | proposal | Opt-in documentation tooling for development containers |
| `doctor/wraptool-doctor.md` | doctor | implemented | `wraptool doctor` — dependency, posture, policy-lint and drift preflight (drift is informational: §7.6 records why absence claims are unsound) |
| `doctor/implementation-plan.md` | doctor | implemented | Step-by-step delivery of `wraptool doctor`; A1–G3 shipped, F3/F4 deliberately not built |
| `introspect/introspection-cache-invalidation.md` | introspect | implemented | Introspection cache invalidates on its real inputs |
| `introspect/tool-descriptions-from-help.md` | introspect | decided | Tool descriptions come from `--help`, not config |
| `audit/claude-audit.md` | audit | findings | Security audit — wraptool |
| `audit/kimi-audit.md` | audit | findings | Full-surface review: network edge, token at rest, policy, scrubbing, isolation |

## Adding a note

1. Put it under the matching subsystem subdirectory, or at the root only if it
   is foundational like `mcp-server-wrapping.md`.
2. Start it with a `# Design: <title>` heading and a `Status: <token>` line.
3. Add a row to the index table above with the new path, area, status, and a
   one-line summary.
4. Reference other notes by path relative to the repo root
   (`design/policy/flag-type-inference.md`), so links stay stable under `git mv`.
