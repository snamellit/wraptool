package pool

import (
	"fmt"
	"time"

	"github.com/Masterminds/semver/v3"
)

// This file is the PURE decision core UpdateHarnessLocked's imperative shell
// (orchestrator.go) builds an input from and switches on the output of. No
// I/O, no clock reads, and no Source/Backend calls happen anywhere below —
// every fact Decide needs (lock-file contents, the manifest constraint, the
// candidate list, the effective policy, "now") is read once by the shell and
// handed in as a DecisionInput value.
//
// It exists because UpdateHarnessLocked used to hand-roll this as inline
// if/else over a six-dimensional state space (lock exists × package matches
// × recipe matches × constraint satisfied × scan outcome × Advance mode,
// plus allow-young) with no explicit representation — during MR !59 a fix to
// one quadrant broke another twice. Collecting every predicate into one
// table-shaped function, tested exhaustively over that same state space
// (decision_test.go), is the fix. Phase 2 (realize/activate) extends this
// same flow, so the table needs to exist before it grows another dimension.

// DecisionKind is the closed set of outcomes Decide can produce (§9, §10).
// It is a typed-const enum (the PlatformKey convention,
// design/policy/flag-constraint-type-single-source.md) so a switch over it is
// exhaustive-lint-enforced: adding a new outcome forces every switch,
// including UpdateHarnessLocked's, to be updated.
type DecisionKind string

const (
	// DecisionUpToDate: no-op on BOTH axes — the lock already agrees
	// (resolve axis: package, recipe, and the manifest constraint) AND the
	// target platform's generation is already realized (realize axis:
	// GenerationDigestRecorded && GenerationDirPresent, §16 Phase 2).
	// Reached either before any candidate scan (install's strict
	// short-circuit) or after one (update scanned and the newest eligible
	// candidate turned out to be exactly what's already locked — nothing to
	// advance to). Either way there is nothing new to report: the caller
	// keeps its own existing lock entry unchanged, with no Note, and never
	// calls RealizeGeneration.
	DecisionUpToDate DecisionKind = "up_to_date"
	// DecisionRealize: the resolve axis is exactly as settled as it would be
	// for DecisionUpToDate — a usable lock entry exists, whether
	// pre-existing (the strict short-circuit's condition) or reached by
	// scanning to exactly the already-locked version — but the realize axis
	// is not: GenerationDigestRecorded is false (this platform has never been
	// realized), or it's true but GenerationDirPresent is false (the
	// generation directory named by the recorded digest is gone — lost
	// cache, manual deletion, a fresh workstation from a copied lock file;
	// §15/§17's "repair" case). The caller must run RealizeGeneration for the
	// target platform key; nothing on the resolve axis (Source.Resolve/
	// Fetch, or any lock field besides Generations) runs again.
	DecisionRealize DecisionKind = "realize"
	// DecisionKeepExisting: a candidate scan ran and found nothing eligible,
	// but a usable existing lock covers this harness (same package, still
	// satisfies the manifest constraint) — kept as-is rather than erroring,
	// with Note explaining why (the newest quarantined candidate and its
	// remaining wait, or that nothing is eligible at all, §9). Decide does
	// not fold the realize axis into this outcome (unlike DecisionUpToDate/
	// DecisionRealize above) — the shell checks GenerationDigestRecorded/
	// GenerationDirPresent itself on this path and calls RealizeGeneration
	// when needed, since it already has those facts in hand.
	DecisionKeepExisting DecisionKind = "keep_existing"
	// DecisionResolveVersion: advance to (or first-lock) Selected.Version.
	// The caller must run Source.Resolve/Fetch for it, persist a new lock
	// entry, and then run RealizeGeneration for the target platform key — a
	// freshly resolved version never has a recorded generation to reuse.
	DecisionResolveVersion DecisionKind = "resolve_version"
	// DecisionFail: no usable existing lock AND nothing eligible (or the
	// manifest constraint itself failed to parse). Err is the fully
	// formatted error the caller returns as-is.
	DecisionFail DecisionKind = "fail"
)

// DecisionInput captures everything Decide needs to choose an outcome. Every
// field is a plain value already read by the shell — nothing here triggers
// I/O, a clock read, or a Source/Backend call when Decide consumes it.
type DecisionInput struct {
	// Harness is the manifest/lock map key (e.g. "claude"), needed only to
	// format an error message identically to the pre-refactor text.
	Harness string
	// Package is the target source package identity Decide is choosing a
	// version of, e.g. "@anthropic-ai/claude-code" — compared against
	// LockPackage to detect a stale (different-package) lock entry.
	Package string
	// Advance selects install's strict idempotence (false) vs update's
	// scan-first, always-advance-if-eligible semantics (true) — see
	// UpdateOptions.Advance's doc comment for the full contract.
	Advance bool

	// HasLock, LockPackage, LockVersion, and LockRecipeRevision describe the
	// pool's existing lock entry for Harness, if any. LockPackage/
	// LockVersion/LockRecipeRevision are meaningless when HasLock is false.
	HasLock            bool
	LockPackage        string
	LockVersion        string
	LockRecipeRevision string
	// LockNPMRawIncomplete reports whether the existing lock entry's raw npm
	// lockfile is INCOMPLETE per !(*NPMLock).RawComplete() — see that
	// method's doc comment for what "incomplete" means and why it matters
	// (npm-raw-lockfile-backfill). Named as the negative (a "problem found"
	// flag, zero value false) rather than positive so every DecisionInput
	// literal that predates this field — including every existing test —
	// keeps its prior, unaffected behavior with no edits: the false zero
	// value means "nothing to heal," matching a non-npm entry (no NPMLock at
	// all, e.g. archive-sourced), a lock with no Lockfile yet, or simply
	// HasLock being false, all of which the shell also leaves at the zero
	// value since there is nothing to heal in any of them. When true, Decide
	// folds it into lockMatchesRecipe exactly like a recipe-format bump: the
	// existing entry is treated as NOT matching, forcing a fresh
	// DecisionResolveVersion so Source.Resolve rewrites the entry with a
	// patched raw lockfile — the same self-heal mechanism
	// recipeFormatVersion bumps use (orchestrator.go), applied per-entry
	// instead of pool-wide.
	LockNPMRawIncomplete bool

	// Constraint is the manifest's semver constraint for Harness (empty
	// means "any version", §7's descriptor stable channel).
	Constraint string
	// RecipeRevision is the freshly computed recipe_revision for Package
	// (npmRecipeRevision's result) — compared against LockRecipeRevision to
	// detect a stale recipe.
	RecipeRevision string

	// Candidates is the full candidate list for Package, post-refresh, with
	// FirstSeenAt already filled from this pool's own observations.
	Candidates []Candidate
	// Policy is the harness's effective, fully resolved policy (§7, §9).
	Policy EffectivePolicy

	// AllowYoung requests bypassing both age gates for candidate selection
	// (§9's emergency bypass). It never bypasses a yanked or
	// (policy-disallowed) prerelease exclusion — those are not age gates.
	AllowYoung bool
	// Now is the single clock read the whole decision is made against.
	Now time.Time

	// GenerationDigestRecorded reports whether the pool's existing lock
	// entry already records a receipt digest for the platform this
	// update/install targets, i.e. LockHarness.Generations[key] is set — a
	// plain map lookup the shell does once, no I/O. Meaningless when HasLock
	// is false (there is no existing entry to have recorded anything in).
	GenerationDigestRecorded bool
	// GenerationDirPresent reports whether that recorded generation's
	// directory still exists on disk (the shell's own os.Stat, read once
	// before calling Decide — Decide performs no I/O of its own). False
	// alongside GenerationDigestRecorded true is the "repair" case (§15/§17):
	// a digest is locked but the tree it names is gone, so realization must
	// run again even though nothing about the resolve axis changed.
	// Meaningless when GenerationDigestRecorded is false.
	GenerationDirPresent bool
}

// Decision is Decide's outcome: a Kind plus only the fields that Kind
// actually uses (the brief's "typed kind + fields" shape — not every field
// is meaningful for every Kind, see each field's doc comment).
type Decision struct {
	Kind DecisionKind

	// Note is set only for DecisionKeepExisting: the human-readable
	// explanation naming the kept version and, when relevant, the newest
	// quarantined candidate and its remaining wait, or that nothing is
	// eligible at all (§9).
	Note string

	// Selected is set only for DecisionResolveVersion: the candidate Decide
	// chose to resolve. The caller needs its Version (what to
	// resolve/fetch) and FirstSeenAt (stamped onto the freshly resolved
	// LockHarness).
	Selected Candidate
	// AllowYoungApplied is set only for DecisionResolveVersion: true when
	// AllowYoung actually changed which candidate got selected — Selected
	// would not have been Eligible under the real, non-bypassed Policy.
	// Requesting AllowYoung when it wasn't needed leaves this false (§9:
	// "only an effective bypass gets audited").
	AllowYoungApplied bool

	// Err is set only for DecisionFail: the fully formatted error the
	// caller should return unchanged.
	Err error
}

// generationRealized is the single realize-axis predicate: a platform is
// considered realized only when its lock entry both records a generation
// digest AND that generation's directory still exists on disk (§15/§17's
// "repair" case is exactly digestRecorded=true, dirPresent=false). Both
// Decide (the strict short-circuit and the scan-converges-to-locked path,
// which fold this into DecisionUpToDate vs DecisionRealize) and
// UpdateHarnessLocked's shell (the DecisionKeepExisting path, which Decide
// itself does not consult this on) call this same function — one rule, one
// place, so the two can never quietly drift apart.
func generationRealized(digestRecorded, dirPresent bool) bool {
	return digestRecorded && dirPresent
}

// Decide chooses one outcome for UpdateHarnessLocked's resolve+lock+fetch
// pass (§9 phases 2-3, §10 commands) from in, with no I/O of its own. It
// mirrors UpdateHarnessLocked's pre-refactor control flow exactly (see that
// function's doc comment for the three-case narrative this implements), now
// extended with the realize axis (§16 Phase 2, GenerationDigestRecorded/
// GenerationDirPresent):
//
//  1. install's strict short-circuit (in.Advance == false): if the existing
//     lock already matches in.Package and in.RecipeRevision, AND its version
//     satisfies in.Constraint, that is fully realized intent on the resolve
//     axis — without even looking at in.Candidates. Whether that's
//     DecisionUpToDate or DecisionRealize then depends purely on the realize
//     axis: realized (both GenerationDigestRecorded and GenerationDirPresent)
//     yields DecisionUpToDate, anything else yields DecisionRealize.
//  2. Otherwise, select the newest candidate that satisfies in.Constraint and
//     is Eligible under in.Policy (in.AllowYoung, when set, bypasses both age
//     gates for this selection only — never a yanked or disallowed-
//     prerelease exclusion, since Eligible enforces those unconditionally).
//     If nothing qualifies: DecisionKeepExisting when a usable existing lock
//     (same package, still satisfying in.Constraint) can be kept instead,
//     otherwise DecisionFail. (Decide does not consult the realize axis on
//     this path — see DecisionKeepExisting's doc comment.)
//  3. If selection succeeds but lands on exactly what's already locked (same
//     package, same recipe, same version): the same realize-axis check as
//     step 1 decides between DecisionUpToDate and DecisionRealize — nothing
//     to advance to on the resolve axis either way.
//  4. Otherwise: DecisionResolveVersion, naming the selected candidate and
//     whether reaching it required AllowYoung. A freshly resolved version
//     always needs a fresh realize; the caller does not consult the realize
//     axis for this outcome.
func Decide(in DecisionInput) Decision {
	var constraintObj *semver.Constraints
	if in.Constraint != "" {
		c, err := semver.NewConstraint(in.Constraint)
		if err != nil {
			return Decision{Kind: DecisionFail, Err: fmt.Errorf("harness %q: invalid constraint %q: %w", in.Harness, in.Constraint, err)}
		}
		constraintObj = c
	}

	lockSatisfiesConstraint := false
	if in.HasLock {
		if v, verr := semver.NewVersion(in.LockVersion); verr == nil {
			lockSatisfiesConstraint = constraintObj == nil || constraintObj.Check(v)
		}
	}
	// LockNPMRawIncomplete is folded in here, not as a separate branch: an
	// existing entry whose raw npm lockfile is incomplete (see that field's
	// doc comment) is treated exactly like a recipe mismatch — "not fully
	// settled on the resolve axis" — so every lockMatchesRecipe consumer
	// below (both the strict short-circuit and the "scan converged to what's
	// already locked" case) automatically routes to a fresh resolve without
	// needing its own awareness of the raw-lockfile concept.
	lockMatchesRecipe := in.HasLock && in.LockPackage == in.Package && in.LockRecipeRevision == in.RecipeRevision && !in.LockNPMRawIncomplete

	// realized is the realize axis's own verdict, independent of everything
	// above — see generationRealized's doc comment. The shell (orchestrator.go)
	// consults the SAME helper on the DecisionKeepExisting path, which Decide
	// itself does not fold the realize axis into (see DecisionKeepExisting's
	// doc comment): one rule, one place.
	realized := generationRealized(in.GenerationDigestRecorded, in.GenerationDirPresent)

	// install's strict short-circuit (Advance=false only): skip scanning
	// entirely when intent is already fully realized on the resolve axis,
	// even if a newer eligible candidate exists — install does not advance
	// (§10). Whether that's a full no-op (DecisionUpToDate) or still needs a
	// realize (DecisionRealize) depends only on the realize axis above.
	if !in.Advance && lockMatchesRecipe && lockSatisfiesConstraint {
		if realized {
			return Decision{Kind: DecisionUpToDate}
		}
		return Decision{Kind: DecisionRealize}
	}

	selectPolicy := in.Policy
	if in.AllowYoung {
		selectPolicy.MinimumReleaseAge = 0
		selectPolicy.MinimumObservationAge = 0
	}
	selected, selErr := selectNewestEligible(in.Candidates, in.Constraint, selectPolicy, in.Now)
	if selErr != nil {
		// Nothing eligible right now (even with a bypass applied), in EITHER
		// mode. Keep the existing lock instead of erroring, as long as it's
		// for the same package and its version still satisfies the
		// constraint — a recipe mismatch doesn't matter here, since nothing
		// new is being resolved. Hard-error only when there is no usable
		// lock to fall back to.
		if in.HasLock && in.LockPackage == in.Package && lockSatisfiesConstraint {
			note := fmt.Sprintf("keeping %s", in.LockVersion)
			if ver, remaining, ok := quarantinedCandidate(in.Candidates, in.Constraint, in.Policy, in.Now); ver != "" {
				if ok {
					note += fmt.Sprintf("; %s quarantined, %s remaining", ver, remaining)
				} else {
					note += fmt.Sprintf("; %s not eligible", ver)
				}
			} else {
				note += "; no eligible candidate"
			}
			return Decision{Kind: DecisionKeepExisting, Note: note}
		}
		return Decision{Kind: DecisionFail, Err: fmt.Errorf("harness %q: %w", in.Harness, selErr)}
	}

	// The scan succeeded. If it selected exactly what's already locked
	// (package+recipe unchanged too), this is a no-op for update — nothing
	// better to advance to. For install, reaching this point at all means
	// the Advance=false short-circuit above didn't fire, which only happens
	// when something didn't already match (no lock, package/recipe
	// mismatch, or an unsatisfied constraint) — so this branch mostly
	// matters for update, but applies uniformly.
	if lockMatchesRecipe && in.LockVersion == selected.Version {
		if realized {
			return Decision{Kind: DecisionUpToDate}
		}
		return Decision{Kind: DecisionRealize}
	}

	// An "effective" bypass: in.AllowYoung was set AND it's the reason
	// `selected` was reachable at all (it would not be Eligible under the
	// real, non-bypassed policy).
	allowYoungApplied := in.AllowYoung && !Eligible(selected, in.Policy, in.Now)

	return Decision{Kind: DecisionResolveVersion, Selected: selected, AllowYoungApplied: allowYoungApplied}
}

// versionedCandidate pairs a Candidate with its parsed semver.Version so
// repeated scans don't reparse Version on every comparison.
type versionedCandidate struct {
	cand Candidate
	ver  *semver.Version
}

// candidateScan is the result of scanning a candidate list against a version
// constraint: the best ELIGIBLE candidate under policy (if any), and,
// independently, the newest candidate satisfying the constraint regardless
// of eligibility — used to report what's currently being held back by
// quarantine.
type candidateScan struct {
	eligible *versionedCandidate
	newest   *versionedCandidate
}

func scanCandidatesForConstraint(candidates []Candidate, constraintStr string, policy EffectivePolicy, now time.Time) (candidateScan, error) {
	var constraintObj *semver.Constraints
	if constraintStr != "" {
		c, err := semver.NewConstraint(constraintStr)
		if err != nil {
			return candidateScan{}, fmt.Errorf("invalid constraint %q: %w", constraintStr, err)
		}
		constraintObj = c
	}

	var scan candidateScan
	for i := range candidates {
		cand := candidates[i]
		v, err := semver.NewVersion(cand.Version)
		if err != nil {
			continue // not a valid semver version; not selectable
		}
		if constraintObj != nil && !constraintObj.Check(v) {
			continue
		}
		if scan.newest == nil || v.GreaterThan(scan.newest.ver) {
			scan.newest = &versionedCandidate{cand, v}
		}
		if Eligible(cand, policy, now) && (scan.eligible == nil || v.GreaterThan(scan.eligible.ver)) {
			scan.eligible = &versionedCandidate{cand, v}
		}
	}
	return scan, nil
}

// selectNewestEligible picks the newest candidate that both satisfies
// constraint (empty means "any version", per §7 "Empty constraint means the
// descriptor's stable channel") and is Eligible under policy at now. When no
// candidate qualifies, it returns an error naming the newest
// constraint-satisfying candidate and, when that candidate's ineligibility is
// purely a matter of waiting, how much longer via QuarantineRemaining (§9's
// requirement that a failed update names the quarantined candidate and its
// remaining time).
func selectNewestEligible(candidates []Candidate, constraintStr string, policy EffectivePolicy, now time.Time) (Candidate, error) {
	scan, err := scanCandidatesForConstraint(candidates, constraintStr, policy, now)
	if err != nil {
		return Candidate{}, err
	}

	if scan.eligible != nil {
		return scan.eligible.cand, nil
	}
	if scan.newest == nil {
		if constraintStr == "" {
			return Candidate{}, fmt.Errorf("no observed candidate; run `wraptool harness refresh` first")
		}
		return Candidate{}, fmt.Errorf("no observed candidate satisfies constraint %q; run `wraptool harness refresh` first", constraintStr)
	}
	if remaining, ok := QuarantineRemaining(scan.newest.cand, policy, now); ok {
		return Candidate{}, fmt.Errorf("no eligible candidate satisfies constraint %q: newest matching candidate %s is quarantined, %s remaining",
			constraintStr, scan.newest.cand.Version, remaining)
	}
	return Candidate{}, fmt.Errorf("no eligible candidate satisfies constraint %q: newest matching candidate %s is not eligible (yanked, or a disallowed prerelease)",
		constraintStr, scan.newest.cand.Version)
}

// quarantinedCandidate reports the newest candidate satisfying constraint
// that is currently being held back from selection by policy. version==""
// means there is nothing to report: either no observed candidate satisfies
// constraint at all, or the newest one already IS the best eligible one
// (nothing is being held back). A constraint parse failure is swallowed to
// version=="" here — this helper only builds an informative message on an
// already-successful no-op path, where an invalid constraint would already
// have surfaced as a hard error earlier in the same call.
func quarantinedCandidate(candidates []Candidate, constraintStr string, policy EffectivePolicy, now time.Time) (version string, remaining time.Duration, ok bool) {
	scan, err := scanCandidatesForConstraint(candidates, constraintStr, policy, now)
	if err != nil || scan.newest == nil {
		return "", 0, false
	}
	if scan.eligible != nil && !scan.newest.ver.GreaterThan(scan.eligible.ver) {
		return "", 0, false
	}
	remaining, ok = QuarantineRemaining(scan.newest.cand, policy, now)
	return scan.newest.cand.Version, remaining, ok
}
