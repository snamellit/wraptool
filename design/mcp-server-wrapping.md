# Design: Wrapping local stdio MCP servers

Status: **draft / planning**
Scope: add first-class support for policing **stdio MCP servers that wraptool
itself spawns**, alongside the existing CLI-tool wrapping — with the same
security profile, the same audit/scrub/request-capability machinery, and
transparent passthrough of the upstream tools' schemas and payloads.

## 1. Goal

Today wraptool wraps CLI tools (`git`, `gcloud`, `kubectl`) behind a whitelist,
keeping their credentials out of the AI context. Many of those same
capabilities now also ship as **local MCP servers** (a GitHub MCP server, a
filesystem server, etc.) that run with the same host credentials. We want the
AI to reach those servers through wraptool, so they inherit:

- **Credential isolation** — the server's secrets never enter the AI context.
- **Policy** — an allow/deny gate over which upstream tools are exposed/callable.
- **Scrubbing** — credential/secret scrubbing on the results returned to the AI.
- **Audit** — every call recorded in the append-only event log.
- **Capability requests** — denied calls can be requested and admin-approved.

"Transparent" here means **access and functional payload**: the upstream tool's
JSON Schema and its argument/result payloads pass through unchanged. It does
**not** mean skipping scrubbing or secret-hiding — those stay.

### Explicitly in scope

- **stdio transport only.** The upstream server is a child process wraptool
  launches (`exec` of a binary + args), speaking MCP over its stdin/stdout.
- **wraptool-spawned only.** We do not connect to servers someone else started.
  This is what gives us the CLI-equivalent security profile (see §2).

### Explicitly out of scope (for now)

- Remote/SSE/HTTP upstreams, or attaching to already-running servers.
- Argument-level constraint language for MCP calls (payloads pass through; policy
  is at tool-selection granularity). May come later — see §9.
- Upstream sampling / elicitation / roots callbacks (server→client requests).
  MVP treats upstreams as request/response tool providers only.

## 2. Security model — parity with CLI tools

The key claim, and the reason to restrict to wraptool-spawned stdio servers:

> A stdio MCP server that wraptool launches has the **same security profile** as
> a CLI tool wraptool execs.

Both are child processes wraptool starts with `exec.CommandContext` (no shell),
an explicit argument vector, and a **minimal, curated environment** — only
`PATH`/`HOME`/`LANG` plus the tool's configured `env`/`env_files`. Credentials
are injected by wraptool into the child and never surface to the AI; results are
scrubbed on the way back. wraptool "cannot sandbox what the child does
internally" — but that is equally true of `git` today, so it is not a new
weakening.

The differences from CLI wrapping are **operational, not security-model**:

| Aspect | CLI tool | stdio MCP server |
|---|---|---|
| Lifetime | one-shot per call | long-lived process, many calls |
| Env injection | per exec | once at spawn |
| Concurrency | one process per call | one process, calls multiplexed by JSON-RPC id |
| Failure | non-zero exit | process crash → needs restart/supervision |
| Interface | argv + stdout | JSON-RPC `tools/call` + structured result |

These drive the implementation work (a supervised long-lived client) but do not
change the isolation guarantees.

One genuinely new surface to keep minimal: an MCP server may declare **server→
client capabilities** (sampling, elicitation, roots). MVP declares **none** of
these to the upstream, so it cannot ask wraptool (and by extension the AI) to do
anything — it can only answer `tools/call`. This keeps the trust boundary tight.

## 3. Current architecture (recap of the seams we reuse)

The CLI request pipeline, with the files/functions involved:

```
AI → MCP tool call
  → mcptools.Handler.Handle           handler.go
      → whitelist.Policy.Evaluate     policy.go   (deny > allow > constraints)
      → executor.Run                  executor.go (minimal env, exec, scrub)
      → requestlog logCall            (audit event)
  → result
```

Registration + lifecycle:

- `mcptools.RegisterAll(s, cfg, policy, store)` iterates `cfg.Tools`, introspects
  each, and registers one MCP tool per allow-rule (`registry.go`).
- Hot-reload (`cmd/reload.go` `reloadConfig`) swaps the policy and re-registers
  the tool set via `DeleteTools` + `RegisterAll`, emitting `tools/list_changed`.
- Audit: `requestlog.Store`, append-only JSONL, `EventToolCall` etc.
  (`internal/requestlog/types.go`).
- Scrubbing: `executor.ScrubCredentials` + `ScrubEnvVars` (`scrub.go`).
- Config env: `config.ToolConfig.ResolvedEnv()` merges `env_files` + inline `env`.

Almost all of this is source-agnostic. The only CLI-specific pieces are the
`subcommand`/`flags`/`positionalArgs` shape in `Policy.Evaluate` and the argv
building in `executor.Run`.

## 4. Proposed design

### 4.1 A `Source` seam (small refactor, makes the addition first-class)

Introduce an internal notion of a **tool source** that produces registrable
tools and invokes them. Two implementations:

- `cliSource` — wraps today's `executor` + `whitelist` (the current behaviour).
- `mcpSource` — wraps one supervised upstream stdio client.

```go
// sketch — the pinned, implementation-ready shape is in §10.2.
type Source interface {
    // Tools the source currently offers, already policy-filtered.
    Tools() []RegisteredTool
}

type RegisteredTool struct {
    Name        string                 // final (namespaced) MCP tool name
    Description string
    RawSchema   json.RawMessage        // nil for CLI (built from flags)
    Options     []mcp.ToolOption       // used for CLI
    Invoke      server.ToolHandlerFunc // the per-tool handler
}
```

`RegisterAll` becomes "for each source, register its tools." **Decision: do this
refactor** (rather than bolting `mcp_servers` on beside `Tools`) — the seam keeps
`registry.go`/`reload.go` from growing a second parallel registration/reload code
path. It lands in Phase 0 with `cliSource` reproducing today's output
byte-for-byte before any MCP code is added.

### 4.2 Config schema

A new top-level `mcp_servers:` map, deliberately parallel to `tools:`:

```yaml
mcp_servers:
  github:
    command: /usr/bin/github-mcp-server   # binary wraptool spawns
    args: ["stdio"]
    working_dir: ~/src                     # optional
    env_files: ["~/.config/wraptool/github.env"]
    env:                                   # injected creds, same as tools:
      GITHUB_TOKEN: "${GITHUB_TOKEN}"      # (or via env_files)
    timeout: 30s                           # per-call timeout
    init_timeout: 10s                      # handshake/startup budget
    name_prefix: gh                        # optional; default = server key

    # Policy: default-deny, opt in per upstream tool name.
    allow:
      - tool: create_issue
      - tool: get_file_contents
      - tool: list_pull_requests
    deny:
      - tool: delete_repository            # deny wins, even if allow-listed
```

Notes:

- `command`/`args`/`working_dir`/`env`/`env_files`/`timeout` mirror `ToolConfig`
  so the **shared env builder** (§4.6) and mental model carry over verbatim.
- Policy is a list of upstream **tool names** (not subcommands/flags). Default is
  deny-all; you allow-list the tools you want. `deny` takes precedence, matching
  the existing "deny > allow" rule.
- `name_prefix` controls namespacing (§4.4).

`config.Config` gains `MCPServers map[string]MCPServerConfig`. `applyDefaults`
fills `timeout`/`init_timeout`. `Validate` checks `command` is set, prefixes are
unique and collision-free with `tools:`, and every `deny`/`allow` tool name is a
non-empty string.

### 4.3 New package `internal/mcpproxy`

Responsibilities:

1. **Spawn + handshake.** For each configured server, build the child env with
   the shared builder (§4.6), then `client.NewStdioMCPClient(command, envSlice,
   args...)`, `Initialize` (declaring **no** client capabilities), within
   `init_timeout`.
2. **Discover.** `ListTools` (paginate) → the upstream tool list with schemas.
3. **Invoke.** On a wrapped call, `CallTool` against the upstream within the
   per-call `timeout`, then scrub the result (§4.5) and return it.
4. **Supervise.** Detect child exit; restart with backoff; re-list tools on
   restart and on upstream `notifications/tools/list_changed`, propagating a
   re-registration up to the server (§4.7).
5. **Shutdown.** Close clients and reap children on server stop and on reload of
   a removed/changed server.

Concurrency: a single stdio client multiplexes concurrent `tools/call`s over one
pipe (mcp-go correlates by JSON-RPC id), so N in-flight AI calls do **not** need
N processes. We rely on the client's own request routing.

### 4.4 Registration, namespacing, schema passthrough

For each **allowed** upstream tool, register a wraptool tool whose input schema
is the upstream's, **unchanged**:

```go
tool := mcp.NewToolWithRawSchema(finalName, upstream.Description, upstream.RawInputSchema)
s.AddTool(tool, mcpSourceHandler)   // handler forwards to the upstream client
```

`mcp.NewToolWithRawSchema` (confirmed present in mcp-go v0.54.0) is exactly the
"transparent functional payload" mechanism: the JSON Schema the upstream
advertises is what the AI sees, and the arguments object is forwarded verbatim.
This also means **no introspection** for MCP sources — we skip the entire
`--help`-parsing path; the schema comes for free from `tools/list`.

**Namespacing.** Final name = `<name_prefix>_<upstream_tool>` (underscore
separator, default prefix = the `mcp_servers:` key), so `github` +
`create_issue` → `github_create_issue`.

The underscore is safe despite snake_case tool names being near-universal,
because the final name is **never reverse-parsed** — the handler is bound by
closure at registration and already knows its upstream + tool, so the flattened
string is just an opaque unique key. `.`/`/` are rejected as separators anyway:
client tool-name charsets (e.g. Anthropic's `^[a-zA-Z0-9_-]{1,64}$`) forbid
them.

The real guard is **load-time uniqueness validation**: `Validate` computes every
final name (across all `mcp_servers` *and* CLI `tools`) and refuses to start on a
duplicate. The only underscore clash vector — one server's prefix equalling
another's `prefix_<leading-tool-segment>` (e.g. server `github`+`create_issue`
vs server `github_create`+`issue`, both → `github_create_issue`) — is an
operator prefix coincidence, not caused by upstream naming, and is caught
deterministically here rather than silently mis-routing. (Hyphen `-` is a
structurally collision-proof alternative, being outside the snake_case charset,
if construction-level safety is ever preferred over the conventional look.)

### 4.5 Scrubbing over MCP results

`CallTool` returns `*mcp.CallToolResult` whose `Content` is a slice of typed
blocks (`TextContent`, `ImageContent`, `EmbeddedResource`, …). The scrubber runs
over the **text-bearing** blocks:

- `TextContent.Text` → `ScrubCredentials` + `ScrubEnvVars(_, resolvedEnv)`
  (reusing the existing functions, with the server's own resolved env as the
  secret set — same as CLI).
- Non-text blocks (images, binary resources): pass through, but **log** their
  presence in the audit event. (Optionally add a config switch to strip
  non-text content for stricter servers — future.)

Factor the CLI executor's `scrubbed := func(s string){...}` closure into a small
exported helper so both paths share one implementation.

### 4.6 Shared minimal-env builder (refactor)

`executor.Run` currently builds the child env inline (lines ~74–82:
`PATH=childPath(binary)`, `HOME`, `LANG`, + resolved env). Extract this into a
reusable function so `mcpproxy` spawns children **identically**:

```go
// executor (or a small shared pkg)
func ChildEnv(binary string, resolved map[string]string) []string
```

Both `executor.Run` and `mcpproxy` call it. This is what mechanically guarantees
the "same security profile" claim in §2 — one code path decides what a wrapped
child's environment is.

### 4.7 Hot-reload / lifecycle reconciliation

Extend `reloadConfig` (`cmd/reload.go`) to reconcile the **set of servers**, not
just re-register tools:

- **Added server** → spawn + handshake + register its tools.
- **Removed server** → deregister its tools, shut the child down.
- **Changed server** (command/args/env/policy differ) → restart the child and
  re-register.
- **Unchanged server** → keep running; only re-apply the tool allow/deny filter
  if policy changed.

Because starting a process is heavier and failure-prone (unlike a pure policy
swap), reload must be resilient: a server that fails to start logs an error and
is skipped (its tools simply aren't registered), exactly as introspection
failures are non-fatal today. The `reloadState` gains a handle to the running
supervisor so it can diff old vs new.

### 4.8 Policy + audit integration

*(Pinned to concrete types in §10.4–10.5.)*

- **Policy.** Add MCP evaluation — MVP is tool-name allow/deny with deny
  precedence, as a new sibling `whitelist.MCPPolicy` (leaves the CLI engine
  untouched). Denied calls return the same structured denial result and can
  drive `request_capability`.
- **Capability requests.** `request_capability` and the Web UI review flow
  generalize: a request references an `mcp_servers.<name>` + tool instead of a
  tool + subcommand. `config.ApplyRequest` gains an MCP branch that appends an
  `allow: [{tool: ...}]` entry (same node-tree editor, comment-preserving).
- **Audit.** Reuse `EventToolCall`. Its schema (`internal/requestlog/types.go`)
  currently records flags/positional args; generalize to carry the tool name and
  an arguments summary (redacted), plus a `source` discriminator
  (`cli` | `mcp`). `Handler.logCall` grows an MCP-shaped sibling.

## 5. Phased implementation plan

**Phase 0 — refactors (no behaviour change).**
- Extract `executor.ChildEnv`; route `executor.Run` through it.
- Extract the scrub closure into a shared helper.
- Introduce the `Source` seam and make `RegisterAll` iterate sources, with
  `cliSource` reproducing today's output byte-for-byte. (Decided — see §4.1.)

**Phase 1 — read-only MVP.**
- `config`: add `MCPServers`, defaults, validation.
- `internal/mcpproxy`: spawn one server, `Initialize`, `ListTools`, `CallTool`;
  no supervision yet (start at serve, stop at exit).
- Register allowed upstream tools via `NewToolWithRawSchema`; forward calls;
  scrub text content; audit via `EventToolCall` (source=mcp).
- Default-deny tool policy (allow-list only). deny-list honoured.
- Tests: a tiny in-repo stub MCP server binary under `testdata/` (or an
  in-process `client.NewInProcessClient`) to exercise discovery + call + scrub.

**Phase 2 — lifecycle + reload.**
- Supervisor: crash detection, restart w/ backoff, `tools/list_changed`
  propagation.
- Reconcile servers on reload/SIGHUP (add/remove/change/unchanged).
- Graceful shutdown + child reaping.

**Phase 3 — operator surface.**
- `request_capability` + Web UI + `config.ApplyRequest` MCP branch.
- `wraptool init`/docs updates; a `wraptool mcp status` style introspection of
  running upstreams (optional).

## 6. Testing

- **Unit:** config parse/validate/defaults; namespacing + collision detection;
  scrubbing over `CallToolResult` content; policy allow/deny precedence.
- **Integration:** stub upstream MCP server (in-process via mcp-go, or a small
  binary) — assert discovery, forwarded call, scrubbed result, audit event, and
  that a denied tool is not registered / is refused.
- **Security:** assert the spawned child's env equals `ChildEnv(...)` and does
  **not** contain wraptool's own environment (mirror existing executor env test);
  assert a secret in `env` is scrubbed from a result that echoes it.
- **Reload:** add/remove/change a server across a reload; assert tool set and
  process lifecycle track the config.

## 7. Risks / caveats

- **Upstream trust.** Same as CLI: we police which tool + which args flow, not
  the server's internals. Documented, unchanged from status quo.
- **Long-lived process resource use.** Supervision/backoff needed to avoid
  crash-loops; cap restarts.
- **Schema passthrough size.** Some servers advertise large schemas / many
  tools; the allow-list keeps the AI-visible surface (and token cost) bounded —
  another reason default-deny is the right default.
- **Non-text content** (images/resources) can't be scrubbed meaningfully; MVP
  passes them through and logs them. Offer a strict "text-only" switch if needed.
- **Server→client features** (sampling/elicitation/roots) intentionally
  unsupported in MVP; revisit deliberately, as they widen the trust boundary.

## 8. Effort estimate

- Phase 0 + Phase 1 (working, tested, read-through-and-write MVP): **~a few
  days**, leaning on the existing registration/audit/scrub/config scaffolding.
- Phase 2 (supervision + reload): a further **1–2 days**.
- Phase 3 (request workflow + Web UI + docs): **1–2 days**.

## 9. Decisions

1. **Namespacing separator** — **underscore** (`github_create_issue`), guaranteed
   by load-time uniqueness validation, not by separator parsing. `.`/`/` are
   invalid under client tool-name charsets. See §4.4. *(resolved)*
3. **`Source` refactor** — **do it**, in Phase 0, with `cliSource` reproducing
   current output byte-for-byte. See §4.1. *(resolved)*
4. **Strict "text-only" content mode** — **deferred** past v1; MVP passes
   non-text content through and logs its presence. See §4.5 / §7. *(resolved)*
5. **Config key** — **`mcp_servers`**. *(resolved)*
2. **Argument constraints** — **tool-level allow/deny only in v1**; payloads pass
   through transparently (schema + arguments + results unchanged, scrubbing
   retained). Per-argument JSON-path constraints are a deliberate later
   extension, not part of v1. See §1 (scope) / §4.8. *(resolved)*

All planning questions resolved. The grounding spike (§10) pins the concrete
APIs and acceptance tests, so Phase 0/1 are ready to delegate.

## 10. Execution spec (grounding spike results)

The design questions above are decided; this section pins the concrete APIs,
signatures, and acceptance tests so Phase 0/1 can be handed to execution agents
without further design decisions. All facts below are verified against
**mcp-go v0.54.0** (the pinned dependency) and the current internal code.

### 10.1 Verified mcp-go client API

- `client.NewStdioMCPClient(command string, env []string, args ...string)
  (*client.Client, error)` — spawns the child. **`env` is a full replacement
  environment** (`[]string` of `K=V`), so it takes `executor.ChildEnv(...)`
  output directly. This is the mechanical guarantee of CLI-parity env (§4.6).
- `(*Client).Start(ctx)` — idempotent; wires the notification handler. Call it
  before `Initialize` so `tools/list_changed` can be observed.
- `(*Client).Initialize(ctx, mcp.InitializeRequest) (*mcp.InitializeResult,
  error)` — leave `Params.Capabilities` zero to **declare no client
  capabilities** (no sampling/roots/elicitation — matches §2). Empty
  `ProtocolVersion` defaults to `mcp.LATEST_PROTOCOL_VERSION` (`"2025-11-25"`).
- `(*Client).ListTools(ctx, mcp.ListToolsRequest) (*mcp.ListToolsResult, error)`
  — **auto-paginates internally** (follows `NextCursor`); one call returns all
  `Tools []mcp.Tool`. No cursor loop needed on our side.
- `(*Client).CallTool(ctx, mcp.CallToolRequest) (*mcp.CallToolResult, error)` —
  `Params` is `mcp.CallToolParams{Name string, Arguments any}`. `Arguments` is
  `any`, so the AI's arguments object is **forwarded verbatim** (payload
  transparency, §4.8).
- `(*Client).OnNotification(func(mcp.JSONRPCNotification))` — for upstream
  `notifications/tools/list_changed` (Phase 2).
- `(*Client).Close() error` — child shutdown (Phase 2 supervisor).

### 10.2 Pinned `Source` interface (resolves §4.1 "TBD")

New file `internal/mcptools/source.go`:

```go
// Source produces registrable MCP tools and their handlers. Implementations:
// cliSource (this package) and mcpSource (internal/mcpproxy).
type Source interface {
    // Tools returns the tools to register, already policy-filtered. Called at
    // registration and on reload. ctx bounds any upstream I/O.
    Tools(ctx context.Context) ([]RegisteredTool, error)
    // Close releases resources (stops upstream child processes). CLI: no-op.
    Close() error
}

type RegisteredTool struct {
    Name    string                 // final (namespaced for MCP) tool name
    Tool    mcp.Tool               // fully built (flag opts, or raw schema)
    Handler server.ToolHandlerFunc // per-tool invocation handler
}
```

`RegisterAll(s *server.MCPServer, sources []Source, store) ([]string, error)`
iterates sources, calls `Tools(ctx)`, `s.AddTool(rt.Tool, rt.Handler)`, and
collects names. `cmd/serve` builds a `cliSource` (from `cfg.Tools` + policy +
introspect, reproducing today's `registerTool` output byte-for-byte) plus one
`mcpSource` per `cfg.MCPServers`, and passes the slice. Import direction:
`mcpproxy` → `mcptools` (for `Source`/`RegisteredTool`); `mcptools` never imports
`mcpproxy`, so no cycle.

### 10.3 Reference snippet — connect, discover, register (Phase 1)

```go
env := executor.ChildEnv(cfg.Command, resolvedEnv)          // == CLI env builder
c, err := client.NewStdioMCPClient(cfg.Command, env, cfg.Args...)
if err != nil { return err }                                // spawn failed
if err := c.Start(ctx); err != nil { return err }

initReq := mcp.InitializeRequest{}
initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
initReq.Params.ClientInfo = mcp.Implementation{Name: "wraptool", Version: ver}
// Capabilities left zero → declare none.
initCtx, cancel := context.WithTimeout(ctx, cfg.InitTimeout); defer cancel()
if _, err := c.Initialize(initCtx, initReq); err != nil { return err }

lt, err := c.ListTools(ctx, mcp.ListToolsRequest{})         // auto-paginates
for _, ut := range lt.Tools {
    if !policy.Allowed(serverName, ut.Name) { continue }    // default-deny
    raw, err := json.Marshal(ut.InputSchema)                // schema passthrough
    if err != nil { log.Printf("skip %s: %v", ut.Name, err); continue }
    final := prefix + "_" + ut.Name                         // namespacing (§4.4)
    tool := mcp.NewToolWithRawSchema(final, ut.Description, raw)
    // → RegisteredTool{Name: final, Tool: tool, Handler: mcpHandler{...}.Handle}
}
```

### 10.4 Invoke + scrub handler (resolves §4.8 "sibling" hand-wave)

A dedicated handler type (do **not** overload `mcptools.Handler`, which is
CLI-shaped with subcommand/flag parsing):

```go
type mcpHandler struct {
    client     *client.Client
    serverName string
    toolName   string            // upstream (un-prefixed) name
    env        map[string]string // resolved env → scrub secret set
    timeout    time.Duration
    policy     *whitelist.MCPPolicy
    store      *requestlog.Store
}

func (h *mcpHandler) Handle(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
    start := time.Now()
    if d := h.policy.Evaluate(h.serverName, h.toolName); !d.Allowed {
        h.audit(ctx, false, d.Reason, time.Since(start))
        return mcp.NewToolResultError(d.Reason), nil
    }
    up := mcp.CallToolRequest{}
    up.Params.Name = h.toolName
    up.Params.Arguments = req.GetArguments()                // forward verbatim
    cctx, cancel := context.WithTimeout(ctx, h.timeout); defer cancel()
    res, err := h.client.CallTool(cctx, up)
    if err != nil {
        h.audit(ctx, true, "", time.Since(start))
        return mcp.NewToolResultError(fmt.Sprintf("upstream error: %s", err)), nil
    }
    scrubContent(res.Content, h.env)                        // scrub in place
    h.audit(ctx, true, "", time.Since(start))
    return res, nil
}
```

`whitelist.MCPPolicy` is a **new sibling** to `whitelist.Policy` (leaves the CLI
engine untouched): `Evaluate(server, tool string) Decision`, deny-before-allow,
default-deny. Denials reuse the existing structured denial + `request_capability`
path.

Scrub over content (verified against `mcp.AsTextContent` + `TextContent`):

```go
func scrubContent(content []mcp.Content, env map[string]string) {
    for i, c := range content {
        if tc, ok := mcp.AsTextContent(c); ok {             // *mcp.TextContent
            tc.Text = executor.Scrub(tc.Text, env)          // ScrubCredentials+ScrubEnvVars
            content[i] = *tc
        }
        // ImageContent/AudioContent/EmbeddedResource pass through; log presence.
    }
}
```

(`executor.Scrub(s string, env map[string]string) string` is the extracted
shared helper from §4.6 wrapping `ScrubCredentials` + `ScrubEnvVars`.)

### 10.5 Audit schema delta (backward-compatible)

`ToolCallEvent` (`internal/requestlog/types.go`) gains three `omitempty` fields —
purely additive, so existing JSONL lines still unmarshal and current projections
(`Store.ListToolCalls`) keep working:

```go
type ToolCallEvent struct {
    // ... existing CLI fields unchanged ...
    Source    string         `json:"source,omitempty"`    // "cli" | "mcp"; empty ⇒ cli
    Server    string         `json:"server,omitempty"`    // mcp_servers.<name>
    Arguments map[string]any `json:"arguments,omitempty"` // redacted arg summary
}
```

Readers (CLI `requests`, Web UI `/calls`) treat empty `Source` as `cli`. For MCP
events, `Tool` holds the upstream tool name and `Subcommand`/`Flags`/
`PositionalArgs` stay empty.

### 10.6 Per-task acceptance tests

| Phase | Task | Done when |
|---|---|---|
| 0 | `executor.ChildEnv` extraction | `executor.Run` routes through it; existing executor env test still green; `ChildEnv` unit test covers the `childPath` cases. |
| 0 | `executor.Scrub` helper | `Run` uses it; existing scrub tests unchanged. |
| 0 | `Source` seam | `RegisterAll([]Source)`; `cliSource` registers the **same tool names + schemas** as before (golden test over a sample config). No behaviour change. |
| 1 | `config.MCPServers` | parse/defaults/validate tests: missing `command` rejected; prefix uniqueness across `tools`+`mcp_servers` enforced; `ResolvedEnv` reused. |
| 1 | `mcpproxy` connect+list+call | against an in-process stub server (`client.NewInProcessClient` or a `testdata/` stub): discovers tools, forwards a call, returns result. |
| 1 | passthrough + namespacing | registered tool's raw schema equals `json.Marshal(upstream.InputSchema)`; name is `prefix_tool`. |
| 1 | policy | default-deny (un-allow-listed tool not registered / refused); `deny` overrides `allow`. |
| 1 | scrub | a secret in the server's `env` echoed in a tool result is scrubbed from the returned content. |
| 1 | env isolation | spawned child's env equals `ChildEnv(...)` and contains **none** of wraptool's own environment (mirror the existing executor env test). |
| 1 | audit | an MCP call appends a `ToolCallEvent{Source:"mcp",Server:…,Tool:…}`; an old CLI event line still parses. |

With §10 pinned, **Phase 0 and Phase 1 are ready to delegate.** Phase 2
(supervisor: `OnNotification` re-list, crash/backoff, reload reconciliation,
`Close`) and Phase 3 (`request_capability`/Web UI/`ApplyRequest` MCP branch)
remain design-level and should get the same spike treatment before hand-off.
