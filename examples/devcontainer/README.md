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
   wraptool serve
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
