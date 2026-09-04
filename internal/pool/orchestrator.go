package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/uuid"

	"github.com/pti/wraptool/internal/pool/backend"
)

// npmSourceName is the fixed Observations/Lock "source" identity for every
// npm-sourced harness (§8.1). It is not exported: callers never need to
// spell it, they just name a package.
const npmSourceName = "npm"

// archiveSourceName is npmSourceName's counterpart for a direct-download
// ("archive") harness (§6.2, §8.1) — internal/pool/source's Archive
// implementation. Same visibility reasoning as npmSourceName.
const archiveSourceName = "archive"

// SourceKind names which pool.Source acquisition style an orchestrator call
// is driving — SourceNPM or SourceArchive (§8.1's Lock.Source values). It
// selects the Observations/Lock namespace prefix ("<kind>/<pkg>/<version>",
// §8.1) and the recipe_revision formula (npmRecipeRevision vs
// archiveRecipeRevision). This package's Source interface is structural,
// deliberately not internal/pool/source's concrete types (see that
// interface's own doc comment below), so it cannot infer a caller's source
// style at runtime by inspecting the Source value itself; the caller
// (cmd/harness.go, which does know internal/harness.Descriptor) states it
// explicitly via UpdateOptions.Source / RefreshHarness(Locked)'s and
// StatusHarness's own kind parameter.
type SourceKind string

const (
	SourceNPM     SourceKind = SourceKind(npmSourceName)
	SourceArchive SourceKind = SourceKind(archiveSourceName)
)

// effective normalizes a SourceKind, treating the zero value ("") as
// SourceNPM — UpdateOptions.Source's documented default (see its doc
// comment): every UpdateOptions literal written before that field existed,
// this package's own pre-existing tests among them, drove an npm Source, so
// the zero value preserves that behavior with no edits required. Positional
// parameters elsewhere in this package (RefreshHarnessLocked/RefreshHarness/
// StatusHarness's own kind argument) are NOT defaulted this way — a
// required parameter has no "unset" state to default from, so those callers
// must always pass an explicit SourceNPM/SourceArchive.
func (k SourceKind) effective() SourceKind {
	if k == "" {
		return SourceNPM
	}
	return k
}

// defaultNodeLine is the pool-wide default Node major line an npm-sourced
// harness resolves against when its descriptor's NodeLine
// (internal/harness.Descriptor.NodeLine) is empty (design/harness/
// node-runtime-tracking.md, WP1: "pin the line, not the patch" — replaces
// the old defaultNodeVersion exact-patch constant issue #38's interim fix
// introduced). The exact patch within this line is no longer a compile-time
// pin at all: selectNodeVersion resolves it at UPDATE time, from whatever
// patch RefreshNodeLine most recently observed on nodejs.org/dist and §9's
// quarantine currently admits — see both functions' doc comments. A var,
// not a const, only because it is read alongside other package-level state
// in this file; nothing outside a test ever reassigns it, and moving a
// harness (or this default) to a new major stays a deliberate code change,
// exactly as §-non-goals intends.
var defaultNodeLine = "22"

// recipeFormatVersion is bumped whenever this package's Phase-1 install
// recipe (the legacy npm-install-pinned-to-an-exact-version path, driven
// from cmd/harness.go) changes in a way that should invalidate a previously
// computed recipe_revision, so a resolved-and-locked generation can't be
// silently reused under changed realization assumptions (§8).
//
// 1 -> 2 (wave1-integration-fixes finding 1): this wave changed realization
// semantics — internal/pool/source's NPM.Realize now requires the embedded
// verbatim package-lock.json (NPMLock.Lockfile), which a Phase-1 lock (no
// such field) never carries. Without this bump, a Phase-1 pool is
// permanently wedged: install -> DecisionRealize (the stale Phase-1
// recipe_revision still "matches" under the OLD format, so the resolve axis
// looks fully agreed) -> NPM.Realize errors "lock predates the
// embedded-lockfile schema; re-run wraptool harness update" -> a later
// update short-circuits right back to DecisionRealize (nothing about
// in.RecipeRevision changed) -> the same error, forever — Resolve never
// runs, so the lockfile field is never backfilled. Bumping the format
// changes the freshly computed recipe_revision, so lockMatchesRecipe (§8,
// decision.go) becomes false, Decide instead returns
// DecisionResolveVersion, and the ensuing Resolve writes a lockfile-carrying
// entry under the new recipe_revision — a Phase-1 pool self-heals on its
// very next install/update.
//
// 2 -> 3 (oci-backend-wiring, §8/§11.2): the pinned pool-provisioning OCI
// image reference is now part of the hashed payload
// (UpdateOptions.ProvisioningImage, folded in by npmRecipeRevision below) —
// realization can now run inside that image (the oci backend) as well as
// inside guix, and §8 requires recipe_revision to cover "the backend
// provisioning-image digest". A wave-2-era lock has no provisioning image
// folded into its recipe_revision at all, so it can't simply be reinterpreted
// under the new formula; the format bump forces the same self-heal path as
// the 1->2 bump above — a stale recipe_revision no longer matches, Decide
// returns DecisionResolveVersion, and the ensuing Resolve+Fetch+Realize
// re-locks under the new, image-aware recipe_revision.
//
// 3 -> 4 (issue #38): defaultNodeVersion moved 22.11.0 -> 22.23.2 (the
// bundled runtime lacked node's zstd API, added in v22.15.0, crashing new
// pi releases). The node version is not part of the hashed payload, so
// without a format bump every existing lock still "matches" and keeps
// serving generations built on the old runtime forever. The bump forces the
// standard self-heal: Decide returns DecisionResolveVersion, Resolve runs
// with the new constant, and the next realize ships the new runtime.
//
// 4 -> 5 (design/harness/node-runtime-tracking.md WP2): the node version is
// now a genuine input to npmRecipeRevision (see its doc comment for the
// "would resolve to now" semantics) instead of an unhashed side channel, so
// a routine node-line bump on nodejs.org — no code change at all — makes
// recipe_revision itself change and drives the same self-heal every prior
// bump needed a format version for. A format-4-era lock's recipe_revision
// was computed with no node input whatsoever, so it cannot be reinterpreted
// under the new formula; the bump forces the same self-heal path as every
// entry above: Decide returns DecisionResolveVersion, Resolve re-resolves
// (picking up whatever node patch is currently eligible), and the next
// realize ships a generation actually built against that runtime.
const recipeFormatVersion = 5

// npmRecipeRevision computes recipe_revision (§8: "sha256 over the
// descriptor's serialized install recipe + the backend provisioning-image
// digest (§11.2) + a recipe-format version constant") for one npm package.
// provisioningImage is UpdateOptions.ProvisioningImage, folded in verbatim —
// see that field's doc comment for why it is included regardless of which
// backend (guix or oci) a given call actually realizes through. §16 Phase 1
// kept the actual harness install on the legacy, non-backend npm path;
// resolve/realize now do run inside a backend (this work package), so the
// image identity is a genuine input to the realization-environment contract,
// not a placeholder.
//
// nodeVersion (design/harness/node-runtime-tracking.md WP2) is the node
// patch the lock WOULD resolve to right now — the same value
// selectNodeVersion would hand to Source.Resolve on a fresh
// DecisionResolveVersion call, not merely whatever an existing lock happens
// to record — computed by UpdateHarnessLocked BEFORE Decide runs (deviation
// from §8's own inputs list becoming testable ordering: recipe_revision is
// itself one of Decide's inputs, so it must reflect the CURRENT would-
// resolve-to node version, not last time's). That is what gives a fresh
// eligible node release on nodejs.org — no package version change, no code
// change — the power to make lockMatchesRecipe false and trigger the
// standard resolve self-heal: the hashed payload changes the moment a newer
// eligible patch appears, precisely mirroring how a package version bump
// already drives DecisionResolveVersion via the version-selection axis.
// UpdateHarnessLocked falls back to the existing lock's own recorded node
// version when nothing can be selected right now (no node observation yet,
// e.g. a pool that has never run `wraptool harness refresh`) so a missing
// observation cannot manufacture a spurious recipe mismatch — see that
// function's own nodeVersionForRecipe local for the exact fallback.
func npmRecipeRevision(pkg, provisioningImage, nodeVersion string) (string, error) {
	data, err := json.Marshal(struct {
		Package           string `json:"package"`
		ProvisioningImage string `json:"provisioning_image"`
		NodeVersion       string `json:"node_version"`
		Format            int    `json:"recipe_format_version"`
	}{Package: pkg, ProvisioningImage: provisioningImage, NodeVersion: nodeVersion, Format: recipeFormatVersion})
	if err != nil {
		return "", fmt.Errorf("marshaling recipe for hashing: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// archiveRecipeFormatVersion is npmRecipeRevision's recipeFormatVersion
// counterpart for an archive-sourced harness — bumped whenever this
// package's archive realization assumptions change in a way that should
// invalidate a previously computed recipe_revision. Deliberately a SEPARATE
// constant from recipeFormatVersion: a recipe change to one source must not
// invalidate the other source's already-realized generations (§8's
// recipe_revision exists to guard exactly one source's assumptions at a
// time).
const archiveRecipeFormatVersion = 1

// archiveRecipeRevision computes recipe_revision (§8) for one archive
// harness — the archive-source counterpart of npmRecipeRevision. UNLIKE
// npm's version (which now folds in the provisioning image, oci-backend-
// wiring), archive resolution/realization has no backend provisioning image
// to fold in at all (internal/pool/source/archive.go's own doc comment:
// "EVERY method here is pure Go, host-side, and NEVER invokes ...
// backend.Backend"), so this is package identity + archiveRecipeFormatVersion
// only — it still changes the moment either input does.
func archiveRecipeRevision(pkg string) (string, error) {
	data, err := json.Marshal(struct {
		Package string `json:"package"`
		Format  int    `json:"archive_recipe_format_version"`
	}{Package: pkg, Format: archiveRecipeFormatVersion})
	if err != nil {
		return "", fmt.Errorf("marshaling archive recipe for hashing: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// recipeRevisionForSource computes recipe_revision (§8) for pkg under kind,
// dispatching to npmRecipeRevision or archiveRecipeRevision. provisioningImage
// and nodeVersion are passed through to npmRecipeRevision only —
// archiveRecipeRevision ignores both (archive resolve/realize invoke no
// backend and bundle no node runtime, §6.2/node-runtime-tracking.md
// non-goals). Exhaustive over SourceKind (golangci-lint's exhaustive check,
// .golangci.yml): a future SourceKind added without wiring its own recipe
// formula fails loudly here instead of silently reusing another source's
// revision.
func recipeRevisionForSource(kind SourceKind, pkg, provisioningImage, nodeVersion string) (string, error) {
	switch kind {
	case SourceNPM:
		return npmRecipeRevision(pkg, provisioningImage, nodeVersion)
	case SourceArchive:
		return archiveRecipeRevision(pkg)
	default:
		return "", fmt.Errorf("internal error: unknown pool source kind %q", kind)
	}
}

// Source is the local artifact-acquisition seam this package's command-level
// workflows call through (design/unified-harness-pool.md §11.3). Its method
// set is pinned to match internal/pool/source's npm Source implementation
// (a sibling work package built in parallel), so that concrete type satisfies
// this interface by Go's structural typing alone once both land — this file
// deliberately does not import internal/pool/source.
type Source interface {
	// Refresh lists candidate versions for pkg from registry metadata (host,
	// pure Go, no backend — §6.2). Every returned Candidate's FirstSeenAt is
	// the zero value; RefreshHarness fills it in from this pool's own
	// observations (§6.6: first_seen_at is pool-local, write-once state, not
	// something a registry can report).
	Refresh(ctx context.Context, pkg string) ([]Candidate, error)
	// Resolve produces the complete lock entry for one exact (pkg, version),
	// running inside b (registry-only network, §6.2/§8.1). workDir is a
	// scratch directory the Source may use for the synthesized package.json
	// / package-lock.json (§8.1 steps 1-3) — always non-empty (UpdateHarness
	// creates one under staging/ when the caller didn't supply one; the
	// Source seam never sees ""). nodeVersion and recipeRevision are supplied
	// by this package, not discovered by Source: nodeVersion is the EXACT,
	// published Node patch this harness resolves against for a SourceNPM
	// call — selectNodeVersion's choice, the newest observed patch on the
	// harness's Node line (UpdateOptions.NodeLine, default defaultNodeLine)
	// that §9's quarantine currently admits (design/harness/node-runtime-
	// tracking.md) — and is unused/ignored for a SourceArchive call (agy
	// bundles no node runtime). recipeRevision is this package's own recipe
	// hash (npmRecipeRevision, §8's recipe_revision).
	Resolve(ctx context.Context, b backend.Backend, pkg, version, workDir, nodeVersion, recipeRevision string) (LockHarness, error)
	// Fetch downloads every artifact h describes into cacheDir, verifying
	// integrity before admission (host, pure Go, locked URLs only — §6.2).
	// It MUTATES h in place: each npm package entry's pool-local SHA256
	// cache-address field is filled (existing ones re-verified) as objects
	// are admitted to cache, so UpdateHarness calls Fetch before persisting
	// h into harness-lock.json, never after.
	Fetch(ctx context.Context, h *LockHarness, cacheDir string) error
	// Realize builds one platform generation for harnessName (executable
	// binName) from h's locked artifacts, inside b with no network access
	// (§6.2 Realize phase, §11.4), unpacking into stagingGenDir (a fresh,
	// empty directory RealizeGeneration owns — the Source must not assume
	// anything about its prior contents beyond that). cacheDir is the same
	// flat, content-addressed cache Fetch admits objects into. The returned
	// Receipt describes stagingGenDir's contents (§11.5); RealizeGeneration
	// — never Source — is responsible for host-side verification, writing
	// receipt.json, and the atomic rename into
	// runtime/generations/<generation-id>/. This method's signature is
	// PINNED: internal/pool/source's npm Source implementation satisfies it
	// structurally, without this package importing that one.
	Realize(ctx context.Context, b backend.Backend, harnessName, binName string, h *LockHarness, key PlatformKey, cacheDir, stagingGenDir string) (*Receipt, error)
}

// PoolPaths bundles the pool file locations one orchestrator command
// operates on. Production callers build one from the real pool root's
// layout accessors (Root, ManifestFile, LockFile, ...); tests build one
// from t.TempDir() so no orchestrator test depends on the developer's real
// $XDG_DATA_HOME — mirroring how the rest of this package's tests pass
// explicit paths rather than calling Root() (see layout_test.go's siblings).
type PoolPaths struct {
	Manifest     string
	Lock         string
	Observations string
	Audit        string
	AdvisoryLock string
	// Cache MUST be the flat, content-addressed cache root passed straight
	// to Source.Fetch — pool.CacheSHA256Dir() (cache/sha256/<sha256hex>),
	// never pool.CacheDir() (cache/, one level too shallow). Source.Fetch
	// writes/looks up objects directly under this path by their sha256 hex
	// name; passing CacheDir() by mistake does not error visibly — it
	// silently makes every object permanently un-cached (each call re-hits
	// the download path, or later looks up a cache location that's
	// actually one directory off). There is deliberately no CacheDir field
	// alongside this one: a caller with both would have to know which is
	// "right" for Fetch, which is exactly the mistake this single,
	// precisely-named field exists to prevent.
	Cache string
	// Staging roots scratch work directories UpdateHarness creates itself
	// when a caller's UpdateOptions.WorkDir is empty (§5 layout:
	// staging/txn-<id>/, never mounted into project containers), and that
	// RealizeGeneration always creates itself for its own realize txn (it
	// takes no WorkDir override).
	Staging string
	// Runtime is the pool's runtime/ root — RealizeGeneration publishes
	// generations under Runtime/generations/<generation-id>/ and reads
	// existing ones back from there. Kept as an explicit field, like Cache
	// and Staging, so tests operate under a t.TempDir() root instead of the
	// developer's real pool (production wiring passes pool.RuntimeDir()).
	Runtime string
	// LegacyRemovedMarker is pool.LegacyRemovedMarkerFile() (production) or
	// a t.TempDir()-rooted equivalent (tests) — see that function's own doc
	// comment. Only GCOptions.Legacy's removal step (gc.go) writes it; every
	// other caller leaves this the zero value.
	LegacyRemovedMarker string
}

// LockPool acquires the pool's single exclusive advisory lock (§6.5) for
// paths and returns a release func the caller must defer-call exactly once
// when done. It exists so a cmd-level command spanning several orchestrator
// calls (e.g. `install`: write manifest intent, refresh, update, then run
// the legacy installer) acquires the pool lock ONCE for its whole span,
// rather than each call acquiring and releasing its own — critically,
// because github.com/gofrs/flock is not reentrant in-process: a second
// open-file-description flock on the same path from the same process blocks
// on the first, already-held one, so nesting two of this package's
// lock-acquiring calls deadlocks instead of composing. The exported
// RefreshHarness/UpdateHarness/SyncHarness each still acquire their own lock
// for a single, self-contained call; their *Locked siblings assume the
// caller already holds one obtained via LockPool and must never acquire
// another.
func LockPool(ctx context.Context, paths PoolPaths) (release func(), err error) {
	lock := NewPoolLock(paths.AdvisoryLock)
	if err := lock.Acquire(ctx); err != nil {
		return nil, err
	}
	return func() { _ = lock.Release() }, nil
}

// UpdateOptions configures one resolve+lock+fetch pass (§9 phases 2-3),
// shared by the `update` and `install` commands (§10) — the only two
// commands allowed to advance a harness's locked version. `install`
// additionally writes the manifest constraint before calling UpdateHarness;
// that step lives in cmd, not here (§7's manifest edit is a cmd-owned
// yaml.Node editor, kept out of this package per this work package's brief).
type UpdateOptions struct {
	// Harness is the manifest/lock map key, e.g. "claude".
	Harness string
	// Package is the source package identity, e.g. "@anthropic-ai/claude-code"
	// for an npm-sourced harness, or the harness's own name for an
	// archive-sourced one (internal/harness.Descriptor.PoolPackageID()).
	Package string
	// Source selects which pool.Source acquisition style this call is
	// driving — SourceNPM or SourceArchive (§8.1) — determining the
	// Observations/Lock namespace prefix and the recipe_revision formula
	// (see SourceKind's own doc comment). The zero value is SourceNPM (see
	// SourceKind.effective's doc comment): every UpdateOptions literal
	// written before this field existed drove an npm Source, so leaving it
	// unset preserves that behavior with no edits required.
	Source SourceKind
	// BinName is the harness's executable name inside a realized generation
	// (internal/harness.Descriptor.Bin), passed through to
	// RealizeGeneration/Source.Realize. cmd/harness.go is the only caller
	// that knows Descriptor; this package never imports internal/harness.
	BinName string
	// Now is the command's single clock read, threaded through instead of
	// calling time.Now() internally (§6.6, and this package's own test-
	// clock-injection convention).
	Now time.Time
	// WorkDir is passed through to Source.Resolve. Empty means
	// UpdateHarness creates and owns a scratch directory itself, under
	// paths.Staging, removed unconditionally when the call returns.
	WorkDir string
	// AllowYoung bypasses the release/observation age gates (§9 "Emergency
	// security updates"). Requires a non-empty Reason.
	AllowYoung bool
	Reason     string
	// Advance selects between install's and update's differing idempotence
	// semantics (§10: "update is the only normal command that advances a
	// floating constraint"). install passes false, update passes true.
	//
	//   - false (install): if the existing lock already matches
	//     opts.Package and the freshly computed recipe_revision, AND its
	//     version satisfies the manifest constraint, that is treated as
	//     fully realized intent — a pure no-op with NO candidate scan at
	//     all, even when a newer eligible version exists. Install's job is
	//     to realize existing intent, not to advance it.
	//   - true (update): always scans candidates first, regardless of
	//     whether the existing lock already matches. If the scan selects a
	//     version different from what's locked, that is an advance
	//     (resolve+fetch+lock). If it selects the SAME version as already
	//     locked (with matching package+recipe), that's a no-op for a
	//     different reason: nothing better to advance to. If nothing is
	//     eligible, the existing rules apply (keep an existing
	//     constraint-satisfying lock with a Note, or error if none exists).
	Advance bool
	// ProvisioningImage is the pinned pool-provisioning OCI image reference
	// (§11.2, cmd/harness.go's provisioningImage constant), folded into
	// npmRecipeRevision's hashed payload (§8: recipe_revision covers "the
	// backend provisioning-image digest"). Only meaningful for SourceNPM —
	// archiveRecipeRevision never consults it, since archive resolve/realize
	// invoke no backend at all (§6.2). The RULING behind this field is that
	// the image identity is folded regardless of which backend a given call
	// actually executes through (guix or oci): the recipe describes ONE
	// realization-environment contract, and §17's cross-backend receipt
	// equality requires a guix host and a Mac to agree on the same lock. So
	// cmd/harness.go always passes its provisioningImage constant here, even
	// on a call that resolves to use the guix backend.
	ProvisioningImage string
	// NodeLine is the npm-sourced harness's pinned Node major line
	// (internal/harness.Descriptor.NodeLine); empty means defaultNodeLine,
	// the pool-wide default. Only meaningful for SourceNPM — ignored for a
	// SourceArchive call, which never resolves a node version at all (agy
	// bundles no node runtime). On a fresh resolve (DecisionResolveVersion),
	// UpdateHarnessLocked passes selectNodeVersion's choice — the newest
	// ELIGIBLE observed patch on this line, §9's quarantine applied exactly
	// as it is to package versions — to Source.Resolve in place of the old
	// defaultNodeVersion constant (design/harness/node-runtime-tracking.md
	// WP1). See selectNodeVersion's doc comment for the fallback/error rules
	// when nothing is eligible.
	NodeLine string
}

// UpdateResult is UpdateHarness's outcome.
type UpdateResult struct {
	// Harness is the harness's lock entry after the call — freshly resolved
	// when Changed, otherwise whatever was already locked (kept as-is).
	Harness *LockHarness
	// Changed is true only when Source.Resolve/Fetch actually ran and a new
	// lock entry was written.
	Changed bool
	// Note is a human-readable explanation for a changed=false result that
	// isn't the plain "everything already agrees" case — specifically, a
	// kept lock because no candidate is currently eligible (§9). Empty
	// otherwise.
	Note string
	// AllowYoungApplied is true when opts.AllowYoung actually changed which
	// candidate got selected — i.e. the selected candidate would not have
	// been Eligible under the real, non-bypassed policy. Requesting
	// --allow-young when it wasn't needed leaves this false: "only an
	// effective bypass gets audited" (§9).
	AllowYoungApplied bool
	// CandidateAge and CandidateIntegrity are populated only alongside
	// AllowYoungApplied — now minus the selected candidate's published_at,
	// and the resolved package's upstream integrity value when known — for
	// the caller to print per §9 ("prints candidate age and integrity").
	CandidateAge       time.Duration
	CandidateIntegrity string
	// RealizedGenerationID is set whenever this call ran RealizeGeneration
	// for the host platform (DecisionRealize, a DecisionKeepExisting that
	// still needed realizing, or the unconditional realize after a fresh
	// DecisionResolveVersion) — the generation id RealizeGeneration
	// published or confirmed. Empty only for DecisionUpToDate, where
	// RealizeGeneration is never called because the platform was already
	// realized — there is nothing new to report. A caller must not infer
	// "nothing happened" from Changed=false alone: a DecisionRealize/
	// DecisionKeepExisting-needing-realize result also has Changed=false
	// (the LOCKED version didn't change) but DID build/confirm a
	// generation, which RealizedGenerationID surfaces.
	RealizedGenerationID string
	// RealizeErr is nil on success. It is set instead of UpdateHarnessLocked
	// returning the error (wave1-integration-fixes finding 2, §16 Phase 2
	// item 4 "legacy fallback retained"): dispatcher/activation/mounting are
	// a later wave, so the ONLY thing this wave's `install`/`update` actually
	// EXECUTE is the legacy npm-install path (cmd/harness.go's
	// maybeInstallLocked) — a Realize failure must not abort that. The
	// resolve+lock+fetch effects above (and, for update, any lock advance)
	// still happened and are reflected in Harness/Changed exactly as if
	// Realize had succeeded; only the realize attempt itself failed.
	// RealizedGenerationID is empty whenever this is set. The caller
	// (cmd/harness.go) is expected to print a clearly-labeled warning and
	// proceed to the legacy install regardless. SyncHarnessLocked does NOT
	// use this field — sync's whole purpose is realization, so a sync
	// failure stays a hard error returned normally.
	RealizeErr error
	// NodeVersionNote reports the Node runtime version selection made on a
	// fresh resolve (DecisionResolveVersion, SourceNPM only) whenever it
	// differs from the harness's previously locked node version — e.g.
	// "node runtime: 22.20.0 -> 22.23.2 (newest eligible on line 22)" (the
	// WP1 work-package brief's own requirement: "log/print which node
	// version was chosen and why when it differs from the lock's current
	// one" — design/harness/node-runtime-tracking.md itself does not spell
	// this out, WP1's brief added it). Empty when nothing changed, when
	// this call did not resolve at all, or for a SourceArchive harness.
	NodeVersionNote string
}

// RefreshHarness runs the metadata-only refresh phase (§9 phase 1, §6.2)
// under its own, self-contained pool lock acquisition. See RefreshHarnessLocked
// for the behavior; this wrapper exists for a caller that isn't already
// composing multiple orchestrator calls under one LockPool span.
func RefreshHarness(ctx context.Context, src Source, paths PoolPaths, kind SourceKind, pkg string, now time.Time) ([]Candidate, error) {
	release, err := LockPool(ctx, paths)
	if err != nil {
		return nil, err
	}
	defer release()
	return RefreshHarnessLocked(ctx, src, paths, kind, pkg, now)
}

// RefreshHarnessLocked is RefreshHarness's body, assuming the caller already
// holds the pool lock (via LockPool) — it never acquires one itself. It asks
// src for pkg's candidate versions, merges them into observations.json
// (first-seen stamped with now for keys observations.json has never seen
// before — MergeObservations/Observe's write-once rule, §6.6 — so a second
// refresh with a later now never moves an existing first_seen_at), and
// returns the candidates annotated with the FirstSeenAt/PublishedAt now on
// record (which, for a version seen on a prior refresh, are those earlier
// values, not this call's fresh ones). Refresh never touches the lock, and
// never calls Source.Resolve or Source.Fetch.
//
// PublishedAt synthesis (§9/§6.6): a candidate with no real publish-history
// timestamp — every candidate an archive Source returns
// (internal/pool/source/archive.go's Refresh doc comment: "this channel's
// manifest has no publish-history field ... this method deliberately does
// NOT invent one"), and occasionally an npm one missing registry "time"
// metadata (NPM.Refresh's own doc comment) — gets PublishedAt synthesized as
// FirstSeenAt: first observation can only ever be at or after actual
// publication, so it's a conservative proxy; for an archive-sourced harness
// this makes minimum_release_age effectively evaluated against
// first_seen_at, since no real published_at exists at all. Doing the
// synthesis HERE means pool.Eligible/QuarantineRemaining themselves need NO
// source-aware branch: by the time any candidate is ever visible to them, a
// zero PublishedAt has already been replaced by a real (if conservative)
// timestamp.
//
// CORRECTNESS-CRITICAL: unlike first_seen_at, published_at is NOT
// write-once (Observe overwrites it on every merge, by design — an upstream
// republish/reclassify must be reflected). If the synthesis above were fed
// THIS CALL's own `now` (or, worse, the local `firstSeen` variable when that
// itself defaulted to `now`), a repeated refresh of the same still-zero-
// PublishedAt candidate would restamp published_at forward every time,
// so `now - published_at` never grows and the version stays quarantined
// forever under any nonzero minimum_release_age. The fix: synthesis must be
// DETERMINISTIC across repeated refreshes, so it reads each candidate's own
// EXISTING first_seen_at (loaded below, BEFORE the merge loop) — which
// itself only ever gets set once, on first observation — and uses that
// (never this call's `now`) for both the candidate's FirstSeenAt and the
// synthesized PublishedAt. The correct framing is: first_seen_at is
// write-once; published_at, when synthesized, must be DERIVED from it
// deterministically so it does not drift with every refresh.
func RefreshHarnessLocked(ctx context.Context, src Source, paths PoolPaths, kind SourceKind, pkg string, now time.Time) ([]Candidate, error) {
	candidates, err := src.Refresh(ctx, pkg)
	if err != nil {
		return nil, fmt.Errorf("refreshing %s: %w", pkg, err)
	}

	// Loaded BEFORE the merge loop, deliberately: the PublishedAt synthesis
	// below must read each candidate's own ALREADY-STORED first_seen_at (if
	// any), not this call's `now` — see this function's own doc comment for
	// why using `now` there would make published_at drift forward on every
	// refresh instead of staying stable.
	existing, err := LoadObservations(paths.Observations)
	if err != nil {
		return nil, fmt.Errorf("loading observations: %w", err)
	}

	toMerge := make(Observations, len(candidates))
	for _, c := range candidates {
		key := ObservationKey(string(kind), pkg, c.Version)

		// firstSeen is `now` only the FIRST time this (source, package,
		// version) is ever observed (matching today's behavior); a version
		// already on record keeps its own already-stored first_seen_at
		// (§6.6 write-once) — read HERE, not left to MergeObservations'
		// later write-once check, because this value also feeds the
		// PublishedAt synthesis below, which must be deterministic.
		firstSeen := c.FirstSeenAt
		if firstSeen.IsZero() {
			firstSeen = now
		}
		if prior, ok := existing[key]; ok {
			if fsa, perr := ParseTimestamp(prior.FirstSeenAt); perr == nil {
				firstSeen = fsa
			}
		}

		// synthesized records whether THIS candidate's PublishedAt, as
		// returned by src.Refresh, was zero — the one place that actually
		// knows the fact, before it gets overwritten by the firstSeen proxy
		// below. Persisted verbatim (Candidate.PublishedAtSynthesized's doc
		// comment): re-deriving it later by comparing the stored
		// PublishedAt/FirstSeenAt for equality would misclassify a real
		// PublishedAt that legitimately coincides with FirstSeenAt.
		synthesized := c.PublishedAt.IsZero()
		publishedAt := c.PublishedAt
		if publishedAt.IsZero() {
			publishedAt = firstSeen
		}
		if err := toMerge.Observe(key, FormatTimestamp(firstSeen), FormatTimestamp(publishedAt), c.Prerelease, c.Yanked, synthesized); err != nil {
			return nil, fmt.Errorf("recording observation for %s@%s: %w", pkg, c.Version, err)
		}
	}
	if err := MergeObservations(paths.Observations, toMerge); err != nil {
		return nil, fmt.Errorf("merging observations: %w", err)
	}

	// Re-load so the returned candidates carry the true (possibly older, per
	// the write-once rule above) first_seen_at, and the true (possibly
	// synthesized, per the PublishedAt synthesis above) published_at, rather
	// than whatever src.Refresh returned.
	stored, err := LoadObservations(paths.Observations)
	if err != nil {
		return nil, fmt.Errorf("reloading observations: %w", err)
	}
	out := make([]Candidate, len(candidates))
	for i, c := range candidates {
		key := ObservationKey(string(kind), pkg, c.Version)
		if obs, ok := stored[key]; ok {
			if fsa, err := ParseTimestamp(obs.FirstSeenAt); err == nil {
				c.FirstSeenAt = fsa
			}
			if pa, err := ParseTimestamp(obs.PublishedAt); err == nil {
				c.PublishedAt = pa
			}
			// PublishedAtSynthesized (§9, wave2-integration-fixes item 1):
			// read from the stored observation, not re-derived — see
			// Candidate.PublishedAtSynthesized's doc comment for why
			// re-deriving it from the timestamps alone is unsound.
			c.PublishedAtSynthesized = obs.PublishedAtSynthesized
		}
		out[i] = c
	}
	return out, nil
}

// UpdateHarness runs the resolve+lock+fetch phases (§9 phases 2-3, §6.2) for
// one harness under its own, self-contained pool lock acquisition. See
// UpdateHarnessLocked for the behavior; this wrapper exists for a caller
// that isn't already composing multiple orchestrator calls under one
// LockPool span.
func UpdateHarness(ctx context.Context, src Source, b backend.Backend, paths PoolPaths, opts UpdateOptions) (*UpdateResult, error) {
	release, err := LockPool(ctx, paths)
	if err != nil {
		return nil, err
	}
	defer release()
	return UpdateHarnessLocked(ctx, src, b, paths, opts)
}

// UpdateHarnessLocked is UpdateHarness's body, assuming the caller already
// holds the pool lock (via LockPool) — it never acquires one itself.
//
// It is a thin imperative shell around decision.go's pure core: every
// clock/lock-file read happens here to assemble one DecisionInput, Decide is
// called exactly once, and the result determines which effects (if any) run
// below. opts.Advance selects between install's and update's differing
// idempotence semantics (§10: "update is the only normal command that
// advances a floating constraint" — see UpdateOptions.Advance's doc comment
// and Decide's doc comment for the full decision narrative):
//
//  1. install (Advance=false): if the pool's existing lock entry for
//     opts.Harness already matches opts.Package and the freshly computed
//     recipe_revision, AND its version still satisfies the manifest's
//     constraint, this is a pure no-op: changed=false, no candidates are
//     even scanned. This is §10's "install is idempotent when manifest,
//     lock, and recipe agree" — it does not care whether a NEWER version
//     would also be eligible; install realizes existing intent, it does not
//     go looking for something better.
//  2. update (Advance=true) ALWAYS scans first, so it notices a newer
//     eligible candidate or a widened constraint that now admits one. If the
//     scan selects a version different from what's locked (or the recipe
//     changed even at the same version), that's an advance: resolve, fetch,
//     write a new lock entry. If the scan selects the SAME version already
//     locked (package+recipe also unchanged), that's a no-op for a
//     different reason — changed=false, no Note (nothing to advance to).
//  3. In BOTH modes, when the scan finds nothing eligible: a lock that
//     already exists for this package and still satisfies the constraint is
//     kept rather than erroring (changed=false, with an informative Note
//     naming the newest quarantined candidate and its remaining wait, §9).
//     A hard error is returned only when there is no usable existing lock
//     AND nothing is eligible.
//
// When a fresh resolve does happen: it stamps FirstSeenAt (pool-local,
// unknown to Source) onto the resolved LockHarness, then calls Source.Fetch
// BEFORE writing the lock — deliberately not §9's textual phase order.
// Source.Fetch mutates its LockHarness argument in place, filling (and
// verifying) each artifact's pool-local sha256 cache-address field as it
// admits objects to cache (§8's "the pool's own sha256"); a lock persisted
// before Fetch runs would be missing those fields. Only once Fetch succeeds
// does UpdateHarnessLocked stamp PolicyHash/GeneratedAt onto the Lock,
// validate, and atomically write harness-lock.json — so a Fetch failure
// (including a partial one) leaves the previous lock file completely
// untouched, never a half-fetched resolution.
//
// opts.AllowYoung bypasses the release/observation age gates for candidate
// selection (§9's emergency bypass) and requires a non-empty opts.Reason.
// Only an "effective" bypass — one that actually changed which candidate got
// selected — is audited (appended to audit.jsonl, after the successful lock
// write) or reported via UpdateResult.AllowYoungApplied; requesting
// --allow-young when the normally-selected candidate was eligible anyway is
// a no-op bypass and neither prints nor audits anything extra.
//
// opts.WorkDir, when empty, is filled with a fresh staging/txn-<id> scratch
// directory that is always removed (success or failure) before returning —
// Source.Resolve is never called with an empty workDir.
//
// Every RealizeGeneration call this function makes (DecisionRealize, a
// DecisionKeepExisting that still needs realizing, or the unconditional
// realize after a fresh DecisionResolveVersion) treats a Realize failure as
// NON-FATAL: it is captured into the returned UpdateResult.RealizeErr rather
// than returned as this function's error (wave1-integration-fixes finding 2,
// §16 Phase 2 item 4 "legacy fallback retained" — dispatcher/activation/
// mounting are a later wave, so the legacy npm-install path driven from
// cmd/harness.go is the ONLY thing `install`/`update` actually execute this
// wave, and a realize failure must not prevent that from running). This is
// distinct from SyncHarnessLocked, whose entire purpose is realization and
// which still hard-fails on a Realize error in the ordinary way.
func UpdateHarnessLocked(ctx context.Context, src Source, b backend.Backend, paths PoolPaths, opts UpdateOptions) (*UpdateResult, error) {
	if opts.AllowYoung && opts.Reason == "" {
		return nil, fmt.Errorf("--allow-young requires a non-empty --reason")
	}

	m, err := LoadManifest(paths.Manifest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no pool manifest yet; run `wraptool harness install %s` first", opts.Harness)
		}
		return nil, err
	}
	hm, ok := m.Harnesses[opts.Harness]
	if !ok {
		return nil, fmt.Errorf("harness %q has no manifest entry; run `wraptool harness install %s` first", opts.Harness, opts.Harness)
	}
	policy := m.EffectivePolicy(opts.Harness)

	kind := opts.Source.effective()

	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		return nil, err
	}
	existing, hasLock := lockDoc.Harnesses[opts.Harness]

	obs, err := LoadObservations(paths.Observations)
	if err != nil {
		return nil, fmt.Errorf("loading observations: %w", err)
	}
	candidates, err := candidatesForPackage(obs, kind, opts.Package)
	if err != nil {
		return nil, err
	}

	// nodeVersion (design/harness/node-runtime-tracking.md WP2): computed
	// HERE, before recipeRevision, not merely inside the DecisionResolveVersion
	// branch below (WP1's original placement) — recipe_revision is itself one
	// of Decide's inputs, so it must fold in the node patch the lock WOULD
	// resolve to RIGHT NOW (see npmRecipeRevision's doc comment) for a fresh
	// eligible node release to make lockMatchesRecipe false and trigger the
	// standard resolve self-heal. Only SourceNPM ever resolves a node runtime
	// at all (SourceArchive/agy bundles none). nodeErr is deliberately not
	// returned yet: a pool that has never observed anything for this line
	// (no `wraptool harness refresh` run yet) must not fail every OTHER
	// decision axis (up-to-date, keep-existing, realize-only) just because
	// recipe_revision can't be computed with a fresh node input — it is
	// surfaced only if DecisionResolveVersion actually needs nodeVersion
	// below. nodeVersionForRecipe falls back to the existing lock's own
	// recorded node version in that case, so a missing observation cannot
	// manufacture a spurious recipe mismatch out of thin air — but ONLY when
	// that recorded version is still on the REQUESTED line (nodeVersionOnLine,
	// review MAJOR-2): without this guard, moving a harness's NodeLine (e.g.
	// "22" -> "24", the design's one deliberately-supported code change)
	// would keep recipeRevision computed from the OLD line's locked version,
	// `harness update` would report up-to-date, and the pool would silently
	// keep building the old line forever. line is hoisted out of the
	// SourceNPM block below so this check has it even when selectNodeVersion
	// itself was never reached for archive sources.
	line := opts.NodeLine
	if line == "" {
		line = defaultNodeLine
	}
	var nodeVersion, nodeReason string
	var nodeErr error
	if kind == SourceNPM {
		nodeVersion, nodeReason, nodeErr = selectNodeVersion(obs, line, policy, opts.Now)
	}
	nodeVersionForRecipe := nodeVersion
	if nodeErr != nil && hasLock && existing.Runtime != nil && nodeVersionOnLine(existing.Runtime.Version, line) {
		nodeVersionForRecipe = existing.Runtime.Version
	}

	recipeRevision, err := recipeRevisionForSource(kind, opts.Package, opts.ProvisioningImage, nodeVersionForRecipe)
	if err != nil {
		return nil, err
	}

	// The realize axis's two facts (§16 Phase 2): read once, here, exactly
	// like every other DecisionInput fact — Decide itself performs no I/O.
	// platformKey is always the HOST's own platform (HostPlatformKey, the
	// one helper §5.1 designates for this) for both install and update;
	// only `sync` lets a caller target a different platform (SyncHarnessLocked).
	platformKey, err := HostPlatformKey()
	if err != nil {
		return nil, err
	}
	var digestRecorded string
	if hasLock {
		digestRecorded = existing.Generations[platformKey]
	}
	dirPresent := false
	if digestRecorded != "" {
		if genID, genErr := GenerationID(opts.Harness, existing.Version, platformKey, digestRecorded); genErr == nil {
			if _, statErr := os.Stat(runtimeGenerationDir(paths.Runtime, genID)); statErr == nil {
				dirPresent = true
			}
		}
	}

	// npm-raw-lockfile-backfill field lesson: a lock persisted by a pre-fix
	// binary can carry a raw npm lockfile that is syntactically valid (loads
	// fine, validateNPMLockfile tolerates it) but semantically incomplete —
	// a raw entry missing integrity that realize-side `npm ci --offline`
	// cannot match by cache. false (the zero value) when there is nothing to
	// heal: no existing lock, or an existing entry with no NPMLock at all
	// (archive-sourced). See (*NPMLock).RawComplete's and
	// DecisionInput.LockNPMRawIncomplete's doc comments for the self-heal
	// this feeds into.
	rawIncomplete := false
	if hasLock && existing.NPMLock != nil {
		rawIncomplete = !existing.NPMLock.RawComplete()
	}

	// Decide validates hm.Constraint itself (it has to — the table test
	// exercises that failure directly), which is a deliberate, accepted
	// ordering change from the pre-decision-core code: an invalid manifest
	// constraint used to be checked before the lock/observations reads
	// above, so it won out over a corrupt lock or observations.json; now a
	// corrupt file read above wins instead, since it happens first. In
	// practice this is unreachable — LoadManifest already validates every
	// constraint (manifest.go's Validate) before UpdateHarnessLocked ever
	// runs — so no real caller can observe the precedence flip.
	decision := Decide(DecisionInput{
		Harness:                  opts.Harness,
		Package:                  opts.Package,
		Advance:                  opts.Advance,
		HasLock:                  hasLock,
		LockPackage:              existing.Package,
		LockVersion:              existing.Version,
		LockRecipeRevision:       existing.RecipeRevision,
		LockNPMRawIncomplete:     rawIncomplete,
		Constraint:               hm.Constraint,
		RecipeRevision:           recipeRevision,
		Candidates:               candidates,
		Policy:                   policy,
		AllowYoung:               opts.AllowYoung,
		Now:                      opts.Now,
		GenerationDigestRecorded: digestRecorded != "",
		GenerationDirPresent:     dirPresent,
	})

	switch decision.Kind {
	case DecisionUpToDate:
		out := existing
		result := &UpdateResult{Harness: &out, Changed: false}
		// Activation repair on the up-to-date path: a pool whose generation
		// was realized before activation existed (Phase 2 wave 1) is locked
		// AND realized — so Decide correctly reports up-to-date — but has no
		// pointer or dispatcher, and nothing else in this call would ever
		// write them. §10 makes the ACTIVE receipt part of install's
		// idempotence contract, so repair it here: activateIfNeeded no-ops
		// when the pointer already names this generation, and a failure is
		// non-fatal (RealizeErr) like every other realize-phase failure on
		// the install/update path.
		if generationRealized(digestRecorded != "", dirPresent) {
			genID, genErr := GenerationID(opts.Harness, out.Version, platformKey, digestRecorded)
			if genErr != nil {
				result.RealizeErr = genErr
				return result, nil
			}
			if active, ok := ActiveGeneration(paths, opts.Harness, platformKey); !ok || active != genID {
				if aerr := activateIfNeeded(paths, opts.Harness, platformKey, genID); aerr != nil {
					result.RealizeErr = aerr
				} else {
					result.Note = "activated " + genID
				}
			}
		}
		return result, nil
	case DecisionRealize:
		out := existing
		result := &UpdateResult{Harness: &out, Changed: false}
		// A Realize failure is non-fatal here (wave1-integration-fixes
		// finding 2, §16 Phase 2 item 4 "legacy fallback retained"): it is
		// captured into result.RealizeErr instead of aborting the call, so
		// the caller's legacy install still runs. See RealizeErr's doc
		// comment.
		receipt, rerr := RealizeGeneration(ctx, src, b, paths, opts.Harness, opts.BinName, &out, platformKey)
		if rerr != nil {
			result.RealizeErr = rerr
			return result, nil
		}
		genID, err := generationIDFromReceipt(platformKey, receipt)
		if err != nil {
			return nil, err
		}
		result.RealizedGenerationID = genID
		return result, nil
	case DecisionKeepExisting:
		out := existing
		result := &UpdateResult{Harness: &out, Changed: false, Note: decision.Note}
		// Decide does not fold the realize axis into DecisionKeepExisting
		// (decision.go's doc comment) — the shell checks the same facts it
		// already read above (via the same generationRealized predicate
		// Decide itself uses) and realizes here when needed, same as the
		// DecisionRealize case.
		if !generationRealized(digestRecorded != "", dirPresent) {
			// Non-fatal, same as the DecisionRealize case above — see
			// RealizeErr's doc comment.
			receipt, rerr := RealizeGeneration(ctx, src, b, paths, opts.Harness, opts.BinName, &out, platformKey)
			if rerr != nil {
				result.RealizeErr = rerr
				return result, nil
			}
			genID, err := generationIDFromReceipt(platformKey, receipt)
			if err != nil {
				return nil, err
			}
			result.RealizedGenerationID = genID
		}
		return result, nil
	case DecisionFail:
		return nil, decision.Err
	case DecisionResolveVersion:
		// Fall through to the resolve+fetch+persist effects below.
	default:
		// Fail closed: golangci-lint's exhaustive linter (default-signifies-
		// exhaustive: false, .golangci.yml) still requires every DecisionKind
		// to be named above, so this default is unreachable for any Decide
		// result today. It exists so a future DecisionKind added without an
		// explicit case here fails loudly at runtime instead of silently
		// falling through to the resolve+fetch+persist effects meant only
		// for DecisionResolveVersion.
		return nil, fmt.Errorf("internal error: unhandled decision kind %q", decision.Kind)
	}

	selected := decision.Selected
	allowYoungApplied := decision.AllowYoungApplied

	workDir := opts.WorkDir
	if workDir == "" {
		dir, cleanup, err := newScratchDir(paths.Staging)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		workDir = dir
	}

	// nodeVersion/nodeReason were already selected above (before recipeRevision
	// was computed, so a fresh eligible node release can drive this very
	// DecisionResolveVersion) — a SourceArchive call passes "" through unused
	// (Archive.Resolve ignores this parameter, agy bundles no node runtime).
	// The error selectNodeVersion could produce was deferred rather than
	// returned immediately up there (some other decision axis might not have
	// needed it at all); an actual resolve DOES need it, so it surfaces here.
	var nodeNote string
	if kind == SourceNPM {
		if nodeErr != nil {
			return nil, nodeErr
		}
		var priorNode string
		if hasLock && existing.Runtime != nil {
			priorNode = existing.Runtime.Version
		}
		if priorNode != nodeVersion {
			if priorNode == "" {
				nodeNote = fmt.Sprintf("node runtime: %s (%s)", nodeVersion, nodeReason)
			} else {
				nodeNote = fmt.Sprintf("node runtime: %s -> %s (%s)", priorNode, nodeVersion, nodeReason)
			}
		}
	}

	resolved, err := src.Resolve(ctx, b, opts.Package, selected.Version, workDir, nodeVersion, recipeRevision)
	if err != nil {
		return nil, fmt.Errorf("resolving %s@%s: %w", opts.Package, selected.Version, err)
	}
	resolved.FirstSeenAt = FormatTimestamp(selected.FirstSeenAt)
	// PublishedAt synthesis, resolve-time counterpart of RefreshHarnessLocked's
	// own (see that function's doc comment for the full rationale): an
	// archive Source's Resolve always returns a zero PublishedAt
	// (internal/pool/source/archive.go's Resolve doc comment — "there is no
	// real value to fill PublishedAt with at THIS stage either") because its
	// release channel carries no publish-history timestamp at all; an npm
	// Source's Resolve, by contrast, always returns a real one or fails
	// (NPM.Resolve's own doc comment), so this is a no-op for npm in
	// practice. (*Lock).Validate rejects a zero published_at outright
	// (validateLockTimestamp) — without this, an archive-sourced lock entry
	// could never be persisted at all. Parsed (not string-compared against a
	// formatted zero value) so a malformed published_at from a Source fails
	// loudly here instead of silently being treated as either zero or
	// non-zero.
	resolvedPublishedAt, err := ParseTimestamp(resolved.PublishedAt)
	if err != nil {
		return nil, fmt.Errorf("resolving %s@%s: published_at: %w", opts.Package, selected.Version, err)
	}
	if resolvedPublishedAt.IsZero() {
		resolved.PublishedAt = resolved.FirstSeenAt
	}

	// Fetch BEFORE writing the lock: it mutates resolved in place, filling
	// (and verifying) each artifact's pool-local sha256 cache-address field
	// as it admits objects to cache — a lock persisted before Fetch runs
	// would be missing the sha256 values §8 requires. A Fetch failure
	// (including a partial one, e.g. the second of three npm tarballs) must
	// leave the pool exactly as it was: since the previous lock file has not
	// been touched yet at this point, returning here does that for free —
	// no half-fetched resolution is ever persisted.
	if err := src.Fetch(ctx, &resolved, paths.Cache); err != nil {
		return nil, fmt.Errorf("fetching %s@%s: %w", opts.Package, selected.Version, err)
	}

	policyHash, err := PolicyHash(policy)
	if err != nil {
		return nil, err
	}
	lockDoc.SchemaVersion = LockSchemaVersion
	lockDoc.GeneratedAt = FormatTimestamp(opts.Now)
	lockDoc.PolicyHash = policyHash
	if lockDoc.Harnesses == nil {
		lockDoc.Harnesses = map[string]LockHarness{}
	}
	lockDoc.Harnesses[opts.Harness] = resolved
	if err := lockDoc.Validate(); err != nil {
		return nil, fmt.Errorf("validating lock: %w", err)
	}
	data, err := lockDoc.Marshal()
	if err != nil {
		return nil, err
	}
	if err := WriteFileAtomic(paths.Lock, data, 0o644); err != nil {
		return nil, err
	}

	result := &UpdateResult{Harness: &resolved, Changed: true, NodeVersionNote: nodeNote}

	// Only an effective bypass is audited — "only an effective bypass gets
	// audited" (§9 minor) — and only after the lock write above succeeded.
	// This MUST happen before RealizeGeneration below: a realize failure
	// must not be able to leave a quarantine-bypassed lock persisted with no
	// audit record. If it ran after and realize failed, the caller's retry
	// would find the lock already matches (DecisionUpToDate/DecisionRealize,
	// not DecisionResolveVersion) and this AllowYoung branch would never run
	// again — permanently losing the audit trail for an already-effective
	// bypass.
	var age time.Duration
	var integrity string
	if allowYoungApplied {
		age = opts.Now.Sub(selected.PublishedAt)
		if resolved.NPMLock != nil {
			if pkgEntry, ok := resolved.NPMLock.Packages[opts.Package]; ok {
				integrity = pkgEntry.Integrity
			}
		}
		if err := AppendAudit(paths.Audit, "allow_young", map[string]any{
			"harness":   opts.Harness,
			"package":   opts.Package,
			"version":   selected.Version,
			"reason":    opts.Reason,
			"age":       age.String(),
			"integrity": integrity,
		}); err != nil {
			return nil, fmt.Errorf("recording allow-young audit event: %w", err)
		}
		result.AllowYoungApplied = true
		result.CandidateAge = age
		result.CandidateIntegrity = integrity
	}

	// A freshly resolved version never has a recorded generation for
	// platformKey (it's a new lock entry), so realization always runs here —
	// Decide is not consulted for this outcome (decision.go's doc comment on
	// DecisionResolveVersion). Runs AFTER the allow_young audit above (see
	// its comment): the lock is already durably persisted and audited by
	// this point, so a realize failure here is retriable without losing
	// either. A Realize failure is non-fatal (wave1-integration-fixes finding
	// 2, RealizeErr's doc comment): the resolve+fetch+lock effects above have
	// already durably succeeded and are reported as such regardless of what
	// happens here.
	out := resolved
	result.Harness = &out
	receipt, rerr := RealizeGeneration(ctx, src, b, paths, opts.Harness, opts.BinName, &out, platformKey)
	if rerr != nil {
		result.RealizeErr = rerr
		return result, nil
	}
	genID, err := generationIDFromReceipt(platformKey, receipt)
	if err != nil {
		return nil, err
	}
	result.RealizedGenerationID = genID

	return result, nil
}

// generationIDFromReceipt computes the generation id a just-returned Receipt
// describes, for the caller (UpdateResult.RealizedGenerationID) to report —
// r.Harness/r.Version are already guaranteed (by RealizeGeneration's own
// pre-publish field-match guard) to agree with what was requested, so this
// is equivalent to recomputing from the caller's own (harnessName, version,
// key) inputs.
func generationIDFromReceipt(key PlatformKey, r *Receipt) (string, error) {
	return GenerationID(r.Harness, r.Version, key, r.ManifestDigest)
}

// newScratchDir creates a fresh staging/txn-<id> scratch directory (§5
// layout) for Source.Resolve to use when UpdateOptions.WorkDir is empty. The
// returned cleanup func removes it unconditionally; the caller MUST defer it
// immediately so it runs on both the success and failure paths — a scratch
// dir is disposable, never part of the pool's persistent state, and never
// mounted into a project container.
func newScratchDir(stagingRoot string) (dir string, cleanup func(), err error) {
	dir = filepath.Join(stagingRoot, "txn-"+uuid.NewString())
	if err := os.MkdirAll(dir, poolDirPerm); err != nil {
		return "", nil, fmt.Errorf("creating scratch dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// SyncHarness realizes the pool's exact existing lock entry for harnessName
// under its own, self-contained pool lock acquisition. See SyncHarnessLocked
// for the behavior; this wrapper exists for a caller that isn't already
// composing multiple orchestrator calls under one LockPool span.
func SyncHarness(ctx context.Context, src Source, b backend.Backend, paths PoolPaths, harnessName, binName string, key PlatformKey) (*LockHarness, error) {
	release, err := LockPool(ctx, paths)
	if err != nil {
		return nil, err
	}
	defer release()
	return SyncHarnessLocked(ctx, src, b, paths, harnessName, binName, key)
}

// SyncHarnessLocked is SyncHarness's body, assuming the caller already holds
// the pool lock (via LockPool) — it never acquires one itself. It never
// calls Source.Resolve (§9's Resolve phase is skipped entirely — sync
// "resolves NOTHING"), only Source.Fetch (to backfill any cache objects
// missing since the lock was written — new workstation, cache loss, §10) and
// then RealizeGeneration for key (§10: "a platform the lock already knows
// about but this pool hasn't realized yet" — key need not be this host's own
// platform; the caller, cmd/harness.go's `sync --platform`, decides which
// one). b runs the realize phase's Linux container step (§6.2) — sync skips
// Resolve but still needs a backend for Realize. Errors if no lock entry
// exists for harnessName; sync has nothing to realize from in that case.
func SyncHarnessLocked(ctx context.Context, src Source, b backend.Backend, paths PoolPaths, harnessName, binName string, key PlatformKey) (*LockHarness, error) {
	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		return nil, err
	}
	lh, ok := lockDoc.Harnesses[harnessName]
	if !ok {
		return nil, fmt.Errorf("no lock entry for harness %q; run `wraptool harness install %s` first", harnessName, harnessName)
	}
	if err := src.Fetch(ctx, &lh, paths.Cache); err != nil {
		return nil, fmt.Errorf("fetching %s: %w", harnessName, err)
	}
	if _, err := RealizeGeneration(ctx, src, b, paths, harnessName, binName, &lh, key); err != nil {
		return nil, err
	}
	return &lh, nil
}

// rollbackCandidate is one realized generation this package's rollback scan
// found for a given (harness, platform) pair, with just enough data to
// choose among them: which version it is, and how recently it was
// published into runtime/generations/ (used as "most recently created" when
// no --to version is given).
type rollbackCandidate struct {
	id      string
	version string
	modTime time.Time
}

// scanRollbackCandidates lists every generation under
// runtime/generations/<paths.Runtime> whose OWN receipt.json names
// (harnessName, key) — receipts are the authoritative source (§10's brief:
// "prefer reading receipt.json, ids are <harness>-<version>-<platform-key>-
// <digest8> but receipts are authoritative"), not a string-split of the
// generation id, which would be ambiguous whenever a harness name or a
// semver prerelease identifier itself contains a hyphen. A directory that
// isn't a valid generation (unreadable or unparseable receipt) is skipped,
// not an error — GC's own eventual cleanup, not rollback, is responsible for
// judging orphaned/corrupt directories.
func scanRollbackCandidates(paths PoolPaths, harnessName string, key PlatformKey) ([]rollbackCandidate, error) {
	entries, err := os.ReadDir(runtimeGenerationsDir(paths.Runtime))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing generations: %w", err)
	}

	var out []rollbackCandidate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		genDir := runtimeGenerationDir(paths.Runtime, entry.Name())
		info, err := os.Stat(genDir)
		if err != nil {
			continue
		}
		r, err := readGenerationReceipt(genDir)
		if err != nil {
			continue
		}
		if r.Harness != harnessName || r.Platform != key {
			continue
		}
		out = append(out, rollbackCandidate{id: entry.Name(), version: r.Version, modTime: info.ModTime()})
	}
	return out, nil
}

// RollbackHarness switches harnessName's active pointer for key back to a
// previously realized generation, without any network access or lockfile
// mutation (§10 "Switch the pointer to a retained generation without network
// access"), under its own, self-contained pool lock acquisition. See
// RollbackHarnessLocked for the behavior; this wrapper exists for a caller
// that isn't already composing multiple orchestrator calls under one
// LockPool span.
func RollbackHarness(ctx context.Context, paths PoolPaths, harnessName string, key PlatformKey, toVersion string) (generationID string, err error) {
	release, err := LockPool(ctx, paths)
	if err != nil {
		return "", err
	}
	defer release()
	return RollbackHarnessLocked(paths, harnessName, key, toVersion)
}

// RollbackHarnessLocked is RollbackHarness's body, assuming the caller
// already holds the pool lock (via LockPool) — it never acquires one
// itself. It performs no network access, calls no Backend, and never
// touches harness-lock.json (§10: rollback "resolves NOTHING" the way sync
// does — it only ever rewrites the active pointer, via ActivateGeneration,
// §5.2/§13's "default rollback switches the pointer only and preserves
// state").
//
// Candidate selection scans every generation already realized for
// (harnessName, key) via scanRollbackCandidates:
//
//   - with toVersion non-empty: the candidate(s) whose receipt version
//     equals toVersion exactly — version is NOT guaranteed unique to one
//     generation (e.g. two distinct realizes of the same version, with
//     different content, each get their own generation id), so ties are
//     broken by the newest generation directory (ModTime), same as the
//     no-toVersion case below. An error lists every OTHER version found (so
//     a typo'd version is immediately correctable) when toVersion matches
//     none of them.
//   - with toVersion empty: the candidate with the newest generation
//     directory (most recently created, by mtime) that is NOT the
//     currently active generation — rollback's whole point is to switch
//     to something else. An error names the situation when there is
//     nothing else retained to roll back to (only the active generation
//     itself was found, or nothing was found at all).
//
// Either way, the returned generationID (and the caller's printed output,
// cmd/harness.go) names the exact chosen generation, digest8 included —
// precise even when more than one generation shares the same version.
//
// On success it calls ActivateGeneration for the chosen generation (which
// itself appends a "generation_activated" audit event), then appends a
// SEPARATE "generation_rollback" audit event naming both the generation
// switched away from and the one switched to.
func RollbackHarnessLocked(paths PoolPaths, harnessName string, key PlatformKey, toVersion string) (generationID string, err error) {
	candidates, err := scanRollbackCandidates(paths, harnessName, key)
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no realized generation found for %s/%s to roll back to", harnessName, key)
	}

	// "" when nothing is active yet for this platform — a legitimate
	// rollback target selection still proceeds (there is simply nothing to
	// exclude as "currently active").
	currentID, _ := ActiveGeneration(paths, harnessName, key)

	var chosen *rollbackCandidate
	if toVersion != "" {
		for i := range candidates {
			c := &candidates[i]
			if c.version != toVersion {
				continue
			}
			// version is not guaranteed unique to one generation (two
			// distinct realizes of the same version can each get their own
			// generation id) — tie-break by the newest generation directory,
			// same ordering as the no-toVersion case below.
			if chosen == nil || c.modTime.After(chosen.modTime) {
				chosen = c
			}
		}
		if chosen == nil {
			seen := map[string]bool{}
			var versions []string
			for _, c := range candidates {
				if !seen[c.version] {
					seen[c.version] = true
					versions = append(versions, c.version)
				}
			}
			sort.Strings(versions)
			return "", fmt.Errorf("no realized generation for %s/%s at version %q; available: %s",
				harnessName, key, toVersion, strings.Join(versions, ", "))
		}
	} else {
		var best *rollbackCandidate
		for i := range candidates {
			c := &candidates[i]
			if c.id == currentID {
				continue
			}
			if best == nil || c.modTime.After(best.modTime) {
				best = c
			}
		}
		if best == nil {
			return "", fmt.Errorf("no other realized generation for %s/%s to roll back to (only the currently active generation is retained)", harnessName, key)
		}
		chosen = best
	}

	if err := ActivateGeneration(paths, harnessName, key, chosen.id); err != nil {
		return "", err
	}
	if err := AppendAudit(paths.Audit, "generation_rollback", map[string]any{
		"harness": harnessName, "platform": string(key), "from_generation": currentID, "to_generation": chosen.id,
	}); err != nil {
		return "", fmt.Errorf("recording generation_rollback audit event: %w", err)
	}
	return chosen.id, nil
}

// ManifestHarnessNames returns the harness names currently listed in the
// pool manifest — i.e. those with recorded developer intent — in
// unspecified order. A missing manifest returns an empty slice, not an
// error (mirrors loadManifestOrEmpty's read-path semantics). Used by cmd's
// bare `refresh`/`update` to iterate only harnesses someone actually asked
// the pool to manage, rather than every registry-known lockable harness —
// which would otherwise needlessly seed observations.json for packages
// nobody has installed.
func ManifestHarnessNames(path string) ([]string, error) {
	m, err := loadManifestOrEmpty(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m.Harnesses))
	for name := range m.Harnesses {
		names = append(names, name)
	}
	return names, nil
}

// LockHarnessNames returns the harness names currently present in
// harness-lock.json, in unspecified order. A missing lock returns an empty
// slice, not an error. Used by cmd's bare `sync` to iterate only harnesses
// that actually have an existing lock entry to realize from.
func LockHarnessNames(path string) ([]string, error) {
	l, err := loadLockOrEmpty(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(l.Harnesses))
	for name := range l.Harnesses {
		names = append(names, name)
	}
	return names, nil
}

// HarnessStatus is one harness's row of `wraptool harness list` (§10): the
// developer's desired constraint, what's locked, the newest known version
// satisfying that constraint, the newest of those that's actually eligible
// today, and — when the newest known version isn't eligible yet purely
// because of the age gates — that version and how much longer it must wait.
type HarnessStatus struct {
	Harness    string
	Constraint string // "" if the harness has no manifest entry yet
	Locked     string // "" if never locked
	Latest     string // "" if no observed candidate satisfies the constraint
	Eligible   string // "" if no observed candidate is both eligible and satisfies the constraint

	// QuarantinedVersion is set when Latest is not yet eligible purely due to
	// the age gates (i.e. Latest != Eligible and QuarantineRemaining reports
	// a meaningful countdown for it); "" otherwise, including when Latest ==
	// Eligible (nothing is quarantined) or Latest is empty.
	QuarantinedVersion  string
	QuarantineRemaining time.Duration
	// QuarantineOK mirrors pool.QuarantineRemaining's ok: false means
	// QuarantinedVersion carries a timestamp that makes it unconditionally
	// ineligible (no countdown to render), true means QuarantineRemaining is
	// a meaningful duration.
	QuarantineOK bool

	// Realized lists the platform keys the locked harness has a recorded
	// generation digest for (LockHarness.Generations), sorted, in unspecified
	// separator-free form — cmd/harness.go's `list` renders them
	// comma-joined as the REALIZED column (§16 Phase 2). Empty when never
	// locked or never realized for any platform. This one field reflects
	// only what the LOCK records, not a disk check — unlike ActiveGenerations
	// below, which DOES read runtime/ (pointer files, and — via
	// GenerationVersion — a generation's own receipt.json). Either way
	// `list` never mutates and never takes the pool lock (§6.5 "list takes
	// no lock").
	Realized []PlatformKey

	// ActiveGenerations maps a platform key to the generation id currently
	// active for this harness (runtime/active/<key>/<harness>.gen's content,
	// read via ActiveGeneration/ReadPointer), for whichever of the closed
	// PlatformKey values currently have a pointer file. A platform with no
	// pointer yet (never activated, or racing an in-flight activation) is
	// simply absent from the map — StatusHarness tolerates every
	// ActiveGeneration failure silently, the same lockless-read tolerance
	// Realized gets from a missing lock (§6.5 "list takes no lock ... must
	// tolerate a concurrent transaction"). cmd/harness.go's `list` renders
	// the ACTIVE column from this host platform's entry only, resolving the
	// generation id to a human version via GenerationVersion.
	ActiveGenerations map[PlatformKey]string
}

// StatusHarness computes one harness's list row (§10) from the pool's
// current manifest, lock, and observations — read-only, no pool lock taken
// (§6.5: "list takes no lock"). A missing manifest or lock file is treated
// as empty (nothing installed yet), not an error, so `list` works before any
// `install` has ever run. kind selects the Observations namespace this
// harness's candidates were recorded under (SourceNPM/SourceArchive, §8.1) —
// the caller (cmd/harness.go) knows the harness's Descriptor and states it
// explicitly; this package cannot infer it from pkg alone.
func StatusHarness(paths PoolPaths, harnessName string, kind SourceKind, pkg string, now time.Time) (HarnessStatus, error) {
	status := HarnessStatus{Harness: harnessName}

	// Lockless by construction (§6.5): ActiveGeneration is a single
	// best-effort ReadPointer per platform, tolerating every failure mode
	// (never activated, an in-flight rename, a missing active/<key> dir) as
	// "absent from the map" rather than an error.
	for _, key := range allPlatformKeys() {
		if id, ok := ActiveGeneration(paths, harnessName, key); ok {
			if status.ActiveGenerations == nil {
				status.ActiveGenerations = map[PlatformKey]string{}
			}
			status.ActiveGenerations[key] = id
		}
	}

	m, err := loadManifestOrEmpty(paths.Manifest)
	if err != nil {
		return status, err
	}
	if hm, ok := m.Harnesses[harnessName]; ok {
		status.Constraint = hm.Constraint
	}
	policy := m.EffectivePolicy(harnessName)

	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		return status, err
	}
	if lh, ok := lockDoc.Harnesses[harnessName]; ok {
		status.Locked = lh.Version
		for key := range lh.Generations {
			status.Realized = append(status.Realized, key)
		}
		sort.Slice(status.Realized, func(i, j int) bool { return status.Realized[i] < status.Realized[j] })
	}

	obs, err := LoadObservations(paths.Observations)
	if err != nil {
		return status, err
	}
	candidates, err := candidatesForPackage(obs, kind, pkg)
	if err != nil {
		return status, err
	}

	var constraintObj *semver.Constraints
	if status.Constraint != "" {
		constraintObj, err = semver.NewConstraint(status.Constraint)
		if err != nil {
			return status, fmt.Errorf("harness %q: invalid constraint %q: %w", harnessName, status.Constraint, err)
		}
	}

	var latest, eligibleBest *versionedCandidate
	for _, c := range candidates {
		v, verr := semver.NewVersion(c.Version)
		if verr != nil {
			continue
		}
		if constraintObj != nil && !constraintObj.Check(v) {
			continue
		}
		if c.Yanked {
			continue
		}
		if c.Prerelease && !policy.AllowPrereleases {
			continue
		}
		if latest == nil || v.GreaterThan(latest.ver) {
			latest = &versionedCandidate{c, v}
		}
		if Eligible(c, policy, now) && (eligibleBest == nil || v.GreaterThan(eligibleBest.ver)) {
			eligibleBest = &versionedCandidate{c, v}
		}
	}
	if latest != nil {
		status.Latest = latest.cand.Version
	}
	if eligibleBest != nil {
		status.Eligible = eligibleBest.cand.Version
	}
	if latest != nil && (eligibleBest == nil || latest.ver.GreaterThan(eligibleBest.ver)) {
		if remaining, ok := QuarantineRemaining(latest.cand, policy, now); ok {
			status.QuarantinedVersion = latest.cand.Version
			status.QuarantineRemaining = remaining
			status.QuarantineOK = true
		}
	}
	return status, nil
}

// candidatesForPackage extracts every observed candidate for pkg under
// namespace kind out of the flat Observations map, parsing each
// "<kind>/<pkg>/<version>" key back into a Candidate ready for
// Eligible/QuarantineRemaining. Matching is a literal prefix check against
// "<kind>/<pkg>/" — the trailing slash is load-bearing: without it, pkg
// "foo" would also match a stored key for an unrelated package "foo-bar"
// ("npm/foo-bar/1.0.0" starts with "npm/foo" but not "npm/foo/"). This is a
// literal string-prefix check, not a path split, so a scoped npm package
// name (which itself contains "/") is handled correctly too. kind is
// normalized via SourceKind.effective (the zero value means SourceNPM, the
// same convention UpdateOptions.Source uses) so a caller's zero-value kind
// can never silently mean "match nothing" instead of "npm".
func candidatesForPackage(obs Observations, kind SourceKind, pkg string) ([]Candidate, error) {
	return candidatesWithPrefix(obs, string(kind.effective())+"/"+pkg+"/")
}

// candidatesWithPrefix extracts every observation whose key starts with
// prefix out of the flat Observations map, parsing each into a Candidate
// ready for Eligible/QuarantineRemaining — the shared body behind
// candidatesForPackage's "<source>/<package>/" namespace (§8.1) and
// node.go's nodeCandidates' "node:<line>/" namespace (design/harness/
// node-runtime-tracking.md WP1). See candidatesForPackage's own doc comment
// for the trailing-slash-is-load-bearing rationale, which applies here
// unchanged regardless of which prefix shape a caller passes.
func candidatesWithPrefix(obs Observations, prefix string) ([]Candidate, error) {
	var out []Candidate
	for key, o := range obs {
		version, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		publishedAt, err := ParseTimestamp(o.PublishedAt)
		if err != nil {
			return nil, fmt.Errorf("observation %q: published_at: %w", key, err)
		}
		firstSeenAt, err := ParseTimestamp(o.FirstSeenAt)
		if err != nil {
			return nil, fmt.Errorf("observation %q: first_seen_at: %w", key, err)
		}
		out = append(out, Candidate{
			Version:     version,
			PublishedAt: publishedAt,
			FirstSeenAt: firstSeenAt,
			Prerelease:  o.Prerelease,
			Yanked:      o.Yanked,
			// Read straight from the stored observation — see
			// Candidate.PublishedAtSynthesized's doc comment for why this
			// must be the persisted fact, not re-derived from the
			// timestamps (§9, wave2-integration-fixes item 1).
			PublishedAtSynthesized: o.PublishedAtSynthesized,
		})
	}
	return out, nil
}

// loadManifestOrEmpty loads the manifest, treating a missing file as an
// empty one (no `install` has ever run) rather than an error — appropriate
// for read paths (StatusHarness, ManifestHarnessNames) that must work
// before any pool mutation. Mutating paths that need a real, already-created
// manifest (UpdateHarnessLocked) use LoadManifest directly and surface its
// error.
func loadManifestOrEmpty(path string) (*Manifest, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return &Manifest{SchemaVersion: ManifestSchemaVersion, Harnesses: map[string]HarnessManifest{}}, nil
		}
		return nil, fmt.Errorf("checking manifest: %w", err)
	}
	return LoadManifest(path)
}

// loadLockOrEmpty reads and validates harness-lock.json, treating a missing
// file as an empty lock (nothing resolved yet) rather than an error.
func loadLockOrEmpty(path string) (*Lock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Lock{SchemaVersion: LockSchemaVersion, Harnesses: map[string]LockHarness{}}, nil
		}
		return nil, fmt.Errorf("reading lock: %w", err)
	}
	return UnmarshalLock(data)
}
