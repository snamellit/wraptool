# Design: accessibility and reachability of the operator review UI

Status: **proposed**. This note captures an accessibility audit of the
existing review UI (`internal/webui`) plus the operator-facing question of how
the UI is *reached* and *discovered*, and evaluates two additional surfaces
(auto-opening a browser, a TUI) against the project's constraints. It does
not change the review UI's security model — see `design/webauthn-admin-auth.md`
for the authentication boundary, which this note assumes stays as designed.

## 0. What we have today

Reachability and discoverability:

- The review UI is a separate loopback HTTP listener (`admin.listen`),
  distinct from the MCP listener — `cmd/serve.go:384` `startWebUI`. It is
  never AI-facing; it is the operator/host plane.
- The default config (`internal/config/default_config.yaml:26`) ships the
  entire `admin:` block **commented out**. The UI is only auto-enabled by
  `wraptool up` (`cmd/up.go:131` `enableAdminDefault`), opt-out with
  `--no-admin`. A bare `wraptool serve` does not start it unless the operator
  uncommented the block.
- The **only** surfacing of the UI is a single line printed after `up`:
  `review UI: http://127.0.0.1:8718/?token=…` (`cmd/up.go:138`,
  `cmd/serve.go:401`). The token is embedded in the URL so the first click
  sets the session cookie. After that line scrolls off the terminal there is
  no way to recover the URL without re-reading the token file.
- `wraptool server status` (`cmd/server.go:33`) probes the **MCP** listener
  only. It never reports the review UI's listener, the token file, or the URL.
  An operator running `status` to "is the server healthy?" gets no hint that the
  UI exists.
- There is no `wraptool admin …` subcommand. The only CLI path to acting on
  requests is `cmd/requests.go` (`approve --apply` etc.), which edits the
  config directly and never touches the review UI.
- Auth: token via `?token=` query, login form, or `Bearer` for scripts. The
  token file must live outside the workspace (`cmd/up.go:120`
  `validatePathOutsideWorkspace`) so a credential-bearing container cannot
  read it; default `~/.config/wraptool/admin-token`.

The site itself (`internal/webui/templates/*.html`):

- HTML5, `lang="en"`, viewport meta, doctype — basics are correct.
- **No `aria-*` anywhere** in the templates. One grep hit on "aria" is a
  code comment in `webui.go`, not markup.
- The deny `reason` text input (`list.html:47`) and the login `token` input
  (`login.html:16`) have only `placeholder=`, no `<label>` and no `aria-label`.
  Placeholder-as-label fails WCAG 1.3.1 and is invisible to many assistive
  technologies.
- The capability command line (`list.html:29`) is built from nested
  `<span>`s (`tool`, `flag`, …) with no `aria-label` or `role="text"`. A
  screen reader hears the words as a flat list, not as a single "command
  git pull --rebase".
- The approve form's per-flag `<select>`s (`list.html:42`) use implicit
  labelling by wrapping `<code>--flag</code>` + `<select>` inside
  `<label class="ft">`. This is valid HTML but fragile — it breaks the moment
  the `<code>` and `<select>` are no longer siblings, and some screen readers
  handle wrapped labelling inconsistently. An explicit
  `<label for=…>` + `id` is more robust.
- Flash messages (`<div class="flash err">`, `list.html:22`,
  `login.html:13`) are not `role="alert"` or `aria-live="polite"`. Errors
  on form resubmit are not announced; the operator must notice the visual
  change.
- No skip-link to `<main>`. Single `<main>` per page is fine as a landmark,
  but keyboard users tab through the entire header on every page load.
- No `:focus-visible` style (not confirmed in `style.css`, but no matches in
  grep). Default browser focus rings are increasingly removed by reset
  stylesheets; if the project's `style.css` resets `outline` without
  replacing it, keyboard users have no visible focus.
- Status badges (`list.html:30`) convey meaning by class **and** by text
  ("pending"/"approved"/"denied") — not color-only. Good.
- Approve/Deny are real `<button type="submit">` in separate forms
  (`list.html:44,48`). No JavaScript required to act. Good.
- No `<html lang>` overrides for code/identifier fragments.
- CSRF: the only auth is the session cookie. `SameSite` on the cookie is not
  verified here (would need to check `webui.go` `setCookie`). Same-origin
  loopback POSTs are low-risk, but a documented `SameSite=Lax` choice plus a
  token-backed CSRF field is the conventional fix and costs little. This is
  a security note, not strictly a11y, but it shares a fix window with the
  form-label work because both touch the same forms.

## 1. Stage A — accessibility fixes to the existing review UI (do first)

Smallest, highest-leverage, zero new dependencies. Helps every audience
(browser user, future TUI user migrating from the browser, anyone using a
screen reader or keyboard nav). Independently shippable.

Concrete changes, all in `internal/webui/templates/*.html` and `style.css`:

1. **Associate every input with a label.**
   - `login.html:16`: add `<label for="token" class="visually-hidden">Access token</label>` and `id="token"` on the input, or replace the
     placeholder with a visible label. Keep `autocomplete="current-password"`.
   - `list.html:47`: add `<label for="reason-{{.ID}}" class="visually-hidden">Deny reason</label>` + `id`. (Per-card ids are needed because the form
     repeats in the range loop.)
   - `list.html:42`: convert the wrapped `<label class="ft"><code>…</code><select>` to explicit `<label for="ftype-{{.ID}}-{{.Flag}}">` + `id` on
     the `<select>`. The `<code>` becomes a sibling, not a label child.
2. **Announce the capability command as one unit.**
   - `list.html:29`: wrap the `.cmd` span in `role="text"` (or give the span
     `aria-label="{{.Tool}} {{Subcommands}} {{Flags}}"`). Screen readers then
     treat the composed command as a single phrase.
3. **Live-region the flash messages.**
   - Add `role="alert"` to `.flash.err` and `aria-live="polite"` to
     `.flash.ok`/`.flash.info`. Errors are announced on resubmit.
4. **Visible keyboard focus.**
   - In `style.css`, ensure `:focus-visible` has a clearly visible outline on
     all interactive elements (links, buttons, inputs, selects). If a reset
     blanks `outline`, restore it on `:focus-visible` only (not `:focus`) so
     mouse users don't see a ring.
5. **Skip link.**
   - Add `<a href="#main" class="skip-link">Skip to content</a>` as the first
     focusable element in each template, and `id="main"` on `<main>`. CSS
     hides it until focused.
6. **Cookie hardening (alongside the forms touch).**
   - `webui.go` `setCookie`: set `SameSite=Lax`, `HttpOnly` (verify),
     `Secure` when the listener is TLS (today never, but cheap to gate). Add
     a per-session CSRF token in the cookie + a hidden form field, validated
     on POST. Loopback-only makes this low-urgency but it is the right time
     to do it because the templates are already being edited.

Tests: extend `webui_test.go` to assert the presence of `for=`/`id` pairs,
`role="alert"`, the skip link, and `aria-label` on `.cmd`. These are
cheap string-contains assertions and prevent regressions.

Effort: ~1 day. No new Go dependencies. No schema change. No impact on the
MCP or admin security boundary.

## 2. Stage B — reachability without surprise (`wraptool admin` + `--open`)

The single printed line is not discoverability; it is a log message that
scrolls away. Two additions, both opt-in or on-demand:

1. **`wraptool admin url`** — prints the UI URL with the token embedded, or
   `http://127.0.0.1:8718/` (login form) if the token file is not readable.
   Reads the loaded config's `admin.listen` + `admin.auth_token_file`, applies
   the same `0.0.0.0 → 127.0.0.1` rewrite as `webURL` (`serve.go:414`).
   Scriptable, `--help`-discoverable, no surprise. This is the building block
   everything else composes.
2. **`wraptool admin open`** — `admin url` plus `xdg-open`/`open`/`start`
   of that URL. Guarded: silent no-op when there is no `DISPLAY`/`WAYLAND_DISPLAY`
   (Linux), no `BROWSER`, stdin/stdout not a TTY, or the review UI's listener is
   not confirmed up. Never errors the parent command on failure to open —
   prints the URL regardless so the operator has a fallback.
3. **`wraptool up --open`** — opt-in flag, default off. Runs `admin open`
   after `ensureServer` succeeds and the review UI's listener is confirmed
   bound.
   Does **not** auto-open by default (see §3).
4. **`wraptool server status`** — extend to also probe and report the review
   UI listener, and print `review ui: <url>` when reachable. This closes the
   "I ran status and learned nothing about the UI" gap. No token printed by
   `status` (it is a health check, not an auth helper) — just whether the
   listener is up and on what address.

Implementation: a new `cmd/admin.go` with `url`/`open` subcommands sharing
one resolver. `enableAdminDefault` already gives us the listener; this is
pure read-side. ~150 lines plus tests.

Non-goals for this stage: no auto-open default (see §3), no tray/notification
integration, no desktop wrapper (see §4).

## 3. Should the browser auto-open by default? — No, opt-in only

The project's own proactiveness rule ("do not surprise the user") settles
this for the *default*, but the question deserves the reasoning because it
is the headline reachability lever.

**Pros of auto-opening**
- Closes the discoverability gap entirely: the operator no longer needs to
  read a log line or know where the token file is. The browser tab *is* the
  entry. This matters most for non-CLI-native operators, which is precisely
  who the review UI exists to serve versus `requests approve`.
- Matches `wraptool up`'s existing "bring you to your workspace" posture —
  it already prints `container runtime: guix` and drops you somewhere.
- Cheap: the token is already in the printed URL, so `xdg-open` of that
  exact URL is ~10 lines.

**Cons of auto-opening by default**
- `wraptool up` routinely runs **headless**: over SSH, in CI, inside a
  container whose only TTY is piped, or in a remote tmux session. Auto-open
  there either no-ops (no `DISPLAY`/`BROWSER`) or, worse, opens on a remote
  X server the operator cannot see, or spews errors into logs. Net: it must
  be guarded and silent on failure, which makes its behavior feel
  inconsistent ("sometimes a tab appears, sometimes not") — a worse
  experience than a predictable on-demand command.
- Surprises users who did not ask for a GUI window. The `--no-admin` opt-out
  already covers disabling the UI; an auto-open needs its own opt-out or,
  per §2, an opt-in (`--open`).
- Race: the listener must be confirmed up *before* opening, or the tab
  points at a dead URL. `startWebUI` runs in a goroutine; `admin open` must
  dial the review UI's listener before launching the browser, retrying briefly.
- Token hygiene: embedding the bearer token in a browser URL persists it
  into browser history and browser profile state. The printed-URL approach
  already does this; auto-open doubles down. Acceptable for loopback but
  worth noting, and an argument for `admin open` to prefer the login-form
  URL (no token in the address bar) and let the operator paste once.

**Decision:** ship `--open` and `admin open` as **opt-in**. Never auto-open.
This keeps `up` non-surprising for the headless/SSH/CI majority while
giving the at-the-keyboard operator a one-flag path. The `admin url`
subcommand is the scriptable fallback that makes "open it for me" a
shell function away (`BROWSER=… wraptool admin url | xargs -r open`).

## 4. Should we ship a desktop-style wrapper? — No

The UI is loopback HTTP behind a cookie. A "desktop app" wrapping it is a
webview (Tauri/Wails/Electron) pointed at `127.0.0.1:8718` — zero functional
gain, large dependency and attack surface, and a runtime (Chromium/WebKit)
that `manifest.scm` deliberately keeps out. The project's whole philosophy
(`manifest.scm` is the single source of truth, no dynamic runtimes, minimal
direct deps: mcp-go/cobra/yaml.v3/xdg/uuid) rejects this category.

The real problems (discoverability, token handling, missing focus styles)
are fixable in ~50 lines of HTML/Go (Stage A) plus the `admin url`/`open`
commands (Stage B), not by adding a GUI toolkit.

If a "double-clickable" entrypoint is wanted, the Guix-native answer is a
`.desktop` file that runs `wraptool up --open` — no new binary, no new
runtime, no new dependency.

One narrow exception worth considering: a **tray/status indicator** ("server
running, 3 pending requests") — but that is better as an OS-native notify
hook (`notify-send` on a `requestlog.Store` watch firing on new pending
requests) than a wrapped app. It composes with Stage B's `admin url`
naturally ("click the notification → `admin open`"). Out of scope for this
note; file separately if wanted.

## 5. Should we offer a TUI? — Yes, higher value than auto-open

This is the higher-value addition, and more than auto-open, because
wraptool's core audience is exactly the SSH/tmux operator.

**Why it fits**
- The audience that runs `wraptool up` over SSH and lives in tmux is the
  *same* audience that today must context-switch to a browser to click
  Approve. A TUI keeps them in the same pane — no token-in-URL, no browser
  round-trip, no leaving the session that has the failing agent in it.
- The data layer is already clean and presentation-agnostic:
  `requestlog.Store` (append-only JSONL, `ListRequests`/`ListToolCalls`)
  and `config.ApplyRequest`/`AddRule`/`UpdateRule`/`DeleteRule`. The review UI
  is a thin presenter over these. A TUI is a *second* presenter over the
  same store — no new business logic, no new auth boundary (it is a local
  process reading the same files the review UI serves), no HTTP. The a11y work
  in Stage A is largely orthogonal; the TUI is keyboard-first by
  construction.
- It sidesteps the browser's accessibility gaps entirely for its audience,
  and removes the auto-open debate for that audience.

**Cost / risk**
- A real new direct dependency (bubbletea or tview). Against the
  "minimal direct deps" ethos, but one static Go library is defensible
  alongside yaml.v3/xdg/uuid — it does not bring a runtime.
- Flag-type `<select>` editing and rule-CRUD forms need real form work in
  the TUI; bounded but non-trivial. Stage 1 of
  `design/webui-management-completion.md` (show proposed types) and Stage 2
  (operator overrides) have direct TUI analogues and should be designed
  together so the two presenters share a common "edit flag types" call
  rather than two bespoke implementations.
- Color status must not be color-only in the TUI either — the review UI's
  text+badge convention translates directly to text+symbol (`● pending`,
  `✓ approved`, `✗ denied`).

**Scope of the TUI**
- Subcommand `wraptool admin tui` (composes with `admin url`/`open`; the
  `admin` parent gets `url`/`open`/`tui`).
- Screens mirroring the review UI's three tabs: **Requests** (list + Approve
  with flag-type editing + Deny with reason), **Rules** (list + Add/Update/
  Delete over `config.AddRule`/`UpdateRule`/`DeleteRule`), **Calls** (audit
  log with allowed/denied filter).
- No new auth boundary: the TUI runs as the operator, reading the config
  and requestlog files the operator already owns. It does not speak HTTP,
  does not need the token. The review UI stays for the case where the operator
  is not the person at the terminal (the reason admin is separate from the
  AI-facing MCP in the first place).
- Hot-reload: the TUI watches the config and requestlog files directly
  (fsnotify, same debounce as the server's hot-reload) so an approval
  applied from the review UI appears in the TUI without restart, and vice
  versa.

**Non-goals for the TUI**
- Replacing the review UI. The two coexist; the TUI serves the
  always-in-terminal operator, the review UI serves the
  at-the-keyboard-but-not-in-tmux operator.
- Speaking the MCP protocol. The TUI is admin-plane only.
- Remote operation. The TUI is local-first; remote admin stays HTTP.

## 6. Suggested sequencing

1. **Stage A** — review UI a11y fixes (labels, `aria-label` on `.cmd`,
   `role="alert"` on flash, `:focus-visible`, skip-link, `SameSite`+CSRF).
   ~1 day, zero new deps, helps every audience.
2. **Stage B** — `wraptool admin url` / `admin open` / `wraptool up --open`
   (opt-in) / `server status` reports the review UI's listener. Reachability
   without surprise. ~150 lines + tests.
3. **Stage C** — `wraptool admin tui` over the existing Store and config
   editor APIs. Real value for the core SSH/tmux audience; the a11y fix in
   Stage A is the browser-side counterpart.
4. **Skip** the desktop wrapper. Add a `.desktop` file only if a
   double-clickable entrypoint is requested.

Stages A and B are independent and can land in either order. Stage C
depends on Stage A's flag-type-editing decisions being made once (so the
TUI and review UI share the editing call) but not on Stage A's markup work.
