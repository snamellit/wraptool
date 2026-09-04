# Tracking the bundled Node runtime (issue #38)

Status: **implemented** (WP1 `internal/pool/node.go`; WP2 recipe_revision fold-in, engines.node check, and cmd wiring)

## Problem

`internal/pool/orchestrator.go` pins the Node runtime every npm-sourced
harness bundles as a compile-time constant (`defaultNodeVersion`). The
version is not a lock input: `update` re-resolves against the same constant,
reinstall short-circuits because `recipe_revision` still matches, and
rollback only reaches older generations built on the same runtime. Issue #38
is the consequence — pi started calling node's zstd API (v22.15.0+) while
the pool bundled 22.11.0, and no user-reachable path could move it. The
interim remedy (PR #39) bumps the constant plus `recipeFormatVersion`; this
note removes the need to patch code for routine runtime bumps.

## Design

Node versions become **observed, quarantined, locked inputs** — the same
lifecycle package versions already have. No new machinery; the existing
observations store, eligibility rules, lock, and recipe self-heal carry it.

1. **Pin the line, not the patch.** `internal/harness.Descriptor` gains
   `NodeLine` (e.g. `"22"`); empty means the pool-wide default line (a
   var, replacing `defaultNodeVersion`). Moving a harness — or the default —
   to a new major stays a deliberate code change: majors are compatibility
   decisions, patches are maintenance.
2. **Observe at refresh.** `RefreshHarness` for an npm-sourced harness also
   fetches `nodejs.org/dist/index.json`, selects the newest patch on the
   pinned line, and records it in the observations store under a
   `node:<line>` subject — write-once `first_seen_at`, `published_at` from
   the index's date field, same file format (§8.1). Node releases thereby
   get the same `minimum_release_age`/`minimum_observation_age` quarantine
   as package versions: a fresh node release sits out the cooling-off
   period before any pool realizes it.
3. **Resolve the eligible patch.** `UpdateHarness` passes the newest
   *eligible* observed patch to `Source.Resolve` instead of the constant.
   The npm Source's existing SHASUMS256.txt fetch stays the integrity
   check; `LockHarness.Runtime` already records the exact version and
   per-platform artifacts, so the lock shape is unchanged.
4. **Fold it into `recipe_revision`.** `npmRecipeRevision` adds the resolved
   node version to its hashed payload (one `recipeFormatVersion` bump for
   the field-shape change — the last one runtime maintenance needs). After
   that, a runtime bump makes the old lock mismatch and the standard
   Decide → resolve → realize self-heal ships it; `sync` on other machines
   realizes from the updated lock as usual.
5. **Check `engines.node` at resolve.** The packument's `engines.node` for
   the selected package version is evaluated against the resolved node
   version (Masterminds/semver, already a dependency). A mismatch fails
   `update` with an actionable error naming both versions and the line to
   change — catching #38's class at update time instead of harness startup.
   A missing or unparsable `engines` field is a non-event.

Result: routine node patch/security bumps flow through
`refresh`/`update`/`sync` with zero code changes. A code patch remains only
for changing a *line* (22 → 24) — as intended.

## Non-goals

- musl/alpine node artifacts (unchanged deferral, §5.1 of the pool design).
- Per-generation multiple node versions; one runtime per generation stays.
- Archive-sourced harnesses (agy bundles no node; untouched).

## Work packages

- **WP1 — observe + resolve:** descriptor `NodeLine`, the `node:<line>`
  observation subject in refresh, eligible-patch selection in update, the
  index.json fetch in `internal/pool/source` behind the existing test-server
  seams. Tests: quarantine applies to node observations; bare-line 404
  regression (the MAJOR-2 case) stays covered.
- **WP2 — lock + recipe + engines:** fold the resolved version into
  `npmRecipeRevision` (format bump + golden), `engines.node` check with its
  error text, self-heal integration test from a format-3-era lock.

WP2 depends on WP1's resolved-version plumbing; run sequentially or as one
package if the seam proves thin.
