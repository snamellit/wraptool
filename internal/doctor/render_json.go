package doctor

import (
	"encoding/json"
	"io"
)

// jsonReport is the wire shape of design/doctor/wraptool-doctor.md §9. It is a
// SEPARATE type from Report rather than json struct tags on Report itself:
// Report's Go-facing field names (Schema, ConfigPath, Version, Probed) do not
// match §9's wire names (schema, config_path, wraptool_version, probed), and
// Report is documented (doctor.go) as the engine's own result type, not a wire
// format — a future Report field earns no JSON exposure just by existing.
//
// Finding and SkipRecord already carry the right `json:` tags for §9 (set in
// finding.go/doctor.go respectively) and are embedded as-is.
//
// incomplete/incomplete_reason/config_state are ADDITIVE (§9, wave-1
// followups item 9): schema stays 1. Before these fields, a run that could
// not complete (an unknown --only selector, an unreadable config, an
// internal error) still emitted a structurally clean report on stdout — exit
// 2 named the cause only on stderr, so a CI consumer that parses stdout and
// never checks the process exit code reads it as a passing run.
type jsonReport struct {
	Schema           int          `json:"schema"`
	ConfigPath       string       `json:"config_path"`
	WraptoolVersion  string       `json:"wraptool_version"`
	Probed           bool         `json:"probed"`
	Incomplete       bool         `json:"incomplete"`
	IncompleteReason string       `json:"incomplete_reason"`
	ConfigState      string       `json:"config_state"`
	Summary          Summary      `json:"summary"`
	Findings         []Finding    `json:"findings"`
	Skipped          []SkipRecord `json:"skipped"`
}

// RenderJSON writes r as §9's JSON report to w, one object followed by a
// trailing newline (json.Encoder's own convention). cmd passes
// cmd.OutOrStdout() under --json; tests decode the bytes and assert on the
// decoded shape rather than byte-diffing, since map/slice key order needs no
// pinning here (unlike RenderHuman's grouped, human-read text).
//
// §9.1: the emitted `id` values are a compatibility surface — an ID may be
// added and its severity lowered, but never renamed or repurposed. See
// cmd/doctor.go's --json flag help.
func RenderJSON(w io.Writer, r Report) error {
	jr := jsonReport{
		Schema:           r.Schema,
		ConfigPath:       r.ConfigPath,
		WraptoolVersion:  r.Version,
		Probed:           r.Probed,
		Incomplete:       r.Incomplete,
		IncompleteReason: r.IncompleteReason,
		ConfigState:      string(r.ConfigState),
		Summary:          r.Summary,
		Findings:         r.Findings,
		Skipped:          r.Skipped,
	}
	// A nil slice marshals as `null`; §9's sample shows `"skipped": [...]` and
	// implies `"findings": []` on a clean run, not `null` — encode an explicit
	// empty array either way so a consumer never has to special-case null.
	if jr.Findings == nil {
		jr.Findings = []Finding{}
	}
	if jr.Skipped == nil {
		jr.Skipped = []SkipRecord{}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// doctor's JSON is never embedded in HTML: a detail or remedy legitimately
	// contains <, >, or & (a flag value, a shell redirect in an example), and
	// the default escaping would turn those into <-style sequences in a
	// report meant to be read or diffed as plain text.
	enc.SetEscapeHTML(false)
	return enc.Encode(jr)
}
