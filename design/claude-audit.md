# Security Audit — wraptool

Date: 2026-07-22
Method: 5 parallel subsystem audits (executor, whitelist policy, web UI, MCP
transport/auth, config/introspect/requestlog). Each finding verified against
code; PoCs noted where run. Ranked by exploitability × blast radius.

Two recurring themes:

1. **Auth silently off on a non-loopback bind** — the same all-or-nothing gate
   in both the MCP transport and the Web UI, in exactly the configuration the
   docs steer operators toward.
2. **Credential-leak paths that undercut the core isolation promise** — executor
   `HOME`, introspection env, and scrub gaps let a wrapped tool reach or echo
   real secrets despite the "minimal child env" / scrubbing design.

---

## P0 — fix now (bypass the core security boundary)

### 1. HIGH — Whitelist bypass: valueless/boolean flags smuggle an arbitrary argv token
`internal/whitelist/constraints.go:51-53` + `internal/executor/executor.go:157-162`

`validateValue` returns `nil` for a valueless/`type: boolean` flag (and for an
untyped allow-listed flag) **without inspecting the value**. `buildArgs` then
emits `flag, value` as two argv tokens whenever `value != ""`, never consulting
boolean-ness. Because MCP input-schema validation is off (finding #2), the AI
sends the "boolean" param as a string payload; `SanitizeFlagValues` only blocks
values starting with `-`, so any other token passes.

Concrete bypass (verified with PoC against real code): rule
`allow: git log, flags: [--oneline], arg_constraints: {positional_max: 0}`. AI
calls `git_log {"oneline": "/etc/passwd"}` → policy returns **Allowed=true**,
executor runs `git log --oneline /etc/passwd`. The `/etc/passwd` token bypasses
`positional_max: 0` — an extra pathspec/refspec the operator forbade.

Fix: in `validateValue`, for a valueless kind reject a non-empty value
(`if k.Valueless { if value != "" { return err }; return nil }`). Defense in
depth: executor should drop the value for known-valueless flags rather than
trusting the caller.

### 2. HIGH — MCP input-schema validation disabled (enabling condition for #1)
`cmd/serve.go:85-89`

`server.NewMCPServer` is built without `server.WithInputSchemaValidation`. In
mcp-go v0.54.0, `handleToolCall` only validates arguments when the validator is
non-nil, so declared per-flag types and the server-forced
`additionalProperties:false` are advisory only. This is what turns #1 from
theoretical into exploitable.

Fix: enable `server.WithInputSchemaValidation`. Still fix #1 — the policy must be
the authoritative gate regardless of schema enforcement.

### 3. CRITICAL — Web UI CSRF → privilege escalation (worst in tokenless mode)
`internal/webui/webui.go:88-102`

No CSRF token, no `Origin`/`Referer` validation on any state-changing POST
(approve/deny, tool add/delete/metadata, rule add/update/delete). Only defense is
the session cookie `SameSite=Lax` — and in localhost-trust mode (`token == ""`,
the documented default) `AuthMiddleware` returns `next` unwrapped, so every POST
is accepted with **no cookie at all**.

Exploit: operator has the localhost UI configured and visits any attacker page;
that page silently `fetch('http://127.0.0.1:8718/tools/add', {method:'POST', …})`
then `/rules/<tool>/allow/add` → AI now has an unreviewed tool with an
attacker-chosen binary path. Approving a pending capability mutates `config.yaml`
and the server hot-reloads it.

Fix: per-session CSRF token in every form, validated on POST, **independent of the
auth token** so localhost-trust mode is covered. Set `SameSite=Strict`. Reject
POSTs whose `Origin`/`Sec-Fetch-Site` is cross-site.

### 4. HIGH — Auth silently off on a non-loopback bind (MCP + Web UI, same root cause)
`cmd/serve.go:232-237` (`authWrapped`) and `internal/webui/webui.go:790-792`

When `auth_token_file == ""` the handler is returned **unwrapped** — no auth.
The docs recommend `listen: 0.0.0.0:8717` so the container can reach the server,
and the guix container runs `--network` (shared host netns). Nothing ties a
wildcard/non-loopback bind to "auth required," so an operator who follows the
docs but omits the token exposes an unauthenticated, credential-backed MCP server
(and the full CRUD Web UI) on every interface / the LAN. No `X-Forwarded-For`
trust exists (checked) — the risk is purely the unvalidated bind.

Fix: in `config.Validate` / `serveSSE`, refuse to start (or force-generate a
token) when the resolved bind host is not loopback and no token file is set. Fail
closed. Same guard for `web.listen`.

---

## P1 — credential leaks that undercut isolation

### 5. CRITICAL — Bearer token written world-readable in the shared/tracked workspace
`internal/harness/harness.go:167`

`os.WriteFile(path, content, 0644)`. The rendered harness MCP config
(`.mcp.json` etc.) embeds the live Bearer token and, in the guix runtime, is
written into the shared workspace root — which is the project's git worktree and
a `--share=root=root` bind mount. The highest-value secret in the system (it is
the credential-isolation boundary) is stored world-readable, inside the repo tree
where it can be accidentally `git add`ed.

Fix: write harness configs `0600`; keep out of the tracked tree (gitignore or
out-of-tree location). Never wider than `0600`.

### 6. HIGH — Introspection runs wrapped tools with wraptool's full environment
`internal/introspect/introspect.go:192`

`runTool` calls `exec.CommandContext` and **never sets `cmd.Env`**, so every
`--help`/`--version`/`help` probe inherits all of wraptool's environment. This
directly contradicts the "Minimal child env" design and the sibling
`executor.Run`, which sets `cmd.Env = ChildEnv(...)`. At startup / reload /
cache-miss, a tool whose help echoes env-derived defaults (or that phones home, or
is a shim) sees wraptool's credentials; introspected descriptions then flow into
the MCP schema shown to the AI. Also missing vs executor: no `cmd.Dir` and no
`Scrub` before caching.

Fix: `cmd.Env = executor.ChildEnv(tc.Binary, resolvedEnv)`; scrub help output
before caching.

### 7. HIGH — Executor forwards `HOME` unconditionally
`internal/executor/executor.go:59`

`"HOME=" + os.Getenv("HOME")`. The package's stated invariant is "never inherit
wraptool's env except PATH," but `HOME` passes straight through. In the
non-container deployment the wrapped tool now points at the real `$HOME`, where
`~/.aws/credentials`, `~/.config/gh/hosts.yml`, `~/.kube/config`, `~/.netrc`,
git credential store, and gcloud ADC all live — and (per #8) can echo them back
unredacted. Mitigated only by the container HOME mount; the executor package
itself provides no isolation, so the guarantee is deployment-dependent while the
comment asserts it unconditionally.

Fix: make `HOME` explicit/overridable per tool config, default to a scratch dir;
at minimum correct the comment.

### 8. HIGH — Credential scrub runs after truncation and misses bare tokens
`internal/executor/executor.go:126-137` and `internal/executor/scrub.go:10-27`

Two gaps:

- `limitedWriter` truncates raw output at the 1MB byte boundary, and `Scrub` runs
  only on the already-truncated string. A secret straddling the cut is sliced in
  half; the surviving head fragment may no longer match the anchored regex →
  partial credential reaches the AI context.
- `ScrubCredentials` only redacts value-like secrets (a `key=value` shape, a
  known prefix, or a sensitive key name). A secret printed bare — `ghp_…`,
  `glpat-…`, `xoxb-…`, `sk-…`, a raw JWT `eyJ….….…` — on its own line matches no
  pattern. `ScrubEnvVars` only covers credentials wraptool itself injected, not
  ones the wrapped tool obtains on its own and echoes.

Fix: scrub the full captured buffer **before** any length-based cut (or reserve a
scrub-safe margin); add high-signal prefix patterns for common token formats.
Note: Go's RE2 means the scrub regexes are not ReDoS-vulnerable (verified).

---

## P2 — hardening (operator-tricked or defense-in-depth)

### 9. MEDIUM — Subcommand tokens not sanitized
`internal/config/validate.go:159-168`, `internal/config/edit.go:218-226`

An allow rule's `subcommand` is only checked for non-emptiness, then appended
verbatim to argv — it is **not** run through the positional/flag-value sanitizers.
A token beginning with `-` (e.g. `--upload-pack=/tmp/evil`, `--output=…`) becomes
a global option to the tool, at introspection time and on every call. Presented
to the operator as an innocuous "subcommand," so review is misled.

Fix: reject any subcommand token starting with `-`, or require
`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`.

### 10. MEDIUM — `env_files` accepts arbitrary/traversing paths
`internal/config/config.go:339-358` (`resolveEnv`)

No path containment: `/etc/passwd`, `~/.aws/credentials`, `../../secret` are read
and each `KEY=VALUE` line injected into the child env. Operator-controlled in the
config file (low), **but** the Web UI now edits tool metadata including
`env_files` (commit 4105818), so anyone with UI access can point it at a host
secret and exfiltrate through the tool.

Fix: constrain `env_files` to an allow-listed root (reuse `AllowedCWDRoots`),
reject `..` segments; gate UI-supplied `env_files` more tightly than config-file
ones.

### 11. HIGH — Unix-socket peer CWD trusted with no UID check + TOCTOU
`internal/paths/context.go:55-63`, `cmd/peer_linux.go`

For the unix transport, `ResolveCWD` reads `/proc/<peer-pid>/cwd` and runs the
tool there. The peer PID comes from `SO_PEERCRED` but nothing asserts
`ucred.Uid == os.Getuid()`, and the pid can be reused between the syscall and the
`readlink` (TOCTOU). With default config (`allowed_cwd_roots` empty, warn-only)
the peer's cwd is used unchecked. Socket is `0600` (same-UID) so blast radius is
bounded — hence HIGH not CRITICAL.

Fix: assert the peer UID before trusting it; make `allowed_cwd_roots` containment
default-on rather than warn-only.

### 12. MEDIUM — Session cookie is the raw token, no `Secure` flag
`internal/webui/webui.go:830-836`

`HttpOnly` + `SameSite=Lax` but no `Secure`, and the cookie `Value` is the shared
token verbatim (no derived/opaque session id, no rotation, no expiry). If the UI
is ever served beyond loopback without TLS the token rides in cleartext and is
replayable; a single cookie leak = permanent full access until the token file
changes.

Fix: issue an opaque random session id mapped server-side; set `Secure` when the
request is TLS; add expiry. (Token comparison is already constant-time —
`subtle.ConstantTimeCompare` — verified.)

### 13. MEDIUM — Config edits are not serialized
`internal/webui/webui.go` edit handlers → `internal/config/edit.go:121-166`

`editConfig` is an unsynchronized read-modify-write-atomic-rename with no file
lock or process mutex; net/http runs each request in its own goroutine. Two
concurrent approves/rule edits both read the old file and the second write
clobbers the first (lost update). The `expect` guard only checks the rule's prior
subcommand at a fixed index — not a version/CAS token — so edits to different
rules race with no protection. Integrity, not direct escalation (validation still
runs per write), but an operator can silently lose an intended deny/revoke.

Fix: wrap all `editConfig` calls behind one `sync.Mutex` (or advisory flock on the
config path) so writes serialize.

### 14. MEDIUM — Token accepted via `?token=` URL query
`internal/webui/webui.go:820`, `cmd/serve.go:365` (`webURL`)

The UI accepts the token in the query string and `up` prints a clickable URL with
`?token=<token>` embedded. Query strings land in shell scrollback, terminal logs,
browser history, and any future reverse-proxy access log. Mitigated: UI is
localhost-only by design and the middleware strips the token from the URL after
setting the cookie, but it transits the URL at least once and is printed to the
terminal.

Fix: prefer the POST login form / Bearer for scripts; avoid printing the
tokenized URL (or print only to a TTY and document the exposure).

---

## Low / noted (no action strictly required)

- **Introspection cache path uses unsanitized `toolName`** —
  `internal/paths/paths.go:56-57`; a key like `../../foo` escapes the cache dir.
  Bounded: keys are operator-authored and the cache is presentation-only (config
  `flag_constraints` override introspection; the whitelist enforces on config, so
  a poisoned cache cannot widen policy). Fix: `filepath.Base`-clamp the tool name.
- **Audit log not tamper-evident** — `internal/requestlog/store.go:228-268`; a
  request's `approved` status is a projection over appended events, so anyone who
  can append to the JSONL can forge a `capability_approved`. Acceptable under the
  threat model (log is 0600, server-written, no AI-reachable write path but
  `Append`, which only emits `pending`). JSONL injection is prevented
  (`json.Marshal` escapes newlines). Fix only if tamper-evidence is a goal:
  hash-chain / HMAC per line.
- **Sanitizer allows tab/control chars** — `internal/executor/sanitize.go`;
  `shellMetachars` misses `\t`, C0 controls, NUL, U+2028/U+2029. Defense-only (no
  shell is used, flag names are allow-listed). Fix: also reject `unicode.IsControl`
  runes.
- **Predictable pidfile/lock/socket paths** — `cmd/server.go`, `cmd/up.go`; files
  are 0600 under a 0700 XDG runtime dir (good). Weaker only in the cache-dir
  fallback when `$XDG_RUNTIME_DIR` is unset. Fix: ensure the fallback dir is 0700
  per-user; consider `O_EXCL` on lock creation.
- **MCP 401 vs 403 status divergence** — `cmd/serve.go:384/389`; distinguishes
  "no Bearer" from "wrong token" (minor oracle), and `ConstantTimeCompare` leaks
  token length on mismatch (standard, low impact for a 32-byte secret).

---

## Verified sound (checked, no finding)

- **Deny-before-allow ordering** and **default-deny** — correct in both
  `Policy.Evaluate` and `MCPPolicy.Evaluate` (PoC: deny beats allow; unknown
  tool/subcommand/server → `Allowed:false`).
- **Subcommand injection** — subcommand is fixed per-handler, not client-supplied;
  exact-length match, no path/traversal semantics.
- **Option injection / `--` handling** — positional args and flag values reject a
  leading `-`; positionals emitted after a `--` separator; no shell, so `=`,
  spaces, globs are literal argv tokens.
- **Pattern matching / ReDoS** — anchored `^(?:…)$`, RE2 (no backtracking),
  operator-supplied, invalid patterns fail **closed**.
- **`request_capability` privilege escalation** — only appends a `pending`
  request event; approval is strictly out-of-band (`requests approve --apply` /
  Web UI), which re-validates. The AI cannot self-approve.
- **Token generation** — `crypto/rand`, 32 bytes, base64; token file 0600, dir
  0700.
- **Server env / token leaking to child** — `executor.ChildEnv` builds a minimal
  env; `WRAPTOOL_TOKEN` does not reach wrapped tools. Token passed to the
  container via env, not argv (not in `ps`).
- **Constant-time comparisons** — MCP and Web UI both use
  `subtle.ConstantTimeCompare`.
- **XSS** — no `template.HTML`/`template.JS`/`template.URL` anywhere; all
  user-controlled data flows through `html/template` auto-escaping. Open redirect
  (`sanitizeNextValue`) and path traversal (embedded `fs.Sub`) both handled.
- **ApplyRequest fidelity** — applies exactly the reviewed fields; deny-conflict
  check is exact-match and consistent with enforcement; `flag_types` filtered to
  string/integer/boolean and round-trip re-validated.
- **Atomic write** — `writeAtomic` creates the temp in-dir, chmods 0600 before
  rename, cleans up on every error path.
- **YAML** — yaml.v3 v3.0.1 has alias-bomb limits; config is operator-authored.
- **File perms** — request log 0600 / dir 0700; cache 0600 / dir 0700.

---

## Suggested order of attack

1. **#1 + #2 together** — the real whitelist bypass and its enabler.
2. **#4** — the config the docs recommend is the vulnerable one; fail closed.
3. **#3** — CSRF in the tokenless default is a one-click escalation.
4. **#5, #6, #7, #8** — close the credential-leak paths that make the isolation
   promise real.
5. P2 hardening.
