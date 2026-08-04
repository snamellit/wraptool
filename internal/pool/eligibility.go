package pool

import "time"

// Candidate is one version available from a Source's Refresh, paired with
// pool-local observation data, ready for eligibility evaluation (§9). A real
// candidate always carries a non-zero PublishedAt (§8: "publication ...
// times" is a required lock property); FirstSeenAt is populated by this
// pool's own Observations the first time a candidate is refreshed and is
// zero only before that has ever happened.
type Candidate struct {
	Version     string
	PublishedAt time.Time
	FirstSeenAt time.Time
	Prerelease  bool
	Yanked      bool

	// PublishedAtSynthesized is true when PublishedAt is not a real upstream
	// publication timestamp but RefreshHarnessLocked's own synthesized proxy
	// (PublishedAt := FirstSeenAt; see that function's doc comment for the
	// full rationale) — the case for every candidate an archive Source
	// returns (its release channel carries no publish-history field at all)
	// and occasionally an npm one missing registry "time" metadata.
	//
	// RULING (wave2-integration-fixes item 1, §9): minimum_release_age only
	// means something against a real upstream publish timestamp. Requiring
	// it of a source with no publish history makes it an unconditional wall
	// on every first install (a fresh pool's FirstSeenAt == PublishedAt ==
	// now, so minimum_release_age can never be satisfied no matter how
	// long a developer waits before ever running refresh) — exactly the
	// friction minimum_observation_age's 0h default exists to avoid. So for
	// a source with no publish history, minimum_observation_age (measured
	// against FirstSeenAt, which IS real — this pool's own first-refresh
	// clock read) is THE quarantine mechanism, and Eligible/
	// QuarantineRemaining below skip the minimum_release_age check entirely
	// when this is true. See internal/pool/source/archive.go's Refresh doc
	// comment for the archive-specific framing of the same ruling.
	//
	// Persisted verbatim as observations.json's fifth field,
	// published_at_synthesized (observations.go's Observation type) —
	// EARLIER drafts of this fix derived it by comparing the loaded
	// PublishedAt/FirstSeenAt for exact equality instead of persisting it,
	// reasoning that a real upstream publish timestamp could never
	// coincide with this pool's own first-observation instant. That
	// reasoning was wrong in practice: a harness whose candidate is first
	// refreshed at (or very close to) its real publish time — the ordinary
	// case for an npm harness refreshed promptly, and exactly what several
	// of this package's own tests construct — legitimately has
	// PublishedAt == FirstSeenAt with a REAL PublishedAt, which the
	// equality heuristic misclassified as synthesized and wrongly
	// exempted from minimum_release_age. Recording the fact explicitly, at
	// the one place that actually knows it (RefreshHarnessLocked, from
	// whether src.Refresh itself returned a zero PublishedAt for this
	// candidate), is the only correct source of truth.
	PublishedAtSynthesized bool
}

// Eligible reports whether candidate satisfies effective policy at now (§9):
//
//	now - published_at  >= minimum_release_age
//	now - first_seen_at >= minimum_observation_age
//
// A yanked candidate is always ineligible. A prerelease candidate is
// ineligible unless policy.AllowPrereleases. A zero MinimumReleaseAge or
// MinimumObservationAge always satisfies its own inequality outright — the
// comparison is skipped entirely, so it holds even for a future-dated
// timestamp, where now.Sub(...) would otherwise be negative (§7 "A duration
// of 0h disables that delay").
//
// A zero (unset) PublishedAt makes candidate unconditionally ineligible: a
// missing publication time cannot be trusted to satisfy any age requirement,
// including a 0h one, so it is checked before the 0h short-circuit above. A
// zero FirstSeenAt is ineligible only when MinimumObservationAge > 0 — with
// no observation-age requirement in effect, an unobserved candidate is not
// disqualified by that alone.
//
// c.PublishedAtSynthesized skips the minimum_release_age check entirely
// (RULING, Candidate.PublishedAtSynthesized's doc comment): a synthesized
// PublishedAt is not a real publish timestamp, so no minimum_release_age —
// including a 0h one, which is why this is a separate branch from the 0h
// short-circuit — can be meaningfully evaluated against it.
// minimum_observation_age against the real FirstSeenAt still applies
// unconditionally; it is the only quarantine such a candidate gets.
func Eligible(c Candidate, policy EffectivePolicy, now time.Time) bool {
	if c.Yanked {
		return false
	}
	if c.PublishedAt.IsZero() {
		return false
	}
	if policy.MinimumObservationAge > 0 && c.FirstSeenAt.IsZero() {
		return false
	}
	if c.Prerelease && !policy.AllowPrereleases {
		return false
	}
	if !c.PublishedAtSynthesized && policy.MinimumReleaseAge > 0 && now.Sub(c.PublishedAt) < policy.MinimumReleaseAge {
		return false
	}
	if policy.MinimumObservationAge > 0 && now.Sub(c.FirstSeenAt) < policy.MinimumObservationAge {
		return false
	}
	return true
}

// QuarantineRemaining returns how much longer c must wait before its age
// alone would satisfy policy at now — the larger of the release-age and
// observation-age remainders, floored at zero — and whether that duration is
// meaningful at all. ok is false when c carries a timestamp that makes it
// unconditionally ineligible regardless of elapsed time (a zero PublishedAt,
// or a zero FirstSeenAt under a non-zero MinimumObservationAge, mirroring
// Eligible's own zero-timestamp handling): no duration of waiting will ever
// satisfy the requirement, so callers must not render a countdown (e.g.
// `list`'s "(3d left)" column, §10) for these cases — show something like
// "not eligible" instead.
//
// When ok is true, QuarantineRemaining does not account for c.Yanked or
// c.Prerelease, which are binary exclusions a caller checks separately (a
// yanked or disallowed-prerelease candidate can report zero remaining time
// while still being ineligible).
//
// c.PublishedAtSynthesized excludes the release-age remainder from the
// countdown entirely (mirroring Eligible's own skip, RULING in
// Candidate.PublishedAtSynthesized's doc comment) — the countdown a
// synthesized candidate reports is the observation-age remainder alone, so
// e.g. `list`'s "(Nd left)" column for a quarantined archive-sourced
// harness reflects minimum_observation_age, the mechanism actually gating
// it, not a minimum_release_age that source can never really satisfy.
func QuarantineRemaining(c Candidate, policy EffectivePolicy, now time.Time) (remaining time.Duration, ok bool) {
	if c.PublishedAt.IsZero() {
		return 0, false
	}
	if policy.MinimumObservationAge > 0 && c.FirstSeenAt.IsZero() {
		return 0, false
	}

	var releaseRemaining, observationRemaining time.Duration
	if !c.PublishedAtSynthesized && policy.MinimumReleaseAge > 0 {
		releaseRemaining = policy.MinimumReleaseAge - now.Sub(c.PublishedAt)
	}
	if policy.MinimumObservationAge > 0 {
		observationRemaining = policy.MinimumObservationAge - now.Sub(c.FirstSeenAt)
	}

	remaining = releaseRemaining
	if observationRemaining > remaining {
		remaining = observationRemaining
	}
	if remaining < 0 {
		return 0, true
	}
	return remaining, true
}
