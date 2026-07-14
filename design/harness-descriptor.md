# Design: a shared harness descriptor registry

Status: **implemented (phases 1–3)** — PRs #15, #16, + this; phase 4
(connect.sh → `wraptool init`) is a follow-up.
Scope: unify "how to point coding-harness *X* at the wraptool MCP server" into
one registry shared by `wraptool init`, `wraptool up` (both the guix and
devcontainer runtimes), and — eventually — the `wraptool-connect` feature.
Today that logic exists three times, diverging; adding a harness (opencode, pi,
agy, …) means reimplementing it in each. This makes adding a harness a
one-descriptor change. **MCP wiring only** — harness *binary provisioning*
(channel package vs persisted install) is a separate concern (§7).

## 1. Problem — three diverging implementations

The same job (write an MCP client config that points a harness at wraptool) is
implemented in three places, and they disagree on what they support:

| Site | Harnesses | Auth token | CWD scoping | Merge-preserving |
|---|---|---|---|---|
| `cmd/init.go` (`harnessGenerators`) | **7** (agy, claude, cursor, opencode, gemini, roo, windsurf) | **no** | `appendCWD` | yes (`writeConfig`/`mergeJSONConfig`) |
| `cmd/guixcontainer.go` (`writeMCPConfig`) | **claude only** | **yes** (Bearer) | `?cwd=` | backup-once, no merge |
| `features/src/wraptool-connect/connect.sh` | **claude only** | **yes** (Bearer) | `?cwd=` | jq-merge if present |

Consequences:

- The rich registry (`init.go`) is the *only* place that knows all 7 harnesses,
  but it is the *only* one that can't emit an auth token — so it can't be used
  for the networked container path as-is.
- The two paths that *do* carry a token (`writeMCPConfig`, `connect.sh`) each
  hard-code Claude. Adding opencode/pi/agy to the container means writing each
  harness's format a second and third time.
- Adding a harness today touches ≥2 unrelated implementations, in two languages.

## 2. What actually varies per harness (the descriptor)

The per-harness surface is small and well-defined — it is exactly what the
`init.go` generators already encode. Distilled from the current code:

| Harness | Config path (project / global) | Top-level key | Server entry (sse) |
|---|---|---|---|
| claude | `.mcp.json` / `~/.claude.json` | `mcpServers` | `{type: sse, url}` |
| cursor | `.cursor/mcp.json` | `mcpServers` | `{url}` |
| opencode | `opencode.json` / `~/.config/opencode/config.json` | **`mcp`** | `{type: remote, url, enabled: true}` |
| gemini | `.gemini/settings.json` | `mcpServers` | `{httpUrl}` |
| roo | `.roo/mcp.json` (no global) | `mcpServers` | `{type: sse, url}` |
| windsurf | `~/.codeium/windsurf/mcp_config.json` (global only) | `mcpServers` | `{serverUrl}` |
| agy | `.agents/mcp_config.json` / … | `mcpServers` | `{serverUrl}` |

Three independent axes vary — **path**, **top-level key** (`mcpServers` vs
`mcp`), and **server URL key** (`url` / `serverUrl` / `httpUrl`) plus extra
fields (`type`, `enabled`). That irreducible variance *is* the descriptor. Note
it does **not** currently include auth — see §5.

## 3. Design — `internal/harness`

Move the registry into a shared package both commands import.

```go
package harness

// Params is everything any generator might need. init and up populate it;
// unused fields (Token/CWD for a purely-local stdio setup) stay zero.
type Params struct {
    Transport   string // "stdio" | "sse" | "unix"
    BaseURL     string // e.g. http://127.0.0.1:8717  (sse/unix)
    SocketPath  string // unix transport
    Token       string // bearer; "" = none (localhost-trust)
    CWD         string // HOST worktree path -> ?cwd= scoping
    WraptoolBin string // stdio transport
    ConfigFile  string // stdio transport
    Global      bool   // user-level vs project-level path
}

// Descriptor is one harness's knowledge. Render returns where its MCP config
// lives and the bytes to write (merge-preserving handled by the caller/registry).
type Descriptor struct {
    Name string
    Doc  string
    Auth AuthMode          // see §5
    Render func(Params) (path string, content []byte, err error)
}

var Registry = map[string]Descriptor{ /* claude, cursor, opencode, ... */ }

// Write renders + merges into an existing config (preserving other servers),
// atomically. The single writer for every caller.
func Write(name string, p Params) (path string, err error)
```

The current `harnessGenerator` funcs become `Descriptor.Render` almost verbatim;
`writeConfig`/`mergeJSONConfig`/`mustJSON`/`appendCWD` move into the package.

## 4. Callers collapse onto it

- **`cmd/init.go`** — becomes a thin CLI over `harness.Registry` / `harness.Write`
  (no behaviour change; it already works this way internally).
- **`cmd/guixcontainer.go`** — delete `writeMCPConfig`; call
  `harness.Write(selected, Params{Transport:"sse", BaseURL:probeAddr(listen),
  Token:token, CWD:root})` to write into the shared workspace. Claude stops
  being special-cased; any registry harness works in the container.
- **`connect.sh`** — keep for the *standalone* devcontainer-feature path (VS Code
  "Reopen in Container" with no `wraptool` process driving it). When `wraptool
  up` drives the devcontainer, prefer the host-side `harness.Write` (the
  workspace is bind-mounted, same as the guix path). **Follow-up (§8.4):**
  have `connect.sh` shell out to `wraptool init` (the binary is on the host)
  to retire the shell reimplementation and reach a single source.

## 5. The auth gap — model it, don't paper over it

The `init.go` generators emit **no token**. That is fine for a localhost stdio
setup but **wrong for the networked container**, which must authenticate. So the
descriptor must model auth as a first-class, per-harness capability:

```go
type AuthMode int
const (
    AuthNone   AuthMode = iota // localhost-trust only
    AuthHeader                 // Authorization: Bearer <token> on the server entry
    AuthQuery                  // token folded into the URL (last resort)
    AuthUnsupported            // client cannot carry auth to a remote MCP server
)
```

Claude is `AuthHeader` (`headers.Authorization`). For the others we must
**verify** whether their MCP client accepts an auth header on a remote server —
that verification is the real per-harness integration work, and the type makes
it explicit. If a selected harness is `AuthUnsupported` and a token is set,
`harness.Write` **errors loudly** ("harness X cannot authenticate to a remote
wraptool; use a localhost/stdio setup or a socket bridge") instead of silently
writing a config that will 401. This is the honest failure the current
claude-only paths dodge by only supporting claude.

## 6. Harness selection

- `wraptool up --harness <name>` (default `claude`), mirroring `init --harness`.
- Or read it from the wraptool config / the `wraptool-connect` feature's existing
  `harness` option, so `up` and the feature agree.
- `up` validates the name against `harness.Registry` up front (clear error listing
  supported names) — same UX `init` already gives.

## 7. Provisioning — a shared developer harness pool (decoupled axis)

This descriptor is **MCP wiring only**. Getting each harness *binary* into the
container is a separate axis, and the decision is: **the harness is the
developer's toolbox, not the project** — so wraptool neither packages harnesses
(it must not inherit N agents' release cadences) nor puts them in `manifest.scm`
(that is the project toolchain, and would bloat CI/dev).

Instead, a **developer-scoped shared pool** — `$XDG_DATA_HOME/wraptool/harness-pool/`,
outside any repo — holds the harness binaries (`bin/`) and their own service
config/auth (`home/`, used as the harness `HOME`). `wraptool up` mounts it into
every container **split by trust**: `bin/` **read-only** (`--expose`) and appended
(not prepended) to PATH, `home/` **writable** (`--share`). Combined with
`--emulate-fhs`, native binaries just run. Because the pool is shared across all
projects and worktrees, a harness is installed **once per developer** (via its own
installer — npm/curl/…, run host-side by `wraptool harness install`), and wraptool
tracks zero harness versions.

**Why the split — the pool is a cross-project trust domain.** It is shared, rw in
part, and survives container teardown, so a compromised agent in one project could
otherwise poison what *every other* project runs. The mount policy narrows that:
`bin/` read-only + PATH-appended means a poisoned pool cannot introduce or shadow
the binaries (`git`/`go`/`bash`/the harness itself) another project's container
executes — installs are a deliberate host-side act, never something the in-container
agent can do. `home/` must stay writable for login/config to persist, so it remains
a residual channel: a harness's own config there (e.g. hooks, MCP entries) runs in
every session, and its service credential is readable by any project's agent. That
is an accepted cost of "install/login once per developer," not host-secret exposure
(SSH/gcloud creds are never mounted — see below); curate `home/` accordingly.

**Curate the pool; never mount `$HOME`.** A harness needs its *own* service
credential (e.g. Claude Code's Anthropic key) to function — a *developer*
credential that legitimately belongs in the container. That is categorically
distinct from *project/host* secrets (SSH keys, gcloud tokens), which stay on the
host and are reached only through wraptool's policed MCP tools. Three tiers:

| Tier | Example | Delivery |
|---|---|---|
| Project toolchain | go, node, git binary | `manifest.scm` → container |
| Developer toolbox | claude/opencode binary + its API key | shared pool → mounted in |
| Host/project secrets | SSH keys, gcloud creds | host only, via wraptool MCP |

The two axes stay decoupled: this doc makes N harnesses cheap to **wire** (one
descriptor row); the shared pool makes them cheap to **provision** (drop the
binary in once). A dedicated provisioning design can expand the pool mechanics;
the descriptor deliberately carries no fetch logic.

### 7a. Provisioning on a devcontainer host (no guix)

The pool as described (`bin/` read-only mount + `home/` writable mount) is the
**guix runtime**. A devcontainer host has no guix to run the FHS-emulated pool
installers, and the pool `bin/` is never mounted into a devcontainer. So the two
axes get **different mechanisms** there, chosen to be devcontainer-native:

| Axis | Guix runtime | Devcontainer runtime |
|---|---|---|
| Binary | shared pool `bin/`, mounted read-only | **`wraptool-harness` feature** — installed per image at build time (`npm i -g` / the harness's own script). npm runs in-image where node lives, so the guix FHS/detect-libc workaround never applies. |
| State  | shared pool `home/`, mounted writable | **same shared pool `home/`**, bind-mounted onto the feature's fixed `HOME` (`/home/wraptool-harness` — user-space, not system `/opt`). Login/skills/config persist across every project — the toolbox value. |

`wraptool up` (devcontainer path) adds the `home/` bind mount on `devcontainer
up`; standalone "Reopen in Container" declares the same mount + feature in
`devcontainer.json` (see `examples/devcontainer`). Two devcontainer-specific
requirements, both verified end-to-end against `mcr.microsoft.com/devcontainers/base:debian`
+ the node feature:

- **`HOME` must be set via `containerEnv`, not `remoteEnv`.** `devcontainer exec`
  runs a login-shell user-env probe that reverts `remoteEnv.HOME` to the base
  user's home, so the harness silently writes to `/home/vscode` (unshared).
  `containerEnv` bakes `HOME` into the image env, which the probe and every exec
  inherit — confirmed `HOME=/home/wraptool-harness` in both `devcontainer exec`
  and raw `docker exec printenv`.
- **`installsAfter: [node]`.** Without it the feature can install before the node
  feature, so `npm` is absent and the node guard aborts the build.

Ownership: the container user (`vscode`, uid 1000) matches a typical host dev
account (uid 1000), so writes to the bind-mounted host pool succeed. On a uid
mismatch the harness can't write shared state — a future refinement if usage
surfaces it.

Trust model is unchanged:
`home/` stays the shared writable residual-risk channel (§7); the binary is now
**per-image** (isolation *stronger* than the read-only bin mount — no shared bin
surface at all). Per-image binaries can drift in version while sharing one
`home/` state; pin the version in the feature option for determinism.

`wraptool harness install` (guix pool populate) is intentionally **not** extended
to the devcontainer world — off guix it errors and points at the feature, since
the feature *is* the devcontainer delivery path.

**The isolation boundary is the host source, not the container path.** The pool
lives at `paths.DataDir()/harness-pool/home` — XDG data (`~/.local/share/...`),
one per developer account. Different OS accounts (e.g. per customer) have
different `$HOME`, hence different pools, hence no cross-account leakage. The
in-container mount target (`/home/wraptool-harness`) is namespace-private to a
single-user throwaway container and carries no sharing semantics; it is a
dedicated user-space HOME chosen over `/opt` only to avoid the "system-shared"
misread. State is XDG **data** (not cache): it holds auth/skills that cache
eviction must never wipe.

## 8. Phased plan

1. ✅ **Extract, no behaviour change** — PR #15. Registry + writers in
   `internal/harness`; `Descriptor.Render(Params)`; `init.go` is a thin CLI.
2. ✅ **Auth + container path** — PR #16. `Token`/`CWD`/`Dir` + `AuthMode`;
   claude Bearer header; `bringUpGuixContainer` writes via `harness.Write`
   (`writeMCPConfig` deleted); parity test retargeted.
3. ✅ **Selection + more harnesses** — `up --harness <name>` (default claude).
   Auth verified against each tool's docs: **agy, claude, cursor, gemini,
   opencode, windsurf** all take `headers: {Authorization: "Bearer …"}` →
   `AuthHeader`. **roo** accepts a headers object but a known bug drops it on
   SSE (RooCodeInc/Roo-Code#2480) → left `AuthUnverified` (a token fails loudly).
   **`pi`** is not yet a descriptor — its tool identity / config schema is
   unknown; adding it is a single registry row once identified.
4. **Retire the shell reimpl (follow-up).** `connect.sh` shells out to
   `wraptool init`, collapsing three implementations to one.

## 9. Testing

- **Per-harness golden files**: `(path, content)` for each descriptor × transport
  (stdio/sse), asserting the top-level key and URL-key variance from §2.
- **Auth**: token set → `AuthHeader` harness emits the Bearer header;
  `AuthUnsupported` harness returns an error.
- **Parity (retargeted)**: `harness.Write("claude", …)` ⟷ `connect.sh` — the
  existing semantic-parity test, pointed at the registry.
- **Merge-preserving**: writing wraptool into a config that already has another
  MCP server keeps the other server.

## 10. Why this is the scalable line

wraptool's job is credential isolation + MCP policing. The bounded, cheap
per-harness surface is the **MCP descriptor** (path + format + auth mode) — a
table that grows one row per harness. Everything expensive and fast-moving (the
agent binaries, their release cadence) is delegated (§7). Adding opencode/pi/agy
becomes: one descriptor row + a one-time auth-capability check, reusing the same
isolation, `-F`, workspace-share, token, and `?cwd=` plumbing that already works.
