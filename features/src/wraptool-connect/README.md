# wraptool-connect (Dev Container Feature)

Wires a dev container to a **host-side wraptool** MCP server, so an AI coding
client inside the container uses credential-isolated CLI tools (git, gcloud,
kubectl, ...) that run on the host. The feature is **image-agnostic**: it adds no
tools and makes no assumptions about your base image — it only writes the MCP
client config and points it at the host.

## What it does

At `postCreate` it writes the MCP client config (Claude Code `.mcp.json` in the
workspace) pointing at wraptool, with a bearer token when one is supplied. It
finds the host automatically as the container's **default-route gateway** — no
`host.docker.internal`, no `--add-host`, works across any docker network (so
multiple worktrees on different networks just work).

## Usage

```jsonc
// .devcontainer/devcontainer.json — YOUR image and tools, unchanged
{
  "image": "mcr.microsoft.com/devcontainers/base:bookworm",
  "features": {
    "forge.snamellit.com/pti/wraptool/wraptool-connect:0": {}
  },
  // REQUIRED: the feature reads these at postCreate but cannot inject them
  // itself (feature-level containerEnv is not variable-substituted). Set them at
  // the top level, where ${localWorkspaceFolder}/${localEnv:...} resolve.
  "containerEnv": {
    "WRAPTOOL_CWD": "${localWorkspaceFolder}"
  },
  // The token is a secret — use remoteEnv (runtime, reaches postCreate) so it is
  // NOT baked into an image layer as containerEnv would be.
  "remoteEnv": {
    "WRAPTOOL_TOKEN": "${localEnv:WRAPTOOL_TOKEN}"
  }
}
```

On the host, export the token before opening the container so it is injected:

```bash
export WRAPTOOL_TOKEN=$(cat ~/.config/wraptool/auth_token)
# VS Code: "Reopen in Container"  — or:  devcontainer up --workspace-folder .
```

`wraptool up` (the orchestrator) sets `WRAPTOOL_TOKEN` and `WRAPTOOL_CWD` for you;
the top-level `WRAPTOOL_CWD` line above is only needed for the plain VS Code flow.

## Options

| Option | Default | Description |
|--------|---------|-------------|
| `harness` | `claude` | MCP client to configure (Claude Code `.mcp.json`). More to follow. |
| `port` | `8717` | Host wraptool port, used when `url` is auto-detected. |
| `url` | `""` | Explicit base URL (e.g. `http://172.18.0.1:8717`). Empty = auto-detect via default-route gateway. |

## Inputs (environment)

Set these in the consumer's **top-level** config (the feature reads them at
postCreate but cannot inject them — feature-level `containerEnv` is not
variable-substituted):

| Var | Where | Set to | Purpose |
|-----|-------|--------|---------|
| `WRAPTOOL_TOKEN` | `remoteEnv` | `${localEnv:WRAPTOOL_TOKEN}` | Bearer token (secret → remoteEnv, not baked); omit if the server trusts localhost. |
| `WRAPTOOL_CWD` | `containerEnv` | `${localWorkspaceFolder}` | Host workspace path, sent to the server as `?cwd=`. |

If `WRAPTOOL_TOKEN`/`WRAPTOOL_CWD` are empty the feature still writes a config and
warns; the server then can't authenticate or scope tools to this worktree.

An existing `.mcp.json` is merged (via `jq` when present, preserving other MCP
servers) or backed up to `.mcp.json.wraptool-bak` before writing.

## Networking

The host is reached at the container's default-route gateway, so wraptool must
listen where that gateway can reach it — bind the docker gateway or `0.0.0.0`,
**not** host `127.0.0.1`. See `docs/isolated-environments.md`.

## Test

```bash
sh features/test/wraptool-connect/selftest.sh   # no docker / devcontainer CLI needed
```

## Publishing

The `features/` tree follows the standard `src/<id>` layout and is published to
Forgejo's OCI registry by `.forgejo/workflows/publish-feature.yml`. To release,
bump `version` in `devcontainer-feature.json` and push a matching tag:

```bash
git tag wraptool-connect-v0.1.1 && git push origin wraptool-connect-v0.1.1
```

The CI runs the selftest, logs in to `forge.snamellit.com`, and runs
`devcontainer features publish` (equivalent to running it locally):

```bash
devcontainer features publish ./features/src \
  --namespace pti/wraptool --registry forge.snamellit.com
```

Consumers then reference `forge.snamellit.com/pti/wraptool/wraptool-connect:0`.
The package is public (anonymous `read:package`), so no registry login is needed
to consume it.
