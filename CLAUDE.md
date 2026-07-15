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

`manifest.scm` is the single source of truth for the toolchain — one **flat** list (go/git/golangci-lint/node + a shell/coreutils/nss-certs plumbing var; golangci-lint is a pinned pre-built binary since Guix has no package), no build-time toggles. It is used verbatim by: local dev (`guix shell -m manifest.scm`), Forgejo lint (same, native), the GitLab CI image (`guix pack -f docker -m manifest.scm` via `.forgejo/workflows/publish-ci-image.yml`, path-filtered on `manifest.scm`, pushed to `forge.snamellit.com/pti/wraptool/ci-toolchain`), and the `wraptool up` guix container. Same toolchain everywhere; credential isolation is the mount/user namespace, not the package set.

## Architecture

```
main.go                         entrypoint
cmd/
  serve.go                      MCP server (stdio/SSE/unix transports; the HTTP listener serves both legacy SSE at `/sse` and Streamable HTTP at `/mcp` — Antigravity negotiates the latter — behind one Bearer-auth wrapper)
  up.go                         `wraptool up` orchestrator: ensure shared server (detached, pidfile+flock in XDG runtime dir), then bring up the container wired with token + worktree cwd, then drop into a shell (`--shell=auto|always|never`; auto = when stdin/stdout are a TTY). Bare `wraptool` in a repo runs this.
  guixcontainer.go              container backend selection + the Guix-native runtime. `--runtime=auto|guix|devcontainer` (auto = guix when `guix` is on PATH and the worktree has `manifest.scm`, else devcontainer; the chosen runtime is printed). The guix path runs `guix shell --container --network --emulate-fhs -m manifest.scm` (`-F` gives an FHS layout — `/usr/bin/env`, `/lib64/ld-linux-*` — so the FHS-linked `claude` binary finds its loader/interpreter) — the **same** flat toolchain a developer uses, so the harness can compile/test/analyze locally; it writes `.mcp.json` on the host into the shared workspace and marks the shell with `WRAPTOOL_CONTAINER=1`. Credential isolation = mount/user namespace (host `$HOME`/secrets never shared), so privileged/remote git (push, private fetch) still goes through wraptool's MCP git tool; local non-privileged git works in-box. `-N` gives internet at the cost of a shared host netns. `--harness <name>` (default claude) picks which harness config to write (registry: `internal/harness`). A developer **harness pool** (`$XDG_DATA_HOME/wraptool/harness-pool`, `bin/` on PATH + `home/` as the harness HOME) is shared into every container, so a harness binary + its auth installs once across all projects, not per session — the developer's toolbox, not the project. See `design/guix-container-runtime.md` and `design/harness-descriptor.md`.
  down.go                       `wraptool down` — stop this project's dev container (by devcontainer workspace label); `--rm` removes it. Leaves the shared server running.
  server.go                     `wraptool server status|stop` — lifecycle of the `up`-started detached server
  requests.go                   admin CLI for capability request review
  validate.go                   `wraptool config validate [path]` — strict-decode (KnownFields) + semantic check of a config without starting the server; catches typo'd keys the lenient runtime load silently ignores
  init.go                       generates MCP config for coding harnesses (thin CLI over `internal/harness`). Subcommand `init devcontainer` (init_devcontainer.go) scaffolds a fresh `.devcontainer/devcontainer.json` (base image + language features + the `wraptool` feature with a chosen harness + WRAPTOOL_* env/pool mount); writes a new file only (refuses to clobber, no JSONC merge), auto-adds the node feature for npm harnesses. Follows `docs/devcontainers.qmd`.
  harness.go                    `wraptool harness list|install` — manage the developer harness pool (`$XDG_DATA_HOME/wraptool/harness-pool`). `list` shows each registry harness (container CLI vs host IDE, installed?, auth); `install <name>` runs the harness's own installer into the pool (claude: `npm i -g @anthropic-ai/claude-code`), or prints manual/IDE guidance. wraptool never packages harnesses — it delegates to their installers.
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
- **Minimal child env** — child processes get only PATH/HOME/LANG plus configured vars; wraptool's own env is never inherited (except PATH), so credentials don't leak. PATH is the tool binary's own directory prepended to the **server's inherited PATH** (`executor.childPath`; falls back to `/usr/bin:/bin` when unset), so both co-located helpers (kubectl → `gke-gcloud-auth-plugin`) and genuine runtime deps in a separate prefix (git → `ssh` on a store-based distro) resolve. Inheriting only PATH — not the full env — restores real dependencies without leaking secrets.
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
- `features/src/` holds the Dev Container **features** (published to the Forgejo OCI registry by `.forgejo/workflows/publish-feature.yml`, tested by `features/test/*/selftest.sh`): `wraptool` is the merged feature keyed on one `harness` option with two orthogonal, default-on toggles — `provision` **installs** the harness binary per image, `connect` **wires** it to the host wraptool (MCP config via `connect.sh` at postCreate). It pins `HOME` to `/home/wraptool-harness` (user-space, not a system `/opt` path; a feature invariant via `containerEnv`, since features can't gate `containerEnv` on an option and `containerEnv` is the layer that survives `devcontainer exec`), onto which `wraptool up` bind-mounts the shared pool `home/` (host source `paths.DataDir()` = per-developer XDG data — the real isolation boundary) so auth/skills persist across projects. This is the devcontainer counterpart of the guix pool (`design/harness-descriptor.md` §7a); the guix-based `wraptool harness install` errors and points here when `guix` is absent. `wraptool-connect` (wiring only) is **deprecated** but stays published for existing configs.
- Run integration test: start `wraptool serve` with SSE, then run `test-container.sh`

## Config Location

Uses XDG: `~/.config/wraptool/config.yaml` (Linux), overridable with
`--config` or `WRAPTOOL_CONFIG`.

## Dependencies

Direct: mcp-go, cobra, yaml.v3, xdg, uuid. No dynamic language runtimes.
