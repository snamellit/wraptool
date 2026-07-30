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

CI runs gofmt + `go vet` + `golangci-lint` on every branch — GitLab (`.gitlab-ci.yml`, `lint` job) and Forgejo (`.forgejo/workflows/lint.yml`, on the `guix` runner). The `exhaustive` linter is enabled so a `switch` over a typed-const enum must name every member (see `design/policy/flag-constraint-type-single-source.md`).

`manifest.scm` is the single source of truth for the package set (go/git/golangci-lint/node/quarto plus shell/coreutils/nss-certs plumbing), with no build-time toggles. `channels.scm` pins Snamguix and brings in its Nonguix dependency; local development and Forgejo evaluate the manifest with `guix time-machine -C channels.scm -- ...`, and the CI image workflow rebuilds when either file changes. `wraptool up` uses the same manifest with the developer's configured channels. Same packages everywhere; credential isolation is the mount/user namespace, not the package set.

## Architecture

```
main.go                         entrypoint
cmd/
  serve.go                      MCP server (registered as `server start`, plus a hidden root-level `serve` alias — generated `.mcp.json` files and documented systemd units invoke `serve` and outlive an upgrade, so that spelling is permanent; stdio/SSE/unix transports; the HTTP listener serves both legacy SSE at `/sse` and Streamable HTTP at `/mcp` — Antigravity negotiates the latter — behind one Bearer-auth wrapper)
  up.go                         `wraptool up` orchestrator: ensure shared server (detached, flock in XDG runtime dir), then bring up the container wired with token + worktree cwd, then drop into a shell (`--shell=auto|always|never`; auto = when stdin/stdout are a TTY). Bare `wraptool` in a repo runs this. The pidfile is written by `serve` itself (`cmd/server.go:writePidFile`), not `up` — so `server stop|status` finds the server however it was started (up, a direct `wraptool server start`, a service manager). serve writes it for the persistent transports (sse/unix), removes it on exit incl. SIGINT/SIGTERM; stdio is per-session and never touches the shared pidfile.
  guixcontainer.go              container backend selection + the Guix-native runtime. `--runtime=auto|guix|devcontainer` (auto = guix when `guix` is on PATH and the worktree has `manifest.scm`, else devcontainer; the chosen runtime is printed). The guix path runs `guix shell --container --network --emulate-fhs -m manifest.scm` (`-F` gives an FHS layout — `/usr/bin/env`, `/lib64/ld-linux-*` — so the FHS-linked `claude` binary finds its loader/interpreter) — the **same** flat toolchain a developer uses, so the harness can compile/test/analyze locally; it writes `.mcp.json` on the host into the shared workspace and marks the shell with `WRAPTOOL_CONTAINER=1`. Credential isolation = mount/user namespace (host `$HOME`/secrets never shared), so privileged/remote git (push, private fetch) still goes through wraptool's MCP git tool; local non-privileged git works in-box. `-N` gives internet at the cost of a shared host netns. `--harness <name>` (default claude) picks which harness config to write (registry: `internal/harness`). A developer **harness pool** (`$XDG_DATA_HOME/wraptool/harness-pool`, `bin/` on PATH + `home/` as the harness HOME) is shared into every container, so a harness binary + its auth installs once across all projects, not per session — the developer's toolbox, not the project. See `design/harness/guix-container-runtime.md` and `design/harness/harness-descriptor.md`.
  down.go                       `wraptool down` — stop this project's dev container (by devcontainer workspace label); `--rm` removes it. Leaves the shared server running.
  server.go                     `wraptool server start|status|stop` — lifecycle of the shared server (`start` is `serve.go`'s command; see above)
  requests.go                   admin CLI for capability request review
  validate.go                   `wraptool config validate [path]` — strict-decode (KnownFields) + semantic check of a config without starting the server; catches typo'd keys the lenient runtime load silently ignores
  init.go                       generates MCP config for coding harnesses (thin CLI over `internal/harness`). Subcommand `init devcontainer` (init_devcontainer.go) scaffolds a fresh `.devcontainer/devcontainer.json` (base image + language features + the `wraptool` feature with a chosen harness + WRAPTOOL_* env/pool mount); writes a new file only (refuses to clobber, no JSONC merge), auto-adds the node feature for npm harnesses. Follows `docs/devcontainers.qmd`.
  harness.go                    `wraptool harness list|install|refresh|update|sync|policy` — manage the version-locked developer harness pool (`$XDG_DATA_HOME/wraptool/harness-pool`, design/unified-harness-pool.md). `list` renders the §10 HARNESS/CONSTRAINT/LOCKED/ACTIVE/LATEST/ELIGIBLE/QUARANTINED table; `policy set` edits the commented manifest.yaml (yaml.Node editor, comments preserved); `refresh`/`update`/`sync` drive `internal/pool`'s orchestrator (metadata-only / resolve+lock+fetch / realize-from-lock, §9's phase separation) for pool-lockable harnesses (`internal/harness.Descriptor.PoolPackage` set — claude/opencode/pi; a harness without one is "legacy (not lockable)"); `install <name> [--constraint]` composes constraint-write→refresh→update→install under one pool-lock span. Phase 1 execution stays legacy: the actual binary install still shells out to the harness's own npm installer, now pinned to the exact pool-locked version instead of floating `latest`. `newHarnessSource` wires in `internal/pool/source`'s NPM implementation as the default `pool.Source`, so `refresh`/`update`/`install`/`sync` are functionally end-to-end against the public npm registry and nodejs.org (`list`/`policy set` need no Source at all).
  root.go                       cobra root command
internal/
  config/                       YAML config types, loading, validation, in-place edits (edit.go + rules.go: editConfig node-tree editor preserves comments via yaml.Node; ApplyRequest + rule CRUD)
  whitelist/                    policy engine — deny > allow > constraints
  executor/                     exec.CommandContext, sanitization, credential scrubbing
  mcptools/                     MCP tool registry, handler, request_capability tool, `wraptool_discover` (`discover_tool.go`) — the catalog of wrapped tools: what policy allows today (+ the MCP tool names) and what each tool advertises but does not yet permit. Closes a bootstrap deadlock: a tool with **no** allow rules is inert (registers zero MCP tools, `config.validateTool` permits this deliberately), so without discovery the AI cannot name a real subcommand in a capability request. It reports policy, never bypasses it — everything unpermitted still needs operator approval — and uses cache-backed introspection; a stale cache may run fixed help argv, never AI-supplied arguments.
  introspect/                   --help parsing (git/GNU/fallback), caching, overrides. Two independent products: `Subcommands` (flags for each **allowed** subcommand) and `Available` (the tool's advertised subcommand catalog from its **top-level** help, via `BestParseSubcommands`) — the latter runs regardless of allow rules so an inert tool is still discoverable, on a fixed `-h`/`--help`/`help` argv that no AI input reaches. Flag value types are syntactic (value placeholder → typed) with a placeholder-aware default: a help that shows **no** placeholder anywhere (e.g. glab) can't distinguish a value flag from a switch, so valueless flags are left **unknown** (`Type==""`) and the schema builder defaults them to string — safer than asserting boolean, which makes a value flag uncarriable. The authoritative type comes from the capability author, not the regex: `wraptool_request_capability` takes an optional `flag_types` map that `config.ApplyRequest` writes as `flag_constraints` (which override introspection). See `design/policy/flag-type-inference.md`. The introspection cache invalidates on its real inputs, not just a TTL: a `configHash` (binary + allowed subcommands + overrides path) plus the mtimes of the tool binary, the wraptool binary (parser code), and the overrides file — so editing config, upgrading a tool, or rebuilding wraptool re-introspects with no manual cache clear. A missing tool binary surfaces a meaningful error. See `design/introspect/introspection-cache-invalidation.md`.
  requestlog/                   append-only JSONL event log (capability requests + tool-call audit); state is a projection folded over the stream, never rewritten in place
  webui/                        operator-facing request-review Web UI (stdlib net/http + html/template, go:embed)
  paths/                        XDG platform paths via adrg/xdg
  pool/                         developer-scoped harness pool (design/unified-harness-pool.md): layout accessors rooted at `paths.DataDir()/harness-pool`, the `PlatformKey` enum, strict-decoded manifest/effective-policy types, the harness-lock.json shape with deterministic serialization, write-once observations, release/observation-age eligibility, the shared atomic-write + pointer-file helpers, the cross-platform advisory pool lock (gofrs/flock), and append-only audit events — plus `orchestrator.go`'s command-level workflows (`RefreshHarness`/`UpdateHarness`/`SyncHarness`/`StatusHarness`, each with a `*Locked` sibling assuming the caller already holds `LockPool`'s lock, §6.5) that `cmd/harness.go` drives. Its local `Source` interface (Refresh/Resolve/Fetch) is structurally, not literally, satisfied by the sibling `internal/pool/source` npm implementation. Still no `Backend`-driving resolve/realize mechanics or generations here — Phase 1 keeps actual harness execution on the legacy npm-install path (§16).
  buildinfo/                    build identity from a release-tag version injected with `go build -ldflags -X` plus `runtime/debug.ReadBuildInfo` VCS revision/dirty state. Tagged builds report exact SemVer; local builds report `devel+<short-sha>[-dirty]`. The tag is the sole release-version source. Feeds MCP `serverInfo.version` + upstream ClientInfo, `wraptool --version`/`wraptool version`, and the `wraptool_info` MCP tool (`mcptools/info_tool.go`) which reports build identity + per-tool introspection freshness (`introspect.CacheStamp`) so a client can confirm which binary is serving and whether an introspection change is live — instead of fingerprinting tool schemas.
```

## Key Design Decisions

- **NEVER SURFACE THE `stdio` MCP TRANSPORT IN EXAMPLES, DOCS, OR SCAFFOLDED DEFAULTS.** wraptool's own `stdio` transport exists for TESTS ONLY. All user-facing config, documentation, and defaults use `sse` (or `unix`). It stays in the code (`cmd/serve.go`) because tests need it, but it must be invisible to users — leaking it into user-visible context has already caused a real bug (a `stdio` scaffold default that `wraptool up` then rejected), and continued leaks will force removing the transport entirely. **Distinct and unrelated:** the `mcp_servers` feature spawns and proxies *upstream* MCP servers that speak stdio ("local stdio MCP servers") — that is a real user-facing concept and stays; only wraptool's own *server transport* stdio is banned from docs.


- **No shell execution** — always `exec.CommandContext` with explicit arg arrays
- **Deny before allow** — deny rules are evaluated first and take absolute precedence
- **Minimal child env** — child processes get only PATH/HOME/LANG plus configured vars; wraptool's own env is never inherited (except PATH), so credentials don't leak. PATH is the tool binary's own directory prepended to the **server's inherited PATH** (`executor.childPath`; falls back to `/usr/bin:/bin` when unset), so both co-located helpers (kubectl → `gke-gcloud-auth-plugin`) and genuine runtime deps in a separate prefix (git → `ssh` on a store-based distro) resolve. Inheriting only PATH — not the full env — restores real dependencies without leaking secrets.
- **Credential scrubbing** — output is scrubbed before returning to the AI context
- **Hot-reload** — config file is watched via fsnotify (parent dir, 200ms debounce, survives editor atomic-rename saves); SIGHUP also triggers reload. Reload swaps policy, replaces MCP tools (`DeleteTools` + `RegisterAll`), and mcp-go emits `notifications/tools/list_changed`. In-flight requests use the old policy.
- **Event log / audit** — `requestlog.Store` is an append-only JSONL event stream (`capability_requested`/`approved`/`denied`, `tool_call`) discriminated by `type`. Every MCP tool invocation is recorded (`Handler.logCall`: outcome, exit code, duration, client/session from the MCP context) — non-fatal, never blocks the call. Request status is a projection folded over the stream, so approve/deny appends a decision event rather than rewriting the file; tool-call events are never clobbered.
- **`requests approve --apply`** — merges an approved capability into the YAML config via `config.ApplyRequest`. Uses `yaml.v3` Node API to preserve comments and ordering; rejects unknown tools and deny-rule conflicts; validates the result before atomic-renaming the new file into place. File-watch reload picks it up.
- **Web UI** — top-level `admin.listen` + required, distinct `admin.auth_token_file` starts an operator-facing review UI inside the serve process on its own listener, independent of the MCP transport (works even under stdio). It lists/approves/denies requests via `requestlog.Store` + `config.ApplyRequest`, and offers full rule CRUD (`/rules`) via `config.AddRule`/`UpdateRule`/`DeleteRule`; both paths share the `config.editConfig` node-tree editor (preserves comments/order, round-trip validates, atomic write) and trigger the same file-watch reload. Rule edits use optimistic concurrency (the rule's prior subcommand is sent as `expect`). `/calls` surfaces the tool-call audit log (`Store.ListToolCalls`) with an allowed/denied filter. Auth: token via `?token=` query or login form → `HttpOnly` session cookie, or `Bearer` for scripts. Bind to `127.0.0.1` — it is **not** AI-facing.

## Testing

- Unit tests in `*_test.go` files next to the code they test
- `testdata/` has captured `--help` outputs for parser tests
- `test-container.sh` runs the full isolation test in a Guix container
- `examples/devcontainer/` is a copy-into-your-project Dev Container template (client-side); `test-connection.sh` there is its isolation + MCP-reachability smoke test (targets host wraptool over the docker bridge, Bearer auth)
- `features/src/` holds the Dev Container **features** (published to the Forgejo OCI registry by `.forgejo/workflows/publish-feature.yml`, tested by `features/test/*/selftest.sh`): `wraptool` is the merged feature keyed on one `harness` option with two orthogonal, default-on toggles — `provision` **installs** the harness binary per image, `connect` **wires** it to the host wraptool (MCP config via `connect.sh` at postCreate). It pins `HOME` to `/home/wraptool-harness` (user-space, not a system `/opt` path; a feature invariant via `containerEnv`, since features can't gate `containerEnv` on an option and `containerEnv` is the layer that survives `devcontainer exec`), onto which `wraptool up` bind-mounts the shared pool `home/` (host source `paths.DataDir()` = per-developer XDG data — the real isolation boundary) so auth/skills persist across projects. This is the devcontainer counterpart of the guix pool (`design/harness/harness-descriptor.md` §7a); the guix-based `wraptool harness install` errors and points here when `guix` is absent. `wraptool-connect` (wiring only) is **deprecated** but stays published for existing configs.
- Run integration test: start `wraptool server start` with SSE, then run `test-container.sh`

## Config Location

Uses XDG: `~/.config/wraptool/config.yaml` (Linux), overridable with
`--config` or `WRAPTOOL_CONFIG`. First run scaffolds it if absent:
`serve`/`up` call `config.EnsureConfigFile` (`internal/config/scaffold.go` +
embedded `default_config.yaml`) which writes a commented starter — one
privileged example tool, `git pull` (its binary path resolved via `LookPath`
since the whitelist needs an absolute, existing binary) so first-run `serve`
both comes up and satisfies the ≥1-tool validation. Never clobbers an existing
file (create-exclusive).

## Dependencies

Direct: mcp-go, cobra, yaml.v3, xdg, uuid, Masterminds/semver/v3 (harness
pool version constraints, `internal/pool`), gofrs/flock (harness pool
advisory lock, `internal/pool`). No dynamic language runtimes.
