# Isolated Development Environments with wraptool

This guide shows how to run your coding agent in an isolated
environment (container or Guix shell) while delegating credentialed CLI and
local MCP-server access to wraptool running on the host, where credentials live.

## Architecture

```
┌─────────────────────────────────────────────┐
│  Host (privileged)                          │
│                                             │
│  wraptool server start                      │
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

The key insight: **host CLI credentials never enter the isolated environment**.
The agent calls tools via MCP, wraptool validates the request against the
policy, and executes the command on the host. Credentials deliberately owned
by the harness, such as its pooled login state and MCP bearer token, are available
inside the environment.

Be clear on what this does and does not buy you. The container still has
network access and the full source tree bind-mounted, and any code running in
it can read the MCP config plus the bearer token and invoke the same host
tools the agent can, if policy allows them. So wraptool protects
**credentials** (SSH keys, gcloud/kube config, and privileged git like push
stay on the host) and **bounds operations** by the policy — it does **not**
protect source confidentiality or restrict network egress. For those, add container
network controls of your own.

For the Guix runtime specifically, `--network` shares the host network namespace,
so host localhost services are reachable, and the harness pool HOME is shared
read-write across projects. These are intentional convenience tradeoffs, not
isolation boundaries. See [Guix: convenience versus
isolation](getting-started.qmd#guix-convenience-versus-isolation) for the
manifest approval workflow and operational guidance.

## Option 1: Guix Shell Container

Guix provides `guix shell --container` which creates a lightweight,
reproducible isolated environment using Linux namespaces (no Docker needed).

> **Shortcut:** on a Guix host, `wraptool up` (or bare `wraptool`) in a repo
> with a `manifest.scm` does all of this for you — starts the host server,
> mints/injects the token, builds the container from `manifest.scm`, writes
> `.mcp.json`, and drops you into a shell. The steps below are the manual
> equivalent, useful for understanding the mechanism or customising it. See
> [Getting started](getting-started.qmd#one-command-per-project-wraptool-up).

### Step 1: Start wraptool on the host

This policy follows the split execution model from [Hardening `git` as a
wrapped tool](git-hardening.qmd#split-execution-model): host credentials
back the transport subcommands wraptool exposes, but `checkout` and
`switch` are **denied**, not allowed. Both can run
`filter.<driver>.clean`/`.smudge`/`.process` from an untrusted repository's
`.gitattributes` — a code-execution path this credentialed host process
gains nothing from opening, since branch checkout is a local operation and
belongs in an uncredentialed `git` installed inside the container, not
routed through wraptool. See [that page's policy-boundary
section](git-hardening.qmd#the-split-is-a-policy-boundary-not-a-preference)
for the full reasoning and a more thoroughly hardened environment.

```bash
# Create the config
mkdir -p ~/.config/wraptool
cat > ~/.config/wraptool/config.yaml << 'EOF'
mcp:
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
    deny:
      - subcommand: [remote, set-url]
      - subcommand: [config]
      - subcommand: [checkout]
      - subcommand: [switch]
EOF

# Start wraptool (runs in foreground, use tmux/screen or & for background)
wraptool server start
```

### Step 2: Enter the isolated Guix shell

```bash
guix shell --container \
  --network \
  --share=$HOME/src/myproject=$HOME/src/myproject \
  --expose=/run/user/1000/wraptool.sock=/run/user/1000/wraptool.sock \
  coreutils grep sed findutils git \
  -- bash
```

Flags explained:
- `--container`: full namespace isolation (PID, mount, user)
- `--network`: share the host network namespace; this provides internet and host
  loopback access, including local databases and test services
- `--share=...`: bind-mount your project directory read-write
- `--expose=...`: expose the wraptool socket read-only into the container
- The package list contains basic tools plus an uncredentialed `git` — no cloud
  CLIs, no SSH. `git` is needed here: the policy above denies `checkout` and
  `switch` on the host, precisely so those local operations run against this
  in-container `git` instead

`--network` is not harmless even when MCP uses a Unix socket: every service on
host localhost becomes reachable from untrusted container code. wraptool accepts
this tradeoff by default because direct access to local PostgreSQL, Redis,
MongoDB, and test servers substantially reduces development setup. Authenticate
those services and use test-only data/accounts.

### Step 3: Configure Claude Code inside the container

Claude Code reads a project-level `.mcp.json` (not `~/.config/claude/...`).
Generate it with `wraptool init` rather than hand-writing the JSON — the shape
and endpoint differ per harness. Run it on the host (or anywhere `wraptool` is
on `PATH`) from the project root:

```bash
wraptool init --harness claude --transport sse \
  --url http://127.0.0.1:8717/sse
```

That writes `.mcp.json` with the entry Claude Code expects (a Unix-socket host
carries no auth token, so no header is emitted):

```json
{
  "mcpServers": {
    "wraptool": {
      "type": "sse",
      "url": "http://127.0.0.1:8717/sse?cwd=/home/you/src/myproject"
    }
  }
}
```

Then, inside the container:

```bash
# Start Claude Code — it discovers git_status, git_commit, etc. via MCP
claude
```

> **Note:** Claude Code has no native MCP Unix-socket client, so it always
> connects over HTTP/SSE. To keep the host on a Unix socket, bridge it with
> `socat` (see [Option 3](#option-3-unix-socket-with-sse-transport)); otherwise
> run wraptool with `listen: 127.0.0.1:8717` and `--network`.

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
   "git"          ; local branch work; checkout/switch are denied on the host
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
mcp:
  transport: sse
  listen: 127.0.0.1:8717
  auth_token_file: ~/.config/wraptool/auth_token
# ... tools config as above ...
```

Generate an auth token:

```bash
head -c 32 /dev/urandom | base64 > ~/.config/wraptool/auth_token
chmod 600 ~/.config/wraptool/auth_token
wraptool server start
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

Claude Code reads a project `.mcp.json`, which must carry the
`Authorization: Bearer` header for an auth-enabled server. The Dev Container
`wraptool` feature's `connect.sh` renders exactly this from the `WRAPTOOL_TOKEN`
(and `WRAPTOOL_CWD`) env you already passed in — the recommended path
([Option 4](#option-4-vs-code-dev-container)). For a plain `docker run`, drop
`connect.sh` into the image and run it from the workspace, or write the file
directly:

```json
{
  "mcpServers": {
    "wraptool": {
      "type": "sse",
      "url": "http://127.0.0.1:8717/sse?cwd=/workspace",
      "headers": { "Authorization": "Bearer <WRAPTOOL_TOKEN>" }
    }
  }
}
```

```bash
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
The wraptool Unix socket is the intended external communication channel. This
assumes no other bind mounts, inherited devices, or container-runtime escape
paths are present.

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

The recommended path uses the **wiring-only `wraptool` Dev Container feature**
(`forge.snamellit.com/pti/wraptool/wraptool:1`), keyed on a single `harness`
choice: it writes the MCP client config wiring the container to the host
wraptool, and pins `HOME`/`PATH`, but installs **no harness binary** — that
comes from the shared harness pool's `runtime/` tree, built once per developer
on the host and mounted read-only into the container. Install the harness,
scaffold a config, and bring it up:

```bash
# Host: build/lock the harness once into the shared pool.
wraptool harness install claude

# In your project root: generate .devcontainer/devcontainer.json
wraptool init devcontainer --harness claude --lang go

# Host: start wraptool on a container-reachable address (see note below) and
# export the token so it reaches the container out of the image layers.
export WRAPTOOL_TOKEN=$(cat ~/.config/wraptool/auth_token)

# Bring it up: `wraptool up`, or VS Code "Reopen in Container", or:
wraptool up            # (off Guix this runs `devcontainer up` for you)
#   devcontainer up --workspace-folder .
```

`wraptool init devcontainer` emits a starting-point `devcontainer.json` from the
`base:ubuntu` + language-features model: the language features you list, the
`wraptool` feature with your harness, and the `WRAPTOOL_CWD` / `WRAPTOOL_TOKEN`
env plus both shared-pool mounts (`home/` read-write, `runtime/` read-only)
prefilled. It writes a fresh file and refuses to clobber an existing one
(pass `--force`).

At container-create the feature's `connect.sh` writes `.mcp.json` pointing at
wraptool, auto-detecting the host as the container's default-route gateway — so
there's no `host.docker.internal` or `--add-host` to manage, and it works across
docker networks and git worktrees. The token stays out of image layers (passed
via `remoteEnv`), and `WRAPTOOL_CWD=${localWorkspaceFolder}` selects this
worktree as the host CWD. Configure `mcp.allowed_cwd_roots` if the client must be
prevented from selecting a different host path.

### Shared harness pool

The feature pins the harness's `HOME` to `/home/wraptool-harness` and mounts
the host **harness pool**'s `home/` (`$XDG_DATA_HOME/wraptool/harness-pool/home`,
e.g. `~/.local/share/wraptool/harness-pool/home`) onto it, read-write, so the
harness's auth, skills, and config persist **across every project** — you log
in and install skills once, not per container. The pool's `runtime/` — the
version-locked harness binary — is mounted read-only onto
`/opt/wraptool-harness/runtime`; its `bin/` is appended to `PATH` by the
feature's `containerEnv`. `wraptool up` bind-mounts `home/` for you and
verifies the `runtime/` mount is correctly declared (it can't inject a
read-only mount itself — the devcontainer CLI's `--mount` flag has no
read-only field); a standalone "Reopen in Container" declares both mounts
itself (the scaffolder prefills them).

`"connect": false` on the feature wires the MCP config yourself (e.g. host-side
`wraptool init`) instead of at container-create.

See [Dev containers for harnesses](devcontainers.qmd) for the full feature
reference (every option, the base-image-plus-features rationale, and the
`wraptool up`/`down` lifecycle).

### Legacy hand-rolled example

[`examples/devcontainer/`](https://forge.snamellit.com/pti/wraptool/src/branch/main/examples/devcontainer)
is retained as a transport and isolation smoke-test fixture. Its hand-written
setup predates the merged `wraptool` feature and current harness configuration;
do not copy it as a new project template. Use `wraptool init devcontainer` for a
current starting point.

## Security Checklist

Before using this setup in production:

- [ ] **Policy is minimal** — only allow subcommands the agent actually needs
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
git push origin main           # git is installed but has no credentials

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
ExecStart=%h/.local/bin/wraptool server start
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload
systemctl --user enable --now wraptool
```

Reload the policy without restarting:

```bash
systemctl --user reload wraptool   # sends SIGHUP
```
