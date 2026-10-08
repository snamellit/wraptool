# Design: one runtime per worktree

Status: decided (2026-10-07); implementation planned as a work package on
the `feat/sbx-runtime` branch

## 1. Decision

A worktree is served by **one** container runtime at a time — Guix, Dev
Container, or Docker Sandboxes. `wraptool up` records which, `up` without
`--runtime` reuses it, switching needs an explicit `--runtime` after a
`wraptool down --rm`, and `wraptool down` acts only on the recorded runtime.

Running two runtimes against the same worktree concurrently is not
supported and `up` refuses it.

## 2. Why

The question came up on 2026-10-07 while testing the Docker Sandboxes
runtime on the wraptool repository itself, from a session running inside
that repository's Dev Container. `wraptool down` found a sandbox *and* a dev
container for the root and stopped both — including the container the
session was in. `wraptool up --runtime=sbx` then rewrote the worktree's
`.mcp.json` to the sandbox URL, which the still-running Dev Container
cannot reach. Both outcomes follow from treating runtimes as independent
per worktree, and neither can be fixed without the per-runtime config
files and `down --runtime` flags that a coexistence model would need.

There is no use case to pay for that with:

- **Choice of runtime is a property of the host, not of the worktree.** A
  Guix laptop uses the Guix runtime, a Mac the Dev Container or sandbox one.
  Those hosts have separate state directories; nothing is shared between
  them except the repository.
- **Changing runtimes on one host is a migration**, sequential by nature:
  stop the old one, start the new one. It does not need the two alive
  together.
- **The one thing that does want coexistence is testing wraptool on itself**,
  which is also the case where coexistence bites hardest. The remedy there
  is a scratch repository, not a feature.

What coexistence would cost, and this decision avoids:

- `.mcp.json` is one file at the workspace root with one server URL; two
  runtimes need two URLs (`127.0.0.1`, the bridge gateway,
  `host.docker.internal`). Per-runtime config files would push the choice
  into every harness descriptor in `internal/harness`.
- Two containers writing the same workspace, both presenting the same
  `?cwd=` to the server. The audit log cannot tell them apart.
- `down` needs a `--runtime` flag and a story for "stop one, keep the
  other", and today's probe-everything `down` is exactly what stopped the
  session's container.

## 3. Behaviour

### 3.1 Record

`up` writes a per-worktree record under `paths.DataDir()/workspaces/<key>.json`
where `<key>` is the first 16 hex of `sha256(root)` and `root` is the
absolute worktree path, after `filepath.Abs`:

```json
{
  "root": "/home/pti/src/proj",
  "runtime": "sbx",
  "harness": "claude",
  "created_at": "2026-10-07T12:50:00Z",
  "wraptool_version": "devel+4754697"
}
```

The record is written after the runtime's own bring-up succeeds (so a
failed first `up` leaves no record) and before the interactive shell is
entered. The Docker Sandboxes runtime's own `sandboxes/<name>.json` keeps
its runtime-specific fields (mounts, pinhole, template) and is unchanged;
the worktree record is the cross-runtime index above it.

### 3.2 `up`

1. No `--runtime` (or `--runtime=auto`), record present → the recorded
   runtime is used, printed as `container runtime: sbx (recorded)`. The
   `auto` probe (`selectRuntime`) runs only when there is no record.
2. Explicit `--runtime=X`, record says Y ≠ X → refuse:

   ```
   this worktree is on the devcontainer runtime (recorded 2026-10-07);
   run `wraptool down --rm` first, then `wraptool up --runtime=sbx`
   ```

   No implicit teardown: the other runtime may be in use by a session, as
   it was on 2026-10-07.
3. Explicit `--runtime=X`, record says X, or no record → proceed; the
   record is (re)written.
4. Record present but its runtime's backing object is gone (sandbox
   removed by hand with `sbx rm`, container removed with `docker rm`) →
   `up` treats the record as stale, says so, and proceeds as in 3 with
   whatever runtime was chosen. `down --rm` also removes the record.

### 3.3 `down`

- Record present → tear down only the recorded runtime. `--rm` also
  deletes the record (after the runtime's own removal succeeded).
- No record → today's behaviour: probe every runtime whose CLI is on PATH
  and act on whatever belongs to this root, printing which it found, so a
  worktree set up before this change can still be taken down. After this
  one legacy pass nothing is recorded; the next `up` records normally.

### 3.4 `.mcp.json`

Unchanged mechanically — one runtime at a time means one URL at a time.
`harness.Write`'s backup-on-overwrite covers the switch.

## 4. Non-goals

- Moving worktree records between hosts. They are host-local state like
  the pidfile and the sandbox state files.
- Recording the runtime inside the repository (a dotfile in the worktree).
  The worktree is agent-writable; the record decides which host processes
  `down` stops, so it stays in the developer's own data dir.
- A `wraptool status` surface for the record. `up` prints it; that is
  enough until `wraptool doctor` grows a workspace section.

## 5. Work package

```
Package:      one-runtime-per-worktree
Base branch:  feat/sbx-runtime
Spec:         design/harness/one-runtime-per-worktree.md §3
Deliverables: cmd/workspacestate.go (record type, path, read/write/remove,
              staleness probe per runtime); up.go uses the record before
              selectRuntime and refuses a runtime switch; down.go dispatches
              on the record and keeps the legacy probe-all path for "no
              record"
Tests:        up truth table: {no record, record=X, record=Y} ×
              {no flag, --runtime=X, --runtime=Y}; stale record proceeds and
              says so; failed bring-up writes no record; down with record
              touches only that runtime (fake sbx + fake docker both
              installed, assert the other is never called); down without
              record keeps today's two-sided behaviour byte-identical;
              --rm removes the record only after a successful teardown
Non-goals:    per-runtime .mcp.json; `down --runtime`; status command
Watch out:    record path uses filepath.Abs(root) exactly as sbxSandboxName
              does, or the sbx state and the worktree record disagree on
              identity; exhaustive switch over containerRuntime in the
              staleness probe; never auto-tear-down on a switch
```

## 6. Cross-references

- `design/harness/docker-sandbox-runtime.md` §5.2 (sandbox identity and
  lifecycle) — the sandbox state file this record indexes.
- `design/harness/guix-container-runtime.md` §6.1 — `selectRuntime`, now
  consulted only when no record exists.
- `design/project-isolation.md` §3 — project grants carry a
  non-authoritative `runtime` field; the worktree record is where `up`
  would read it from.
