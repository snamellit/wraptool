package introspect

import (
	"context"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/executor"
)

// CatalogProbe is the result of probing a tool's subcommand catalog with a
// specific, per-tool argv known (outside this package — internal/doctor's
// per-tool knowledge table, CatalogArgv) to be exhaustive.
//
// `-h` is NOT a catalog: getHelp tries it first and returns it once output
// exceeds 20 bytes, but `git -h` lists only the "common" commands — verified
// on git 2.54: stash, config, remote, worktree, rev-parse, cherry-pick, and
// submodule are all absent from it (see testdata/git_help.txt vs
// testdata/git_help_a.txt, the `git help -a` catalog). Treating `-h`'s
// output as a catalog would report `git config` as a subcommand that does
// not exist. See design/doctor/wraptool-doctor.md §5.3.
type CatalogProbe struct {
	// Catalog is the parsed subcommand name -> description map. Only
	// meaningful when Exhaustive is true.
	Catalog map[string]string
	// Exhaustive reports whether Catalog may be trusted as this tool's
	// COMPLETE subcommand listing. false, with Catalog == nil, for every
	// case that is not a genuine, non-empty, per-tool-vetted catalog parse —
	// "unknown", never a partial list a caller could mistake for complete.
	Exhaustive bool
}

// ProbeCatalog runs argv against tc's binary — under the same
// executor.ChildEnv(tc.Binary, nil) environment as every other probe, see
// RunProbe's doc comment — and parses the result as a subcommand catalog.
//
// Exhaustive is false (Catalog == nil) for every input that is not a
// genuinely trustworthy, non-empty catalog:
//
//   - argv is empty — no catalog argv is known for this tool at all. A tool
//     with no entry in internal/doctor's per-tool knowledge table (or one
//     whose entry has no CatalogArgv) must never fall back to a partial
//     source, per the design's own bounding rule (§5.3): "unknown", not a
//     guess.
//   - the probe errored (missing binary, no output at all).
//   - the probe did not exit cleanly: it never produced an exit status at
//     all (failed to start, or was killed by a signal), or runProbeEnv's own
//     10s context timeout killed it mid-listing, or it exited non-zero. Any
//     of these can hand back PARTIAL output — a catalog command that was
//     cut off produces a truncated listing, not an error page, so byte
//     length alone can't catch it. This is the fix for the single most
//     damaging defect in this package: swallowing exit status let a
//     killed-mid-listing `git help -a` be recorded as EXHAUSTIVE, which is
//     what made drift report `git config` as a subcommand git does not
//     have — a partial catalog mistaken for a complete one.
//   - the output does not have the structural shape of real listing text
//     (looksLikeHelp's positive-structure bound — see its own doc comment):
//     a one-line CLI error page (wrong subcommand, wrong flag) must not
//     become a catalog any more than it may become parsed help.
//   - the output parsed to zero subcommands. A catalog argv that produced no
//     recognizable listing is not evidence the tool HAS no subcommands — an
//     upstream help-format change, for instance — so it must not be reported
//     as "this tool has an empty catalog".
//
// A caller (the later drift.unknown-subcommand check) MUST treat
// Exhaustive == false as drift.unverifiable, never as "no subcommands
// exist".
func ProbeCatalog(ctx context.Context, tc *config.ToolConfig, argv []string) CatalogProbe {
	if len(argv) == 0 {
		return CatalogProbe{}
	}

	output, result, err := runProbeEnv(ctx, tc, argv, executor.ChildEnv(tc.Binary, nil))
	if err != nil {
		return CatalogProbe{}
	}
	if !result.Exited || result.DeadlineExceeded || result.ExitCode != 0 {
		return CatalogProbe{}
	}
	if !looksLikeHelp(output) {
		return CatalogProbe{}
	}

	catalog := BestParseCatalog(output)
	if len(catalog) == 0 {
		return CatalogProbe{}
	}

	return CatalogProbe{Catalog: catalog, Exhaustive: true}
}
