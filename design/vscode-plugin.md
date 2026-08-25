# Design: VS Code extension for operator request review

Status: **proposed**. Feasibility sketch for a VS Code extension that surfaces
wraptool capability requests inside the editor — the operator's most common
context — and lets them approve or deny without switching to the browser.
Companion to `webui/webui-accessibility-reachability.md`, which rejected a
desktop-style wrapper (§4) and favored a TUI (§5); an editor extension is a
different trade: the operator is *already* in VS Code, so there is no new
window to manage, and the extension can piggyback on VS Code's own
notification and port-forwarding machinery.

## 0. Verdict

Feasible, with one precondition: wraptool needs a small JSON admin API first.
The extension itself is routine VS Code plumbing (TreeView + notifications +
a few commands). The recommendation is to build the JSON API in this
repository and the extension in a **separate repository** — see §5.

## 1. What exists today

The serve process can run an operator review UI on `admin.listen`
(`internal/webui`), guarded by a Bearer token (`admin.auth_token_file`).
The `AuthMiddleware` already accepts `Authorization: Bearer` for scripted
clients, so authentication for a machine client is solved.

What is *not* solved:

- **Every endpoint speaks HTML.** `GET /requests`, `GET /rules`, `GET /calls`
  render templates; mutations are HTML form POSTs
  (`POST /requests/{id}/approve`, `POST /rules/{tool}/{kind}/...`). A machine
  client would have to scrape templates — workable but brittle, and template
  changes would silently break the extension.
- **No push channel.** The Web UI is refresh-driven. An extension whose whole
  point is "a request came in, look up from your code" needs either an event
  stream or cheap polling.
- **The CLI has no machine output.** `wraptool requests list` prints a human
  table; there is no `--json`.

The underlying data model is already ideal for this: `requestlog.Store` is an
append-only JSONL event stream, and request status is a pure projection over
it. Nothing about the storage or the approve/deny semantics needs to change.

## 2. Precondition: a JSON admin API (this repository)

Add a versioned JSON surface next to the HTML one, same listener, same
`AuthMiddleware`, same underlying `requestlog.Store` + `config.ApplyRequest`
calls the HTML handlers use:

```
GET  /api/v1/requests?status=pending      list requests (the projection)
POST /api/v1/requests/{id}/approve        body: {apply: bool, flag_types: {...}}
POST /api/v1/requests/{id}/deny           body: {reason: string}
GET  /api/v1/events                       SSE: request lifecycle events
GET  /api/v1/calls                        tool-call audit (read-only)
GET  /api/v1/rules                        rules snapshot (read-only, phase 2)
```

Notes:

- **SSE, not WebSocket.** The serve process already speaks SSE on the MCP
  side; `net/http` does it with no new dependency, and the extension client
  (`fetch` + `EventSource` semantics) is trivial. The event payloads are the
  existing `requestlog` event types (`capability_requested`, `approved`,
  `denied`) — the JSONL line, re-emitted. Polling `GET /requests` every few
  seconds is an acceptable degraded mode and the extension should fall back
  to it automatically.
- **The API is a thin veneer.** Handlers must share the exact code paths the
  HTML handlers use (list projection, `ApplyRequest`, optimistic-concurrency
  rules editing) so the two surfaces cannot drift in behavior. Refactor the
  HTML handlers to call the same internal functions rather than duplicating.
- **Versioned under `/api/v1/`** so the extension can pin against it. The
  contract lives in this repository (this document, plus Go types); the
  extension vendors nothing.
- Independently useful for scripts, any future remote client, and `requests
  list --json` if that flag is ever wanted. The TUI proposed in
  `webui-accessibility-reachability.md` §5 is *not* a consumer: it is
  in-tree and deliberately file-direct (no HTTP, no token), and stays that
  way.

## 3. The extension (separate repository)

Standard TypeScript VS Code extension. No wraptool code runs inside it — it
is purely an HTTP client of the admin API.

**Surfaces:**

- **Activity-bar TreeView** listing pending requests (tool, subcommand,
  requesting client, age). Approved/denied history as collapsed sections.
- **OS-level notification** (`window.showInformationMessage` with action
  buttons) when a `capability_requested` event arrives: *Approve*,
  *Approve + apply*, *Deny...*, *Open details*. This is the core value over
  the Web UI — zero-latency awareness while coding.
- **Details webview** for one request: the requested argv, proposed
  `flag_types`, and the diff `--apply` would make to the config. Reuses the
  Web UI's information layout, not its templates.
- **Status-bar item**: pending-request count, click to open the TreeView.
- Phase 2: read-only rules browser; full rules CRUD stays in the Web UI
  (its optimistic-concurrency editing flow is form-heavy and duplicating it
  in a webview is cost without benefit).

**Configuration:** `wraptool.adminUrl` plus the token. The token must go in
VS Code `SecretStorage`, never `settings.json` (which is routinely committed).
A sign-in command prompts once and stores it. Optionally, auto-discovery:
if the workspace has a wraptool config and the admin listener is local, offer
to read `admin.auth_token_file` directly.

**Remote scenarios** (the reason an editor extension beats a browser tab):

- *Devcontainer / Remote-SSH:* the extension runs in the **UI extension
  host** (declare `"extensionKind": ["ui"]`), so it talks to the operator's
  host network, where `admin.listen` on `127.0.0.1` actually lives. Running
  it in the workspace host would put it inside the very container wraptool
  isolates, where the admin listener is deliberately unreachable — that is
  the isolation working, not a bug to solve. The agent-facing container must
  never gain a path to the approval surface.
- If the serve process runs on a remote dev box, VS Code port forwarding of
  the admin port covers it with no wraptool changes.

**Security posture:** the extension is an *operator* tool, exactly like the
Web UI. It must never be installed or surfaced inside the sandboxed harness
environment. Bind guidance stays `127.0.0.1`. The extension adds no new
attack surface beyond what the Bearer token already grants; the main risk is
token leakage, which `SecretStorage` addresses.

## 4. Effort estimate

- JSON API + SSE endpoint in `internal/webui`: small — the logic exists, the
  work is extraction and JSON encoding, plus handler tests. No new
  dependencies.
- Extension MVP (TreeView, notifications, approve/deny, SecretStorage,
  polling fallback): a few days of routine extension work; the VS Code APIs
  involved are stable and well-documented. Marketplace + Open VSX publishing
  is boilerplate CI.

No blockers found. The one design decision with real consequences is the
API contract; everything on the extension side is reversible.

## 5. Separate project or in-tree?

**Recommendation: separate repository** (e.g. `wraptool-vscode`), with the
API contract owned here. Reasons:

- **Toolchain mismatch.** wraptool is pure Go with "no dynamic language
  runtimes" as a stated dependency posture; an extension drags in
  TypeScript, npm lockfiles, `vsce`/`ovsx`, and Marketplace publishing CI.
  Keeping that out of this repository keeps `manifest.scm`, the lint jobs,
  and the release story untouched.
- **Different release cadence and channel.** The extension ships to the VS
  Code Marketplace / Open VSX on its own version line, pinned to `/api/v1`,
  not to wraptool releases. A versioned API is exactly the decoupling point
  a repo boundary should sit on.
- **The lockstep argument is weak here.** In-tree pays off when client and
  server evolve one commit at a time against an unstable interface. `/api/v1`
  is deliberately stable and tiny; the extension consuming it from outside is
  the normal, honest shape.

What stays in this repository: this design note, the `/api/v1` implementation
and its tests, and the contract documentation the extension pins against.

## 6. Suggested sequencing

1. `/api/v1` requests endpoints + SSE in `internal/webui`, sharing internals
   with the HTML handlers (this repo).
2. Extension MVP: TreeView + notification + approve/deny + SecretStorage
   (new repo).
3. Details webview with `flag_types` review and config-diff preview.
4. Read-only rules and calls views; revisit whether rules CRUD is worth it
   only if operators ask.
