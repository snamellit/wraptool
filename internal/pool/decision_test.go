package pool

import (
	"strings"
	"testing"
	"time"
)

// This file is the table test that motivates decision.go's existence: it
// enumerates Decide's reachable state space directly (mode × lock state ×
// candidate shape × allow-young), asserting the exact DecisionKind and
// payload for each reachable cell. Every predicate branch inside Decide must
// flip at least one of these rows if it changes.
//
// Fixed test fixtures, shared by every row below:
const (
	decisionTestHarness    = "claude"
	decisionTestConstraint = ">=2 <3"
	decisionTestPkg        = "@anthropic-ai/claude-code"
	decisionTestOtherPkg   = "other-package"
	decisionTestCurrent    = "sha256:current" // the freshly computed recipe_revision
	decisionTestStale      = "sha256:stale"   // a recipe_revision that does NOT match current
)

var decisionTestNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

var decisionTestPolicy = EffectivePolicy{
	MinimumReleaseAge:     168 * time.Hour,
	MinimumObservationAge: 0,
	AllowPrereleases:      false,
}

// decisionTestPolicyWithObservationGate additionally requires 48h of local
// observation, used by the handful of rows that isolate AllowYoung's
// observation-age bypass from its release-age bypass — decisionTestPolicy's
// MinimumObservationAge is 0 (already disabled), so under it alone deleting
// `selectPolicy.MinimumObservationAge = 0` inside Decide would go unnoticed.
var decisionTestPolicyWithObservationGate = EffectivePolicy{
	MinimumReleaseAge:     168 * time.Hour,
	MinimumObservationAge: 48 * time.Hour,
	AllowPrereleases:      false,
}

// --- candidate shapes (the "candidates" dimension) ---

// candNewerEligible: two candidates, both eligible under decisionTestPolicy
// (published 200h ago, well past the 168h release-age gate); 2.1.224 is the
// newer of the two.
func candNewerEligible() []Candidate {
	old := decisionTestNow.Add(-200 * time.Hour)
	return []Candidate{
		{Version: "2.1.220", PublishedAt: old, FirstSeenAt: old},
		{Version: "2.1.224", PublishedAt: old, FirstSeenAt: old},
	}
}

// candOnlySameVersionEligible: exactly one eligible candidate, 2.1.220 — the
// version an "agrees fully" lock is already pinned to.
func candOnlySameVersionEligible() []Candidate {
	old := decisionTestNow.Add(-200 * time.Hour)
	return []Candidate{{Version: "2.1.220", PublishedAt: old, FirstSeenAt: old}}
}

// candQuarantinedOnly: one candidate, published only 1h ago — quarantined
// under decisionTestPolicy's 168h release-age gate, but old enough to become
// eligible the moment AllowYoung bypasses that gate.
func candQuarantinedOnly() []Candidate {
	young := decisionTestNow.Add(-1 * time.Hour)
	return []Candidate{{Version: "2.1.230", PublishedAt: young, FirstSeenAt: young}}
}

// candNone: no observed candidates at all.
func candNone() []Candidate { return nil }

// candYankedButOld: old enough to clear both age gates, but yanked —
// AllowYoung bypasses age gates only, never a yanked exclusion (§9).
func candYankedButOld() []Candidate {
	old := decisionTestNow.Add(-300 * time.Hour)
	return []Candidate{{Version: "2.1.230", PublishedAt: old, FirstSeenAt: old, Yanked: true}}
}

// candQuarantinedAndYanked: published only 1h ago (quarantined by age) AND
// yanked. Unlike candYankedButOld (which is already old enough that its
// quarantine note would show "0s remaining" either way), this candidate is
// genuinely young: AllowYoung zeroes the age gates for selection, but the
// candidate is still unselectable because it's yanked, so Decide falls back
// to the SAME "nothing eligible, keep existing" path as when AllowYoung is
// off. That fallback's note names the quarantined candidate's remaining
// time computed from the real, un-bypassed policy (in.Policy) — this
// fixture is what lets a table row prove that, as opposed to accidentally
// reading 0s remaining from the age-zeroed selection policy.
func candQuarantinedAndYanked() []Candidate {
	young := decisionTestNow.Add(-1 * time.Hour)
	return []Candidate{{Version: "2.1.230", PublishedAt: young, FirstSeenAt: young, Yanked: true}}
}

// candQuarantinedByObservationOnly: published long enough ago to clear a
// 168h release-age gate on its own, but first-seen too recently to clear a
// separate, nonzero observation-age gate. decisionTestPolicy's own
// MinimumObservationAge is 0 (already disabled), so this fixture only means
// something paired with decisionTestPolicyWithObservationGate — it isolates
// AllowYoung's observation-age bypass from its release-age bypass, which
// candQuarantinedOnly alone can't distinguish (that fixture is young on
// BOTH clocks at once).
func candQuarantinedByObservationOnly() []Candidate {
	return []Candidate{{
		Version:     "2.1.230",
		PublishedAt: decisionTestNow.Add(-200 * time.Hour), // clears the 168h release-age gate
		FirstSeenAt: decisionTestNow.Add(-1 * time.Hour),   // fails the 48h observation-age gate
	}}
}

// candDisallowedPrereleaseYoung: a prerelease candidate under a policy that
// disallows prereleases — AllowYoung bypasses age gates only, never the
// prerelease exclusion (§9). Age is irrelevant here (1h old, would also be
// quarantined), but the point of this fixture is the prerelease flag.
func candDisallowedPrereleaseYoung() []Candidate {
	young := decisionTestNow.Add(-1 * time.Hour)
	return []Candidate{{Version: "2.1.230-beta.1", PublishedAt: young, FirstSeenAt: young, Prerelease: true}}
}

// --- lock shapes (the "lock" dimension) ---

// lockState is the (exists, package, version, recipeRevision) tuple
// DecisionInput carries for the pool's existing lock entry.
type lockState struct {
	has            bool
	pkg, ver, recp string
}

var (
	// lockAbsent: no lock entry exists yet — first install/update.
	lockAbsent = lockState{has: false}
	// lockAgrees: fully agreeing lock (right package, right recipe, version
	// 2.1.220 satisfies decisionTestConstraint).
	lockAgrees = lockState{has: true, pkg: decisionTestPkg, ver: "2.1.220", recp: decisionTestCurrent}
	// lockStaleRecipe: same package and constraint-satisfying version as
	// lockAgrees, but an outdated recipe_revision.
	lockStaleRecipe = lockState{has: true, pkg: decisionTestPkg, ver: "2.1.220", recp: decisionTestStale}
	// lockConstraintUnsatisfied: right package and recipe, but a version
	// (1.0.0) that no longer satisfies decisionTestConstraint (">=2 <3") —
	// e.g. the manifest constraint was tightened since this was locked.
	lockConstraintUnsatisfied = lockState{has: true, pkg: decisionTestPkg, ver: "1.0.0", recp: decisionTestCurrent}
	// lockDifferentPackage: a lock entry for an entirely different package
	// under the same harness name (e.g. the manifest's PoolPackage changed).
	// Its version happens to satisfy the constraint on its own terms —
	// lockSatisfiesConstraint does not itself check package identity, only
	// the package-equality checks elsewhere do.
	lockDifferentPackage = lockState{has: true, pkg: decisionTestOtherPkg, ver: "2.1.220", recp: decisionTestCurrent}
)

// --- the table ---

type decisionWant struct {
	kind              DecisionKind
	noteContains      []string // DecisionKeepExisting only
	noteNotContains   []string // DecisionKeepExisting only
	selectedVersion   string   // DecisionResolveVersion only
	allowYoungApplied bool     // DecisionResolveVersion only
	errContains       []string // DecisionFail only
	errNotContains    []string // DecisionFail only
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name       string
		advance    bool // false = install, true = update
		lock       lockState
		candidates []Candidate
		allowYoung bool
		policy     EffectivePolicy // zero value means decisionTestPolicy (see the loop below)
		want       decisionWant
	}{
		// --- install (Advance=false) + a fully agreeing lock: the strict
		// short-circuit fires purely from (Advance, lock) — it never even
		// looks at candidates or AllowYoung. Every combination of the
		// candidates and allowYoung dimensions collapses to this one
		// DecisionUpToDate outcome, so this row stands in for all 4×2 of
		// them; TestDecideInstallShortCircuitIgnoresCandidatesAndAllowYoung
		// below proves that collapse explicitly rather than just asserting it
		// in a comment.
		{
			name: "install + lock agrees: up to date, no scan needed",
			lock: lockAgrees, candidates: candNewerEligible(),
			want: decisionWant{kind: DecisionUpToDate},
		},

		// --- update (Advance=true) + a fully agreeing lock: update always
		// scans, so unlike install it DOES notice a newer eligible candidate
		// or reclassify "nothing eligible" as a scan outcome. This quartet is
		// the crux of §10's "update is the only normal command that
		// advances" and the MR !59 regression this refactor exists to guard.
		{
			name:    "update + lock agrees + newer eligible candidate: advances",
			advance: true, lock: lockAgrees, candidates: candNewerEligible(),
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.224", allowYoungApplied: false},
		},
		{
			name:    "update + lock agrees + only the locked version eligible: up to date (nothing to advance to)",
			advance: true, lock: lockAgrees, candidates: candOnlySameVersionEligible(),
			want: decisionWant{kind: DecisionUpToDate},
		},
		{
			name:    "update + lock agrees + nothing eligible (quarantined): keeps existing with a note",
			advance: true, lock: lockAgrees, candidates: candQuarantinedOnly(),
			want: decisionWant{kind: DecisionKeepExisting, noteContains: []string{"2.1.220", "2.1.230", "quarantined", "remaining"}},
		},
		{
			name:    "update + lock agrees + nothing eligible (quarantined) + allow-young: bypasses quarantine and advances",
			advance: true, lock: lockAgrees, candidates: candQuarantinedOnly(), allowYoung: true,
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.230", allowYoungApplied: true},
		},
		{
			name:    "update + lock agrees + no candidates at all: keeps existing with the default note",
			advance: true, lock: lockAgrees, candidates: candNone(),
			want: decisionWant{kind: DecisionKeepExisting, noteContains: []string{"2.1.220", "no eligible candidate"}},
		},
		// candNone + allowYoung collapses with the row above: AllowYoung
		// bypasses age gates, it cannot conjure a candidate that was never
		// observed at all.
		{
			// The sole candidate is both young (quarantined) and yanked:
			// AllowYoung zeroes the age gates for selection, but Eligible
			// still excludes it for being yanked, so selection fails exactly
			// as it would with AllowYoung off, and Decide falls into the
			// SAME "keep existing" path — DecisionKeepExisting, not
			// DecisionResolveVersion. The remaining-quarantine text in the
			// Note must come from the real, un-bypassed policy (168h - 1h =
			// 167h), not the age-zeroed selection policy (which would read
			// "0s remaining") — this is the row that kills a mutant
			// replacing in.Policy with selectPolicy in Decide's
			// quarantinedCandidate call.
			name:    "update + lock agrees + nothing eligible (quarantined AND yanked) + allow-young: yanked defeats the bypass, keeps existing with the UN-bypassed remaining time",
			advance: true, lock: lockAgrees, candidates: candQuarantinedAndYanked(), allowYoung: true,
			want: decisionWant{
				kind:            DecisionKeepExisting,
				noteContains:    []string{"2.1.220", "2.1.230", "quarantined", "167h0m0s", "remaining"},
				noteNotContains: []string{"quarantined, 0s remaining"},
			},
		},
		{
			// decisionTestPolicyWithObservationGate's 48h observation-age
			// gate is the only thing holding this candidate back (it
			// already clears the 168h release-age gate on its own) — proves
			// the observation-age half of the bypass independently of the
			// release-age half, which candQuarantinedOnly can't isolate.
			name:    "update + lock agrees + nothing eligible (observation-age quarantined only): keeps existing with a note",
			advance: true, lock: lockAgrees, candidates: candQuarantinedByObservationOnly(), policy: decisionTestPolicyWithObservationGate,
			want: decisionWant{kind: DecisionKeepExisting, noteContains: []string{"2.1.220", "2.1.230", "quarantined", "remaining"}},
		},
		{
			// Same candidate and policy as the row above, but with
			// AllowYoung on: this is the row that kills a mutant deleting
			// `selectPolicy.MinimumObservationAge = 0` inside Decide — with
			// only the release-age gate bypassed, this candidate would stay
			// ineligible (its FirstSeenAt is still only 1h old against a
			// 48h gate) and Decide would wrongly keep the existing lock
			// instead of advancing.
			name:    "update + lock agrees + nothing eligible (observation-age quarantined only) + allow-young: bypasses the observation gate and advances",
			advance: true, lock: lockAgrees, candidates: candQuarantinedByObservationOnly(), policy: decisionTestPolicyWithObservationGate, allowYoung: true,
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.230", allowYoungApplied: true},
		},
		{
			// AllowYoung requested but not needed: the candidate is already
			// eligible under the real policy, so the bypass has nothing to
			// do. Kills a mutant simplifying
			// `in.AllowYoung && !Eligible(selected, in.Policy, in.Now)` down
			// to just `in.AllowYoung` — that mutant would read
			// AllowYoungApplied=true here instead of false.
			name:    "update + lock agrees + newer eligible candidate + allow-young: advances, but the bypass was NOT needed (AllowYoungApplied stays false)",
			advance: true, lock: lockAgrees, candidates: candNewerEligible(), allowYoung: true,
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.224", allowYoungApplied: false},
		},

		// --- a stale recipe defeats the short-circuit in BOTH modes, since
		// lockMatchesRecipe is false regardless of Advance — install and
		// update become indistinguishable here. Both are asserted explicitly
		// once (this is exactly §8/§10's "even at the same version, a stale
		// recipe_revision forces a fresh resolve").
		{
			name: "install + stale recipe + only the locked version eligible: re-resolves the SAME version",
			lock: lockStaleRecipe, candidates: candOnlySameVersionEligible(),
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.220", allowYoungApplied: false},
		},
		{
			name:    "update + stale recipe + only the locked version eligible: re-resolves the SAME version (recipe forces re-resolve in both modes)",
			advance: true, lock: lockStaleRecipe, candidates: candOnlySameVersionEligible(),
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.220", allowYoungApplied: false},
		},
		{
			name: "install + stale recipe + newer eligible candidate: advances to it",
			lock: lockStaleRecipe, candidates: candNewerEligible(),
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.224", allowYoungApplied: false},
		},
		{
			name: "install + stale recipe + nothing eligible (quarantined): keeps existing with a note",
			lock: lockStaleRecipe, candidates: candQuarantinedOnly(),
			want: decisionWant{kind: DecisionKeepExisting, noteContains: []string{"2.1.220", "2.1.230", "quarantined", "remaining"}},
		},
		{
			name: "install + stale recipe + nothing eligible (quarantined) + allow-young: bypasses and advances",
			lock: lockStaleRecipe, candidates: candQuarantinedOnly(), allowYoung: true,
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.230", allowYoungApplied: true},
		},
		{
			name: "install + stale recipe + no candidates: keeps existing with the default note",
			lock: lockStaleRecipe, candidates: candNone(),
			want: decisionWant{kind: DecisionKeepExisting, noteContains: []string{"2.1.220", "no eligible candidate"}},
		},

		// --- a lock whose version no longer satisfies the manifest
		// constraint: lockSatisfiesConstraint is false, so the "keep existing
		// on nothing eligible" fallback does NOT apply here — this is the
		// case that distinguishes DecisionFail from DecisionKeepExisting.
		// Mode-independent for the same reason as the stale-recipe group
		// (lockMatchesRecipe is irrelevant once lockSatisfiesConstraint is
		// already false).
		{
			name: "install + constraint-unsatisfied lock + newer eligible candidate: advances (ignores the stale lock)",
			lock: lockConstraintUnsatisfied, candidates: candNewerEligible(),
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.224", allowYoungApplied: false},
		},
		{
			name:    "update + constraint-unsatisfied lock + nothing eligible: FAILS (an unsatisfying lock is not usable to fall back on)",
			advance: true, lock: lockConstraintUnsatisfied, candidates: candQuarantinedOnly(),
			want: decisionWant{kind: DecisionFail, errContains: []string{decisionTestHarness, "2.1.230"}},
		},
		{
			name:    "update + constraint-unsatisfied lock + nothing eligible + allow-young: bypasses and advances",
			advance: true, lock: lockConstraintUnsatisfied, candidates: candQuarantinedOnly(), allowYoung: true,
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.230", allowYoungApplied: true},
		},
		{
			name:    "update + constraint-unsatisfied lock + no candidates: FAILS",
			advance: true, lock: lockConstraintUnsatisfied, candidates: candNone(),
			want: decisionWant{kind: DecisionFail, errContains: []string{decisionTestHarness, decisionTestConstraint}},
		},

		// --- a lock entry for a different package under the same harness
		// name: lockMatchesRecipe is false (package mismatch), and the
		// keep-existing fallback's own package check independently excludes
		// it even though lockSatisfiesConstraint alone would have been true.
		{
			name: "install + lock for a different package + newer eligible candidate: advances",
			lock: lockDifferentPackage, candidates: candNewerEligible(),
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.224", allowYoungApplied: false},
		},
		{
			name:    "update + lock for a different package + nothing eligible: FAILS (keep-existing's package check excludes it)",
			advance: true, lock: lockDifferentPackage, candidates: candQuarantinedOnly(),
			want: decisionWant{kind: DecisionFail, errContains: []string{decisionTestHarness, "2.1.230"}},
		},
		{
			name:    "update + lock for a different package + nothing eligible + allow-young: bypasses and advances",
			advance: true, lock: lockDifferentPackage, candidates: candQuarantinedOnly(), allowYoung: true,
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.230", allowYoungApplied: true},
		},

		// --- no lock at all: first install/update. Nothing to fall back on,
		// so "nothing eligible" is always DecisionFail.
		{
			name: "install + no lock + newer eligible candidates: picks the newest eligible",
			lock: lockAbsent, candidates: candNewerEligible(),
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.224", allowYoungApplied: false},
		},
		{
			name: "install + no lock + exactly one eligible candidate: picks it",
			lock: lockAbsent, candidates: candOnlySameVersionEligible(),
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.220", allowYoungApplied: false},
		},
		{
			name: "install + no lock + nothing eligible (quarantined): FAILS naming the quarantined candidate and remaining wait",
			lock: lockAbsent, candidates: candQuarantinedOnly(),
			want: decisionWant{kind: DecisionFail, errContains: []string{decisionTestHarness, "2.1.230", "remaining"}},
		},
		{
			name: "install + no lock + nothing eligible (quarantined) + allow-young: bypasses and advances",
			lock: lockAbsent, candidates: candQuarantinedOnly(), allowYoung: true,
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.230", allowYoungApplied: true},
		},
		{
			name: "install + no lock + no candidates at all: FAILS",
			lock: lockAbsent, candidates: candNone(),
			want: decisionWant{kind: DecisionFail, errContains: []string{decisionTestHarness, decisionTestConstraint}},
		},

		// --- allow-young's bypass is age-gates only: it never selects a
		// yanked candidate or a disallowed prerelease, no matter how old
		// they are (§9).
		{
			name: "no lock + only candidate is old but yanked + allow-young: still FAILS (yanked is never bypassed)",
			lock: lockAbsent, candidates: candYankedButOld(), allowYoung: true,
			want: decisionWant{kind: DecisionFail, errContains: []string{decisionTestHarness, "2.1.230"}},
		},
		{
			// Empty constraint deliberately: Masterminds/semver excludes a
			// prerelease version from a non-prerelease range check
			// (">=2 <3") entirely, which would make this row exercise
			// constraint filtering instead of the prerelease exclusion it's
			// meant to prove. With no constraint to filter on, the candidate
			// reaches Eligible, which is where AllowYoung must still not
			// bypass Prerelease && !AllowPrereleases.
			name: "no lock + empty constraint + only candidate is a disallowed prerelease + allow-young: still FAILS (prerelease exclusion is never bypassed)",
			lock: lockAbsent, candidates: candDisallowedPrereleaseYoung(), allowYoung: true,
			want: decisionWant{kind: DecisionFail, errContains: []string{decisionTestHarness, "2.1.230-beta.1"}},
		},

		// --- an empty manifest constraint means "any version" (§7): no
		// candidate is filtered out, and the "nothing observed" error text
		// drops the "satisfies constraint" phrasing since there is no
		// constraint to name.
		{
			name: "no lock + empty constraint + newer eligible candidates: picks the newest with no constraint filtering",
			lock: lockAbsent, candidates: candNewerEligible(),
			want: decisionWant{kind: DecisionResolveVersion, selectedVersion: "2.1.224", allowYoungApplied: false},
		},
		{
			name: "no lock + empty constraint + no candidates: FAILS without 'satisfies constraint' in the message",
			lock: lockAbsent, candidates: candNone(),
			want: decisionWant{kind: DecisionFail, errContains: []string{decisionTestHarness, "no observed candidate"}, errNotContains: []string{"satisfies constraint"}},
		},

		// --- an unparseable manifest constraint is a hard failure
		// independent of every other dimension.
		{
			name: "invalid manifest constraint: FAILS regardless of lock or candidates",
			lock: lockAgrees, candidates: candNewerEligible(),
			want: decisionWant{kind: DecisionFail, errContains: []string{decisionTestHarness, "not-a-valid-constraint", "invalid constraint"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			constraint := decisionTestConstraint
			// The two rows above that exercise an empty/invalid constraint
			// signal it via their want.kind + a name match instead of adding
			// yet another table column used by only one or two rows.
			switch {
			case strings.Contains(tt.name, "empty constraint"):
				constraint = ""
			case strings.Contains(tt.name, "invalid manifest constraint"):
				constraint = "not-a-valid-constraint"
			}

			policy := tt.policy
			if policy == (EffectivePolicy{}) {
				policy = decisionTestPolicy
			}

			in := DecisionInput{
				Harness:            decisionTestHarness,
				Package:            decisionTestPkg,
				Advance:            tt.advance,
				HasLock:            tt.lock.has,
				LockPackage:        tt.lock.pkg,
				LockVersion:        tt.lock.ver,
				LockRecipeRevision: tt.lock.recp,
				Constraint:         constraint,
				RecipeRevision:     decisionTestCurrent,
				Candidates:         tt.candidates,
				Policy:             policy,
				AllowYoung:         tt.allowYoung,
				Now:                decisionTestNow,
			}

			got := Decide(in)

			if got.Kind != tt.want.kind {
				t.Fatalf("Kind = %v, want %v (decision: %+v)", got.Kind, tt.want.kind, got)
			}

			switch tt.want.kind {
			case DecisionUpToDate:
				// No payload to check.
			case DecisionKeepExisting:
				for _, s := range tt.want.noteContains {
					if !strings.Contains(got.Note, s) {
						t.Errorf("Note = %q, want it to contain %q", got.Note, s)
					}
				}
				for _, s := range tt.want.noteNotContains {
					if strings.Contains(got.Note, s) {
						t.Errorf("Note = %q, want it to NOT contain %q", got.Note, s)
					}
				}
			case DecisionResolveVersion:
				if got.Selected.Version != tt.want.selectedVersion {
					t.Errorf("Selected.Version = %q, want %q", got.Selected.Version, tt.want.selectedVersion)
				}
				if got.AllowYoungApplied != tt.want.allowYoungApplied {
					t.Errorf("AllowYoungApplied = %v, want %v", got.AllowYoungApplied, tt.want.allowYoungApplied)
				}
			case DecisionFail:
				if got.Err == nil {
					t.Fatal("Err = nil, want a non-nil error")
				}
				for _, s := range tt.want.errContains {
					if !strings.Contains(got.Err.Error(), s) {
						t.Errorf("Err = %q, want it to contain %q", got.Err.Error(), s)
					}
				}
				for _, s := range tt.want.errNotContains {
					if strings.Contains(got.Err.Error(), s) {
						t.Errorf("Err = %q, want it to NOT contain %q", got.Err.Error(), s)
					}
				}
			}
		})
	}
}

// TestDecideInstallShortCircuitIgnoresCandidatesAndAllowYoung proves the
// collapse claimed by the first row of TestDecide's table: install's strict
// short-circuit depends only on (Advance, lock), never on candidates or
// AllowYoung. It exercises every candidates × allowYoung combination against
// a fully agreeing lock under Advance=false and requires all of them to
// produce the identical DecisionUpToDate decision.
func TestDecideInstallShortCircuitIgnoresCandidatesAndAllowYoung(t *testing.T) {
	candidateSets := map[string][]Candidate{
		"newer eligible":        candNewerEligible(),
		"only same version":     candOnlySameVersionEligible(),
		"quarantined only":      candQuarantinedOnly(),
		"none":                  candNone(),
		"yanked but old":        candYankedButOld(),
		"disallowed prerelease": candDisallowedPrereleaseYoung(),
	}
	for name, candidates := range candidateSets {
		for _, allowYoung := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				got := Decide(DecisionInput{
					Harness:            decisionTestHarness,
					Package:            decisionTestPkg,
					Advance:            false,
					HasLock:            true,
					LockPackage:        lockAgrees.pkg,
					LockVersion:        lockAgrees.ver,
					LockRecipeRevision: lockAgrees.recp,
					Constraint:         decisionTestConstraint,
					RecipeRevision:     decisionTestCurrent,
					Candidates:         candidates,
					Policy:             decisionTestPolicy,
					AllowYoung:         allowYoung,
					Now:                decisionTestNow,
				})
				if got.Kind != DecisionUpToDate {
					t.Fatalf("candidates=%q allowYoung=%v: Kind = %v, want DecisionUpToDate (install must short-circuit before consulting either)", name, allowYoung, got.Kind)
				}
			})
		}
	}
}
