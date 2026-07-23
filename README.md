# wraptool

An MCP server that wraps CLI tools behind a security whitelist, so AI
coding assistants can use tools like `git`, `gcloud`, and `kubectl`
without direct access to credentials.

## Why

AI coding assistants need CLI tools to be productive, but giving them
direct access exposes API keys, SSH keys, kubeconfigs, and service
account credentials to the model context. The same risk runs through
everyday development: a `postinstall` hook or a compromised transitive
dependency executes with your full credentials the moment you build,
test, or bump a package. wraptool **treats everything running in your
workspace — the assistant, its tool calls, and the build/test/dependency
code they trigger — as untrusted**, and bounds what it can reach:

- **Credentials stay isolated** -- wraptool runs as a separate process
  with its own environment; secrets never enter the AI context.
- **Bounded blast radius** -- inside `wraptool up`'s throwaway container
  the assistant, your build scripts, and the dependencies they pull in
  all run with no host credentials. A hijacked agent *or* a poisoned
  dependency can't steal your credentials or exceed the whitelist: raw
  host secrets (SSH keys, gcloud, kubeconfig, push access) never leave the
  host, and privileged `git`/`kubectl` happen only through wraptool's
  whitelisted, constrained subcommands. A malicious payload still runs
  with the container's network and source tree, and can invoke the same
  approved tools the agent can — but policy, not the payload, bounds which
  operations reach the host.
- **Subcommand-level whitelisting** -- allow `git commit` but deny
  `git push --force`, with flag-level granularity and value
  constraints.
- **Auto-discovery** -- wraptool parses `--help` output to generate
  MCP tool schemas with real descriptions.
- **Structured denial workflow** -- denied commands trigger a
  capability-request flow that an operator approves from the CLI, so
  escalation always requires a human.

## Quick start

Build it (on Guix: `guix shell -m manifest.scm -- go build -o wraptool .`):

```bash
go build -o wraptool .
```

Create `~/.config/wraptool/config.yaml` listing the tools to expose (allow/deny
rules, auth token) — see [Getting started](docs/getting-started.qmd) for a full
example. Then pick how your AI assistant reaches wraptool:

### 1. One isolated container per project — recommended

```bash
wraptool up          # or just `wraptool`, from any git repo
```

Brings up a credential-isolated container — a native **Guix container** built from
your `manifest.scm` when Guix is available, otherwise a **Dev Container** — with
your coding assistant inside it (installed once: a shared pool for Guix, the
`wraptool` feature for Dev Containers) wired to the wraptool server on the host.
Secrets never enter the container: `git push`, `gcloud`, and `kubectl` run on the
host through MCP. The host gateway is auto-detected — no URL to set.

### 2. Scaffold a committed Dev Container

```bash
wraptool init devcontainer --harness claude --lang go,node
```

Writes `.devcontainer/devcontainer.json` — base image + language features + the
`wraptool` feature (installs the harness, writes its MCP wiring, and shares your
harness login/skills across projects). Commit it; teammates get the same isolated
box on "Reopen in Container" or `wraptool up`.

### 3. Minimal — assistant on the host, no container

```bash
wraptool init --harness claude   # writes .mcp.json pointing at localhost
wraptool serve
```

The assistant runs on the same host as wraptool, so `127.0.0.1:8717` is reachable.
From **inside a container it is not** — loopback is the container's own, not the
host's; use option 1 or 2, which detect the host gateway for you.

## Web UI

An optional operator-facing dashboard (bound to `127.0.0.1`, **not**
AI-facing) reviews capability requests and provides full rule CRUD —
edit a tool's allow/deny rules in the browser and the running server
hot-reloads the change. See [`docs/web-ui.qmd`](docs/web-ui.qmd).

![wraptool Web UI — the Rules view, with per-tool allow/deny rules and inline edit controls](docs/rules_view.png)

## Documentation

Full documentation lives in `docs/`, is rendered with
[Quarto](https://quarto.org/), and is published at
<https://pti.pages.snamellit.com/wraptool/> from the Forgejo `pages` branch:

| Page | Source |
|------|--------|
| Getting started | [`docs/getting-started.qmd`](docs/getting-started.qmd) |
| Installation | [`docs/installation.qmd`](docs/installation.qmd) |
| Configuration reference | [`docs/configuration.qmd`](docs/configuration.qmd) |
| CLI reference | [`docs/cli-reference.qmd`](docs/cli-reference.qmd) |
| MCP integration | [`docs/mcp-integration.qmd`](docs/mcp-integration.qmd) |
| Architecture | [`docs/architecture.qmd`](docs/architecture.qmd) |
| Security model | [`docs/security.qmd`](docs/security.qmd) |
| Hot-reload & capability requests | [`docs/hot-reload.qmd`](docs/hot-reload.qmd) |
| Request-review Web UI | [`docs/web-ui.qmd`](docs/web-ui.qmd) |
| Isolated environments | [`docs/isolated-environments.md`](docs/isolated-environments.md) |
| Troubleshooting | [`docs/troubleshooting.qmd`](docs/troubleshooting.qmd) |
| Contributing | [`docs/contributing.qmd`](docs/contributing.qmd) |

### Build the site locally

```bash
quarto preview docs
quarto render docs
```

The render writes `docs/_site/`, which is ignored and must not be committed to
`main`. The deployment workflow publishes only that generated tree to the
orphan `pages` branch.

## License

GPLv3 -- see [LICENSE](LICENSE).
