package pool

import "time"

// Candidate is one version available from a Source's Refresh, paired with
// pool-local observation data, ready for eligibility evaluation (§9).
type Candidate struct {
	Version     string
	PublishedAt time.Time
	FirstSeenAt time.Time
	Prerelease  bool
	Yanked      bool
}

// Eligible reports whether candidate satisfies effective policy at now (§9):
//
//	now - published_at  >= minimum_release_age
//	now - first_seen_at >= minimum_observation_age
//
// A yanked candidate is always ineligible. A prerelease candidate is
// ineligible unless policy.AllowPrereleases. A zero quarantine duration (0h)
// always satisfies its inequality, i.e. disables that check (§7 "A duration
// of 0h disables that delay").
func Eligible(c Candidate, policy EffectivePolicy, now time.Time) bool {
	if c.Yanked {
		return false
	}
	if c.Prerelease && !policy.AllowPrereleases {
		return false
	}
	if now.Sub(c.PublishedAt) < policy.MinimumReleaseAge {
		return false
	}
	if now.Sub(c.FirstSeenAt) < policy.MinimumObservationAge {
		return false
	}
	return true
}

// QuarantineRemaining returns how much longer c must wait before its age
// alone would satisfy policy at now — the larger of the release-age and
// observation-age remainders, floored at zero. It reports only the time-based
// component (for `list`'s "(3d left)" column, §10); it does not account for
// c.Yanked or c.Prerelease, which are binary exclusions a caller checks
// separately (a yanked or disallowed-prerelease candidate can report zero
// remaining time while still being ineligible).
func QuarantineRemaining(c Candidate, policy EffectivePolicy, now time.Time) time.Duration {
	releaseRemaining := policy.MinimumReleaseAge - now.Sub(c.PublishedAt)
	observationRemaining := policy.MinimumObservationAge - now.Sub(c.FirstSeenAt)

	remaining := releaseRemaining
	if observationRemaining > remaining {
		remaining = observationRemaining
	}
	if remaining < 0 {
		return 0
	}
	return remaining
}
