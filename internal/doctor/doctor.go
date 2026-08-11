package doctor

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pti/wraptool/internal/buildinfo"
	"github.com/pti/wraptool/internal/config"
)

// HostPaths is internal/paths resolved ONCE by cmd/doctor.go. Checks never call
// paths.DataDir() etc. directly: adrg/xdg caches its globals at package init, so
// a check that read them would not be table-testable.
type HostPaths struct {
	ConfigDir  string
	DataDir    string
	CacheDir   string
	RuntimeDir string // serve.state, pidfile (B9)
	// PoolRoot is pool.Root(), resolved ONCE here. internal/pool's zero-argument
	// layout accessors (Root/HomeDir/LockFile/RuntimeDir/CacheDir/...) are NOT
	// root-parameterized — they resolve through paths.DataDir() directly
	// (internal/pool/layout.go), so calling e.g. pool.HomeDir() from a check
	// would read the REAL host's $XDG_DATA_HOME even under a MapEnv-backed unit
	// test. Only the root-parameterized functions are safe: ActiveGeneration and
	// GenerationVersion take a pool.PoolPaths (build one from this field), while
	// VerifyGenerationTree takes an explicit root string directly. B8 uses
	// these, never pool's zero-arg accessors (enforced by invariants_test.go's
	// invariantsForbiddenHostSelectors).
	PoolRoot string
}

// ConfigState is a closed enum covering the three states §10.1 distinguishes.
// Absent is NOT an error: `wraptool doctor` must be safe as the very first
// command a new user runs, before anything has scaffolded a config.
type ConfigState string

const (
	ConfigLoaded     ConfigState = "loaded"
	ConfigAbsent     ConfigState = "absent"     // exit 0; header says "no config yet"
	ConfigUnreadable ConfigState = "unreadable" // exit 2
)

// Valid reports whether s is one of the three declared members.
func (s ConfigState) Valid() bool {
	switch s {
	case ConfigLoaded, ConfigAbsent, ConfigUnreadable:
		return true
	}
	return false
}

// Input is the subject under audit plus the injected environment. Checks receive
// a *Input and nothing else.
//
// Input is the intended GROWTH POINT of this package. Check, Result, Finding and
// Env are not: adding a field here is cheap, changing those is not.
type Input struct {
	Env   Env
	Paths HostPaths
	GOOS  string // runtime.GOOS, injected so the Windows skip path is testable on Linux

	ConfigPath  string // resolved absolute path; may not exist
	ConfigState ConfigState
	// Config is the LENIENTLY loaded, DEFAULTED config (config.Load), or nil.
	// READ THIS BEFORE WRITING ANY CHECK: applyDefaults has already run, so
	// -h/--help are seeded into every allow rule (config.SeedHelpFlags), timeouts
	// and cache_ttl are populated, request_log is defaulted, and ~ is expanded.
	// Config is NOT the file. Anything that must reason about what the operator
	// ACTUALLY WROTE — every policy lint, every Location — reads the node tree
	// (C1), not this. Strict decode + Validate is B2's job, reported as findings.
	Config    *config.Config
	ConfigErr error  // the lenient load error when ConfigState != ConfigLoaded
	ConfigRaw []byte // config bytes as read via Env.ReadFile; nil when absent. C1/B2 hook.

	Workspace  string // project root, or "" — an os.Stat walk in cmd, never `git rev-parse` (§5)
	Probe      bool   // false under --no-probe (A2); always false in A1
	VerifyPool bool   // --verify-pool (A2/B8); always false in A1
	// C1 adds: Doc *yaml.Node — parsed from ConfigRaw, so both views come from
	// one read. Left out of A1 rather than shipped as a permanently nil field.
	// F1 adds: Prober — an introspection-shaped interface, nil meaning
	// unavailable, NEVER a generic Exec on Env.
	//
	// B9 does NOT need an Input field: it sits inside the nine-way parallel
	// wave and cannot widen a shared type without conflicting with every other
	// Phase-B step. cmd/server.go's serveState is unexported anyway, so
	// runtime.go reads and decodes serve.state itself — Input.Paths.RuntimeDir
	// names the file, Env.ReadFile reads it — entirely within B9's own file.
	// E1 lands AFTER the parallel wave and can safely add Server *ServerState
	// here if a later step benefits from a pre-parsed, shared representation.
}

// RunOptions is the engine-side selection hook. A2 wires --only/--skip/--strict
// to these fields and adds render_json.go; nothing structural changes.
type RunOptions struct {
	Only    []string // check IDs or group names; empty = all
	Skip    []string // check IDs or group names; wins over Only
	Strict  bool     // A2: promotes unsuppressed warn to a non-zero EXIT, never to fail severity
	Locator Locator  // nil => nullLocator. C1's ONE-LINE change is at the caller.
	Checks  []Check  // nil => Registered(). Tests pass their own; keeps t.Parallel() safe.
}

// CheckOutcome is the per-check row that backs the report's run-order view.
type CheckOutcome struct {
	ID       CheckID
	Outcome  Outcome
	Skip     string
	Findings int
}

// SkipRecord is one entry of §9's `skipped` array.
type SkipRecord struct {
	ID     CheckID `json:"id"`
	Reason string  `json:"reason"`
}

// Summary carries TWO axes and names them apart on purpose. §8's footer mixes
// them — "2 failed, 5 warnings, 3 info" counts FINDINGS while "22 passed,
// 6 skipped" counts CHECKS — which is exactly the arithmetic bug this naming
// prevents. The two families are never summed together.
//
// Invariants, asserted by an engine test:
//
//	Pass + WithFindings + Skipped == Total == len(selected) == len(Report.Outcomes)
//	Fail + Warn + Info == len(Report.Findings);  Suppressed is a SUBSET of Warn
type Summary struct {
	Fail       int `json:"fail"`
	Warn       int `json:"warn"`
	Suppressed int `json:"suppressed"` // C2
	Info       int `json:"info"`
	Pass       int `json:"pass"`
	Skipped    int `json:"skipped"`

	Total        int `json:"-"`
	WithFindings int `json:"-"`
}

// Report is the complete result of one Run.
type Report struct {
	Schema      int    // SchemaVersion; A2's --json consumes it
	Version     string // buildinfo.Version()
	ConfigPath  string
	ConfigState ConfigState
	Workspace   string
	Probed      bool
	StartedAt   time.Time

	// Findings in CANONICAL order (RULING R2): severity, then ID, then Location,
	// then Detail. A total order on CONTENT, so two runs over identical inputs
	// produce byte-identical output regardless of check execution order.
	Findings []Finding
	Outcomes []CheckOutcome // run order
	Skipped  []SkipRecord   // run order; §9's skipped array
	Summary  Summary
	Strict   bool

	// Incomplete => exit 2. Set by a registry error, a Result.Fatal, an invalid
	// Result or Finding, or ConfigState == ConfigUnreadable.
	Incomplete       bool
	IncompleteReason string
	// Aborted is true iff Incomplete AND at least one SELECTED check never ran
	// because of it: a registry error, a selection error, a check returning
	// Result.Fatal, or a check returning an invalid Result — every one of these
	// cuts the run short (RenderHuman's "Checks after this point did not run.").
	// False for the ConfigUnreadable-only case (§10.1): every selected check,
	// including the full env.* group, still ran and is reported below the
	// incomplete banner, so that clause would be actively wrong there.
	Aborted bool
}

// SchemaVersion is A2's --json schema identifier.
const SchemaVersion = 1

// groupOrder is the closed run-order/report-section table (RULING R1 + R2).
// No Phase-B step edits it. Never switch on Group — the exhaustive linter
// would police it; use groupRank/knownGroup below instead.
var groupOrder = []struct {
	Name  Group
	Title string
}{
	{GroupEnv, "environment"},
	{GroupConfig, "configuration"},
	{GroupExposure, "exposure"},
	{GroupTools, "tools"},
	{GroupPool, "harness pool"},
	{GroupRuntime, "runtime"},
	{GroupDrift, "drift"},
	{GroupPolicy, "policy"},
}

func groupRank(g Group) int {
	for i, e := range groupOrder {
		if e.Name == g {
			return i
		}
	}
	return len(groupOrder) // unknown group sorts last
}

func knownGroup(g Group) bool {
	for _, e := range groupOrder {
		if e.Name == g {
			return true
		}
	}
	return false
}

// Run executes the selected checks against in, in deterministic order, and
// returns a fully ordered, fully counted Report.
//
// Pipeline (four separable phases, so C2's suppression pass can insert between
// collect and count without restructuring): execute -> collect -> sort -> count.
func Run(ctx context.Context, in *Input, opts RunOptions) Report {
	// A nil Input or a nil Env would panic on in.Env.Now() below. The package
	// otherwise takes a deliberate no-panic posture (RULING R3: register
	// records rather than panics) — a Phase-B author hand-building an Input in
	// a table test is the obvious way to hit this, so it gets the same
	// Incomplete/exit-2 treatment as every other "cannot proceed" case rather
	// than a crash.
	if in == nil || in.Env == nil {
		return Report{
			Schema:           SchemaVersion,
			Version:          buildinfo.Version(),
			Incomplete:       true,
			IncompleteReason: "doctor: Input.Env is nil",
			Aborted:          true,
		}
	}

	r := Report{
		Schema:      SchemaVersion,
		Version:     buildinfo.Version(),
		ConfigPath:  in.ConfigPath,
		ConfigState: in.ConfigState,
		Workspace:   in.Workspace,
		Probed:      in.Probe,
		StartedAt:   in.Env.Now(),
		Strict:      opts.Strict,
	}

	// An inconsistent catalog cannot produce a trustworthy report: stop before
	// running anything. Gated on opts.Checks == nil: a caller supplying its own
	// check list (every engine test, and any future external caller) never
	// depends on the GLOBAL registry, so the one collision two parallel Phase-B
	// authors can create that git merges cleanly (a duplicate ID, caught by
	// TestRegistryIsWellFormed) does not also break every synthetic-check
	// engine test that happens to run first in the same `go test` process.
	if opts.Checks == nil {
		if err := RegistryErr(); err != nil {
			r.Incomplete = true
			r.IncompleteReason = fmt.Sprintf("doctor check registry is invalid: %v", err)
			r.Aborted = true
			return r
		}
	}

	checks := opts.Checks
	if checks == nil {
		checks = Registered()
	} else {
		checks = append([]Check(nil), checks...)
		sortChecks(checks)
	}

	selected, err := selectChecks(checks, opts.Only, opts.Skip)
	if err != nil {
		r.Incomplete = true
		r.IncompleteReason = err.Error()
		r.Aborted = true
		return r
	}
	r.Summary.Total = len(selected)

	loc := opts.Locator
	if loc == nil {
		loc = nullLocator{}
	}

	have := provides(in)
	aborted := false
	var abortedAfter CheckID
	// cutShort counts checks actually skipped BECAUSE of an abort (the `if
	// aborted` branch below), which is the only thing Report.Aborted means:
	// at least one selected check that would otherwise have run did not.
	// Deciding it at the moment an abort is triggered is wrong when the
	// aborting check is the last one selected — nothing was cut short, so the
	// "Checks after this point did not run." banner would be false.
	cutShort := 0

	for _, c := range selected {
		m := c.Meta()

		if aborted {
			reason := fmt.Sprintf("not run: doctor aborted after %s", abortedAfter)
			r.Skipped = append(r.Skipped, SkipRecord{ID: m.ID, Reason: reason})
			r.Outcomes = append(r.Outcomes, CheckOutcome{ID: m.ID, Outcome: OutcomeSkipped, Skip: reason})
			r.Summary.Skipped++
			cutShort++
			continue
		}

		if missing := m.Needs &^ have; missing != 0 {
			reason := firstMissingReason(missing)
			r.Skipped = append(r.Skipped, SkipRecord{ID: m.ID, Reason: reason})
			r.Outcomes = append(r.Outcomes, CheckOutcome{ID: m.ID, Outcome: OutcomeSkipped, Skip: reason})
			r.Summary.Skipped++
			continue
		}

		res := c.Run(ctx, in)

		if reason := validateResult(m.ID, res); reason != "" {
			// A malformed Result is untrustworthy: record THIS check as skipped
			// (never publish whatever it returned), abort the run so every
			// remaining selected check is also skipped, and still fall through
			// to sortFindings/countSeverities — an early, uncounted `return r`
			// here would violate Summary's own documented invariants (see the
			// Summary doc comment) and publish findings out of canonical order.
			r.Incomplete = true
			r.IncompleteReason = reason
			r.Skipped = append(r.Skipped, SkipRecord{ID: m.ID, Reason: reason})
			r.Outcomes = append(r.Outcomes, CheckOutcome{ID: m.ID, Outcome: OutcomeSkipped, Skip: reason})
			r.Summary.Skipped++
			aborted = true
			abortedAfter = m.ID
			continue
		}

		outcome := res.Outcome()
		co := CheckOutcome{ID: m.ID, Outcome: outcome}

		switch outcome {
		case OutcomeSkipped:
			// res.SkipReason is empty for a Fatal-with-no-findings Result (see
			// Result.Outcome): fall back to the Fatal error text so this
			// check's own skip record still says something.
			reason := res.SkipReason
			if reason == "" && res.Fatal != nil {
				reason = res.Fatal.Error()
			}
			co.Skip = reason
			r.Skipped = append(r.Skipped, SkipRecord{ID: m.ID, Reason: reason})
			r.Summary.Skipped++
		case OutcomePass:
			r.Summary.Pass++
		case OutcomeFinding:
			for _, f := range res.Findings {
				if f.ID == "" {
					f.ID = m.ID
				}
				if !f.Path.Empty() {
					if site, ok := loc.Locate(f.Path); ok {
						f.site = site
						f.Location = site.String()
					}
				}
				r.Findings = append(r.Findings, f)
			}
			co.Findings = len(res.Findings)
			r.Summary.WithFindings++
		}

		r.Outcomes = append(r.Outcomes, co)

		if res.Fatal != nil {
			r.Incomplete = true
			r.IncompleteReason = res.Fatal.Error()
			aborted = true
			abortedAfter = m.ID
		}
	}
	r.Aborted = cutShort > 0

	// An unreadable config makes the report incomplete even though A1 has no
	// check to say so yet (that is B2's config.parse, via Result.Fatal — once
	// it lands this clause becomes redundant with it, never contradictory: both
	// set Incomplete for the same underlying cause). Checked here, not before
	// the loop, so every selected check still runs and is reported — §10.1's
	// "renders the header and the full env.* group, then exits 2".
	if in.ConfigState == ConfigUnreadable && !r.Incomplete {
		r.Incomplete = true
		r.IncompleteReason = unreadableConfigReason(in)
	}

	sortFindings(r.Findings)
	countSeverities(&r)

	return r
}

// validateResult enforces Result's stated invariants (§ finding.go doc
// comments) and returns a non-empty reason when the check misbehaved. The
// caller treats a non-empty reason as an internal error: Incomplete, abort.
func validateResult(id CheckID, res Result) string {
	if res.SkipReason != "" && len(res.Findings) > 0 {
		return fmt.Sprintf("doctor check %q returned both a skip reason and findings", id)
	}
	for _, f := range res.Findings {
		if !f.Severity.Valid() {
			return fmt.Sprintf("doctor check %q emitted a finding with invalid severity %q", id, f.Severity)
		}
		if f.ID != "" {
			if _, ok := regSites[f.ID]; !ok {
				return fmt.Sprintf("doctor check %q emitted a finding with unregistered ID %q", id, f.ID)
			}
		}
	}
	return ""
}

// unreadableConfigReason renders the IncompleteReason for a ConfigUnreadable
// input: in.ConfigErr's own message when set, a generic fallback otherwise.
func unreadableConfigReason(in *Input) string {
	if in.ConfigErr != nil {
		return in.ConfigErr.Error()
	}
	return "config file is unreadable"
}

// provides computes which Requirement bits in is able to satisfy. This is the
// single place every check-gating guard lives (see Requirement's doc comment).
func provides(in *Input) Requirement {
	var have Requirement
	if in.Config != nil {
		have |= NeedsConfig
	}
	if in.Workspace != "" {
		have |= NeedsWorkspace
	}
	if in.GOOS != "windows" {
		have |= NeedsModeBits
	}
	if in.Probe {
		have |= NeedsProbe
	}
	if in.VerifyPool {
		have |= NeedsPoolVerify
	}
	return have
}

// selectChecks applies --only/--skip token filtering (RULING R4: a deselected
// check leaves the run entirely, uncounted). A token matching nothing at all —
// neither a registered check ID nor a known group name — is an error, never a
// silent empty run.
func selectChecks(checks []Check, only, skip []string) ([]Check, error) {
	if err := validateSelectors(checks, only); err != nil {
		return nil, err
	}
	if err := validateSelectors(checks, skip); err != nil {
		return nil, err
	}
	var out []Check
	for _, c := range checks {
		id := c.Meta().ID
		if len(skip) > 0 && matchesAny(id, skip) {
			continue
		}
		if len(only) > 0 && !matchesAny(id, only) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func matchesAny(id CheckID, tokens []string) bool {
	for _, t := range tokens {
		if t == string(id) || Group(t) == id.Group() {
			return true
		}
	}
	return false
}

func validateSelectors(checks []Check, tokens []string) error {
	for _, t := range tokens {
		if knownGroup(Group(t)) {
			continue
		}
		matched := false
		for _, c := range checks {
			if string(c.Meta().ID) == t {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("doctor: unknown check or group %q", t)
		}
	}
	return nil
}

// sortFindings imposes the canonical, total order on findings (RULING R2):
// severity, then ID, then Location, then Detail. Detail as the final key is
// what makes it total rather than merely stable — a single check legitimately
// emits several same-ID, same-(pre-C1)-location findings.
func sortFindings(fs []Finding) {
	slices.SortStableFunc(fs, func(a, b Finding) int {
		return cmp.Or(
			cmp.Compare(a.Severity.rank(), b.Severity.rank()),
			strings.Compare(string(a.ID), string(b.ID)),
			strings.Compare(a.Location, b.Location),
			strings.Compare(a.Detail, b.Detail),
		)
	})
}

func countSeverities(r *Report) {
	for _, f := range r.Findings {
		switch f.Severity {
		case SeverityFail:
			r.Summary.Fail++
		case SeverityWarn:
			r.Summary.Warn++
		case SeverityInfo:
			r.Summary.Info++
		}
	}
}

// ExitCode maps a Report to a process exit status: 0 clean, 1 at least one
// fail finding (or, under --strict, an unsuppressed warn), 2 doctor could not
// complete.
func (r Report) ExitCode() int {
	switch {
	case r.Incomplete:
		return 2
	case r.Summary.Fail > 0:
		return 1
	case r.Strict && r.Summary.Warn-r.Summary.Suppressed > 0:
		return 1 // A2 sets Strict; never mutates severities
	default:
		return 0
	}
}
