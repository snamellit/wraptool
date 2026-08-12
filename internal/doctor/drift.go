// drift.go implements design §7.6's drift.* group. F1 shipped the half of
// the catalog that CANNOT assert an absence: drift.tool-version-changed
// (context: why other drift findings may have appeared) and drift.unverifiable
// (honesty: what the group could not confirm, and why). F2 (this file's
// second layer, see the "drift.unknown-subcommand" section below) adds the
// first check that DOES assert an absence — allow rules only; deny rules are
// F3's job and carry fail severity, per design §7.6's own row. The design's
// own ordering rationale (§"F1 before F2"): "ships the honest half first, so
// the group is useful and harmless before any absence claim exists."
//
// F2's own binding investigation, recorded here rather than only in a review
// thread (mirrors F1's "Rulings taken while F1 shipped" — this file's F2
// review round is recorded in design/doctor/wraptool-doctor.md §7.6 as
// "Rulings taken while F2 shipped"): see driftEligibleCatalogTools' and
// driftCatalogExcludedTools' own doc comments for why NONE of git,
// kubectl, gcloud, or terraform is trusted to support a subcommand-absence
// claim today — an ALLOWLIST, not a denylist of tools known to be risky
// (RULING 1) — and this file's own report (delivered alongside the branch)
// for the per-tool evidence in full.
//
// Decisions taken since design/doctor/implementation-plan.md was written,
// binding on this file:
//
//   - doctor calls internal/introspect DIRECTLY. There is no doctor-specific
//     probe path and no Prober seam on Input/Env: introspect's own exported
//     surface (LoadCacheStaleAt, ProbeVersion, ProbeCatalog) already builds
//     its child environment via executor.ChildEnv itself (design §5.1). This
//     is the same "sanctioned sibling-package escape hatch" host.go's Env doc
//     comment already carves out for introspect.CacheStamp — the introspect
//     package is not in invariants_test.go's forbidden-selector table, so
//     TestChecksDoNotTouchTheRealHost does not (and must not) see through this.
//   - doctor NEVER calls Introspect or SaveCache — see driftFlagItems' doc
//     comment for why LoadCache (the validity-gated read) is not usable from
//     outside this package either, which leaves LoadCacheStaleAt as the only
//     cache read doctor can perform, unconditionally.
//   - The cache read is ROOT-PARAMETERIZED (design §7.6 RULING 1, the F0 API
//     gap this file originally shipped with: introspect.LoadCacheStale
//     resolved through the real host's paths.CacheDir() regardless of what
//     Input carried, so doctor's own unit tests — and the shared,
//     Probe:true registry-wide invariants — read/wrote the DEVELOPER's real
//     introspection cache). Every read here goes through
//     introspect.LoadCacheStaleAt(in.Paths.CacheDir, name), never
//     introspect.LoadCacheStale.
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
	driftUnknownSubcommand  CheckID = "drift.unknown-subcommand"
)

// driftToolVersionChangedMeta, driftUnverifiableMeta, driftUnknownSubcommandMeta
// are named so drift_test.go's meta assertions read the SAME literal init()
// registers — never through Registered() (plan §1 rule 8).
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
		Summary: "reports, tool by tool, which allow/deny rules' subcommand/flag drift could not be confirmed " +
			"against the installed tool, and why",
	}
	driftUnknownSubcommandMeta = Meta{
		ID:    driftUnknownSubcommand,
		Needs: NeedsConfig | NeedsProbe,
		Summary: "warns when an ALLOW rule's subcommand is absent from a trustworthy, per-tool-vetted catalog " +
			"probe of the installed tool (§5.3; deny rules are F3's, at fail severity)",
	}
)

func init() {
	register(driftToolVersionChangedMeta, runDriftToolVersionChanged)
	register(driftUnverifiableMeta, runDriftUnverifiable)
	register(driftUnknownSubcommandMeta, runDriftUnknownSubcommand)
}

// ---------------------------------------------------------------- drift.tool-version-changed

// runDriftToolVersionChanged compares each configured tool's CURRENTLY
// installed --version output (a live probe: introspect.ProbeVersion) against
// the version string recorded in its cached schema
// (introspect.LoadCacheStaleAt, rooted at in.Paths.CacheDir — deliberately
// the STALE-tolerant read: a tool upgrade bumps the binary's own mtime, which
// is exactly what makes the validity-gated LoadCache reject the cache entry,
// discarding the one field this check needs, see LoadCacheStaleAt's own doc
// comment).
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

		cached, ok := introspect.LoadCacheStaleAt(in.Paths.CacheDir, name)
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

// driftUnverifiableReason is a CLOSED, iota-numbered set naming every way
// this step's data can fall short of supporting a subcommand- or flag-drift
// claim (design §7.6 + §5.3). It is deliberately NOT a string-const enum:
// driftReasonCount is a live sentinel that lets TestDriftReasonTableIsComplete
// (ruling 6) enumerate "every declared reason" by counting from zero, rather
// than hand-maintaining a second list that could itself silently drop the
// very entry a bug removes from driftReasonOrder or driftReasonText — the
// exact failure a reviewer produced by deleting one entry from
// driftReasonOrder ("tools.git has 1 allow rule(s). .", a finding with no
// cause at all). Each reason is reported with its own count and a capped
// example list — never folded into one "could not verify" line with no
// cause, which is exactly the did-not-look silence the design forbids.
type driftUnverifiableReason int

const (
	// driftReasonMultiSegment: a rule's subcommand has more than one segment.
	// Matching is first-segment-only (§5.3) — a rule shaped [compute,
	// instances, list] can never be verified past "compute", regardless of
	// how exhaustive the catalog probe is. Applies to both allow and deny
	// rules: both are matched the same first-segment-only way.
	driftReasonMultiSegment driftUnverifiableReason = iota
	// driftReasonNoCachedSchema: introspect.LoadCacheStaleAt found nothing at
	// all for this tool. doctor never writes the cache (design §5.2), so on a
	// host where `wraptool serve` has never introspected this tool there is
	// nothing to read — that is unverifiable, not drift. Allow rules only:
	// deny rules carry no flags and are never introspected into the cache in
	// the first place (Introspect ranges over tc.Allow only), so "no cached
	// schema" is not a fact about a deny rule specifically.
	driftReasonNoCachedSchema
	// driftReasonBinaryChanged: a cached schema exists, but Schema.Binary
	// (stamped at introspection time) names a DIFFERENT path than the tool's
	// currently configured binary. LoadCacheStaleAt performs no validity
	// check at all (design §5.2's own tradeoff), so without this the flag
	// checks below would silently trust a schema describing a different
	// program — the exact gap a reviewer measured: a config whose binary
	// points at a different file reporting the same --version output, with
	// drift saying nothing. An empty Schema.Binary (a hand-built or
	// pre-Binary-field cache entry) is treated as unknown, never a mismatch.
	driftReasonBinaryChanged
	// driftReasonCacheOlderThanBinary: the cached schema's IntrospectedAt
	// predates the currently configured binary's own mtime (Env.Stat) — the
	// binary was rebuilt or upgraded in place (same path, so
	// driftReasonBinaryChanged does not catch it) since the cache was last
	// written. Checked only when the binary can be stat'd at all; a missing
	// binary is drift.tool-version-changed's and driftCatalogAxis's own
	// concern, not reinvented here as a third reason for the same fact.
	driftReasonCacheOlderThanBinary
	// driftReasonNotInCache: a cached schema exists for the TOOL, and it is
	// trustworthy for the currently configured binary, but this specific
	// subcommand key is absent from it — added to the allow list since the
	// cache was last built, most likely.
	driftReasonNotInCache
	// driftReasonNotParsed: the cached Subcommand's ParseOutcome is not
	// ParseOutcomeParsed (help failed outright, or "succeeded" with garbage
	// that clears the byte threshold but isn't a real help page — see
	// ParseOutcome's own doc comment). Absence may only ever be asserted from
	// a genuine parse.
	driftReasonNotParsed
	// driftReasonCoverageUnmeasurable: the cached Subcommand's help text had
	// no option-shaped tokens at all (CoverageTokens == 0) to check the parse
	// against — not the same as "the parse captured everything".
	driftReasonCoverageUnmeasurable
	// driftReasonLowCoverage: the parse captured fewer flag-shaped tokens
	// than the help text actually contained (CoverageFound < CoverageTokens)
	// — a signal the text itself supplies, independent of parser confidence
	// (measured: `git log -h` parses at the maximum 0.90 confidence while
	// missing --oneline/--graph/--pretty/--since/--author entirely).
	driftReasonLowCoverage
	// driftReasonNoExhaustivenessAssertion: the parse achieved FULL coverage
	// (CoverageFound == CoverageTokens, and every case above already ruled
	// out) — the one case that, before this reason existed, silently fell
	// through the switch and produced NO item at all, reading as fully
	// flag-verifiable. Design §7.6 gates a flag-level absence claim on TWO
	// conditions: the coverage fraction AND a per-tool "help is exhaustive"
	// assertion — and no tool in the §7.8 knowledge table carries that
	// assertion field yet (it does not exist), so today NO tool can support
	// one, regardless of coverage. This reason says so, for every tool,
	// unconditionally, until a later step (F4) adds the table field and this
	// reason becomes conditional on it.
	driftReasonNoExhaustivenessAssertion
	// driftReasonCatalogNotVetted: the tool's subcommand catalog probe
	// succeeded and reported Exhaustive by drift.unverifiable's own lower bar
	// (driftCatalogAxis's reason came back ""), but the tool is not on
	// driftEligibleCatalogTools' allowlist, or its probed catalog did not
	// contain the eligible entry's own Sentinel (design §7.6 F2 RULINGS
	// 1-2) — the SAME two facts runDriftUnknownSubcommand itself gates its
	// own findings on (driftCatalogEligibilityReason, shared by both).
	// Allow rules only: this is specifically the "could a
	// subcommand-absence claim ever have been made for this rule" question
	// drift.unknown-subcommand answers for allow rules alone (deny rules are
	// F3's). Reported per rule rather than folded into driftCatalogAxis's
	// own tool-wide sentence so a deny-only tool — which
	// drift.unknown-subcommand never examines regardless of eligibility —
	// stays silent on this axis instead of reporting noise about a check
	// that was never going to run against it.
	driftReasonCatalogNotVetted
	// driftReasonCatalogSentinelMissing: the tool IS on the allowlist, but
	// this run's probed catalog did not contain the sentinel subcommand its
	// vetted catalog is expected to include (design §7.6 F2 RULING 2). That
	// is a different fact from "never vetted", and the operator needs the
	// difference: this one says the tool's catalog output changed shape or was
	// truncated on THIS run, which is worth investigating, whereas the other
	// says wraptool has simply never established completeness for this tool.
	driftReasonCatalogSentinelMissing

	// driftReasonCount is a sentinel, not a real reason: the number of
	// declared members above, used by driftAllReasons to enumerate the whole
	// set for TestDriftReasonTableIsComplete without a hand-maintained
	// parallel list. Anything appended above MUST stay above this line.
	driftReasonCount
)

// driftAllReasons enumerates every declared driftUnverifiableReason by
// counting from zero to the driftReasonCount sentinel — the completeness
// oracle ruling 6 requires: a reason struck from driftReasonOrder or
// driftReasonText is still IN this list, so the completeness test built on
// it catches the omission structurally rather than via a second table that
// could just as easily be edited to agree with the bug.
func driftAllReasons() []driftUnverifiableReason {
	out := make([]driftUnverifiableReason, 0, driftReasonCount)
	for r := driftUnverifiableReason(0); r < driftReasonCount; r++ {
		out = append(out, r)
	}
	return out
}

// driftReasonOrder is the fixed rendering order for driftBuildUnverifiableFinding
// — a slice, not a map range, so two runs over the same Input produce
// byte-identical Detail text (TestChecksAreDeterministic). Must name every
// member of driftAllReasons() exactly once (TestDriftReasonTableIsComplete).
var driftReasonOrder = []driftUnverifiableReason{
	driftReasonMultiSegment,
	driftReasonNoCachedSchema,
	driftReasonBinaryChanged,
	driftReasonCacheOlderThanBinary,
	driftReasonNotInCache,
	driftReasonNotParsed,
	driftReasonCoverageUnmeasurable,
	driftReasonLowCoverage,
	driftReasonNoExhaustivenessAssertion,
	driftReasonCatalogNotVetted,
	driftReasonCatalogSentinelMissing,
}

// driftReasonText is the human clause for each per-rule reason, completing
// "N <allow|deny> rule(s) <text>". Must have a non-empty entry for every
// member of driftAllReasons() (TestDriftReasonTableIsComplete).
var driftReasonText = map[driftUnverifiableReason]string{
	driftReasonMultiSegment: "have more than one subcommand segment — only the first is ever matched " +
		"against a catalog (§5.3)",
	driftReasonNoCachedSchema: "have never been introspected — there is no cached schema for this tool yet " +
		"(doctor never probes to build one; this fills in once `wraptool serve` has run against this config)",
	driftReasonBinaryChanged: "have a cached schema that was built from a different binary path than the one " +
		"currently configured — the cache describes a different program",
	driftReasonCacheOlderThanBinary: "have a cached schema older than the currently configured binary's own " +
		"modification time — the binary was likely rebuilt or upgraded in place since the cache was last written",
	driftReasonNotInCache: "are missing from the last cached introspection (added to the config since, or " +
		"the cache predates them)",
	driftReasonNotParsed: "have help output that was not confirmed as a genuine, parsed help page",
	driftReasonCoverageUnmeasurable: "have help text with no flag-shaped tokens to measure the parse " +
		"against",
	driftReasonLowCoverage: "have help text the parse did not fully account for",
	driftReasonNoExhaustivenessAssertion: "have full flag coverage against the cached help text, but no tool " +
		"yet carries the design §7.6 \"help is exhaustive\" assertion — until a later step adds it, full " +
		"coverage alone is not proof no other flags exist, for any tool",
	driftReasonCatalogNotVetted: "could have supported a subcommand-absence claim only if this tool's catalog " +
		"had been vetted as listing every subcommand it dispatches — it has not been (design §7.6 F2 RULING 1)",
	driftReasonCatalogSentinelMissing: "could have supported a subcommand-absence claim, but this run's probed " +
		"catalog did not contain the sentinel subcommand the vetted catalog is expected to include — the " +
		"listing looks truncated or reshaped, so it is not what was vetted (design §7.6 F2 RULING 2)",
}

// driftRuleKind distinguishes an allow rule from a deny rule in a
// driftItem/finding sentence — design §7.6: "drift.unknown-subcommand" is
// fail-severity for a deny rule specifically (a typo'd deny is fail-open),
// so its honest "could not tell" counterpart must say which kind of rule it
// is talking about, not silently fold deny rules into "allow rule(s)" text
// or drop them from consideration entirely.
type driftRuleKind string

const (
	driftKindAllow driftRuleKind = "allow"
	driftKindDeny  driftRuleKind = "deny"
)

// driftRuleKindOrder is the fixed rendering order for driftBuildUnverifiableFinding
// — allow before deny — mirrored for the same determinism reason as
// driftReasonOrder.
var driftRuleKindOrder = []driftRuleKind{driftKindAllow, driftKindDeny}

// driftItem is one (reason, rule kind, example subcommand) triple collected
// while examining a tool's allow and deny rules.
type driftItem struct {
	reason  driftUnverifiableReason
	kind    driftRuleKind
	subject string
}

// runDriftUnverifiable examines every configured tool with at least one
// allow OR deny rule along two INDEPENDENT axes — see driftCatalogAxis (can
// this tool's subcommand catalog be trusted at all) and driftFlagItems (can
// each individual rule be trusted) — and emits at most ONE finding per tool
// aggregating both. A tool with neither allow nor deny rules has nothing
// here to verify (it is tools.inert's own subject for the allow-rule case; a
// tool config with only deny rules and no allow rules is not tools.inert,
// but genuinely has zero rules of ANY kind for drift to examine only when
// both lists are empty). A tool where every axis comes back clean gets no
// finding at all: silence means checked-and-clean, and design §7.6 is
// explicit that MOST tools will not reach that state (only four tools carry
// a knowledge-table entry, and full flag coverage is the exception even for
// those — and, as of driftReasonNoExhaustivenessAssertion, full coverage
// alone no longer reaches that state either).
func runDriftUnverifiable(ctx context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range toolSortedNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		if tc.Binary == "" || (len(tc.Allow) == 0 && len(tc.Deny) == 0) {
			continue
		}
		if f, ok := driftToolUnverifiable(ctx, in, name, tc); ok {
			findings = append(findings, f)
		}
	}
	return Found(findings...)
}

// driftToolUnverifiable combines one tool's catalog-dimension reason (at
// most one, applies to the whole tool) and flag-dimension items (0..N, one
// per affected allow/deny rule) into a single Finding, or reports ok == false
// when neither axis found anything to report (or the resulting sentence list
// is somehow empty regardless — see driftBuildUnverifiableFinding).
func driftToolUnverifiable(ctx context.Context, in *Input, name string, tc config.ToolConfig) (Finding, bool) {
	catalogReason, catalogItems := driftCatalogAxis(ctx, name, tc)
	items := driftFlagItems(in, name, tc)
	items = append(items, catalogItems...)
	return driftBuildUnverifiableFinding(name, len(tc.Allow), len(tc.Deny), catalogReason, items)
}

// driftCatalogAxis answers this tool's SUBCOMMAND CATALOG question along two
// INDEPENDENT facts, mirroring driftFlagItems' tool-wide-then-per-rule
// shape:
//
//   - reason (tool-wide, F1's original bar, UNCHANGED by the F2 ruling
//     round below): can this tool's catalog be trusted as a genuine,
//     non-empty, per-tool-vetted PARSE at all (design §5.3)? "" means yes;
//     any other value is why not — no LookupToolKnowledge entry, no
//     CatalogArgv, or a live introspect.ProbeCatalog that came back
//     !Exhaustive (the probe failed, timed out, exited non-zero, or did not
//     parse as a genuine listing — every one of those can leave behind
//     plausible-looking partial output, which introspect's own CatalogProbe
//     refuses to call complete).
//   - items (per ALLOW rule, NEW in design §7.6's F2 ruling round): even
//     when reason == "" — the parse itself is genuine — can it support a
//     subcommand-ABSENCE claim specifically? driftCatalogEligibilityReason
//     answers this, shared verbatim with runDriftUnknownSubcommand's own
//     gate so the two checks can never drift text or logic apart (mirroring
//     driftCacheTrustReason, already shared the same way for the
//     cache-trust axis). Reported per rule, deny rules excluded
//     (drift.unknown-subcommand never examines them), and ONLY when
//     reason == "" — a tool whose parse itself failed is already fully
//     explained by reason, and a second, derivative note here would
//     double-report the same underlying fact.
func driftCatalogAxis(ctx context.Context, name string, tc config.ToolConfig) (reason string, items []driftItem) {
	basename := ToolBasename(tc.Binary)
	tk, ok := LookupToolKnowledge(basename)
	switch {
	case !ok:
		return fmt.Sprintf(
			"%s (binary basename %s) has no per-tool knowledge-table entry, so its subcommand catalog is unknown",
			name, strconv.Quote(basename)), nil
	case len(tk.CatalogArgv) == 0:
		return fmt.Sprintf("no catalog argv is known for %s (design §5.3)", strconv.Quote(basename)), nil
	}

	probe := introspect.ProbeCatalog(ctx, &tc, tk.CatalogArgv)
	if !probe.Exhaustive {
		return fmt.Sprintf(
			"the subcommand catalog probe (%s) did not produce a listing that could be trusted as exhaustive "+
				"(the probe failed, timed out, exited non-zero, or its output did not parse as a genuine catalog)",
			strings.Join(tk.CatalogArgv, " ")), nil
	}

	// Which of the two eligibility facts held decides which reason the operator
	// sees: RULING 2 asks for the sentinel mismatch to be reported AS that
	// reason, and a static "A or B" sentence would hide the case worth acting
	// on (a catalog that changed shape on this run) behind the case that is
	// merely a standing limitation.
	if reason, ok := driftCatalogEligibilityFailure(basename, probe); ok {
		for _, rule := range tc.Allow {
			subject := strconv.Quote(name + " " + strings.Join(rule.Subcommand, " "))
			items = append(items, driftItem{reason: reason, kind: driftKindAllow, subject: subject})
		}
	}
	return "", items
}

// driftCacheTrustReason answers whether a TOOL's cached schema — once it
// exists at all — can still be trusted to describe the CURRENTLY configured
// binary, independent of any single rule (design §7.6 RULING 3).
// introspect.LoadCacheStaleAt performs no validity check whatsoever (that is
// its whole point, for drift.tool-version-changed's sake), so a rule-by-rule
// read on top of it would otherwise silently believe a schema describing a
// DIFFERENT binary, or one older than the binary it now names — exactly the
// gap a reviewer measured: a config whose binary points at a different file
// reporting the same --version output, with drift.unverifiable saying
// nothing.
//
// A binary path change is checked first via Schema.Binary (stamped at
// introspection time by Introspect) against the tool's currently configured
// Binary — an empty Schema.Binary (a hand-built or pre-Binary-field cache
// entry) is treated as unknown, never a mismatch, since it carries no signal
// either way. Staleness is checked only when env.Stat can resolve the
// currently configured binary at all; a binary that cannot be stat'd is
// drift.tool-version-changed's and driftCatalogAxis's own concern, not
// reinvented here as a third reason for the same underlying fact.
func driftCacheTrustReason(env Env, tc config.ToolConfig, schema *introspect.Schema) (driftUnverifiableReason, bool) {
	if schema.Binary != "" && schema.Binary != tc.Binary {
		return driftReasonBinaryChanged, true
	}
	if fi, err := env.Stat(tc.Binary); err == nil && fi.ModTime.After(schema.IntrospectedAt) {
		return driftReasonCacheOlderThanBinary, true
	}
	// driftReasonCount, never a bare 0: 0 is driftReasonMultiSegment, a legal
	// member, so a caller that dropped the bool would silently report every
	// trusted tool's rules as multi-segment -- a plausible-looking claim the
	// data does not support, which no test would catch. The sentinel has no
	// text, and driftBuildUnverifiableFinding already refuses a finding whose
	// sentences came out empty, so a dropped bool degrades into silence
	// instead.
	return driftReasonCount, false
}

// driftFlagItems examines every one of tc's allow AND deny rules. Allow
// rules are examined along the FLAG dimension: can this rule's flags be
// checked against the installed tool at all. Deny rules carry no flags
// (config.DenyRule has only Subcommand) and are never introspected into the
// cache (Introspect ranges over tc.Allow only), so a deny rule is examined
// only for the one thing that applies to it: whether its subcommand can even
// be matched past the first segment (§5.3) — its catalog-membership
// trustworthiness is driftCatalogAxis's tool-wide concern, reported once,
// not duplicated per deny rule here.
//
// It reads introspect.LoadCacheStaleAt ONCE per tool, rooted at
// in.Paths.CacheDir (never introspect.LoadCacheStale, which resolves the
// REAL host's cache dir regardless of Input — design §7.6 RULING 1 — and
// never LoadCache): LoadCache is validity-gated against a
// CacheValidity{ConfigHash, ...}, and ConfigHash is produced by introspect's
// own unexported configHash — doctor cannot recompute it and has no way to
// construct a CacheValidity that would ever genuinely match a real cache
// entry, so LoadCache is not a usable read from outside the package in
// practice (see this file's header comment: "the cached-only path isn't
// available"). This also means doctor cannot skip probing when a cache
// "looks fresh" — freshness itself is unobservable from here, which is why
// driftCatalogAxis above probes unconditionally rather than trying to
// shortcut on cache state.
//
// A rule may appear in more than one bucket (e.g. a multi-segment allow rule
// that is ALSO missing from the cache) — that is two distinct, independently
// true facts about the same rule, not a double-count of one problem.
func driftFlagItems(in *Input, name string, tc config.ToolConfig) []driftItem {
	var items []driftItem
	schema, cached := introspect.LoadCacheStaleAt(in.Paths.CacheDir, name)

	var trust driftUnverifiableReason
	var untrusted bool
	if cached {
		trust, untrusted = driftCacheTrustReason(in.Env, tc, schema)
	}

	for _, rule := range tc.Allow {
		subcmdKey := strings.Join(rule.Subcommand, " ")
		subject := strconv.Quote(name + " " + subcmdKey)

		if len(rule.Subcommand) > 1 {
			items = append(items, driftItem{reason: driftReasonMultiSegment, kind: driftKindAllow, subject: subject})
		}

		if !cached {
			items = append(items, driftItem{reason: driftReasonNoCachedSchema, kind: driftKindAllow, subject: subject})
			continue
		}
		if untrusted {
			items = append(items, driftItem{reason: trust, kind: driftKindAllow, subject: subject})
			continue
		}

		sub, ok := schema.Subcommands[subcmdKey]
		switch {
		case !ok:
			items = append(items, driftItem{reason: driftReasonNotInCache, kind: driftKindAllow, subject: subject})
		case sub.ParseOutcome != introspect.ParseOutcomeParsed:
			items = append(items, driftItem{reason: driftReasonNotParsed, kind: driftKindAllow, subject: subject})
		case sub.CoverageTokens == 0:
			items = append(items, driftItem{reason: driftReasonCoverageUnmeasurable, kind: driftKindAllow, subject: subject})
		case sub.CoverageFound < sub.CoverageTokens:
			items = append(items, driftItem{reason: driftReasonLowCoverage, kind: driftKindAllow, subject: subject})
		default:
			items = append(items, driftItem{reason: driftReasonNoExhaustivenessAssertion, kind: driftKindAllow, subject: subject})
		}
	}

	for _, rule := range tc.Deny {
		subcmdKey := strings.Join(rule.Subcommand, " ")
		subject := strconv.Quote(name + " " + subcmdKey)
		if len(rule.Subcommand) > 1 {
			items = append(items, driftItem{reason: driftReasonMultiSegment, kind: driftKindDeny, subject: subject})
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
// (if any) and flag-dimension items (bucketed by (reason, rule kind), in
// driftReasonOrder x driftRuleKindOrder) into a single Finding. ok is false
// when the resulting sentence list is EMPTY — design §7.6 RULING 6: a reason
// bucket with items but no text (a driftReasonText entry gone missing, or a
// reason struck from driftReasonOrder so its bucket is never visited) must
// never produce a finding with a body-less Detail ("tools.git has 1 allow
// rule(s). .", the exact shape a reviewer produced by deleting one table
// entry) — silence is preferable to a causeless finding.
// TestDriftReasonTableIsComplete is the structural guard that keeps this
// branch unreachable in practice; this is the runtime backstop.
func driftBuildUnverifiableFinding(name string, allowCount, denyCount int, catalogReason string, items []driftItem) (Finding, bool) {
	var sentences []string
	if catalogReason != "" {
		sentences = append(sentences, "subcommand catalog: "+catalogReason)
	}

	type driftBucketKey struct {
		reason driftUnverifiableReason
		kind   driftRuleKind
	}
	byBucket := map[driftBucketKey][]string{}
	for _, it := range items {
		k := driftBucketKey{reason: it.reason, kind: it.kind}
		byBucket[k] = append(byBucket[k], it.subject)
	}

	for _, reason := range driftReasonOrder {
		text, haveText := driftReasonText[reason]
		if !haveText || text == "" {
			continue
		}
		for _, kind := range driftRuleKindOrder {
			subjects := byBucket[driftBucketKey{reason: reason, kind: kind}]
			if len(subjects) == 0 {
				continue
			}
			sort.Strings(subjects)
			named, suffix := driftCapList(subjects)
			sentences = append(sentences, fmt.Sprintf(
				"%d %s rule(s) %s (%s%s)",
				len(subjects), kind, text, strings.Join(named, ", "), suffix))
		}
	}

	if len(sentences) == 0 {
		return Finding{}, false
	}

	base := fmt.Sprintf("tools.%s has %d allow rule(s)", name, allowCount)
	if denyCount > 0 {
		base += fmt.Sprintf(" and %d deny rule(s)", denyCount)
	}

	return Finding{
		Severity: SeverityInfo,
		Title:    fmt.Sprintf("drift could not confirm all of %s's rules against the installed tool", name),
		Detail: fmt.Sprintf(
			"%s. %s.", base, strings.Join(sentences, "; ")),
		Path:   At("tools", name),
		DocRef: "design/doctor/wraptool-doctor.md",
	}, true
}

// ---------------------------------------------------------------- drift.unknown-subcommand

// driftCatalogEligible is ONE tool's evidence that its per-tool CatalogArgv
// probe may support a subcommand-ABSENCE claim (design §7.6 F2 RULINGS 1-2):
// what was measured, against a REAL installed binary, to establish the
// probed catalog enumerates EVERYTHING the tool will dispatch — built-ins,
// aliases, AND external/plugin commands, since a tool can pass F1's lower
// "did the probe even parse" bar while still silently omitting an
// operator's real, working plugin subcommand (see
// driftEligibleCatalogTools' own doc comment for the case that made this
// bar necessary) — plus a Sentinel bound against a probe result truncated or
// reshaped in a way that would still read as Exhaustive.
// NOTE on VersionMeasured: it is a RECORD, not a gate. Eligibility is keyed on
// basename alone, so an entry asserts that the tool's catalog is complete
// across every version a host might run -- not merely the one measured. If a
// tool's catalog shape ever diverges between versions, that assumption breaks
// and the entry must gain a version constraint. The sentinel is the only
// runtime guard today, and it catches a reshaped listing rather than a
// differently-complete one.
type driftCatalogEligible struct {
	// VersionMeasured is the exact binary version the evidence below was
	// gathered against — not itself printed into any finding (ProbeVersion's
	// live probe does that job, driftUnknownSubcommandVersionDisplay), but
	// recorded here so a later maintainer knows what was actually run.
	VersionMeasured string
	// Evidence describes what was checked to establish completeness:
	// built-ins, aliases, and external/plugin dispatch, and how each was
	// confirmed. Never empty (TestDriftEligibleCatalogToolsHaveEvidence).
	Evidence string
	// Sentinel is a subcommand certain to be present in a genuine, complete,
	// untruncated catalog for this tool (design §7.6 F2 RULING 2) — e.g.
	// "commit" for git, "version" for gcloud, "init" for terraform. If a
	// probed catalog does not contain it, the probe result does not match
	// what this entry's evidence describes and cannot be trusted this run.
	// Never empty (TestDriftEligibleCatalogToolsHaveEvidence).
	Sentinel string
}

// driftEligibleCatalogTools is an ALLOWLIST, keyed by tool basename, of
// tools whose catalog probe has been measured — against a real, installed
// binary — to enumerate EVERYTHING the tool will dispatch. This REPLACES an
// earlier denylist (driftPluginRiskyTools) that inverted the intent exactly:
// any tool with a CatalogArgv could assert an absence by DEFAULT, so git —
// the one tool this file actually measured — was excluded, while gcloud and
// terraform, verified on NO host at authoring time, were the only tools
// that COULD produce an absence claim. A reviewer downloaded real terraform
// (1.9.8 and 1.15.8) and demonstrated the consequence end to end:
// `terraform env` is a working, undocumented built-in subcommand absent
// from `terraform --help`, and the denylist shape reported it as not
// existing.
//
// A tool absent from this map NEVER produces a drift.unknown-subcommand
// finding, regardless of what LookupToolKnowledge or ProbeCatalog say — it
// falls through to drift.unverifiable instead (driftReasonCatalogNotVetted),
// which names this fact explicitly. EXPECT THIS MAP TO BE EMPTY OR NEARLY
// SO (architect ruling, verbatim): "shipping a check that fires rarely and
// truthfully beats one that fires often and sometimes lies." Never add an
// entry without evidence gathered against a real binary —
// TestDriftCatalogEligibilityIsExplicit (drift_test.go) requires every
// toolKnowledgeTable entry with a non-empty CatalogArgv to be accounted for
// here OR in driftCatalogExcludedTools, so a later CatalogArgv addition can
// never silently grant absence-claim authority by omission.
//
// Currently empty. Every toolKnowledgeTable entry with a CatalogArgv is
// accounted for in driftCatalogExcludedTools instead — see that map for the
// per-tool reason none of today's four known tools qualifies yet.
var driftEligibleCatalogTools = map[string]driftCatalogEligible{}

// driftCatalogExcludedTools records, for every toolKnowledgeTable entry with
// a non-empty CatalogArgv that is NOT in driftEligibleCatalogTools, why it
// was deliberately left out — the "explicitly excluded" half
// TestDriftCatalogEligibilityIsExplicit (design §7.6 F2 RULING 3) requires,
// so a maintainer who adds a CatalogArgv without touching either map fails
// the build loudly rather than leaving the decision to be inferred from an
// omission.
var driftCatalogExcludedTools = map[string]string{
	"git": "MEASURED, on git 2.54.0: `git help -a` lists a PATH-installed external command (a `git-foo` " +
		"script placed on PATH) under a genuine \"External commands\" section, but with NO description " +
		"column (`cat -A`: \"   foo$\", nothing after the name). internal/introspect's subcommandRe " +
		"(`^\\s{2,}(\\w[\\w-]*)\\s{2,}(.+)$`, parser_git.go) requires a SECOND 2+-space-separated group to " +
		"match at all, so the line is silently dropped from the parsed catalog even though " +
		"ProbeCatalog.Exhaustive still reports true (git's built-in commands alone already make a " +
		"non-empty, well-formed catalog). git's own built-ins and `git config alias.*` entries are NOT " +
		"affected (verified separately: `git help -a` lists a \"Command aliases\" section in the SAME " +
		"two-column, WITH-description shape). This is a PARSER gap in internal/introspect, not a " +
		"knowledge-table omission — recorded as design/doctor/wraptool-doctor.md §7.6's own named " +
		"prerequisite for making git eligible; fixing it needs its own step and its own verification, " +
		"since internal/introspect is depended on by `serve`.",
	"kubectl": "NOT verified on this host — kubectl is not installed here, so nothing could be measured " +
		"against a real binary. kubectl's own documented krew plugin convention (a `kubectl-<name>` " +
		"binary anywhere on PATH becomes `kubectl <name>`) is not enumerated by `kubectl --help` at all, " +
		"so the identical failure mode git's entry describes would apply here too if it were ever " +
		"measured — but an unmeasured, documented limitation is still not evidence, per this map's own " +
		"rule.",
	"gcloud": "NOT verified on any host this step has run against — gcloud is not installed here. Neither " +
		"installed nor measured, so `help`'s completeness for built-in, alias, AND plugin dispatch is " +
		"simply unknown; no claim either way.",
	"terraform": "MEASURED by a reviewer against real terraform 1.9.8 and 1.15.8: `terraform --help` does " +
		"not list `env`, a genuine, working built-in subcommand, at all. `--help`'s own catalog is " +
		"demonstrably NOT exhaustive for terraform's built-ins, independent of any plugin-dispatch " +
		"question.",
}

// driftCatalogEligibilityReason answers whether basename's catalog probe —
// already run against its CatalogArgv and already Exhaustive by F1's lower
// bar (looksLikeHelp, clean exit, non-empty parse) — may additionally
// support a subcommand-ABSENCE claim under design §7.6's F2 ruling round:
// "" means yes; any other value names why not. Shared verbatim by
// driftCatalogAxis (drift.unverifiable's per-rule catalog-eligibility items)
// and runDriftUnknownSubcommand (which gates its OWN findings on the same
// two facts) so the two checks can never drift text or logic apart —
// mirroring driftCacheTrustReason, already shared the same way for the
// cache-trust axis. Caller MUST have already confirmed probe.Exhaustive;
// this function does not check it again.
// driftCatalogEligibilityFailure is driftCatalogEligibilityReason's typed
// counterpart: same two questions, answered as the reason enum
// drift.unverifiable reports rather than as prose. ok is false when the tool
// is eligible and its catalog matched what was vetted.
func driftCatalogEligibilityFailure(basename string, probe introspect.CatalogProbe) (driftUnverifiableReason, bool) {
	eligible, ok := driftEligibleCatalogTools[basename]
	if !ok {
		return driftReasonCatalogNotVetted, true
	}
	if _, found := probe.Catalog[eligible.Sentinel]; !found {
		return driftReasonCatalogSentinelMissing, true
	}
	return driftReasonCount, false
}

func driftCatalogEligibilityReason(basename string, probe introspect.CatalogProbe) string {
	eligible, ok := driftEligibleCatalogTools[basename]
	if !ok {
		return fmt.Sprintf(
			"%s's subcommand catalog has not been vetted as an exhaustive listing of every built-in, alias, "+
				"and external/plugin subcommand it dispatches (design §7.6 F2 RULING 1) — no subcommand-absence "+
				"claim may be built on it yet",
			strconv.Quote(basename))
	}
	if _, ok := probe.Catalog[eligible.Sentinel]; !ok {
		return fmt.Sprintf(
			"the probed catalog for %s did not contain %q, the sentinel subcommand its vetted catalog is "+
				"expected to include (design §7.6 F2 RULING 2) — this run's result may be truncated or "+
				"reshaped and does not match what was vetted",
			strconv.Quote(basename), eligible.Sentinel)
	}
	return ""
}

// driftUnknownSubcommandVersionDisplay renders introspect.ProbeVersion's
// result for a finding's Detail/Remedy — design §7.6's own instruction
// ("Include the version probed in the finding, so a stale claim is
// self-dating") applies even when the live --version probe itself produced
// nothing (a tool that answers `--help`-shaped catalog argv but not
// `--version`, or a race against the binary being replaced between the two
// probes): a named, honest "unknown" beats silently omitting the clause THE
// GATE below requires.
func driftUnknownSubcommandVersionDisplay(version string) string {
	if version == "" {
		return "unknown (a live --version probe produced no output)"
	}
	return version
}

// runDriftUnknownSubcommand implements design §7.6's drift.unknown-subcommand
// for ALLOW rules only (deny rules are F3's, at fail severity — a typo'd deny
// is fail-open). THE GATE (architect brief, binding): a finding for one
// allow rule requires ALL of —
//
//  1. the tool has a knowledge-table entry AND a non-empty CatalogArgv
//     (LookupToolKnowledge, design §5.3);
//  2. introspect.ProbeCatalog(argv) reports Exhaustive == true — a clean,
//     non-deadline-killed, zero-exit probe whose output has the shape of a
//     genuine listing (see ProbeCatalog's own doc comment);
//  3. basename is on driftEligibleCatalogTools' ALLOWLIST, and the probed
//     catalog contains that entry's own Sentinel
//     (driftCatalogEligibilityReason, design §7.6 F2 RULINGS 1-2) — an
//     ALLOWLIST, not a denylist of tools known to be risky: THE GATE's own
//     text ("if a tool's catalog argv cannot enumerate plugins, that tool
//     CANNOT support this check") demands evidence of INCLUSION, not merely
//     the absence of a known problem, and the sentinel bound catches a
//     probe result reshaped or truncated in a way Exhaustive's own
//     exit-status/shape/non-emptiness checks cannot see;
//  4. the tool's cached schema exists AND is still trustworthy for the
//     currently configured binary — reused verbatim from F1:
//     introspect.LoadCacheStaleAt + driftCacheTrustReason, the SAME
//     "cached-schema trust conditions" drift.unverifiable already applies
//     per rule (design §7.6 RULING 3). This is a TOOL-wide gate (computed
//     once, like driftCacheTrustReason itself), not a per-subcommand one —
//     it says nothing about whether THIS rule's flags were ever captured,
//     only that the tool's own introspection state is coherent enough to
//     stand behind ANY drift claim about it right now.
//
// Anything short of all four is drift.unverifiable's job, not this one's —
// this function emits NOTHING for a disqualified tool, exactly like F1's
// checks do for the same underlying reasons (no double-reporting: a silent
// skip here, a named reason there). driftEligibleCatalogTools is expected to
// be EMPTY OR NEARLY SO (design §7.6 F2 RULING 1), so on most hosts this
// function never produces a finding at all — that is the correct, intended
// output, not a bug.
//
// Matching is FIRST-SEGMENT ONLY (design §5.3): rule.Subcommand[0] against
// probe.Catalog, regardless of how many segments the rule has. A multi-segment
// rule whose first segment DOES match reports nothing here (its deeper levels
// are drift.unverifiable's driftReasonMultiSegment, unchanged, reported
// separately) — this check only ever speaks to the first segment.
func runDriftUnknownSubcommand(ctx context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range toolSortedNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		if tc.Binary == "" || len(tc.Allow) == 0 {
			continue
		}

		basename := ToolBasename(tc.Binary)
		tk, ok := LookupToolKnowledge(basename)
		if !ok || len(tk.CatalogArgv) == 0 {
			continue // gate 1: no catalog argv at all — drift.unverifiable's job
		}

		probe := introspect.ProbeCatalog(ctx, &tc, tk.CatalogArgv)
		if !probe.Exhaustive {
			continue // gate 2: the live probe could not be trusted as exhaustive
		}
		if driftCatalogEligibilityReason(basename, probe) != "" {
			continue // gate 3: not on the vetted allowlist, or the sentinel is missing
		}

		schema, cached := introspect.LoadCacheStaleAt(in.Paths.CacheDir, name)
		if !cached {
			continue // gate 4: never introspected — driftReasonNoCachedSchema's tool
		}
		if _, untrusted := driftCacheTrustReason(in.Env, tc, schema); untrusted {
			continue // gate 4: cache no longer describes the configured binary
		}

		version := driftUnknownSubcommandVersionDisplay(introspect.ProbeVersion(ctx, &tc))
		argv := strings.Join(tk.CatalogArgv, " ")

		for _, rule := range tc.Allow {
			if len(rule.Subcommand) == 0 {
				continue
			}
			first := rule.Subcommand[0]
			if _, ok := probe.Catalog[first]; ok {
				continue
			}

			sub := strings.Join(rule.Subcommand, " ")
			findings = append(findings, Finding{
				Severity: SeverityWarn,
				Title:    "Allowed subcommand not found in the tool's catalog",
				Detail: fmt.Sprintf(
					"tools.%s.allow permits subcommand %q, whose first segment %q was not found in the "+
						"subcommand catalog probed with `%s %s` against the installed %s (version %s).",
					name, sub, first, basename, argv, name, version),
				Path: At("tools", name, "allow").Item("subcommand", sub),
				Remedy: fmt.Sprintf(
					"%q was not found in %s's subcommand catalog (probed with `%s %s`, version %s) — this "+
						"may be a typo, or a gap in the catalog probe itself; run it yourself and confirm "+
						"before changing tools.%s.allow's %q entry, never remove it on this finding alone",
					first, name, basename, argv, version, name, sub),
				DocRef: "design/doctor/wraptool-doctor.md",
			})
		}
	}
	return Found(findings...)
}
