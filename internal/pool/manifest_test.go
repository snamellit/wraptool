package pool

import (
	"os"
	"strings"
	"testing"
	"time"
)

// durPtr is a small test helper so literal *time.Duration fields (Policy's
// global overrides and HarnessManifest's per-harness overrides) can be
// constructed inline.
func durPtr(d time.Duration) *time.Duration { return &d }

func TestParseManifestStrictRejectsUnknownKeys(t *testing.T) {
	data := []byte(`
schema_version: 1
policy:
  minimum_release_age: 168h
harnesses:
  claude:
    constraint: ">=2 <3"
    typo_field: oops
`)
	_, err := ParseManifest(data)
	if err == nil {
		t.Fatal("expected an error for an unknown key, got nil")
	}
}

func TestParseManifestRejectsBadConstraint(t *testing.T) {
	data := []byte(`
schema_version: 1
harnesses:
  claude:
    constraint: "not a valid constraint??"
`)
	_, err := ParseManifest(data)
	if err == nil {
		t.Fatal("expected an error for an invalid version constraint, got nil")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error should name the offending harness, got: %v", err)
	}
}

func TestParseManifestAcceptsEmptyConstraint(t *testing.T) {
	data := []byte(`
schema_version: 1
harnesses:
  claude: {}
`)
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Harnesses["claude"].Constraint != "" {
		t.Errorf("expected empty constraint, got %q", m.Harnesses["claude"].Constraint)
	}
}

func TestParseManifestValidDocument(t *testing.T) {
	data := []byte(`
schema_version: 1

policy:
  minimum_release_age: 168h
  minimum_observation_age: 0h
  allow_prereleases: false

harnesses:
  claude:
    constraint: ">=2 <3"
  opencode:
    constraint: ">=1 <2"
    minimum_release_age: 72h
`)
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", m.SchemaVersion)
	}
	if m.Policy.MinimumReleaseAge == nil || *m.Policy.MinimumReleaseAge != 168*time.Hour {
		t.Errorf("Policy.MinimumReleaseAge = %v, want 168h", m.Policy.MinimumReleaseAge)
	}
	if m.Harnesses["opencode"].MinimumReleaseAge == nil || *m.Harnesses["opencode"].MinimumReleaseAge != 72*time.Hour {
		t.Errorf("opencode.MinimumReleaseAge = %v, want 72h", m.Harnesses["opencode"].MinimumReleaseAge)
	}
}

func TestParseManifestRejectsMultipleDocuments(t *testing.T) {
	data := []byte(`
schema_version: 1
policy:
  minimum_release_age: 168h
---
schema_version: 1
policy:
  minimum_release_age: 0h
`)
	if _, err := ParseManifest(data); err == nil {
		t.Fatal("expected an error for a multi-document manifest, got nil")
	}
}

func TestParseManifestRejectsNegativeDuration(t *testing.T) {
	t.Run("global", func(t *testing.T) {
		data := []byte("schema_version: 1\npolicy:\n  minimum_release_age: -1h\n")
		if _, err := ParseManifest(data); err == nil {
			t.Fatal("expected an error for a negative global minimum_release_age, got nil")
		}
	})
	t.Run("per-harness", func(t *testing.T) {
		data := []byte("schema_version: 1\nharnesses:\n  claude:\n    minimum_observation_age: -24h\n")
		if _, err := ParseManifest(data); err == nil {
			t.Fatal("expected an error for a negative per-harness minimum_observation_age, got nil")
		}
	})
}

func TestLoadManifestMissingFile(t *testing.T) {
	if _, err := LoadManifest("/nonexistent/manifest.yaml"); err == nil {
		t.Fatal("expected an error for a missing file, got nil")
	}
}

func TestLoadManifestFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/manifest.yaml"
	data := []byte("schema_version: 1\npolicy:\n  minimum_release_age: 24h\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Policy.MinimumReleaseAge == nil || *m.Policy.MinimumReleaseAge != 24*time.Hour {
		t.Errorf("MinimumReleaseAge = %v, want 24h", m.Policy.MinimumReleaseAge)
	}
}

func TestEffectivePolicyInheritsGlobal(t *testing.T) {
	m := &Manifest{
		Policy: Policy{
			MinimumReleaseAge:     durPtr(168 * time.Hour),
			MinimumObservationAge: durPtr(time.Hour),
			AllowPrereleases:      false,
		},
		Harnesses: map[string]HarnessManifest{
			"claude": {Constraint: ">=2 <3"}, // no overrides
		},
	}
	eff := m.EffectivePolicy("claude")
	if eff.MinimumReleaseAge != 168*time.Hour {
		t.Errorf("MinimumReleaseAge = %v, want inherited 168h", eff.MinimumReleaseAge)
	}
	if eff.MinimumObservationAge != time.Hour {
		t.Errorf("MinimumObservationAge = %v, want inherited 1h", eff.MinimumObservationAge)
	}
	if eff.AllowPrereleases != false {
		t.Errorf("AllowPrereleases = %v, want inherited false", eff.AllowPrereleases)
	}
}

func TestEffectivePolicyPerHarnessOverride(t *testing.T) {
	allowPre := true
	m := &Manifest{
		Policy: Policy{
			MinimumReleaseAge:     durPtr(168 * time.Hour),
			MinimumObservationAge: durPtr(time.Hour),
			AllowPrereleases:      false,
		},
		Harnesses: map[string]HarnessManifest{
			"claude": {
				Constraint:        ">=2 <3",
				MinimumReleaseAge: durPtr(24 * time.Hour),
				AllowPrereleases:  &allowPre,
				// MinimumObservationAge omitted -> inherits.
			},
		},
	}
	eff := m.EffectivePolicy("claude")
	if eff.MinimumReleaseAge != 24*time.Hour {
		t.Errorf("MinimumReleaseAge = %v, want overridden 24h", eff.MinimumReleaseAge)
	}
	if eff.MinimumObservationAge != time.Hour {
		t.Errorf("MinimumObservationAge = %v, want inherited 1h", eff.MinimumObservationAge)
	}
	if eff.AllowPrereleases != true {
		t.Errorf("AllowPrereleases = %v, want overridden true", eff.AllowPrereleases)
	}
}

func TestEffectivePolicyZeroDisables(t *testing.T) {
	m := &Manifest{
		Policy: Policy{MinimumReleaseAge: durPtr(168 * time.Hour)},
		Harnesses: map[string]HarnessManifest{
			"claude": {MinimumReleaseAge: durPtr(0)},
		},
	}
	eff := m.EffectivePolicy("claude")
	if eff.MinimumReleaseAge != 0 {
		t.Errorf("MinimumReleaseAge = %v, want 0 (disabled)", eff.MinimumReleaseAge)
	}

	// An explicit global 0h disables for a harness with no override too.
	m2 := &Manifest{Policy: Policy{MinimumReleaseAge: durPtr(0)}}
	eff2 := m2.EffectivePolicy("unlisted")
	if eff2.MinimumReleaseAge != 0 {
		t.Errorf("global explicit 0h: MinimumReleaseAge = %v, want 0", eff2.MinimumReleaseAge)
	}
}

func TestEffectivePolicyUnknownHarnessGetsGlobal(t *testing.T) {
	m := &Manifest{Policy: Policy{MinimumReleaseAge: durPtr(168 * time.Hour), AllowPrereleases: true}}
	eff := m.EffectivePolicy("does-not-exist")
	if eff.MinimumReleaseAge != 168*time.Hour || eff.AllowPrereleases != true {
		t.Errorf("unlisted harness should get the global policy unchanged, got %+v", eff)
	}
}

// TestEffectivePolicyIgnoresEnvironment documents and locks in §7's "Project
// files and environment variables cannot weaken these settings" contract: the
// manifest type has no environment-variable input at all, so setting one
// cannot influence EffectivePolicy.
func TestEffectivePolicyIgnoresEnvironment(t *testing.T) {
	t.Setenv("WRAPTOOL_HARNESS_MINIMUM_RELEASE_AGE", "0h")
	m := &Manifest{
		Policy: Policy{MinimumReleaseAge: durPtr(168 * time.Hour)},
	}
	eff := m.EffectivePolicy("claude")
	if eff.MinimumReleaseAge != 168*time.Hour {
		t.Errorf("environment variable influenced policy: MinimumReleaseAge = %v, want 168h", eff.MinimumReleaseAge)
	}
}

// --- MAJOR 2: the default effective release-age quarantine is seven days,
// not zero, whenever the manifest is silent about it. ---

func TestEffectivePolicyDefaultsToSevenDaysOnEmptyManifest(t *testing.T) {
	m := &Manifest{} // zero-value: no policy block, no harnesses
	eff := m.EffectivePolicy("claude")
	if eff.MinimumReleaseAge != 168*time.Hour {
		t.Errorf("MinimumReleaseAge = %v, want default 168h", eff.MinimumReleaseAge)
	}
	if eff.MinimumObservationAge != 0 {
		t.Errorf("MinimumObservationAge = %v, want default 0h", eff.MinimumObservationAge)
	}
	if eff.AllowPrereleases {
		t.Error("AllowPrereleases = true, want default false")
	}
}

func TestEffectivePolicyDefaultsToSevenDaysOnMissingPolicyBlock(t *testing.T) {
	data := []byte("schema_version: 1\nharnesses:\n  claude:\n    constraint: \">=2 <3\"\n")
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	eff := m.EffectivePolicy("claude")
	if eff.MinimumReleaseAge != 168*time.Hour {
		t.Errorf("MinimumReleaseAge = %v, want default 168h", eff.MinimumReleaseAge)
	}
}

func TestEffectivePolicyDefaultsToSevenDaysOnCommentOnlyFile(t *testing.T) {
	data := []byte("# just a comment, no document content\n")
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	eff := m.EffectivePolicy("claude")
	if eff.MinimumReleaseAge != 168*time.Hour {
		t.Errorf("MinimumReleaseAge = %v, want default 168h", eff.MinimumReleaseAge)
	}
}

func TestEffectivePolicyExplicitZeroOverridesDefault(t *testing.T) {
	data := []byte("schema_version: 1\npolicy:\n  minimum_release_age: 0h\n")
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	eff := m.EffectivePolicy("claude")
	if eff.MinimumReleaseAge != 0 {
		t.Errorf("MinimumReleaseAge = %v, want explicit 0h (disabled), not the 168h default", eff.MinimumReleaseAge)
	}
}

func TestEffectivePolicyPerHarnessOverridesOmittedGlobal(t *testing.T) {
	// Global policy is entirely omitted (defaults apply); one harness pins a
	// fast-track override on top of that default.
	data := []byte("schema_version: 1\nharnesses:\n  claude:\n    minimum_release_age: 24h\n  opencode: {}\n")
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := m.EffectivePolicy("claude").MinimumReleaseAge; got != 24*time.Hour {
		t.Errorf("claude MinimumReleaseAge = %v, want overridden 24h", got)
	}
	if got := m.EffectivePolicy("opencode").MinimumReleaseAge; got != 168*time.Hour {
		t.Errorf("opencode MinimumReleaseAge = %v, want the 168h default (no override, omitted global)", got)
	}
}

// TestDefaultPolicyReturnsFreshValueEachCall covers N3: DefaultPolicy is a
// function, not a shared mutable var, so mutating one caller's returned
// EffectivePolicy can never leak into another caller's defaults.
func TestDefaultPolicyReturnsFreshValueEachCall(t *testing.T) {
	want := EffectivePolicy{MinimumReleaseAge: 168 * time.Hour, MinimumObservationAge: 0, AllowPrereleases: false}
	if got := DefaultPolicy(); got != want {
		t.Errorf("DefaultPolicy() = %+v, want %+v", got, want)
	}

	// Mutate the value returned by one call...
	mutated := DefaultPolicy()
	mutated.MinimumReleaseAge = 0
	mutated.AllowPrereleases = true

	// ...and confirm a fresh call, and EffectivePolicy resolution built on
	// top of it, are both unaffected.
	if got := DefaultPolicy(); got != want {
		t.Errorf("DefaultPolicy() after mutating a previous return = %+v, want unchanged %+v", got, want)
	}
	m := &Manifest{}
	if got := m.EffectivePolicy("claude"); got != want {
		t.Errorf("EffectivePolicy() after mutating a previous DefaultPolicy() return = %+v, want unchanged %+v", got, want)
	}
}
