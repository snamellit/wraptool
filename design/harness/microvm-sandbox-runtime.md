# Docker Sandboxes microVMs as a wraptool container runtime

Status: proposed (investigation only — no implementation planned until the
spike below answers the blocking questions)

## Motivation

`docs/comparison.qmd` frames wraptool and Docker Sandboxes as
complementary: wraptool mediates capabilities (per-subcommand/flag policy,
credential isolation, audit), Sandboxes isolates environments (dedicated
microVM per agent). The comparison's closing claim — "nothing prevents
running wraptool's container runtime inside a harder isolation boundary" —
is currently untested. This note scopes the investigation.

The prize: wraptool's weakest point against a kernel-level attacker is the
boundary itself. The Guix runtime is user/mount namespaces on a shared
kernel; the devcontainer runtime is an ordinary container. A microVM
underneath either (or as a third runtime) would close the
container-escape class while keeping everything that makes wraptool
wraptool — the whitelist, the scrubbing, the audit trail, the harness
pool. Isolation vendors solve isolation; we keep solving mediation.

## Candidate shapes

1. **`--runtime=sandbox`** — a third runtime beside `guix` and
   `devcontainer` (`cmd/guixcontainer.go` backend selection): `wraptool
   up` drives `docker sandbox run` (`sbx`) with a custom agent entry that
   is just our shell/harness, the workspace mounted as the sandbox
   workspace, and the harness pool mounts added. Auto-selection order
   would need a ruling (sandbox when `sbx` on PATH? never auto?).
2. **Sandbox under the devcontainer runtime** — keep `devcontainer up`
   as-is but point it at a Docker endpoint that lands containers inside a
   microVM. Depends entirely on what `sbx` exposes; may be a
   Docker-Desktop-only arrangement.
3. **Reject** — document the combination for users who want to arrange it
   themselves, implement nothing. The honest default if the blocking
   questions below come back negative.

## Blocking questions (the spike answers these first)

- **Extra mounts.** Sandboxes mounts "only your project workspace". Can a
  sandbox carry additional mounts at all — the pool `runtime/` tree
  (read-only!) and `home/` (read-write) at stable paths? The devcontainer
  CLI's `--mount` regex taught us (§12.2, unified-harness-pool.md) that
  "supports mounts" and "supports *read-only* mounts" are different
  claims; verify against the real flag surface, not docs.
- **Host reachability.** The harness inside the microVM must reach the
  host wraptool server (SSE/Streamable HTTP, Bearer). What does the
  sandbox network path look like — bridge with a stable host address
  (like the docker bridge the devcontainer template uses), or a proxying
  arrangement that breaks non-HTTP or rewrites traffic?
- **Credential proxy interplay.** Sandboxes' keychain proxy injects
  secrets at the boundary — the exact opposite of wraptool's model. Can
  the proxy be disabled outright / never enrolled, so the box contains
  no credential path except wraptool's MCP tools? A sandbox that
  helpfully proxies the developer's git credential into the box would
  *weaken* a wraptool setup, not harden it.
- **Custom agent contract.** "Custom agents supported" — what is the
  actual contract (image? command? manifest?), and can it carry our
  entry-wrapped shell + `.mcp.json` wiring instead of a vendor harness
  install? Our harnesses come from the pool, version-locked and
  quarantined; a runtime that insists on installing its own floating
  agent binary defeats the pool.
- **Linux support.** `sbx` installs are documented for macOS/Windows.
  Guix hosts keep the guix runtime regardless, but if plain-Linux hosts
  aren't served, this is a Mac/Windows-only hardening layer — still
  worth having (it's where the devcontainer runtime runs today), but it
  changes the pitch.
- **Stability/licensing.** Experimental preview; commands may change.
  Free tier vs Docker AI Governance boundaries — which features land
  behind the paid tier? Pinning: can we pin an `sbx` version the way we
  pin the provisioning image digest, or is it self-updating (a
  supply-chain surface our own quarantine model would frown at)?

## Non-goals

- Replacing the Guix runtime on Guix hosts. Its value (identical
  manifest-driven toolchain inside and out, no daemon) is orthogonal to
  boundary hardness, and guix hosts are the developer's own trusted
  machines.
- Using microVMs for pool *realization*. The OCI backend's threat model
  (running `npm ci` on locked inputs) is already served; realize output
  is receipt-verified regardless of where it ran.
- Adopting Sandboxes' credential proxy. Ever. wraptool's model is that
  the box holds no credentials, proxied or otherwise.

## Spike plan (Mac, ~half a day)

1. `brew install docker/tap/sbx`; run a trivial custom agent (a shell)
   in a scratch repo. Record the real CLI surface (`sbx --help` all the
   way down) — docs lag the preview.
2. From inside: probe host wraptool reachability (the devcontainer
   template's `test-connection.sh` is the harness), env inheritance,
   what the credential proxy exposes with nothing enrolled.
3. Attempt both pool mounts; check the `runtime/` mount is genuinely
   read-only (EROFS on write, as the Mac field test proved for the
   devcontainer runtime).
4. Write findings back into this note; flip Status to
   `findings` and decide between shapes 1–3.

## Verdict criteria

Adopt (shape 1 or 2) only if: extra read-only mounts work, host MCP
reachability is plain TCP with a stable address, the credential proxy can
be fully absent, and the custom-agent contract accepts our pool-provided
harness. Any miss → shape 3 (document, don't implement) until the
product matures; revisit on Sandboxes GA.
