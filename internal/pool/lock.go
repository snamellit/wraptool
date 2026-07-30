package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// LockSchemaVersion is the schema_version this package writes and
// understands for harness-lock.json (§8).
const LockSchemaVersion = 1

// Lock is the generated, exact resolution of every harness the manifest
// selects (§8) — harness-lock.json. It contains everything realization needs
// without consulting a mutable version selector: exact version, integrity for
// every artifact, the complete transitive dependency graph (npm sources), and
// a receipt digest per realized platform.
//
// This package defines the shape and (de)serialization only. Producing a Lock
// (resolve mechanics, §8.1) is a later work package's Source implementation.
type Lock struct {
	SchemaVersion int                    `json:"schema_version"`
	GeneratedAt   string                 `json:"generated_at"` // UTC RFC3339
	PolicyHash    string                 `json:"policy_hash"`  // "sha256:<hex>"
	Harnesses     map[string]LockHarness `json:"harnesses"`
}

// LockHarness is one harness's exact resolution.
type LockHarness struct {
	Source         string       `json:"source"`  // e.g. "npm"
	Package        string       `json:"package"` // e.g. "@anthropic-ai/claude-code"
	Version        string       `json:"version"`
	PublishedAt    string       `json:"published_at"`    // UTC RFC3339
	FirstSeenAt    string       `json:"first_seen_at"`   // UTC RFC3339
	RecipeRevision string       `json:"recipe_revision"` // "sha256:<hex>"
	Runtime        *LockRuntime `json:"runtime,omitempty"`
	NPMLock        *NPMLock     `json:"npm_lock,omitempty"`
	// Generations maps a platform key to its receipt digest ("sha256:<hex>"),
	// present once that platform has been realized (§11.5).
	Generations map[string]string `json:"generations,omitempty"`
}

// LockRuntime pins the bundled language runtime (e.g. Node) a harness needs,
// per platform.
type LockRuntime struct {
	Name      string                         `json:"name"`
	Version   string                         `json:"version"`
	Artifacts map[string]LockRuntimeArtifact `json:"artifacts"` // platform key -> artifact
}

// LockRuntimeArtifact is one platform's downloadable runtime artifact.
type LockRuntimeArtifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// NPMLock embeds the resolved npm dependency graph for one harness (§8.1):
// exact versions, resolved tarball URLs, and both the upstream sha512 SRI
// integrity value and the pool's own sha256 cache address.
type NPMLock struct {
	LockfileVersion int                       `json:"lockfile_version"`
	Packages        map[string]NPMLockPackage `json:"packages"`
}

// NPMLockPackage is one entry of the npm dependency graph.
type NPMLockPackage struct {
	Version   string `json:"version"`
	Resolved  string `json:"resolved"`
	Integrity string `json:"integrity"` // upstream sha512 SRI
	SHA256    string `json:"sha256"`    // pool cache address (cache/sha256/<digest>)
}

// Marshal serializes the lock deterministically. encoding/json already sorts
// map[string]... keys and struct field order is fixed by the type
// definition, so repeated calls on an equal value produce byte-identical
// output (§17 "deterministic lock serialization").
func (l *Lock) Marshal() ([]byte, error) {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling lock: %w", err)
	}
	return data, nil
}

// UnmarshalLock parses lock JSON produced by Marshal.
func UnmarshalLock(data []byte) (*Lock, error) {
	var l Lock
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("parsing lock: %w", err)
	}
	return &l, nil
}

// policyHashPayload is the canonical, minimal representation of an
// EffectivePolicy hashed by PolicyHash. Keeping it separate from
// EffectivePolicy insulates the hash from unrelated field additions to the
// policy struct that don't change quarantine semantics.
type policyHashPayload struct {
	MinimumReleaseAge     string `json:"minimum_release_age"`
	MinimumObservationAge string `json:"minimum_observation_age"`
	AllowPrereleases      bool   `json:"allow_prereleases"`
}

// PolicyHash returns "sha256:<hex>" over the canonical serialization of an
// effective policy (§8 "policy_hash", §9 "Effective policy and its hash are
// recorded in lock and audit output"). Two EffectivePolicy values compare
// equal under PolicyHash iff their three fields are equal.
func PolicyHash(p EffectivePolicy) (string, error) {
	data, err := json.Marshal(policyHashPayload{
		MinimumReleaseAge:     p.MinimumReleaseAge.String(),
		MinimumObservationAge: p.MinimumObservationAge.String(),
		AllowPrereleases:      p.AllowPrereleases,
	})
	if err != nil {
		return "", fmt.Errorf("marshaling policy for hashing: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
