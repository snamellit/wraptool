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

func TestQuarantineRemaining(t *testing.T) {
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	policy := EffectivePolicy{MinimumReleaseAge: 168 * time.Hour, MinimumObservationAge: 24 * time.Hour}

	// Published 3 days ago (release remaining = 168h-72h = 96h); observed
	// 20 hours ago (observation remaining = 4h). Release dominates.
	c := Candidate{
		PublishedAt: now.Add(-72 * time.Hour),
		FirstSeenAt: now.Add(-20 * time.Hour),
	}
	got := QuarantineRemaining(c, policy, now)
	want := 96 * time.Hour
	if got != want {
		t.Errorf("QuarantineRemaining() = %v, want %v", got, want)
	}

	// Already eligible: remaining must floor at zero, not go negative.
	eligibleCandidate := Candidate{
		PublishedAt: now.Add(-365 * 24 * time.Hour),
		FirstSeenAt: now.Add(-365 * 24 * time.Hour),
	}
	if got := QuarantineRemaining(eligibleCandidate, policy, now); got != 0 {
		t.Errorf("QuarantineRemaining() for an eligible candidate = %v, want 0", got)
	}
}
