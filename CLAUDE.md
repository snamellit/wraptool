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
```

## Architecture

```
main.go                         entrypoint
cmd/
  serve.go                      MCP server (stdio/SSE/unix transports)
  requests.go                   admin CLI for capability request review
  init.go                       generates MCP config for coding harnesses
  root.go                       cobra root command
internal/
  config/                       YAML config types, loading, validation
  whitelist/                    policy engine — deny > allow > constraints
  executor/                     exec.CommandContext, sanitization, credential scrubbing
  mcptools/                     MCP tool registry, handler, request_capability tool
  introspect/                   --help parsing (git/GNU/fallback), caching, overrides
  requestlog/                   JSONL store for capability requests
  paths/                        XDG platform paths via adrg/xdg
```

## Key Design Decisions

- **No shell execution** — always `exec.CommandContext` with explicit arg arrays
- **Deny before allow** — deny rules are evaluated first and take absolute precedence
- **Minimal child env** — child processes get only PATH/HOME/LANG plus configured vars
- **Credential scrubbing** — output is scrubbed before returning to the AI context
- **Hot-reload** — SIGHUP reloads policy config; in-flight requests use the old policy

## Testing

- Unit tests in `*_test.go` files next to the code they test
- `testdata/` has captured `--help` outputs for parser tests
- `test-container.sh` runs the full isolation test in a Guix container
- Run integration test: start `wraptool serve` with SSE, then run `test-container.sh`

## Config Location

Uses XDG: `~/.config/wraptool/config.yaml` (Linux), overridable with
`--config` or `WRAPTOOL_CONFIG`.

## Dependencies

Direct: mcp-go, cobra, yaml.v3, xdg, uuid. No dynamic language runtimes.
