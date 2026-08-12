package introspect

import (
	"context"

	"github.com/pti/wraptool/internal/config"
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

// ProbeCatalog runs argv against tc's binary and parses the result as a
// subcommand catalog, using the same env-injection contract as RunProbe (see
// its own doc comment for what env == nil/non-nil means).
//
// Exhaustive is false (Catalog == nil) for every input that is not a
// genuinely trustworthy, non-empty catalog:
//
//   - argv is empty — no catalog argv is known for this tool at all. A tool
//     with no entry in internal/doctor's per-tool knowledge table (or one
//     whose entry has no CatalogArgv) must never fall back to a partial
//     source, per the design's own bounding rule (§5.3): "unknown", not a
//     guess.
//   - the probe errored (missing binary, exec failure, timeout).
//   - the output parsed to zero subcommands. A catalog argv that produced no
//     recognizable listing is not evidence the tool HAS no subcommands — an
//     upstream help-format change, for instance — so it must not be reported
//     as "this tool has an empty catalog".
//
// A caller (the later drift.unknown-subcommand check) MUST treat
// Exhaustive == false as drift.unverifiable, never as "no subcommands
// exist".
func ProbeCatalog(ctx context.Context, tc *config.ToolConfig, argv []string, env []string) CatalogProbe {
	if len(argv) == 0 {
		return CatalogProbe{}
	}

	output, err := RunProbe(ctx, tc, argv, env)
	if err != nil {
		return CatalogProbe{}
	}

	catalog := BestParseSubcommands(output)
	if len(catalog) == 0 {
		return CatalogProbe{}
	}

	return CatalogProbe{Catalog: catalog, Exhaustive: true}
}
