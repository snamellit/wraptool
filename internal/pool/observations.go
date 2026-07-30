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
//
// firstSeenAt and publishedAt must both parse as UTC RFC3339 (§6.6); Observe
// rejects a malformed value with an error and leaves o unchanged, rather than
// storing it. This is write-path validation, mirroring LoadObservations'
// read-path check, so this package can never itself produce an
// observations.json that a later LoadObservations would reject.
func (o Observations) Observe(key string, firstSeenAt, publishedAt string, prerelease, yanked bool) error {
	if _, err := ParseTimestamp(firstSeenAt); err != nil {
		return fmt.Errorf("observation %q: first_seen_at: %w", key, err)
	}
	if _, err := ParseTimestamp(publishedAt); err != nil {
		return fmt.Errorf("observation %q: published_at: %w", key, err)
	}
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
	return nil
}

// validate checks that every entry's first_seen_at and published_at parse as
// UTC RFC3339 (§6.6). Shared by LoadObservations (a file written by
// something other than this package, or hand-edited) and Save (so a caller
// that bypasses Observe by writing map entries directly still can't produce
// a file LoadObservations would later reject).
func (o Observations) validate() error {
	for key, obs := range o {
		if _, err := ParseTimestamp(obs.FirstSeenAt); err != nil {
			return fmt.Errorf("observation %q: first_seen_at: %w", key, err)
		}
		if _, err := ParseTimestamp(obs.PublishedAt); err != nil {
			return fmt.Errorf("observation %q: published_at: %w", key, err)
		}
	}
	return nil
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
		// Name the file and give a concrete remedy: unlike harness-lock.json
		// or manifest.yaml, observations.json is disposable pool-local cache
		// state (§9), so "delete it" is always a safe, actionable recovery —
		// the cost is only that first_seen_at quarantine timestamps restart
		// on the next refresh, not data loss.
		return nil, fmt.Errorf("%s is corrupt and could not be parsed as observations.json (%w) — delete it to reset observation quarantine (first-seen timestamps will restart)", path, err)
	}
	if o == nil {
		o = Observations{}
	}
	if err := o.validate(); err != nil {
		return nil, err
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
//
// Save re-validates every entry's timestamps before writing (the same check
// LoadObservations applies), so a map assembled by direct map writes rather
// than Observe can't reach disk in a state a later LoadObservations would
// reject.
func (o Observations) Save(path string) error {
	if err := o.validate(); err != nil {
		return err
	}
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
//
// MergeObservations is a load-modify-save read-modify-write cycle, not an
// atomic transaction by itself: a caller must hold PoolLock for the duration
// (§6.5), or two concurrent merges can race and one's update can be lost.
//
// If any entry in obs fails validation (a malformed first_seen_at or
// published_at), MergeObservations returns an error before calling Save, so
// the file on disk is left exactly as it was — a bad merge can never brick
// observations.json for every subsequent LoadObservations caller.
func MergeObservations(path string, obs Observations) error {
	existing, err := LoadObservations(path)
	if err != nil {
		return err
	}
	for key, incoming := range obs {
		if err := existing.Observe(key, incoming.FirstSeenAt, incoming.PublishedAt, incoming.Prerelease, incoming.Yanked); err != nil {
			return err
		}
	}
	return existing.Save(path)
}
