# Design: Docker Sandboxes runtime for `wraptool up`

Status: proposed (implementation plan; Phase 0 is a hands-on spike that gates
everything after it)

Supersedes the investigation note `design/harness/microvm-sandbox-runtime.md`,
whose blocking questions are carried forward in §3 with their current answers.

Scope: give `wraptool up` a third runtime, `--runtime=sbx`, that runs the
harness inside a Docker Sandboxes microVM (`sbx` CLI) instead of a Guix
container or a Dev Container. The server, the policy engine, the harness pool
and the two existing runtimes do not change behaviour.

## 1. Why

wraptool mediates privileged operations; it does not harden the box the
harness runs in. Both existing runtimes share the host kernel, and each has a
known network weakness:

| | Guix runtime | Dev Container runtime | Docker Sandboxes runtime (this note) |
|---|---|---|---|
| Kernel boundary | shared (user/mount namespaces) | shared (container) | separate kernel per sandbox (microVM) |
| Host loopback from the box | **all of it** (`-N` shares the host netns, `design/harness/guix-container-runtime.md` §7) | none, but the server is rebound to `0.0.0.0` to be reachable over the bridge (`adjustServerBindForRuntime`) | one pinhole: `localhost:<mcp port>`, allowed by a sandbox-scoped policy rule; the server stays loopback-bound |
| Review UI listener reachable from the box | yes (token-protected) | no | no |
| Egress control | none | none | host-side proxy, deny-by-default presets, per-sandbox rules |
| Host Docker daemon | n/a | reachable only if the config mounts the socket | never; the sandbox has a private Docker Engine |

The prize is therefore larger than "microVM instead of namespaces": this is
the first runtime where the box cannot reach arbitrary host loopback services
*and* the server does not have to listen on a wildcard address.

The cost is that Docker Sandboxes ships a credential model that is the
opposite of wraptool's (a proxy that injects credentials at the boundary, SSH
agent forwarding on by default, an MCP gateway that brokers host-registered
servers). Left at its defaults it would **weaken** a wraptool setup by giving
the agent credential paths that bypass policy. Most of this design is about
making those paths provably absent (§6).

## 2. Evidence status — read this first

No `sbx` binary was run for this note. The development environment is an Arch
host inside a Dev Container with no Docker CLI and no KVM access; Docker
Sandboxes supports macOS 14+ (Apple silicon), Windows 11, and Ubuntu 24.04+
with KVM only.

Everything below marked **(docs)** is taken from Docker's published
documentation as read on 2026-10-06 (sources in §13). The CLI reference pages
(`/reference/cli/sbx/*`) did not render for the fetcher, so flag spellings
come from the prose guides, not the reference. Everything marked
**(unverified)** is an inference. The unified-harness-pool §12.2 lesson
applies in full: "supports mounts" and "supports read-only mounts" were
different claims for the devcontainer CLI, and only the real flag surface
settled it. Phase 0 (§9) re-establishes every load-bearing fact on a real
host before any code is written.

## 3. The old note's blocking questions, answered

| Question (`microvm-sandbox-runtime.md`) | Answer | Confidence |
|---|---|---|
| Extra mounts, including read-only | `sbx run AGENT PRIMARY EXTRA:ro …` — additional workspaces as extra positional arguments, `:ro` suffix for read-only, each "at its original absolute path inside the sandbox" | docs; EROFS behaviour unverified (S1) |
| Host reachability | All outbound TCP goes through a host-side proxy. `host.docker.internal` is rewritten to the host's `localhost`; the destination must be allowed with `sbx policy allow network localhost:<port>`. `127.0.0.1` inside the VM is the VM. | docs; SSE/long-lived streams and the `Host` header unverified (S2, S3) |
| Credential proxy can be absent | "No credentials are available to the sandbox unless you provide them using `sbx secret` or environment variables." **But**: secrets are global by default, and the SSH agent is forwarded whenever `SSH_AUTH_SOCK` is set in the connecting client. Absent by default only on a host that never used `sbx` for anything else. | docs; enforcement design in §6 |
| Custom agent contract | A built-in `shell` agent: "Bash login shell inside a sandbox with no pre-installed agent binary". Custom templates via `-t/--template <OCI ref>`. Kits (`docker/sandbox-kit-spec` v3) can define a workload with its own entrypoint, but also declare credentials and network authority. | docs |
| Linux support | Ubuntu 24.04+ with KVM (amd64, arm64). Other distributions are untested and unsupported. No Docker Desktop required. | docs |
| Stability / licensing / pinning | Local sandboxes are free including commercial use; a Docker account sign-in (`sbx login`) is mandatory; CLI telemetry is on unless `SBX_NO_TELEMETRY=1`. No documented way to pin an `sbx` version; `-e/--env` needs ≥ 0.39.0. | docs; update behaviour unverified (S11) |

Verdict against the old note's adoption criteria: all four are met on paper,
so the old default of "document, don't implement" no longer holds. Shape 1
(a third runtime) is the plan. Shape 2 (devcontainer inside a sandbox) is
dropped: it nests two runtimes to gain nothing shape 1 lacks.

## 4. Decisions

- **D1. Flag value `--runtime=sbx`.** The existing values name the CLI that is
  driven (`guix`, `devcontainer`); `sbx` follows suit and leaves "sandbox"
  free as a generic word. Prose name: *the Docker Sandboxes runtime*. Go
  identifier `runtimeSbx`, file `cmd/sbxcontainer.go`.
- **D2. Never auto-selected.** `auto` keeps its current two-way choice. The
  runtime needs a Docker account, sends telemetry by default, and has host
  state (`sbx secret`, `sbx mcp`) that wraptool must inspect; opting in is
  explicit. Revisit after one release of field use.
- **D3. Use the built-in `shell` agent, never a vendor agent kind.** A vendor
  kind (`claude`, `codex`, …) brings a floating agent binary baked into
  Docker's template, default credential bindings, and the shared skills
  store. The harness comes from the harness pool, version-locked and
  quarantined, exactly as on the other runtimes.
- **D4. Authority lives in wraptool's argv, not in a kit.** Mounts,
  environment and network rules are passed on the `sbx` command line by
  `up`. wraptool never passes `--kit`, and never derives mounts, network
  rules or credentials from a file inside the workspace: the workspace is
  agent-writable, and a kit is an authority grant.
- **D5. The toolchain comes from a template, chosen by the operator.**
  `--sandbox-template <OCI ref>` (env `WRAPTOOL_SANDBOX_TEMPLATE`); default
  is the `shell` agent's own base image. A template decides only what is
  inside the VM, so it is not an authority grant, but v1 still takes it from
  the host side only. Building a template from `manifest.scm` is a non-goal
  (§11).
- **D6. Pool `runtime/` only — no legacy `bin/`+`lib/` fallback.** A new
  runtime has no legacy installs to honour. A harness without an active
  generation for the sandbox platform fails with the fix command
  (`wraptool harness install <name>`). Harnesses that are not pool-lockable
  are unsupported on this runtime in v1.
- **D7. The server stays loopback-bound.** `adjustServerBindForRuntime` must
  not widen the bind for `runtimeSbx`; reachability is the proxy pinhole.
- **D8. Refuse rather than warn when a bypass path exists.** If the preflight
  in §6 finds a credential path wraptool cannot close, `up` fails with the
  offending item and the remedy. An explicit override flag exists
  (`--allow-sbx-secrets`), is named in the error, and is recorded in `up`'s
  output.
- **D9. `sbx` runs with a minimal environment.** Same principle as the
  executor's child environment: an allowlist (PATH, HOME, TERM, COLORTERM,
  LANG/LC_*, XDG_*, `SBX_*`, `DOCKER_*` — exact set fixed by S5), never
  `os.Environ()`. This is what removes `SSH_AUTH_SOCK`.

## 5. Bring-up design

### 5.1 Sequence

`runUp` is unchanged above the runtime switch except for one new case.

1. **Preflight** (§6): `sbx` on PATH, version ≥ minimum, signed in, no
   bypass paths.
2. **Pool plan.** `resolveSbxPoolPlan(poolPlanInput)` — the shared input
   struct the two existing plans already use. Active pointer for
   `pool.HostPlatformKey()` resolves to a live generation, or error (D6).
   `HostPlatformKey` already maps `GOARCH` to a `linux-*-glibc` key, which is
   what a macOS/Windows host needs too: the VM is Linux on the host's
   architecture. The template must be glibc-based (S9).
3. **Harness config.** `harness.Write` into the workspace root, as the Guix
   path does, with `URL: http://host.docker.internal:<port>/sse` and
   `CWD: root`. The port comes from `cfg.MCP.Listen`.
4. **Create** (first run only):

   ```
   sbx create --name <name> [-t <template>] \
       -e WRAPTOOL_CONTAINER=1 -e WRAPTOOL_CWD=<root> \
       shell <root> <pool runtime dir>:ro <pool home dir>
   ```

   Workspaces appear at their host absolute paths, which is the property the
   Guix runtime relies on: `?cwd=` matches without translation, and the
   dispatcher scripts in `runtime/bin` carry absolute pool paths that stay
   valid.
5. **Pinhole.** `sbx policy allow network --sandbox <name> localhost:<port>`
   — sandbox-scoped, one port. Nothing else on host loopback is opened.
6. **Enter** (when `wantShell`): `sbx exec -it <name> bash -lc <entry>`, with
   `<entry>` built by the existing `buildContainerEntry(root, poolHome,
   pathDirs)` — append-never-prepend PATH, HOME pinned to the pool `home/`,
   `cd` into the workspace. Reusing it keeps the shadowing guarantee
   identical across runtimes. `sbx exec` is chosen over `sbx run --name … --
   -c …` because reconnect-with-arguments semantics are undocumented (S6).
7. Without a shell: stop after step 5 and print the re-entry command.

All argv construction is in pure functions (`buildSbxCreateArgs`,
`buildSbxPolicyArgs`, `buildSbxExecArgs`) pinned by tests, like
`buildGuixShellArgs`. No shell is involved on the host side.

### 5.2 Sandbox identity and lifecycle

- **Name:** `wt-<sanitized basename>-<first 8 hex of sha256(root)>`,
  lowercase `[a-z0-9-]`, length-capped. Deterministic per worktree, so
  `up` is idempotent and `down` needs no state to find it.
- **Persistence:** a sandbox persists until removed (docs). A second `up`
  finds the sandbox via `sbx ls` and skips to step 5.
- **Drift:** the mount set and environment are fixed at creation
  (unverified, S7). `up` stores the creation inputs (template, mounts, env
  names, wraptool version) in `paths.DataDir()/sandboxes/<name>.json` and
  compares on re-entry. On mismatch it refuses and names
  `wraptool down --rm`; it never destroys a sandbox on its own, because the
  VM holds developer state.
- **`wraptool down`:** `sbx stop <name>`; `--rm` runs `sbx rm --force
  <name>`, removes the state file, and removes the sandbox-scoped policy
  rule if `sbx rm` leaves it behind (S8). `runDown` today hard-requires
  `docker`; it becomes a dispatch over what exists for this root (a sandbox
  by name when `sbx` is on PATH, dev containers by label when `docker` is).
- **Nested-`up` guard:** `WRAPTOOL_CONTAINER=1` is set at creation and by
  the entry, so `inContainer` refuses as on the other runtimes.

### 5.3 Reaching the server through the proxy

Two things can break here and neither can be settled from documentation.

- **`Host` header (S3).** `browserGuard` on a loopback-bound listener accepts
  only `localhost`, `127.0.0.1`, `::1`, or the listen host. If Docker's
  proxy forwards the request with `Host: host.docker.internal:<port>`
  intact, every MCP call gets `403 host not allowed`. Options, in order of
  preference:
  1. The proxy already rewrites `Host` — nothing to do.
  2. Add an explicit allowlist, `mcp.extra_hosts`, default empty. `up
     --runtime=sbx` adds `host.docker.internal` through the existing
     `serveOverrides`/`serveState` mechanism (a running server with a
     different desired state is already restarted for bind changes); an
     operator running the server under a service manager sets the key.
     The DNS-rebinding defence is not weakened in substance: the `Origin`
     check is unchanged, so a browser page still cannot issue the
     cross-origin request, and `host.docker.internal` is not a name an
     attacker's DNS controls.
  3. Reject: a blanket "any Host on loopback" relaxation.
- **Streaming (S2).** The legacy SSE transport holds one long-lived GET. If
  the proxy buffers or times it out, harnesses that support it are pointed
  at Streamable HTTP (`/mcp`, `streamableURL` in `internal/harness`); if
  both fail, the runtime is not viable and Phase 0 ends in "no-go".

The bearer token crosses Docker's host-side proxy in cleartext HTTP. That
process runs as the developer on the host, which is inside the trust
boundary (`design/project-isolation.md` §2), so this is recorded, not
mitigated.

## 6. Keeping Docker Sandboxes' credential paths closed

This is the section that decides whether the runtime is a hardening or a
regression. Each row is a path by which the agent could use a credential
without going through policy.

| Path | Default behaviour (docs) | wraptool's control | Verify |
|---|---|---|---|
| SSH agent forwarding | on whenever `SSH_AUTH_SOCK` is set "in the connecting client" | D9: `sbx` is spawned without `SSH_AUTH_SOCK`. If forwarding is decided by a long-lived `sbx` daemon instead of the client process, this is insufficient and needs a real off switch. | S4: `ssh-add -l` inside the sandbox must fail |
| Global `sbx secret` entries (`github`, custom hosts, registry credentials scoped to all sandboxes) | injected by the proxy into matching outbound requests from **every** sandbox | Preflight `sbx secret ls`. Any global secret that is not a model-provider service fails `up` (D8), naming each entry and the two remedies: `sbx secret rm <name>`, or re-scope it with `--sandbox` to the sandboxes that should have it. | S5: output format; whether a global secret can be excluded for one sandbox |
| Model-provider secrets (`anthropic`, `openai`, `google`, `cursor`) | same mechanism | Allowed. The agent can call its model API by definition; a proxy-held key is *less* exposure than the key in pool `home/`. | — |
| MCP gateway | dynamic mode exposes `mcp-find`/`mcp-add` so the agent can attach any host-registered server; local stdio servers "run on the host, not inside the sandbox" | Must be off for wraptool sandboxes. The docs list gateway support for vendor agent kinds, not `shell`, so D3 may already exclude it. If not: create with an empty static set, or fail `up` when `sbx mcp ls` is non-empty. | S10 — a blocker if it cannot be disabled |
| Environment variables | `-e NAME` copies from the host environment | Only `WRAPTOOL_CONTAINER` and `WRAPTOOL_CWD` are passed, always as `NAME=value`; never `--env-file`. The bearer token is not passed through the environment at all — it is only in the harness config file, as today. | unit test on argv |
| Kits | third-party kits declare credentials, mounts and network allow rules | D4: never used. | unit test on argv |
| Shared agent skills store | mounted read-only "for supported agents" | D3 should exclude it; it is read-only content, not a credential, so a miss is a hygiene issue rather than a bypass. | S10 |
| Host loopback beyond the pinhole | governed by the developer's global policy preset; under `allow-all` the proxy may forward `host.docker.internal:<any port>` | After step 5, probe with `sbx policy check network --sandbox <name> localhost:<review UI port>` (and port 22). If allowed, warn loudly with the remedy (`sbx policy init balanced`); a sandbox-scoped deny cannot fix it because deny beats the pinhole's allow. | S12 |

What this runtime does **not** change: the workspace is a direct read-write
mount, so `.git/hooks` and other execute-on-host files remain
agent-writable exactly as on the other runtimes
(`design/git-execution-hardening.md`); and pool `home/` stays shared and
writable across projects (`design/project-isolation.md` §4).

## 7. Preflight and `wraptool doctor`

`up --runtime=sbx` runs these before touching the server, in this order, and
each failure names its fix:

1. `sbx` on PATH (stubbable package var `hasSbx`, the `hasGuix` pattern).
2. `sbx version` ≥ the minimum this design is verified against (at least
   0.39.0 for `-e`; the real floor is whatever Phase 0 ran). The version is
   printed in `up`'s output. An unknown *newer* version is a notice, not a
   failure.
3. Signed in.
4. §6's secret and MCP-gateway checks.

`wraptool doctor` (`internal/doctor`) gains matching always-report checks,
mirroring `env.devcontainer-cli` and `exposure_listen.go`:

- `env.sbx-cli` — presence, version, sign-in state.
- `exposure.sbx-secrets` — global non-model-provider secrets present.
- `exposure.sbx-policy-preset` — global preset is `allow-all`.
- `envBackends` and the "no backend" remedy text learn `sbx`.

The doctor checks run fixed argv (`sbx version`, `sbx secret ls`, `sbx
policy ls`) through the existing `Env` seam so tests need no `sbx`.

## 8. Code changes

| File | Change |
|---|---|
| `cmd/guixcontainer.go` | `runtimeSbx` enum member, `String()`, `selectRuntime` accepts `sbx` (never from `auto`), error text lists the third value |
| `cmd/sbxcontainer.go` (new) | `bringUpSbxContainer`, pure argv builders, `sbxSandboxName`, `resolveSbxPoolPlan`, state file read/write, `hasSbx` |
| `cmd/sbxpreflight.go` (new) | version parse and gate, sign-in check, secret classification, gateway check, minimal-env builder (D9), loopback probe |
| `cmd/up.go` | `--runtime` help text, `--sandbox-template`, `--allow-sbx-secrets`, new `switch rt` case (the `exhaustive` linter forces it), `adjustServerBindForRuntime` leaves `runtimeSbx` on loopback |
| `cmd/down.go` | dispatch over runtimes instead of requiring `docker` |
| `cmd/serve.go`, `internal/config` | only if S3 fails: `mcp.extra_hosts` plus the override plumbing |
| `internal/doctor/env.go`, new `exposure_sbx.go` | §7 |
| `testdata/sbx/` | captured `sbx version`, `sbx secret ls`, `sbx ls`, `sbx policy ls` output from Phase 0 |
| `test-sandbox.sh` (new) | field test, the counterpart of `test-container.sh` |
| `docs/`, `CLAUDE.md`, `CHANGELOG.md` | §10 |

Nothing in `internal/pool`, `internal/whitelist`, `internal/executor` or
`internal/mcptools` changes.

## 9. Phases

One MR per phase. Phases 1 and 2 follow the architect loop in
`.claude/agents/README.md`; briefs are written after Phase 0 so they can
quote real output.

### Phase 0 — spike (hands-on, about half a day; macOS on Apple silicon or Ubuntu 24.04 with KVM)

Record raw output for every step into this note as a §14 "Spike evidence"
section, and save the listed command outputs as `testdata/sbx/` fixtures.

| # | Question | Pass |
|---|---|---|
| S1 | Extra workspace `:ro` | Mounted at the host absolute path; a write returns `EROFS`; a path containing a space works (macOS `Application Support`) |
| S2 | SSE and Streamable HTTP through the proxy | `curl -N http://host.docker.internal:<port>/sse` with the bearer token streams the `endpoint` event and stays open ≥ 10 minutes idle; a harness completes `initialize` and one tool call |
| S3 | `Host` header the server sees | Read it from the server's `rejected … Host` log line or a plain listener; decides §5.3 |
| S4 | SSH agent with `SSH_AUTH_SOCK` unset in the `sbx` client | `ssh-add -l` inside fails; repeat with the variable set to confirm the test can fail |
| S5 | `sbx secret ls` | Machine-parseable form exists (or the table is stable enough to fixture); a global `github` secret is visibly injected from a `shell` sandbox, proving the §6 refusal is necessary; can it be excluded per sandbox |
| S6 | Entry | `sbx exec -it <name> bash -lc '<entry>'` works on a created-but-not-running sandbox; TTY, resize and exit code behave |
| S7 | Mutability | Can workspaces or env be changed after creation |
| S8 | `sbx rm` | Removes sandbox-scoped policy rules |
| S9 | Pool generation in the VM | A realized `claude` generation for `HostPlatformKey()` runs from the `:ro` mount with HOME on the pool `home/` mount; files it writes there have sane ownership on the host |
| S10 | `shell` agent extras | No MCP gateway endpoint, no skills-store mount, no `proxy-managed` sentinel variables that confuse the harness |
| S11 | Versioning | Does `sbx` self-update; is the Homebrew/apt version pinnable |
| S12 | Loopback under each preset | `sbx policy check network localhost:<other port>` under `balanced`, `deny-all`, `allow-all` |

Also record: what the default `shell` template contains (git? a compiler?),
whether a plain non-Docker OCI image works as `-t`, and whether local git
works in a linked worktree (its `.git` file points outside the mount — the
same limitation the other runtimes have).

**Gate.** Go if S1, S2, S4, S6, S9, S10 pass. S3, S5, S7, S8, S11, S12 shape
the implementation but do not block it. Any failure among the six: stop,
write the findings here, set Status to `findings`, and fall back to
documenting the manual combination.

### Phase 1 — the runtime, closed by default (MR 1)

Two packages with disjoint files, then a third on top.

```
Package:      sbx-runtime-core
Base branch:  feat/sbx-runtime
Spec:         this note §4, §5, §6, §7 (preflight half), §14
Deliverables: cmd/sbxcontainer.go, cmd/sbxpreflight.go, runtimeSbx in
              cmd/guixcontainer.go, the up.go flag/switch/bind changes
Tests:        selectRuntime table incl. "auto never yields sbx"; golden argv
              for create/policy/exec; name derivation (collision, sanitizing,
              length); pool plan quadrants (active / no pointer / dangling
              pointer / non-lockable harness); secret classification against
              testdata/sbx fixtures; minimal env never contains SSH_AUTH_SOCK
              even when the parent has it; state-file drift refusal;
              adjustServerBindForRuntime keeps loopback for runtimeSbx
Non-goals:    down, doctor, docs, egress management, MCP gateway wiring
Watch out:    exhaustive linter on containerRuntime switches; no shell on the
              host side; -e always NAME=value; never --kit, never --env-file;
              refuse, do not warn, on a non-model-provider global secret
```

```
Package:      sbx-down
Base branch:  feat/sbx-runtime
Spec:         this note §5.2
Deliverables: cmd/down.go runtime dispatch; sandbox stop/rm; state-file and
              policy-rule cleanup per S8
Tests:        neither/either/both backends present; --rm removes state file;
              devcontainer behaviour byte-identical to today
Non-goals:    anything in up.go
Watch out:    down must still work on a host with docker and no sbx, and the
              reverse
```

```
Package:      sbx-host-allowlist        (only if S3 shows Host is preserved)
Base branch:  impl/sbx-runtime-core
Spec:         this note §5.3 option 2
Deliverables: mcp.extra_hosts (strict-decode + validate), hostAllowed takes
              the set, serveOverrides/serveState carry it, up adds
              host.docker.internal for runtimeSbx
Tests:        hostAllowed truth table incl. wildcard bind unchanged; Origin
              check unaffected by the new key; runningMatches restarts on a
              changed set; an integration test driving the real handler
              chain with Host: host.docker.internal
Non-goals:    any relaxation of originAllowed
Watch out:    empty default must be byte-identical to today's behaviour
```

The seam between `sbx-runtime-core` and `sbx-host-allowlist` gets a real
handler-chain test, per the README's rule on pinned seams.

### Phase 2 — doctor, field test, documentation (MR 2)

- `internal/doctor` checks from §7.
- `test-sandbox.sh`: asserts from inside a sandbox that host `~/.ssh` is not
  visible, `ssh-add -l` fails, `localhost:<review UI port>` is refused, the
  pool `runtime/` mount is read-only, and one policed MCP tool call
  succeeds. Manual, on macOS and Ubuntu; CI's guix runner cannot host a
  microVM.
- Documentation, §10.

### Phase 3 — optional hardening, each item its own decision

Not planned; listed so nobody rediscovers them.

- **wraptool-managed egress.** `up` installs a sandbox-scoped allowlist
  (registries the toolchain needs, the model API) and reports denials. This
  is the first exfiltration control wraptool would have, and it needs its
  own note: the allowlist is policy, so it belongs in the reviewed config,
  not in flags.
- **Bearer token outside the VM.** `sbx mcp add wraptool-<name> --url
  http://localhost:<port>/mcp --header 'Authorization: Bearer ${…}'` keeps
  the token on the host. Blocked on: gateway support for the `shell` agent,
  whether `tools/list_changed` survives the gateway (hot-reload depends on
  it), and how the gateway's meta-tools interact with
  `wraptool_request_capability`. It also overlaps project grants
  (`design/project-isolation.md` §3), which solve the same exposure
  natively.
- **Harness authentication through the proxy.** An `anthropic` secret
  injected at the boundary is `design/project-isolation.md` §5 option 3 (a
  host-side broker) delivered by the vendor, and would let pool `home/`
  stop carrying a credential. The old note's non-goal ("never adopt the
  credential proxy") was about privileged-tool credentials; whether it
  extends to the model API key is the maintainer's call.
- **Clone mode** (`--clone`): the agent's edits never touch the host tree.
  Docs say it rejects linked worktrees, which is wraptool's normal layout.
- **`auto` selection** (revisit D2).

## 10. Documentation to touch (Phase 2)

- `docs/terminology.qmd`: the **runtime** row names two runtimes; add the
  Docker Sandboxes runtime. That table is binding, so this lands first.
- A runtime page section for `--runtime=sbx`: prerequisites, the refusal
  messages and their remedies, what is and is not isolated.
- `docs/comparison.qmd`: the closing "nothing prevents running wraptool's
  container runtime inside a harder isolation boundary" becomes a link to
  the shipped runtime.
- `CLAUDE.md` architecture entries for the new files; `CHANGELOG.md`.
- `design/release-1-0.md` §3.4 and §3.6 still describe the old note's
  likely outcome as "document, do not implement"; update when this note is
  accepted. A new `--runtime` enum member remains additive, so the 1.x
  placement stands.
- All examples use the `sse` transport. wraptool's own stdio transport does
  not appear.

## 11. Non-goals

- Replacing or changing the Guix or Dev Container runtimes.
- Building a sandbox template from `manifest.scm`. A guix-pack image was
  tried for the provisioning image and abandoned over cross-architecture
  builds; the same wall applies.
- Using microVMs for pool realization; the OCI backend's output is
  receipt-verified wherever it ran.
- Docker Sandboxes cloud compute, organization governance, or any paid
  tier.
- Windows hosts in v1. Nothing here is known to preclude them; "host
  absolute path inside the sandbox" for `C:\…` is simply unexamined.
- Private per-project harness HOME — `design/project-isolation.md` owns
  that for all runtimes.

## 12. Open questions for the maintainer

1. D1: `sbx` versus `sandbox` as the flag value.
2. D8: is refusing on a global `github` secret acceptable, given a
   developer may use `sbx` for unrelated work on the same machine? The
   alternative is a warning, which makes the regression silent.
3. A mandatory Docker account and default-on telemetry for a
   credential-isolation tool's runtime: document only, or should `up` set
   `SBX_NO_TELEMETRY=1` for the processes it spawns?
4. Is the model-provider proxy (Phase 3) in or out of the "never adopt the
   credential proxy" rule?
5. Who runs Phase 0, on which host?

## 13. Sources

Docker documentation, read 2026-10-06:

- `https://docs.docker.com/ai/sandboxes/` and `…/install/`,
  `…/architecture/`, `…/usage/`, `…/faq/`, `…/troubleshooting/`
- `…/security/`, `…/security/isolation`, `…/security/defaults`,
  `…/security/policy/`
- `…/configuration/credentials/`
- `…/agents`, `…/agents/shell/`
- `…/mcp-gateway/`
- `…/customize`, `…/customize/author/`
- `https://github.com/docker/sandbox-kit-spec`
- `https://docs.docker.com/guides/claude-code-sandbox-model-runner/`
  (the `host.docker.internal` → `localhost` rewrite and its policy rule)

## 14. Spike evidence

Empty until Phase 0 runs.

## 15. CLI surface as read from the reference data (not run)

Added after §2 was written. The cobra reference YAML that the rendered pages
are generated from is readable in the documentation repository:
`https://raw.githubusercontent.com/docker/docs/main/data/sbx_cli/<command>.yaml`
(vendored for `sbx` v0.47.0, read 2026-10-06). This settles flag spellings.
It does not settle behaviour: nothing below was executed, and every S-question
in §9 stays open.

| Command | Usage line and flags that matter here |
|---|---|
| `sbx create` | `sbx create [flags] AGENT\|SANDBOX_KIT [PATH...]`. `--name` (no short form; ≥ 2 characters, starts with a letter or digit, only letters, digits, hyphens, periods; `default` is reserved), `-t/--template`, `-e/--env` (repeatable; `KEY=VALUE`, or a bare `KEY` copied from the caller's environment), `--env-file`, `--kit`, `--static-mcp` (repeatable; omitted means *dynamic* gateway mode), `--skills off\|readonly\|readwrite` (default `readonly`), `--deny-network`, `--clone`. First PATH is the primary workspace; an extra PATH takes a `:ro` suffix. The sandbox stops by itself once created. |
| `sbx exec` | `sbx exec [flags] SANDBOX COMMAND [ARG...]`. `-i`, `-t`, `-e`, `-w/--workdir`, `-u/--user`. "If the sandbox is stopped, it is started first." No shell is started unless the command is one. |
| `sbx ls` | `--json` (schema undocumented), `-q/--quiet` ("Only display sandbox names"). |
| `sbx stop` | `sbx stop SANDBOX [SANDBOX...]`, no flags. |
| `sbx rm` | `sbx rm [SANDBOX...]`, `-f/--force`. Also "deletes secrets scoped to each removed sandbox". |
| `sbx version` | `--json` only (schema undocumented). Text output is not documented and has changed shape between releases; issue reports show `sbx version: v0.45.0 <commit>`. |
| `sbx secret ls` | `-g/--global`, `--sandbox`, `--service`, `--json` (schema undocumented), `-q`. Documented table: `SCOPE  TYPE  NAME  SECRET`, with a row `(global)   service   github    gho_GCaw4o****...****43qy`. |
| `sbx secret set` / `rm` | `set` is **global by default**; `--sandbox` scopes it. `rm [SERVICE] --sandbox … -f`. Built-in services: anthropic, copilot, cursor, devin, droid, github, google, groq, mistral, nebius, openai, openrouter, xai. |
| `sbx policy allow network` | `sbx policy allow network [--sandbox SANDBOX] RESOURCES`; RESOURCES is comma-separated, with optional `:port`. |
| `sbx policy check network` | `[--sandbox SANDBOX] TARGET`, `--json`. Documented text output: `Allowed: <target>` or `Denied: <target>` plus a `Reason:` line. Exit status on a denial is undocumented. |
| `sbx policy rm network` | `[--sandbox SANDBOX] --resource <list> [-f]`. The resource is a flag here, not a positional. |
| `sbx policy ls` | `sbx policy ls [SANDBOX]`; the sandbox is a positional here. `--json`, `--type`, `--decision`. |
| `sbx policy init` | `sbx policy init <allow-all\|balanced\|deny-all>`; "must be run before adding custom allow/deny rules or starting a sandbox for the first time". |
| `sbx mcp ls` | `--json`, `-q`. |

Facts that change §3–§7:

- **No command reports sign-in state.** There is no `whoami` and no
  `login --status`; only `sbx diagnose` includes it, in an undocumented
  format. The documented failure text is `You are not authenticated to
  Docker`.
- **Every sandbox starts an MCP gateway.** Only vendor agent integrations
  register it with their agent, and `shell` is not one of them, but there is
  no documented per-sandbox off switch. Whether `--static-mcp` accepts an
  empty set is undocumented.
- **The skills store has an off switch**: `--skills off`.
- **SSH agent forwarding** uses "the value from the client that creates,
  starts, or joins each sandbox", which supports D9. Two host-wide settings
  matter and wraptool controls neither: `ssh.agentForwardingEnabled` (the
  real off switch, needs `sbx daemon restart`) and `ssh.agentSocketPath`
  (a fixed socket path, which would defeat D9).
- **State and secrets on Linux** follow XDG (`~/.local/state/sandboxes`,
  `~/.config/sandboxes`), with secrets in the Secret Service when one is
  available. The minimal environment therefore needs the session bus
  address, or `sbx` cannot read its own login.
- **The `Host` header and streaming through the proxy** (S2, S3) are not
  documented anywhere.

## 16. Rulings for the test implementation

Phase 1 is being built before Phase 0 has run, as a test implementation: the
first run on a real host is the spike. These rulings stand in for the facts
Phase 0 was meant to supply. Each is provisional and names the S-question
that confirms or replaces it.

- **R1. Version floor 0.47.0**, the release the flag surface was read from.
  The version is taken from the first `\d+\.\d+\.\d+` in `sbx version`
  output. Unparseable output fails the preflight; a newer version is a
  notice. (S11)
- **R2. Sign-in is detected from the first real call.** The preflight runs
  `sbx ls --quiet`, which `up` needs anyway to learn whether the sandbox
  exists. Output containing `not authenticated` becomes "run `sbx login`";
  any other failure is reported verbatim.
- **R3. Secrets are read with `sbx secret ls --global`** and parsed from the
  documented table. Allowed: type `service` with a name in D8's
  model-provider set (anthropic, openai, google, cursor — the other model
  services in §15 stay refused until the maintainer rules on question 6).
  Anything else, including a row or a table the parser does not recognise,
  refuses. The parser fails closed: a row whose scope is not exactly
  `(global)` is not skipped but refused, because the command asked for
  global entries only, and empty output is refused too, because the table
  may have gone to another stream. Error text names scope, type and name
  only, never the secret column, and never echoes raw output, including
  when the command itself fails.
  `--allow-sbx-secrets` overrides both the refusal and the parse failure.
  (S5)
- **R4. MCP gateway: refuse when `sbx mcp ls --quiet` prints any name.**
  With nothing registered on the host, the dynamic gateway has nothing to
  offer. The remedy names `sbx mcp rm <name>`. `--static-mcp` is not passed.
  There is no override flag for this refusal. (S10)
- **R5. `--skills off` is always passed** at creation.
- **R6. Minimal environment** (D9): PATH, HOME, USER, LOGNAME, TMPDIR, TERM,
  COLORTERM, LANG, `LC_*`, `XDG_*`, `SBX_*`, `DOCKER_*`,
  DBUS_SESSION_BUS_ADDRESS. Nothing else, for every `sbx` process wraptool
  spawns. The `DOCKER_*` family admits `DOCKER_AUTH_CONFIG` (inline registry
  credentials) to the `sbx` client; whether the client carries it into a
  sandbox is unverified. `SBX_NO_TELEMETRY` is passed through when the developer set it and
  is not set by wraptool (question 3 is open). (S4, S5)
- **R7. The state file records the wraptool version but drift does not
  compare it.** A development build's version changes on every commit, and a
  version change alone does not alter what is mounted. Compared: agent,
  template, mounts with their modes, environment variable names, skills
  mode, pinhole resource. A sandbox with the right name and no state file
  was not created by this code and is refused the same way as drift. The
  pinhole resource is compared because a changed MCP port would otherwise
  leave the old port allowed for the sandbox; it is recorded so `down --rm`
  can remove the rule. (S7, S8)
- **R8. The loopback probe reads the output, not the exit status**: a line
  starting `Allowed:` warns, `Denied:` is silent, anything else prints a
  notice that the probe was inconclusive. It never fails `up`. (S12)
- **R9. The runtime needs the `sse` transport.** A `unix` listener cannot
  cross the VM boundary; `up --runtime=sbx` says so instead of creating a
  sandbox that cannot connect.
- **R10. `sbx-host-allowlist` is not built.** S3 is unknown. If the proxy
  preserves `Host`, every MCP call from the sandbox gets `403` and the
  server log shows `rejected … Host "host.docker.internal:<port>"`. That log
  line is the S3 answer.
- **R11. Fixtures in `testdata/sbx/` are transcribed from documentation**,
  not captured, and a README in that directory says so. Phase 0 replaces
  them.

Open question added to §12:

6. Which of the non-GitHub built-in services count as model providers for
   D8? `copilot` authenticates with a GitHub token, so it is not obviously
   one.
