# wraptool devcontainer template

A VS Code / CLI **Dev Container** for running an AI coding client (e.g. Claude
Code) in isolation, reaching a wraptool server on the host over MCP. Privileged
tools and credentials (git, gcloud, kubectl, SSH keys, ...) stay on the host;
the container gets only the whitelisted MCP tools.

This is the devcontainer analogue of the Guix (`test-container.sh`) and
Docker/Podman flows in [`docs/isolated-environments.md`](../../docs/isolated-environments.md).

> **On a Guix host, prefer the native runtime.** `wraptool up` auto-selects a
> Guix container (`guix shell --container -m manifest.scm`) when `guix` is on
> `PATH` and the worktree has a `manifest.scm` — no Docker, no image build, no
> extra config: the container is built from your project's own `manifest.scm`,
> so the harness gets the same toolchain you do. This whole devcontainer
> template is only needed off Guix (Arch, macOS, …) or with
> `--runtime=devcontainer`. See
> [`design/guix-container-runtime.md`](../../design/guix-container-runtime.md).

## Files

| File | Role |
|------|------|
| `devcontainer.json` | Dev Container spec: builds the image, maps `host.docker.internal`, injects `WRAPTOOL_URL`/`WRAPTOOL_TOKEN`, runs `setup-mcp.sh`. |
| `Dockerfile` | Minimal debian client image — `curl`, `jq`, CA certs. No git, no credentials. |
| `setup-mcp.sh` | postCreate: writes `~/.config/claude/settings.json` pointing at host wraptool. |
| `test-connection.sh` | Client-side smoke test: isolation + MCP reachability + a tool call. |

## Use

1. Copy this directory into your project as `.devcontainer/`:

   ```bash
   cp -r examples/devcontainer /path/to/your-project/.devcontainer
   ```

2. On the host, start wraptool with SSE + an auth token, listening where the
   container can reach it. `127.0.0.1` is NOT reachable from the container —
   bind the docker gateway or `0.0.0.0` (see the networking note below):

   ```yaml
   # ~/.config/wraptool/config.yaml
   server:
     transport: sse
     listen: 0.0.0.0:8717            # or a specific docker gateway, e.g. 172.17.0.1:8717
     auth_token_file: ~/.config/wraptool/auth_token
   ```

   ```bash
   head -c 32 /dev/urandom | base64 > ~/.config/wraptool/auth_token
   chmod 600 ~/.config/wraptool/auth_token
   wraptool server start
   ```

3. Export the token so the container inherits it, then open the folder in a Dev
   Container:

   ```bash
   export WRAPTOOL_TOKEN=$(cat ~/.config/wraptool/auth_token)
   # VS Code: "Reopen in Container", or the devcontainer CLI:
   devcontainer up --workspace-folder /path/to/your-project
   ```

4. Verify the wiring from inside the container:

   ```bash
   .devcontainer/test-connection.sh
   ```

## Sharing the harness across projects (the developer toolbox)

The harness *binary* is cheap to install per container, but its **state** — auth
token, skills, extensions, MCP config — is a per-developer toolbox you don't want
to rebuild in every project. The `wraptool` feature (`provision: true`) installs the
binary at build time and pins the container's `HOME` to `/home/wraptool-harness`;
bind-mount your host pool onto that path so the state persists across every project.

When `wraptool up` drives the devcontainer it adds the mount for you. For a
standalone "Reopen in Container" (no `wraptool` process), declare it yourself:

```jsonc
// .devcontainer/devcontainer.json
{
  "features": {
    // claude/opencode/pi install via npm, so pull a node runtime too:
    "ghcr.io/devcontainers/features/node:1": {},
    // One feature, one harness choice. provision (install the binary) and
    // connect (write the MCP wiring) both default true; flip either off for
    // BYO-binary or wire-it-yourself setups.
    "forge.snamellit.com/pti/wraptool/wraptool:0": { "harness": "claude" }
  },
  // Share the host harness-state pool (login/skills once, every project):
  "mounts": [
    "source=${localEnv:HOME}/.local/share/wraptool/harness-pool/home,target=/home/wraptool-harness,type=bind"
  ]
}
```

The mount source is your host pool (`$XDG_DATA_HOME/wraptool/harness-pool/home`).
The `~/.local/share/...` above is the **Linux** location; match your OS or the
bind fails on a source that does not exist. Use forward slashes under
`${localEnv:HOME}` on every OS (backslashes are invalid in a JSON string): on
**macOS** `${localEnv:HOME}/Library/Application Support/wraptool/harness-pool/home`,
on **Windows** `${localEnv:HOME}/AppData/Local/wraptool/harness-pool/home`.
`wraptool init devcontainer` and `wraptool up` emit/mount the correct path for
your platform automatically; only a hand-written mount needs this adjustment. Only `home/` (state) is shared — the binary lives
in the image, isolated per project. This mirrors the Guix runtime's split; see
[`design/harness-descriptor.md`](../../design/harness-descriptor.md) §7 for the
trust model (shared writable `home/` is an accepted residual-risk channel; never
mount host `$HOME` or credentials).

## Networking

The container reaches the host at `host.docker.internal`, mapped to the host
gateway via `--add-host=host.docker.internal:host-gateway`. If wraptool binds a
specific docker-network gateway rather than `0.0.0.0`, override the endpoint:

```jsonc
"containerEnv": { "WRAPTOOL_URL": "http://172.18.0.1:8717" }
```

`127.0.0.1` on the host is a different network namespace from the container and
will not work. The wraptool Web UI stays on host `127.0.0.1` — it is operator-
only and never exposed to the container.

## Running the test standalone (no VS Code)

```bash
docker build -t wraptool-client examples/devcontainer
docker run --rm \
  --add-host=host.docker.internal:host-gateway \
  -e WRAPTOOL_URL=http://host.docker.internal:8717 \
  -e WRAPTOOL_TOKEN="$(cat ~/.config/wraptool/auth_token)" \
  -v "$PWD/examples/devcontainer/test-connection.sh:/test.sh:ro" \
  wraptool-client sh /test.sh
```
