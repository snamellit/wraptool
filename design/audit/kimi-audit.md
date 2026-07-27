# Security Audit: full-surface review (network edge, token at rest, policy semantics, scrubbing, isolation)

Status: **revalidated — remediation in progress**
Date: 2026-07-22
Revalidated: 2026-07-23 at `d09e8cc`
Scope: whole codebase (~14.5k LOC Go + `features/` shell). Five parallel
read-only sweeps: (1) executor + whitelist, (2) webui, (3) transports +
mcpproxy + mcptools, (4) config + requestlog + paths, (5) container runtime +
init + harness pool + feature scripts. Empirical verification where cheap
(scrub regexes, `ext::` transport behind `--`).
Relationship to prior work: complements `security-audit-policy-enforcement.md`
(2026-07-08). Where this audit re-touches those findings it says so; F1/F5/F6
from that audit remain fixed, F3/F4 are re-confirmed open and re-prioritized
here.

## 2026-07-23 status update

### Changes since the audit

- `07a43c1` split MCP and admin settings, requires authentication whenever the
  admin listener is enabled, and gives the admin plane a separate credential.
- `0fc33a1` refuses to start a container when the active policy resolves inside
  its writable workspace.
- `92c2fa9`, `29825a6`, `816ee14`, and `e0588ae` keep the admin token outside
  that workspace, including symlink, missing-file, and disabled-admin cases.
- `ec14a96` verifies that the MCP token cannot authorize admin rule edits.
- `77d340a` reports Guix profile warm-up failures instead of claiming success.

The first four changes close the original tokenless Guix self-approval chain.
They deliberately do not hide the MCP token from the container, because the MCP
client needs it; the remaining question is how narrowly and safely that token is
stored and exposed.

### Finding status

| Finding | Current status | Revalidation note |
|---|---|---|
| C1 | **Original chain fixed; residual hardening open** | Admin auth is mandatory and distinct, and its file is excluded from the workspace. Guix still shares host networking; different token files may contain equal values; only the workspace, not every writable share, is checked. |
| C2 | **Open** | TCP MCP authentication is still optional; Host/Origin/CIDR restrictions remain absent. |
| C3 | **Open** | The live MCP token is still embedded in a `0644` worktree file with no generic Git exclusion. |
| C4 | **Partially fixed** | Tokenless admin mode is gone. CSRF, Host/Origin checks, security headers, and loopback bind enforcement remain open. |
| H1-H6 | **Open** | Deny semantics, typed/empty flag values, positional policy, scrubbing, and dangerous-flag guardrails are unchanged. |
| H7 | **Tracked by H5** | The non-text MCP scrub gap remains, but should not be tracked twice. |
| M1-M7 | **Open** | Request-log, HTTP, shared-HOME, supply-chain, CWD, edit durability, and worktree-config issues remain. |
| M8 | **Partially fixed** | Admin/MCP credentials are split and the admin token path is protected; logging, permissions, rotation, and token-content equality remain open. |
| M9-M10 | **Open** | Server identity probing and absolute upstream command validation are unchanged. |
| L1-L18 | **Open unless noted** | L7 already has partial collision validation; the remaining P3 observations still reproduce in current code. |

### Additional Guix findings

#### G1. CRITICAL candidate - writable `manifest.scm` is later evaluated by a host Guix process

`bringUpGuixContainer` passes the worktree's `manifest.scm` directly to both
`guix shell -m` invocations (`cmd/guixcontainer.go:72,104,147`) and mounts the
worktree read-write (`:138`). Scheme manifests are executable input, not inert
package lists. A compromised container can therefore replace `manifest.scm`,
wait for the operator to exit, and target the next host-side `wraptool up` when
Guix loads the manifest before entering the new container.

This is the Guix equivalent of M7, but with potential host code execution rather
than only harness configuration poisoning. Confirm the exact evaluation context
with a harmless marker-file PoC on the Guix runner before assigning a final
severity. The current development environment used for this revalidation does
not provide the `guix` executable.

Effective mitigation: store an approved manifest digest outside the worktree and
refuse changed manifests until the operator explicitly trusts the new digest.
Copying the file alone is insufficient because it preserves executable content.
A safe first version can be a fail-closed digest checkpoint with a dedicated
host-side approval command and an actionable error.

#### G2. HIGH - sensitive-path validation omits the writable harness pool HOME

`runUp` rejects config and admin-token paths only when they resolve below the
workspace (`cmd/up.go:103-113,345-409`). Guix separately mounts the shared pool
HOME read-write (`cmd/guixcontainer.go:144`). An operator can place either
sensitive file under that pool and pass the current check, exposing policy or the
admin credential to every project container.

Effective mitigation: build the complete set of writable host shares before
validation and reject policy/admin-token paths beneath any of them. Cover direct,
symlinked-parent, missing-target, and equality cases in unit tests.

#### G3. HIGH - shared writable HOME is a cross-project persistence boundary

M3 remains open: every Guix project mounts the same pool HOME read-write and
starts `bash -l` with it (`cmd/guixcontainer.go:124-144`). One project can alter
shell startup, harness settings, skills, or auth state consumed by another.
Pool directories are also created `0755` (`:181-190`).

Effective first step: create the pool and HOME with `0700` and repair existing
permissions. Target design is a read-only operator-curated skills/extensions
seed plus per-project copy-on-write HOME/config/state. Shared harness
authentication remains an explicit open decision; see `project-isolation.md`.

#### G4. HIGH - shared host networking amplifies every loopback listener flaw

Guix still unconditionally uses `--network` (`cmd/guixcontainer.go:136`), making
host loopback reachable by the untrusted workload. Mandatory admin auth removed
the worst chain, but optional MCP auth (C2), missing HTTP bounds (M2), and any
other loopback service remain exposed.

Effective mitigation: require MCP authentication for TCP transports first. Then
make Guix networking an explicit mode or provide a no-host-network path. A warning
is not a security boundary.

### Effective Guix remediation queue

| Priority | Work item | Benefit | Effort/risk | Guix verification |
|---|---|---|---|---|
| 1 | Confirm G1 and add an approved-manifest digest checkpoint | Closes likely host-code-execution persistence | Medium; workflow change | Harmless manifest PoC, digest-change refusal, explicit approval, then container smoke test |
| 2 | Fix H2/H3 and enable MCP schema validation | Closes direct policy/argv bypasses | Small, low risk | `go test ./internal/whitelist ./internal/mcptools ./cmd` |
| 3 | Write harness configs `0600`, tighten existing modes, add repository-local Git exclusion | Reduces token theft and accidental commits | Small-medium, low risk | `go test ./internal/harness ./cmd`; inspect mode and `git check-ignore` in a temporary repo |
| 4 | Validate sensitive paths against every writable share; make pool directories `0700` | Closes G2 and multi-user exposure | Small, low risk | Unit tests plus Guix mount smoke test |
| 5 | Require auth and resource bounds on TCP MCP; add admin CSRF/Origin protections | Shrinks the network attack surface exposed by `--network` | Medium, medium risk | Host/Guix-container HTTP integration tests; unauthenticated requests must fail |
| 6 | Give introspection a minimal environment, bounded and scrubbed output | Closes a direct server-environment leak | Small-medium, low risk | Guix is useful here because PATH dependencies span store prefixes |
| 7 | Expand credential scrubbing and make truncation safe | Reduces high-impact output leaks | Medium, medium risk | Table tests plus secrets split at the output limit |
| 8 | Prefix-deny, reject option-shaped subcommands, and add positional constraints | Improves least privilege | Medium, compatibility risk | Policy/config tests; migrate examples before changing positional defaults |

Platform-neutral resilience work that is also straightforward to verify on Guix
follows these items: cap request-log fields and tolerate oversized lines (M1),
serialize and strictly validate config edits (M6), and require absolute regular
files for upstream MCP commands (M10).

## Original audit (2026-07-22)

Threat model: the **AI client is untrusted** (prompt-injectable, loops,
crafts arguments). The operator is trusted but fallible (mis-binds, mis-approves).
The container workload is untrusted. The host may be multi-user. wraptool's two
core promises: *only whitelisted commands run* and *credentials never reach the
AI context*. Findings are ranked by exploitability × damage to those promises.

---

## Summary

Architecture is fundamentally sound: deny-before-allow, no shell anywhere
(argv-only `exec.CommandContext`), minimal curated child env (CLI and MCP
upstreams), subcommand pinning per tool, pinned binary paths, constant-time
token compares, 0600 token files, html/template autoescape intact, JSONL
injection-safe via `json.Marshal`, symlink-safe atomic rename.

Exposure concentrates at three seams:

1. **Network edge** — auth optional and *silent* on the MCP listener, no
   Host/Origin validation, wildcard CORS, no CSRF defense, docs suggest
   `0.0.0.0`, and guix `--network` erases the loopback trust boundary the
   empty-token Web UI relies on.
2. **Token at rest** — the single eternal Bearer token is written plaintext
   0644 into the git worktree (`.mcp.json`) with no gitignore guard.
3. **Policy enforcement gaps** — deny semantics weaker than documented,
   boolean constraints don't constrain, empty values collapse argv,
   positionals default-unlimited, and the output scrubber misses PEM bodies,
   creds-in-URL, JSON-quoted and vendor-prefixed tokens.

---

## P0 — Critical chains (fix first)

### C1. Guix `--network` + localhost-trust Web UI = agent self-approves → arbitrary host commands

- `cmd/guixcontainer.go:134` + `internal/webui/webui.go:790` + `cmd/serve.go:327-335`

`guix shell --network` shares the host netns, so the container reaches every
loopback-only host service. The Web UI in empty-token mode trusts loopback
without verifying the bind. Chain: agent in container POSTs
`http://127.0.0.1:<web>/rules/git/allow/add` (tool names guessable) or approves
its own capability requests → whitelist rewritten → arbitrary host commands
with wraptool's credentials. A tokenless MCP listener is equally exposed.
**This breaks the product's core premise.**

Fix: refuse to start (not warn) when a listener token is empty and bind is not
loopback *or* when guix runtime may share the netns; make `--network` a
separate opt-in with a loud warning; document that loopback is not a trust
boundary under `-N`.

### C2. MCP HTTP listener silently unauthenticated; docs suggest `0.0.0.0`; no Host/Origin checks

- `cmd/serve.go:232-237` (`authWrapped` returns handler bare, no warning —
  unlike webui at `:334`), `cmd/up.go:149` (error text suggests `0.0.0.0:8717`),
  mcp-go `sse.go:501-502` (`Access-Control-Allow-Origin: *`, no Origin/Host
  validation in either transport)

Operator binds `0.0.0.0` without a token → any LAN host runs whitelisted
commands. Even on `127.0.0.1`, any website the operator visits reaches the
listener via DNS rebinding (no Host check) and drives tool calls cross-origin.

Fix: fail-closed or loudly warn on non-loopback bind without token (parity
with webui warning); Host-header allowlist + Origin validation; drop wildcard
CORS; source-CIDR allowlist on the listener — see "Listener architecture"
below.

### C3. Bearer token written plaintext 0644 into git worktree, never gitignored

- `internal/harness/harness.go:164-168` (Write), `:217-218` (embeds
  `Authorization: Bearer <token>`), `cmd/guixcontainer.go:83-93`,
  `cmd/init.go:94-100`, `features/src/wraptool/connect.sh:118`

`.mcp.json` / `opencode.json` carrying the live token is written
world-readable at the repo root. Nothing appends it to `.gitignore` or
`.git/info/exclude`. One `git add -A && git push` publishes the single token
guarding every whitelisted host tool (git push, kubectl). On multi-user hosts
any local user reads it. Compounds with C2 (token + reachable listener = full
access) and H8 (one eternal token, no rotation).

Fix: write token-carrying configs 0600; auto-exclude from git (or refuse/warn
when the worktree doesn't ignore it); prefer env-var indirection over
embedding the token.

### C4. Web UI: no CSRF token, no bind enforcement, no security headers

- `internal/webui/webui.go:789-791` + all POST routes (`:89-102`),
  `cmd/serve.go:327-335`; headers absent on all handlers (e.g. `:202,:508,:860`)

No CSRF token anywhere; defense is solely `SameSite=Lax`. In empty-token mode
there is no cookie at all → any web page the operator visits can blind-POST
rule additions; DNS rebinding (no Host check) upgrades to read +
approve-by-ID. Even with a token, `Lax` treats every localhost port as
same-site → any other local app can CSRF the UI. No `X-Frame-Options`/CSP →
clickjack rule edits in empty-token mode. `Config.Validate` never inspects
`web.listen`, so `0.0.0.0` + empty token = unauthenticated remote admin
console that can rewrite the whitelist.

Fix: per-session CSRF token on all POSTs; Host/Origin checks;
`X-Frame-Options: DENY` + `nosniff` + minimal CSP in middleware; refuse
non-loopback bind without token at startup.

---

## P1 — Policy & credential-scrub integrity

### H1. Deny rules are exact-token-match only; documented and shipped as prefix

- `internal/whitelist/policy.go:125-135`; `docs/isolated-environments.md:101`;
  ApplyRequest conflict check `internal/config/edit.go:45-55`

`subcommandMatches` requires `len(rule)==len(actual)` — contradicting its own
comment ("prefix of (or equal to)"). Consequences: `deny: [remote]` does not
block `remote add`; the docs' shipped example `deny: [config, "--global"]` is
a **no-op** (subcommand tokens never contain flags) — an operator who believes
`--global` is revoked while `allow [config]` exists leaves
`git config --global core.hooksPath/core.sshCommand` (RCE on next git op) wide
open. ApplyRequest inherits the false assurance (reports "no conflict" for
narrow-deny/nearby-allow). Re-confirms prior-audit F4, raised to High.

Fix: prefix semantics for deny (deny ≥ allow granularity) + validate/warn on
flag-looking tokens inside `subcommand:` lists; fix comment and conflict check
together.

### H2. `boolean` constraint doesn't enforce valuelessness; requester `flag_types` smuggle values through approval

- `internal/whitelist/constraints.go:50-53` + `internal/config/edit.go:234-255`
  (`applyFlagTypes`) + `internal/executor/executor.go:157-162` +
  `internal/mcptools/handler.go:54-64`

A `boolean` constraint returns `nil` from `validateValue` regardless of value;
`buildArgs` emits `--flag value` when value ≠ ""; `ApplyRequest` writes the
requester's self-certified `flag_types` as authoritative constraints. Exploit:
AI requests `{"flags": ["--namespace"], "flag_types": {"--namespace":
"boolean"}}`, operator approves a "bare switch", AI calls with
`{"--namespace": "kube-system"}` and the value flows unchecked. Prior-audit F2
closed the sanitize path; this hole is via the *type-certification* path.

Fix: `validateValue` rejects non-empty value when `Valueless`; `buildArgs`
never emits a value for boolean-constrained flags.

### H3. Empty-string flag value collapses to bare flag, breaking the two-token argv invariant

- `internal/executor/executor.go:159` + `internal/mcptools/handler.go:54-59`
  + `internal/whitelist/constraints.go:15`

Handler maps both `true` (bool) and `""` (string) to `flags[flag]=""`;
`buildArgs` drops empty values → bare flag. The CLI then consumes the *next*
argv token — another allowlisted flag or the `--` separator — as its value.
Verified shape: `git_commit {"message": "", "amend": true}` → argv
`[commit --message --amend]` → message becomes `--amend`. Also silently
satisfies `required: true` with an empty value. Audit log records the flags
*map*, not real argv — executed command provably diverges from policy/audit
view.

Fix: reject empty-string values for non-boolean flags (SanitizeFlagValues or
handler), or emit `--flag=` for typed string flags; treat empty as absent for
`required`.

### H4. Positionals: unlimited by default, zero content constraint

- `internal/whitelist/policy.go:104` (`ArgConstraints == nil` ⇒ unlimited),
  `internal/mcpproxy/cli_source.go:147`; shipped docs config has no
  `arg_constraints` anywhere

Only guards: no leading `-`, no shell metachars. Absolute paths, `../../`,
tool-magic tokens pass. **Verified:** `git ls-remote -- 'ext::sh -c "…" %S x'`
spawns a shell where `protocol.allow` permits it — the `--` separator does
**not** stop transport parsing. `ssh://`/file URLs similarly unconstrained.
Re-confirms prior-audit F3, raised to High.

Fix: default `positional_max: 0` (opt-in positionals); per-rule path/regex
content constraints; consider rejecting `scheme:` URLs in positionals for
transport-capable tools.

### H5. Scrubber misses whole secret classes (primary remaining credential-leak path)

- `internal/executor/scrub.go:10-47`, `internal/mcpproxy/scrub.go:19-27`

All verified against the live regexes:

| Class | Example that leaks today |
|---|---|
| PEM body | `-----BEGIN OPENSSH PRIVATE KEY-----` redacted, base64 body lines pass — `git show`/`diff` on a key file leaks full key |
| Credentials-in-URL | `fatal: unable to access 'https://oauth2:glpat-secret@gitlab/…'` — git echoes remote URL on any auth failure; most likely real-world leak |
| JSON-quoted secrets | `{"password": "hunter2"}` — separator class `[=:]` can't cross the closing quote; `docker inspect`, `kubectl -o json` |
| Vendor token shapes | `ghp_…`, `glpat-…`, `ya29.…` (gcloud!), `xoxb-…` unpatterned |
| Encoded env secrets | base64 of a secret value passes (`kubectl get secret` output); ≤7-char secrets never scrubbed |
| Non-text MCP content | mcpproxy scrubs only `TextContent`; embedded resources/images pass with secrets unredacted |
| Truncation split | 1MB cap (`executor.go:180-193`) can cut a token — unscrubbed prefix leaks |

Fix: stateful BEGIN…END PEM redaction; `[a-z]+://[^/\s:@]+:[^@\s]+@` URL
pattern; JSON `"key"\s*:\s*"value"` pattern; vendor-prefix patterns; scrub
base64 encodings of env values; recurse into EmbeddedResource; suppress final
partial-token window after truncation.

### H6. No built-in guardrail for known-execution flags

- `internal/whitelist/policy.go:83-92` + `internal/executor/sanitize.go:53`

`git -c core.sshCommand=/tmp/x.sh`, `--upload-pack`, `--exec`, `--config`,
`--ext-header` need no shell metachars → pass sanitize if allowlisted without
a tight pattern. Documented in `docs/git-hardening.qmd` but nothing enforced.

Fix: per-tool default-deny list of known-execution flags requiring explicit
`allow_unsafe` override.

### H7. mcpproxy non-text scrubbing gap

Folded into H5 (same fix batch).

---

## P2 — Resilience / isolation hardening

### M1. One >4MB JSONL line permanently wedges all requestlog reads — AI-reachable

- `internal/requestlog/store.go:284` (scanner cap 4MB, `Err()` returned);
  writers unbounded (`mcptools/request_tool.go:58-112` rationale;
  `handler.go:103-124` denied-call args verbatim)

`bufio.ErrTooLong` makes `List`/`Get`/`UpdateStatus`/`ListToolCalls` all fail
forever while appends keep succeeding. AI submits a 5MB rationale → operator
approve/deny (CLI + Web UI) bricked until manual log surgery.

Fix: truncate fields at append (e.g. 64KB/event); skip oversized lines on
scan; add rotation.

### M2. No HTTP timeouts / body caps on MCP and Web UI servers

- `cmd/serve.go:257-260,337`; mcp-go streamable uncapped `io.ReadAll`
  (`streamable_http.go:338`), SSE decode unbounded (`sse.go:664`)

Unauthenticated slowloris (one goroutine per trickling connection); any token
holder (or anyone when auth off) POSTs multi-GB body → memory exhaustion of
the credential-holding process.

Fix: `ReadHeaderTimeout`/`IdleTimeout`/`MaxHeaderBytes` on both servers;
`http.MaxBytesReader` on bodies.

### M3. Shared rw pool `home/` = cross-project code execution; pool created 0755

- `cmd/up.go:342` + `cmd/guixcontainer.go:141` (rw mount as `$HOME`),
  `cmd/guixcontainer.go:183` (MkdirAll 0755),
  `features/src/wraptool/devcontainer-feature.json:44`

Compromised agent in project A writes `$POOL_HOME/.bashrc`,
`~/.claude.json`, skills, npm-prefix binaries → project B's container
executes them (`bash -l` sources dotfiles). RO `bin/` mitigates direct binary
trojaning; HOME reintroduces the same class. 0755 pool exposes harness
auth/session files to other host users.

Fix: per-project rw overlay for HOME/config/state, with only operator-curated
skills/extensions shared read-only; pool directories 0700. Authentication
sharing needs a separate decision. Read-only binaries should be invoked by
absolute descriptor path or precede writable PATH entries. See
`project-isolation.md`.

### M4. Unpinned supply chain for harness binaries; agy installer unconfined

- `internal/harness/harness.go:90-96` (npm defaults to `latest`, no
  integrity), `:102-105` (agy `curl -fsSL … | bash` **without**
  `guix shell --container`, unlike npmInstall),
  `features/src/wraptool/install.sh:50,66`

Trojaned release lands in the shared pool (all projects) or image as root.
The agy remote script runs as the user with full `$HOME`/network during
`wraptool harness install agy`.

Fix: pin default versions + integrity (`--integrity`/lockfile, sha256 for
agy); unpinned only via explicit override; container parity for agy installer.

### M5. Client-supplied `?cwd=` uncontained by default

- `cmd/serve.go:182-186`, `internal/paths/context.go:72-95`, `cmd/up.go:319`

Empty `allowed_cwd_roots` = warn-only; container A runs whitelisted tools in
worktree B or any host path — breaks the advertised per-worktree scoping
(`up.go:46-47`). Plus symlink-swap TOCTOU between `EvalSymlinks` and `chdir`.

Fix: have host-side `wraptool up` register a server-side project grant before
container startup. Its bearer secret maps to immutable worktree CWD; reject
client-supplied CWD. Apply `allowed_cwd_roots` when issuing grants and retain an
opened directory through child startup. See `project-isolation.md`.

### M6. editConfig round-trip validates leniently; serve/reload strict → approvals silently never apply

- `internal/config/edit.go:156-163` (plain `yaml.Unmarshal`) vs
  `cmd/reload.go:87` (`LoadStrict`)

`requests approve --apply` on a config with an unknown key succeeds and prints
success; the running server's reload then fails and keeps the old policy.
Fail-closed but the security operation is silently wedged. Also: no fsync
(file or dir) around atomic rename (`edit.go:299-327`); read→rename TOCTOU
lost-update between concurrent webui/CLI edits (no lock, `expect` only on
UpdateRule/DeleteRule).

Fix: `KnownFields(true)` in the round-trip; fsync file + dir; package mutex or
flock around editConfig.

### M7. Container can poison worktree `.mcp.json` used later on the host

- `cmd/guixcontainer.go:83-93`

In-container agent rewrites the rw-shared `.mcp.json` URL to an attacker MCP
server → hijacks a future host-side harness session on the same project
(prompt injection + context exfiltration).

Fix: write guix-path harness config into pool home (container-private), or
mark it clearly container-only.

### M8. Token written to logs; single eternal token, no rotation; perms unchecked on read

- `cmd/serve.go:338,364-366` (`webURL()` embeds `?token=<raw>`, logged at
  startup → journal/serve.log); `cmd/up.go:264-284` (one token for all
  projects/containers forever); `:269-271` (existing token file accepted
  without 0600 check); `internal/config/validate.go:40-44` (token/env/config
  files only `Stat`ed)

Fix: log URL with token redacted; issue project grant bearer secrets that map
server-side to immutable worktree context; add expiry/revocation/rotation;
stat-and-warn on loose perms at load/read; warn-or-fail when secret-bearing
files are group/other-readable. See `project-isolation.md`.

### M9. `up` treats a bare TCP dial as "server already running"

- `cmd/up.go:151-155,202-209`

Any local process squatting the port is accepted as the wraptool server; `up`
then injects the real token into containers whose MCP clients present it to
the squatter (which also serves malicious tool metadata).

Fix: probe an identity endpoint (MCP `initialize` verifying serverInfo, or
token-gated health) before trusting the listener.

### M10. `mcp_servers.*.command` unvalidated (PATH lookup at spawn)

- `internal/config/validate.go:94-98` vs `mcpproxy/client.go:187`

Unlike `ToolConfig.Binary` (absolute + exists + regular), a relative upstream
command is PATH-resolved at spawn → PATH manipulation substitutes the
"policed" upstream binary.

Fix: mirror `validateTool` — absolute path, resolve symlinks, regular file.

---

## P3 — Hardening backlog

| # | Finding | Evidence |
|---|---------|----------|
| L1 | Webui login open redirect via backslash (`/\evil.com` passes `sanitizeNextValue`); no login rate limit | `webui.go:884-889,819-829` |
| L2 | Session cookie missing `Secure`; HTTP-only server; cookie value is raw token | `webui.go:830-836`, `cmd/serve.go:337` |
| L3 | Flash msg/detail (config path, YAML snippet) travel in redirect query strings → history/proxy logs | `webui.go:402-409,344,170,466` |
| L4 | AI-controlled strings printed raw to operator terminal / reflected into MCP results (ANSI-escape injection); `\x1b` not in `shellMetachars` | `cmd/requests.go:73-88`, `mcptools/handler.go:68-71,127-152` |
| L5 | `wraptool_request_capability` unbounded: no rate limit, dedup, length caps, rotation; web UI folds whole log per page | `request_tool.go:58-122`, `requestlog/store.go` |
| L6 | Audit identity fields client-asserted (forge `clientInfo.name`); streamable accepts any `mcp-session-id` | `mcptools/handler.go:157-169` |
| L7 | Upstream MCP tool names unvalidated; collisions shadow real CLI tools (last registration wins) | `mcpproxy/source.go:91` |
| L8 | Projection trusts forged `status` in `capability_requested` payload; denied→approved flips unguarded; approve marks approved before apply (approved-but-unapplied on failure) | `requestlog/store.go:233-244,145-170`, `cmd/requests.go:115-136` |
| L9 | Audit log stores raw args/secrets (e.g. `-c http.extraHeader=Authorization:…`); perms 0600 only at creation, pre-existing loose perms kept | `requestlog/store.go:47-58`, `requestlog/types.go:53-75` |
| L10 | Node-tree edits don't resolve anchors/aliases/merge keys (edit-time checks disagree with runtime); yaml.v3 alias-expansion unbounded on hostile config | `config/edit.go:184-216`, `config.go:207` |
| L11 | Untagged YAML scalars for operator values (`yes`, `0123`, `~` re-parse as non-string → edit rejected; fails safe, usability bug); `strNode` exists unused | `config/edit.go:265-267`, `rules.go:58,74,220-222` |
| L12 | Pidfile trusted without identity check (PID reuse → `server stop` SIGTERMs unrelated process); opens follow symlinks | `cmd/server.go:66-105` |
| L13 | Feature scripts: option values frozen into sourced `options.env` in single quotes (quote-breakout code exec at postCreate); JSON built by printf unescaped; `urlencode` misses `+"\`; pool-HOME mount point `chmod 0777` when unmounted | `features/src/wraptool/install.sh:34-35,87-92`, `connect.sh:96-101` |
| L14 | `devcontainerDeclaresPoolMount` raw substring check (comment suppresses mount); label-only container lookup in `down --rm` | `cmd/up.go:388-395`, `cmd/down.go:85-99` |
| L15 | Nondeterministic argv order from flag map iteration (order-sensitive CLIs); JSON null/array flag values stringify to `<nil>`/`[a b]`; float64 renders `1e+06`; empty positional passes sanitize | `executor.go:157`, `handler.go:61-64`, `sanitize.go:35` |
| L16 | Policy/config TOCTOU on hot-reload: stale `*ToolConfig` binary/env with new policy decision | `mcptools/handler.go:68,82`, `policy.go:29` |
| L17 | `--args` flag param-name collides with reserved positional param (unreachable, availability only) | `mcptools/schema.go:22`, `handler.go:39` |
| L18 | `WRAPTOOL_CONTAINER` marker advisory only — must never be treated as a security boundary | `cmd/up.go:414` |

---

## Verified NOT vulnerable (deliberately checked)

- Deny>allow precedence; subcommand pinning per MCP tool (client cannot
  smuggle subcommand tokens).
- `--config=evil` single-token form and combined short flags blocked by
  exact-match allowlist; `ParamToFlag` cannot forge unlisted flags.
- Constraint patterns correctly anchored (`^(?:…)$`; Go `$` = end-of-text).
- No shell anywhere; binary path config-only; client cannot inject env vars.
- Token generation 256-bit `crypto/rand`; token file/socket 0600;
  `subtle.ConstantTimeCompare` on both listeners; auth wrapper precedes
  routing (no path/trailing-slash bypass).
- html/template autoescape intact — zero `template.HTML/JS/URL` usage; no XSS
  surface found in any template.
- JSONL injection impossible (`json.Marshal` escapes); ServeMux path-cleaning
  makes `/static/../` a redirect, not a bypass; `ParseForm` caps bodies 10MB.
- mcpproxy does not bypass the whitelist (tool-name gate at registration AND
  per call); upstream spawned with curated env (no `os.Environ()` leak);
  stdio-only upstreams (no SSRF surface).
- Atomic rename onto symlinked path replaces the symlink (safe); temp files
  0600; store mutex-guarded.
- CWD containment uses `EvalSymlinks`+`Rel` correctly (modulo M5 TOCTOU).

---

## Listener architecture: bind restriction design (2026-07-22 discussion)

Question: are separate binds for MCP service and admin UI a real security
benefit, and how should the MCP bind be restricted given containers must reach
it without loopback access?

### The split is justified — admin-plane vs data-plane

- **Asymmetric power.** MCP endpoint compromise (auth bypass) = attacker runs
  *whitelisted* commands, bounded by policy. Admin UI compromise = rule CRUD +
  approvals = *rewrites* policy = arbitrary commands. Strictly worse, so it
  deserves strictly smaller exposure. One socket forces the network-facing
  surface to include the most powerful endpoint.
- **Exposure requirements genuinely differ.** MCP *must* be network-reachable
  (devcontainers reach the host via `host.docker.internal` over the docker
  bridge). Admin's only user is the operator on the host — zero need for
  network reachability.
- **Failure isolation.** Separate tokens, middleware, `http.Server`s: an auth
  bug on `/mcp` doesn't touch `/rules`; admin works even when MCP transport is
  stdio-only.
- **Different client classes.** Admin is browser-facing (CSRF, rebinding,
  clickjacking — C4); MCP is agent-facing (Host checks, no browser concerns).
- The alternative (single listener + loopback-only middleware on admin
  routes) is *more* fragile: one routing/auth bug exposes everything. Two
  sockets is the honest encoding of different exposure.
- Caveat: the split's value is conditional — empty-token admin "localhost
  trust" evaporates when guix `--network` shares the netns (C1), and a silent
  unauthenticated MCP listener (C2) makes the split pointless. Both layers
  must fail closed.

### Bind-address pinning rejected

Binding MCP to "the docker bridge IP" does not work: multiple docker networks
(each `br-xxxx` with its own subnet), compose networks, podman conventions
(slirp4netns vs pasta), Docker Desktop VM NAT, and networks created *after*
server start. Enumerating bridges/interfaces at startup or `SO_BINDTODEVICE`
per network has the same dynamism problem. Don't chase the bind side.

### Key insight: container source IPs are topology-stable

Bridge addresses vary; the source *class* doesn't:

| Runtime/netmode | Source IP seen by host listener |
|---|---|
| docker default bridge | 172.17.x.x (RFC1918) |
| docker custom/compose network | 172.18+/16, 192.168.x (RFC1918) |
| podman bridge | 10.88.x.x (RFC1918) |
| podman slirp4netns (rootless) | 10.0.2.x (RFC1918) |
| Docker Desktop (Mac/Win) | VM NAT, private range |
| WSL2 | NAT, private range |
| guix `--network` | 127.0.0.1 (loopback) |

All RFC1918 / ULA / loopback.

### Design: layered restriction (each layer independent)

1. **Token mandatory on any non-loopback bind — the actual boundary.** Once
   auth fails closed, bind width stops being the security mechanism and
   becomes exposure reduction. Survives every topology, including unthought
   ones.
2. **Source-CIDR allowlist on the listener — topology-agnostic.** New config
   `server.allowed_cidrs`, default `127.0.0.0/8, ::1/128, 10.0.0.0/8,
   172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, fc00::/7`. Wrap the
   listener and drop non-matching connections at `Accept()` (before HTTP,
   before auth — cheap):

   ```go
   func (l *cidrListener) Accept() (net.Conn, error) {
       for {
           c, err := l.Listener.Accept()
           if err != nil { return nil, err }
           ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
           if !allowed(l.nets, net.ParseIP(ip)) { c.Close(); continue }
           return c, nil
       }
   }
   ```

   Kills the worst case: `0.0.0.0` on a VPS/CI runner with a public IP →
   internet-wide exposure. LAN hosts are usually RFC1918 too, so this layer
   doesn't exclude the LAN — layer 1 does. Exotic netmodes (podman pasta can
   rewrite source to the host's external IP) → operator overrides one config
   key. Configurable, not hardcoded, for exactly that reason.
3. **Host-header allowlist** — DNS-rebinding protection, orthogonal.
4. **Optional zero-network-exposure path: unix socket.** Unix transport
   already exists (`cmd/serve.go:279`); bind-mount the socket into the
   container (docker/podman both support), filesystem perms become the
   boundary, no IP question at all. Friction: harness MCP clients speak HTTP
   URLs, so the container needs a `socat TCP-LISTEN` → `UNIX-CONNECT` shim in
   the feature scripts. Defer; do 1–3 first.

---

## Remediation plan (suggested batches)

1. **Network edge (P0, one PR):** fail-closed auth posture on both listeners
   (refuse non-loopback bind without token), source-CIDR allowlist per
   "Listener architecture" (layer 2), Host-header allowlist, drop wildcard
   CORS, CSRF tokens + security headers on webui, `--network` opt-in
   warning, `.mcp.json` 0600 + auto-gitignore. Closes C1–C4.
2. **Policy semantics (P1, one PR):** prefix deny + flag-token validation in
   subcommand lists (H1), boolean valuelessness enforced at validateValue +
   buildArgs (H2), reject empty-string values for typed flags (H3),
   `positional_max: 0` default + content constraints (H4).
3. **Scrub overhaul (P1, one PR):** PEM block redaction, URL-credential,
   JSON-quoted, vendor-prefix patterns, base64 env values, EmbeddedResource
   recursion, truncation window (H5). Add per-tool dangerous-flag default-deny
   (H6).
4. **Resilience (P2):** requestlog append caps + scan tolerance (M1), HTTP
   timeouts + MaxBytesReader (M2), strict round-trip in editConfig + fsync +
   edit lock (M6), upstream command validation (M10).
5. **Isolation & supply chain (P2):** read-only shared skills/extensions seed +
   per-project COW HOME/state + pool 0700 (M3), version/integrity pinning + agy
   containerization (M4), server-issued project grants with immutable CWD (M5,
   M8), container-private `.mcp.json` (M7), rotation + redacted logs (M8),
   server identity probe (M9). See `project-isolation.md`.
6. **Backlog (P3):** pick off L1–L18 opportunistically; L13 shell-script
   fixes pair naturally with M4.
