# Cutting wraptool 1.0

Status: **proposed**

Scope: the smallest set of work that must land before tagging `wraptool-v1.0.0`,
and — more importantly — an explicit record of everything deliberately left out.
Verified against `main` at `6af5d99`. Every claim below names the file and line
it rests on, so acting on this note is mechanical rather than a
re-investigation.

The premise, taken as given: the core has proven robust, this is a
solo-maintained project, and 1.0 ships soon. This note does not argue with that.
It argues only about *which* work is genuinely load-bearing for the version
number, and it errs hard toward "defer".

The default answer for any candidate is **defer to a minor release**. An item is
a blocker only if it satisfies one of four tests:

- **T1 — breaks a promise 1.0 makes.** 1.0 declares a stable contract. Anything
  that would need a breaking change soon after is fixed now, or has its contract
  narrowed now so 1.x need not break.
- **T2 — security defect or false security claim.** wraptool's entire value is a
  credential boundary. Documentation asserting a protection that is not
  implemented is worse than shipping later.
- **T3 — documented as working, does not work.** A user following the docs must
  not hit a wall.
- **T4 — blocks the release mechanics.** Build, tag, artifacts, install path.

Eight blockers survive. Roughly forty items are deferred. That ratio is the
point of this note.

---

## 1. What 1.0 means for wraptool

1.0 says one thing: **the surfaces listed as frozen below will not break under
any `1.x` release.** It does not say the software is finished, and it does not
promise that everything in the repository is stable — only the enumerated list.

This section should ship as a page in `docs/` (see blocker B1); the text here is
the substance, not the final prose.

### 1.1 What a version bump means

| Bump | Promise |
|------|---------|
| `1.0.x` → `1.0.y` | Bug fixes only. No new keys, flags, or tools. |
| `1.x` → `1.y` | New capability. Everything frozen below still works with no edit to your configuration, your generated harness config, or your `devcontainer.json`. |
| `1` → `2` | The frozen list may change. |

### 1.2 Frozen

- **The configuration schema** as documented in `docs/configuration.qmd`. A 1.x
  release may add keys and may add members to a value enum. It may not remove a
  key, rename one, or change a documented default such that an existing file
  behaves differently.
- **CLI command names and their documented flags**, per `docs/cli-reference.qmd`.
  New commands and new flags may appear. Documented ones keep their name,
  meaning, and default.
- **MCP tool naming.** A wrapped capability is `<tool>_<subcommand tokens joined
  by _>` (`internal/mcptools/schema.go:12-17`). The three fixed tools
  `wraptool_discover`, `wraptool_info` and `wraptool_request_capability` keep
  their names and their input schemas
  (`internal/mcptools/discover_tool.go:47-53`, `info_tool.go:25-30`,
  `request_tool.go:30-56`). These are wire identifiers an agent has learned; a
  rename is a break.
- **Generated harness configuration**: the file path and JSON shape written per
  harness (`internal/harness/harness.go:143-172`, renderers at `:230-353`).
- **The `wraptool:1` Dev Container feature**: its option set, its
  `containerEnv` (`HOME=/home/wraptool-harness`, appended pool `PATH`), and the
  two pool mount targets it depends on
  (`features/src/wraptool/devcontainer-feature.json`).
- **Release artifacts**: the tag format `wraptool-vX.Y.Z`, the six binary asset
  names, `checksums.txt`, `build-info.txt`, and the signing key
  `C8ABC91CAB2DC195B9763877DFABC43C55F2E63A`.
- **`wraptool doctor` exit codes** 0/1/2 and the `--json` schema version.

### 1.3 Explicitly excluded

Writing these down is the cheapest work in this whole note, and it is what buys
1.x its headroom. Narrowing a contract before 1.0 costs a paragraph; after 1.0
it costs a major version.

- **`design/` notes.** Working documents and audit findings, not a release
  ledger. `docs/releases.qmd:88-92` already says this; keep it.
- **Go packages.** Everything real is under `internal/` and unimportable. The
  only other package is `github.com/pti/wraptool/cmd`, which is not a supported
  API. The module path itself (`go.mod:1` says `github.com/pti/wraptool`, while
  the code lives only on `forge.snamellit.com`) may move.
- **`mcp.transport: stdio`.** wraptool's own stdio server transport exists for
  tests. It is unsupported and may be removed in any 1.x. See blocker B4 — today
  it is still advertised in `wraptool init --help`.
- **`mcp_servers.*` — experimental.** Filtering is name-only, `working_dir` is
  parsed and never applied (`internal/config/config.go:66`; no `cmd.Dir` in
  `internal/mcpproxy`), and there is no rule CRUD or capability-request flow for
  it — the source says so itself at `internal/config/config.go:76-88`. The shape
  may change in 1.x.
- **Harness pool cutover commands — experimental.** `harness sync`, `rollback`,
  `gc`, `policy set`, and the `--backend`/`--platform`/`--legacy` flags are
  mid-migration machinery (`cmd/harness.go:462-471`). `harness list`, `install`,
  `refresh` and `update` are the stable pair-plus-two. None of the experimental
  set is documented today, which is convenient: do not document them as stable.
- **The legacy harness pool `bin/` + `lib/` trees.** Already carry a shipped
  removal notice (`cmd/guixcontainer.go:517-520`). Not a supported surface.
- **The `wraptool:0` feature major and the `wraptool-connect` feature.**
  Deprecated and sunset respectively. Published artifacts keep working
  indefinitely; nothing new ships for either.
- **`serve`'s four override flags** (`--listen`, `--auth-token-file`,
  `--admin-listen`, `--admin-auth-token-file`, `cmd/serve.go:103-106`). An
  internal `up` → serve channel on an already-hidden command.
- **The review UI's HTML, URLs, and form fields.** Operator-facing, not an API.
- **Console output text**, including `server status`'s prose (`cmd/server.go:39-60`).
- **The scaffolded starter configuration.** `EnsureConfigFile` never overwrites,
  so its content is a starting point, not a contract.
- **Internal formats**: the introspection cache, pool generation IDs, receipts,
  and `recipe_revision` (`internal/pool/orchestrator.go:119`). All self-healing
  by design.

### 1.4 The one reserved exception

**A security fix may narrow what policy permits in a minor release.**

This sentence has to be in the promise, and it is worth more than any single
blocker below. wraptool's policy engine has four known permissive behaviours
that are honestly documented today and that a future release should tighten:
exact-length deny matching (`internal/whitelist/policy.go:125-135`), unbounded
positionals when `arg_constraints` is absent (`policy.go:104`), default-open
`?cwd=` containment (`internal/paths/context.go:73-75`), and unenforced
valuelessness on boolean flags (`internal/whitelist/constraints.go:27-31`).

Without the exception, fixing any of them in 1.x is a breaking change and they
all become 1.0 blockers. With it, they become a roadmap. Reserve it explicitly,
name the four behaviours as candidates, and the entire policy-hardening backlog
moves out of this release legitimately rather than by hand-waving.

---

## 2. Blockers

Eight. Ordered so that each one's output is available to the next; B8 must land
last because it documents what B3, B4, B5 and B7 changed.

### B1 — Publish the compatibility promise

**Test: T1.** 1.0 is a claim about stability and wraptool currently makes no such
claim anywhere. Searching `docs/` and `README.md` for "semver", "compatibility",
"breaking change" or "stability" returns nothing relevant. Without §1 written
down, every item in §3 is arguably a blocker, because nothing says it is not.

**Done looks like:** the substance of §1 above as a `docs/` page (a new
`compatibility.qmd`, or a section of `docs/releases.qmd` — the latter is
cheaper and it is already in the sidebar). It must contain the frozen list, the
excluded list, and §1.4's reserved exception. `mcp_servers` and the experimental
`harness` subcommands must be named as excluded, in writing, before 1.0.

**Size:** half a day. Pure prose. Blocks nothing technically, but it is what
converts the rest of this note from opinion into policy.

### B2 — Get the release pipeline green

**Test: T4, and T3.** `docs/releases.qmd:12-13` promises that a `wraptool-v*`
tag publishes precompiled binaries. Forgejo's task list shows a **failed
`publish` run on `f2017bec20`, the 0.7.0 tag commit** (task #279, `failure
publish 40m44s`), and the newest release carrying assets appears to be 0.6.0 —
so the Homebrew tap, `installation.qmd`'s "resolve the newest release" scripts,
and every documented install path currently deliver 0.6.0 while the repository
says 0.7.0.

The `lint` job is also unreliable: it failed or was cancelled on six of the last
seven `main` commits (#271, #273, #275, #277, #281, #283 — the last of these on
current `HEAD`). This is probably the runner rather than the code — `gofmt -l $(git
ls-files '*.go')`, `go vet ./...`, `go test ./...` and `golangci-lint run` are
all clean locally, with the only staticcheck hits coming from gitignored
`.claude/worktrees/` copies — but "probably" is not good enough to tag against.

**Done looks like:** one `wraptool-v*` tag whose Forgejo `Publish release
binaries` run completes, attaches all six binaries plus `checksums.txt` and
`build-info.txt`, and updates the tap; and a `lint` run that is green on `main`
because it passed, not because it was retried. If 0.7.0 is recoverable, the
documented remedy is a manual `workflow_dispatch` with the existing tag
(`docs/releases.qmd:66-69`) — running it also proves the retry path works before
1.0 needs it.

**Size:** unknown until the failure log is read; that is the first task. This is
the one blocker that could genuinely delay the date, which is why it is second
and not last.

### B3 — Authenticate the MCP listener by default; reject browser origins

**Test: T2 (security defect).** In the default configuration and on the flagship
`wraptool up --runtime=guix` path, the MCP listener is **unauthenticated and
reachable from any web page the developer visits**.

Three facts compose:

1. `authWrapped` returns the handler unwrapped when no token file is configured
   (`cmd/serve.go:324-328`), and `Validate` requires a token only for a
   *non-loopback* bind (`internal/config/validate.go:43-46`). Loopback + no
   token is unauthenticated, and the scaffold ships with
   `mcp.auth_token_file` commented out.
2. `adjustServerBindForRuntime` provisions a token only for the devcontainer
   runtime and returns early for guix (`cmd/up.go:215-217`). The guix path — the
   documented default when `guix` is on `PATH` — therefore starts a tokenless
   listener, which `--network` (`cmd/guixcontainer.go:620`) additionally exposes
   to every process in the untrusted container.
3. mcp-go sets `Access-Control-Allow-Origin: *` on the SSE stream unless CORS is
   explicitly configured (`sse.go:501-503` in mcp-go v0.54.0). Any page can open
   an `EventSource` to `http://127.0.0.1:8717/sse`, read the `endpoint` event
   carrying the session id, and then POST tool calls as a CORS *simple* request
   (`text/plain`, no preflight). `handleMessage` checks method and session id
   only — no `Origin`, no `Content-Type`.

The result is drive-by execution of every whitelisted host command from an
ordinary web page. `docs/security.qmd:206-207` discloses that a tokenless TCP
listener is unauthenticated, but a reader reasonably takes "loopback" to mean
"local processes only". The browser is a local process.

This also satisfies T1 independently: requiring a token unconditionally is a
breaking configuration change, and 1.0 is the only cheap moment to make it.

**Done looks like:**
- `serve` generates a token at the default path when the transport is `sse` and
  none is configured, mirroring what `up` already does for devcontainers, and
  prints where it went. Auto-provisioning rather than a hard validation error
  keeps first-run working.
- `adjustServerBindForRuntime` provisions on the guix path too.
- The auth wrapper rejects any request carrying a browser `Origin` header that
  is not allowlisted, and rejects an unexpected `Host`. Roughly thirty lines,
  and it is the part that survives a leaked token.
- `docs/security.qmd`'s "Known limitations" entry is rewritten to describe what
  is now true; the upgrade note says existing tokenless clients must re-run
  `wraptool init`.

**Size:** one to two days. The largest blocker.

### B4 — Remove `stdio` from every user-visible surface

**Test: T3, and T1.** `wraptool init --help` advertises a worked example — `#
Set up with stdio transport (wraptool started by the harness)` /
`wraptool init --harness claude --transport stdio` (`cmd/init.go:45-46`) — and
the flag help offers it (`cmd/init.go:53`). The command succeeds and writes a
stdio harness config. `wraptool up` then **rejects that configuration outright**
(`cmd/up.go:274`). A user who follows the CLI's own example hits a wall.

This is the exact failure CLAUDE.md records as already having shipped once, with
the standing instruction that continued leaks force removing the transport
entirely. Two error strings leak it too, reachable by anyone who typos
`transport:` — `internal/config/validate.go:31` and `cmd/serve.go:245`. `docs/`
and the scaffold are clean; the CLI is the only leak.

**Done looks like:** the example and the flag hint drop `stdio`; both error
strings read "must be sse or unix"; a test guards `wraptool init --help` the way
`cmd/doctor_test.go:200-201` already guards doctor. The transport stays in the
code for tests. §1.3 records it as unsupported so removing it in a 1.x is not a
break.

**Size:** an hour, including the test.

### B5 — Merge tool-name sanitization; reserve the `wraptool_` prefix

**Test: T1 and T3.** MCP tool names are wire identifiers that §1.2 freezes.
`ToolName` on `main` joins tokens with `_` and does nothing else
(`internal/mcptools/schema.go:12-17`); `mcpproxy` carries an upstream server's
own name verbatim into `<name_prefix>_<upstream_tool>`. An upstream that
namespaces its tools — the branch's commit message cites `sigil/lookup` — mints
`sigil_sigil/lookup`, which Gemini rejects, and Gemini rejects the *entire*
`GenerateContentRequest` over one bad entry. One upstream server therefore
disables every wraptool tool in an Antigravity session. `mcp_servers` is a
documented feature; that is T3.

`origin/fix/sanitize-mcp-tool-names` (`bb2b10d`) is written, tested, and
verified against a real config. Merging it after 1.0 renames tool names, which
§1.2 forbids. Merge it before the freeze or accept the names as they are
forever.

Second, smaller item under the same head: nothing reserves the `wraptool_`
prefix (`grep reserved internal/config/validate.go` is empty). A configured tool
keyed `wraptool` with an allow rule for `discover` mints `wraptool_discover`;
the fixed tool wins at boot by registration order (`cmd/serve.go:171-181`), but
`cmd/registrar.go:124`'s `DeleteTools` on the first hot-reload unregisters the
built-in permanently, since the fixed tools register only at startup. Rejecting
a `wraptool`-keyed tool in `validateTool` is a few lines and cannot be added
later without breaking a config that loads today.

**Size:** half a day, mostly review of an existing branch.

### B6 — Merge the JSON credential-scrubbing fix

**Test: T2 (security defect — a real credential leak).** `internal/executor/scrub.go:14`
uses a single-character separator class `\s*[=:]\s*` between key and value. JSON
writes `":"` — three characters, with the closing quote sitting between the key
text and the colon, which `\s*` cannot consume. **Every credential emitted as
JSON passed through unredacted, regardless of key name**: `glab api`, `gcloud
--format=json`, `kubectl -o json`. Compounding it, the only pattern that could
catch a bare `token`-named key (`scrub.go:22`) requires a value of 40+
characters, and GitLab's `runners_token` is 29.

`impl/scrub-json-credentials` is in review and its fixture is modelled on a real
`glab api` response — this leaked in practice, not in theory. Do not edit those
files; this note only records that the merge gates the release.

**Size:** already done; merge and tag.

### B7 — Stop writing the bearer token world-readable; stop clobbering harness configs

**Test: T2, and T3.** `internal/harness/harness.go:223` writes every generated
harness configuration with `os.WriteFile(path, content, 0644)`. That file
contains `Authorization: Bearer <token>` whenever a token is set, and it is
written into the project worktree. Compare `internal/config/scaffold.go:80-85`,
which correctly uses 0600 for the configuration. The token is capability-
equivalent to every tool on the listener — `docs/security.qmd:190` says so — and
a worktree file is one `git add -A` away from a public remote.

Same function, second defect: `if merged, merr := mergeJSON(existing, content);
merr == nil { content = merged }` (`harness.go:212-218`). When the merge fails,
the code falls through and writes the freshly rendered content. For `wraptool
init --global --harness claude` that silently overwrites the user's entire
`~/.claude.json` with `{"mcpServers":{"wraptool":…}}`. A documented command that
destroys unrelated user data is T3 with interest.

**Done looks like:** mode 0600; append the written path to `.git/info/exclude`
when the target is inside a git worktree; and refuse with a diagnostic naming
the file instead of clobbering on merge failure.

**Size:** half a day.

### B8 — Documentation-accuracy pass on every claim of protection

**Test: T2 (false security claims).** `docs/security.qmd` and
`docs/git-hardening.qmd` are unusually honest — both were audited claim by claim
and hold up, including the hardening page's "Interim protection, not a complete
profile" callout and its "what wraptool does and does not enforce" list
(`docs/git-hardening.qmd:422-428`, `:492-505`). **Shipping 1.0 with those
callouts is correct and should not change.** They are the standard.

The problem is that the pages a first-time reader actually sees do not meet that
standard, and neither does the shipped starter policy. Concretely:

| Location | Claim | Reality |
|---|---|---|
| `docs/security.qmd:121-122` | scrubbing removes `sk-…` / `pk_…` prefixed keys | No such pattern exists. `internal/executor/scrub.go:10-27` is eight fixed regexes; the only one matching `sk-` needs a literal `api_key=`/`token:` prefix. A bare `sk-ant-api03-…` is returned verbatim. |
| `README.md:19,24,65` | "secrets never enter the agent context" / "the container" | The MCP token and the harness pool's `home/` auth state both do. `docs/security.qmd:44-46` carries the caveat; the front page carries none. |
| `docs/comparison.qmd:27,30,52` | "never touches credentials in any form"; "output scrubbed of secrets", unqualified, twice | Same token exposure; scrubbing is heuristic everywhere else it is described. |
| `docs/web-ui.qmd:19` | the review UI "is a separate listener … the agent never reaches it" | `guix shell --container --network` (`cmd/guixcontainer.go:620`) shares the host netns, so `127.0.0.1:8718` **is** routable from the container. It is token-gated, not unreachable. The same page states it correctly at `:67-69`. |
| `docs/index.qmd:61` | "without exposing the admin boundary to the harness" | Same. |
| `docs/isolated-environments.md:86-108` | flagship example policy allows `checkout` and `switch` on the host | Exactly what `docs/git-hardening.qmd:88-127` names as the clearest re-opened code-execution path. The headline isolation example teaches the anti-pattern its own hardening page forbids. |
| `internal/config/default_config.yaml:85-87` | "with no `flags:` list, only -h/--help are accepted, so there is nothing for a caller to smuggle in" | `-h`/`--help` are seeded into every rule (`config.go:300-307`); a flag with no constraint entry is skipped (`constraints.go:27-31`); `buildArgs` emits `flag, value` whenever the value is non-empty (`executor.go:157-162`). A client sends `{"help":"somefile"}` and the argv gains a token past `positional_max`. `SanitizeFlagValues` blocks a leading `-`, so this is a bounded smuggle, not flag injection — but the comment claims it is impossible, and `docs/security.qmd:198-201` already concedes the opposite. |
| `docs/configuration.qmd:136-144` | deny rules "Take absolute precedence", illustrated with `deny: [auth]` | Deny matches at **exact** subcommand length (`internal/whitelist/policy.go:125-135`), so `deny: [auth]` does not cover `auth print-access-token`. `subcommandMatches`'s own doc comment at `:124` falsely says "prefix of (or equal to)". `wraptool doctor` already catches the gap (`policy.deny-prefix-gap`, `internal/doctor/policy_generic.go:302-371`) — the documentation just never says the gap exists. |

**Done looks like:** each row corrected. The scope is fixed and listed; this is
not an invitation to rewrite `docs/`. For the deny row, the minimal fix is
documentation plus the code comment, **not** changing the engine — §1.4's
reserved exception is what lets prefix-matching land in a 1.x instead. My
recommendation is to take the documentation fix now and schedule the engine
change for 1.1, because a deny that silently under-matches is a footgun worth
removing, just not on the critical path.

**Size:** one day. Must land after B3, B4, B5 and B7 so it describes the shipped
state.

---

## 3. Deferred to 1.x

Everything below was considered and cut. Each line says why deferring is safe
and roughly which release it suits. This list is longer than the blocker list on
purpose: it is the roadmap, and it is what makes 1.1 and 1.2 worth waiting for.

### 3.1 Policy hardening — covered by §1.4's reserved exception

All of these are real, all are already honestly documented in
`docs/security.qmd`'s "Known limitations", and all are permitted to land in a
minor release because §1.4 reserves the right to tighten policy for security.
That single sentence is what moves the whole group out of 1.0.

| Item | Evidence | Suits |
|---|---|---|
| Deny matches exact length, not prefix (F4/H1) | `internal/whitelist/policy.go:125-135`; doctor already warns | 1.1 |
| Positionals have no content constraint, only a count, and no count at all without `arg_constraints` (F3/H4) | `policy.go:104`; disclosed at `docs/security.qmd:194-196` | 1.1 |
| Boolean/valueless flags do not enforce valuelessness (claude #1, H2, H3) | `constraints.go:27-31`, `executor.go:157-162`, `handler.go:52-63`; disclosed at `docs/security.qmd:198-201` | 1.1 |
| Enable mcp-go's `WithInputSchemaValidation()` — available in v0.54.0, one line at `cmd/serve.go:158-162` | closes most of the row above generically | 1.1 |
| `?cwd=` containment is default-open (F5/M5) | `internal/paths/context.go:73-75`; warn-only at `cmd/serve.go:274-278`; disclosed at `docs/security.qmd:192-194` | 1.1, or 2.0 via `project-isolation.md` |
| `?cwd=` TOCTOU between `EvalSymlinks` and `cmd.Dir` | `context.go:76` vs `executor.go:117-123` | 1.2 |
| Upstream MCP child spawned even when every tool is denied (M1) | `internal/mcpproxy/source.go:65-73` before the filter at `:83` | 1.1 |
| Introspection has no `cmd.Dir`, no output bound, and is not scrubbed before caching (M2 remainder) | `internal/introspect/introspect.go:334-341`, `cache.go:213-223`. The env half is fixed (`introspect.go:288-291`). | 1.2 |
| Executor forwards host `HOME` while its comment asserts isolation (claude #7) | `internal/executor/executor.go:50-66` — fix the comment early even if the behaviour waits | 1.1 comment, 1.2 behaviour |
| `env_files` paths uncontained (claude #10) | `internal/config/config.go:363-387` — operator-controlled, not agent-reachable | 1.2 |
| `mcp_servers.*.command` validated only for non-emptiness | `internal/config/validate.go:120-124` vs `validateTool`'s absolute/regular-file checks at `:158-181`; doctor warns | 1.1 |

### 3.2 Scrubbing beyond the JSON fix

B6 closes the JSON separator defect. The rest of the scrubbing backlog stays
open, and is safe to defer precisely because `docs/security.qmd:202-203` already
frames scrubbing as heuristic defense in depth rather than a boundary — provided
B8 fixes the one line that overclaims.

- PEM bodies, `https://user:secret@host` userinfo, bare JWTs, non-GitLab vendor
  prefixes, base64-encoded env values (`internal/executor/scrub.go`). 1.1.
- Scrub runs *after* the 1 MiB truncation, so a secret straddling the cut is
  missed (`executor.go:125-138`). 1.2.
- Proxied MCP non-text content is forwarded unscrubbed and uncapped
  (`internal/mcpproxy/scrub.go:19-27`). Disclosed. 1.2.

### 3.3 Review UI and admin plane

The one-click escalation is already closed — `admin.auth_token_file` is
mandatory whenever `admin.listen` is set (`internal/config/validate.go:53-56`).
What remains is hardening against a hostile *local* web page, which is real but
narrower.

- CSRF: no token, no `Origin`/`Sec-Fetch-Site` check, `SameSite=Lax` not
  `Strict`, and mutations read `r.FormValue` (query *or* body) —
  `internal/webui/webui.go:515-530,568-589,685-691,832-838`. 1.1.
- `?token=` in the URL, which `up` prints and `serve` logs
  (`webui.go:822-851`, `cmd/serve.go:428`). 1.1.
- Session cookie is the raw token, no `Secure`, no expiry (`webui.go:832-838`). 1.1.
- Restrict `admin.listen` to literal loopback (D1 in
  `design/credential-webui-unix-hardening.md:54`). Cheap, but it is a config
  break — schedule it deliberately, and note it interacts with WebAuthn's RP-ID
  choice (`design/webui/webauthn-admin-auth.md`). 1.1 or 2.0.
- Security headers; no HTTP timeouts or body caps on either listener
  (`cmd/serve.go:349-352`, `:427`). 1.1.
- WebAuthn admin authentication (`design/webui/webauthn-admin-auth.md`).
  Admin-plane only, touches no MCP contract. 1.2+.
- Web UI flag-constraint authoring (`design/webui/webui-management-completion.md`).
  Pure addition over data that already flows. 1.1.
- Accessibility and `wraptool admin url|open|tui`
  (`design/webui/webui-accessibility-reachability.md`). Additive. 1.2.

### 3.4 Container and runtime isolation

Guix-runtime-specific, already surfaced by `wraptool doctor` warnings, and
already scoped in `docs/security.qmd:48-55` as convenience rather than a
boundary.

- `--network` is unconditional, sharing the host netns (`cmd/guixcontainer.go:620`). 1.2.
- A project-writable `manifest.scm` is evaluated host-side by `guix shell -m`
  (`cmd/guixcontainer.go:72,104,629`) with no digest or approval step. This is
  the sharpest of the four; it suits 1.1 with an approval prompt.
- The pool `home/` is shared read-write across projects, mode 0755
  (`cmd/guixcontainer.go:283-294,626`). Disclosed; doctor warns. 1.2.
- `up`'s sensitive-path check never runs against the pool home it mounts
  (`cmd/up.go:132-137`, `:510-528`). 1.1.
- A container can write the worktree's `.mcp.json` (`cmd/guixcontainer.go:83-93`). 1.2.
- microVM runtime investigation (`design/harness/microvm-sandbox-runtime.md`).
  Adds at most a `--runtime` enum member; the note's own likely outcome is
  "document, do not implement". 1.3+.

### 3.5 The harness pool Phase 4 cutover — the cleanest deferral in the repo

`design/harness/unified-harness-pool.md` §16 lists five Phase 4 items. Four are
already done and the fifth was formally superseded by §12.2's own ruling. The
wiring-only `wraptool:1` feature ships, `init devcontainer` emits the final
two-mount shape, the examples and this repository are migrated, `:0` is
deprecated, and `gc --legacy` plus the legacy-removed marker exist
(`cmd/harness.go:1634-1652`, `internal/pool/gc.go:212-257`).

What genuinely remains is one thing: `harness install` realizes and activates a
generation and *then still* shells out to npm or agy's `curl | bash` to populate
legacy `bin/` + `lib/` (`cmd/harness.go:1019-1043`, `:1137`, `:1182`), and the
guix runtime still exposes that tree alongside `runtime/`
(`cmd/guixcontainer.go:495-506`).

**Deferring costs nothing at 1.0.** Every public surface Phase 4 touches is
already at its post-cutover shape: the feature major, the scaffolded mounts, the
pool layout, and both `schema_version` constants (`internal/pool/lock.go:13-26`,
`manifest.go:15-25`). Phase 4 *deletes* an optional fallback; it adds no field,
renames no path, and bumps no schema. A user who sets up under 1.0 does nothing
when it lands. `recipeFormatVersion` (`internal/pool/orchestrator.go:119`) is
the designed escape hatch and has already absorbed two bumps in production locks
with no user action.

The mixed state *is* user-visible — a stderr deprecation notice
(`cmd/guixcontainer.go:517-520`), a stdout status line (`:204-205`), and up to
three devcontainer mount notices (`cmd/up.go:805-839`) — but visible is not
broken. Keep the deprecation notice; §1.3 records the legacy tree as
unsupported, which is what makes the eventual removal non-breaking by policy as
well as by mechanism. **1.1 or 1.2.**

One cost worth naming: a devcontainer-only user pays for a legacy npm install
they will never mount, since `bin/` is never bind-mounted into a devcontainer.
Wasted time, one more failure surface, already downgraded to a warning
(`cmd/harness.go:1058`). Not worth blocking on.

### 3.6 Unbuilt designs

Almost none of these are 1.0 material, which was the expected answer. Two need a
hedge, and §1.3 is that hedge.

| Note | Status | Verdict |
|---|---|---|
| `design/project-isolation.md` | proposal, decisions open | **Needs the §1.3 hedge.** It deletes `?cwd=` outright (`:50`) and retires the server-wide token (`:52-57`), invalidating every generated `.mcp.json` across all five shapes `connect.sh` writes. Nine decisions are still open (`:137-148`). Doing it later is a 2.0. Do not freeze `?cwd=` or the shared token as stable in B1's list. |
| `design/credential-webui-unix-hardening.md` | planned, nothing started | **Needs the §1.3 hedge.** Its §11 (`:751-775`) is literally a list of contract breaks — D5 makes `HOME` inheritance explicit and invalidates every existing `tools.*` entry; D1 narrows `admin.listen`; D2 removes `?token=`; UNIX-1 rejects rootful Docker clients that work today. The individual CRED/WEB/UNIX hardening items defer freely; the *decisions* D1–D5 should be taken before they compound. |
| `design/git-execution-hardening.md` | proposal, research complete | Safe. §7.1 already shipped (the scaffold's `GIT_CONFIG_COUNT` command-scope block). §8's restrictive profile is scoped to a new opt-in `tools.<n>.hardening` key, never inferred (`:286-290`). 1.2. |
| `design/free-text-sanitization-tier.md` | proposal | Safe — pure addition. Adds one member to the `flag_constraints.type` enum; every existing type maps to today's behaviour byte for byte (`:80-82`). 1.1. |
| `design/policy/opt-in-documentation-tooling.md` | proposal | Safe. Explicitly refuses to add an option to `wraptool:1` (`:148-151`); ships as an independent feature. 1.2. |
| `design/documentation-onboarding.md` | proposed | Safe, and mostly overtaken. **Sequencing trap:** it documents six currently-undocumented `harness` subcommands. Documenting a surface freezes it — do not land that half until the experimental exclusion in §1.3 is published, or rename anything you intend to rename first. 1.1. |
| `design/mcp-server-wrapping.md` Phase 3 | draft (stale — Phases 0–2 landed) | Safe if the capability request gains a new *optional* `server` field. 1.2. |
| `design/harness/microvm-sandbox-runtime.md` | proposed | See §3.4. 1.3+. |

### 3.7 Configuration keys that are shipped but inert

All three are already documented as inert, so implementing them later is purely
additive and none is a blocker. Listing them here is what stops a future
maintainer from treating them as bugs.

- `mcp.log_level` — declared (`internal/config/config.go:31`), defaulted
  (`:266-268`), read nowhere, and surfaced in the first-run scaffold with a
  documented enum. `docs/configuration.qmd:56` admits it does nothing.
  Implement in 1.1; do not remove it, since §1.2 froze it.
- `mcp_servers.<name>.working_dir` — parsed, never applied to `cmd.Dir`.
  `docs/configuration.qmd:258` acknowledges the no-op. 1.1.
- `flag_constraints.<flag>` naming a flag absent from `flags` is silently inert;
  `validateTool` never cross-checks (`internal/config/validate.go:185-194`).
  Doctor catches it. Promote to a validation error in 1.1.

### 3.8 Documentation debt not covered by B8

B8 is scoped to claims of protection. Everything else in `docs/` is deferred,
and most of it should land as a 1.0.1 documentation-only release — which is a
perfectly good use of a patch version and gets 1.0 out the door sooner.

- **`docs/doctor.qmd` describes 0.6.0.** `:130-134` says `drift.*` is "planned
  but not yet implemented" and "`--only drift` selects nothing"; `:136-139` and
  `:219` say `--no-probe` "has no observable effect" and "no check executes a
  configured tool binary". All four are false: three drift checks are registered
  (`internal/doctor/drift.go:93-97`) and genuinely exec the tool
  (`drift.go:130`, `:455`). The error is in the safe direction — the doc
  understates — so it is not a wall, but it is the most embarrassing single
  page. **1.0.1, first item.**
- **`docs/web-ui.qmd` describes a previous UI.** `:117` claims a
  last-allow-rule refusal that does not exist (`internal/config/validate.go:181-187`
  explicitly permits an inert tool); `:103,108-114,126-128` describe an `ADV`
  badge and non-editable constraints, but `adv` appears nowhere in
  `internal/webui` and `/constraint/set`, `/constraint/delete` and `/args/set`
  all exist (`webui.go:100-102`); the screenshot is stale; `:36` misses that
  `up` auto-enables the UI (`cmd/up.go:245-251`). 1.0.1.
- **`docs/cli-reference.qmd` gaps.** It documents the hidden `serve` alias as
  canonical (`:15,33-36`) and never mentions `wraptool server start`; `harness`
  covers only `list`/`install` with no flags; `:105` claims npm harnesses
  auto-add the Node feature, which was deliberately removed
  (`cmd/init_devcontainer.go:46-54,128-141`). 1.0.1.
- **`docs/mcp-integration.qmd:14` and `docs/security.qmd:206-207` disagree with
  the code in opposite directions** about whether network auth is mandatory in
  validation. B3 changes the answer anyway — rewrite both after it lands. 1.0.1.
- `docs/hot-reload.qmd:134` says auto-added flags carry no `flag_constraints`;
  `ApplyRequest` writes them whenever the request carried `flag_types`
  (`internal/config/edit.go:102,180-196`). `:70-72` omits `flag_types` from the
  request-field list. 1.0.1.
- `docs/getting-started.qmd:31` and `docs/configuration.qmd:41-42` say the
  scaffold allows "exactly one thing, `git pull`"; it allows `pull` and `fetch`,
  as the same page correctly says at `:63`. 1.0.1.
- `docs/getting-started.qmd:219-220` puts the guix pool home at
  `/home/wraptool-harness`; that is the devcontainer target only
  (`cmd/guixcontainer.go:609,626`). 1.0.1.
- `docs/installation.qmd:266` says harness installation needs Guix and that the
  feature installs the harness; both halves are stale. 1.0.1.
- `docs/configuration.qmd:99` describes `working_dir` without saying client
  `?cwd=` outranks it — and every harness config wraptool generates sends
  `?cwd=` unconditionally, so under `up`/`init` the configured value is never
  consulted. `docs/architecture.qmd:65-69` states the precedence correctly. 1.0.1.
- `docs/releases.qmd:5-6` says wraptool "does not currently maintain a separate
  changelog file"; `CHANGELOG.md` is 334 tracked lines and the release body the
  workflow writes points readers at it
  (`.forgejo/workflows/publish-binaries.yml:203`). 1.0.1.
- `docs/devcontainers.qmd` is rendered but absent from both the navbar and the
  sidebar in `docs/_quarto.yml`, despite CLAUDE.md treating it as normative for
  the scaffolder. 1.0.1.
- Undocumented but real: duration keys read a bare integer as **nanoseconds**
  (`timeout: 30` is 30ns); and `transport: sse` written explicitly with no
  `listen` hard-fails, because the `listen` default is nested inside the
  transport-empty branch (`internal/config/config.go:258-265` vs
  `validate.go:35-37`). Both deserve a line in `docs/configuration.qmd`. 1.0.1.
- `docs/presentation.qmd` overstates scrubbing (`:199-200`), claims host `$HOME`
  is never mounted (`:113`), and presents the 1 MiB cap as blanket (`:202`). It
  is excluded from the render (`docs/_quarto.yml:7`), which is why it is
  deferred rather than in B8 — but it is tracked, and a deck is quotable. 1.0.1.
- `examples/devcontainer/` is internally inconsistent: `README.md:85-101`
  documents the `wraptool:1` feature and both pool mounts, while the sibling
  `devcontainer.json` is the old hand-rolled template with neither, puts
  `WRAPTOOL_TOKEN` in `containerEnv` against `docs/devcontainers.qmd:134-135`,
  and `setup-mcp.sh:8-9` writes a path wraptool never uses. 1.0.1.
- `cmd/validate.go:30-32`'s help still says "unlike the lenient load the server
  performs at runtime"; `cmd/serve.go:136` has been strict since the F1 fix.
  `cmd/harness.go:459` points at a nonexistent `design/unified-harness-pool.md`.
  1.0.1.
- `design/README.md`'s index omits four notes that exist on disk —
  `credential-webui-unix-hardening.md`, `git-execution-hardening.md`,
  `project-isolation.md`, `free-text-sanitization-tier.md` — two of which are
  the highest-blast-radius unbuilt designs in the repo. Several `Status:` lines
  are stale (`mcp-server-wrapping.md` still "draft" with Phases 0–2 landed;
  `flag-constraint-type-single-source.md` "proposal" though it shipped;
  `unified-harness-pool.md` "proposed" with four-fifths of Phase 4 in the tree;
  `claude-audit.md` has no `Status:` line at all). Internal hygiene; §1.3
  excludes `design/` from the promise. 1.0.1.

### 3.9 Robustness and ergonomics

- An oversized JSONL line wedges request-log reads (`internal/requestlog/store.go:283-296`)
  and the writer imposes no caps (`internal/mcptools/request_tool.go:58-121`).
  This one is agent-reachable and bricks the approve/deny workflow with a single
  oversized `rationale` — the strongest candidate to pull into 1.0.1.
- Config edits are unserialized: no mutex, no flock, no fsync of file or
  directory (`internal/config/edit.go:126-150` → `internal/yamledit/edit.go:135-156`).
  1.1.
- `editConfig`'s round-trip validation uses bare `yaml.Unmarshal` with no
  `KnownFields` (`internal/config/edit.go:140`) while the reload path is strict
  (`cmd/reload.go:76`). Largely theoretical, since the editor only sets known
  fields. 1.1.
- `dialOK` treats a bare TCP connect as "server running" (`cmd/up.go:386-393`). 1.1.
- `up --harness` accepts `cursor` and `roo` via `harness.Names()` (`cmd/up.go:75`),
  both annotated in the registry as host IDE integrations that are not
  pool-lockable (`internal/harness/harness.go:163,171`). Three commands offer
  three different `--harness` value sets. Reconcile in 1.1 — narrowing an
  accepted value is technically a break, so do it early.
- `server status` emits four free-text formats with no `--json`
  (`cmd/server.go:39-60`). Scripts will parse prose; give them a schema. 1.1.
- The fixed MCP tools' input and output shapes are documented nowhere in
  `docs/`, despite §1.2 freezing them. 1.0.1.
- Cache paths use an unsanitized tool name (`internal/paths/paths.go:56-57`);
  the argument sanitizer permits control characters (`internal/executor/sanitize.go:11`);
  401 and 403 diverge in a way that is technically observable
  (`cmd/serve.go:474,479`). 1.2.

---

## 4. Explicitly not planned

- **Removing the `stdio` server transport.** Tests need it. B4 removes it from
  every user-visible surface and §1.3 declares it unsupported, which is the
  whole benefit; deleting the code buys nothing and costs the test suite.
- **A compatibility shim for the legacy harness pool `bin/` + `lib/` trees.**
  `gc --legacy` plus the legacy-removed marker plus `harness install`'s
  `unmarkLegacyRemoved` already give a reversible, user-elected path
  (`cmd/guixcontainer.go:253-258,322`). A second migration mechanism is
  complexity without a user.
- **Republishing the `wraptool-connect` feature or the `wraptool:0` major.**
  Their published artifacts keep working. The tombstone at
  `features/src/wraptool-connect/README.md` is the correct amount of effort.
- **Publishing to homebrew-core, or a `go install` path.** The tap is generated
  from the checksums of the build that produced it, which is a stronger
  guarantee than core review offers here; and the module path does not resolve
  at `github.com/pti/wraptool` (`go.mod:1`). No documentation offers `go
  install`, so nothing is broken. Do not add one without moving the module.
- **Promising anything about `design/`.** They are narratives and audits. The
  callout at `docs/releases.qmd:88-92` is correct and should be repeated in the
  compatibility page.
- **Making `wraptool doctor --strict` a supported CI gate.** Its own help
  already disclaims it — a new check can ship as a warning in any release
  (`cmd/doctor.go:143-144`). Say so in `docs/doctor.qmd` and stop there.

---

## 5. Release checklist

Verified against the real CI configuration, not assumed. Steps 1–3 are the ones
that can actually fail.

**Before anything else**

1. Read the failure log for the `publish` run on the 0.7.0 tag commit
   (`f2017bec20`, Forgejo task #279). Decide whether 0.7.0 is recoverable by a
   `workflow_dispatch` retry with the existing tag
   (`.forgejo/workflows/publish-binaries.yml:11-16`, documented at
   `docs/releases.qmd:66-69`). **Do not move or recreate the tag** — the
   workflow re-fetches and re-verifies the annotated object precisely so a retry
   is safe. Running the retry also proves the recovery path before 1.0 needs it.
2. Get a `lint` run green on `main` on its merits. `gofmt -l $(git ls-files
   '*.go')`, `go vet ./...`, `golangci-lint run ./...` and `GOTOOLCHAIN=local go
   test ./...` are all clean locally, so if CI still fails the cause is the guix
   runner and must be fixed there, not worked around.
3. Land the eight blockers. B8 last.

**Repository hygiene the release script enforces**

4. `./release-version` refuses a dirty tree. `worktree/` is untracked and **not
   gitignored** (`.gitignore` covers `.claude/*` but not `worktree/`); commit
   the ignore rule — `origin/design/acp-bridge` already carries it as `012661b
   chore: ignore in-repo git worktrees` — or remove the directory.
5. Write the 1.0.0 CHANGELOG entry, and date the 0.7.0 one: `CHANGELOG.md:7`
   still reads `## 0.7.0 — unreleased` although `wraptool-v0.7.0` is tagged.
   Note the breaking changes from B3 (a token is now provisioned and required on
   `sse`) and B5 (upstream MCP tool names are sanitized) with their upgrade
   steps.
6. Confirm `main` exactly matches `origin/main` — the script fetches and
   compares (`release-version:63-66`).

**Cut**

7. Dry run: `./release-version --dry-run 1.0.0`. This validates the version,
   confirms the signing key, and runs `GOTOOLCHAIN=local go test ./...` without
   creating a tag.
8. Confirm the signing key is present on the host, not in a container:
   `gpg --list-secret-keys C8ABC91CAB2DC195B9763877DFABC43C55F2E63A`.
9. `./release-version 1.0.0`. It creates a signed annotated tag and pushes it;
   on a push failure it deletes the local tag so the release can be retried
   (`release-version:210-216`).

**After the tag**

10. Watch three tag-triggered runs, not one:
    - `.forgejo/workflows/publish-binaries.yml` — verifies the signature against
      the committed `release-signing-key.asc`, cross-compiles six targets,
      publishes the Forgejo release, generates `wraptool.rb`, and pushes the tap
      if `HOMEBREW_TAP_TOKEN` is set. A missing tap token is a logged skip, not
      a failure.
    - `.forgejo/workflows/publish-feature.yml` — republishes the Dev Container
      features. Note that `features/src/wraptool` is itself at `version 1.0.0`,
      coincidentally matching wraptool 1.0.0. They are independent; say so in
      the release notes so nobody infers a coupling.
    - GitLab's `release:binaries` / `release:publish` on the mirror
      (`.gitlab-ci.yml`). These use the pinned `CI_TOOLCHAIN_IMAGE` digest
      rather than `guix time-machine`, with `GOTOOLCHAIN: local` — so if the
      image's Go is older than `go.mod`'s `go 1.26.0` directive the job fails
      loudly. The pin is current with `manifest.scm` (both at `2026-07-24`), but
      confirm rather than assume.
11. Verify the install paths the documentation promises actually deliver 1.0.0:
    the `releases/latest` API resolution in `docs/installation.qmd:118-119`, the
    checksum verification at `:136-140`, and `brew upgrade
    pti/wraptool/wraptool` against the regenerated formula.
12. Publish the compatibility page (B1) with the release, not after it. A
    stability promise that arrives a week later is not a promise.

---

## 6. Where I am uncertain

Three places, with my recommendation rather than a hedge:

- **B2's size.** I could confirm that a `publish` job failed on the 0.7.0 tag
  commit, but not *why*, and not with certainty that no assets were attached.
  **Recommendation:** treat it as the first task, not the last. If it turns out
  to be a transient runner fault, the blocker evaporates in an hour; if the
  release pipeline is genuinely broken, everything else in this note is
  irrelevant until it is fixed.
- **Whether the deny-semantics fix belongs in 1.0.** The engine change is small
  and moves in the fail-closed direction, so it is tempting. **Recommendation:
  document only.** `wraptool doctor` already warns on the gap
  (`policy.deny-prefix-gap`), §1.4 reserves the right to tighten later, and the
  change would alter evaluation for every existing configuration — which is
  exactly the kind of thing that wants a release of its own rather than a slot
  in a release that is already carrying an auth change.
- **Whether `mcp_servers` should be excluded from the promise or removed from
  1.0 entirely.** It is enforced but has no rule CRUD, no capability-request
  flow, and a `working_dir` that does nothing. **Recommendation: exclude, do not
  remove.** It works for the case it was built for, `design/mcp-server-wrapping.md`
  has a coherent Phase 3, and removing a shipped feature to reach 1.0 is exactly
  the kind of perfectionism this note exists to prevent.

---

## 7. Addendum — re-verified 2026-09-02 against `8237c80`

Delta since the baseline. Everything above stands except as noted here.

- **B2, publish half, confirmed harder.** The releases API shows **no 0.7.0
  release exists at all** — newest is `wraptool-v0.6.0` with full assets. The
  tag `wraptool-v0.7.0` (`61c6d0a` → `f2017be`) is pushed. Every documented
  install path delivers 0.6.0. The failure log is still unread: the wrapped
  `fj actions` has no run-log subcommand, and the Forgejo run page is
  JS-rendered, so reading it needs the operator (or a `workflow_dispatch`
  retry, which `fj actions dispatch` *can* do from here — running it is both
  the documented remedy and the recovery-path proof).
- **B2, lint half, worse than stated.** Now **eight consecutive** failed or
  cancelled `lint` runs on `main`, including today's HEAD (`#291`, failure,
  6m34s; `#289` failed in 40s — mixed fast/slow failures suggest runner setup
  trouble, not one flaky test). Local gates remain clean.
- **New frozen-list candidate: the `/api/v1` admin JSON API** (merged since
  baseline, PRs #35/#36; `internal/webui/api.go`,
  `design/webui/admin-json-api.md`). The design doc already binds it: renames
  or semantic changes require `/api/v2`, additive fields are fine. B1's page
  should restate that one sentence and stop — the path version is the promise;
  no need to enumerate fields. It is operator-plane, so nothing else changes.
- **B8 unchanged in substance,** but `docs/git-hardening.qmd` gained a
  "policy boundary" section (`8237c80`) that names `checkout`/`switch` on the
  host as the anti-pattern explicitly — which makes the
  `docs/isolated-environments.md:86-108` row more urgent, and shifts that
  page's cited line numbers (`:422-428`, `:492-505` are now lower).
- **B5 and B6 branches still unmerged** (`origin/fix/sanitize-mcp-tool-names`
  at `bb2b10d`; `impl/scrub-json-credentials`, two commits). Checklist items 4
  and 5 still open: `worktree/` untracked and unignored; `CHANGELOG.md:7`
  still `0.7.0 — unreleased`.
- **B2 root causes found (2026-09-02, later the same day).** Both halves
  diagnosed:
  - *Lint:* the channel pin brought Go 1.27.0 while golangci-lint 2.12.2 was
    built with go1.26.2 — its embedded go/types cannot parse the 1.27 stdlib
    (`math/rand/v2` generics). Fixed by bumping `manifest.scm` to
    golangci-lint 2.13.2 (built with go1.27.0); verified locally, `0 issues`,
    and confirmed in CI: lint run #295 on `1dab209` passed — the first green
    `main` lint in nine runs.
  - *Publish (#279):* `publish-binaries.yml:70` runs `go test ./...`;
    `TestUpdateServeStateConfigLoadedAtRequiresOwnership` set
    `XDG_RUNTIME_DIR` with a bare `t.Setenv`, which adrg/xdg ignores, so the
    test hit the real runtime dir — `mkdir /run/user/980: permission denied`
    on the runner. The fix already exists, unmerged:
    `origin/fix/serve-state-test-isolates-runtime-dir` (`8fb54e8`), whose
    commit message names run 279 explicitly.
  - **Consequence: the 0.7.0 tag is unbuildable on the runner — the broken
    test is in the tagged tree, so a `workflow_dispatch` retry fails
    deterministically. Supersedes checklist step 1's retry advice. Cut
    `wraptool-v0.7.1` from `main` after merging `8fb54e8` and the manifest
    bump; that run is also B2's pipeline proof.** Update the 0.7.0 CHANGELOG
    heading to note it shipped no binaries.
