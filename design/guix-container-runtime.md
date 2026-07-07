# Design: Guix-container runtime for `wraptool up`

Status: **proposal**
Scope: give `wraptool up` a second container backend that uses native Guix
containers (`guix shell --container`) instead of the `devcontainer` CLI, so a
Guix host can bring up an isolated, credential-scrubbed dev environment with no
Docker daemon and no image build — the toolchain coming from the same
`manifest.scm` that already feeds CI. Selection is automatic: **Guix first,
`devcontainer` as the fallback.** No change to the server, the policy engine, or
the existing devcontainer path.

## 1. Problem

`wraptool up` (`cmd/up.go`) brings up an isolated coding environment by shelling
out to the `devcontainer` CLI: it ensures the shared MCP server, then runs
`devcontainer up` against `<root>/.devcontainer`, injecting `WRAPTOOL_TOKEN` +
`WRAPTOOL_CWD` so the in-container harness talks to the host-side wraptool over
the Docker bridge. That path assumes Docker + the `@devcontainers/cli` npm
package + an image build.

On a Guix host this is the wrong shape:

- Isolation is *already* a first-class primitive — `guix shell --container`
  gives PID/mount/IPC/user-namespace isolation, rootless, with only the store
  plus explicitly-shared paths visible. No daemon, no image.
- The dev toolchain is *already* declared in `manifest.scm` (the single source
  of truth CI consumes). A Docker image re-derives that same toolchain a second
  way, from a different base (`debian:bookworm-slim` in
  `examples/devcontainer/Dockerfile`), drifting from the Guix definition.
- Pulling in Docker + Node's `@devcontainers/cli` just to get a namespace is
  pure ceremony where `guix` is on `$PATH`.

The goal is to make the Guix-native path the default *when it is available*,
while leaving `devcontainer` intact for Arch and every other distro.

## 2. Goal

- `wraptool up` in a repo/worktree on a Guix host brings up an isolated
  container from `manifest.scm` and wires the in-container harness to the host
  wraptool — no Docker, no image build.
- **Credential isolation is preserved or improved**: the host `$HOME`, ssh
  keys, cloud tokens, and environment are not visible inside the container;
  privileged tools (git, gcloud, kubectl, …) run host-side and are reachable
  only through wraptool's whitelisted MCP tools. Same threat model as the
  devcontainer path.
- **Network access is mandatory.** Modern toolchains pull dependencies from
  external repositories (npm, PyPI, crates.io, Go module proxy, …) at
  development time. The Guix build system is a superior model for this but is
  largely incompatible with those workflows, so the container must have plain
  outbound internet. This is a hard requirement and, as §5 shows, it dictates
  the network mode.
- Selection is automatic and overridable: Guix container when a Guix container
  can be built here, `devcontainer` otherwise.

### Explicitly out of scope (for now)

- Strong *network* isolation combined with internet (the `passt`/`slirp4netns`
  path — see §9, deferred).
- Non-Claude harnesses (mirrors the current `wraptool-connect` limitation).
- `guix system container` (full-OS declarations) — heavier than needed; this
  design uses `guix shell --container` only.

## 3. Security model — what the container does and does not isolate

wraptool's threat model is **credential isolation**: keep the AI harness away
from secrets on the host (files, env, keys, tokens), while letting it *use*
privileged tools indirectly via policed MCP calls. Two isolation axes matter,
and they are independent:

| Axis | Mechanism | Guix container (`-N`, see §5) |
|---|---|---|
| **Filesystem / env** (the actual goal) | mount + user namespace; minimal env | **Strong.** Only `/gnu/store` + the `--share`d workspace are visible. Host `$HOME`, `~/.ssh`, `~/.config`, cloud creds: **not mounted**. Env starts empty; only explicitly-preserved `WRAPTOOL_*` vars pass. |
| **Network** | net namespace | **Weak under `-N`.** The container shares the host network namespace, so host `127.0.0.1` services are reachable from inside (verified in §10). |

The key insight driving the whole design: **credential isolation is a
filesystem/env property, not a network property.** It holds regardless of the
network mode. The network axis is a secondary, defence-in-depth concern here —
it is *not* what stops the harness reading `~/.aws/credentials` (the mount
namespace does that). So accepting weak network isolation to satisfy the
mandatory-internet requirement does not compromise the primary goal.

Residual risk under `-N` and its mitigations are in §7.

## 4. Current architecture we reuse

From `cmd/up.go`, unchanged and backend-agnostic:

- `runUp` → `ensureServer` (idempotent, flock-guarded, requires `sse`) →
  `readAuthToken` → *bring up container*.
- `ensureServer` already demands a network-reachable transport; the server
  listens on `sse` (`0.0.0.0:8717`) or `unix`.
- The container-facing contract is two env vars: `WRAPTOOL_TOKEN` (bearer) and
  `WRAPTOOL_CWD` (the **host** worktree path, echoed back to the server as
  `?cwd=` so tools are scoped to this worktree).
- `wantShell` / `isTTY`, `inContainer`, and the server-supervision block are
  transport- and backend-agnostic.

Only `bringUpContainer` is devcontainer-specific. This design adds a sibling and
a selector; everything above the seam is reused verbatim.

## 5. Why the network mode is forced (spike-verified)

`guix shell --container` offers a **binary** network choice — there is no
docker-style bridge-with-NAT, only:

| mode | flag | internet | net isolation | wraptool reach |
|---|---|---|---|---|
| isolated netns (default) | *(none)* | ❌ `lo` only | **strong** — host loopback `REFUSED` | via shared unix socket + `socat` |
| shared host netns | `-N` / `--network` | ✅ verified HTTP 200 | **weak** — host loopback `CONNECTED` | direct `127.0.0.1:8717` |

Both rows were verified empirically (§10). Because **internet is mandatory
(§2)** and `guix shell --container` has no built-in userspace-NAT mode, the
container **must** run with `-N`. That collapses two earlier design questions:

1. **Transport.** With `-N`, the container shares the host loopback, so the
   harness reaches wraptool directly at `http://127.0.0.1:8717/sse`. The elegant
   isolated-netns trick (wraptool `unix` transport + a `socat` loopback proxy
   over a `--share`d socket) buys nothing here — host loopback is already shared
   — so v1 uses plain **SSE on loopback**. The unix-socket+proxy recipe is
   retained only for the future strict-net mode (§9) where it earns its keep.
2. **`host.docker.internal` / gateway detection** disappears — there is no
   bridge. `connect.sh`'s gateway parsing is bypassed by setting `WT_URL`
   explicitly.

## 6. Proposed design

### 6.1 Runtime selection

New `--runtime` flag on `up`: `auto` (default) | `guix` | `devcontainer`.

```
func selectRuntime(mode, root string) (runtime, error) {
    switch mode {
    case "guix":         return runtimeGuix, nil
    case "devcontainer": return runtimeDevcontainer, nil
    case "auto", "":
        // Guix first: needs the guix CLI AND a manifest to build from.
        if hasGuix() && fileExists(filepath.Join(root, "manifest.scm")) {
            return runtimeGuix, nil
        }
        return runtimeDevcontainer, nil
    default:
        return 0, fmt.Errorf("unknown --runtime %q (auto|guix|devcontainer)", mode)
    }
}

func hasGuix() bool { _, err := exec.LookPath("guix"); return err == nil }
```

Rationale for the `auto` predicate (per the maintainer's directive, *Guix
first, devcontainer if Guix is unavailable or there is no `manifest.scm`*):
`guix` on `$PATH` proves the host can build a container; `manifest.scm` at the
worktree root proves there is a toolchain to put in it. Absent either, fall
back. `runUp` calls `selectRuntime` after resolving `root` and dispatches to
`bringUpGuixContainer` or the existing `bringUpContainer`.

### 6.2 Container bring-up

```
func bringUpGuixContainer(root, token string, shell bool) error {
    manifest := containerManifest(root) // see §6.4
    if manifest == "" {
        return fmt.Errorf(
            "no container manifest found (looked for %s)\n\n"+
            "Scaffold one with `wraptool up --scaffold-guix` (writes .wraptool/container.scm), "+
            "then re-run.", strings.Join(candidateManifests(root), ", "))
    }

    // The harness reads .mcp.json from the workspace root. The workspace is
    // shared writable into the container, so write the connection config on the
    // host before entering — no in-container jq/connect.sh needed.
    url := "http://127.0.0.1:8717" // -N shares host loopback; server on :8717
    if err := writeMCPConfig(filepath.Join(root, ".mcp.json"), url, token, root); err != nil {
        return err
    }

    args := []string{
        "shell", "--container", "--network",
        "--share=" + root + "=" + root,      // workspace rw, host path preserved → ?cwd= matches
        "--preserve=^WRAPTOOL_",             // forward WRAPTOOL_TOKEN / _CWD / _CONTAINER
        "-m", manifest,
        "--", "bash", "-lc",
        // Mark the shell so a nested `wraptool up` is refused (§6.3); land in the
        // workspace; keep the container alive with an interactive shell.
        "export WRAPTOOL_CONTAINER=1; cd " + shellQuote(root) + "; exec bash -l",
    }

    env := append(os.Environ(),
        "WRAPTOOL_TOKEN="+token,
        "WRAPTOOL_CWD="+root,
        "WRAPTOOL_CONTAINER=1", // preserved into the container by --preserve
    )
    cmd := exec.Command("guix", args...)
    cmd.Env = env
    cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
    // ... same shell-vs-detached handling as bringUpContainer (wantShell) ...
    return cmd.Run()
}
```

Notes:

- `--share=$root=$root` keeps the workspace at its **host path** inside the
  container, so `WRAPTOOL_CWD=$root` is a path the server already understands
  for `?cwd=` scoping. (A remapped path would break worktree scoping.)
- `--preserve=^WRAPTOOL_` is a regexp; guix containers strip the environment
  otherwise, which is exactly the credential-isolation property we want.
- Unlike the devcontainer path there is **no image build and no feature
  install** — `guix shell` realizes the manifest profile (cached after first
  run) and enters.

### 6.3 `inContainer` detection

`inContainer` (the guard that refuses `wraptool up` *inside* a container)
currently checks `/.dockerenv` and `$REMOTE_CONTAINERS`. A Guix container sets
neither. Add the marker this design injects:

```
func inContainer() bool {
    if _, err := os.Stat("/.dockerenv"); err == nil { return true }
    if os.Getenv("REMOTE_CONTAINERS") != "" { return true }
    if os.Getenv("WRAPTOOL_CONTAINER") != "" { return true } // guix-container marker
    return false
}
```

The entry command exports `WRAPTOOL_CONTAINER=1`, so a nested `up` is refused as
it is under devcontainer.

### 6.4 Container manifest — the harness runtime, not the toolchain

Critical distinction: the *dev toolchain* manifest (`manifest.scm`: go, git,
golangci-lint) is **not** what the container runs. The container runs the **AI
harness** and deliberately has **no git and no credentials** — identical intent
to `examples/devcontainer/Dockerfile`. Privileged tools stay host-side.

`containerManifest(root)` searches, in order: `.wraptool/container.scm`, then
`container.scm`. Scaffolded template:

```scheme
;; Runtime for the wraptool Guix dev container (`wraptool up`, guix runtime).
;; Deliberately has NO git and NO credentials — privileged tools run on the host
;; and are reached only through wraptool's MCP tools. This is the harness box,
;; not the toolchain (that is manifest.scm).
(specifications->manifest
 '("node"        ; Claude Code
   "coreutils"   ; the harness/shell prep expects mkdir/mv/…
   "bash"
   "nss-certs")) ; CA roots for HTTPS dependency pulls (npm/pip/cargo/…)
```

Open item for Phase 1: whether `node`-based Claude Code needs `guix shell -F`
(FHS emulation) for native addons / hard-coded `/usr/bin/env`. Verify before
committing to a flag (§8, §10 leaves this unverified — no node was exercised in
the spike).

### 6.5 Connection config writer (`writeMCPConfig`)

The devcontainer path writes `.mcp.json` *inside* the container via the
`wraptool-connect` feature's `connect.sh` (needs `jq`, gateway detection). The
Guix path writes it on the **host** into the shared workspace — simpler, no
in-container tooling, no gateway parsing:

```
func writeMCPConfig(path, baseURL, token, hostCWD string) error {
    sse := strings.TrimRight(baseURL, "/") + "/sse?cwd=" + url.QueryEscape(hostCWD)
    server := map[string]any{"type": "sse", "url": sse}
    if token != "" {
        server["headers"] = map[string]string{"Authorization": "Bearer " + token}
    }
    doc := map[string]any{"mcpServers": map[string]any{"wraptool": server}}
    // marshal + atomic write; merge with jq-style preservation is a Phase-2 nicety
    ...
}
```

This duplicates the JSON shape that `connect.sh` produces. `connect.sh` remains
the reference implementation for the devcontainer/feature path;
`writeMCPConfig` is its Go twin for the Guix path. The duplication is small and
both are covered by the same golden-file test (§ Testing). A future refactor
could have `connect.sh` call a `wraptool connect` subcommand to collapse the two
— noted, not required for v1.

## 7. Risks / caveats

- **Weak network isolation under `-N`** (host loopback shared). Mitigations:
  (a) credential isolation is unaffected (§3); (b) document that operators
  running sensitive services should not bind them to `0.0.0.0`/shared loopback
  where a container could reach them; (c) the strict-net mode (§9) removes this
  entirely for those who can forgo the always-on-internet convenience or adopt
  pasta. Surface the caveat in `up`'s output when the guix runtime is selected.
- **Node/Claude Code under Guix** may need `-F` or extra packages (build tools
  for native npm modules). Unverified in the spike; a Phase-1 gate.
- **`guix` not present / no `manifest.scm`** → `auto` silently falls back to
  devcontainer. Good default, but print which runtime was chosen so the fallback
  is not invisible.
- **First-run latency**: realizing the container profile builds/downloads on
  first `up`. Cached thereafter. Comparable to a first `devcontainer` image
  build; call it out in output.
- **`.mcp.json` written into the workspace** — already true for the
  devcontainer path (connect.sh writes `$PWD/.mcp.json`); ensure it is
  gitignored guidance in the scaffold, and back up an existing file once (as
  connect.sh does).

## 8. Effort estimate

Medium, additive, no changes above the bring-up seam:

- `selectRuntime` + `--runtime` flag + `hasGuix` — small.
- `bringUpGuixContainer` — the bulk; mirrors `bringUpContainer`'s structure.
- `inContainer` marker — one line.
- `writeMCPConfig` — small, golden-file tested against connect.sh output.
- `--scaffold-guix` writing `.wraptool/container.scm` — small (template above).
- No Dockerfile, no image publish, no feature. The devcontainer path is
  untouched.

The hard part (crossing the isolation boundary for MCP) is already solved: under
`-N` it is a plain loopback SSE call, verified in §10.

## 9. Deferred: strict-net mode (strong isolation **and** internet)

`passt` (pasta) and `slirp4netns` are both packaged in Guix, so a private
network namespace with userspace NAT — internet without sharing the host netns
— is achievable. But neither is wired into `guix shell --container` (which
offers only `-N`), so it means orchestrating the netns + pasta around the
container ourselves. When built, this is the mode where the **unix transport +
`socat` loopback proxy** (already proven in §10, EXP1) returns: an isolated
container that reaches wraptool only through a `--share`d socket, with pasta
providing outbound internet. Gate it behind `--strict-net`; default stays `-N`.

## 10. Spike evidence (verified)

All commands run on the Guix host, 2026-07-07, against a real
`wraptool serve --transport unix` and `guix shell --container`.

### 10.1 Baseline — wraptool `unix` transport speaks SSE over a socket

```
$ wraptool serve --config wt.yaml   # server.transport: unix, listen: $PWD/wt.sock
$ curl -s --max-time 1 --unix-socket wt.sock http://localhost/sse
event: endpoint
data: http://localhost/message?sessionId=5dbd5160-...
```

### 10.2 EXP1 — isolated netns: strong iso, socket reachable, **no internet**

```
$ guix shell --container --share=$PWD/wt.sock socat curl coreutils bash -- bash -c '
    socat TCP-LISTEN:8717,fork,reuseaddr UNIX-CONNECT:$PWD/wt.sock &
    curl -s --max-time 1 http://127.0.0.1:8717/sse | head -2   # -> event: endpoint  ✅
    curl -s --max-time 3 http://example.com                    # -> (nothing)        ❌
    ls /sys/class/net'                                         # -> lo   (only loopback)
```

Proves the unix-socket + `socat` loopback proxy path works inside a fully
network-isolated container — but the container has no outbound internet.

### 10.3 EXP2 — `-N`: internet works, socket proxy still works

```
$ guix shell --container --network --share=$PWD/wt.sock socat curl bash nss-certs -- bash -c '
    curl -s --max-time 5 -o /dev/null -w "HTTP %{http_code}\n" https://example.com'  # -> HTTP 200 ✅
    # socat proxy to the socket -> event: endpoint  ✅
```

### 10.4 The isolation cost of `-N` — host loopback exposure

Host-only listener on `127.0.0.1:9999`, probed from inside each mode via bash
`/dev/tcp`:

```
isolated netns : REFUSED     # host loopback sealed  → strong net iso
--network (-N) : CONNECTED   # host loopback shared  → weak   net iso
```

This is the empirical basis for §3/§5: `-N` shares the host network namespace.

### 10.5 Toolchain availability

`guix show passt` → `2024_12_11.09478d5`; `guix show slirp4netns` → `1.3.3`
(both present → §9 is feasible). `socat`, `nss-certs`, `node`, `coreutils`,
`bash` all packaged.

Unverified by the spike (Phase-1 gates): Node/Claude Code actually running under
a Guix container (FHS need), and `writeMCPConfig` output matching `connect.sh`.

## 11. Phased plan

1. **Runtime selection + Guix bring-up (happy path).** `--runtime`,
   `selectRuntime`, `hasGuix`, `bringUpGuixContainer` with `-N` + SSE loopback,
   `writeMCPConfig`, `inContainer` marker. Gate: `wraptool up` on this repo
   enters a container, `.mcp.json` points at `127.0.0.1:8717`, and a manual MCP
   call reaches host wraptool. Resolve the Node/`-F` question here.
2. **Scaffolding + DX.** `--scaffold-guix` writes `.wraptool/container.scm`;
   print the chosen runtime and the network caveat; back up an existing
   `.mcp.json`. Gate: fresh repo → scaffold → up, no manual steps.
3. **Parity tests + docs.** Golden-file test `writeMCPConfig` vs `connect.sh`;
   update `CLAUDE.md` and `examples/` with the Guix path. Gate: CI green.
4. **(Deferred) strict-net mode.** pasta-wrapped private netns + unix
   transport + `socat`, behind `--strict-net` (§9).

## 12. Testing

- Unit: `selectRuntime` truth table (guix present/absent × manifest
  present/absent × explicit flag); `writeMCPConfig` golden file (token / no
  token; `?cwd=` URL-encoding of spaces) asserted byte-equal to `connect.sh`'s
  output for the same inputs.
- Integration (Guix host only, tagged): `wraptool up --runtime=guix` against a
  unix- or sse-transport server; assert the in-container harness completes an
  MCP `initialize` + one policed tool call; assert host `~/.ssh` is **not**
  visible inside; assert `WRAPTOOL_CONTAINER` set (nested-up refusal).
- Regression: `--runtime=devcontainer` still drives the existing path
  unchanged.
