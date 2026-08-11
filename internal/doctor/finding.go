// Package doctor implements `wraptool doctor`, the operator-facing preflight
// report. It is OPERATOR-facing only: nothing here is ever registered as an MCP
// tool or exposed on an unauthenticated surface — its output is a map of exactly
// where the policy is weakest (design §2 non-goals). internal/mcptools and
// internal/webui must never import this package; invariants_test.go enforces it.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------- Severity

// Severity is a closed enum. .golangci.yml enables `exhaustive` with
// default-signifies-exhaustive: false, so every switch below names all three
// members and a `default:` clause does not excuse a missing case.
type Severity string

const (
	// SeverityFail: wraptool is broken, or the credential boundary is provably
	// not what the config claims. Never fires on a configuration working as
	// designed.
	SeverityFail Severity = "fail"
	// SeverityWarn: a heuristic, a default-open setting, or a shape dangerous
	// in most deployments but legitimately intended in some. Only warn is
	// suppressible (§6.2, step C2).
	SeverityWarn Severity = "warn"
	// SeverityInfo: state worth knowing, never a problem.
	SeverityInfo Severity = "info"
)

// rank orders severities for the canonical finding order (§8: fail→warn→info).
// String order is wrong ("fail" < "info" < "warn"), so this table exists.
func (s Severity) rank() int {
	switch s {
	case SeverityFail:
		return 0
	case SeverityWarn:
		return 1
	case SeverityInfo:
		return 2
	}
	// Not a declared member — i.e. the zero value, which the engine rejects as
	// an internal error before any finding reaches the sort. Ranked last so a
	// bug degrades into ugly output rather than a panic.
	return 3
}

// Valid reports whether s is one of the three declared members.
func (s Severity) Valid() bool {
	switch s {
	case SeverityFail, SeverityWarn, SeverityInfo:
		return true
	}
	return false
}

// ---------------------------------------------------------------- Outcome

// Outcome is per CHECK, not per finding: exactly one Outcome per selected check,
// which is what makes §6's `pass + findings + skipped = total` hold by
// construction. It is DERIVED from Result, never set by a check, so "pass with
// findings" is unrepresentable.
type Outcome string

const (
	OutcomePass    Outcome = "pass"    // ran, found nothing
	OutcomeFinding Outcome = "finding" // ran, produced >= 1 finding
	OutcomeSkipped Outcome = "skipped" // did not run; SkipReason says why
)

// Valid reports whether o is one of the three declared members.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomePass, OutcomeFinding, OutcomeSkipped:
		return true
	}
	return false
}

// ---------------------------------------------------------------- identity

// CheckID is a stable dotted catalog ID (§9.1): "policy.unpatterned-value-flag".
type CheckID string

// Group is derived from the prefix, never stored in Meta — one fewer field for
// an author to get wrong and one fewer field two authors can disagree about.
func (id CheckID) Group() Group {
	g, _, _ := strings.Cut(string(id), ".")
	return Group(g)
}

// Group is both the report section and the run-order bucket.
type Group string

const (
	GroupEnv      Group = "env"
	GroupConfig   Group = "config"
	GroupExposure Group = "exposure"
	GroupTools    Group = "tools"
	GroupPool     Group = "pool"
	GroupRuntime  Group = "runtime"
	GroupDrift    Group = "drift"
	GroupPolicy   Group = "policy"
)

// idPattern is the catalog ID shape. Enforcing it at registration keeps Group()
// total and makes §9.1's group-level CI gating (`--only config,exposure`)
// well-defined instead of leaving nine authors to invent nine spellings.
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9]*\.[a-z0-9]+(-[a-z0-9]+)*$`)

// ---------------------------------------------------------------- Requirement

// Requirement is a bitmask of inputs a check needs. The engine computes what the
// Input actually provides once and skips any check whose Needs are unmet, with
// ONE canned reason per bit — so nine parallel authors do not invent nine
// phrasings of "no config file", and the plan's own acknowledged
// `skipIfNoModeBits()` duplication between B3/B5/B6 (plan §3, "Windows
// handling") never happens.
//
// This vocabulary is CLOSED in A1. A Phase-B step that believes it needs a new
// bit has hit plan §1 rule 6: stop and report, do not widen.
//
// Deliberately NOT a bit: "a live server". runtime.stale-pidfile (§7.5) fires
// precisely when the pid is DEAD, so gating it on liveness would suppress the
// check. There is also deliberately no Input.Server field: B9 reads and
// decodes serve.state itself, via Input.Paths.RuntimeDir + Env.ReadFile,
// entirely inside runtime.go (see doctor.go's Input doc comment).
type Requirement uint8

const (
	NeedsNothing    Requirement = 0
	NeedsConfig     Requirement = 1 << 0 // Input.Config != nil
	NeedsWorkspace  Requirement = 1 << 1 // Input.Workspace != ""
	NeedsModeBits   Requirement = 1 << 2 // POSIX permission bits meaningful (§12)
	NeedsProbe      Requirement = 1 << 3 // Input.Probe — false under --no-probe (F)
	NeedsPoolVerify Requirement = 1 << 4 // Input.VerifyPool — --verify-pool (§7.9)
)

// requirementReasons is an ORDERED table, not a switch: a switch over a bitmask
// type would trip the `exhaustive` linter, and the order fixes which reason is
// reported when several bits are missing (the first, most fundamental one).
var requirementReasons = []struct {
	bit    Requirement
	reason string
}{
	{NeedsConfig, "no readable config file"},
	{NeedsWorkspace, "no project workspace (pass --workspace)"},
	{NeedsModeBits, "file permission bits are not meaningful on this platform"},
	{NeedsProbe, "probing disabled (--no-probe)"},
	{NeedsPoolVerify, "pass --verify-pool to run this check"},
}

func firstMissingReason(missing Requirement) string {
	for _, r := range requirementReasons {
		if missing&r.bit != 0 {
			return r.reason
		}
	}
	return "a required input was unavailable"
}

// ---------------------------------------------------------------- locations

// ConfigPath is a structured pointer into the config DOCUMENT. It is keyed by
// VALUE, never by slice index — §6.1: SeedHelpFlags both APPENDS -h/--help and
// DROPS deny_flags entries (internal/config/config.go:233+), so struct index N
// is not file index N and any index-based mapping reports the wrong line.
//
// A1 defines the type and its builders but resolves nothing (nullLocator). C1
// ships locate.go with a real Locator and changes ONE line in cmd/doctor.go.
// Because the builders are the only thing group files touch, C1 can rework the
// internal representation without editing a single group file.
//
// MULTI-SEGMENT SUBCOMMANDS, RULING R6: a subcommand value is always spelled
// strings.Join(rule.Subcommand, " ") — At("tools","gcloud","allow").
// Item("subcommand", "compute instances list"). C1's resolver joins the file's
// sequence node the same way. Spelling it any other way silently resolves to
// nothing (ok == false → blank location, a quiet failure).
type ConfigPath struct{ steps []pathStep }

type pathStep struct {
	Key   string // mapping key to descend ("tools", "git", "allow")
	Field string // sequence step: item field whose scalar identifies it
	Value string // sequence step: the identifying value
	seq   bool   // true for a sequence step (Elem/Item), false for a Key step
}

// At starts a path at the document root: At("tools", name, "binary").
func At(keys ...string) ConfigPath { return ConfigPath{}.Key(keys...) }

// Key descends into one or more mapping keys.
func (p ConfigPath) Key(keys ...string) ConfigPath {
	out := ConfigPath{steps: append([]pathStep(nil), p.steps...)}
	for _, k := range keys {
		out.steps = append(out.steps, pathStep{Key: k})
	}
	return out
}

// Elem selects a SCALAR sequence element by its value — e.g. a flag inside
// `flags:`. First match wins if the operator wrote a duplicate.
func (p ConfigPath) Elem(value string) ConfigPath {
	out := ConfigPath{steps: append([]pathStep(nil), p.steps...)}
	out.steps = append(out.steps, pathStep{Value: value, seq: true})
	return out
}

// Item selects a MAPPING sequence item by one of its fields — e.g. the allow
// rule whose `subcommand` is [pull]: Item("subcommand", "pull"). See R6 for
// multi-segment values.
func (p ConfigPath) Item(field, value string) ConfigPath {
	out := ConfigPath{steps: append([]pathStep(nil), p.steps...)}
	out.steps = append(out.steps, pathStep{Field: field, Value: value, seq: true})
	return out
}

// Empty reports whether p has no steps at all.
func (p ConfigPath) Empty() bool { return len(p.steps) == 0 }

// String is a stable dotted rendering: tools.git.allow[subcommand=pull].flags[-c]
// Used as a deterministic identity in tests and as a debugging aid. It is NOT a
// file location and is never rendered as one.
func (p ConfigPath) String() string {
	var b strings.Builder
	for _, s := range p.steps {
		switch {
		case !s.seq:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(s.Key)
		case s.Field == "":
			fmt.Fprintf(&b, "[%s]", s.Value)
		default:
			fmt.Fprintf(&b, "[%s=%s]", s.Field, s.Value)
		}
	}
	return b.String()
}

// Site is what a Locator resolves a ConfigPath to.
//
// It carries BOTH nodes because C2 (§6.2) reads HeadComment/LineComment for
// `# doctor:ignore`, and yaml.v3 attaches a comment written before a mapping key
// to the KEY SCALAR, not to the value node. A Locator returning only file:line
// would force C2 to re-traverse and would make C2 a second cross-cutting step.
// KeyNode is nil when the last path step is a sequence step.
type Site struct {
	File      string
	Line      int
	KeyNode   *yaml.Node // the key scalar, when the last step was a Key step
	ValueNode *yaml.Node // the node the path resolves to
}

// String renders a stable "file:line" location, or "" when unresolved.
func (s Site) String() string {
	if s.File == "" || s.Line == 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d", s.File, s.Line)
}

// Locator maps a ConfigPath to a file position. A1 ships nullLocator only.
type Locator interface {
	Locate(ConfigPath) (Site, bool)
}

type nullLocator struct{}

func (nullLocator) Locate(ConfigPath) (Site, bool) { return Site{}, false }

// ---------------------------------------------------------------- Finding

// Finding is one reportable observation. Field set is §6 verbatim plus Path
// (R5) and C2's two reserved fields.
type Finding struct {
	// ID is stamped by the engine from the owning check's Meta.ID when a check
	// leaves it empty, which is the normal case and the one Phase-B authors
	// should use. A check MAY set it explicitly to emit a sibling catalog ID
	// (drift.unverifiable alongside drift.unknown-subcommand); the engine then
	// verifies that ID is registered and fails the run if it is not.
	ID       CheckID  `json:"id"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`  // short, condition-shaped
	Detail   string   `json:"detail"` // what was found, with the concrete value

	// Path is a check's ONLY way to say where in the CONFIG a finding lives.
	// See R5. Leave it zero for a non-config subject.
	Path ConfigPath `json:"-"`

	// Location is ENGINE-OWNED for config findings: the engine fills it from
	// Path via the Locator. A check may set it directly ONLY for a non-config
	// subject (a binary path, a socket directory, a pool generation dir); a
	// non-empty Path always wins. Reserving this field is what makes C1 a
	// zero-line edit per group file.
	//
	// Plan §1 rule 9: never set Location for a config finding — set Path.
	// Hand-rolling Location as in.ConfigPath (or in.ConfigPath plus a
	// guessed ":line") looks like it works today (C1 has not landed a real
	// Locator yet, so nullLocator never overwrites it), but it is exactly the
	// silent rot C1 is designed to avoid: C1 cannot retrofit a real file:line
	// onto a finding that already has a Location, without editing that
	// check's own group file. TestFindingsAreWellFormed additionally rejects a
	// Location that has in.ConfigPath as a prefix while Path is empty.
	Location string `json:"location"`

	Remedy string `json:"remedy"`  // the change to make; "" when there is nothing to do
	DocRef string `json:"doc_ref"` // e.g. "docs/configuration.qmd#flag-constraints"

	// Reserved for C2. Zero values are correct until then, so C2 touches
	// suppress.go plus one filter call and no shared shape, and A2's --json
	// schema does not get bumped by C2.
	Suppressed        bool   `json:"suppressed"`
	SuppressionReason string `json:"suppression_reason"`

	// site is the resolved node, kept for C2's comment lookup. Unexported: no
	// caller outside internal/doctor has any business with a *yaml.Node.
	site Site
}

// ---------------------------------------------------------------- Result

// Result is what a check returns. The states are mutually exclusive and the
// engine validates that; a violation is an internal error (exit 2) naming the
// check, never a silently reinterpreted report.
type Result struct {
	Findings   []Finding
	SkipReason string // non-empty => OutcomeSkipped; shown verbatim in the report
	// Fatal non-nil => doctor cannot complete. The engine records the finding(s),
	// stops, marks every remaining selected check skipped, sets Report.Incomplete,
	// and exits 2. This is B2's exit-2 abort path (§10.1) — shipped in A1 so B2,
	// which lands INSIDE the nine-way parallel wave, never edits doctor.go.
	Fatal error
}

// Pass reports that the check ran and found nothing. Silence means "checked,
// clean", never "did not look" (§6).
func Pass() Result { return Result{} }

// Skipped reports that the check could not run. Use it for every data-source
// case the engine cannot know about ("tool binary not found").
func Skipped(format string, a ...any) Result {
	return Result{SkipReason: fmt.Sprintf(format, a...)}
}

// Found returns a finding result. Found() with no findings returns a Pass, so
// `return Found(fs...)` is correct even when the slice came out empty — the
// single most likely way a Phase-B author would otherwise break the summary
// arithmetic.
func Found(f ...Finding) Result {
	if len(f) == 0 {
		return Pass()
	}
	return Result{Findings: f}
}

// Abort reports findings AND stops the run. See Result.Fatal.
func Abort(err error, f ...Finding) Result {
	return Result{Findings: f, Fatal: err}
}

// Outcome derives this result's per-check Outcome. A Fatal Result with no
// findings (Abort's findings parameter is variadic, so this is legal) is
// OutcomeSkipped, never OutcomePass: the check that aborted the whole run
// must not be reported as having passed.
func (r Result) Outcome() Outcome {
	switch {
	case r.SkipReason != "":
		return OutcomeSkipped
	case len(r.Findings) > 0:
		return OutcomeFinding
	case r.Fatal != nil:
		return OutcomeSkipped
	default:
		return OutcomePass
	}
}

// ---------------------------------------------------------------- the seam

// RunFunc is a check's implementation.
type RunFunc func(ctx context.Context, in *Input) Result

// Meta is a check's static description. Severity is deliberately NOT here:
// §7.6's drift.unknown-subcommand is warn for allow rules and fail for deny
// rules, so severity is a property of the finding, not of the check.
type Meta struct {
	ID    CheckID
	Needs Requirement
	// Summary is one line, present tense, describing what the check looks for.
	// Consumed by A2's --list and G1's docs table. Required; the registry
	// invariant test rejects an empty one.
	Summary string
}

// Check is the engine's extension point. Group files never implement it by hand
// — register() adapts a Meta + RunFunc — but it stays an interface so tests can
// build a check list from synthetic checks without touching the global registry,
// and so a stateful check (a compiled regexp set, a per-tool table) stays
// possible.
type Check interface {
	Meta() Meta
	Run(ctx context.Context, in *Input) Result
}

type funcCheck struct {
	meta Meta
	run  RunFunc
}

func (c funcCheck) Meta() Meta { return c.meta }

func (c funcCheck) Run(ctx context.Context, in *Input) Result { return c.run(ctx, in) }

// ---------------------------------------------------------------- registry

var (
	registry     []Check
	regSites     = map[CheckID]string{} // ID -> file:line of its register() call
	registryErrs []error
)

// register is THE seam. A check file calls it from its own init(), so two
// concurrently authored Phase-B steps never edit a shared list — this is what
// makes the nine-way parallel wave mergeable (plan §1 rule 4).
//
//	func init() {
//	    register(Meta{ID: "env.container-backend", Needs: NeedsNothing,
//	        Summary: "reports which container backends are on PATH"},
//	        runEnvContainerBackend)
//	}
//
// It records problems rather than panicking (RULING R3). A duplicate ID is the
// one collision two parallel authors can create that git merges CLEANLY; both
// registration sites are captured via runtime.Caller so the failure message
// names them, and `go test ./internal/doctor` fails on the merge commit while
// `wraptool serve` still boots.
func register(m Meta, run RunFunc) {
	_, file, line, _ := runtime.Caller(1)
	registerCheck(funcCheck{meta: m, run: run}, fmt.Sprintf("%s:%d", file, line))
}

func registerCheck(c Check, site string) {
	m := c.Meta()
	switch {
	case !idPattern.MatchString(string(m.ID)):
		registryErrs = append(registryErrs,
			fmt.Errorf("%s: invalid doctor check ID %q, want group.name", site, m.ID))
		return
	case !knownGroup(m.ID.Group()):
		registryErrs = append(registryErrs,
			fmt.Errorf("%s: doctor check ID %q has unknown group %q", site, m.ID, m.ID.Group()))
		return
	case strings.TrimSpace(m.Summary) == "":
		registryErrs = append(registryErrs,
			fmt.Errorf("%s: doctor check %q has no Meta.Summary", site, m.ID))
		return
	}
	if prev, dup := regSites[m.ID]; dup {
		registryErrs = append(registryErrs, fmt.Errorf(
			"duplicate doctor check ID %q registered at %s and %s", m.ID, prev, site))
		return
	}
	regSites[m.ID] = site
	registry = append(registry, c)
}

// Registered returns every registered check in deterministic run order:
// group order first (RULING R1), then ID. Go's cross-file init order within a
// package is not something to rely on, so run order is imposed here, not
// inherited. The returned slice is a copy.
func Registered() []Check {
	out := append([]Check(nil), registry...)
	sortChecks(out)
	return out
}

func sortChecks(cs []Check) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i].Meta().ID, cs[j].Meta().ID
		if ra, rb := groupRank(a.Group()), groupRank(b.Group()); ra != rb {
			return ra < rb
		}
		return a < b
	})
}

// RegistryErr reports registration problems. Non-nil => Run refuses to execute
// anything and the report is Incomplete (exit 2).
func RegistryErr() error { return errors.Join(registryErrs...) }
