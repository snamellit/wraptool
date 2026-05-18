# wraptool

An MCP server that wraps CLI tools behind a security whitelist, so AI
coding assistants can use tools like git, gcloud, and kubectl without
direct access to credentials.

## Why

AI coding assistants need CLI tools to be productive, but giving them
direct access means exposing API keys, SSH keys, kubeconfigs, and
service account credentials to the model context. wraptool sits between
the assistant and the tools:

- **Credentials stay isolated** -- wraptool runs as a separate process
  with its own environment; secrets never enter the AI context.
- **Subcommand-level whitelisting** -- allow `git commit` but deny
  `git push --force`, with flag-level granularity and value constraints.
- **Auto-discovery** -- wraptool parses `--help` output to generate
  MCP tool schemas with real descriptions, so the AI knows what each
  flag does.
- **Structured denial workflow** -- when a command is denied, the AI
  can submit a capability request that an admin reviews and approves
  from the command line.

## Quick Start

### 1. Build

```bash
go build -o wraptool .
```

On Guix systems, use the included manifest:

```bash
guix shell -m manifest.scm -- go build -o wraptool .
```

### 2. Configure

Create `~/.config/wraptool/config.yaml`:

```yaml
server:
  transport: sse
  listen: 127.0.0.1:8717

tools:
  git:
    binary: /usr/bin/git
    timeout: 30s
    allow:
      - subcommand: [status]
        flags: ["--short", "--branch", "--porcelain"]
      - subcommand: [log]
        flags: ["--oneline", "-n", "--format", "--graph"]
        flag_constraints:
          "-n": { type: integer, max: 100 }
      - subcommand: [diff]
        flags: ["--stat", "--name-only", "--staged"]
      - subcommand: [commit]
        flags: ["--message", "--amend"]
        flag_constraints:
          "--message": { required: true }
      - subcommand: [push]
        flags: ["--set-upstream"]
        deny_flags: ["--force", "--force-with-lease"]
    deny:
      - subcommand: [remote, set-url]
```

### 3. Start the server

```bash
wraptool serve
```

### 4. Connect your coding tool

```bash
wraptool init --harness claude --harness cursor --url http://127.0.0.1:8717/sse
```

This writes the MCP config for each tool. Supported harnesses:
`claude`, `cursor`, `opencode`, `gemini`, `roo`, `windsurf`, or `all`.

## Transports

| Transport | Config | Use case |
|-----------|--------|----------|
| `stdio` | `transport: stdio` | Harness launches wraptool directly |
| `sse` | `transport: sse`, `listen: host:port` | Independent process, TCP |
| `unix` | `transport: unix`, `listen: /path/to.sock` | Independent process, Unix socket |

For SSE and Unix transports, add an auth token:

```yaml
server:
  transport: sse
  listen: 127.0.0.1:8717
  auth_token_file: ~/.config/wraptool/auth_token
```

```bash
head -c 32 /dev/urandom | base64 > ~/.config/wraptool/auth_token
```

## Whitelist Configuration

### Allow rules

Each tool has a list of allowed subcommands with flag-level control:

```yaml
allow:
  - subcommand: [compute, instances, list]   # gcloud compute instances list
    flags: ["--format", "--filter"]           # only these flags permitted
    deny_flags: ["--quiet"]                   # explicitly blocked
    flag_constraints:
      "--filter": { pattern: "^[a-zA-Z0-9_ =.\"'-]+$" }
    arg_constraints:
      positional_max: 2
```

### Deny rules

Deny rules take absolute precedence over allow rules:

```yaml
deny:
  - subcommand: [exec]       # kubectl exec -- always denied
  - subcommand: [auth]       # gcloud auth -- always denied
```

### Flag constraints

| Field | Type | Description |
|-------|------|-------------|
| `required` | bool | Flag must be provided |
| `type` | string | `integer`, `boolean`, or `string` |
| `min`, `max` | int | Range for integer values |
| `pattern` | string | Regex the value must match |
| `deny` | bool | Flag is always denied |

### Environment isolation

Each tool gets its own environment, separate from both the host shell
and the AI context:

```yaml
tools:
  gcloud:
    binary: /usr/bin/gcloud
    env:
      CLOUDSDK_CORE_PROJECT: my-project
      CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE: /etc/wraptool/sa.json
    env_files:
      - /etc/wraptool/gcloud.env
```

Inline `env` values override `env_files`. Child processes receive only
a minimal base environment (`PATH`, `HOME`, `LANG`) plus the configured
variables.

## Capability Requests

When the AI attempts a denied command, wraptool returns:

```
DENIED: 'git push --force origin main' is not permitted.

Policy: flag '--force' is not in the allowlist for 'git push'.
Allowed flags for 'git push': --set-upstream

To request this capability, call the 'wraptool_request_capability' tool.
```

The AI can then call `wraptool_request_capability` with a structured
rationale. Requests are stored in a log file and reviewed from the
command line:

```bash
wraptool requests list
wraptool requests approve req_23ae3793
wraptool requests deny req_01a2b3c4 --reason "use --force-with-lease instead"
```

Approving a request marks it in the log. Add the rule to the config
manually, or use `--apply` to write it automatically:

```bash
wraptool requests approve req_23ae3793 --apply
```

With `--apply`, wraptool merges the approved capability into the
config file in place:

- The tool must already exist in `tools:` (binary path must be set).
- If a deny rule covers the same subcommand, the apply is refused.
- An existing allow rule with the same subcommand gains the new flags;
  duplicates are dropped.
- Otherwise a new allow rule is appended.
- Comments and key ordering are preserved (the YAML node tree is
  edited, not re-marshaled from typed structs).
- The edited config is validated before being written; on failure the
  original file is left untouched.
- The write is atomic (temp file + rename) and the running server
  picks the change up via the file watcher.

Auto-added flags have no `flag_constraints` -- tighten them by hand
if you want to bound their values.

## Hot-Reload

wraptool watches the config file and reloads on every save. The reload:

1. Parses and validates the new config (rejected reloads leave the
   running policy untouched and log the error).
2. Atomically swaps the policy used for new evaluations. In-flight
   commands keep running against the policy they started with.
3. Removes the previously registered MCP tools and registers a fresh
   set from the new config.
4. Sends `notifications/tools/list_changed` to every connected MCP
   client. Clients that honor the notification refresh their tool
   list without reconnecting.

Triggers:

- **File save** -- the parent directory of the config file is watched
  with [fsnotify](https://github.com/fsnotify/fsnotify), so editors
  that save via atomic rename (vim, helix, etc.) are handled. Events
  are debounced by 200 ms to coalesce the CREATE+WRITE+CHMOD bursts
  most editors emit.
- **SIGHUP** -- still supported for scripted reloads or when the
  watcher cannot be initialized:

  ```bash
  kill -HUP $(pidof wraptool)
  ```

If `fsnotify` fails to start (e.g. no inotify available), wraptool
logs a warning and falls back to SIGHUP-only reload.

## CLI Introspection

wraptool parses `--help` output at startup to enrich MCP tool schemas
with real flag descriptions and types. It supports:

- **Git/Cobra** style (`git commit -h`, `kubectl get -h`)
- **GNU** style (`curl --help`, `wget --help`)
- **Fallback** regex extraction for unusual formats

Results are cached in `~/.cache/wraptool/introspection/`. Override
misparses with a YAML file:

```yaml
# ~/.config/wraptool/overrides/git.yaml
subcommands:
  commit:
    description: "Record changes to the repository"
    flags:
      "--message":
        short: "-m"
        type: string
        description: "Commit message"
```

Reference it in the tool config:

```yaml
introspection:
  cache_ttl: 24h
  overrides_file: ~/.config/wraptool/overrides/git.yaml
```

## Isolated Environments

For maximum security, run the AI assistant in an isolated container
while wraptool runs on the host with credentials. See
[docs/isolated-environments.md](docs/isolated-environments.md) for
guides covering:

- Guix `shell --container`
- Docker / Podman
- systemd user services

## Security

- **No shell execution** -- commands run via `exec.CommandContext`
  with an explicit argument array, never `sh -c`.
- **Argument sanitization** -- shell metacharacters (`;|&$\`(){}`) are
  rejected. Positional arguments cannot start with `-`.
- **Credential scrubbing** -- Bearer tokens, API keys, AWS credentials,
  SSH key markers, and configured env values are stripped from output
  before returning to the AI.
- **Output limits** -- stdout/stderr capped at 1 MB per command.
- **Timeout enforcement** -- per-tool `context.WithTimeout`.
- **Hot-reload safety** -- in-flight requests complete against the old
  policy when config is reloaded (see [Hot-Reload](#hot-reload)).

## Platform Paths

wraptool uses XDG base directories (via [adrg/xdg](https://github.com/adrg/xdg)):

| Purpose | Linux | macOS |
|---------|-------|-------|
| Config | `~/.config/wraptool/` | `~/Library/Application Support/wraptool/` |
| Data | `~/.local/share/wraptool/` | `~/Library/Application Support/wraptool/` |
| Cache | `~/.cache/wraptool/` | `~/Library/Caches/wraptool/` |

Override with `WRAPTOOL_CONFIG` or `--config`.

## License

GPLv3 -- see [LICENSE](LICENSE).
