package pool

import (
	"os"
	"strings"
	"testing"
	"time"
)

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
	if m.Policy.MinimumReleaseAge != 168*time.Hour {
		t.Errorf("Policy.MinimumReleaseAge = %v, want 168h", m.Policy.MinimumReleaseAge)
	}
	if m.Harnesses["opencode"].MinimumReleaseAge == nil || *m.Harnesses["opencode"].MinimumReleaseAge != 72*time.Hour {
		t.Errorf("opencode.MinimumReleaseAge = %v, want 72h", m.Harnesses["opencode"].MinimumReleaseAge)
	}
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
	if m.Policy.MinimumReleaseAge != 24*time.Hour {
		t.Errorf("MinimumReleaseAge = %v, want 24h", m.Policy.MinimumReleaseAge)
	}
}

func TestEffectivePolicyInheritsGlobal(t *testing.T) {
	m := &Manifest{
		Policy: Policy{
			MinimumReleaseAge:     168 * time.Hour,
			MinimumObservationAge: time.Hour,
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
	override := 24 * time.Hour
	allowPre := true
	m := &Manifest{
		Policy: Policy{
			MinimumReleaseAge:     168 * time.Hour,
			MinimumObservationAge: time.Hour,
			AllowPrereleases:      false,
		},
		Harnesses: map[string]HarnessManifest{
			"claude": {
				Constraint:        ">=2 <3",
				MinimumReleaseAge: &override,
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
	zero := time.Duration(0)
	m := &Manifest{
		Policy: Policy{MinimumReleaseAge: 168 * time.Hour},
		Harnesses: map[string]HarnessManifest{
			"claude": {MinimumReleaseAge: &zero},
		},
	}
	eff := m.EffectivePolicy("claude")
	if eff.MinimumReleaseAge != 0 {
		t.Errorf("MinimumReleaseAge = %v, want 0 (disabled)", eff.MinimumReleaseAge)
	}

	// Global 0h disables for a harness with no override too.
	m2 := &Manifest{Policy: Policy{MinimumReleaseAge: 0}}
	eff2 := m2.EffectivePolicy("unlisted")
	if eff2.MinimumReleaseAge != 0 {
		t.Errorf("global 0h: MinimumReleaseAge = %v, want 0", eff2.MinimumReleaseAge)
	}
}

func TestEffectivePolicyUnknownHarnessGetsGlobal(t *testing.T) {
	m := &Manifest{Policy: Policy{MinimumReleaseAge: 168 * time.Hour, AllowPrereleases: true}}
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
		Policy: Policy{MinimumReleaseAge: 168 * time.Hour},
	}
	eff := m.EffectivePolicy("claude")
	if eff.MinimumReleaseAge != 168*time.Hour {
		t.Errorf("environment variable influenced policy: MinimumReleaseAge = %v, want 168h", eff.MinimumReleaseAge)
	}
}
