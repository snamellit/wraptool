// drift.go implements the FIRST slice of design §7.6's drift.* group: the
// half of the catalog that CANNOT assert an absence. drift.tool-version-changed
// (context: why other drift findings may have appeared) and drift.unverifiable
// (honesty: what the group could not confirm, and why) ship alone in this
// step — drift.unknown-subcommand and drift.unknown-flag, the checks that DO
// assert an absence, are later steps layered on top of the probe/cache/report
// plumbing this file proves out first. The design's own ordering rationale
// (§"F1 before F2"): "ships the honest half first, so the group is useful and
// harmless before any absence claim exists."
//
// Decisions taken since design/doctor/implementation-plan.md was written,
// binding on this file:
//
//   - doctor calls internal/introspect DIRECTLY. There is no doctor-specific
//     probe path and no Prober seam on Input/Env: introspect's own exported
//     surface (LoadCacheStale, ProbeVersion, ProbeCatalog) already builds its
//     child environment via executor.ChildEnv itself (design §5.1). This is
//     the same "sanctioned sibling-package escape hatch" host.go's Env doc
//     comment already carves out for introspect.CacheStamp — the introspect
//     package is not in invariants_test.go's forbidden-selector table, so
//     TestChecksDoNotTouchTheRealHost does not (and must not) see through this.
//   - doctor NEVER calls Introspect or SaveCache — see driftFlagItems' doc
//     comment for why LoadCache (the validity-gated read) is not usable from
//     outside this package either, which leaves LoadCacheStale as the only
//     cache read doctor can perform, unconditionally.
//   - NeedsProbe (already reserved in finding.go) gates both checks here:
//     even though the flag-dimension half of drift.unverifiable only reads
//     the cache (no exec), the catalog-dimension half executes a live
//     ProbeCatalog, and design §7.6 states the WHOLE group is "skipped under
//     --no-probe" with no split behaviour — so both checks declare
//     NeedsConfig | NeedsProbe, uniformly.
package doctor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/introspect"
)

const (
	driftToolVersionChanged CheckID = "drift.tool-version-changed"
	driftUnverifiable       CheckID = "drift.unverifiable"
)

// driftToolVersionChangedMeta, driftUnverifiableMeta are named so
// drift_test.go's meta assertions read the SAME literal init() registers —
// never through Registered() (plan §1 rule 8).
var (
	driftToolVersionChangedMeta = Meta{
		ID:    driftToolVersionChanged,
		Needs: NeedsConfig | NeedsProbe,
		Summary: "reports when an installed tool's --version output differs from what was last cached — " +
			"context for why other drift findings may have appeared",
	}
	driftUnverifiableMeta = Meta{
		ID:    driftUnverifiable,
		Needs: NeedsConfig | NeedsProbe,
		Summary: "reports, tool by tool, which allow rules' subcommand/flag drift could not be confirmed " +
			"against the installed tool, and why",
	}
)

func init() {
	register(driftToolVersionChangedMeta, runDriftToolVersionChanged)
	register(driftUnverifiableMeta, runDriftUnverifiable)
}

// ---------------------------------------------------------------- drift.tool-version-changed

// runDriftToolVersionChanged compares each configured tool's CURRENTLY
// installed --version output (a live probe: introspect.ProbeVersion) against
// the version string recorded in its cached schema (introspect.LoadCacheStale
// — deliberately the STALE-tolerant read: a tool upgrade bumps the binary's
// own mtime, which is exactly what makes the validity-gated LoadCache reject
// the cache entry, discarding the one field this check needs, see
// LoadCacheStale's own doc comment).
//
// A tool with no cached schema at all, or a cached schema with no recorded
// version (--version itself failed at introspection time), or a live probe
// that produces no output, yields NO finding here — there is nothing to
// compare. That silence is not "did not look": a tool with genuinely no
// cached version is reported by drift.unverifiable instead (driftReasonNoCachedSchema),
// which is the honest, specific way to say "never probed" rather than
// stretching this check to cover it too.
func runDriftToolVersionChanged(ctx context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range toolSortedNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		if tc.Binary == "" {
			continue
		}

		cached, ok := introspect.LoadCacheStale(name)
		if !ok || cached.VersionOutput == "" {
			continue
		}

		current := introspect.ProbeVersion(ctx, &tc)
		if current == "" || current == cached.VersionOutput {
			continue
		}

		findings = append(findings, Finding{
			Severity: SeverityInfo,
			Title:    "installed tool version differs from the last cached introspection",
			Detail: fmt.Sprintf(
				"tools.%s's cached schema was built against version %q; the installed binary now reports %q. "+
					"This is context, not a problem: other drift findings for this tool reflect whichever "+
					"version was last probed, which may not be the one currently installed.",
				name, cached.VersionOutput, current),
			Path:   At("tools", name, "binary"),
			DocRef: "design/doctor/wraptool-doctor.md",
		})
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- drift.unverifiable

// driftMaxNamedItems bounds how many "tool subcommand" examples one finding
// names verbatim per reason bucket, mirroring policygenericMaxNamedSubcommands
// and poolMaxOffendingListed's identical reasoning: drift.unverifiable is the
// EXPECTED output for most tools (design §7.6), not an edge case, so a real
// config's allow rules must not turn this into the wall of text
// policy.unbounded-positional-count had to be rescued from — the count is
// always reported in full; only the enumerated examples are capped.
const driftMaxNamedItems = 5

// driftUnverifiableReason is a closed set naming EVERY way this step's data
// can fall short of supporting a subcommand- or flag-drift claim (design
// §7.6 + §5.3). Each reason is reported with its own count and a capped
// example list — never folded into one "could not verify" line with no
// cause, which is exactly the did-not-look silence the design forbids.
type driftUnverifiableReason string

const (
	// driftReasonMultiSegment: a rule's subcommand has more than one segment.
	// Matching is first-segment-only (§5.3) — a rule shaped [compute,
	// instances, list] can never be verified past "compute", regardless of
	// how exhaustive the catalog probe is.
	driftReasonMultiSegment driftUnverifiableReason = "multi-segment"
	// driftReasonNoCachedSchema: introspect.LoadCacheStale found nothing at
	// all for this tool. doctor never writes the cache (design §5.2), so on a
	// host where `wraptool serve` has never introspected this tool there is
	// nothing to read — that is unverifiable, not drift.
	driftReasonNoCachedSchema driftUnverifiableReason = "no-cached-schema"
	// driftReasonNotInCache: a cached schema exists for the TOOL, but this
	// specific subcommand key is absent from it — added to the allow list
	// since the cache was last built, most likely.
	driftReasonNotInCache driftUnverifiableReason = "not-in-cache"
	// driftReasonNotParsed: the cached Subcommand's ParseOutcome is not
	// ParseOutcomeParsed (help failed outright, or "succeeded" with garbage
	// that clears the byte threshold but isn't a real help page — see
	// ParseOutcome's own doc comment). Absence may only ever be asserted from
	// a genuine parse.
	driftReasonNotParsed driftUnverifiableReason = "not-parsed"
	// driftReasonCoverageUnmeasurable: the cached Subcommand's help text had
	// no option-shaped tokens at all (CoverageTokens == 0) to check the parse
	// against — not the same as "the parse captured everything".
	driftReasonCoverageUnmeasurable driftUnverifiableReason = "coverage-unmeasurable"
	// driftReasonLowCoverage: the parse captured fewer flag-shaped tokens
	// than the help text actually contained (CoverageFound < CoverageTokens)
	// — a signal the text itself supplies, independent of parser confidence
	// (measured: `git log -h` parses at the maximum 0.90 confidence while
	// missing --oneline/--graph/--pretty/--since/--author entirely).
	driftReasonLowCoverage driftUnverifiableReason = "low-coverage"
)

// driftReasonOrder is the fixed rendering order for driftBuildUnverifiableFinding
// — a slice, not a map range, so two runs over the same Input produce
// byte-identical Detail text (TestChecksAreDeterministic).
var driftReasonOrder = []driftUnverifiableReason{
	driftReasonMultiSegment,
	driftReasonNoCachedSchema,
	driftReasonNotInCache,
	driftReasonNotParsed,
	driftReasonCoverageUnmeasurable,
	driftReasonLowCoverage,
}

// driftReasonText is the human clause for each per-rule reason, completing
// "N allow rule(s) <text>".
var driftReasonText = map[driftUnverifiableReason]string{
	driftReasonMultiSegment: "have more than one subcommand segment — only the first is ever matched " +
		"against a catalog (§5.3)",
	driftReasonNoCachedSchema: "have never been introspected — there is no cached schema for this tool yet " +
		"(doctor never probes to build one; this fills in once `wraptool serve` has run against this config)",
	driftReasonNotInCache: "are missing from the last cached introspection (added to the config since, or " +
		"the cache predates them)",
	driftReasonNotParsed: "have help output that was not confirmed as a genuine, parsed help page",
	driftReasonCoverageUnmeasurable: "have help text with no flag-shaped tokens to measure the parse " +
		"against",
	driftReasonLowCoverage: "have help text the parse did not fully account for",
}

// driftItem is one (reason, example subcommand) pair collected while
// examining a tool's allow rules.
type driftItem struct {
	reason  driftUnverifiableReason
	subject string
}

// runDriftUnverifiable examines every configured tool with at least one
// allow rule (an inert tool, tools.inert's own subject, has nothing here to
// verify) along two INDEPENDENT axes — see driftCatalogReason (can this
// tool's subcommand catalog be trusted at all) and driftFlagItems (can each
// individual allow rule's flags be trusted) — and emits at most ONE finding
// per tool aggregating both. A tool where every axis comes back clean gets
// no finding at all: silence means checked-and-clean, and design §7.6 is
// explicit that MOST tools will not reach that state (only four tools carry
// a knowledge-table entry, and full flag coverage is the exception even for
// those).
func runDriftUnverifiable(ctx context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range toolSortedNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		if tc.Binary == "" || len(tc.Allow) == 0 {
			continue
		}
		if f, ok := driftToolUnverifiable(ctx, name, tc); ok {
			findings = append(findings, f)
		}
	}
	return Found(findings...)
}

// driftToolUnverifiable combines one tool's catalog-dimension reason (at
// most one, applies to the whole tool) and flag-dimension items (0..N, one
// per affected allow rule) into a single Finding, or reports ok == false
// when neither axis found anything to report.
func driftToolUnverifiable(ctx context.Context, name string, tc config.ToolConfig) (Finding, bool) {
	catalogReason := driftCatalogReason(ctx, name, tc)
	items := driftFlagItems(name, tc)
	if catalogReason == "" && len(items) == 0 {
		return Finding{}, false
	}
	return driftBuildUnverifiableFinding(name, len(tc.Allow), catalogReason, items), true
}

// driftCatalogReason answers whether this tool's SUBCOMMAND CATALOG can be
// trusted as exhaustive at all (design §5.3), independent of any specific
// allow rule: "" means yes (a genuine, non-empty, per-tool-vetted catalog
// probe succeeded); any other value is the specific reason it does not,
// named per the design's own bounding rule ("unknown", never a guess built
// from a partial source).
//
//   - no LookupToolKnowledge entry at all for this tool's basename: the
//     catalog is unknown, full stop (§7.8: "an unknown tool gets the generic
//     checks only").
//   - an entry exists but names no CatalogArgv: `-h`/`--help` alone is NOT a
//     catalog (git's `-h` omits stash/config/remote/worktree/rev-parse/
//     cherry-pick/submodule — verified on git 2.54, see introspect's
//     catalog.go), so without a per-tool-vetted argv there is nothing
//     trustworthy to probe.
//   - a live introspect.ProbeCatalog of that argv comes back !Exhaustive:
//     the probe failed, timed out, exited non-zero, or did not parse as a
//     genuine listing — every one of those can leave behind plausible-looking
//     partial output, which introspect's own CatalogProbe refuses to call
//     complete.
func driftCatalogReason(ctx context.Context, name string, tc config.ToolConfig) string {
	basename := ToolBasename(tc.Binary)
	tk, ok := LookupToolKnowledge(basename)
	switch {
	case !ok:
		return fmt.Sprintf(
			"%s (binary basename %s) has no per-tool knowledge-table entry, so its subcommand catalog is unknown",
			name, strconv.Quote(basename))
	case len(tk.CatalogArgv) == 0:
		return fmt.Sprintf("no catalog argv is known for %s (design §5.3)", strconv.Quote(basename))
	}

	probe := introspect.ProbeCatalog(ctx, &tc, tk.CatalogArgv)
	if !probe.Exhaustive {
		return fmt.Sprintf(
			"the subcommand catalog probe (%s) did not produce a listing that could be trusted as exhaustive "+
				"(the probe failed, timed out, exited non-zero, or its output did not parse as a genuine catalog)",
			strings.Join(tk.CatalogArgv, " "))
	}
	return ""
}

// driftFlagItems examines every one of tc's allow rules along the FLAG
// dimension: can this rule's flags be checked against the installed tool at
// all. It reads introspect.LoadCacheStale ONCE per tool (never LoadCache):
// LoadCache is validity-gated against a CacheValidity{ConfigHash, ...}, and
// ConfigHash is produced by introspect's own unexported configHash — doctor
// cannot recompute it and has no way to construct a CacheValidity that would
// ever genuinely match a real cache entry, so LoadCache is not a usable read
// from outside the package in practice (see the design decisions in this
// file's header comment: "the cached-only path isn't available"). This also
// means doctor cannot skip probing when a cache "looks fresh" — freshness
// itself is unobservable from here, which is why driftCatalogReason above
// probes unconditionally rather than trying to shortcut on cache state.
//
// A rule may appear in more than one bucket (e.g. a multi-segment rule that
// is ALSO missing from the cache) — that is two distinct, independently true
// facts about the same rule, not a double-count of one problem.
func driftFlagItems(name string, tc config.ToolConfig) []driftItem {
	var items []driftItem
	schema, cached := introspect.LoadCacheStale(name)

	for _, rule := range tc.Allow {
		subcmdKey := strings.Join(rule.Subcommand, " ")
		subject := strconv.Quote(name + " " + subcmdKey)

		if len(rule.Subcommand) > 1 {
			items = append(items, driftItem{reason: driftReasonMultiSegment, subject: subject})
		}

		if !cached {
			items = append(items, driftItem{reason: driftReasonNoCachedSchema, subject: subject})
			continue
		}

		sub, ok := schema.Subcommands[subcmdKey]
		switch {
		case !ok:
			items = append(items, driftItem{reason: driftReasonNotInCache, subject: subject})
		case sub.ParseOutcome != introspect.ParseOutcomeParsed:
			items = append(items, driftItem{reason: driftReasonNotParsed, subject: subject})
		case sub.CoverageTokens == 0:
			items = append(items, driftItem{reason: driftReasonCoverageUnmeasurable, subject: subject})
		case sub.CoverageFound < sub.CoverageTokens:
			items = append(items, driftItem{reason: driftReasonLowCoverage, subject: subject})
		}
	}
	return items
}

// driftCapList returns items with a bound applied (driftMaxNamedItems),
// plus the ", and N more" suffix naming the remainder — the same
// enumerate-some/count-all-of-them shape pool.go and policy_generic.go both
// already use.
func driftCapList(items []string) (named []string, suffix string) {
	named = items
	if len(named) > driftMaxNamedItems {
		suffix = fmt.Sprintf(", and %d more", len(named)-driftMaxNamedItems)
		named = named[:driftMaxNamedItems]
	}
	return named, suffix
}

// driftBuildUnverifiableFinding renders one tool's catalog-dimension reason
// (if any) and flag-dimension items (bucketed by reason, in driftReasonOrder)
// into a single Finding.
func driftBuildUnverifiableFinding(name string, ruleCount int, catalogReason string, items []driftItem) Finding {
	var sentences []string
	if catalogReason != "" {
		sentences = append(sentences, "subcommand catalog: "+catalogReason)
	}

	byReason := map[driftUnverifiableReason][]string{}
	for _, it := range items {
		byReason[it.reason] = append(byReason[it.reason], it.subject)
	}
	for _, reason := range driftReasonOrder {
		subjects := byReason[reason]
		if len(subjects) == 0 {
			continue
		}
		sort.Strings(subjects)
		named, suffix := driftCapList(subjects)
		sentences = append(sentences, fmt.Sprintf(
			"%d allow rule(s) %s (%s%s)",
			len(subjects), driftReasonText[reason], strings.Join(named, ", "), suffix))
	}

	return Finding{
		Severity: SeverityInfo,
		Title:    fmt.Sprintf("drift could not confirm all of %s's allow rules against the installed tool", name),
		Detail: fmt.Sprintf(
			"tools.%s has %d allow rule(s). %s.",
			name, ruleCount, strings.Join(sentences, "; ")),
		Path:   At("tools", name, "allow"),
		DocRef: "design/doctor/wraptool-doctor.md",
	}
}
