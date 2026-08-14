# Design: running an ACP harness inside a wraptool container

Status: **proposed** (design only — nothing implemented). §13's capability
tests have been run on a Guix host and **R1's premise holds**: stdio into the
container is byte-transparent and interactive (§13.1). This note still
describes future work, not current behaviour: per
`docs/contributing.qmd#documentation-accuracy` a design document is not
automatically a statement of what wraptool does today.

Scope: let an editor that speaks the Agent Client Protocol (Zed, and the
Neovim/Emacs clients built on the same contract) drive a harness running
inside a wraptool container. The note covers what ACP is, why the capability
belongs in the documentation, the two small primitives the container runtime
is missing, the security consequences of a transparent pipe, and what a
capability test must establish first. It does **not** propose changes to the
policy engine, the MCP tool surface, the scrubbing pipeline, the review UI, or
the pool's lock format.

> **Ruling R1 — primitives and documentation, not a mediating bridge.**
> An earlier revision of this note made a message-parsing, default-deny relay
> a normative requirement: the launcher would own the child's pipes, parse
> every ACP message, narrow the `initialize` handshake and refuse
> non-allowlisted methods at the wire. That is now **rejected for the first
> release** (§10). wraptool stays ACP-agnostic: it gains the ability to run a
> named program inside the project container with a byte-transparent stdio
> path, and the documentation explains how to point an editor at it. The
> security analysis in §7 is retained **in full and unchanged in substance** —
> not because a relay closes those holes, but because under R1 the
> documentation is what carries them to the developer. §10 states what R1
> gives up and the conditions that would reopen it.

## 1. Problem

`wraptool up` brings a harness up inside a container and drops the developer
into an interactive shell there (`cmd/guixcontainer.go:176-180` for the Guix
runtime, `cmd/up.go:1024-1030` for the Dev Container runtime). The developer
then types `claude` — or `opencode`, or `gemini` — at that shell. The harness
is a terminal program and the terminal is the interface.

ACP editors want a different arrangement. The editor is the interface: it
renders the conversation, the diffs, the tool calls and the permission
prompts in its own UI, and it expects to *spawn the harness itself* as a
child process and speak JSON-RPC to it. Zed's configuration surface for this
is exactly three fields — `command`, `args`, `env` — under an
`agent_servers` entry, and the process it spawns is expected to be the ACP
server. There is no place in that configuration to say "and first enter this
container".

So the container boundary is in the way twice over:

- **Launch.** Nothing in wraptool runs a named program inside the *project*
  container — both `up` runtimes hardcode an interactive login shell as the
  container entry (`cmd/guixcontainer.go:610` ends the built entry string
  with `exec bash -l`; `cmd/up.go:888-894` builds `devcontainer exec
  --workspace-folder <root> bash -l`), and `wraptool up`'s `RunE` calls
  `runUp(opts)` while ignoring `args` (`cmd/up.go:63-65`) — the command sets
  no cobra `Args` field, so extra positional arguments are accepted and
  silently discarded rather than rejected. The pool installer *does*
  parameterise a container argv already: `npmInstall` builds `guix shell
  --container --network --emulate-fhs --share={prefix}={prefix} node -- npm
  install -g <pkg> --prefix {prefix}` (`internal/harness/harness.go:131-137`),
  run by `runInstallArgv` (`cmd/harness.go:1124-1128`), and
  `internal/pool/backend` execs provisioning containers the same way
  (`internal/pool/backend/backend.go:171-180`). That is the closest existing
  precedent for §9's launch descriptor — an argv template threaded through a
  container invocation is not new; threading one through *this* container is.
- **The channel back.** ACP is not a one-way command invocation. The harness
  gets to send *requests to the editor*, and several of them ask the editor
  to read files, write files, and run shell commands — in the editor's
  environment, which is the host, with the developer's credentials. Every
  one of those is a hole straight through the boundary the container exists
  to hold. §7 is the substance of this note.

The launch problem is cheap in bring-up terms: ACP's transport is the child
process's own stdin and stdout, and the container process is already a child
of whatever launched it, so no network hop is needed. The bridge is therefore
a **host-side launcher**, not a network service. But cheap bring-up is not the
same as a cheap component: §4.1 states the invariant that the bridge must own
the pipes rather than hand the container the editor's own descriptors, and §7
is why. The launcher has to be built as a policy point from the first commit
rather than as a pipe that grows one later.

### 1.1 Three unrelated pipes — do not conflate them

Three message channels appear in this note and they have nothing to do with
each other:

- **The ACP connection**, between the editor and the harness process,
  carried on that process's own stdin and stdout as newline-delimited
  JSON-RPC. This is what the note is about.
- **wraptool's MCP transport**, between the harness and the wraptool server.
  That is `sse` or `unix`, and it stays exactly as it is. One TCP listener
  serves legacy SSE at `/sse` and Streamable HTTP at `/mcp` behind Bearer
  auth (`cmd/serve.go`). Nothing in this note proposes a new MCP transport,
  changes the existing ones, or moves the harness onto a different one.
- **Local stdio MCP servers**, which an ACP client can push into a session
  via `session/new`'s `mcpServers` array (§7.3). These are *upstream* MCP
  servers the harness would spawn and speak stdio to — wraptool's own
  `mcp_servers` concept, unrelated to wraptool's own server transport. §7.3
  recommends stripping them.

Where the note has to name ACP's own framing it says "ACP's stdio framing" or
spells out "the harness process's stdin and stdout". The token is never a
statement about how the harness reaches the wraptool server.

### 1.2 Vocabulary — ACP's nouns collide with ours

ACP calls the editor the *Client* and the coding-agent program the *Agent*.
`docs/terminology.qmd`'s actors table defines **agent** as "The AI acting
through a harness: it decides what to do and calls MCP tools", and names the
program it runs in the **harness**. Read naively, "the ACP agent" means the
opposite thing to each side.

| ACP's word | This note writes | Meaning |
|---|---|---|
| Client | **ACP client** | The editor — Zed, Neovim, Emacs |
| Agent | **the harness** (never bare "agent") | The `claude` / `opencode` / `gemini` process inside the container |
| — | **agent** | Reserved for the coding agent itself, per `docs/terminology.qmd`'s actors table |
| — | **the bridge** | The host-side wraptool component the editor spawns |

"Bridge" is chosen deliberately over "proxy": **proxy** is a loaded word here
(`docs/terminology.qmd`'s concepts table contrasts wraptool's *mediation*
model against a credential *proxy*), and over "wrapper", which the
Preferred/Avoid table in `docs/terminology.qmd` rules out for anything
harness-shaped (the harness row's Avoid column is "wrapper, agent binary,
tool"). "Adapter" is not a listed term at all; this note uses it only where it
names a third-party upstream package that calls itself that (§8).

If this design proceeds, `ACP client` and `bridge` are project vocabulary and
belong in `docs/terminology.qmd`'s table when the feature ships. That table
binds `docs/` pages; this note establishes the vocabulary for design use only,
which is why the rows are scheduled with WP3 (§12) rather than added here.

## 2. Goal and non-goals

The goal: a developer can point an ACP editor at a harness running inside the
project container, and the documentation tells them how. wraptool's side of
that is deliberately small — the ability to run a **named program** inside the
project container instead of a login shell, over a stdio path that carries the
program's bytes and nothing else. ACP is then a *use* of that primitive, not a
feature of wraptool. Nothing in the codebase needs to know what ACP is.

That framing is the point, not an economy measure. A primitive that runs an
arbitrary program in the project container with clean stdio serves ACP,
scripted harness invocations, CI, and every protocol that has not been
invented yet. A `wraptool acp` command that speaks a specific version of one
protocol serves exactly one, and dates.

### 2.1 Why this belongs in the documentation

wraptool contains no ACP code and, under R1, never will. It would be easy to
conclude there is therefore nothing to document. That is wrong on three
counts, and this section is the reason the documentation work in §12 is not
optional.

**It is the strongest statement of the product thesis.** wraptool exists so a
developer can keep working in the environment they are comfortable in — their
own editor, their own keybindings, their own host — while the harness does the
high-volume, higher-risk work inside a restricted environment where the blast
radius is bounded. Every existing entry point makes the developer *follow the
harness into the container* and work at a shell there. An ACP editor inverts
that: the harness is confined and the developer is not. That is the clearest
expression of the isolation argument wraptool has, and it is currently absent
from every page.

**Users will look for it and conclude, wrongly, that it is unsupported.**
"Does this work with Zed?" is a question with a good answer that nobody can
find. Absence of documentation reads as absence of capability, and the
capability is nearly free.

**The reasoning is not discoverable by inspection.** That transparent stdio is
sufficient — that a protocol wraptool has never heard of works because
JSON-RPC over a pipe crosses a namespace boundary without help — is obvious
once stated and invisible until then. A user reading the source finds no ACP
support because there is none to find. Only the documentation can carry it.

The documented recipe is also what makes §7's residual risks reachable by the
person who has to accept them. Under R1 there is no code that refuses anything
on the developer's behalf, so the documentation is the only control surface
that exists (§10.2).

### 2.2 Explicitly out of scope (for now)

- **A networked ACP endpoint.** ACP's remote transport (Streamable HTTP /
  WebSocket) is a draft RFD, not stable, and the real editors speak the
  stdio framing only. The harness presents as a process the editor spawned.
  Revisit when the RFD stabilises. Note that "out of scope for wraptool" is
  not the same as "unavailable to the harness" — §7.7 joins those two
  statements.
- **wraptool as an ACP client.** Nothing here proposes wraptool driving a
  harness programmatically for its own purposes.
- **ACP v2.** A v2 draft exists and has an open RFD specifically about the
  client filesystem/terminal capability model — the exact area §7 depends
  on. Design against v1; §14.1 carries the risk.
- **Host-side harnesses.** `cursor` and `roo` are registered as host IDE
  integrations with an empty `Bin` (`internal/harness/harness.go:163,171`)
  and are not container CLIs. They are out of scope regardless of whether
  their products speak ACP.
- **Changing the policy engine.** An ACP session does not add, remove or
  reinterpret a single policy rule. §7.8 is explicit about this.

## 3. What ACP actually is

Grounded in the v1 specification; capability and method names below are the
spec's own.

### 3.1 Transport and framing

Transport is normative in ACP v1, and there are exactly two: the stdio
framing, and a Streamable HTTP transport still marked as a draft proposal.
Agents and clients "**SHOULD** support stdio whenever possible", and every
harness surveyed in §8 implements that one only.

The rules, which are the design constraints:

- The client launches the harness as a subprocess. The harness reads
  JSON-RPC from its stdin and writes to its stdout.
- Messages are newline-delimited and **MUST NOT** contain embedded newlines.
  This is *not* LSP-style `Content-Length` framing.
- JSON-RPC messages **MUST** be UTF-8.
- The harness **MAY** write UTF-8 logging to stderr; clients may capture,
  forward or ignore it.
- **The harness MUST NOT write anything to its stdout that is not a valid
  ACP message.** The client MUST NOT write anything to the harness's stdin
  that is not a valid ACP message.

That last rule is the one that collides with wraptool's current behaviour,
and §5 is its own section because of it.

ACP also blesses custom transports explicitly ("the protocol is
transport-agnostic … implementers **MUST** ensure they preserve the JSON-RPC
message format and lifecycle requirements"), which is the licence for a
bridge that speaks the stdio framing to the editor while carrying the bytes
into a container by whatever means. It does not licence presenting anything
other than the stdio framing *to the editor*.

### 3.2 Methods, by direction

Client → harness requests, in full: `initialize`, `authenticate`, `logout`,
`session/new`, `session/load`, `session/resume`, `session/list`,
`session/close`, `session/delete`, `session/prompt`,
`session/set_config_option`, `session/set_mode`. Plus one client → harness
notification, `session/cancel`. Every harness **MUST** support `session/new`,
`session/prompt`, `session/cancel` and `session/update`; the rest are
capability-gated.

Harness → client requests, in full: `fs/read_text_file`,
`fs/write_text_file`, `session/request_permission`, `terminal/create`,
`terminal/output`, `terminal/kill`, `terminal/wait_for_exit`,
`terminal/release`, `elicitation/create`. Harness → client notifications:
`session/update`, `elicitation/complete`. The **only** baseline client method
is `session/request_permission`; `fs/*`, `terminal/*` and
`elicitation/create` are all optional.

Both sides implement one protocol-level method, `$/cancel_request`; a
receiver may ignore unknown `$/`-prefixed notifications, and must answer a
cancelled request with a response or error `-32800`. "A receiver may ignore
unknown `$/` notifications" is exactly the property that makes a denylist
unsafe (§7.2): the namespace is open by design.

There is **no** `initialized` notification in v1 — unlike MCP and LSP, the
handshake completes with the `initialize` response. (The draft HTTP RFD's
prose says otherwise; that phrasing is sloppy, not normative.)

### 3.3 Capability negotiation — the design lever

`initialize` params carry `protocolVersion`, `clientInfo`, and
`clientCapabilities`: `fs` (`{readTextFile, writeTextFile}`, both defaulting
to false), `terminal` (boolean, default false), `elicitation`, `session`
(only `configOptions` today), `_meta`. The response carries
`protocolVersion`, `agentInfo`, `agentCapabilities` (`loadSession`,
`promptCapabilities`, `mcpCapabilities`, `auth`, `sessionCapabilities`) and
`authMethods`.

Three spec statements make capability subsetting a real control rather than a
hint:

- "All capabilities included in the `initialize` request are **OPTIONAL**."
- "Clients and Agents **MUST** treat all capabilities omitted in the
  `initialize` request as **UNSUPPORTED**."
- Per feature: "If `readTextFile` or `writeTextFile` is `false` or not
  present, the Agent **MUST NOT** attempt to call the corresponding
  filesystem method." "If `terminal` is `false` or not present, the Agent
  **MUST NOT** attempt to call any terminal methods."

A client that advertises `{}` is a legal ACP client. §7.4 is careful about
what that does and does not buy.

Version negotiation is symmetric: the client sends its latest supported
version, the harness echoes it if supported or answers with its own latest,
and a client that cannot speak the answer should close the connection.
Current stable version is `1`. §7.5 explains why the bridge caps the
negotiated version rather than passing an arbitrary one through.

### 3.4 Sessions

Sessions are per-connection contexts, not per-process: one harness process
hosts many sessions, each with its own `sessionId`, history and state.
`session/new` takes a required absolute `cwd`, a required (possibly empty)
`mcpServers` array, and optional `additionalDirectories`. The effective root
set is `[cwd, ...additionalDirectories]` and "**SHOULD** serve as a boundary
for tool operations on the file system" — SHOULD, advisory, not a sandbox.

`session/cancel` is turn-level and distinct from `$/cancel_request`; the
harness may keep emitting `session/update` after a cancel but must do so
before responding to `session/prompt`. Anything in the message path must
therefore preserve per-session ordering.

All file paths in the protocol **MUST** be absolute, and the spec has no
notion of URI schemes for them, of roots negotiation, or of host↔container
path rewriting. §6 is the consequence.

## 4. What the bring-up gives for free, and what it does not

The bring-up already does every hard part of getting a container running.
Walking `runUp` (`cmd/up.go:80`) in order: the in-container guard
(`cmd/up.go:81`), the worktree probe (`cmd/up.go:88`), configuration load
(`cmd/up.go:96-110`), runtime selection (`cmd/up.go:113-115`,
`cmd/guixcontainer.go:38`), harness-name validation against the registry
(`cmd/up.go:126`), the workspace-containment guards for the config and the
admin token (`cmd/up.go:132`, `cmd/up.go:504-528`), bind adjustment for the
runtime (`cmd/up.go:143`), the shared-server ensure with its runtime-dir
flock (`cmd/up.go:149`, `cmd/up.go:272`), token read (`cmd/up.go:163`), then
the runtime dispatch (`cmd/up.go:171-174`).

On the Guix path, `bringUpGuixContainer` (`cmd/guixcontainer.go:71`) then
writes the harness's MCP configuration on the host into the shared workspace
(`cmd/guixcontainer.go:83-92`, via `harness.Write`,
`internal/harness/harness.go:193`), resolves the pool plan, builds the
container entry string, and runs `guix shell`:

- `buildContainerEntry` (`cmd/guixcontainer.go:597-611`) emits exactly
  `export WRAPTOOL_CONTAINER=1; export TERM=xterm-256color; export
  PATH=$PATH[:<quoted dir>]*; export HOME=<pool home>; cd <root>; exec bash
  -l`. Pool directories are **appended** to `$PATH`, never prepended
  (`cmd/guixcontainer.go:608`), so a poisoned pool cannot shadow the
  toolchain.
- `buildGuixShellArgs` (`cmd/guixcontainer.go:618-633`) assembles `guix shell
  --container --network --emulate-fhs --share=<root>=<root>`, the pool
  plan's read-only exposes, `--share=<poolHome>=<poolHome>`,
  `--preserve=^WRAPTOOL_`, `--preserve=^COLORTERM$`, `-m <manifest>`, then
  `-- bash -lc <entry>` (`cmd/guixcontainer.go:630`).
- The environment handed to the `guix` process carries `WRAPTOOL_TOKEN`,
  `WRAPTOOL_CWD` (the *host* path, so the server's `?cwd=` scoping matches)
  and `WRAPTOOL_CONTAINER` (`cmd/guixcontainer.go:170-174`); only names
  matching those two `--preserve` patterns cross the namespace.
- The container process inherits the parent's file descriptors directly —
  `cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr`
  (`cmd/guixcontainer.go:179`) — and `cmd.Run()` blocks, so the process
  lifetime *is* the session lifetime (`cmd/guixcontainer.go:180`).

Everything in that list except the last bullet is reused verbatim: the
workspace share, the pool exposes, the PATH append, the `HOME` pin, the
`WRAPTOOL_*` environment, the MCP configuration write, the shared server, and
the three guards named above.

### 4.1 The hard invariant: the pipe stays transparent

The last bullet is the one that matters most, and under R1 it is reused
deliberately rather than replaced. The container child already inherits the
real descriptors (`cmd/guixcontainer.go:179`). That is exactly the behaviour
an ACP editor needs: the harness's stdin and stdout simply *are* the editor's
pipes, and JSON-RPC crosses the namespace boundary with no host-side
participant in the message path at all.

> **Invariant.** On an ACP launch the harness's stdout **MUST** carry the
> harness's bytes and nothing else, unmodified and unbuffered by wraptool.
> No wraptool prose, no runtime progress output, no login-shell echo. Every
> intermediary on the path — `guix shell`, `bash`, `devcontainer exec` —
> **MUST** be shown to pass the child's stdout through byte-for-byte (§13),
> or be removed from the path.

The whole feature is that invariant plus a way to name the program. There is
no relay, no parse step, no re-serialisation, and therefore no per-session
ordering, backpressure or `$/cancel_request` bookkeeping to get right. What
would have been the security-critical component of this feature does not
exist, which is the strongest argument for R1 and, simultaneously, the reason
§7 has to be carried into the documentation instead of into code (§10).

So the changes are: (a) the tail of the entry string, `exec bash -l` becoming
`exec <harness ACP argv>`; and (b) the output-hygiene work of §5, so that (a)
is the only thing on stdout. Both are small. Neither is ACP-aware.

The test pin that WP1 carries is the inverse of the earlier revision's: the
non-interactive entry path must hand the child the inherited descriptors
**unwrapped**, and no print site may fire on it (§5.1, §12).

### 4.2 What has to change shape, not just content

- `upOptions.shell` is a three-valued string (`cmd/up.go:33`) resolved once
  into a `bool` by `wantShell` (`cmd/up.go:182-191`) and passed positionally
  to both runtimes (`cmd/up.go:172,174`; `cmd/guixcontainer.go:71`,
  `cmd/up.go:898`). A bool cannot express "run this argv". It becomes an
  entry *mode*.
- `wantShell`'s `auto` branch is TTY-driven (`cmd/up.go:189`, `isTTY` at
  `cmd/up.go:193-196`). Under an ACP editor, stdin and stdout are pipes, so
  `auto` resolves to `false` — and on the Guix path `shell == false` never
  enters a container at all (`cmd/guixcontainer.go:99-111`). **A bridge must
  not route through `wantShell`'s auto branch.**
- `buildContainerEntry` (`cmd/guixcontainer.go:597`) and
  `buildDevcontainerExecArgs` / `buildDevcontainerShellEntry`
  (`cmd/up.go:877-894`) each hardcode the login shell and must take the
  entry argv as a parameter.
- Both argv builders are pure string/slice functions with no exec, written
  that way so tests can pin them exactly (`cmd/guixcontainer.go:595-596,617`
  doc comments; pinned at `cmd/guixcontainer_test.go:750,794,852` and
  `cmd/up_test.go:711-719`), so the parameterisation is mechanical and
  testable. The two runtimes' pool-plan resolvers —
  `resolveGuixPoolPlan` (`cmd/guixcontainer.go:472`) and
  `resolveDevcontainerPoolPlan` (`cmd/up.go:634`) — are separate functions
  with different return types that share only their input struct
  (`poolPlanInput`, `cmd/guixcontainer.go:392`, embedded by
  `devcontainerPoolPlanInput` at `cmd/up.go:605-607`). Both return
  `notice`/`err` strings for the caller to emit and print nothing themselves,
  so a bridge can reuse either and choose its own output sink.

## 5. The blockers — three ways stdout gets contaminated

ACP's hardest rule is that the harness process's stdout carries ACP messages
and nothing else (§3.1). Today the `up` path writes human prose to stdout
freely and hands children the real descriptor. Three distinct problems, each
with its own fix.

### 5.1 wraptool's own progress output

Twenty-three unconditional `fmt.Print*` call sites on the `up` path write to
`os.Stdout` (one of them, `cmd/up.go:477`, can fire twice in a run).

- **Shared prefix, both runtimes:** `cmd/up.go:154` (the review-UI URL —
  **with the admin token embedded**), `cmd/up.go:168`, `cmd/up.go:298`,
  `cmd/up.go:306`, `cmd/up.go:337`, `cmd/up.go:477`, and `cmd/server.go:95`
  reachable via the stale-pidfile path.
- **Guix runtime:** `cmd/guixcontainer.go:93,95,96,103,109,166,176`. Note
  that `:103` and `:109` sit inside the `if !shell` block
  (`cmd/guixcontainer.go:99-111`) and never fire on the path that actually
  enters a container — which matters for §5.3, because a non-interactive
  launch reuses
  that warm-up deliberately.
- **Dev Container runtime:** `cmd/up.go:225`, `cmd/up.go:229` (both inside
  `adjustServerBindForRuntime`, which returns early at `cmd/up.go:216-218`
  unless the runtime is devcontainer and the transport is `sse`, so they can
  never fire on the Guix path), `cmd/up.go:961,975,980,988,1015,1017,1024`.

Fix: route every one of them through an injected `io.Writer` that defaults
to `os.Stdout` and is `os.Stderr` on a non-interactive launch. Mechanical.

Already clean and needing no work: the stale-binary notice
(`cmd/up.go:1339`), every `log.Printf` (stdlib `log` defaults to stderr and
nothing in the repo calls `log.SetOutput`), the plan notices
(`cmd/up.go:958`, `cmd/guixcontainer.go:163`), and the Dev Container
runtime's mount warnings (`cmd/up.go:977,990,996,998`). The helper packages
are clean too: `internal/harness`, `internal/config`, `internal/paths` and
`internal/webui` write nothing to stdout, and `internal/pool`'s single stdout
write (`internal/pool/backend/backend.go:178`) is on the `harness
install`/`sync` realize path, never reached by `up`.

**`cmd/up.go:154` needs more than a redirect.** It prints
`webURL(cfg.Admin.Listen, adminToken)` — the review-UI URL with the admin
token in its query string (`design/audit/claude-audit.md` finding 14, still
open; finding 12 on cookie handling compounds it). Moving it to stderr does
not fix this under a bridge: editors that spawn an ACP agent server routinely
capture the child's stderr into a log file on disk, so stderr under a bridge
is a persisted, editor-owned log, not a terminal. Routing a live admin token
there writes the credential that can approve capabilities and edit policy
into that log. **The requirement is therefore stronger than "route through an
injected writer": on a non-interactive launch, no stream carries a
secret.** Print the bare
URL without the token, or print nothing. The same rule applies to every WP2
print site that carries a secret.

### 5.2 Children that inherit stdout

Harder than the print sites, because the output is third-party and
unbounded. Four assignments:

| Site | Child | Ruling on a non-interactive launch |
|---|---|---|
| `cmd/guixcontainer.go:105` | `guix shell -m … -- true` profile warm-up | Must go to stderr |
| `cmd/up.go:1008-1009` | `devcontainer up` | Must go to stderr or a log |
| `cmd/guixcontainer.go:179` | the container process | **Kept as-is** — the inherited descriptors are the feature (§4.1) |
| `cmd/up.go:1027-1029` | `devcontainer exec` | Same, for that runtime |

`devcontainer up` is the worst of these: it emits human progress lines and a
terminating JSON result object on stdout, and the argv wraptool builds
(`cmd/up.go:684-690`) passes no quieting flag. Whether the CLI offers one is
an open question (§14.2); if not, `cmd/up.go:1008` is simply repointed at
stderr on a non-interactive launch, which changes nothing for an ordinary
`wraptool up`.

### 5.3 First-run profile realization streams build logs

On the Guix runtime, a cold profile realizes on first entry. The existing
non-shell path deliberately warms it and *shows* the output
(`cmd/guixcontainer.go:103-105`). A bridge must do the same warm-up, before
the ACP-carrying container starts, with the child's stdout pointed at stderr
— so the editor sees the harness's first JSON-RPC frame rather than a build
log. (Guix normally writes build progress to stderr already, but that has
not been verified empirically here; the warm-up must not depend on it.
§14.2.)

### 5.4 The login shell can echo before the harness starts

Both runtimes go through `bash -l` / `bash -lc`
(`cmd/guixcontainer.go:630`, `cmd/up.go:888-894`). A login shell sources
profile scripts, and a profile script that prints — a banner, a version
notice, a `direnv` line — lands on stdout ahead of the harness. The
container's `HOME` is the shared pool `home/` (`cmd/guixcontainer.go:609`),
which is developer-owned and can accumulate exactly such files.

Three candidate fixes, in increasing strictness: keep `bash -lc` but redirect
the profile phase; use `bash -c` (no login profile) and set the needed
environment explicitly in the entry string, which is what the entry already
does for `PATH`, `HOME`, `TERM` and `WRAPTOOL_CONTAINER`; or have the bridge
tolerate and discard leading non-JSON lines. The third is a protocol
violation dressed as robustness and should be rejected. The spike (§14.2)
should establish whether dropping `-l` breaks anything a harness needs.

## 6. Path identity and translation

ACP requires every path to be absolute and defines no mapping mechanism
(§3.4). So either container paths equal host paths, or something in the
middle rewrites them.

**Guix runtime: identity holds for the workspace, for free.**
`buildGuixShellArgs` shares the worktree at the same host path —
`--share=<root>=<root>` with the comment "workspace rw, host path preserved →
`?cwd=` matches" (`cmd/guixcontainer.go:622`) — and the entry `cd`s to that
same path (`cmd/guixcontainer.go:610`). The pool `home/` is likewise shared
at its host path (`cmd/guixcontainer.go:626`). An ACP client's absolute
worktree paths therefore need no translation at all, which is the single
biggest reason to ship the Guix runtime first.

**Dev Container runtime: identity does not hold.** Nothing in the repository
sets `workspaceFolder` or `workspaceMount`, so the devcontainer CLI's default
applies and the workspace appears at `/workspaces/<basename>`, while
`WRAPTOOL_CWD` is injected as the *host* path (`cmd/up.go:916`). The pool
`home/` is pinned to `/home/wraptool-harness` by the wiring-only feature, not
mounted at its host path. So for this runtime the bridge must rewrite paths
in both directions.

Path rewriting is genuinely lossy and should be treated as such. Paths appear
in `session/new`.`cwd`, `session/load`/`session/resume` `cwd`,
`additionalDirectories[]`, `fs/read_text_file`.`path`,
`fs/write_text_file`.`path`, `terminal/create`.`cwd`, a local stdio MCP
server entry's `command` (`McpServerStdio.command`), `ToolCallLocation.path`,
`Diff.path`, the `session/list` `cwd` filter and `SessionInfo.cwd` — *and*
inside opaque `rawInput`/`rawOutput` objects and `file://` URIs in
`resource_link` / `resource` content blocks, which a rewriter cannot reach
without guessing.

Two alternatives to rewriting, both worth the spike's attention: pin
`workspaceFolder` in the scaffolded `devcontainer.json`
(`cmd/init_devcontainer.go`) so the container path equals the host path for
new projects, which fixes the problem at the source but does nothing for
existing configurations; or declare the Dev Container runtime unsupported for
ACP in the first release and say so plainly.

## 7. Security analysis

This is the section that decides whether the feature is documentable at all.

The threat model is `docs/security.qmd`'s, unchanged: the whole workspace is
untrusted, prompt injection can steer the agent, and a compromised transitive
dependency runs drive-by the moment you build or test. §7.11 states plainly
which of the risks below are *created* by this feature and which merely
change shape.

> **How to read this section under R1.** Every "Strip", "Validate", "Refuse"
> and "Cap" ruling below was written for a mediating relay that no longer
> exists in the plan. **None of them is enforced by wraptool under R1.** They
> are retained verbatim, and deliberately, for three reasons: they are an
> accurate description of what an ACP connection can carry; they are the
> source material for the documentation §12 requires, where each becomes
> something the developer must know rather than something wraptool refuses;
> and they are the specification a relay would implement if §10.3's reopening
> conditions are ever met. Read every ruling as **"a developer pointing an
> editor at a container harness is exposed to this"**, not as "wraptool
> prevents this".

### 7.1 What the container isolates today, and what it does not

The isolation claim is narrow and precise: credential isolation is the
mount/user namespace, not the package set. The host `$HOME` and the
developer's secrets are never shared into the container, so privileged and
remote git still has to go through wraptool's MCP git tool, while local
non-privileged git works in-box (`design/harness/guix-container-runtime.md`
R2 banner). Only `WRAPTOOL_*` and `COLORTERM` cross the environment boundary
(`cmd/guixcontainer.go:627-628`).

It is **not** a network boundary. `--network`
(`cmd/guixcontainer.go:620`) shares the host network namespace, and wraptool
says so in the bring-up output: "note: `--network` shares the host network
namespace (internet + host loopback reachable); credentials stay isolated via
the mount namespace" (`cmd/guixcontainer.go:96`). The container can already
reach the internet and host loopback. ACP does not change that, and no claim
in this section rests on network isolation — but §7.7 and §7.9 both turn on
it.

It is also not a *filesystem* boundary for the worktree: the workspace is
shared read-write at the host path (`--share=<root>=<root>`,
`cmd/guixcontainer.go:622`), and the pool `home/` is shared read-write into
every project's container (`--share=<poolHome>=<poolHome>`,
`cmd/guixcontainer.go:626`). §7.6 is what those two facts become once an
editor is attached to the same tree.

What ACP adds is new and has no equivalent today: **a channel on which the
harness can ask the host to act on its behalf.** Today the container's
stdout goes to a terminal a human reads. There is no callback.

### 7.2 The harness → client direction: an allowlist, not a denylist

`fs/*` and `terminal/*` execute "within the Client's environment" — the
spec's own words — which under a bridge means the editor, on the host, as
the developer.

The enforcement rule is stated as an **allowlist**, because a denylist of the
nine v1 method names fails open in at least three ways: a client-side method
added in a v1.x revision would be forwarded by default; a vendor or extension
method (a `$/`-prefixed notification, which §3.2 notes a receiver may ignore,
or a namespaced `zed/*`) would be forwarded by default; and any negotiated
protocol version above the one the bridge enumerated brings a method set the
denylist provably does not know.

> **Rule.** In the harness → client direction the bridge forwards exactly two
> messages: the request `session/request_permission`, and the notification
> `session/update`. Everything else is dropped — unknown method, unknown
> `$/` notification, unknown namespace, unknown extension — answering a
> request with a JSON-RPC error and dropping a notification silently at the
> wire. `$/cancel_request` is handled by the bridge itself as part of its own
> relay bookkeeping (§14.1) rather than blindly forwarded.

Unknown-field preservation, were a relay ever built (§10.4), would apply
**only within allowlisted
messages and only in the client → harness direction**. A harness → client
`_meta` object is an unbounded, unparsed side-channel from the container to
the editor; it must be dropped, or allowlisted key by key, never passed
through untouched.

Classified, with what forwarding each would defeat:

| Method | Runs where | What forwarding it defeats | Ruling |
|---|---|---|---|
| `terminal/create` | Host, editor's environment. Takes `command`, `args`, `env`, `cwd` | Everything. Arbitrary host command execution is strictly more than *any* policy-approved MCP tool, because policy constrains per subcommand and per flag | **Never forward** |
| `terminal/output` | Host | Reads the output of the above | **Never forward** |
| `terminal/kill` | Host | — | **Never forward** |
| `terminal/wait_for_exit` | Host | — | **Never forward** |
| `terminal/release` | Host | — | **Never forward** |
| `fs/read_text_file` | Host, including unsaved editor buffers | The credential isolation premise entirely: `~/.ssh/id_*`, `~/.aws/credentials`, the wraptool `config.yaml`, and the MCP/admin token files written under XDG paths at mode 0600 (`cmd/up.go:459-477`) are all readable by absolute path | **Never forward** |
| `fs/write_text_file` | Host. Absolute path, no root restriction; client **MUST** create the file if absent | Persistence and privilege escalation on the host — `~/.bashrc`, `~/.ssh/authorized_keys`, editor configuration, wraptool's own `config.yaml`. Worktree writes are a separate and sharper problem under an ACP editor: see §7.6 | **Never forward** |
| `elicitation/create` | Host UI | Developer-to-container secret solicitation with the editor's trust dressing: elicitation returns developer-typed text *into a container-side process*, so a harness can ask for a credential in editor-native UI and receive it in plaintext, in a container with network egress (§7.1) | **Never forward** (wire-level, not merely un-advertised) |
| `session/request_permission` | Host UI | The one baseline client method; the harness cannot function without it. It is also the design's primary attacker-controlled surface — see §7.11 | **Must forward**, marked (§7.11) |
| `elicitation/complete` (notification) | Host | Paired with a method that is never forwarded | **Never forward** |
| `session/update` (notification) | Host UI | Carries no host authority; subject to §6's path rewriting on the Dev Container runtime | **Forward** |

So: of the nine harness → client **requests**, eight are refused and one —
`session/request_permission` — is required; of the two harness → client
**notifications**, `elicitation/complete` is refused and `session/update` is
forwarded. **Every optional client capability is refused at the wire**,
`elicitation/create` included: refusing it merely by not advertising it
applies the distinction §7.4 draws between *mitigate* and *prevent* unevenly,
and a nonconforming harness simply calls the method that got the weaker
treatment.

That is the default-deny posture, and it is why a dumb pipe is not an
acceptable intermediate milestone — §12 ships the launcher and the filtering
together.

### 7.3 The client → harness direction: what the editor pushes in

The reverse direction is not benign, and refusing `fs/read_text_file` does
not by itself preserve the isolation claim.

| What arrives | Why it matters | Ruling |
|---|---|---|
| `session/new`.`mcpServers` | An array of MCP servers **pushed by the editor into the container**; the spec's own example shows a local stdio MCP server entry with `env` holding an `API_KEY`. A channel for editor configuration to inject credentials and arbitrary server commands into the container — weakening isolation from the other side | **Strip** (§7.9) |
| `session/new`.`cwd`, `additionalDirectories[]` | Nothing constrains them to the worktree the container actually shares | **Validate** — reject anything outside the shared worktree |
| `session/prompt` content blocks: `resource`, `resource_link`, `file://` URIs | The developer attaches host file content in the editor and it is carried into the container. §6 treats these only as a path-rewriting problem; they are also a content path | **Forward, but see below** |
| `session/load`, `session/resume`, `session/list` | Harness session state lives in the pool `home/`, shared read-write into every project's container (`cmd/guixcontainer.go:626`) and already a known cross-project channel (`docs/security.qmd`, Known limitations: "one project can alter state observed by later projects using the same pool"). So `session/list` enumerates other projects' `SessionInfo.cwd`, and `session/load <other-project-session-id>` pulls another project's conversation state into this container. The `cwd` validation in the row above covers `cwd` and `additionalDirectories`, not `sessionId` | **Scope or refuse** — see below |
| `initialize`.`protocolVersion` | Determines which method set is legal (§7.5) | **Cap**, do not pass through blindly |

Two consequences worth stating without hedging.

**Host file content can still reach the container**, via editor-attached
context. `docs/security.qmd` says host credentials "never enter the container
or the agent context". Under an ACP connection that remains true for
credential *files* only because the developer would have to attach them
deliberately — the mount boundary is not what is doing the work on this path.
Say that in the documentation rather than implying otherwise (§7.10).

**Session methods need scoping or refusal.** The bridge should scope
`session/list` to sessions whose recorded `cwd` is inside this connection's
worktree, and reject `session/load`/`session/resume` for a session whose
recorded `cwd` is outside it. If that turns out to need harness cooperation
the bridge does not have, declare those three methods refused in the first
release and say so.

### 7.4 Does filtering the handshake actually bind the harness?

Only in the sense that a contract binds. The spec's language is normative —
a conforming harness "**MUST NOT** attempt to call" a method whose capability
was not advertised — and it does mean a correct harness will not try. But it
is a statement about conforming implementations, not a mechanism. A buggy
harness, a harness whose adapter has a defaulting bug, or a harness driven by
a prompt-injected agent can emit `terminal/create` anyway.

**Under R1 there is no second line of defence, and this must not be
misstated.** With a transparent pipe (§4.1) nothing sits on the message path,
so a `terminal/create` the harness was never authorised to send arrives at the
editor and is handled by the editor. The contract is the only thing between
them, and a contract does not constrain a harness that is buggy,
prompt-injected, or hostile.

What remains is not nothing, but it is not wraptool's:

- **The editor's own permission model.** `session/request_permission` exists
  precisely so the client can put a human in front of a consequential action,
  and the mainstream ACP editors do prompt. That is a real control, and for
  the developer-in-the-loop case it is the control that is actually load
  bearing. Its weaknesses are stated plainly in §7.11 — permission fatigue,
  one-keystroke accept, and the fact that a granted permission is recorded
  nowhere.
- **The capabilities the client advertises.** A client that does not advertise
  `terminal` should not be asked for one. Whether the editor lets the
  developer turn those capabilities off is per-editor and unverified (§14.1),
  which is why §13's capability test asks.

So the honest statement, and the one the documentation must carry: **an ACP
connection into the container is mediated by the editor, not by wraptool.**
This is the deliberate posture of R1, and §10.2 gives the reasoning for
accepting it rather than building the relay that would change it.

`-32601` is deliberately ambiguous on the wire — a deliberate policy refusal
is indistinguishable from a client that simply lacks the capability, which
also denies a nonconforming harness a cheap oracle for what the bridge does
and does not intercept. **That ambiguity must not propagate into the audit
trail.** Every refusal is logged at the refusal point regardless of the code
returned to the harness, or the probe leaves no trace at all. R1 logs
neither: §10.3 accepts the gap.

Stated in the modalities `docs/contributing.qmd#documentation-style`
requires: capability filtering in `initialize` **mitigates** — it removes the
temptation and keeps a conforming harness on the intended path; refusing to
forward **prevents** — a refused method cannot reach the editor over a
connection that goes through the bridge.

### 7.5 What the bridge composes: capability narrowing and a version ceiling

The editor's advertisement is not authoritative and must not be passed
through. The bridge composes the `initialize` request it forwards inward: it
takes the editor's params and **narrows** the capability object to what it
sanctions, dropping `fs`, `terminal` and `elicitation` regardless of what
arrived. Under the spec this is truthful — a client advertising `{}` is a
legal client, and omitted capabilities are binding as unsupported — so the
bridge is not lying to the harness, it is *being* a narrower client.
`clientInfo` should identify the bridge honestly rather than impersonate the
editor.

`protocolVersion` is the one field it is tempting to forward unchanged, on the
grounds that version negotiation is end-to-end (§3.3). It cannot be forwarded
unchanged. A version the bridge has not enumerated
brings a client-side method set the bridge cannot classify, which defeats
§7.2's allowlist by construction. **The bridge advertises the highest
protocol version whose client-method set it has enumerated (today: `1`) and
refuses to proceed above it**, closing the connection with a clear error
rather than relaying an unknown negotiation. Downgrading within the versions
it knows is fine; silently brokering a version it does not know is not.

### 7.6 The worktree is an editor-configuration channel

This is a container → host escape created by this feature, and it is the
sharpest residual risk in the design.

The worktree is shared read-write at the host path
(`--share=<root>=<root>`, `cmd/guixcontainer.go:622`), so the harness can
write any project-local file. ACP editors read project-local configuration
from that same tree — Zed's `.zed/settings.json` carries `agent_servers` with
exactly the `command` / `args` / `env` triple §1 describes. A prompt-injected
or malicious harness writes an `agent_servers` entry naming an arbitrary host
command; the next project open or agent restart executes it **on the host as
the developer**, with no container and no bridge in the path at all.

The same channel poisons the bridge's own execution. `env` in that
configuration sets the bridge's `PATH`, and the bridge resolves its
subprocesses by `PATH` — `exec.Command("guix", …)`
(`cmd/guixcontainer.go:104,177`), `exec.Command("devcontainer", …)`
(`cmd/up.go:1006,1025`), `exec.Command("git", …)`
(`cmd/up.go:1259,1291`). It also supplies `WRAPTOOL_CONFIG`, which
`runUp` reads from the environment (`cmd/up.go:98`).

Relatedly, the bridge runs host-side `git` inside the agent-writable
repository on every launch. That is finding F5's repo-local-config exec class
(`design/policy/security-audit-policy-enforcement.md:128-148`: `.git/config`
keys such as `core.fsmonitor`, `core.pager`, `core.sshCommand`, plus
`.git/hooks`) with a new automatic, editor-driven trigger instead of a human
typing `wraptool up` once.

Requirements this imposes on the bridge:

- Resolve `guix`, `devcontainer` and `git` to absolute paths, and refuse to
  run when any of them resolves to a path inside the worktree.
- Refuse to run when the bridge's own `argv[0]` resolves inside the worktree.
- Keep the three workspace-containment guards of §4 mandatory on the ACP
  path: the in-container guard (`cmd/up.go:81`),
  `validateConfigOutsideWorkspace` and `validatePathOutsideWorkspace`
  (`cmd/up.go:504-528`). These matter *more* under a bridge than under `up`,
  because the config path is read from `os.Getenv("WRAPTOOL_CONFIG")`
  (`cmd/up.go:98`) and the bridge's environment comes from editor
  configuration that can itself be project-local and agent-writable.
- Document, for users, that **any editor honouring project-local spawn
  configuration turns worktree write access into host execution**, and that
  ACP does not change the F5 posture on host-side `git` in an agent-writable
  repository.

### 7.7 The bridge is not a mandatory gate

Two bypasses, and they are different in kind.

**Developer-initiated.** Nothing stops a developer from configuring the
editor to spawn a harness's ACP entry point directly on the host — that is
three lines of editor configuration, and the upstream packages are ordinary
npm installs. In that arrangement there is no container, no bridge, no
wraptool: the harness runs on the host with the developer's credentials and
every client capability the editor offers. wraptool **does not address** this
and cannot. It is the same class of thing as running `claude` in a terminal
instead of running `wraptool up` — the product has never claimed to be a
mandatory gate on the developer's own machine. It is worth stating in user
documentation rather than papering over, because an ACP editor makes the
bypass *easier to reach by accident* than the terminal case does.

**Agent-initiated, over a channel the bridge does not own.** §2.1 defers
ACP's networked transport as out of scope *for wraptool*. Out of scope is not
the same as unavailable to the harness. The container shares the host network
namespace (§7.1), so if the editor exposes an ACP listener on loopback — the
same draft Streamable HTTP / WebSocket transport §2.1 defers — the harness
can open its own ACP connection to that editor directly, negotiate whatever
capabilities the editor advertises, and call `fs/read_text_file` and
`terminal/create` with the bridge entirely out of the path.

That is the complete answer to "who enforces": **the bridge, and only over
the connection it terminates.** Whether the target editors expose such a
listener today, and whether it can be disabled, is an open question (§14.1).
If it cannot be disabled, the Guix runtime's `--network` and ACP are in
tension, and that should be stated rather than assumed away.

wraptool does not **detect** either bypass. A relay (§10) could propose
audit events; under R1 nothing does, and §7.11 records what remains undetected.

### 7.8 Does an ACP session widen the MCP policy perimeter?

No. It is orthogonal, and the distinction should not blur.

The harness still reaches the wraptool server over the same MCP transport,
with the same Bearer token, scoped to the same worktree by the same `?cwd=`
parameter (`cmd/guixcontainer.go:87`, `internal/harness/harness.go:401-421`).
Deny-before-allow evaluation, flag constraints, credential scrubbing and the
tool-call audit log are all untouched. An ACP session cannot cause a
disallowed subcommand to run through wraptool.

What an ACP session adds is a *second* channel that the policy engine has no
view of at all: the ACP connection itself. Anything the bridge forwards on
that channel is outside policy by construction. This is precisely why §7.2's
allowlist is the whole design and not a hardening pass — the policy engine
cannot be asked to compensate for it later.

One adjacent rule: `session/request_permission` is a *turn-level* prompt
rendered in the editor and has nothing to do with wraptool's capability
approval, which is an operator decision made in the review UI. Nothing in
this design surfaces the review UI through an ACP connection.

The review UI's own reachability deserves an accurate statement rather than a
reassuring one. It binds loopback (`127.0.0.1:8718` by default,
`cmd/up.go:245-247`), but because `--network` shares the host network
namespace (§7.1), host loopback **is reachable from inside the container**.
The review UI is therefore network-reachable by the agent, gated only by the
admin token. `docs/terminology.qmd` calling it "never agent-facing" is a
statement about intended audience, not a reachability property, and must not
be cited as evidence of one. Compounding this: `enableAdminDefault`
(`cmd/up.go:245-247`) turns that listener on by default, so if `wraptool acp`
reuses `runUp`'s preamble, opening a project in an editor silently starts a
credential-holding server plus a review UI with no human at a terminal.
Whether an editor-triggered, non-interactive launch should auto-enable the
review UI at all is an open question (§14.2).

### 7.9 MCP wiring: keep the file, do not push the token

There are two ways the harness could learn where the wraptool server is.

Today, `wraptool up` writes the harness's MCP configuration on the host into
the workspace root (`cmd/guixcontainer.go:83-92`), rendering per harness
(`internal/harness/harness.go:231-353`) with the Bearer token in the file —
so the token already lives in a worktree file on the host, written mode 0644
(`internal/harness/harness.go:223`) into a git-tracked tree
(`design/audit/claude-audit.md` finding 5, still open). On the Dev Container
runtime the equivalent file is built and written *inside* the container by
the feature's `connect.sh` (`features/src/wraptool/connect.sh:130-148`).

The alternative is ACP's own push: the editor supplies an `McpServerHttp`
entry in `session/new` with an `Authorization` header. That would put the
wraptool Bearer token in the **editor's** settings on the host.

Recommendation: keep the existing file-based wiring unchanged, and have the
bridge strip editor-supplied `mcpServers` (§7.3). The reason is that
file-based wiring adds **no new credential location and no new trust
relationship** — not that a worktree file is better protected. It is not: the
worktree file is exactly what an editor indexes, searches, and feeds to its
own features, so attaching an editor adds a reader to that file. Because of
that, **0600 plus a gitignore entry for the rendered harness configuration is
a precondition of WP3**, not a separate cleanup.

### 7.10 Published claims this design changes

`docs/security.qmd` makes claims an ACP connection falsifies. Naming them
here so WP3's documentation scope is not guessed at:

- **"The MCP transport (SSE / Unix socket) is the only path across the
  boundary"** (the sentence following the trust-boundary diagram). An ACP
  connection is a second path across the trust boundary, in both directions.
  Amend the sentence.
- **The trust-boundary diagram** does not contain the editor at all — the new
  *most-trusted* component in the arrangement, and the one the harness gets
  to send requests to. Add the editor and the bridge as a second
  untrusted ↔ trusted edge.
- **"SSH keys, gcloud, kubeconfig, and the rest of the host environment never
  enter the container or the agent context"** (Scope). Qualify per §7.3: it
  remains true for credential *files* only because the developer would have
  to attach them deliberately in the editor.
- **Known limitations** gains a bullet: the ACP channel carries
  agent-initiated requests to the editor and is not covered by policy,
  credential scrubbing, or the tool-call audit.

### 7.11 Residual risk, stated plainly

**The forwarded permission prompt is this design's primary
attacker-controlled surface.** `session/request_permission` is the one method
that must be forwarded, its prompt text is harness-controlled, and prompt
injection — from file content, a dependency, an issue body — is inside the
threat model `docs/security.qmd` already publishes. It is tempting to call
this no worse than the harness's own terminal output today. It is worse.
Today the harness's prose lands in a terminal the developer reads
*as agent output*. Under ACP it lands in the editor's own permission modal —
UI chrome the developer is trained to read as the **editor** asking, usually
with a one-keystroke accept — and what the click authorizes is in-container
tool execution, which per `docs/security.qmd` ("Scope") can invoke the same
policy-allowed host tools the agent can. It is strictly worse than terminal
prose. Requirements: the bridge marks every forwarded prompt as
harness-authored in a way the editor can render (a bridge-owned prefix, or a
bridge-set `_meta` key the harness cannot overwrite — consistent with §7.2's
rule that harness-supplied `_meta` never passes through), and the
documentation states plainly that **a granted permission is recorded
nowhere** — R1 builds no audit for it (§10.3).

**Under R1, restated honestly.** The list below was written for the relay of
§10 and describes what that relay *would* have done. R1 builds none of it, so
the accurate summary is:

- **Prevents:** nothing about ACP message content. wraptool is not on the
  message path.
- **Mitigates:** only what the developer configures in the editor — the
  capabilities it advertises, and its permission prompts. Plus the one thing
  R1 does own: process-group teardown, so a connection close does not leak a
  token-holding container.
- **Detects:** nothing. An audit trail is not buildable without a host-side
  participant on the message path (§10.3, §12).
- **Does not address:** everything in the list below, unchanged.

What a relay would have prevented and mitigated, kept as §10.4's
specification:

- **Would prevent:** host command execution and host file read/write by the
  harness over an ACP connection that goes through the bridge, by forwarding
  only §7.2's two allowlisted messages.
- **Would mitigate:** a conforming harness attempting them at all, by
  narrowing the advertised capabilities in `initialize` and capping the
  negotiated protocol version (§7.5); credential injection from the editor, by
  stripping `session/new`'s `mcpServers` (§7.3); and worktree-sourced
  hijacking of the launcher's own subprocesses, by absolute-path resolution
  (§7.6 — this last one is worth keeping in WP1 regardless, since it costs a
  `LookPath` and defends the launcher itself, not the protocol).
- **Does not address:**
  - An editor pointed straight at a harness on the host with no container
    and no bridge (§7.7).
  - A harness opening its own ACP connection to an editor-side listener over
    the shared host network namespace (§7.7). The bridge enforces on one
    connection, not on the harness → editor relationship.
  - Worktree writes becoming host execution through project-local editor
    configuration (§7.6). This is created by the feature, not merely carried
    over.
  - A compromised bridge process, which runs on the host as the developer
    and is trusted absolutely by this design. Its argv, environment and cwd
    all originate in editor configuration, which can live in the
    agent-writable worktree (§7.6) — that is *how* it gets compromised.
  - Orphaned, token-holding containers. `cmd.Run()`
    (`cmd/guixcontainer.go:180`) has no process-group teardown; if the editor
    kills the bridge, the container can survive holding the MCP Bearer token
    and a writable worktree. Under `wraptool up` a human notices a dead
    terminal; under an editor, nobody does. **This survives R1 as a real
    requirement on WP1** — the launcher kills the container's process group on
    exit and on parent pipe close — and it is the one §7 finding that R1
    answers in code rather than in documentation, because no editor
    configuration and no warning can clean up a leaked process.
  - Cross-project state through the shared pool `home/`, read-write in every
    container (`cmd/guixcontainer.go:626`; `docs/security.qmd`'s Known
    limitations already lists it). ACP makes long-lived, editor-owned harness
    processes the normal case rather than the exception, so concurrent
    sessions sharing that directory become the default rather than an
    occasional overlap — and the number of simultaneously live MCP tokens and
    containers per developer multiplies with it.
  - The path-rewriting gaps of §6 on the Dev Container runtime, which are a
    correctness risk before they are a security one.

## 8. Harness support

ACP entry points divide into three shapes, and only the first is clean.

| Harness | Shape | Entry point | Notes |
|---|---|---|---|
| `opencode` | Native subcommand | `opencode acp` | Same product the pool already locks as `opencode-ai` (`internal/harness/harness.go:164`). Documented gap: `/undo` and `/redo` are unsupported over ACP |
| `gemini` | Native flag | `gemini --acp` | The older `--experimental-acp` spelling is retained upstream but explicitly deprecated. **Not pool-lockable today**: registered with `Bin: "gemini"` and neither `Install` nor `PoolPackage` (`internal/harness/harness.go:165`), so `Descriptor.PoolLockable()` (`internal/harness/harness.go:94`) is false |
| `claude` | Separate adapter package | `@agentclientprotocol/claude-agent-acp`, bin `claude-agent-acp` | The `claude` CLI itself has no ACP flag or subcommand — verified against a live `claude --help`. **Do not spell `claude --acp`.** The adapter is versioned independently and does not depend on the package the pool locks (`internal/harness/harness.go:161-162`) |
| `pi` | Separate adapter package | `pi-acp`, third-party, 0.0.x | Pins an older ACP SDK than the claude adapter. This is a **supply-chain** acceptance decision, not a stability judgement: see §8.1.1 |
| `agy` | None | — | Absent from the ACP agent registry; upstream request open with no implementation. Community adapters exist but pull native modules. **Treat as ACP-unsupported** |
| `cursor`, `roo` | Out of scope | — | Host IDE integrations, empty `Bin` (`internal/harness/harness.go:163,171`) |

Two upstream naming traps worth recording: the claude adapter has been
renamed twice and the older npm names carry deprecation markers, so any
descriptor must name the current one; and `opencode`'s upstream repository
moved publishers while version numbering stayed continuous, so the pool
identity `opencode-ai` is unaffected.

### 8.1 Where a separate adapter package collides with the pool

The pool models exactly one package and one binary per harness. Every layer
assumes it:

- `LockHarness` has a single `Package` and a single `Version`
  (`internal/pool/lock.go:33-41`), and the manifest carries one constraint
  per harness name.
- `UpdateOptions` carries one `Package` and one `BinName`
  (`internal/pool/orchestrator.go:309-328`).
- npm realization hard-requires the harness package to be the lockfile's
  top-level entry and errors otherwise
  (`internal/pool/source/realize.go:165-167`), then generates exactly one
  wrapper at `<generation>/bin/<binName>`
  (`internal/pool/source/realize.go:773`).
- Archive realization places exactly one `bin/<binName>` and refuses more
  than one candidate (`internal/pool/source/archive.go:1026,1058-1094`).
- Generation identity is `(harness, version, platform, digest)` with one
  version string (`internal/pool/receipt.go:208-223`), and the activation
  pointer is one file per harness, `active/<key>/<harness>.gen`
  (`internal/pool/layout.go:117-119`).

There is also a latent mismatch that an ACP entry point would expose.
The generated dispatcher execs `bin/$name` where `$name` is the *harness
name* (`internal/pool/dispatcher.go:38`), while realization writes the
wrapper at `bin/<Descriptor.Bin>` (`internal/pool/source/realize.go:773`).
Today `Name == Bin` for every CLI harness
(`internal/harness/harness.go:143-167`) so the divergence is invisible — but
a second binary, or a binary whose name differs from the harness name, cannot
be reached through the dispatcher without resolving this first.

Consequently, for `claude` and `pi` there are only two honest options:

1. **A second registry entry** — e.g. a `claude-acp` descriptor with its own
   `Name`, `Bin`, `PoolPackage`, manifest constraint, lock entry, generation
   and pointer. Isolation for free, at the cost of a duplicated version
   constraint and a second thing to keep current — and, non-negotiably, of
   pinning the adapter to the pool's own locked harness binary (§8.1.1).
2. **Extend the pool to a package set per harness** — a real change to
   `LockHarness`, `UpdateOptions`, and multi-binary wrapper generation. Much
   larger, and it touches the lock format this note declared out of scope.

Recommendation: option 1, and ship `opencode` first — native subcommand, no
adapter, already pool-lockable — so that the launcher, the output hygiene
and the capability filtering are proven before the packaging question has to
be answered at all.

#### 8.1.1 Adapter pinning is a supply-chain requirement, not a packaging preference

The claude adapter resolves its own `claude` binary from platform- and
libc-specific **optional npm dependencies** at runtime, unless
`CLAUDE_CODE_EXECUTABLE` points elsewhere. Left alone, that means the pool's
lockfile, its `minimum_release_age` quarantine and its generation receipts
govern the *adapter* while something floating governs the thing that actually
executes. Those mechanisms exist precisely for the compromised-upstream case
`docs/security.qmd`'s threat model leads with, so this is a control bypass,
not a versioning annoyance.

> **Requirement.** Any ACP adapter descriptor must pin the adapter to the
> pool's own locked harness binary — for the claude adapter, by setting
> `CLAUDE_CODE_EXECUTABLE` to the active generation's binary. **An adapter
> that cannot be pinned is not poolable and does not ship.**

Install-time blast radius is separately contained on a Guix host, because
`npmInstall` runs inside `guix shell --container`
(`internal/harness/harness.go:131-137`) — so the concern here is what
executes at runtime, not what runs in a `postinstall` hook.

`pi-acp` is a third-party 0.0.x package on the same runtime path. Before it
ships, state the review bar a third-party adapter must clear: named
maintainer and provenance, a pinnable harness-binary reference, a pool lock
entry subject to the same quarantine as every other locked artifact, and no
native modules pulled at install time. If it cannot clear that bar, `pi` is
ACP-unsupported, the same ruling §8 gives `agy`.

## 9. The Descriptor / registry change

`Descriptor` (`internal/harness/harness.go:57-81`) has fields for the
executable name, the legacy installer argv, the pool source identity and the
MCP-configuration renderer. It has **no** field for how to launch the harness
*inside a project container* — nothing in wraptool has ever needed one. The
exec inventory in `cmd/` reaches `docker`, `guix`, `devcontainer`, `git`, a
wraptool respawn, and the harness *installer* (`cmd/harness.go:1124-1128`);
never the harness itself.

The installer is the precedent to copy from rather than invent against:
`Descriptor.Install` is already an argv template with a `{prefix}` token
substituted at run time and executed inside a `guix shell --container`
(`internal/harness/harness.go:131-137`). The addition below is the same idea
applied to the project container.

Sketched as a seam, not an implementation:

```go
// ACP describes how to start this harness's ACP entry point inside a
// container. Nil means the harness cannot serve an ACP connection and
// `wraptool acp <name>` refuses it by name.
type ACP struct {
    // Argv is run inside the container, argv[0] resolved on the
    // container PATH (the pool dispatcher directory is appended to it,
    // cmd/guixcontainer.go:608). e.g. {"opencode", "acp"}.
    Argv []string
    // Adapter names a second pool-lockable registry entry that provides
    // Argv[0] when it is not this harness's own Bin (§8.1 option 1).
    Adapter string
    // Env pins the adapter to the pool's own locked harness binary
    // (§8.1.1) — e.g. CLAUDE_CODE_EXECUTABLE. A descriptor with an
    // Adapter and no pinning Env does not ship.
    Env map[string]string
}
```

Consumers the field has to reach: the runtime dispatch and flag validation
(`cmd/up.go:126,171-174`), the Guix entry builder
(`cmd/guixcontainer.go:597-611`) and argv builder
(`cmd/guixcontainer.go:618-633`), and the Dev Container exec builder
(`cmd/up.go:888-894`).

Three places outside the registry hardcode their own harness list and would
drift: `cmd/init_devcontainer.go:107,123`, the feature's
`options.harness.enum` in
`features/src/wraptool/devcontainer-feature.json`, and the case statement in
`features/src/wraptool/connect.sh:110-116`. None of them needs to know about
ACP in the first release — the bridge does not scaffold configuration — but
the duplication is worth recording as a reason to keep the ACP surface out of
the feature.

One existing gate applies unchanged: `harness.Write` refuses a harness whose
`AuthMode` is the zero value when a token is present
(`internal/harness/harness.go:198-202`). Any new ACP-capable descriptor must
set `AuthHeader` or it is unusable under `up`, and therefore under the
bridge.

## 10. Rejected for the first release: the mediating bridge

§7 makes a strong case that an ACP connection hands the harness a channel the
container was built to deny. The obvious response — and the one an earlier
revision of this note made normative — is a host-side relay that owns the
pipes, narrows the `initialize` handshake and refuses non-allowlisted methods
at the wire (§4.1, §7.2, §7.4, §7.5). This section records why that is
**rejected for the first release**, because a rejected alternative with its
reasoning is worth more later than a silently dropped one.

### 10.1 What the relay would cost

It is not a wrapper around the launcher; it is the larger half of the feature.
A correct relay needs a JSON-RPC framing layer, per-session ordering with a
single writer, backpressure, `$/cancel_request` bookkeeping, a method
allowlist that must track a moving spec, a protocol-version ceiling, and
unknown-field handling that is asymmetric by direction. All of it sits
directly on the message path, so a defect is a hang, a dropped notification,
or a silently broken session. And all of it is ACP-specific: it makes wraptool
a participant in a protocol it otherwise has no opinion about, with a
maintenance obligation that follows the spec's release cadence rather than
wraptool's.

Against that, the primitives R1 needs are an entry argv and a print sink.

### 10.2 Why the security argument does not carry it

The relay's case rests on it being a control the developer cannot get wrong.
Three things undercut that.

**It is bypassable by construction, and cheaply.** §7.7 already establishes
that a developer can point the editor straight at the harness — the relay is
one line of editor configuration away from being out of the path. A control
that the person it protects can remove by accident, while everything appears
to work, is a weak control. Worse, it is a *misleading* one: a documented
guarantee that silently does not apply to a configuration the developer
believes is equivalent.

**It may not survive contact with the harnesses.** §14.1 records an
unverified, load-bearing assumption: what real harnesses do when a client
advertises neither `fs` nor `terminal`. The spec says they should cope. If
they instead degrade badly, the relay's central mechanism — narrowing the
handshake — is unusable, and the work is wasted. Building the expensive thing
before answering that question is the wrong order (§13).

**The mediation the developer actually relies on is the editor's.** The
consequential client-side calls are exactly the ones `session/request_permission`
was designed to gate, and the mainstream editors do put a human in front of
them. A wraptool relay would be a second, weaker approval surface, with less
context than the editor has and no UI of its own.

None of this makes §7's risks go away — §10.3 is explicit that R1 accepts
them. It makes the relay a poor instrument for reducing them.

### 10.3 What R1 accepts, plainly

Under R1, and stated without softening:

- A harness inside the container can ask the editor to run commands **on the
  host**, and the editor may do it. The container boundary does not stop this;
  only the editor's permission prompt does.
- A harness can ask the editor to read and write files outside the shared
  worktree, subject to the same prompt and to whatever the editor scopes.
- Nothing wraptool logs records that any of this happened. The tool-call audit
  covers the MCP path only (§7.8); an ACP session is invisible to it.
- The `docs/security.qmd` claims §7.10 identifies become **conditionally
  wrong** — accurate for a shell-in-container session, not for an ACP one.

Accepting these is defensible because wraptool's perimeter is credential
isolation, and the editor is the developer's own trusted program running as
the developer. It is *not* defensible to accept them silently. §12's
documentation work is therefore not an optional companion to the code; it is
the deliverable that makes R1 honest, which is the argument of §2.1 arriving
from the other direction.

### 10.4 What would reopen it

- A capability test (§13) showing harnesses cope cleanly with narrowed
  capabilities, **and** a concrete report of an agent-initiated host action
  through an ACP editor.
- ACP v2 landing a capability model that is enforceable rather than
  advisory (§14.1).
- A deployment where the developer is not the person accepting the risk —
  a shared or managed environment — since §10.2's "the developer can bypass
  it anyway" argument does not hold when the developer is not the operator.

## 11. Lifecycle — one container per what?

ACP sessions are per-connection, not per-process: one harness process hosts
many sessions (§3.4). So the unit that matters is the **connection**, which
is the process the editor spawned, which is the bridge.

**Guix runtime: one container per ACP connection.** There is no alternative
worth designing around — a `guix shell --container` lives only for the
duration of its command, which is why the existing `shell == false` path
warms the profile and returns rather than leaving anything running
(`cmd/guixcontainer.go:99-111`). The container's lifetime is `cmd.Run()`'s
(`cmd/guixcontainer.go:180`), which is exactly the connection's lifetime —
subject to the process-group teardown requirement of §7.11, without which the
container can outlive the bridge. What is shared across projects and across
connections is the *server* and the *pool*, not a container. Two editors on
one worktree means two containers sharing the same workspace path and the
same pool `home/` read-write (`cmd/guixcontainer.go:626`) — the same
concurrency a developer already creates by running `wraptool up` in two
terminals, but now the default rather than the exception, which is why §7.11
lists the pool `home/` channel as amplified by this feature and not merely
inherited. The spike should confirm the harnesses tolerate it.

**Dev Container runtime: attach to the persistent container.** Here a
container genuinely persists across `up` invocations, and `wraptool down`
already stops it by the `devcontainer.local_folder` label
(`cmd/down.go:85-99`). `devcontainer exec` is already used for the
interactive attach (`cmd/up.go:888-894,1025-1030`) and is the cheaper
lifecycle: `devcontainer up` runs once, and each ACP connection is an exec.
The blocking unknown is whether `devcontainer exec` passes the child's
stdout through byte-for-byte or line-buffers/multiplexes it — a JSON-RPC
channel does not survive either. If it does not, the fallback is `docker
exec -i` against the label-resolved container ID, using the same lookup
`cmd/down.go:85-99` already performs. §14.2.

Recommendation: **Guix runtime first, one container per connection**, on the
strength of §6's path identity. Dev Container support follows once the exec
transparency question and the path-rewriting design are settled.

## 12. Delivery order

Under R1 the code is two small work packages and the documentation is the
third. WP1 and WP2 are independently useful and neither mentions ACP; WP3 is
the deliverable §2.1 argues for and is the only place ACP appears at all.
Nothing here ships before §13's capability tests pass, because every one of
these packages assumes an answer §13 supplies.

**WP1 — entry argv.** `buildContainerEntry` takes an entry argv instead of
hardcoding the login shell; `upOptions.shell` becomes a mode rather than a
bool; the runtime entry points take the mode. Touches `cmd/up.go`
(`:28-37,80,169-174,182-191,877-894,898`), `cmd/guixcontainer.go`
(`:71,597-611,618-633`), and the pins in `cmd/up_test.go:48-59,711-719` and
`cmd/guixcontainer_test.go:190,750,794,852`. No behaviour change for existing
commands.

Two constraints from §4.1 and §4.2 carry test pins. The non-interactive entry
path hands the child the inherited descriptors unwrapped — there is no
wrapping layer to accidentally introduce. And the argv path must not route
through `wantShell`'s TTY-driven `auto` branch (`cmd/up.go:189`), which
resolves to `false` under an editor's pipes and, on the Guix path, never
enters a container at all (`cmd/guixcontainer.go:99-111`).

**Teardown is not "kill the child".** §13.1's T5 established empirically that
`guix`'s guile wrapper — wraptool's own direct child — survives `SIGTERM`,
along with the inner `guix` process and everything inside the container. So
WP1 sets `SysProcAttr.Setpgid` on the container command and signals the
negated process-group id on exit and on parent pipe close, escalating to
`SIGKILL` after a grace period. Signalling `cmd.Process` alone leaves exactly
the orphan §7.11 describes: a container holding the MCP Bearer token and a
writable worktree, with no terminal for a human to notice it in.

The surface question — a `--exec` flag on `up`, a trailing `--` argv, or a
separate subcommand — is deliberately left open (§14.2). It should be chosen
for the general primitive, not for ACP.

**WP2 — output hygiene.** Route the 23 print sites of §5.1 through an injected
writer and repoint them when the entry mode is non-interactive; handle the
login-shell echo of §5.4 and the profile-realisation output of §5.3. Touches
`cmd/up.go`, `cmd/guixcontainer.go`, `cmd/server.go`.

One line is stronger than a redirect. `cmd/up.go:154` prints the review-UI URL
**with the admin token embedded**; on a non-interactive launch an editor
persists the child's stderr to a log file, so that line must emit no secret on
*any* stream, not merely a redirected one.

Worth landing on its own merit: today a scripted `wraptool up` interleaves
prose with whatever it runs.

**WP3 — the documentation.** The deliverable §2.1 argues for, and under R1 the
only control surface that exists (§10.2). A `docs/` page carrying:

- The reasoning of §2.1 — why an editor on the host driving a harness in the
  container is the product thesis, not a niche integration.
- A worked example for one editor, with the exact configuration, built on
  whatever §13 actually established. Not written before then.
- **The §7 exposures, in the developer's language.** At minimum: the harness
  can ask the editor to run commands on the host and to touch files outside
  the worktree, the editor's prompt is the only thing between them and the
  developer, and none of it appears in wraptool's audit log (§10.3).
- §7.7's bypass, and §7.6's warning that an editor honouring project-local
  spawn configuration turns worktree write access into host execution.
- **`docs/security.qmd`**, per §7.10: qualify the "only path across the
  boundary" sentence and the host-credential claim to say what they assume,
  and add a Known Limitations bullet. The trust-boundary diagram gains the
  editor as a second edge. Under R1 the honest framing is a documented
  exposure the developer accepts, not a boundary wraptool enforces.
- The `ACP client` row in `docs/terminology.qmd` (§1.2). The `bridge` row is
  **not** added — R1 builds no bridge, and naming one would imply mediation
  that does not exist.

**Deferred, not scheduled.** The launch descriptor of §9 (`Descriptor.ACP`)
buys nothing until more than one harness is documented — until then the argv
lives in the documentation, where it is one line the reader can adapt.
Observability (a bridge-emitted audit trail) is not buildable under R1 at all:
with no host-side participant on the message path, there is nothing positioned
to observe an ACP session, which is why §10.3 lists the audit gap as accepted
rather than deferred. Dev Container support waits on §6 and §13.

## 13. Capability tests — what to establish before documenting

R1 puts the whole feature on one empirical claim: **the stdio path into the
container is transparent enough that a protocol wraptool knows nothing about
survives it.** That claim is currently unverified. It cannot be verified from
this checkout — a container running the harness has neither `guix`, `docker`
nor `devcontainer` on `PATH` — so these run on a developer host, and they run
*before* the documentation is written, not after.

The tests are deliberately protocol-free. None of them needs an editor, an
ACP harness, or any of §14's unresolved protocol questions answered. They
establish the primitive; ACP is then a consequence.

**T1 — byte transparency, Guix.** Run a program inside the container whose
stdout is known exactly — a fixed byte sequence including NUL bytes, a long
line with no trailing newline, and a burst larger than one pipe buffer — with
stdout redirected to a file, and compare byte for byte. Establishes whether
`guix shell --container` and the intervening `bash -lc` alter, buffer, or
line-wrap the child's stdout. §5.3 and §5.4 both assume they can be made not
to; this is the assumption under them.

**T2 — stream separation.** With the same launch, confirm every wraptool prose
line and every `guix` progress line lands on stderr and nothing else lands on
stdout. This enumerates §5.1's print sites empirically instead of by grep, and
it is the test WP2 is complete against.

**T3 — bidirectional interactive round-trip.** Drive a line-oriented echo
program inside the container from a parent process over pipes — not a TTY —
writing and reading concurrently. Establishes that stdin reaches the child
promptly and unbuffered, which JSON-RPC request/response depends on and which
a redirect-to-file test cannot show.

**T4 — no TTY.** Run T1–T3 with stdin and stdout as pipes. §4.2 notes the
`auto` branch is TTY-driven; this confirms nothing else on the path silently
requires a terminal.

**T5 — lifetime and teardown.** Confirm the container process exits when the
parent closes the pipes, and leaves no process behind. An editor closing a
connection is the normal case, not an error case.

**T6 — Dev Container exec transparency.** T1–T5 against `devcontainer exec`,
answering §14.2's question directly. A failure here scopes the first release
to the Guix runtime; it does not block it.

Only after T1–T5 pass is it worth running the protocol-level question — a real
editor against a real harness — and only that result licenses a worked example
in WP3. If T1 or T3 fails, R1 is not viable as stated and the login shell or
the container runtime, not the documentation, is what has to change.

### 13.1 Results — 2026-08-14, Guix host

Run against this worktree's `manifest.scm` on a developer's Guix host, driving
`guix shell --container` directly with the argv of `buildGuixShellArgs`
(`cmd/guixcontainer.go:618`). **R1's premise holds.**

| Test | Result | Evidence |
|---|---|---|
| T1a byte transparency, `bash -lc` | **pass** | `cat` of a multi-megabyte Go binary through the container: md5 identical to the host's |
| T2a stream separation | **pass** | same run, output piped to `md5sum` — any injected byte would change the hash. Nothing from `guix` or the login profile reached stdout |
| cold-cache variant | **pass** | `--rebuild-cache` with stderr discarded: hash unchanged. See caveat below |
| T3 round-trip | **pass** | 5 request/response pairs over a coprocess: intervals 305/306/306/306 ms against a 300 ms send delay, so **round-trip ≈ 5 ms** |
| T4 no TTY | **pass** | T3 ran with pipes on both ends |
| T5 teardown | **fail** | see below — this is the expected failure, and it is a WP1 requirement, not a threat to R1 |
| T6 Dev Container | not run | needs a docker host |

**What this settles.** Stdout is byte-transparent and stdin reaches the child
promptly; a protocol wraptool has no knowledge of survives the trip intact and
interactively. The entry-argv primitive of §12 is sufficient for ACP, and the
relay rejected in §10 is not needed to make the connection *work* — only, had
it been kept, to police it.

**Two things it does not settle.**

*The cold-store case.* `--rebuild-cache` invalidates guix's profile cache, not
the store. With every package already built, no substitute download or compile
occurred, so this is strong evidence that guix's own output goes to stderr —
not proof. A first-ever run on a fresh machine remains untested, and §5.3's
warm-up should not be deleted on this evidence alone, only demoted from
required to probably-unnecessary.

*T1b under `bash -c`.* Not separately run. It does not matter: T1a passed
under `bash -lc`, the spelling shipped today, so the login profile emits
nothing on stdout on this host and §5.4 needs no fix. Dropping `-l` becomes an
option rather than a requirement (§14.2).

**T5, and what it actually showed.** Worse than "no process-group teardown".
The surviving processes included **the direct child that was signalled** —
`guix`'s guile wrapper survived `SIGTERM`, along with the inner `guix` and the
`sleep` inside the container. So a launcher that kills its immediate child on
exit is not sufficient: WP1 must set a process group (`SysProcAttr.Setpgid`)
and signal the negated group id, with escalation to `SIGKILL`. Confirmed by
inspection that no `Setpgid` exists anywhere in the tree today.

This is the §7.11 orphan risk in concrete form: under an editor, a closed
connection would leave a container running with the MCP Bearer token and a
writable worktree, and no human at a terminal to notice.

## 14. Open questions — what a spike must answer first

### 14.1 Protocol and client behaviour

R1 answers none of these in code. They survive as things the documentation
(WP3) must either state or explicitly leave unknown.

- Where does Zed spawn the harness process for a remote/SSH or dev-container
  project — locally, or on the remote host? Zed's documentation says only to
  "check the specific agent's setup path" for those cases. If the spawn
  relocates, §6's path identity changes and the worked example may not hold.
- **Can the developer turn off the editor's `fs` and `terminal` capabilities?**
  Under R1 this replaces "what should the bridge narrow" as the operative
  question, and it is the one mitigation the documentation can actually offer
  for §10.3's first two bullets. Per editor.
- **Does the target editor expose an ACP listener on loopback today, and can
  it be disabled?** §7.7's second bypass: with a shared network namespace, a
  harness can reach an editor-side listener directly. Under R1 there is no
  bridge to bypass, so this is no longer an enforcement hole — but it remains
  a channel from the container to the editor that neither the developer nor
  wraptool is watching, and it belongs in the documentation if it exists.
- What do the real harnesses do when a client advertises **no** `fs` and
  **no** `terminal`? The spec says they should support every capability
  combination. Under R1 this is no longer load-bearing for the design — the
  harness gets whatever the editor advertises — but it is exactly the question
  §10.4 makes a precondition for reopening the relay, and it bounds how much
  the mitigation above is worth.
- How does the ACP v2 draft change the client filesystem/terminal capability
  model? There is an RFD specifically on it. R1's exposure to a spec change is
  much smaller than a relay's would be — wraptool parses nothing — but a v2
  that changes what an editor grants by default changes §10.3's accepted
  risks.

Dropped from this list by R1: cross-session ordering and `$/cancel_request`
bookkeeping (no relay, so no ordering to preserve), and whether `session/list`
/ `session/load` can be scoped by recorded `cwd` (nothing scopes them; §7.3's
cross-project session channel is documented, not closed).

### 14.2 Runtime mechanics

Most of this group is now answered by running §13's tests rather than by
research; the pointer is to the test that settles each one.

- **What is the surface of the primitive?** A `--exec` flag on `up`, a
  trailing `--` argv, or a separate subcommand. Not decided here on purpose:
  §2 argues the primitive is general, so the spelling should be chosen for
  scripting and CI as much as for an editor. It should not contain the string
  `acp`.
- Does `devcontainer exec` pass the child's stdout through byte-for-byte, and
  does it allocate a PTY based on its own TTY detection? **T6.** If it
  multiplexes or line-buffers, the Dev Container path goes to `docker exec -i`
  instead.
- Does `devcontainer up` offer a way to suppress its stdout progress and
  terminating JSON result object? If not, `cmd/up.go:1008` is simply
  repointed at stderr. **T2/T6.**
- ~~Does `guix shell` itself write anything to stdout on the entry path?~~
  **Answered (§13.1): no.** Its output goes to stderr, including under
  `--rebuild-cache`. §5.3's warm-up is demoted from required to probably
  unnecessary — the cold-store case is still untested.
- ~~Can the login shell be dropped (`bash -c` instead of `bash -lc`)?~~
  **Moot (§13.1).** T1a passed under `bash -lc`, so the login profile emits
  nothing on stdout and §5.4 needs no fix. Dropping `-l` is now an option, not
  a requirement. (The pool PATH append lives in the entry string itself,
  `cmd/guixcontainer.go:608`, not in the profile, so a harness on `PATH` never
  depended on `-l` either.)
- **Should a non-interactive launch auto-enable the review UI at all?** Today
  `enableAdminDefault` (`cmd/up.go:245-247`) turns on a credential-holding
  listener reachable from inside the container over the shared network
  namespace (§7.8), with no human at a terminal to notice. Under R1 this is
  sharper, not softer: WP2 stops the token being *printed*, but the listener
  still runs. A default of off for non-interactive launches is the obvious
  answer and needs a ruling.
- Do two concurrent containers sharing the same worktree and the same pool
  `home/` read-write (`cmd/guixcontainer.go:626`) interfere — harness lock
  files, session state, credential refresh?

### 14.3 Packaging

- Should `gemini` be made pool-lockable (`PoolPackage: "@google/gemini-cli"`)
  as part of this work? It is the cleanest native ACP entry point but is
  currently not lockable at all (`internal/harness/harness.go:165`).
- Does the pool's npm realization pick the correct glibc variant for the
  claude adapter's platform- and libc-specific optional dependencies under
  Guix's FHS emulation, given the existing platform workaround at
  `internal/harness/harness.go:131-137`? (Whether the adapter is pinned to
  the pool's own binary is **not** open — §8.1.1 rules it a requirement.)
- Should the dispatcher/realize binary-name mismatch
  (`internal/pool/dispatcher.go:38` versus
  `internal/pool/source/realize.go:773`) be unified *before* any ACP entry
  point with a differing binary name is introduced?
- Is the published ACP agent registry — which already carries pinned
  versions, per-platform archives, digests and launch argv — worth consuming
  as a pool metadata channel, rather than hand-maintaining launch argv per
  harness? It is shaped remarkably like `Descriptor`'s existing npm/archive
  split (`internal/harness/harness.go:68-79`). Consuming it is itself a
  supply-chain decision and must clear §8.1.1's bar.

### 14.4 Product

- Is `agy` simply ACP-unsupported until upstream implements it, or is a
  third-party adapter with native module dependencies acceptable? This note
  recommends the former, and §8.1.1 sets the bar a third-party adapter would
  have to clear.
- Is the Dev Container runtime in scope for the first release at all, or is
  ACP a Guix-runtime feature until §6's translation problem has a design?
