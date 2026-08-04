package pool

import (
	"testing"
	"time"
)

func TestEligibleReleaseAgeBoundaries(t *testing.T) {
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	policy := EffectivePolicy{MinimumReleaseAge: 168 * time.Hour}

	cases := []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"just under threshold (<)", 168*time.Hour - time.Second, false},
		{"exactly at threshold (==)", 168 * time.Hour, true},
		{"just over threshold (>)", 168*time.Hour + time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Candidate{PublishedAt: now.Add(-tc.age), FirstSeenAt: now.Add(-365 * 24 * time.Hour)}
			if got := Eligible(c, policy, now); got != tc.want {
				t.Errorf("Eligible() = %v, want %v (age=%v)", got, tc.want, tc.age)
			}
		})
	}
}

func TestEligibleObservationAgeBoundaries(t *testing.T) {
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	policy := EffectivePolicy{MinimumObservationAge: 24 * time.Hour}

	cases := []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"just under threshold (<)", 24*time.Hour - time.Second, false},
		{"exactly at threshold (==)", 24 * time.Hour, true},
		{"just over threshold (>)", 24*time.Hour + time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Candidate{PublishedAt: now.Add(-365 * 24 * time.Hour), FirstSeenAt: now.Add(-tc.age)}
			if got := Eligible(c, policy, now); got != tc.want {
				t.Errorf("Eligible() = %v, want %v (age=%v)", got, tc.want, tc.age)
			}
		})
	}
}

func TestEligibleZeroDurationAlwaysSatisfied(t *testing.T) {
	now := time.Now().UTC()
	policy := EffectivePolicy{MinimumReleaseAge: 0, MinimumObservationAge: 0}
	c := Candidate{PublishedAt: now, FirstSeenAt: now} // published/observed right now
	if !Eligible(c, policy, now) {
		t.Error("0h quarantine should always be satisfied, even for a just-published/just-observed candidate")
	}
}

// TestEligibleZeroDurationSatisfiedForFutureDatedPublish covers MINOR 3: a
// registry clock slightly ahead of the pool host must not defeat a 0h
// (disabled) release-age policy just because now.Sub(publishedAt) is
// negative.
func TestEligibleZeroDurationSatisfiedForFutureDatedPublish(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(5 * time.Second)
	policy := EffectivePolicy{MinimumReleaseAge: 0, MinimumObservationAge: 0}
	c := Candidate{PublishedAt: future, FirstSeenAt: future}
	if !Eligible(c, policy, now) {
		t.Error("a 0h release-age policy should be satisfied even for a future-dated published_at")
	}
	remaining, ok := QuarantineRemaining(c, policy, now)
	if !ok {
		t.Fatal("QuarantineRemaining: ok = false, want true (candidate is eligible)")
	}
	if remaining != 0 {
		t.Errorf("QuarantineRemaining() = %v, want 0", remaining)
	}
}

func TestEligibleYankedAlwaysExcluded(t *testing.T) {
	now := time.Now().UTC()
	policy := EffectivePolicy{} // no age requirements, prereleases irrelevant
	c := Candidate{
		PublishedAt: now.Add(-365 * 24 * time.Hour),
		FirstSeenAt: now.Add(-365 * 24 * time.Hour),
		Yanked:      true,
	}
	if Eligible(c, policy, now) {
		t.Error("a yanked candidate must never be eligible")
	}
}

func TestEligiblePrereleaseFiltering(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-365 * 24 * time.Hour)

	c := Candidate{PublishedAt: old, FirstSeenAt: old, Prerelease: true}

	if Eligible(c, EffectivePolicy{AllowPrereleases: false}, now) {
		t.Error("prerelease should be excluded when AllowPrereleases is false")
	}
	if !Eligible(c, EffectivePolicy{AllowPrereleases: true}, now) {
		t.Error("prerelease should be eligible when AllowPrereleases is true and age requirements are met")
	}
}

// TestEligibleZeroPublishedAtUnconditionallyIneligible covers MAJOR 1: a
// zero-value PublishedAt (e.g. a Candidate assembled without setting it)
// must never pass eligibility just because time.Time{}'s huge apparent age
// happens to exceed any configured MinimumReleaseAge — including 0h, which
// would otherwise short-circuit straight to "satisfied".
func TestEligibleZeroPublishedAtUnconditionallyIneligible(t *testing.T) {
	now := time.Now().UTC()
	for _, policy := range []EffectivePolicy{
		{}, // fully permissive otherwise
		{MinimumReleaseAge: 0, MinimumObservationAge: 0}, // explicit 0h everywhere
		{MinimumReleaseAge: 168 * time.Hour},
	} {
		c := Candidate{PublishedAt: time.Time{}, FirstSeenAt: now.Add(-365 * 24 * time.Hour)}
		if Eligible(c, policy, now) {
			t.Errorf("a zero PublishedAt must be unconditionally ineligible under policy %+v", policy)
		}
		if _, ok := QuarantineRemaining(c, policy, now); ok {
			t.Errorf("QuarantineRemaining: ok = true for a zero PublishedAt under policy %+v, want false", policy)
		}
	}
}

// TestEligibleZeroFirstSeenAtIneligibleUnderObservationPolicy covers MAJOR 1:
// a zero FirstSeenAt must not pass a non-zero MinimumObservationAge (huge
// apparent age would otherwise trivially "satisfy" it), but with no
// observation-age requirement configured a zero FirstSeenAt is not by itself
// disqualifying.
func TestEligibleZeroFirstSeenAtIneligibleUnderObservationPolicy(t *testing.T) {
	now := time.Now().UTC()
	published := now.Add(-365 * 24 * time.Hour)

	withObservationPolicy := EffectivePolicy{MinimumObservationAge: 24 * time.Hour}
	c := Candidate{PublishedAt: published, FirstSeenAt: time.Time{}}
	if Eligible(c, withObservationPolicy, now) {
		t.Error("a zero FirstSeenAt must be ineligible when MinimumObservationAge > 0")
	}
	if _, ok := QuarantineRemaining(c, withObservationPolicy, now); ok {
		t.Error("QuarantineRemaining: ok = true for a zero FirstSeenAt under a non-zero observation policy, want false")
	}

	noObservationPolicy := EffectivePolicy{MinimumObservationAge: 0}
	if !Eligible(c, noObservationPolicy, now) {
		t.Error("a zero FirstSeenAt should not by itself block eligibility when MinimumObservationAge is 0")
	}
	if remaining, ok := QuarantineRemaining(c, noObservationPolicy, now); !ok || remaining != 0 {
		t.Errorf("QuarantineRemaining() = (%v, %v), want (0, true) when observation age is disabled", remaining, ok)
	}
}

// TestEligibleSynthesizedPublishedAtSkipsReleaseAge covers wave2-
// integration-fixes item 1 (§9): a PublishedAtSynthesized candidate's
// PublishedAt is not a real publish timestamp, so minimum_release_age must
// never gate it — including at DAY 0, when PublishedAt == FirstSeenAt ==
// now and a non-synthesized candidate at the same age would fail the
// default 168h release-age check outright. minimum_observation_age still
// applies unconditionally.
func TestEligibleSynthesizedPublishedAtSkipsReleaseAge(t *testing.T) {
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)

	t.Run("day 0 under default policy", func(t *testing.T) {
		policy := EffectivePolicy{MinimumReleaseAge: 168 * time.Hour, MinimumObservationAge: 0}
		c := Candidate{PublishedAt: now, FirstSeenAt: now, PublishedAtSynthesized: true}
		if !Eligible(c, policy, now) {
			t.Error("a synthesized-PublishedAt candidate observed just now must be eligible under the default policy (minimum_release_age skipped)")
		}
		if remaining, ok := QuarantineRemaining(c, policy, now); !ok || remaining != 0 {
			t.Errorf("QuarantineRemaining() = (%v, %v), want (0, true)", remaining, ok)
		}
	})

	t.Run("a non-synthesized candidate at the same age is still gated", func(t *testing.T) {
		policy := EffectivePolicy{MinimumReleaseAge: 168 * time.Hour, MinimumObservationAge: 0}
		c := Candidate{PublishedAt: now, FirstSeenAt: now, PublishedAtSynthesized: false}
		if Eligible(c, policy, now) {
			t.Error("a real (non-synthesized) PublishedAt at age 0 must still fail the default 168h release-age check")
		}
	})

	t.Run("minimum_observation_age still applies to a synthesized candidate", func(t *testing.T) {
		policy := EffectivePolicy{MinimumReleaseAge: 168 * time.Hour, MinimumObservationAge: 24 * time.Hour}
		fresh := Candidate{PublishedAt: now, FirstSeenAt: now, PublishedAtSynthesized: true}
		if Eligible(fresh, policy, now) {
			t.Error("a synthesized candidate must still be quarantined by a nonzero minimum_observation_age")
		}
		remaining, ok := QuarantineRemaining(fresh, policy, now)
		if !ok || remaining != 24*time.Hour {
			t.Errorf("QuarantineRemaining() = (%v, %v), want (24h, true) — countdown from observation age, not release age", remaining, ok)
		}

		matured := Candidate{PublishedAt: now.Add(-24 * time.Hour), FirstSeenAt: now.Add(-24 * time.Hour), PublishedAtSynthesized: true}
		if !Eligible(matured, policy, now) {
			t.Error("a synthesized candidate observed 24h ago must be eligible once minimum_observation_age elapses, regardless of the 168h release-age policy")
		}
	})
}

func TestQuarantineRemaining(t *testing.T) {
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	policy := EffectivePolicy{MinimumReleaseAge: 168 * time.Hour, MinimumObservationAge: 24 * time.Hour}

	// Published 3 days ago (release remaining = 168h-72h = 96h); observed
	// 20 hours ago (observation remaining = 4h). Release dominates.
	c := Candidate{
		PublishedAt: now.Add(-72 * time.Hour),
		FirstSeenAt: now.Add(-20 * time.Hour),
	}
	got, ok := QuarantineRemaining(c, policy, now)
	if !ok {
		t.Fatal("QuarantineRemaining: ok = false, want true")
	}
	want := 96 * time.Hour
	if got != want {
		t.Errorf("QuarantineRemaining() = %v, want %v", got, want)
	}

	// Already eligible: remaining must floor at zero, not go negative.
	eligibleCandidate := Candidate{
		PublishedAt: now.Add(-365 * 24 * time.Hour),
		FirstSeenAt: now.Add(-365 * 24 * time.Hour),
	}
	if got, ok := QuarantineRemaining(eligibleCandidate, policy, now); !ok || got != 0 {
		t.Errorf("QuarantineRemaining() for an eligible candidate = (%v, %v), want (0, true)", got, ok)
	}
}
