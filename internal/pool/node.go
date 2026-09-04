package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
)

// defaultNodeDistBaseURL is where nodejs.org publishes index.json — the
// same host internal/pool/source's NPM.Resolve fetches SHASUMS256.txt and
// runtime tarballs from (source/npm.go's defaultNodeDistBaseURL), but
// fetched from THIS package: refresh (the observe step, design/harness/
// node-runtime-tracking.md WP1) runs in internal/pool, not
// internal/pool/source, so this file needs its own copy of the constant
// rather than importing that package (which itself already imports this
// one — internal/pool/source depends on internal/pool, never the reverse).
const defaultNodeDistBaseURL = "https://nodejs.org/dist"

// defaultNodeIndexTimeout bounds one index.json request — a small JSON
// document, mirroring internal/pool/source's own metadataTimeout rationale
// (a client-level timeout keeps a stalled metadata connection from wedging
// whatever holds the pool's advisory lock, §6.5, around a refresh call
// indefinitely).
const defaultNodeIndexTimeout = 30 * time.Second

// defaultNodeIndexClient is the package-default HTTP client RefreshNodeLine
// uses when a caller's NodeOptions.Client is nil.
var defaultNodeIndexClient = &http.Client{Timeout: defaultNodeIndexTimeout}

// NodeOptions configures RefreshNodeLine's nodejs.org/dist/index.json fetch
// (design/harness/node-runtime-tracking.md WP1) — a per-call value, not a
// package-level var, mirroring internal/pool/source's own nodeDistBaseURL/
// *http.Client instance fields (source.NewNPM's doc comment) at THIS
// package's own call boundary: this package's own tests run under
// t.Parallel (many already do), where a shared package var would race
// between concurrently-running httptest fixture servers.
type NodeOptions struct {
	// BaseURL overrides nodejs.org's dist base URL (test seam); empty uses
	// the real "https://nodejs.org/dist".
	BaseURL string
	// Client overrides the HTTP client used for the index.json request
	// (test seam); nil uses a package-default client with a bounded
	// timeout.
	Client *http.Client
}

func (o NodeOptions) baseURL() string {
	if o.BaseURL == "" {
		return defaultNodeDistBaseURL
	}
	return strings.TrimRight(o.BaseURL, "/")
}

func (o NodeOptions) client() *http.Client {
	if o.Client == nil {
		return defaultNodeIndexClient
	}
	return o.Client
}

// nodeIndexEntry is the subset of one nodejs.org/dist/index.json array
// element this package reads — Version (e.g. "v22.23.2", always "v"-
// prefixed on the real feed) and Date (release date, "YYYY-MM-DD").
type nodeIndexEntry struct {
	Version string `json:"version"`
	Date    string `json:"date"`
}

// fetchNodeIndex performs the one HTTP GET RefreshNodeLine needs — pure Go,
// host-side, no backend (§6.2, mirroring internal/pool/source's own
// Refresh methods).
func fetchNodeIndex(ctx context.Context, opts NodeOptions) ([]nodeIndexEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.baseURL()+"/index.json", nil)
	if err != nil {
		return nil, fmt.Errorf("building node index request: %w", err)
	}
	resp, err := opts.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching node index: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching node index: unexpected status %s", resp.Status)
	}
	var entries []nodeIndexEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decoding node index: %w", err)
	}
	return entries, nil
}

// nodeObservationSubject returns the observations.json "subject" component
// for one Node major line's observations (design/harness/node-runtime-
// tracking.md WP1: "records it in the observations store under a
// `node:<line>` subject") — the "<source>/<package>" role §8.1's package
// observations use is filled here by "node:<line>" alone, since a node
// release has no separate package identity for a version to vary within,
// unlike an npm/archive package's PoolPackageID.
func nodeObservationSubject(line string) string {
	return "node:" + line
}

// nodeObservationKey builds the flat observations.json key for one node
// version on line: nodeObservationSubject(line) + "/" + version, mirroring
// §8.1's "<source>/<package>/<version>" shape with the subject standing in
// for "<source>/<package>".
func nodeObservationKey(line, version string) string {
	return nodeObservationSubject(line) + "/" + version
}

// nodeCandidates extracts every observed candidate on line out of the flat
// Observations map — the node-runtime counterpart of candidatesForPackage.
func nodeCandidates(obs Observations, line string) ([]Candidate, error) {
	return candidatesWithPrefix(obs, nodeObservationSubject(line)+"/")
}

// nodeVersionOnLine reports whether version's major segment equals line —
// the same comparison RefreshNodeLine's own filter uses when deciding which
// nodejs.org/dist entries belong to one line. Used by orchestrator.go's
// nodeVersionForRecipe fallback (review MAJOR-2): a locked node version from
// a DIFFERENT line (a descriptor's NodeLine moved, e.g. "22" -> "24") must
// never be treated as still matching the line currently requested, or a
// line move silently keeps building the OLD line with no output at all.
func nodeVersionOnLine(version, line string) bool {
	sv, err := semver.NewVersion(version)
	if err != nil {
		return false
	}
	return fmt.Sprint(sv.Major()) == line
}

// RefreshNodeLine fetches nodejs.org/dist/index.json (opts) and records
// every patch found on line into observations.json under the
// "node:<line>" subject (design/harness/node-runtime-tracking.md WP1 item
// 2), reusing the exact same write-once first_seen_at / Observe/
// MergeObservations machinery RefreshHarnessLocked already uses for package
// candidates (§6.6/§8.1) — node releases get the identical §9 quarantine
// treatment, not a parallel mechanism. Called for every npm-sourced
// harness's refresh (kind == SourceNPM only — an archive-sourced harness
// bundles no node runtime at all, this design's non-goals).
//
// Every patch on line is recorded, not merely the newest: an older patch's
// own already-stored first_seen_at (§6.6 write-once) must still be honored
// if it is ever re-observed, and selectNodeVersion needs the full set to
// find the newest ELIGIBLE one, which need not be the newest OBSERVED one.
//
// A malformed or missing release date is recorded with a zero PublishedAt
// rather than an invented one — Eligible already treats a zero PublishedAt
// as unconditionally ineligible (§9's safety net), mirroring
// internal/pool/source's own NPM.Refresh ("a missing/unparsable ... time
// entry yields a zero PublishedAt rather than an invented one").
func RefreshNodeLine(ctx context.Context, paths PoolPaths, opts NodeOptions, line string, now time.Time) ([]Candidate, error) {
	if line == "" {
		line = defaultNodeLine
	}

	entries, err := fetchNodeIndex(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("refreshing node line %s: %w", line, err)
	}

	// Loaded BEFORE the merge loop, deliberately — same reasoning as
	// RefreshHarnessLocked's own existing-observations read: first_seen_at
	// is write-once (§6.6), so a version already on record must keep its
	// own already-stored value.
	existing, err := LoadObservations(paths.Observations)
	if err != nil {
		return nil, fmt.Errorf("loading observations: %w", err)
	}

	toMerge := make(Observations)
	var out []Candidate
	for _, e := range entries {
		version := strings.TrimPrefix(e.Version, "v")
		sv, verr := semver.NewVersion(version)
		if verr != nil || fmt.Sprint(sv.Major()) != line {
			continue
		}

		var publishedAt time.Time
		if e.Date != "" {
			if t, perr := time.Parse("2006-01-02", e.Date); perr == nil {
				publishedAt = t
			}
		}

		key := nodeObservationKey(line, version)
		firstSeen := now
		if prior, ok := existing[key]; ok {
			if fsa, perr := ParseTimestamp(prior.FirstSeenAt); perr == nil {
				firstSeen = fsa
			}
		}

		if err := toMerge.Observe(key, FormatTimestamp(firstSeen), FormatTimestamp(publishedAt), sv.Prerelease() != "", false, false); err != nil {
			return nil, fmt.Errorf("recording node observation for %s: %w", version, err)
		}
		out = append(out, Candidate{Version: version, PublishedAt: publishedAt, FirstSeenAt: firstSeen, Prerelease: sv.Prerelease() != ""})
	}
	if err := MergeObservations(paths.Observations, toMerge); err != nil {
		return nil, fmt.Errorf("merging node observations: %w", err)
	}
	return out, nil
}

// selectNodeVersion picks the Node patch UpdateHarnessLocked passes to
// Source.Resolve for a SourceNPM harness pinned to line (design/harness/
// node-runtime-tracking.md WP2 item 3), reusing decision.go's
// scanCandidatesForConstraint with an empty constraint (line's own
// observations, from nodeCandidates, are already scoped to that one line —
// there is no further version constraint to apply) and mirroring
// selectNewestEligible's own contract EXACTLY — a node release gets the
// same §9 quarantine evaluation a package version does, no separate axis:
//
//   - the newest ELIGIBLE observed patch, when one exists;
//   - otherwise an error naming the newest OBSERVED-but-ineligible patch and,
//     when the ineligibility is purely a matter of waiting, how much longer
//     (QuarantineRemaining) — never silently bundling a quarantined,
//     prerelease, or zero-published_at patch just because §9's gate is
//     nominally "about downloading a version" (review finding MAJOR-1: that
//     reasoning made scan.newest an unaudited quarantine bypass — a node
//     patch published 2h ago could ship on the very first refresh after
//     issue #38's fix, and scan.newest ignores Prerelease entirely, making
//     AllowPrereleases unreachable for node);
//   - an error naming `wraptool harness refresh`, when NOTHING has ever
//     been observed for line (a fresh pool, or refresh never run) — the
//     MAJOR-2 guarantee (design/harness/node-runtime-tracking.md's Problem
//     section) that what reaches Source.Resolve is always an exact
//     published version, never a bare major, extends here: there being no
//     fallback constant left to reach for is precisely the point.
//
// No AllowYoung-style bypass exists for node (unlike the package axis):
// nothing here can audit a caller-supplied --reason against a single node
// release the way the package axis's request/approval flow does, so a
// quarantined node line is a hard error, full stop, until it clears policy
// naturally or minimum_observation_age/minimum_release_age is relaxed.
//
// The returned reason is a short, human-readable explanation for
// UpdateResult.NodeVersionNote.
func selectNodeVersion(obs Observations, line string, policy EffectivePolicy, now time.Time) (version, reason string, err error) {
	candidates, err := nodeCandidates(obs, line)
	if err != nil {
		return "", "", err
	}
	scan, err := scanCandidatesForConstraint(candidates, "", policy, now)
	if err != nil {
		return "", "", err
	}
	if scan.eligible != nil {
		return scan.eligible.cand.Version, fmt.Sprintf("newest eligible on node line %s", line), nil
	}
	if scan.newest == nil {
		return "", "", fmt.Errorf("no observed Node release for line %q; run `wraptool harness refresh` first", line)
	}
	if remaining, ok := QuarantineRemaining(scan.newest.cand, policy, now); ok {
		return "", "", fmt.Errorf("no eligible Node release on line %q: newest observed release %s is quarantined, %s remaining",
			line, scan.newest.cand.Version, remaining)
	}
	return "", "", fmt.Errorf("no eligible Node release on line %q: newest observed release %s is not eligible (yanked, or a disallowed prerelease)",
		line, scan.newest.cand.Version)
}
