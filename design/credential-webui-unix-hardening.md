# Design: credential, Web UI, and Unix transport hardening

Status: **planned - no implementation started**
Created: 2026-07-24
Baseline reviewed: `18188f7` (`wraptool-v0.2.2`)

## 1. Purpose

This document is the persistent implementation plan for three groups of open
security-audit findings:

1. Credential exposure through generated harness files, child environments,
   introspection, output truncation, and MCP result forwarding.
2. Web UI browser and listener hardening.
3. Unix transport peer identity and client-selected working-directory trust.

The work is intentionally split into small pull requests. Each pull request
must leave the repository in a releasable state, include its own tests and
documentation, and update the progress ledger in this document after merge.

This plan consolidates the still-relevant findings in:

- `design/kimi-audit.md`
- `design/claude-audit.md`
- `design/security-audit-policy-enforcement.md`

## 2. Threat model and invariants

The AI client and container workload are untrusted. The operator is trusted but
may make configuration mistakes. The host may be multi-user.

The implementation must preserve these invariants:

- No shell is introduced. Wrapped commands continue to use explicit argv.
- Policy remains authoritative even if an MCP client ignores its input schema.
- Credentials are not deliberately returned to the AI context.
- Browser authentication is not accepted from ambient cross-origin requests.
- A Unix client cannot select an arbitrary host working directory by default.
- Path authorization and child startup refer to the same directory object.
- Existing compatibility exceptions are explicit configuration, not silent
  defaults.

Credential scrubbing is defense in depth, not proof that an unknown secret
format can never be emitted. Opaque payloads that cannot be inspected must be
rejected by default if wraptool is to make a strong output-boundary claim.

## 3. Decision gates

The following are proposed fail-closed defaults. A maintainer must mark each
decision accepted before the dependent implementation starts.

| ID | Proposed decision | Status |
|---|---|---|
| D1 | The admin listener accepts only literal `127.0.0.1` and `::1` addresses. Hostnames, wildcard binds, and non-loopback addresses fail validation. | proposed |
| D2 | Query-string admin authentication is removed. Browser users log in through the form; scripts retain Bearer authentication. | proposed |
| D3 | Replace client-selected CWD with server-issued project grants: `wraptool up` registers the canonical worktree before container startup, and the project bearer secret resolves to immutable server-side CWD metadata. `mcp.allowed_cwd_roots` constrains grant issuance. See `project-isolation.md`. | proposed |
| D4 | Opaque MCP image, audio, and blob content is suppressed by default and requires an explicit per-server unsafe opt-in. | proposed |
| D5 | Host `HOME` inheritance requires `inherit_home: true`; otherwise the tool or MCP server must configure an explicit `HOME`. | proposed |

Additional decisions that are not expected to be controversial:

- Remove implicit `/proc/<peer-pid>/cwd` discovery.
- Require same-effective-UID Unix peers on Linux.
- Require Bearer authentication on platforms where wraptool cannot establish a
  kernel-backed same-user peer identity.
- Reject empty admin and MCP tokens.
- Reject admin and MCP token files whose trimmed contents are equal, even when
  the files have different paths and inodes.

## 4. Delivery rules

Every implementation agent must:

1. Start from current `origin/main`, not the baseline commit recorded above.
2. Implement one plan ID per branch unless this document explicitly combines
   two IDs.
3. Read the current code and tests again before editing; audit line numbers are
   historical evidence, not an implementation API.
4. Add regression tests that fail against the pre-fix behavior.
5. Update user documentation in the same pull request as a behavior change.
6. Avoid compatibility shims unless this document calls for one.
7. Run the focused checks and the repository-wide checks listed below.
8. Ask a reviewer agent to inspect the final diff before opening the PR.
9. Update the progress ledger only after the PR merges.

## 5. Progress ledger

Allowed states: `not started`, `in progress`, `review`, `merged`, `blocked`, and
`cancelled`.

| ID | State | Depends on | Branch / PR | Completion evidence |
|---|---|---|---|---|
| CRED-1 | not started | none | | |
| CRED-2 | not started | none | | |
| CRED-3 | not started | CRED-2 | | |
| CRED-4 | not started | CRED-2, CRED-3 | | |
| CRED-5 | not started | CRED-2 | | |
| CRED-6 | not started | CRED-5, D4 | | |
| CRED-7 | not started | CRED-4, D5 | | |
| WEB-1 | not started | D1 | | |
| WEB-2 | not started | WEB-1, D2 | | |
| WEB-3 | not started | WEB-2 | | |
| WEB-4 | not started | WEB-3 | | |
| UNIX-1 | not started | none | | |
| UNIX-2 | not started | UNIX-1, D3 | | |
| UNIX-3 | not started | UNIX-2, CRED-3 | | |
| UNIX-4 | not started | UNIX-1 | | |

## 6. Credential-leakage workstream

### CRED-1: Protect token-bearing harness files

Goal: reduce local disclosure and accidental publication of the live MCP token.

Primary areas:

- `internal/harness`
- `cmd/init.go`
- `cmd/guixcontainer.go`
- `features/src/wraptool/connect.sh`

Required behavior:

- Write generated token-bearing harness configuration with mode `0600`.
- Tighten an existing generated file that has broader permissions.
- Add generated worktree files to the repository-local Git exclusion file when
  a Git worktree is available. Do not modify a project's tracked `.gitignore`
  without an explicit separate decision.
- Detect a generated token file that is already tracked and return an
  actionable error or high-visibility warning before rewriting it.
- Never print the token while reporting permission or Git-state problems.

Acceptance criteria:

- New and rewritten files are `0600` under a permissive umask.
- Existing broader files are tightened.
- `git check-ignore` reports the generated file as excluded.
- A tracked file is not silently treated as safe.
- Harness generation tests cover Claude and every other registered harness that
  embeds authorization material.

### CRED-2: Expand deterministic textual scrubbing

Goal: cover the high-signal secret forms currently missed by
`internal/executor/scrub.go`.

Required behavior:

- Redact a complete private-key block from `BEGIN` through its matching `END`.
- Redact to end-of-output when a private-key block is unterminated.
- Redact the full userinfo portion of credential-bearing URLs.
- Recognize JSON-quoted sensitive keys and redact their string values.
- Recognize conservative, minimum-length vendor token forms, including common
  GitHub, GitLab, Google OAuth, Slack, cloud access-key, and API-key prefixes.
- Recognize high-confidence bare JWTs.
- Generate literal and standard/raw URL-safe base64 candidates for configured
  secret environment values.
- Deduplicate candidates and replace longest values first.
- Treat short values as secrets only when their environment key is
  credential-like; do not globally replace every short configuration value.

Implementation guidance:

- Do not combine all detectors into one regular expression.
- Use stateful handling for PEM blocks.
- Keep every detector table-driven and independently testable.
- Preserve useful surrounding context and valid JSON where practical.

Acceptance criteria:

- Synthetic tests cover every supported token class and near-miss false
  positives.
- No real credentials appear in fixtures.
- Existing scrubbing behavior remains covered.
- `docs/security.qmd` lists only patterns the implementation actually handles.

### CRED-3: Scrub safely around output limits

Goal: prevent a token split by the current one-MiB raw-output boundary from
escaping scrubbing.

Primary areas:

- `internal/executor/executor.go`
- `internal/executor/executor_test.go`
- `internal/mcptools/handler.go`

Required behavior:

- Keep memory bounded; do not buffer unlimited child output.
- Retain bounded lookahead beyond the public output limit.
- Scrub captured content before applying the public limit.
- If output exceeded the limit, return only complete safe text before the
  boundary and discard an incomplete final line or token window.
- Preserve valid UTF-8.
- Keep stdout and stderr limits explicit as one MiB per stream.
- Preserve accurate truncation state and user-facing warnings.

Acceptance criteria:

- Tests place literal, encoded, vendor, URL, JSON, and PEM secrets across the
  boundary.
- A huge unbroken line is omitted instead of partially returned.
- Complete safe lines before the limit remain available.
- Both stdout and stderr paths are covered.
- The implementation does not materially increase the maximum captured memory.

### CRED-4: Isolate, bound, and scrub introspection

Goal: make fixed help/version probes follow the same environment and output
boundary as normal wrapped execution.

Primary areas:

- `internal/introspect/introspect.go`
- `internal/introspect/cache.go`
- `internal/introspect/types.go`
- `internal/executor`

Required behavior:

- Do not inherit the wraptool server's ambient environment.
- Use the tool's resolved, curated environment through the executor environment
  builder.
- Use the configured tool working directory, never a client-selected CWD.
- Bound combined help/version output.
- Scrub output before parsing, override merge, cache write, or MCP exposure.
- Remove the unused version probe if it serves no validity purpose; otherwise
  scrub its first-line result.
- Add a cache security/schema epoch so caches written before this fix miss once.
- Include non-secret inputs such as working directory and env-file metadata in
  cache validity where they affect help output.
- Never add raw secret values to the externally reported config hash.
- Write cache files atomically with mode `0600` and a `0700` parent.

Acceptance criteria:

- A parent-environment canary is absent from probe output.
- Explicitly configured environment reaches the probe but its secret value does
  not reach parsed metadata or cache bytes.
- Oversized help is bounded.
- The configured working directory is used.
- A pre-fix cache epoch is invalidated.
- The AI-visible config hash does not reveal or directly encode secret values.

### CRED-5: Scrub structured MCP output and discovery metadata

Goal: apply credential handling to every inspectable MCP result and schema
surface, not only `TextContent`.

Primary areas:

- `internal/mcpproxy/scrub.go`
- `internal/mcpproxy/handler.go`
- `internal/mcpproxy/source.go`
- `internal/mcpproxy/mcpproxy_test.go`

Required behavior:

- Recursively scrub strings in structured maps and arrays.
- Redact values directly when a structured key is credential-like.
- Scrub embedded text resources, resource links, nested tool results, tool-use
  input, top-level structured content, and metadata maps.
- Scrub upstream tool descriptions and string values in input schemas before
  registration.
- Preserve data type and structural shape where possible.
- Do not log raw retained upstream stderr. Log a scrubbed bounded tail or only
  its presence and byte count.

Acceptance criteria:

- Tests cover each supported mcp-go content type and nested combination.
- Schema tests assert semantic shape rather than byte-for-byte raw passthrough.
- Secrets in descriptions, defaults, examples, metadata, and embedded text do
  not reach registered MCP tools or results.
- Race tests pass for concurrent proxy calls.

### CRED-6: Fail closed for opaque MCP content

Goal: prevent uninspectable image, audio, or binary content from bypassing the
credential-output boundary.

Required behavior:

- Suppress opaque content by default and replace it with an explanatory text
  result.
- Add an explicit per-MCP-server unsafe opt-in such as
  `allow_opaque_content: true` only after D4 is accepted.
- Include the setting in the relevant source/config fingerprint so reloads take
  effect.
- Document that opt-in content is not credential-scrubbed.

Acceptance criteria:

- Default behavior suppresses image, audio, blob, and unknown content types.
- Explicit opt-in preserves the original content.
- Reloading the setting updates behavior without restarting an unchanged child
  unnecessarily.

### CRED-7: Make HOME access explicit

Goal: stop implicitly exposing the host user's credential and configuration
directories to wrapped CLI and stdio MCP processes.

Required behavior after D5 is accepted:

- Add an explicit `inherit_home` configuration field for CLI tools and stdio MCP
  servers.
- Require either a configured non-empty `HOME` or `inherit_home: true`.
- Reject ambiguous configuration instead of silently choosing the host HOME.
- Use one deterministic environment map so `HOME`, `PATH`, and `LANG`
  precedence is unambiguous.
- Apply the same contract to normal execution, introspection, and stdio MCP
  children.
- Document curated alternatives such as `KUBECONFIG`, `CLOUDSDK_CONFIG`,
  `GH_CONFIG_DIR`, `AWS_SHARED_CREDENTIALS_FILE`, `DOCKER_CONFIG`, and
  `GNUPGHOME`.

Acceptance criteria:

- Validation rejects an implicit HOME.
- An explicit HOME wins exactly once in the child environment.
- `inherit_home: true` preserves intentional legacy behavior.
- Introspection and execution see the same HOME decision.
- Migration tests cover representative Git, gcloud, kubectl, GitHub CLI, AWS,
  Docker, GPG, and SSH-agent configurations using synthetic credentials.

## 7. Web UI workstream

### WEB-1: Fail-closed listener and credential boundary

Goal: make insecure admin-listener configuration impossible and report startup
failure synchronously.

Primary areas:

- `internal/config/validate.go`
- `internal/config/config_test.go`
- `internal/config/validate_test.go`
- `cmd/serve.go`
- `cmd/serve_test.go`

Required behavior after D1 is accepted:

- Parse `admin.listen` and require a literal IPv4 or IPv6 loopback address.
- Reject wildcard, unspecified, hostname, LAN, malformed, zero-port, and
  out-of-range-port values.
- Read and trim configured token files during validation.
- Reject empty admin or MCP tokens.
- Preserve same-file and hardlink checks.
- Reject equal token contents using constant-time comparison.
- Call `net.Listen` synchronously and return bind errors from startup.
- Serve the already-created listener in the background.
- Configure deliberate admin HTTP read-header, read, write, idle, and header
  limits. Do not copy a finite write timeout to long-lived MCP SSE responses.
- Log a clean admin URL without any token.

Recommended admin server limits:

| Setting | Value |
|---|---|
| ReadHeaderTimeout | 5 seconds |
| ReadTimeout | 15 seconds |
| WriteTimeout | 30 seconds |
| IdleTimeout | 60 seconds |
| MaxHeaderBytes | 16 KiB |

Acceptance criteria:

- IPv4 and IPv6 loopback are accepted.
- Every other bind class above fails validation.
- Distinct files with equal tokens fail without logging either token.
- An occupied port causes startup to fail synchronously.
- Startup output and logs contain no admin token.

### WEB-2: Opaque browser sessions and query-token removal

Goal: stop placing the long-lived admin token in cookies, URLs, history, and
routine browser requests.

Primary area: `internal/webui` plus Web UI startup wiring in `cmd/serve.go`.

Required behavior after D2 is accepted:

- Replace separate route and auth wrapping calls with one production handler
  constructor that always installs the complete security chain.
- Generate cryptographically random opaque session IDs.
- Store sessions server-side under a mutex.
- Track creation, last use, idle expiry, and absolute expiry.
- Use a 30-minute idle limit and eight-hour absolute limit unless a later design
  decision changes them.
- Restart invalidates all sessions.
- Store only the opaque ID in the browser cookie.
- Set `HttpOnly`, `Path=/`, `SameSite=Strict`, explicit expiry, and
  `Secure` when the request is TLS.
- Remove query-token authentication.
- If a GET contains `token`, redirect to a safe local page with the parameter
  removed but do not authenticate it.
- Restrict token-form login to `POST /login`.
- Preserve `Authorization: Bearer <admin-token>` for scripts.
- If an Authorization header is present and invalid, fail instead of falling
  back to a cookie.
- Harden post-login redirects to a small allowlist of local Web UI routes.

Acceptance criteria:

- The session cookie never equals or contains the admin token.
- A raw-token legacy cookie fails.
- Query tokens neither authenticate nor set cookies.
- Session idle and absolute expiry use an injected test clock, not sleeps.
- Backslash, encoded backslash, absolute, network-path, control-character, and
  unexpected redirect targets fall back safely.
- Concurrent session tests pass with the race detector.

### WEB-3: CSRF, Host, Origin, and request-body enforcement

Goal: distinguish an intentional same-origin operator action from ambient
browser credentials sent by another site or localhost port.

Required behavior:

- For every request, require `Host` to match the configured loopback IP and port.
- Do not trust forwarded host headers.
- For login and cookie-authenticated mutations, require exact Origin match.
- If supporting a missing Origin for older browsers, require an exact same-origin
  Referer; otherwise reject.
- If `Sec-Fetch-Site` is present, require `same-origin`; reject `same-site` and
  `cross-site`.
- Generate a separate random CSRF value per browser session.
- Put it in every state-changing form and compare it in constant time.
- Read mutation values from `PostForm`, never from the query string.
- Keep valid Bearer requests exempt from form CSRF because Bearer auth is
  explicit and non-ambient. Host checks always apply.
- Cap POST bodies at one MiB with `http.MaxBytesReader`.
- Parse forms once in middleware and return `400`, `413`, or `415` as
  appropriate before route execution.

State-changing routes include request approval/denial, tool CRUD, metadata
updates, rule CRUD, constraint changes, and argument-constraint changes.

Acceptance criteria:

- Wrong Host, Origin, port, fetch-site, CSRF value, media type, or body size
  fails before mutation.
- Missing, query-only, and another-session CSRF values fail.
- Failed requests leave both config bytes and request status unchanged.
- Valid browser session plus CSRF succeeds.
- Valid Bearer automation succeeds without CSRF or browser-origin headers.

### WEB-4: Security headers and strict CSP

Goal: reduce clickjacking, content sniffing, token referrer leakage, caching, and
script-injection impact.

Required headers on every response:

```text
Content-Security-Policy: default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'; object-src 'none'
X-Frame-Options: DENY
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
Cache-Control: no-store
Cross-Origin-Opener-Policy: same-origin
```

Required behavior:

- Remove inline JavaScript and inline event attributes from templates.
- Preserve destructive-action confirmation through an embedded external script
  and data attributes.
- Apply headers to login, authenticated pages, static resources, redirects,
  errors, and not-found responses.
- Do not add HSTS to the built-in plain-HTTP loopback listener.

Acceptance criteria:

- Tests assert every header class above on representative response types.
- Templates contain no inline script or `onsubmit` attributes.
- The strict CSP does not require `unsafe-inline`.

## 8. Unix transport workstream

### UNIX-1: Bind requests to a trusted Unix peer identity

Goal: remove unstable PID-based identity and enforce the owner-only Unix socket
contract at the protocol boundary.

Primary areas:

- `cmd/peer_linux.go`
- `cmd/peer_stub.go`
- `cmd/serve.go`
- `internal/paths/context.go`

Required behavior:

- Remove peer PID context and `/proc/<pid>/cwd` lookup entirely.
- On Linux, read `SO_PEERCRED` at connection establishment and compare its UID
  with `os.Geteuid()`.
- Reject mismatched UIDs before Bearer authentication and routing.
- Fail closed on Linux if peer credentials cannot be extracted.
- On platforms without a kernel-backed same-user implementation, require Bearer
  authentication for Unix transport startup.
- Keep explicit `?cwd=` only as transitional behavior until UNIX-2 replaces it
  with project grants on every transport.

Compatibility effects:

- Clients that relied on implicit peer CWD fall back to configured tool working
  directory or server CWD unless they supply `?cwd=`.
- Rootful Docker clients will normally appear as host UID 0 and be rejected when
  wraptool runs as a non-root user. Documentation must recommend rootless/user
  namespace mapping, matching `--user`, or authenticated TCP.
- A bridge such as `socat` is identified as the bridge process and must run as
  the server UID.

Acceptance criteria:

- No `/proc/<pid>/cwd` read remains.
- Same-EUID Linux peers reach both `/sse` and `/mcp`.
- Mismatched and unknown peers fail before handlers.
- TCP transport behavior is unchanged.
- Darwin and Windows release targets still compile.

### UNIX-2: Bind project clients to project grants

Goal: prevent a project client from selecting host paths at all. Authentication
selects immutable server-side project context registered by host-side
`wraptool up` before container startup or by host-side `wraptool init` before
writing an IDE/project client configuration.

Primary areas:

- `internal/paths/context.go`
- `internal/config`
- `cmd/serve.go`
- `cmd/up.go`
- `cmd/init.go`

Required behavior after D3 is accepted:

- Add a server-issued project grant containing a high-entropy bearer secret and
  canonical worktree root. Do not reuse client-asserted MCP session IDs.
- Apply grant authentication consistently to TCP and Unix transports.
- Register the grant through a host-only control path before container startup.
- Make `wraptool init` register the current canonical worktree and write its
  project grant instead of the server-wide credential and `?cwd=`.
- Give the container only its project bearer secret; do not generate `?cwd=`.
- Resolve CLI CWD from the authenticated grant and reject client-supplied CWD.
- Use `allowed_cwd_roots` to authorize grant issuance, not to let one project
  token select every directory below a server-wide root.
- Keep operator-configured `tools.<name>.working_dir` trusted and separate from
  project grants.
- Validate configured roots for absoluteness, existence, and directory type.
- Preserve exact-root and multiple-root support for grant issuance.
- Continue to require restart for root changes unless listener settings become
  safely hot-reloadable in a separate design.
- Make `wraptool up` fail before container startup, and `wraptool init` fail
  before writing configuration, when its selected worktree is not covered by a
  configured root.
- Do not silently edit a global policy to add a root.
- Expand a leading `~/` in configured roots with the existing config
  `expandPath` helper, matching the other path-valued config fields. Do not add
  environment-variable expansion; `$HOME/src` stays relative and fails the
  absoluteness check with a clear error.
- When `wraptool up` or `wraptool init` fails because the selected worktree is
  uncovered, print the exact `mcp.allowed_cwd_roots` YAML to add. When stdin and
  stdout are a TTY, additionally offer an explicit yes/no prompt to append a
  covering root through the `config.editConfig` machinery (comment-preserving,
  round-trip
  validated, atomic write). Accepting is explicit operator consent, not a
  silent edit; declining, or a non-interactive run, leaves the config
  untouched and exits with the actionable error.
- If a config scaffolding path exists or is added, it writes default roots
  `~/src` and `~/Projects`, filtered at generation time to directories that
  exist on the machine. Runtime validation stays strict; nonexistent
  configured roots are an error, never warn-and-skip.
- Define grant expiry, revocation, server-restart behavior, and a non-Linux
  control-plane fallback before implementation. See `project-isolation.md`.

Acceptance criteria:

- A project token always resolves to its registered canonical worktree.
- Changing or adding `?cwd=` cannot change execution directory.
- One project's token cannot address another registered worktree.
- `wraptool init` and `wraptool up` produce grants with identical CWD binding.
- Empty roots reject grant issuance; relative, nonexistent, outside, and
  prefix-sibling worktrees fail.
- Exact-root, inside-root, and multiple-root grant issuance passes.
- Configured working-directory behavior remains unchanged.
- A `~/`-prefixed root resolves against the operator home; a `$HOME`-prefixed
  root fails validation as non-absolute.
- Non-interactive `wraptool up` or `wraptool init` with an uncovered worktree
  exits nonzero, prints the exact YAML to add, and does not modify the config.
- Accepting the interactive prompt appends the root, revalidates, and the same
  run proceeds; declining exits nonzero with the config file byte-identical.
- Scaffolded default roots exclude directories that do not exist at
  generation time.

### UNIX-3: Pin the authorized CWD through child startup

Goal: close the symlink and path-component TOCTOU between containment validation
and `exec.Cmd.Dir` pathname resolution.

This slice requires a short design/PoC review before production implementation.

Proposed Unix design:

1. Open the requested directory as a descriptor before authorization.
2. Open each allowed root as a descriptor.
3. Verify descriptor ancestry using device and inode identity.
4. Retain the authorized directory descriptor until child startup.
5. Start the current wraptool executable through `exec.CommandContext` as an
   internal trampoline.
6. Pass the CWD descriptor and a setup-status pipe through `ExtraFiles`.
7. The trampoline calls `fchdir`, closes the descriptor, and calls `unix.Exec`
   on the real tool.
8. Mark the setup-status pipe close-on-exec so successful target exec produces
   EOF; setup failure writes a structured error.

This keeps timeout cancellation, signals, exit status, stdout, stderr, explicit
argv, and the no-shell invariant. Do not use process-global `chdir`, a second
pathname check, or undocumented `/proc/self/fd` ordering as the security
boundary.

Acceptance criteria:

- A deterministic test opens an inside directory, replaces its symlink path
  with an outside target, and proves the child still runs in the pinned inside
  directory.
- A symlink initially resolving outside is rejected.
- A symlink resolving inside remains supported.
- The target must be a directory.
- Target exit codes 125 and 127 are not confused with trampoline failure.
- `fchdir` and target-exec failures are reported as setup errors.
- Context cancellation kills the final target.
- CWD/status descriptors do not leak into the final tool.
- Concurrent calls never alter wraptool's process CWD.
- All release targets compile; platform limitations are documented explicitly.

### UNIX-4: Harden Unix socket creation and cleanup

Goal: avoid unlinking active, foreign, or replacement socket paths.

Required behavior:

- Inspect existing paths with `Lstat`.
- Refuse to remove non-socket objects.
- Refuse to remove sockets not owned by the server effective UID.
- Probe an existing socket and refuse startup if it accepts a connection.
- Remove only a confirmed stale, owned socket.
- Apply mode `0600` before serving HTTP requests.
- Record the bound socket identity and remove it on cleanup only if the pathname
  still identifies that socket.
- Coordinate shutdown and cleanup instead of relying on competing signal
  goroutines or unconditional pathname removal.
- Recommend a `0700` socket parent directory.

Acceptance criteria:

- A second server does not unlink an active listener.
- A pre-existing regular file is never removed.
- A stale owned socket is recoverable.
- Cleanup does not remove a replacement socket.
- Socket mode is `0600` before handlers can run.
- Same-EUID middleware rejects a connection queued during any permissive
  bind-to-chmod interval.

## 9. Sequencing and agent allocation

### Wave 1: independent boundary fixes

- Agent A: CRED-1.
- Agent B: CRED-2.
- Agent C: WEB-1 after D1 is accepted.
- Agent D: UNIX-1.

Use isolated branches or worktrees. WEB-1 and UNIX-1 both touch `cmd/serve.go`;
merge WEB-1 first, then rebase UNIX-1 before final review.

### Wave 2: core data-path fixes

- Credential agent: CRED-3, then CRED-4.
- Web agent: WEB-2, then WEB-3.
- Unix agent: UNIX-2 after D3 is accepted.

WEB-2 and WEB-3 should be sequential and preferably owned by the same agent
because they heavily overlap `internal/webui/webui.go` and templates.

### Wave 3: compatibility-sensitive boundaries

- MCP agent: CRED-5, then CRED-6 after D4 is accepted.
- Environment agent: CRED-7 after D5 is accepted.
- Web agent: WEB-4.
- Unix investigator: validate the UNIX-3 trampoline with a focused PoC.
- Unix builder: implement UNIX-3 after design review.
- Unix lifecycle agent: UNIX-4.

CRED-3 should merge before UNIX-3 because both affect executor startup and
output handling. CRED-5 should merge before CRED-6 so opaque-content policy is
added to a result walker that already handles every inspectable content type.

## 10. Verification

Every PR must run the focused package tests it changes, followed by:

```bash
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go vet ./...
golangci-lint run ./...
test -z "$(gofmt -l $(git ls-files '*.go'))"
git diff --check
```

Race-sensitive changes must also run:

```bash
GOTOOLCHAIN=local go test -race ./internal/webui ./internal/mcpproxy \
  ./internal/executor ./internal/paths ./cmd
```

Unix changes must cross-build the release matrix:

```bash
for target in \
  linux/amd64 linux/arm64 \
  darwin/amd64 darwin/arm64 \
  windows/amd64 windows/arm64
do
  goos=${target%/*}
  goarch=${target#*/}
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build ./...
done
```

Security-sensitive CWD and socket tests should also be stressed:

```bash
GOTOOLCHAIN=local go test ./internal/paths ./internal/executor ./cmd \
  -run 'Test(ResolveCWD|RunPinsCWD|RunInCWD|UnixPeer|UnixSocket|UnixListener|UnixCleanup)' \
  -count=100
```

Render documentation without committing generated `docs/_site`:

```bash
guix time-machine -C channels.scm -- shell -m manifest.scm -- quarto render docs
git status --short
```

## 11. Migration and release notes

The following changes require explicit release notes:

- Existing non-loopback, wildcard, and hostname admin binds stop working.
- Distinct admin and MCP token files with equal content become invalid.
- Query-token bookmarks stop authenticating.
- Existing raw-token browser cookies stop working and users must log in again.
- Admin sessions expire and restart invalidates them.
- Historical logs and browser history may contain an old admin token; recommend
  rotating it after the upgrade that removes tokenized URLs.
- Existing server-wide MCP credentials stop authorizing project calls. Existing
  generated project configs must be regenerated with a project grant by
  `wraptool up` or `wraptool init`.
- Grant issuance fails when the worktree is outside configured roots. The first
  failing interactive command offers a consent-based root addition;
  non-interactive failures print the exact YAML to add. Root `/` allows grants
  to be issued for any worktree; it does not restore client-selected CWD.
- Rootful Docker clients may fail the same-UID Unix peer check.
- Opaque MCP content is suppressed unless explicitly enabled.
- Tools and MCP servers must explicitly choose a HOME strategy.

Because several changes intentionally fail closed, they should be grouped in a
minor release with a dedicated migration section rather than appearing in an
unannounced patch release.

## 12. Documentation and audit closeout

Each PR updates the documentation it directly changes. Likely files include:

- `README.md`
- `CLAUDE.md`
- `docs/security.qmd`
- `docs/configuration.qmd`
- `docs/architecture.qmd`
- `docs/web-ui.qmd`
- `docs/mcp-integration.qmd`
- `docs/troubleshooting.qmd`
- `docs/isolated-environments.md`
- `docs/devcontainers.qmd`

Do not edit generated `docs/_site`.

After the final PR in each workstream merges, revalidate and update the status
tables in `design/kimi-audit.md` and `design/claude-audit.md`. Preserve their
historical evidence; add a dated remediation note rather than rewriting the
original finding.

## 13. Explicit non-goals

This plan does not cover:

- The writable Guix `manifest.scm` host-evaluation finding.
- General MCP TCP authentication, Host/Origin, or CORS policy.
- Request-log oversized-line resilience and rotation.
- Config edit serialization and lost-update prevention.
- WebAuthn implementation.
- Full filesystem sandboxing for wrapped tools.
- Proving that heuristic scrubbing recognizes every possible secret format.
- Remote or reverse-proxied Web UI support. That requires a separate explicit
  HTTPS and trusted-origin design.

These exclusions should remain separate issues or design documents so they do
not delay closure of the concrete findings addressed here.
