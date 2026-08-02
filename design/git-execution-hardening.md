# Design: harden host-side Git execution

Status: **proposal - research complete, decisions open**
Created: 2026-07-27

## 1. Purpose

Wraptool runs allowed Git commands as the developer's host user. Git normally
trusts repository-local configuration, hooks, attributes, and administrative
state owned by that user. A malicious project can therefore turn an otherwise
allowed Git operation into host-side command execution.

This design defines a low-friction boundary:

- repository-sensitive Git operations run inside the credential-free project
  container;
- host-side wrapped Git is limited to operations that need host credentials;
- mandatory Git protections are applied below the AI-visible policy;
- exceptional hooks, helpers, and signing programs come only from explicit
  operator-managed read-only paths.

This is a prospective design. Current behavior and interim configuration are
called out separately.

## 2. Upstream security model

Git documents that configuration options and hooks can execute arbitrary shell
commands. A repository obtained by normal network clone does not receive the
source repository's `.git/config` or hooks, but a `.git` directory from an
archive, local shared tree, writable workspace, or another untrusted source is
not safe to use directly.

`safe.directory` is not sufficient. It rejects repositories owned by another
user, but a malicious repository extracted or modified under the developer's
own UID passes that check. Most execution-capable options are honored from
local and worktree config.

Git config precedence is system, global, local, worktree, then command scope.
A curated global config therefore does **not** prevent a repository-local value
from overriding `core.hooksPath`, `core.fsmonitor`, or similar settings.
Mandatory overrides must use command scope (`GIT_CONFIG_COUNT` or fixed `-c`)
or a stronger process boundary. Explicit command-line `-c` takes precedence
over `GIT_CONFIG_COUNT`.

No finite config-key blacklist provides complete protection. Tracked
`.gitattributes` can select arbitrary filter, diff, and merge driver names, and
Git has no universal switch that disables every external filter and merge
driver. New Git versions may add more executable settings.

## 3. Execution surfaces

| Surface | Common trigger | Mandatory countermeasure | Workflow effect |
|---|---|---|---|
| `core.pager`, `pager.<cmd>` | `log`, `diff`, `show`, help | `GIT_PAGER=cat` or fixed `--no-pager` | No interactive paging; irrelevant for captured MCP output |
| `core.fsmonitor` | `status`, `add`, index refresh | command-scope `core.fsmonitor=false` on modern Git | Full scan; negligible for transport-only profile |
| `$GIT_DIR/hooks`, `core.hooksPath` | commit, checkout, merge, push, ref updates | command-scope path to empty read-only directory | Project hooks do not run host-side |
| `core.sshCommand` | SSH clone/fetch/push | fixed `GIT_SSH_COMMAND` to trusted wrapper | Per-repo SSH commands ignored |
| `credential.helper` | HTTPS authentication | command-scope empty value resets list, followed by one trusted helper | User/repo helpers unavailable |
| `core.askPass`, askpass environment | Credential or key prompt | trusted failing askpass plus `GIT_TERMINAL_PROMPT=0` | Authentication fails instead of prompting |
| `core.editor`, `sequence.editor` | commit, tag, interactive rebase | trusted no-op editor or no host-side editing commands | Interactive editing unavailable through MCP |
| `gpg.*.program`, automatic signing | commit/tag/push/signature verification | disable automatic invocation; separate trusted signing capability | Signed push or host signing requires opt-in |
| `diff.external`, `diff.<driver>.command`, `textconv` | diff/log/show selected by attributes | `--no-ext-diff --no-textconv` where supported | No custom diff rendering |
| `filter.<driver>.{clean,smudge,process}` | checkout/add/merge/archive | keep triggering operations in container | LFS/encryption filters run only in container |
| `merge.<driver>.driver` | Content merge selected by attributes | keep merge in container | Custom merge behavior remains available locally |
| `interactive.diffFilter` | Interactive patch commands | command-scope empty value or keep commands local | No host-side custom patch filter |
| `ext::`, custom remote helpers | clone/fetch/push | `GIT_ALLOW_PROTOCOL` allowlist | Custom transports unavailable by default |
| `url.*.insteadOf`, remote URLs | Any remote operation | trusted transport wrapper enforces destination | Unapproved hosts fail |
| Auto-GC and maintenance config/hooks | Object-writing commands | `maintenance.auto=false`, `gc.auto=0` | Maintenance runs separately in container |
| Help viewers and browsers | `help`, `--help` fallback | avoid viewer-based help; use direct `-h` output under hardened env | No graphical/man viewer through MCP |
| Local clone optimization and alternates | Clone from filesystem path | deny `file` protocol; force `--no-local --no-hardlinks` if enabled | No local hardlink/alternate optimization |

Older Git versions through 2.35.1 may interpret boolean `core.fsmonitor=false`
as a hook pathname. The hardening profile must require a current patched Git
version before using that value.

## 4. Recommended split workflow

Repository-sensitive commands do not need host credentials and should run
inside the project container:

```text
git status
git log/show/diff
git add/rm/mv
git commit/tag
git checkout/switch/restore
git merge/rebase/cherry-pick/revert
git stash/reset
git submodule update
git gc/maintenance
```

Host-side wraptool Git should expose only credentialed transport operations:

```text
git fetch
git push
git ls-remote
git clone --no-checkout --no-recurse-submodules
```

Replace a host-side `pull` with two operations:

```text
wraptool:  git fetch
container: git merge/rebase origin/main
```

For a private clone:

```text
wraptool:  git clone --no-checkout --no-recurse-submodules ...
container: git checkout
```

This leaves checkout filters, merge drivers, hooks, editors, and project
tooling inside the credential-free container while preserving authenticated
network access through wraptool.

## 5. Developer and harness experience

Most hardening is invisible:

- Pager suppression does not change MCP output because output is already
  captured rather than attached to an interactive terminal.
- Editor suppression does not matter when commit and rebase remain local.
- Disabling fsmonitor has negligible cost for fetch and push.
- Disabling auto-maintenance removes unpredictable host-side latency;
  maintenance remains available in the container.
- Normal SSH and HTTPS remotes continue to work through operator-managed
  transport configuration.
- Prompt suppression turns a hanging call into an actionable authentication
  error.

Visible behavior changes:

- Project `pre-push` and other hooks do not run during wrapped push.
- Project-specific `core.sshCommand`, credential helpers, custom remote helpers,
  and URL rewrite tricks are ignored or rejected.
- Signed pushes require a separate trusted capability.
- Host-side `pull`, checkout, merge, rebase, commit, and attribute-sensitive
  archive operations are denied by the default profile.

Pre-push validation should run explicitly inside the container or in CI before
the wrapped push. This makes validation visible and keeps project code out of
the host process.

### Git LFS

Git LFS commonly installs a `pre-push` hook. Disabling project hooks would skip
LFS object upload and could leave a push containing only LFS pointers.

If LFS is supported, provide a wraptool-managed read-only hook directory with
only an approved `pre-push` hook that invokes an absolute trusted `git-lfs`
binary. Alternatively expose an explicit constrained `git lfs push` capability.
Never restore the project's general hooks directory for the whole Git process.
Clone remains `--no-checkout`; checkout and ordinary clean/smudge filters run in
the container.

## 6. Transport and destination controls

Neutralizing repository commands is not enough because the project controls
remote URLs through `.git/config`, including `url.*.insteadOf` rewrites.

### SSH

Set `GIT_SSH_COMMAND` to an operator-managed wrapper that enforces:

- approved hostname patterns and SSH users;
- fixed SSH config and identity;
- strict host-key checking;
- no `ProxyCommand` or agent forwarding;
- only the expected Git upload/receive service command;
- absolute trusted executable paths.

The wrapper is stronger than merely setting `ssh -F`: an ordinary SSH config
still accepts arbitrary destination arguments unless it explicitly rejects
them.

### HTTPS

Reset the accumulated `credential.helper` list at command scope, then add one
trusted absolute helper. Enforce:

- credentials scoped to approved host and, where practical, repository path;
- TLS verification;
- constrained redirect policy;
- no repository-selected proxy, credential helper, or extra authorization
  header;
- fast failure instead of terminal or graphical prompts.

A host-side authenticated transport broker is preferable where Git's URL-scoped
HTTP configuration cannot be made reliably invariant.

## 7. Current wraptool capabilities

`ToolConfig.Env` can inject command-scope Git configuration today through
`GIT_CONFIG_COUNT`. Git documents these values as overriding configuration
files, while explicit command-line `git -c` overrides them.

Illustrative SSH-oriented configuration:

```yaml
tools:
  git:
    binary: /usr/bin/git
    env:
      HOME: /var/lib/wraptool/git-home
      GIT_CONFIG_NOSYSTEM: "1"
      GIT_CONFIG_GLOBAL: /var/lib/wraptool/git-home/.gitconfig
      GIT_PAGER: cat
      GIT_TERMINAL_PROMPT: "0"
      GIT_ASKPASS: /usr/bin/false
      SSH_ASKPASS: /usr/bin/false
      GIT_ALLOW_PROTOCOL: ssh:https
      GIT_SSH_COMMAND: /usr/local/libexec/wraptool-git-ssh

      GIT_CONFIG_COUNT: "5"
      GIT_CONFIG_KEY_0: core.hooksPath
      GIT_CONFIG_VALUE_0: /var/lib/wraptool/empty-hooks
      GIT_CONFIG_KEY_1: core.fsmonitor
      GIT_CONFIG_VALUE_1: "false"
      GIT_CONFIG_KEY_2: maintenance.auto
      GIT_CONFIG_VALUE_2: "false"
      GIT_CONFIG_KEY_3: gc.auto
      GIT_CONFIG_VALUE_3: "0"
      GIT_CONFIG_KEY_4: credential.helper
      GIT_CONFIG_VALUE_4: ""
```

For HTTPS, append a second command-scope `credential.helper` value naming the
trusted helper and increase `GIT_CONFIG_COUNT`.

This is useful defense in depth, not yet a complete wraptool guarantee:

- the operator can omit or weaken values;
- introspection currently bypasses `ToolConfig.Env`;
- no fixed pre-subcommand arguments exist;
- no Git identity or hardening mode exists;
- a misconfigured allow rule can place explicit `-c` before the subcommand;
- filters and merge drivers still require command separation;
- positional constraints cannot currently enforce remote URL destinations.

## 8. Proposed wraptool implementation

### 8.1 Explicit hardening mode

Add an explicit mode such as:

```yaml
tools:
  git:
    binary: /usr/bin/git
    hardening: git-remote
```

Do not infer Git semantics from tool map key or executable basename. An explicit
mode makes behavior reviewable and supports wrappers or renamed binaries.

Default `git-remote` profile:

- permits policy rules only for the transport-oriented command set;
- applies mandatory command-scope config and environment after operator config;
- restricts protocols and destinations;
- suppresses pager, editor, askpass, hooks, fsmonitor, and auto-maintenance;
- disables automatic signing unless separately configured;
- denies recursive submodules and checkout during clone;
- applies transfer/fetch fsck settings where compatible;
- requires a supported patched Git version.

An explicit unsafe/trusted-repository mode may relax individual protections,
but must not be the default and must describe the resulting host-code-execution
authority during validation.

### 8.2 Canonical launch plan

Build one immutable execution plan containing:

- resolved binary identity;
- mandatory pre-subcommand argv;
- mandatory environment and command-scope config;
- selected CWD/project grant;
- caller-controlled subcommand, flags, and positionals.

Normal execution and introspection must use the same binary, hardened
environment, and fixed prefix. Introspection should prefer direct `-h` output
and must not invoke repository-selected help viewers.

Include hardening mode, fixed args, environment metadata, and version floor in
the introspection cache identity.

### 8.3 Override prevention

Validation must reject policy subcommands beginning with Git global options,
including:

- `-c`, `--config-env`;
- `-C`, `--git-dir`, `--work-tree`;
- `--exec-path`;
- `--paginate`;
- other global options that replace repository, configuration, executable, or
  transport selection.

Mandatory values must win over ordinary configured environment. Any explicit
operator escape hatch belongs in the hardening profile schema, not arbitrary
`env` replacement.

### 8.4 Diagnostics

Wraptool may inspect effective local config and report suppressed dangerous
settings such as `core.fsmonitor` or `credential.helper`. This improves operator
visibility but is not an enforcement boundary: config can change between scan
and execution, includes complicate provenance, and future Git versions can add
new executable settings.

Audit records should identify the applied hardening profile and trusted
transport/helper identity without logging credentials.

## 9. Stronger future boundary

Mandatory Git overrides cover known execution surfaces but do not protect
against unknown Git settings or future Git vulnerabilities. Strongest model is:

```text
container Git -> constrained host credential/transport broker -> remote
```

Git then retains complete project behavior inside the container while the host
broker performs authenticated SSH or HTTPS transport without returning raw
credentials. This avoids running repository-aware Git as the credential-holding
host user.

Possible broker forms:

- destination-constrained SSH signing/connection broker;
- HTTPS proxy that injects credentials only for approved origins;
- Git remote helper with a narrow protocol and no credential disclosure.

This is larger than the initial hardening profile but should remain the target
for the strongest isolation claim.

## 10. Delivery order

1. Correct current documentation: global config cannot override local config.
2. Add canonical execution plan and make introspection use the executor
   environment and fixed args.
3. Add explicit `git-remote` hardening mode and validation.
4. Add SSH destination wrapper and HTTPS trusted-helper contract.
5. Ship transport-only policy examples and split `pull`/clone guidance.
6. Add LFS trusted-hook or explicit LFS capability if required.
7. Evaluate credential-brokered container Git as a separate design.

Regression tests should use harmless marker executables to prove that hostile
local values for pager, fsmonitor, hooks, SSH command, credential helper,
editor, signing program, diff driver, and protocol helper do not execute under
the hardened profile. Tests must also prove that normal fetch/push behavior,
introspection, reload, and audit output remain functional.

## 11. References

- [Git security model](https://git-scm.com/docs/git#_security)
- [Git configuration scopes and precedence](https://git-scm.com/docs/git-config#SCOPES)
- [`GIT_CONFIG_COUNT` command scope](https://git-scm.com/docs/git-config#ENVIRONMENT)
- [Git hooks](https://git-scm.com/docs/githooks)
- [Git attributes, filters, diff, and merge drivers](https://git-scm.com/docs/gitattributes)
- [Git credential helpers](https://git-scm.com/docs/gitcredentials)
- [`ext::` remote command transport](https://git-scm.com/docs/git-remote-ext)
- [Git clone security guidance](https://git-scm.com/docs/git-clone)
- [Git 2024 security hardening](https://github.blog/open-source/git/securing-git-addressing-5-new-vulnerabilities/)
- Existing user guidance: `docs/git-hardening.qmd`
- Project grant boundary: `design/project-isolation.md`
