package pool

import (
	"encoding/json"
	"fmt"
	"os"
)

// Observation is the locally recorded metadata for one candidate version
// (§8.1).
type Observation struct {
	FirstSeenAt string `json:"first_seen_at"` // UTC RFC3339, write-once (§6.6)
	PublishedAt string `json:"published_at"`  // UTC RFC3339, registry-supplied
	Prerelease  bool   `json:"prerelease"`
	Yanked      bool   `json:"yanked"`
}

// Observations is the flat "<source>/<package>/<version>" -> Observation map
// persisted at observations.json (§8.1).
type Observations map[string]Observation

// ObservationKey builds the flat map key for one candidate, matching the
// "<source>/<package>/<version>" format specified in §8.1.
func ObservationKey(source, pkg, version string) string {
	return source + "/" + pkg + "/" + version
}

// Observe records or updates the observation for key. If key is new,
// firstSeenAt becomes its permanent first_seen_at. If key already exists,
// the existing first_seen_at is kept regardless of the firstSeenAt argument —
// first_seen_at is never modified by a later merge (§6.6). published_at,
// prerelease, and yanked are always overwritten with the latest values, so a
// release that is later yanked or reclassified is reflected on the next
// refresh.
func (o Observations) Observe(key string, firstSeenAt, publishedAt string, prerelease, yanked bool) {
	fsa := firstSeenAt
	if existing, ok := o[key]; ok {
		fsa = existing.FirstSeenAt
	}
	o[key] = Observation{
		FirstSeenAt: fsa,
		PublishedAt: publishedAt,
		Prerelease:  prerelease,
		Yanked:      yanked,
	}
}

// LoadObservations reads observations.json. A missing file is not an error —
// it returns an empty Observations map, matching first-run behavior (no
// refresh has happened yet). Every entry's first_seen_at and published_at
// must parse as UTC RFC3339 (§6.6); a malformed value is rejected loudly
// rather than silently treated as unknown.
func LoadObservations(path string) (Observations, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Observations{}, nil
		}
		return nil, fmt.Errorf("reading observations: %w", err)
	}
	var o Observations
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("parsing observations: %w", err)
	}
	if o == nil {
		o = Observations{}
	}
	for key, obs := range o {
		if _, err := ParseTimestamp(obs.FirstSeenAt); err != nil {
			return nil, fmt.Errorf("observation %q: first_seen_at: %w", key, err)
		}
		if _, err := ParseTimestamp(obs.PublishedAt); err != nil {
			return nil, fmt.Errorf("observation %q: published_at: %w", key, err)
		}
	}
	return o, nil
}

// Save writes observations.json atomically (§6.4), whole-file: it replaces
// every prior key with exactly the contents of o. A caller that assembles a
// partial map (e.g. one refresh's candidates only, not the full history) and
// calls Save directly will silently drop every other key, including their
// first_seen_at — that breaks §6.6. Use MergeObservations for any partial
// update; Save is for a caller that already holds, and intends to persist
// unchanged, the complete map (this file's own tests, or a caller that just
// loaded the full set, mutated it in place, and writes it back).
func (o Observations) Save(path string) error {
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling observations: %w", err)
	}
	return WriteFileAtomic(path, data, 0o644)
}

// MergeObservations is the documented write path for a partial update — the
// shape every real caller has (one refresh's candidates, not the pool's
// entire observation history): it loads the existing observations.json
// (treating a missing file as empty), merges each entry of obs into it via
// Observe (preserving first_seen_at per §6.6), and atomically saves the
// result. Unlike calling Save directly with a partial map, no existing key or
// its first_seen_at is ever lost.
func MergeObservations(path string, obs Observations) error {
	existing, err := LoadObservations(path)
	if err != nil {
		return err
	}
	for key, incoming := range obs {
		existing.Observe(key, incoming.FirstSeenAt, incoming.PublishedAt, incoming.Prerelease, incoming.Yanked)
	}
	return existing.Save(path)
}
