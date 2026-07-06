# CLAUDE.md

## Project

wraptool is a Go MCP server that wraps CLI tools behind a security
whitelist. It isolates credentials from AI coding assistants while
letting them use tools like git, gcloud, and kubectl through MCP.

## Build & Test

```bash
# Guix system — tools come from manifest.scm via direnv
go build -o wraptool .
go test ./...
go vet ./...
golangci-lint run ./...   # config: .golangci.yml (standard set minus errcheck, plus exhaustive)
```

CI runs gofmt + `go vet` + `golangci-lint` on every branch — GitLab (`.gitlab-ci.yml`, `lint` job) and Forgejo (`.forgejo/workflows/lint.yml`, on the `guix` runner). The `exhaustive` linter is enabled so a `switch` over a typed-const enum must name every member (see `design/flag-constraint-type-single-source.md`).

## Architecture

```
main.go                         entrypoint
cmd/
  serve.go                      MCP server (stdio/SSE/unix transports)
  up.go                         `wraptool up` orchestrator: ensure shared server (detached, pidfile+flock in XDG runtime dir), then `devcontainer up` wired with token + worktree cwd, then drop into a shell (`--shell=auto|always|never`; auto = when stdin/stdout are a TTY). Bare `wraptool` in a repo runs this.
  down.go                       `wraptool down` — stop this project's dev container (by devcontainer workspace label); `--rm` removes it. Leaves the shared server running.
  server.go                     `wraptool server status|stop` — lifecycle of the `up`-started detached server
  requests.go                   admin CLI for capability request review
  validate.go                   `wraptool config validate [path]` — strict-decode (KnownFields) + semantic check of a config without starting the server; catches typo'd keys the lenient runtime load silently ignores
  init.go                       generates MCP config for coding harnesses
  root.go                       cobra root command
internal/
  config/                       YAML config types, loading, validation, in-place edits (edit.go + rules.go: editConfig node-tree editor preserves comments via yaml.Node; ApplyRequest + rule CRUD)
  whitelist/                    policy engine — deny > allow > constraints
  executor/                     exec.CommandContext, sanitization, credential scrubbing
  mcptools/                     MCP tool registry, handler, request_capability tool
  introspect/                   --help parsing (git/GNU/fallback), caching, overrides
  requestlog/                   append-only JSONL event log (capability requests + tool-call audit); state is a projection folded over the stream, never rewritten in place
  webui/                        operator-facing request-review Web UI (stdlib net/http + html/template, go:embed)
  paths/                        XDG platform paths via adrg/xdg
```

## Key Design Decisions

- **No shell execution** — always `exec.CommandContext` with explicit arg arrays
- **Deny before allow** — deny rules are evaluated first and take absolute precedence
- **Minimal child env** — child processes get only PATH/HOME/LANG plus configured vars. PATH is the tool binary's own directory prepended to `/usr/bin:/bin` (`executor.childPath`), so co-located helpers resolve (e.g. kubectl → `gke-gcloud-auth-plugin`)
- **Credential scrubbing** — output is scrubbed before returning to the AI context
- **Hot-reload** — config file is watched via fsnotify (parent dir, 200ms debounce, survives editor atomic-rename saves); SIGHUP also triggers reload. Reload swaps policy, replaces MCP tools (`DeleteTools` + `RegisterAll`), and mcp-go emits `notifications/tools/list_changed`. In-flight requests use the old policy.
- **Event log / audit** — `requestlog.Store` is an append-only JSONL event stream (`capability_requested`/`approved`/`denied`, `tool_call`) discriminated by `type`. Every MCP tool invocation is recorded (`Handler.logCall`: outcome, exit code, duration, client/session from the MCP context) — non-fatal, never blocks the call. Request status is a projection folded over the stream, so approve/deny appends a decision event rather than rewriting the file; tool-call events are never clobbered.
- **`requests approve --apply`** — merges an approved capability into the YAML config via `config.ApplyRequest`. Uses `yaml.v3` Node API to preserve comments and ordering; rejects unknown tools and deny-rule conflicts; validates the result before atomic-renaming the new file into place. File-watch reload picks it up.
- **Web UI** — `server.web.listen` (+ optional `web.auth_token_file`) starts an operator-facing review UI inside the serve process on its own listener, independent of the MCP transport (works even under stdio). It lists/approves/denies requests via `requestlog.Store` + `config.ApplyRequest`, and offers full rule CRUD (`/rules`) via `config.AddRule`/`UpdateRule`/`DeleteRule`; both paths share the `config.editConfig` node-tree editor (preserves comments/order, round-trip validates, atomic write) and trigger the same file-watch reload. Rule edits use optimistic concurrency (the rule's prior subcommand is sent as `expect`). `/calls` surfaces the tool-call audit log (`Store.ListToolCalls`) with an allowed/denied filter. Auth: token via `?token=` query or login form → `HttpOnly` session cookie, or `Bearer` for scripts; empty token = localhost-trust. Bind to `127.0.0.1` — it is **not** AI-facing.

## Testing

- Unit tests in `*_test.go` files next to the code they test
- `testdata/` has captured `--help` outputs for parser tests
- `test-container.sh` runs the full isolation test in a Guix container
- `examples/devcontainer/` is a copy-into-your-project Dev Container template (client-side); `test-connection.sh` there is its isolation + MCP-reachability smoke test (targets host wraptool over the docker bridge, Bearer auth)
- Run integration test: start `wraptool serve` with SSE, then run `test-container.sh`

## Config Location

Uses XDG: `~/.config/wraptool/config.yaml` (Linux), overridable with
`--config` or `WRAPTOOL_CONFIG`.

## Dependencies

Direct: mcp-go, cobra, yaml.v3, xdg, uuid. No dynamic language runtimes.
