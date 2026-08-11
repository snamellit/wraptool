package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/pti/wraptool/internal/config"
)

// config.parse and config.validate (design §7.2) are the strict-decode and
// semantic-validation halves of config integrity. Both reuse
// config.LoadStrict/Config.Validate — the SAME code path `wraptool config
// validate` drives (cmd/validate.go) — rather than reimplementing YAML
// decoding or the validation rules here.
//
// This is the sanctioned host.go escape hatch (see Env's doc comment, plan §1
// rule 10): config.LoadStrict os.Stats the token/binary files and
// config.Validate os.Stats auth-token files and resolves+stats tool binaries,
// all directly against the real filesystem, bypassing Env entirely. Both
// checks are therefore tested against a t.TempDir() fixture tree in
// config_load_test.go, never against a MapEnv.
const (
	configLoadParseID    CheckID = "config.parse"
	configLoadValidateID CheckID = "config.validate"
)

func init() {
	register(Meta{
		ID: configLoadParseID,
		// NeedsConfig: Input.Config != nil means the engine's own LENIENT
		// config.Load already succeeded against in.ConfigPath (cmd/doctor.go),
		// so this check has a real file to re-read strictly. A config that is
		// absent or wholly unreadable is the engine's own ConfigUnreadable path
		// (§10.1) and needs no finding from this check to also reach exit 2.
		Needs:   NeedsConfig,
		Summary: "strict-decodes the config, catching unknown or misspelled keys",
	}, runConfigLoadParse)

	register(Meta{
		ID:      configLoadValidateID,
		Needs:   NeedsConfig,
		Summary: "runs Config.Validate() and reports the first semantic problem",
	}, runConfigLoadValidate)
}

// runConfigLoadParse fails (and ABORTS the run, §10.1: exit 2 after the
// env.* group) when config.LoadStrict rejects the config for unknown or
// misspelled keys — audit F1's class. It never re-implements the decoder:
// the Finding's Detail is LoadStrict's own error, the same text `wraptool
// config validate` prints.
func runConfigLoadParse(_ context.Context, in *Input) Result {
	_, err := config.LoadStrict(in.ConfigPath)
	if err == nil {
		return Pass()
	}
	if configLoadFileWentAway(err) {
		// The engine's lenient config.Load succeeded against this exact path
		// moments before this check ran (that is what NeedsConfig gates on), so
		// a failure here that is really "the file could not be read at all" is
		// a TOCTOU race, not a strict-decode content problem — config.parse's
		// own domain. Reporting a decode failure that never actually happened
		// would be worse than saying nothing; the engine's own ConfigUnreadable
		// handling (§10.1) already covers "can't read the config file".
		return Skipped("config file could not be re-read for strict decoding: %v", err)
	}
	return Abort(err, Finding{
		Severity: SeverityFail,
		Title:    "Config fails strict decode",
		Detail:   fmt.Sprintf("wraptool config validate would reject this file: %v", err),
		Remedy:   "fix or remove the unrecognized/misspelled key(s) named above",
		DocRef:   "docs/configuration.qmd#validating-a-config",
	})
}

// runConfigLoadValidate fails when Config.Validate() rejects an otherwise
// strictly-decodable config. It re-runs config.LoadStrict itself rather than
// depending on config.parse having already run in this same pass: --only
// config.validate is a legal, independent selection (§9.1's group-level CI
// gating), and Config.Validate() must see the STRICTLY decoded config, not
// in.Config, which is the lenient, defaulted config the engine already
// loaded and may not agree with what the operator actually wrote (e.g. a
// misspelled key silently dropped).
//
// Config.Validate returns a single error and stops at its first failure
// (internal/config/validate.go) — a known upstream gap out of this step's
// scope (plan §1 rule 6) — so this emits exactly one finding and says so in
// its Detail, per design §7.2.
func runConfigLoadValidate(_ context.Context, in *Input) Result {
	cfg, err := config.LoadStrict(in.ConfigPath)
	if err != nil {
		// config.parse owns reporting a strict-decode failure; skip here rather
		// than emit a second, redundant finding when both checks run together,
		// and degrade gracefully to skipped when only this check is selected.
		return Skipped("config could not be strict-decoded, so semantic validation did not run: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		return Found(Finding{
			Severity: SeverityFail,
			Title:    "Config fails semantic validation",
			Detail: fmt.Sprintf(
				"%v (Config.Validate stops at the first problem, so fixing this may reveal another)", err),
			Remedy: "fix the problem named above, then re-run `wraptool config validate`",
			DocRef: "docs/configuration.qmd#validating-a-config",
		})
	}
	return Pass()
}

// configLoadFileWentAway reports whether err is config.LoadStrict failing to
// even READ the config file (wraps a *fs.PathError, e.g. ENOENT), as opposed
// to failing to DECODE its content. errors.Is sees through LoadStrict's own
// fmt.Errorf("reading config: %w", err) wrapping to the underlying
// *fs.PathError.
func configLoadFileWentAway(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
