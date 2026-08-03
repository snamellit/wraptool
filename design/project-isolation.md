# Design: project grants and harness state isolation

Status: **proposal - decisions open**
Created: 2026-07-27

## 1. Purpose

This design refines two boundaries that are currently advisory or shared:

1. A container chooses `?cwd=` while authenticating with one server-wide MCP
   token.
2. Harness HOME is writable state shared by every project.

The intended model is a server-issued project capability plus read-only shared
harness material and project-private writable state. This is a prospective
design, not a description of current behavior.

## 2. Threat model

Project source, build tools, and the harness process inside a project container
are untrusted. The host-side `wraptool up` process and operator are trusted. A
malicious process already running directly as the developer's host UID is out of
scope; same-UID filesystem and socket permissions cannot distinguish it from
the operator.

Deliberate transfer of a bearer grant between cooperating malicious projects is
out of scope: a bearer capability is transferable by definition, and the
recipient gains only the donor project's authority. The boundary must still
prevent one project from silently planting mutable harness state that executes
or influences a later clean project without that project's cooperation.

## 3. Project grants

Do not overload MCP transport session IDs. They identify protocol sessions and
may be client-asserted; they are not authorization objects.

Before starting a container, host-side `wraptool up` registers a **project
grant** with the shared server. Host-side `wraptool init` does the same for an
IDE or other project client that does not use `up`. The grant contains:

- an opaque project identifier suitable for status and audit output;
- a high-entropy bearer secret;
- the canonical Git worktree root;
- optional non-authoritative metadata such as runtime and harness name;
- creation, expiry, and revocation state.

The container receives only the project bearer secret. The server maps that
secret to immutable project context and supplies the authorized CWD internally.
Project clients do not send `?cwd=` on any transport. A client-supplied CWD is
rejected rather than used as an authorization input.

The existing server-wide bearer token cannot remain valid for project MCP calls:
that would bypass grant binding. During migration it becomes a host-only
bootstrap/control credential, or is replaced by a separate control credential,
and is never injected into a project container. Existing generated project
configs must be regenerated with grants. A deliberately global client needs an
explicit operator-created grant with fixed CWD and policy; there is no
unrestricted compatibility token on the project MCP endpoint.

`mcp.allowed_cwd_roots` remains defense in depth at **grant issuance**: `up` and
`init` may register only worktrees covered by an operator-configured root. A
root no longer means every token holder can select every path below that root.

Grant creation must use a host-only control path distinct from project MCP
authority. Preferred Linux mechanism is an owner-only Unix control socket with
same-EUID peer verification. Non-Linux bootstrap, persistence across server
restart, expiry, rotation, and `down` revocation require design before
implementation.

The server should eventually pin an opened directory through child startup as
specified by UNIX-3 in `credential-webui-unix-hardening.md`. Storing a canonical
pathname alone does not close path-replacement races.

## 4. Harness state

Current Guix behavior mounts pool `bin/` and `lib/` read-only but shares pool
`home/` read-write. Current Dev Container behavior installs the binary in the
image and shares the same writable pool HOME. The writable HOME can contain
configuration, hooks, extensions, skills, shell startup files, and
HOME-local executables, so read-only `bin/` alone does not prevent
cross-project persistence.

Target invariant:

- Shared Guix harness executables and libraries come from a host-managed source
  mounted read-only. An absolute descriptor-selected executable is preferable;
  otherwise its directory precedes project-writable PATH entries. It need not
  shadow the trusted project toolchain.
- Dev Container harness binaries remain per-image and are not shared into later
  projects. A project-controlled image may replace its own harness, but its
  project grant still bounds host-side authority.
- No mutable configuration is shared directly between projects.
- Each canonical worktree gets private writable harness HOME/config/state.
- Deliberately shared skills or extensions are operator-curated and mounted as
  a read-only lower layer.
- Project modifications use a private copy-on-write upper layer and never flow
  back into the shared seed or another project.
- Project MCP grant material is written only to project-private state, never to
  the shared seed.

"Copy-on-write" describes semantics, not a required overlayfs implementation.
A portable implementation may copy a versioned seed into project state on first
use, then retain that private copy. Guix and Dev Container runtimes must expose
the same trust boundary even if their mount mechanisms differ.

## 5. Authentication state

Harness service authentication is the unresolved part of removing shared HOME.
Options to evaluate:

1. Read-only shared authentication plus project-private transient refresh
   state. This works only if the harness tolerates immutable credential files.
2. Per-project authentication copied from an operator-managed seed. This
   duplicates credentials but removes cross-project writes.
3. A host-side credential broker. This gives the strongest boundary but adds a
   new protocol and may not fit harness authentication flows.

Until one is selected, do not claim that the entire harness pool can be mounted
read-only. Binary, library, skill, and extension sharing can be made read-only
independently from authentication behavior.

## 6. Risk priority

Project grants remove the simple global-token plus arbitrary-CWD lateral path.
Read-only/COW harness state removes one-project persistence into later projects.
Neither protects a host-side tool from project-controlled executable
configuration in the authorized worktree.

Git repo-local execution remains the shorter and more likely host escape: one
malicious project can combine a permitted host-side Git operation with settings
such as hooks, pagers, fsmonitor, or SSH commands. Git hardening therefore stays
ahead of defenses aimed only at deliberate token exchange between collaborating
projects.

## 7. Open decisions

- Grant registry persistence and restart behavior.
- Grant expiry, renewal, and `wraptool down` revocation semantics.
- Migration from the server-wide MCP token to separate control and project
  credentials.
- Control-plane authentication on non-Linux hosts.
- Canonical worktree identity across rename and linked worktrees.
- Shared skill/extension seed format, update approval, and versioning.
- Copy-on-write implementation and cleanup policy for each runtime.
- Harness authentication strategy from section 5.
- Whether any harness-defined global hook or executable extension is safe to
  include in the shared read-only seed by default.
