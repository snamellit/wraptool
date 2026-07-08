# Design: shared harness pool for the devcontainer runtime

Status: **proposal**
Scope: make the shared developer **harness pool** — already mounted into the
Guix-native container runtime — available to the **devcontainer** runtime too,
delivered through the existing `wraptool-connect` Dev Container Feature (no new
feature). See `guix-container-runtime.md` and `harness-descriptor.md`.

## 1. Problem

The harness pool (`$XDG_DATA_HOME/wraptool/harness-pool`, `bin/` on PATH +
`home/` for harness state) lets a developer install an AI harness **once** and
reuse it across every project, instead of baking it into each image. Today only
the Guix runtime gets it — `cmd/guixcontainer.go` bind-shares the pool and sets
`PATH`/`HOME` in the container shell it controls.

The **devcontainer** path (`cmd/up.go`) does not. It runs `devcontainer up` with
only `WRAPTOOL_TOKEN`, and relies on the `wraptool-connect` feature to wire the
MCP config. The harness binary itself must be installed into the image
per-project (a feature, a Dockerfile line, …) — exactly the duplication the pool
removes for Guix. Two runtimes, two harness stories.

## 2. Goal

- The devcontainer runtime gets the **same** pool: a harness installed once
  (`wraptool harness install <name>`) is on `PATH` in every devcontainer, with
  its auth/state living in the shared pool.
- Delivered by **extending `wraptool-connect`** — the single devcontainer
  integration point already present — not a second feature. The feature that
  wires the MCP connection also mounts the pool.
- Symmetric with Guix: one pool, one harness-config location, driven by one
  registry field, so the two runtimes cannot drift.

## 3. Design

### 3.1 Extend the `wraptool-connect` feature

Dev Container Features may declare `mounts` and `containerEnv` in
`devcontainer-feature.json`. Add both, plus options to gate/locate the pool:

```jsonc
{
  "id": "wraptool-connect",
  "options": {
    "harness": { /* … */ },
    "pool":     { "type": "boolean", "default": true,
                  "description": "Bind-mount the host harness pool and put it on PATH." },
    "poolPath": { "type": "string", "default": "",
                  "description": "Host pool dir; empty = $XDG_DATA_HOME/wraptool/harness-pool." }
  },
  "mounts": [
    { "source": "${localEnv:HOME}/.local/share/wraptool/harness-pool",
      "target": "/wraptool/harness-pool", "type": "bind" }
  ],
  "containerEnv": {
    "PATH": "/wraptool/harness-pool/bin:${containerEnv:PATH}"
  }
}
```

- **Fixed container target** `/wraptool/harness-pool` (not the host path): the
  host pool dir need not exist at the same path inside the image, and the env
  values below are then static.
- **`PATH`** is prepended via `containerEnv` — features compose these across the
  stack, and `${containerEnv:PATH}` keeps the base image's PATH.
- The **existing** `postCreateCommand` (`connect.sh`) is untouched; the pool is
  pure feature metadata (mounts/env), so the Go/`connect.sh` config-writer parity
  is unaffected.

### 3.2 Harness auth/config home — do **not** hijack `HOME`

Guix sets `HOME=poolHome` so the harness writes its auth under the pool. A
devcontainer already has a real `HOME` (a user with dotfiles, tooling); globally
overriding it to a bind-mounted dir is intrusive and can break the container.

Instead, point **only the harness's own config dir** at the pool, per harness.
Add a config-home hint to the registry `Descriptor`:

```go
type Descriptor struct {
    // … Name, Auth, Bin, Install, Render …
    // ConfigHomeEnv is the env var a harness reads for its config/auth dir, and
    // Sub is its path under the pool home. Empty ConfigHomeEnv = the harness has
    // no relocatable config dir (fall back to HOME= for the Guix path only).
    ConfigHomeEnv string // e.g. "CLAUDE_CONFIG_DIR"
    ConfigHomeSub string // e.g. ".claude"
}
```

Then both runtimes set, for the chosen harness, `CLAUDE_CONFIG_DIR =
<pool-home>/.claude` (devcontainer: via `containerEnv`; Guix: in its shell
command) — auth persists in the pool, `HOME` stays the container's own. This
**unifies** the two runtimes on one mechanism and one auth location, and lets
Guix drop its blunt global `HOME=poolHome`.

Harnesses without a relocatable config dir keep the Guix-only `HOME=poolHome`
fallback and are simply not pool-shared in a devcontainer (documented limitation).

### 3.3 Ensure the pool exists before `devcontainer up`

A bind mount whose **source is missing** fails (or Docker creates a root-owned
empty dir). `ensureHarnessPool()` currently runs only on the Guix path. Lift it
so `cmd/up.go` creates the pool (`bin/`, `home/`) on the devcontainer path too,
before invoking `devcontainer up`.

### 3.4 Pool path resolution

The pool is `paths.DataDir()/harness-pool`. The feature mount source uses
`${localEnv:HOME}/.local/share/wraptool/harness-pool` — the XDG default. A
non-default `$XDG_DATA_HOME` is not expandable in the static mount string, so the
`poolPath` option is the escape hatch (and `wraptool up` can pass it when it
scaffolds/knows the path). Document the default assumption.

## 4. Consistency with the Guix runtime

| | Guix runtime | devcontainer (this proposal) |
|---|---|---|
| Pool mount | `--share=pool=pool` | feature `mounts` bind |
| `bin/` on PATH | shell `export PATH=` | feature `containerEnv` PATH |
| Harness auth dir | `ConfigHomeEnv`→pool home | `ConfigHomeEnv`→pool home |
| Ensured by | `ensureHarnessPool()` | `ensureHarnessPool()` (lifted) |

One registry field (`ConfigHomeEnv`/`Sub`) drives auth location for both, so a
harness added to the registry works in both runtimes with no per-runtime code.

## 5. Phased plan

1. **Registry** — add `ConfigHomeEnv`/`ConfigHomeSub` to `Descriptor`; populate
   for the CLI harnesses (claude → `CLAUDE_CONFIG_DIR`/`.claude`). Switch the
   Guix path to set that env instead of a global `HOME` hijack. No new user
   behaviour, just where the env points.
2. **Feature** — add `pool`/`poolPath` options, `mounts`, and `containerEnv`
   (PATH + the chosen harness's config-home) to `wraptool-connect`. Lift
   `ensureHarnessPool()` onto the devcontainer path in `cmd/up.go`.
3. **Docs + tests** — update `docs/devcontainers.qmd` and the
   `examples/devcontainer` template; extend the feature selftest to assert the
   mount/env are rendered; a Go test that the registry config-home is set for
   both runtimes.

## 6. Risks / open questions

- **uid/gid mismatch** — the container user may differ from the host uid, so
  harness writes to the mounted pool (install, auth refresh) can land with the
  wrong owner or be denied. Devcontainer `updateRemoteUserUID` mitigates for the
  default user; note it, and prefer installing harnesses from the host
  (`wraptool harness install`) so the pool is populated host-side.
- **`$XDG_DATA_HOME` non-default** — not resolvable in the static mount string;
  the `poolPath` option is the workaround (§3.4).
- **Feature ordering** — `containerEnv`/`mounts` compose across features, but if
  another feature also sets `PATH` the prepend order is the feature-install
  order; acceptable (pool bin just needs to be present, not first).
- **Host/IDE harnesses** (`Bin` empty — cursor, roo) — not container CLIs, so the
  pool does not apply; unaffected.
- **Same base assumption as Guix** — the mount lands whatever the host pool
  contains; a harness built against a wildly different libc than the image could
  fail to run. In practice the FHS devcontainer base + a statically-ish linked
  harness (or the host-side install) covers it; call it out.
