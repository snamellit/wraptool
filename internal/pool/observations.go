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
// refresh has happened yet).
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
	return o, nil
}

// Save writes observations.json atomically (§6.4).
func (o Observations) Save(path string) error {
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling observations: %w", err)
	}
	return WriteFileAtomic(path, data, 0o600)
}
