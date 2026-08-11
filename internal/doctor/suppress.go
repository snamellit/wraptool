// Suppress.go implements per-site suppression via YAML comments (design §6.2,
// step C2): a `# doctor:ignore <id> -- reason` comment placed immediately
// before a config node waives every matching WARN finding located within
// that node's subtree, without deleting the underlying rule.
//
// Two placements are supported, and — verified directly against real
// yaml.v3 attachment behaviour rather than assumed (an earlier design draft
// guessed wrong here) — they attach to DIFFERENT nodes:
//
//   - Before a sequence item (`- subcommand: [pull]`): yaml.v3 attaches the
//     comment as HeadComment on the ITEM'S OWN MAPPING NODE. It suppresses
//     every matching finding anywhere within that one rule.
//   - Before a mapping key (`git:`): yaml.v3 attaches the comment as
//     HeadComment on the KEY SCALAR, not on the key's value node. It
//     suppresses every matching finding within that key's whole value
//     subtree (every rule under `git:`, not just one).
//
// Both placements are handled the SAME way here: suppressWalk records the
// LINE RANGE of the subtree each directive decorates (the decorated node's
// own line through the deepest line reachable inside it), and suppression is
// then decided by line-range containment against the finding's own resolved
// Location — the exact granularity C1's Locator already reports findings at
// (file:line, never file:line:col), so this adds no precision the rest of
// the report does not already have. It also means this file never needs to
// re-derive ConfigPath's by-value traversal (locate.go's own concern, kept
// there): a directive's node is provably an ancestor of a finding's resolved
// node whenever the finding's line falls inside the directive's computed
// range, because both are read from the SAME parsed document tree.
package doctor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

const suppressUnknownID CheckID = "config.unknown-suppression"

func init() {
	register(Meta{
		ID:    suppressUnknownID,
		Needs: NeedsConfig,
		Summary: "flags a `# doctor:ignore <id>` comment naming a check ID that is " +
			"not in the catalog",
	}, runSuppressUnknownID)
}

// ---------------------------------------------------------------- directives

// suppressIgnoreText is one parsed "doctor:ignore <id> [-- reason]" comment
// LINE, before its decorated node's line range is known.
type suppressIgnoreText struct {
	ID     CheckID
	Reason string
}

// suppressParseComment parses every `# doctor:ignore ...` line out of a
// yaml.v3 HeadComment, which may hold several stacked `#`-prefixed lines
// joined by "\n" (verified in suppress_test.go). A line that is not a
// doctor:ignore directive — including ordinary prose that merely happens to
// mention the phrase, e.g. "# see doctor:ignore usage" — is silently
// skipped: doctor must not warn about every unrelated comment in the file,
// only about a directive it recognizes as one and cannot resolve.
func suppressParseComment(comment string) []suppressIgnoreText {
	if comment == "" {
		return nil
	}
	var out []suppressIgnoreText
	for _, line := range strings.Split(comment, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		rest, ok := strings.CutPrefix(line, "doctor:ignore")
		if !ok {
			continue
		}
		// A word-boundary check, not a bare prefix match: "doctor:ignoreme"
		// is prose, not a directive naming ID "me".
		if rest != "" && !unicode.IsSpace(rune(rest[0])) {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" {
			continue // "doctor:ignore" with no ID names nothing to suppress
		}
		id, reason, _ := strings.Cut(rest, "--")
		out = append(out, suppressIgnoreText{
			ID:     CheckID(strings.TrimSpace(id)),
			Reason: strings.TrimSpace(reason),
		})
	}
	return out
}

// suppressDirective is one directive resolved against the document: its
// check ID, reason text, and the line range of the node it decorates.
type suppressDirective struct {
	ID        CheckID
	Reason    string
	StartLine int
	EndLine   int // inclusive
	Line      int // the directive comment's own attachment line, for config.unknown-suppression's Detail
}

// covers reports whether line falls within d's decorated subtree.
func (d suppressDirective) covers(line int) bool {
	return line >= d.StartLine && line <= d.EndLine
}

// ---------------------------------------------------------------- tree walk

// suppressWalk recursively scans n for doctor:ignore directives at both
// supported placements and appends each into out, then returns the deepest
// line reachable within n's own subtree. The return value is what lets a
// caller decorating an ANCESTOR node compute that ancestor's own end line
// correctly — a directive on `tools:` must cover everything nested under
// it, several levels down.
//
// Directives are appended in POST-order (a node's own descendants are fully
// walked, and their directives appended, before the node's own directive
// is): suppressMatch takes the FIRST matching entry, so the innermost,
// most specific directive wins when directives at different levels happen
// to name the same check ID. This is deterministic by construction — the
// walk never ranges over a map — so it needs no separate tie-breaking rule.
//
// The switch names every yaml.Kind member (not a default: catch-all) so the
// exhaustive linter forces a decision here if yaml.v3 ever adds one, mirroring
// locate.go's locateFieldValue.
func suppressWalk(n *yaml.Node, out *[]suppressDirective) int {
	if n == nil {
		return 0
	}
	maxLine := n.Line

	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			if m := suppressWalk(c, out); m > maxLine {
				maxLine = m
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			childMax := suppressWalk(value, out)
			if childMax > maxLine {
				maxLine = childMax
			}
			if key.Line > maxLine {
				maxLine = key.Line
			}
			for _, txt := range suppressParseComment(key.HeadComment) {
				*out = append(*out, suppressDirective{
					ID: txt.ID, Reason: txt.Reason,
					StartLine: key.Line, EndLine: childMax, Line: key.Line,
				})
			}
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			childMax := suppressWalk(item, out)
			if childMax > maxLine {
				maxLine = childMax
			}
			// Only a MAPPING sequence item carries the "before a sequence
			// item" placement (design §6.2's own example: an allow rule). A
			// scalar sequence element (a bare flag inside `flags: [...]`) is
			// not one of the two supported placements — see this file's own
			// package doc comment — so it is never scanned as a directive
			// site, deliberately.
			if item.Kind == yaml.MappingNode {
				for _, txt := range suppressParseComment(item.HeadComment) {
					*out = append(*out, suppressDirective{
						ID: txt.ID, Reason: txt.Reason,
						StartLine: item.Line, EndLine: childMax, Line: item.Line,
					})
				}
			}
		}
	case yaml.ScalarNode, yaml.AliasNode:
		// No children, and neither placement attaches a directive to a
		// scalar/alias node itself — see the SequenceNode case above.
	}

	return maxLine
}

// suppressParseRoot parses raw the SAME way locate.go's NewLocator does
// (locateRoot's own doc comment covers every degenerate case: empty,
// unparseable, non-mapping-root), returning nil rather than panicking or
// guessing. Reusing locateRoot rather than re-deriving it is what keeps this
// file from duplicating C1's document-shape handling; the yaml.Unmarshal
// call itself is unavoidably repeated (a second parse of the same bytes,
// same cost NewLocator already accepts — not a hot path).
func suppressParseRoot(raw []byte) *yaml.Node {
	if len(raw) == 0 {
		return nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	return locateRoot(&doc)
}

// suppressDirectivesIn walks raw's whole document and returns every
// doctor:ignore directive found, regardless of whether any currently-emitted
// finding matches it — config.unknown-suppression needs the full set, not
// just the ones a finding happens to reference.
func suppressDirectivesIn(raw []byte) []suppressDirective {
	root := suppressParseRoot(raw)
	if root == nil {
		return nil
	}
	var out []suppressDirective
	suppressWalk(root, &out)
	return out
}

// suppressMatch returns the reason of the first directive (innermost wins,
// see suppressWalk) naming id whose range covers line.
func suppressMatch(directives []suppressDirective, id CheckID, line int) (string, bool) {
	for _, d := range directives {
		if d.ID == id && d.covers(line) {
			return d.Reason, true
		}
	}
	return "", false
}

// suppressLocationLine extracts the trailing line number from a resolved
// "file:line" Location, splitting on the LAST ":" so a Windows drive-letter
// path ("C:\...\config.yaml:37") still parses correctly.
func suppressLocationLine(location string) (int, bool) {
	idx := strings.LastIndex(location, ":")
	if idx < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(location[idx+1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------- entry point

// ApplySuppression is C2's own wiring point, mirroring C1's NewLocator:
// cmd/doctor.go calls it once, right after doctor.Run(), with the SAME
// ConfigRaw bytes already read for config.Load/NewLocator. It returns r with
// every WARN-severity finding suppressed by a matching config comment (§6.2)
// marked Suppressed, its SuppressionReason set, and Summary.Suppressed
// advanced to match — a suppressed finding is still counted (Suppressed is a
// SUBSET of Warn, never subtracted from it) but RenderHuman no longer prints
// it, while RenderJSON always includes it with "suppressed": true.
//
// Only SeverityWarn is suppressible (§6.2): every FAIL in the catalog was
// re-checked against "never fires on a configuration working as designed",
// so a fail is provably a broken invariant, never a judgement call — an
// ignore comment near a fail-severity finding's location is simply inert.
//
// A finding whose Location never resolved (an empty Path, or a Path C1's
// Locator could not match) has no line to anchor a comment lookup to and is
// never suppressed — the same "no guessed line" posture locate.go itself
// takes, rather than falling back to in.ConfigPath with a guessed line.
//
// raw that is empty, unparseable, or has no doctor:ignore directives at all
// is a safe, cheap no-op — mirroring NewLocator's own degrade-to-nothing
// behaviour: never a panic, never a guess.
func ApplySuppression(r Report, raw []byte) Report {
	if len(r.Findings) == 0 {
		return r
	}
	directives := suppressDirectivesIn(raw)
	if len(directives) == 0 {
		return r
	}

	findings := append([]Finding(nil), r.Findings...)
	for i := range findings {
		f := &findings[i]
		if f.Severity != SeverityWarn || f.Location == "" {
			continue
		}
		line, ok := suppressLocationLine(f.Location)
		if !ok {
			continue
		}
		if reason, ok := suppressMatch(directives, f.ID, line); ok {
			f.Suppressed = true
			f.SuppressionReason = reason
			r.Summary.Suppressed++
		}
	}
	r.Findings = findings
	return r
}

// ---------------------------------------------------------------- config.unknown-suppression

// runSuppressUnknownID is the one check that scans the WHOLE document (not
// just the location of a finding some other check already emitted): an
// operator who misspells a check ID in a doctor:ignore comment otherwise
// believes they have suppressed something and finds out only when the
// "suppressed" finding keeps showing up anyway — silently confusing, never
// silently DANGEROUS (the underlying finding is never actually hidden, since
// its ID never matched), which is why this is a warn, not a fail.
//
// This check reports one finding per OCCURRENCE (not one per distinct
// misspelled ID): each occurrence is its own comment at its own line, and an
// operator fixing one should not have to search for whether another exists
// elsewhere with the same typo.
//
// Its Detail embeds the offending line directly as text rather than through
// Finding.Path/Location: a doctor:ignore comment has no config VALUE to
// anchor a ConfigPath to (ConfigPath resolves by value, per §6.1, and a
// comment is not a value), so there is no Path this check could set — and
// hand-rolling Location from in.ConfigPath is exactly what plan §1 rule 9
// forbids. The line number is still real, read from the same parsed tree
// suppressWalk already built, not a guess.
func runSuppressUnknownID(_ context.Context, in *Input) Result {
	if len(in.ConfigRaw) == 0 {
		return Skipped("no config bytes available to scan for doctor:ignore comments")
	}
	// suppressParseRoot, not suppressDirectivesIn, so a genuinely clean
	// config (parses fine, zero doctor:ignore comments — the overwhelmingly
	// common case) is distinguishable from an UNPARSEABLE one: both would
	// otherwise yield the same nil directive slice, and reporting the clean
	// case as "skipped: could not parse" would be exactly the "did not
	// look" false signal §6 forbids.
	root := suppressParseRoot(in.ConfigRaw)
	if root == nil {
		return Skipped("config could not be parsed for doctor:ignore comments")
	}
	var directives []suppressDirective
	suppressWalk(root, &directives)

	var findings []Finding
	for _, d := range directives {
		if _, known := regSites[d.ID]; known {
			continue
		}
		findings = append(findings, Finding{
			Severity: SeverityWarn,
			Title:    "doctor:ignore comment names an unknown check ID",
			Detail: fmt.Sprintf(
				"%s:%d — a `# doctor:ignore %s` comment names a check ID that is not in the catalog; it suppresses nothing",
				in.ConfigPath, d.Line, d.ID),
			Remedy: "fix the check ID (see the catalog in design/doctor/wraptool-doctor.md §7), or remove the comment if it no longer applies",
			DocRef: "design/doctor/wraptool-doctor.md#62-suppression",
		})
	}
	return Found(findings...)
}
