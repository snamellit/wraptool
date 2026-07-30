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

// Policy is the pool-wide default update policy (§7, §9).
type Policy struct {
	// MinimumReleaseAge is how long a release must have been published
	// before it becomes eligible. 0h disables the check.
	MinimumReleaseAge time.Duration `yaml:"minimum_release_age"`
	// MinimumObservationAge is how long a release must have been locally
	// observed (via `refresh`) before it becomes eligible. 0h disables the
	// check; this is also the default (§9).
	MinimumObservationAge time.Duration `yaml:"minimum_observation_age"`
	AllowPrereleases      bool          `yaml:"allow_prereleases"`
}

// HarnessManifest is one harness's developer-selected constraint plus
// optional per-harness policy overrides. A nil override field inherits the
// pool-wide Policy value (§7 "omitted values inherit it"); an explicit 0h
// disables that harness's quarantine even if the global policy does not.
type HarnessManifest struct {
	// Constraint is a semver range (Masterminds/semver/v3 syntax, e.g.
	// ">=2 <3") or an exact version. Empty means the descriptor's stable
	// channel (§7).
	Constraint            string         `yaml:"constraint"`
	MinimumReleaseAge     *time.Duration `yaml:"minimum_release_age"`
	MinimumObservationAge *time.Duration `yaml:"minimum_observation_age"`
	AllowPrereleases      *bool          `yaml:"allow_prereleases"`
}

// EffectivePolicy is the resolved policy for one harness after applying any
// per-harness override on top of the global policy (§7, §9).
type EffectivePolicy struct {
	MinimumReleaseAge     time.Duration
	MinimumObservationAge time.Duration
	AllowPrereleases      bool
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
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks that every harness's version constraint (when non-empty)
// parses as a valid semver constraint. It is called by LoadManifest/
// ParseManifest; exported so a manifest built programmatically (e.g. by a
// future `policy set`) can be validated before it is written.
func (m *Manifest) Validate() error {
	for name, hm := range m.Harnesses {
		if hm.Constraint == "" {
			continue
		}
		if _, err := semver.NewConstraint(hm.Constraint); err != nil {
			return fmt.Errorf("harness %q: invalid version constraint %q: %w", name, hm.Constraint, err)
		}
	}
	return nil
}

// EffectivePolicy resolves the effective policy for harness: any field the
// harness does not override inherits the pool-wide Policy value; an explicit
// per-harness override (including 0h) always wins (§7). A harness with no
// manifest entry gets the pool-wide policy unchanged.
func (m *Manifest) EffectivePolicy(harness string) EffectivePolicy {
	eff := EffectivePolicy{
		MinimumReleaseAge:     m.Policy.MinimumReleaseAge,
		MinimumObservationAge: m.Policy.MinimumObservationAge,
		AllowPrereleases:      m.Policy.AllowPrereleases,
	}
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
