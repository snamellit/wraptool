# Agent-driven implementation workflow

Two subagent definitions support design-doc-driven implementation (built for
the `design/unified-harness-pool.md` migration, reusable for any spec):

- **implementor** — builds one scoped work package on an `impl/<slug>` branch
  in an isolated worktree.
- **reviewer** — adversarially reviews one such branch against its brief and
  the spec; report-only.

The **architect is the main Claude session** (not an agent file): it owns the
feature branch, slices the spec into work packages, spawns the agents,
integrates the results, and prepares the PR. Definition frontmatter requests
`isolation: worktree`; also pass `isolation: "worktree"` explicitly when
spawning, so each agent works on its own checkout either way.

Models: implementor runs on **sonnet** (well-briefed scoped coding; cheap
parallel fan-out), reviewer on **opus** (the quality gate — adversarial
reasoning is where the stronger tier pays). Override per-spawn via the Agent
tool's `model` parameter when a package warrants it (e.g. an opus implementor
for the OCI backend or realization post-processing).

## Architect loop

1. **Feature branch.** Create `feat/<feature>` from `main`; this is the only
   integration target. Agents never touch `main` and never push.
2. **Slice.** Cut the spec into work packages with explicit briefs (template
   below). Parallelize only packages with **disjoint file sets**; anything
   sharing a package/file runs sequentially. Dependency order for the pool
   spec follows its migration phases (§16): types/lock/locking first, then
   sources, then backends, then integration.
3. **Spawn implementors** (parallel where step 2 allows), each with
   `isolation: "worktree"` and one brief. Base branch = the feature branch
   (or a finished dependency's `impl/` branch when packages chain).
4. **Spawn a reviewer per finished branch** with the same brief, the branch
   name, and the integration base. On `request-changes`, send the findings
   back to the same implementor (SendMessage keeps its context) rather than
   spawning fresh.
5. **Integrate.** Merge approved `impl/` branches into the feature branch,
   resolve conflicts, and re-run the full gates (`gofmt -l`, `go build`,
   `go test`, `go vet`, `golangci-lint run`) on the merged result — passing
   per-branch does not imply passing merged.
6. **Final pass.** One reviewer run over the whole feature-branch diff
   against `main`, then the architect writes the PR (repo convention:
   branch → merge into `main` with a merge commit).

## Work-package brief template

```
Package:      <slug>                          (branch will be impl/<slug>)
Base branch:  <feat/... or impl/... it builds on>
Spec:         design/<doc>.md §<sections>     (normative for this package)
Deliverables: <packages/files, interfaces, commands, behaviors>
Tests:        <required cases, incl. which §17 items this package owns>
Non-goals:    <adjacent work explicitly excluded>
Watch out:    <known traps: platform contract points, lint rules, seams>
```

Keep briefs self-contained: agents start with no conversation context — they
know only their prompt, the repo, and CLAUDE.md.
