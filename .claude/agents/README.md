# Agent-driven implementation workflow

Two subagent definitions support design-doc-driven implementation (built for
the `design/harness/unified-harness-pool.md` migration, reusable for any spec):

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
   **When two parallel packages agree on a pinned interface seam, one of the
   same wave's briefs must also require an integration test driving the REAL
   components across that seam** (httptest/fakes only at the true process
   boundary). Per-package fakes systematically mask seam bugs — Phase 1's
   whole-branch review found two majors (wrong cache dir wiring, an
   unresolvable version shape) that every fake-based test was structurally
   unable to see, because the fakes discarded exactly the arguments that
   were wrong.
3. **Spawn implementors** (parallel where step 2 allows), each with
   `isolation: "worktree"` and one brief. Base branch = the feature branch
   (or a finished dependency's `impl/` branch when packages chain).
4. **Spawn a reviewer per finished branch** with the same brief, the branch
   name, and the integration base. On `request-changes`, send the findings
   back to the same implementor (SendMessage keeps its context) rather than
   spawning fresh — but **after 2–3 fix cycles on one agent, prefer a fresh
   implementor with a distilled brief**: accumulated context grows slower and
   no sharper (a Phase 1 implementor ended near 750k tokens with late rounds
   visibly diminished), and writing the fresh brief forces the architect to
   restate the actual remaining problem.
   **Treat fix rounds as new code, not as closure.** After round one, the fix
   is where the fresh bugs live — Phase 1's only blocker-regression, a
   dropped regression test, and a created-by-fix defect all came from fix
   commits. Concretely: (a) security-relevant fixes get a reviewer re-verify
   pass; test-only fixes get an architect delta check; (b) every fix-round
   check includes "**was any test deleted, weakened, or made vacuous by this
   change?**" — diff the test files, not just the code; (c) when a fix
   reshapes a decision predicate, require the fix's tests to cover the
   *other* quadrants of that predicate, not only the reported one.
5. **Integrate.** Merge approved `impl/` branches into the feature branch,
   resolve conflicts, and re-run the full gates (`gofmt -l`, `go build`,
   `go test`, `go vet`, `golangci-lint run`) on the merged result — passing
   per-branch does not imply passing merged.
6. **Final pass.** One reviewer run over the whole feature-branch diff
   against `main`, then the architect writes the PR (repo convention:
   branch → merge into `main` with a merge commit).

## MR sizing: consider one MR per wave

Phase 1 shipped as a single ~10k-line MR (!59). It worked, but every reviewer
of it — internal and external — effectively reviewed incrementally per push;
nobody reviewed the whole at once, and the long-lived branch collected a
mid-flight collision with unrelated work on `main`. For Phase 2 onward,
prefer **one MR per wave** when the wave boundary is a coherent, gate-green
unit (Phase 2: realization+receipts first, then activation+runtime-mount):
roughly half the review surface per MR and a shorter conflict window, at the
cost of briefly shipping infrastructure with no callers. Keep the single-MR
option for waves that only make sense together.

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
