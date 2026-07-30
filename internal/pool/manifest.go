package pool

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"
)

// ManifestSchemaVersion is the only schema_version this package understands.
// LoadManifest does not reject other values (that is a semantic-validation
// concern for a later work package's `wraptool harness` commands), but code
// that writes a fresh manifest should stamp this constant.
const ManifestSchemaVersion = 1

// Manifest is the developer-owned pool intent and update policy (§7),
// user-editable YAML, strict-decoded like internal/config's KnownFields
// pattern so a typo'd key fails to load rather than being silently ignored.
type Manifest struct {
	SchemaVersion int                        `yaml:"schema_version"`
	Policy        Policy                     `yaml:"policy"`
	Harnesses     map[string]HarnessManifest `yaml:"harnesses"`
}

// Policy is the pool-wide default update policy (§7, §9). The duration
// fields are pointers, like HarnessManifest's per-harness overrides, so an
// omitted key (no `policy:` block at all, a comment-only manifest, or the
// key simply absent) is distinguishable from an explicit `0h`: omission
// falls back to DefaultPolicy, while an explicit 0h is a real, disabling
// value that must survive resolution unchanged (§9).
type Policy struct {
	// MinimumReleaseAge is how long a release must have been published
	// before it becomes eligible. Omitted -> DefaultPolicy.MinimumReleaseAge
	// (168h, §9). An explicit 0h disables the check.
	MinimumReleaseAge *time.Duration `yaml:"minimum_release_age"`
	// MinimumObservationAge is how long a release must have been locally
	// observed (via `refresh`) before it becomes eligible. Omitted ->
	// DefaultPolicy.MinimumObservationAge (0h, i.e. already disabled, §9).
	MinimumObservationAge *time.Duration `yaml:"minimum_observation_age"`
	// AllowPrereleases has no disabled/enabled ambiguity at its zero value
	// (false is both "omitted" and "explicitly disabled"), so it stays a
	// plain bool; DefaultPolicy.AllowPrereleases is false regardless.
	AllowPrereleases bool `yaml:"allow_prereleases"`
}

// HarnessManifest is one harness's developer-selected constraint plus
// optional per-harness policy overrides. A nil override field inherits the
// pool-wide effective Policy value (§7 "omitted values inherit it"); an
// explicit 0h disables that harness's quarantine even if the global policy
// does not.
type HarnessManifest struct {
	// Constraint is a semver range (Masterminds/semver/v3 syntax, e.g.
	// ">=2 <3") or an exact version. Empty means the descriptor's stable
	// channel (§7).
	Constraint            string         `yaml:"constraint"`
	MinimumReleaseAge     *time.Duration `yaml:"minimum_release_age"`
	MinimumObservationAge *time.Duration `yaml:"minimum_observation_age"`
	AllowPrereleases      *bool          `yaml:"allow_prereleases"`
}

// EffectivePolicy is the fully resolved policy for one harness after
// defaulting any omitted global field and applying any per-harness override
// on top (§7, §9). Every value here is concrete — no further defaulting.
type EffectivePolicy struct {
	MinimumReleaseAge     time.Duration
	MinimumObservationAge time.Duration
	AllowPrereleases      bool
}

// DefaultPolicy is applied wherever the manifest's policy is silent: no
// `policy:` block, an empty/comment-only manifest, or just one field of the
// block omitted (§9). minimum_release_age defaults to seven days — the
// concrete number this package must not silently drop to zero just because a
// developer wrote a manifest without a policy section.
// minimum_observation_age defaults to 0h (§9: requiring every developer to
// wait through a local observation window by default creates severe
// first-install friction). allow_prereleases defaults to false.
//
// An explicit value in the manifest — including an explicit `0h` — always
// overrides this default; DefaultPolicy only fills what is truly absent.
var DefaultPolicy = EffectivePolicy{
	MinimumReleaseAge:     168 * time.Hour,
	MinimumObservationAge: 0,
	AllowPrereleases:      false,
}

// LoadManifest strict-decodes manifest.yaml (yaml.v3 KnownFields, mirroring
// internal/config.LoadStrict) and validates every harness's version
// constraint eagerly, so an unknown key or an invalid constraint fails at
// load time instead of at resolve time. A missing file is an error — callers
// that want scaffolding behavior implement it themselves, mirroring
// internal/config.EnsureConfigFile's create-exclusive pattern.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// ParseManifest strict-decodes and validates manifest YAML from an in-memory
// byte slice, split out from LoadManifest so callers (and tests) can validate
// content without touching disk.
func ParseManifest(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var m Manifest
	if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}

	// Reject a second `---`-separated YAML document. This file is security
	// policy (release-age quarantine, allowed prereleases); a second document
	// almost certainly means an editing mistake that could hide the operative
	// document from a reviewer scanning the file, so loud is safer than
	// silently taking only the first one.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("parsing manifest: multiple YAML documents are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}

	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks that every harness's version constraint (when non-empty)
// parses as a valid semver constraint, and that no policy duration — global
// or per-harness — is negative. It is called by LoadManifest/ParseManifest;
// exported so a manifest built programmatically (e.g. by a future
// `policy set`) can be validated before it is written.
func (m *Manifest) Validate() error {
	if err := validatePolicyDurations(m.Policy.MinimumReleaseAge, m.Policy.MinimumObservationAge); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	for name, hm := range m.Harnesses {
		if hm.Constraint != "" {
			if _, err := semver.NewConstraint(hm.Constraint); err != nil {
				return fmt.Errorf("harness %q: invalid version constraint %q: %w", name, hm.Constraint, err)
			}
		}
		if err := validatePolicyDurations(hm.MinimumReleaseAge, hm.MinimumObservationAge); err != nil {
			return fmt.Errorf("harness %q: %w", name, err)
		}
	}
	return nil
}

func validatePolicyDurations(releaseAge, observationAge *time.Duration) error {
	if releaseAge != nil && *releaseAge < 0 {
		return fmt.Errorf("minimum_release_age must not be negative, got %s", *releaseAge)
	}
	if observationAge != nil && *observationAge < 0 {
		return fmt.Errorf("minimum_observation_age must not be negative, got %s", *observationAge)
	}
	return nil
}

// EffectivePolicy resolves the effective policy for harness: DefaultPolicy
// fills any field the global policy omits, then any field the harness
// overrides (including an explicit 0h) wins over that result (§7, §9). A
// harness with no manifest entry gets the (defaulted) pool-wide policy
// unchanged.
func (m *Manifest) EffectivePolicy(harness string) EffectivePolicy {
	eff := DefaultPolicy
	if m.Policy.MinimumReleaseAge != nil {
		eff.MinimumReleaseAge = *m.Policy.MinimumReleaseAge
	}
	if m.Policy.MinimumObservationAge != nil {
		eff.MinimumObservationAge = *m.Policy.MinimumObservationAge
	}
	eff.AllowPrereleases = m.Policy.AllowPrereleases

	hm, ok := m.Harnesses[harness]
	if !ok {
		return eff
	}
	if hm.MinimumReleaseAge != nil {
		eff.MinimumReleaseAge = *hm.MinimumReleaseAge
	}
	if hm.MinimumObservationAge != nil {
		eff.MinimumObservationAge = *hm.MinimumObservationAge
	}
	if hm.AllowPrereleases != nil {
		eff.AllowPrereleases = *hm.AllowPrereleases
	}
	return eff
}
