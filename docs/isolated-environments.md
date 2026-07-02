# Isolated Development Environments with wraptool

This guide shows how to run your AI coding assistant in an isolated
environment (container or Guix shell) while delegating CLI tool access
to wraptool running on the host, where credentials live.

## Architecture

```
┌─────────────────────────────────────────────┐
│  Host (privileged)                          │
│                                             │
│  wraptool serve                             │
│    - has access to ~/.ssh, kubeconfig, etc. │
│    - listens on Unix socket or TCP          │
│    - enforces whitelist policy              │
│                                             │
│  /run/wraptool.sock ◄─────────────┐        │
│  (or 127.0.0.1:8717)              │        │
└────────────────────────────────────│────────┘
                                     │
┌────────────────────────────────────│────────┐
│  Container / Guix shell (isolated) │        │
│                                    │        │
│  Claude Code / AI assistant        │        │
│    - no credentials mounted        │        │
│    - MCP connects to wraptool ─────┘        │
│    - can only use whitelisted commands      │
│                                             │
│  Project source code (bind-mounted)         │
└─────────────────────────────────────────────┘
```

The key insight: **credentials never enter the isolated environment**.
The AI assistant calls tools via MCP, wraptool validates the request
against the whitelist, and executes the command on the host.

## Option 1: Guix Shell Container

Guix provides `guix shell --container` which creates a lightweight,
reproducible isolated environment using Linux namespaces (no Docker needed).

### Step 1: Start wraptool on the host

```bash
# Create the config
mkdir -p ~/.config/wraptool
cat > ~/.config/wraptool/config.yaml << 'EOF'
server:
  transport: unix
  listen: /run/user/1000/wraptool.sock

tools:
  git:
    binary: /usr/bin/git
    env:
      GIT_SSH_COMMAND: "ssh -i ~/.ssh/id_ed25519"
    timeout: 30s
    allow:
      - subcommand: [status]
        flags: ["--short", "--branch", "--porcelain"]
      - subcommand: [log]
        flags: ["--oneline", "-n", "--format", "--graph"]
        flag_constraints:
          "-n": { type: integer, max: 100 }
      - subcommand: [diff]
        flags: ["--stat", "--name-only", "--staged", "--cached"]
      - subcommand: [add]
        flags: ["-A", "--"]
      - subcommand: [commit]
        flags: ["--message", "--amend", "--no-edit"]
        flag_constraints:
          "--message": { required: true }
      - subcommand: [push]
        flags: ["--set-upstream"]
        deny_flags: ["--force", "--force-with-lease"]
      - subcommand: [pull]
        flags: ["--rebase"]
      - subcommand: [fetch]
        flags: ["--all", "--prune"]
      - subcommand: [branch]
        flags: ["-a", "--list", "-v"]
      - subcommand: [checkout]
        flags: ["-b"]
      - subcommand: [switch]
        flags: ["-c"]
    deny:
      - subcommand: [remote, set-url]
      - subcommand: [config, "--global"]
EOF

# Start wraptool (runs in foreground, use tmux/screen or & for background)
wraptool serve
```

### Step 2: Enter the isolated Guix shell

```bash
guix shell --container \
  --network \
  --share=$HOME/src/myproject=$HOME/src/myproject \
  --expose=/run/user/1000/wraptool.sock=/run/user/1000/wraptool.sock \
  coreutils grep sed findutils \
  -- bash
```

Flags explained:
- `--container`: full namespace isolation (PID, mount, user)
- `--network`: allow network access (needed for SSE transport; not needed for Unix socket but harmless)
- `--share=...`: bind-mount your project directory read-write
- `--expose=...`: expose the wraptool socket read-only into the container
- The package list contains only basic tools — no git, no cloud CLIs, no SSH

### Step 3: Configure Claude Code inside the container

```bash
# Inside the container
mkdir -p ~/.config/claude

cat > ~/.config/claude/settings.json << 'EOF'
{
  "mcpServers": {
    "wraptool": {
      "url": "http://localhost/sse",
      "transport": {
        "type": "unix",
        "socketPath": "/run/user/1000/wraptool.sock"
      }
    }
  }
}
EOF

# Start Claude Code — it discovers git_status, git_commit, etc. via MCP
claude
```

> **Note:** Claude Code's MCP Unix socket client support depends on your
> version. If Unix sockets aren't supported, use SSE transport with
> `--network` and `listen: 127.0.0.1:8717` instead.

### Guix manifest for the coding environment

For a reproducible setup, create a `container-manifest.scm`:

```scheme
(specifications->manifest
 '("coreutils"
   "grep"
   "sed"
   "findutils"
   "diffutils"
   "less"
   "which"
   "nss-certs"))   ; for HTTPS if using SSE transport
```

Then:

```bash
guix shell --container \
  --manifest=container-manifest.scm \
  --share=$HOME/src/myproject=$HOME/src/myproject \
  --expose=/run/user/1000/wraptool.sock=/run/user/1000/wraptool.sock \
  -- bash
```

## Option 2: Docker / Podman Container

### Step 1: Start wraptool on the host

Use SSE transport (simpler with Docker networking):

```yaml
# ~/.config/wraptool/config.yaml
server:
  transport: sse
  listen: 127.0.0.1:8717
  auth_token_file: ~/.config/wraptool/auth_token
# ... tools config as above ...
```

Generate an auth token:

```bash
head -c 32 /dev/urandom | base64 > ~/.config/wraptool/auth_token
chmod 600 ~/.config/wraptool/auth_token
wraptool serve
```

### Step 2: Run the coding container

```bash
# Read the token for passing to the container
AUTH_TOKEN=$(cat ~/.config/wraptool/auth_token)

docker run -it --rm \
  --network host \
  -v $HOME/src/myproject:/workspace \
  -e WRAPTOOL_TOKEN="$AUTH_TOKEN" \
  -w /workspace \
  debian:bookworm-slim bash
```

Or with Podman (rootless):

```bash
podman run -it --rm \
  --network host \
  -v $HOME/src/myproject:/workspace:Z \
  -e WRAPTOOL_TOKEN="$AUTH_TOKEN" \
  -w /workspace \
  debian:bookworm-slim bash
```

### Step 3: Configure Claude Code inside the container

```bash
# Inside the container
apt-get update && apt-get install -y curl

mkdir -p ~/.config/claude
cat > ~/.config/claude/settings.json << EOF
{
  "mcpServers": {
    "wraptool": {
      "url": "http://127.0.0.1:8717/sse",
      "headers": {
        "Authorization": "Bearer $WRAPTOOL_TOKEN"
      }
    }
  }
}
EOF

# Install and start Claude Code
# ... (install node, npm, claude-code)
claude
```

### Docker with Unix socket (more secure, no network needed)

```bash
docker run -it --rm \
  --network none \
  -v $HOME/src/myproject:/workspace \
  -v /run/user/1000/wraptool.sock:/run/wraptool.sock:ro \
  -w /workspace \
  debian:bookworm-slim bash
```

Using `--network none` fully isolates the container from the network.
The only external communication channel is the wraptool Unix socket.

## Option 3: Unix socket with SSE transport

If your MCP client only supports HTTP URLs (not raw Unix sockets),
you can use `socat` to bridge:

```bash
# Inside the container, forward localhost:8717 to the Unix socket
socat TCP-LISTEN:8717,fork,reuseaddr \
  UNIX-CONNECT:/run/wraptool.sock &

# Then configure MCP to connect to http://127.0.0.1:8717/sse
```

## Option 4: VS Code Dev Container

A ready-made Dev Container template lives in
[`examples/devcontainer/`](../examples/devcontainer/). Copy it into your project
as `.devcontainer/` and it builds a minimal client image (no git, no
credentials), points Claude Code at the host wraptool, and ships a
`test-connection.sh` smoke test.

```bash
cp -r examples/devcontainer /path/to/your-project/.devcontainer

# Host: start wraptool on a container-reachable address (see note below).
export WRAPTOOL_TOKEN=$(cat ~/.config/wraptool/auth_token)
# VS Code: "Reopen in Container"  — or:  devcontainer up --workspace-folder .

# Inside the container, verify isolation + connectivity:
.devcontainer/test-connection.sh
```

The container reaches the host at `host.docker.internal` (mapped to the host
gateway). Because it lives in its own network namespace, host `127.0.0.1` is not
reachable — wraptool must listen on the docker gateway or `0.0.0.0`. Override
`WRAPTOOL_URL` in `devcontainer.json` to target a specific gateway. See
`examples/devcontainer/README.md` for details.

## Security Checklist

Before using this setup in production:

- [ ] **Whitelist is minimal** — only allow subcommands the AI actually needs
- [ ] **Auth token is set** — for SSE transport, always use `auth_token_file`
- [ ] **Socket permissions are restricted** — wraptool sets 0600 by default
- [ ] **No credential files are mounted** — verify with `mount` inside the container
- [ ] **No credential env vars leak** — verify with `env` inside the container
- [ ] **Network is restricted** — use `--network none` with Docker when possible
- [ ] **Review request log** — periodically check `wraptool requests list` for patterns

## Verifying Isolation

From inside the container, confirm that credentials are not accessible:

```bash
# These should all fail
cat ~/.ssh/id_ed25519          # No SSH keys
cat ~/.kube/config             # No kubeconfig
echo $AWS_SECRET_ACCESS_KEY    # No cloud credentials
git push origin main           # git not installed / no credentials

# This should work (via wraptool MCP)
# Claude Code calling git_status -> wraptool -> git on host
```

## Systemd Service (Optional)

To run wraptool as a persistent service on the host:

```ini
# ~/.config/systemd/user/wraptool.service
[Unit]
Description=wraptool MCP server
After=network.target

[Service]
Type=simple
ExecStart=%h/.local/bin/wraptool serve
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload
systemctl --user enable --now wraptool
```

Reload the whitelist config without restarting:

```bash
systemctl --user reload wraptool   # sends SIGHUP
```
