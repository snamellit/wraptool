# Design: shared harness pool for the devcontainer runtime

Status: **superseded by [`unified-harness-pool.md`](unified-harness-pool.md)**
— the mixed-OS/mount analysis (§3, §5) is carried forward there; the
`ConfigHomeEnv` approach (§4.2) was not adopted (the shipped feature pins
`HOME=/home/wraptool-harness` instead). Kept forn the analysis record.
Scope: make the shared developer **harness pool** — already mounted into the
Guix-native container runtime — available to the **devcontainer** runtime too.
The pool plumbing is injected by `wraptool up` at launch, **not** baked into the
committed devcontainer config. See `guix-container-runtime.md` and
`harness-descriptor.md`.

## 1. Problem

The harness pool (`paths.DataDir()/harness-pool`, `bin/` on PATH + `home/` for
harness state) lets a developer install an AI harness **once** and reuse it
across every project, instead of baking it into each image. Today only the Guix
runtime gets it — `cmd/guixcontainer.go` bind-shares the pool and sets
`PATH`/`HOME` in the container shell it controls.

The **devcontainer** path (`cmd/up.go`) does not. It runs `devcontainer up`
(passing `WRAPTOOL_TOKEN` via `--remote-env`) and relies on the
`wraptool-connect` feature to wire the MCP config. The harness binary itself must
be installed into the image per-project — exactly the duplication the pool
removes for Guix. Two runtimes, two harness stories.

## 2. Goal

- The devcontainer runtime gets the **same** pool: a harness installed once
  (`wraptool harness install <name>`) is on `PATH` in every devcontainer, with
  its auth/state living in the shared pool.
- Symmetric with Guix: one pool, one harness-config location, driven by one
  registry field, so the two runtimes cannot drift.
- **Safe for mixed-OS teams** — a repo shared by a macOS dev and a Linux dev must
  not have one person's host paths leak into committed config (see §5).

## 3. Why this cannot live in feature metadata

The obvious approach — declare the pool `mounts`/`containerEnv` in
`wraptool-connect`'s `devcontainer-feature.json`, gated by feature options — does
not work. Two hard limits of the Dev Container spec:

**(a) Feature `mounts`/`containerEnv` are static.** The Dev Container CLI reads
them verbatim; **feature options cannot parameterize them**. A `pool` boolean
cannot make a mount conditional (it mounts regardless), and a `poolPath` string
cannot change the mount source (options never reach the `mounts`/`containerEnv`
blocks). Conditional or per-harness values must be supplied some other way.

**(b) Variable substitution has no OS awareness.** The only substitutions
available in `mounts`/`devcontainer.json` are blind string interpolation:

| Substitution | Yields |
|---|---|
| `${localEnv:NAME}` | one host env var, verbatim |
| `${containerEnv:NAME}` | one container env var |
| `${localWorkspaceFolder}` / `…Basename` | the project dir |
| `${containerWorkspaceFolder}`, `${devcontainerId}` | container-side |

There is **no** known-folder/XDG resolver, no conditional, no default-if-empty.
But the host pool source is genuinely OS-divergent — `paths.DataDir()` resolves
via `adrg/xdg` to:

- Linux:   `~/.local/share/wraptool/harness-pool`
- macOS:   `~/Library/Application Support/wraptool/harness-pool`
- Windows: `%LOCALAPPDATA%/wraptool/harness-pool`

A static string such as `${localEnv:HOME}/.local/share/...` is correct on Linux
and **wrong on macOS**; `${localEnv:XDG_DATA_HOME}/...` is **empty on macOS** and
usually unset on Linux too. No static mount string can be right on every host.

**Conclusion:** the host-source path can only be produced by code that runs real
per-OS logic — which `wraptool up` already does (`paths.DataDir()`). So the pool
is wired by `wraptool up` at launch, and the committed config stays pool-free and
OS-invariant.

## 4. Design

### 4.1 `wraptool up` injects the pool at launch

`cmd/up.go` already builds the `devcontainer up` command line and sets
`--remote-env WRAPTOOL_TOKEN=…`. Extend it: when the pool is enabled, resolve the
host path in Go and add runtime flags — the CLI supports repeatable `--mount` and
`--remote-env`:

```
devcontainer up \
  --mount type=bind,source=<paths.DataDir()/harness-pool>,target=/wraptool/harness-pool \
  --remote-env PATH=/wraptool/harness-pool/bin:${PATH} \
  --remote-env <ConfigHomeEnv>=/wraptool/harness-pool/home/<ConfigHomeSub>
```

- **Container target is fixed** `/wraptool/harness-pool` — a Linux path inside the
  always-Linux container, identical on every host OS. Everything container-side
  (PATH prepend, config-home value) is therefore invariant.
- **Host source is resolved per-OS** by `paths.DataDir()` and passed only as a
  runtime flag — it never touches a committed file.
- **Per-harness config-home env** (`CLAUDE_CONFIG_DIR=…` for claude) is chosen by
  the `--harness` selection at `up` time, which is exactly why it must be a
  runtime `--remote-env` and not a static feature `containerEnv`.
- Toggle: `wraptool up --pool=false` skips all three flags. Optional
  `--pool-path <dir>` overrides the resolved source (a CLI flag on `up`, not a
  feature option).

The `wraptool-connect` feature is **unchanged** — it still owns MCP wiring via
`connect.sh`. The pool is orthogonal and fully `up`-driven, so the Go/`connect.sh`
config-writer parity is unaffected. "No second feature" still holds; the pool is
simply not a feature concern.

### 4.2 Harness auth/config home — do **not** hijack `HOME`

Guix sets `HOME=poolHome` so the harness writes its auth under the pool. A
devcontainer already has a real `HOME` (a user with dotfiles, tooling); globally
overriding it to a bind-mounted dir is intrusive and can break the container.

Instead point **only the harness's own config dir** at the pool, per harness. Add
**two** fields to the registry `Descriptor`:

```go
type Descriptor struct {
    // … Name, Auth, Bin, Install, Render …
    // ConfigHomeEnv is the env var a harness reads for its config/auth dir;
    // ConfigHomeSub is that dir's path under the pool home. Empty ConfigHomeEnv =
    // the harness has no relocatable config dir (fall back to HOME= for Guix only).
    ConfigHomeEnv string // e.g. "CLAUDE_CONFIG_DIR"
    ConfigHomeSub string // e.g. ".claude"
}
```

Both runtimes then set, for the chosen harness,
`<ConfigHomeEnv> = /wraptool/harness-pool/home/<ConfigHomeSub>` — devcontainer via
`up`'s `--remote-env`, Guix in its shell command. Auth persists in the pool,
`HOME` stays the container's own. This unifies the runtimes on one mechanism and
one auth location, and lets Guix drop its blunt global `HOME=poolHome`.

Harnesses without a relocatable config dir keep the Guix-only `HOME=poolHome`
fallback and are simply not pool-shared in a devcontainer (documented limitation).

### 4.3 Ensure the pool exists before `devcontainer up`

A bind mount whose **source is missing** fails (or Docker creates a root-owned
empty dir). `ensureHarnessPool()` currently runs only on the Guix path. Lift it
so `cmd/up.go` creates the pool (`bin/`, `home/`) on the devcontainer path too,
before invoking `devcontainer up`. It runs on the host, so it resolves the
correct per-OS path for free — the same `paths.DataDir()` used for the mount.

## 5. Mixed-OS team (macOS + Linux on one repo)

The design's sharpest constraint: `.devcontainer/devcontainer.json` is checked
into the repo — one file for both developers. If `wraptool up` **wrote** the
resolved host path into that file, the macOS dev would commit
`/Users/alice/Library/Application Support/...` and the Linux dev
`/home/bob/.local/share/...`; each `up` would churn the file, they would conflict
in git, and each other's absolute path **does not exist on the other's box** →
broken container for whoever did not run `up` last.

Injecting via runtime flags (§4.1) avoids this entirely:

| Layer | Content | Committed? | Per-OS? |
|---|---|---|---|
| Feature + `devcontainer.json` | container target, MCP wiring | yes | no — invariant |
| `wraptool up` CLI flags | host source, PATH, config-home env | **no** | yes — each dev's own |

Consequences for the pair:

- **Committed config is OS-invariant** — only the Linux-inside-the-container
  target appears anywhere shared. No churn, no conflicts.
- **Each dev's `up` resolves their own host path locally** (`adrg/xdg` → macOS vs
  Linux dir) and injects it. Neither home path lands in the repo.
- **Pools stay per-developer** — the pool is the developer's toolbox, not the
  project. macOS dev's pool under `~/Library/Application Support`, Linux dev's
  under `~/.local/share`; each installs their harness + auth once, independently.
- **Inside the container both see the same thing** — `/wraptool/harness-pool/bin`
  on PATH, same config-home env. Identical experience regardless of host OS.

Degradation: a teammate who runs bare `devcontainer up` (not `wraptool up`) gets
the committed feature parts but **not** the pool mount. Acceptable — the pool is a
`wraptool up` convenience; document it.

## 6. Consistency with the Guix runtime

| | Guix runtime | devcontainer (this proposal) |
|---|---|---|
| Pool mount | `--share=pool=pool` | `up` injects `--mount` (host path via `paths.DataDir()`) |
| `bin/` on PATH | shell `export PATH=` | `up` injects `--remote-env PATH=` |
| Harness auth dir | `ConfigHomeEnv`→pool home | `ConfigHomeEnv`→pool home (via `--remote-env`) |
| Ensured by | `ensureHarnessPool()` | `ensureHarnessPool()` (lifted) |
| Host path resolution | `paths.DataDir()` | `paths.DataDir()` (same call) |

Both runtimes resolve the host path with the **same** Go call and drive auth from
the **same** registry field, so a harness added to the registry works in both with
no per-runtime, per-OS code.

## 7. Phased plan

1. **Registry** — add `ConfigHomeEnv`/`ConfigHomeSub` to `Descriptor`; populate
   for the CLI harnesses (claude → `CLAUDE_CONFIG_DIR`/`.claude`). Switch the Guix
   path to set that env instead of a global `HOME` hijack. No new user behaviour,
   just where the env points.
2. **`wraptool up`** — resolve `paths.DataDir()/harness-pool`, lift
   `ensureHarnessPool()` onto the devcontainer path, and append the `--mount` +
   `--remote-env` flags (PATH + the chosen harness's config-home) to the existing
   `devcontainer up` invocation. Add `--pool` / `--pool-path` flags on `up`.
3. **Docs + tests** — update `docs/devcontainers.qmd` and the
   `examples/devcontainer` template; a Go test that `up` builds the expected
   `--mount`/`--remote-env` args for a chosen harness on a faked `paths.DataDir()`;
   a test that the registry config-home is set identically for both runtimes.

## 8. Risks / open questions

- **uid/gid mismatch** — the container user may differ from the host uid, so
  harness writes to the mounted pool (install, auth refresh) can land with the
  wrong owner or be denied. Devcontainer `updateRemoteUserUID` mitigates for the
  default user; note it, and prefer installing harnesses from the host
  (`wraptool harness install`) so the pool is populated host-side.
- **Bare `devcontainer up`** — does not get the pool (no wraptool injection);
  documented degradation (§5).
- **PATH ordering across features** — `up`'s `--remote-env PATH=` prepends the
  pool `bin`; other features setting PATH still compose, and pool bin only needs
  to be present, not first.
- **Host/IDE harnesses** (`Bin` empty — cursor, roo) — not container CLIs, so the
  pool does not apply; unaffected.
- **libc skew** — the mount lands whatever the host pool contains; a harness built
  against a wildly different libc than the image could fail to run. The FHS
  devcontainer base + a statically-ish linked harness (or host-side install)
  covers it in practice; call it out.
