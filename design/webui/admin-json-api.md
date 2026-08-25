# Design: JSON admin API (`/api/v1`) for external operator clients

Status: **planned**. Implementation plan for the JSON surface that
`vscode-plugin.md` §2 requires, structured so that the Web UI, the CLI, the
API, and the future TUI (`webui-accessibility-reachability.md` §5) all sit on
one shared core instead of four copies of the same logic. The work packages
below are scoped for independent implementation agents.

## 0. Duplication audit (why the plan looks like this)

The approve flow — `store.Get` → `store.UpdateStatus(approved)` → optional
`config.ApplyRequest` → classify the outcome (new rule / extended rule / no
change) with the "status is approved but apply failed" partial-failure case —
already exists **twice**: `internal/webui.handleApprove` and
`cmd/requests.go`'s approve command. A JSON handler would be the third copy
and the TUI the fourth. Everything else is genuinely thin: list/deny/calls
handlers are one store call plus presentation.

Two other reuse facts the plan leans on:

- `requestlog` types already carry JSON struct tags (the store *is* JSONL),
  so the API's wire format is the stored event format. No parallel DTO layer;
  the JSONL schema becomes the documented contract.
- fsnotify is already a direct dependency (config hot-reload), and the TUI
  design already calls for watching the requestlog file. One watcher in
  `internal/requestlog` serves both the SSE endpoint and the TUI.

## 1. Work packages

### WP1 — extract the approve/deny core: `internal/adminops`

New small package (config ↔ requestlog import-cycle-free meeting point):

```go
type ApproveOutcome int // OutcomeNewRule | OutcomeExtended | OutcomeNoChange | OutcomeNotApplied

type ApproveResult struct {
    Request    *requestlog.CapabilityRequest
    Outcome    ApproveOutcome
    AddedFlags []string
    Snippet    string
    ApplyErr   error // non-nil: status is approved but the config write failed
}

func Approve(store *requestlog.Store, configPath, id string,
    apply bool, flagTypes map[string]string) (*ApproveResult, error)

func Deny(store *requestlog.Store, id, reason string) error
```

Semantics are lifted verbatim from today's handlers and must not change:
status flips to approved *before* the apply attempt; an apply failure is
reported via `ApplyErr` (not the error return) so every presenter can render
the same "approved but edit the config by hand" message; `flagTypes` is the
already-merged map (each presenter owns its own merge — the Web UI's
`ftype:` form fields, the API's request body, the CLI's `req.FlagTypes`
passthrough).

Refactor both existing consumers onto it in the same package's change:
`webui.handleApprove` and `cmd/requests.go`. Their user-visible output must
stay byte-identical (existing webui tests pin the flash messages).

Files: `internal/adminops/adminops.go` + tests (all four outcomes, the
partial-failure case, deny with/without reason); edits to
`internal/webui/webui.go`, `cmd/requests.go`.

### WP2 — event tailing: `requestlog.Watch`

```go
func (s *Store) Watch(ctx context.Context) (<-chan Event, error)
```

Tails the JSONL file: emits every event appended after the call, as the
typed `Event` union the store already decodes. fsnotify on the parent
directory (the config watcher's proven pattern — survives editors and
atomic renames, though the log itself is append-only), byte-offset tracking,
partial-line tolerance (an append may be seen mid-write; buffer until
newline), file-creation handling (the log may not exist yet), and shutdown
via ctx. Multi-process correct by construction: a CLI approve from another
terminal lands in the file and therefore in the channel — this is why
in-process pub-sub on `appendEvent` is not the mechanism.

Consumers: WP3's SSE endpoint now, the TUI later.

Files: `internal/requestlog/watch.go` + test (append while watching →
event arrives; ctx cancel → channel closes).

### WP3 — the `/api/v1` surface in `internal/webui`

Same listener, same mux, same middleware as the HTML UI. Handlers are
JSON-thin veneers over exactly the calls the HTML handlers make — any logic
beyond encode/decode belongs in WP1's core or the store, not the handler.

```
GET  /api/v1/info                      {version, api_version: 1}  (buildinfo)
GET  /api/v1/requests?status=pending   [CapabilityRequest]  (store.List; same
                                       default-pending, explicit-empty=all
                                       semantics as the HTML view)
GET  /api/v1/requests/{id}             CapabilityRequest    (store.Get)
POST /api/v1/requests/{id}/approve     body {apply: bool, flag_types: {..}}
                                       → {outcome, added_flags, snippet,
                                          apply_error}      (adminops.Approve)
POST /api/v1/requests/{id}/deny        body {reason}        (adminops.Deny)
GET  /api/v1/calls?status=             [ToolCallEvent]      (store.ListToolCalls,
                                       allowed/denied filter as HTML)
GET  /api/v1/rules                     read-only config snapshot: tools,
                                       allow/deny rules, constraints
                                       (config.Load; no mutation in v1)
GET  /api/v1/events                    SSE stream of requestlog events
```

- Wire types are the `requestlog` structs as-is. The `/rules` response gets
  its own small view struct (the config types serialize YAML-first).
- Errors: `{"error": "..."}` with 404 (unknown id), 400 (bad body), 500;
  `Content-Type: application/json` everywhere.
- SSE: each requestlog event as one SSE `data:` line, `event:` set to the
  event type (`capability_requested`, `capability_approved`, ...); a
  `: heartbeat` comment every ~15s so proxies and clients detect death; no
  replay/Last-Event-ID in v1 — a client re-`GET /requests` on (re)connect,
  which the extension does anyway. Optional `?types=` filter can wait.
- Contract stability: field renames or semantic changes after this ships
  require `/api/v2`. Additive fields are fine.

Files: `internal/webui/api.go` + `api_test.go` (round-trip list/approve/deny
against a temp store+config, error shapes, SSE first-event + heartbeat).

### WP4 — API-aware auth

`AuthMiddleware` currently answers unauthenticated browsers with the HTML
login form and sets a session cookie. For `/api/` paths: accept the same
Bearer token (and an existing session cookie), but never render login HTML,
never set cookies, and answer failures with `401 {"error": "unauthorized"}`
plus `WWW-Authenticate: Bearer`. Token comparison stays constant-time via
the one existing `ok()` closure — no second comparison path.

Files: `internal/webui/webui.go` (middleware) + cases in `webui_test.go`.

### WP5 — CLI alignment

- `wraptool requests list --json`: emit the same JSON array as
  `GET /api/v1/requests` (marshal the store structs; no new types).
- `requests approve` already refactored onto `adminops.Approve` in WP1;
  verify output parity there, nothing further here.

Files: `cmd/requests.go` + a test.

## 2. Sequencing for agents

- **Wave 1 (parallel, disjoint files):** WP1 (`internal/adminops` + both
  consumer refactors) and WP2 (`internal/requestlog/watch.go`).
- **Wave 2 (single agent):** WP3 + WP4 + WP5 on top of wave 1.
- Review each wave against this document before integration.

Gates for every package: `go build ./...`, `go test ./...`, `go vet ./...`,
`golangci-lint run ./...`.

## 3. Non-goals

- Rules **mutation** over the API. The Web UI's optimistic-concurrency rule
  CRUD stays HTML-only until an external client actually needs it
  (`vscode-plugin.md` §3 phase 2 is read-only rules).
- WebSocket, replay, pagination, API tokens distinct from the admin token.
- Serving the API on a separate listener. Same `admin.listen`, same token,
  same 127.0.0.1 bind guidance.
- Any TUI work. The TUI consumes WP1 and WP2 directly (file-direct, per
  `webui-accessibility-reachability.md` §5), not the HTTP API — that is the
  point of extracting them.
