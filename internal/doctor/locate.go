// Locate.go turns a Finding's structured ConfigPath (finding.go) into a
// concrete "file:line" Location by walking the config document's own
// yaml.Node tree. This is C1 (design/doctor/implementation-plan.md §3): it is
// wired into the run through doctor.RunOptions.Locator (installed by
// cmd/doctor.go, the ONE call site — doctor.go's engine already fills
// Finding.Location from Path via loc.Locate for every finding that sets Path,
// see doctor.go's Run and finding.go's Location doc comment / plan §1 rule 9)
// and touches no group file: every group file already builds a ConfigPath via
// At/Key/Elem/Item and sets Finding.Path, never Location, so there is nothing
// for this file to retrofit there.
package doctor

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// NewLocator parses raw as the config document and returns a Locator that
// resolves a ConfigPath against it, reporting file as every resolved Site's
// File. raw is expected to be the SAME bytes already read into
// Input.ConfigRaw — parsing them again here rather than reusing a
// pre-parsed tree keeps this the one and only place in the package that
// touches gopkg.in/yaml.v3's Node API, at the cost of one extra parse per
// run (a config file, not a generation tree — this is not a hot path).
//
// A raw that is empty, or that fails to parse as YAML, or that does not
// decode to a mapping document (e.g. a config file that is just `null` or a
// bare scalar) yields a Locator that resolves nothing — never a panic and
// never a guessed location. This covers every case so the caller cannot call
// NewLocator wrong: no config file read at all (ConfigRaw is nil), config
// that could not even be parsed leniently (B2's abort path), and any other
// document shape config.Load would itself have rejected before ever reaching
// a check.
func NewLocator(file string, raw []byte) Locator {
	if len(raw) == 0 {
		return nullLocator{}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nullLocator{}
	}
	root := locateRoot(&doc)
	if root == nil {
		return nullLocator{}
	}
	return locateLocator{file: file, root: root}
}

// locateRoot unwraps a parsed document down to its top-level mapping node —
// yaml.Unmarshal into a *yaml.Node always produces a DocumentNode whose sole
// Content entry is the real root. A document whose root is not a mapping
// (empty file, a bare scalar, a top-level sequence) has nothing a ConfigPath
// — which always starts with a Key step, config.Config being struct-typed —
// could ever resolve into, so this returns nil rather than a node no
// traversal step could ever match.
func locateRoot(doc *yaml.Node) *yaml.Node {
	n := doc
	if n != nil && n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	}
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

// locateLocator is the real Locator: a parsed document root plus the file
// path every resolved Site names.
type locateLocator struct {
	file string
	root *yaml.Node
}

// Locate walks p's steps from the document root, descending one step at a
// time and failing closed the moment any step cannot be matched — a
// partially-resolved path is exactly the "guessed line" finding.go's Location
// doc comment forbids, so an unresolved step aborts the whole lookup rather
// than falling back to whatever node was last reached.
//
// Every step matches by VALUE, never by position: a Key step matches a
// mapping's key scalar by name, an Elem step matches a scalar sequence
// element by its own value, and an Item step matches a sequence-of-mappings
// element by one of its own field's values (joining a sequence-shaped field
// with spaces first — R6, see locateFieldValue). This is what makes the
// result correct even though the LOADED config's slice indices do not match
// the file's (applyDefaults' SeedHelpFlags both appends -h/--help and drops
// deny_flags entries, plan §3/§6.1).
func (l locateLocator) Locate(p ConfigPath) (Site, bool) {
	if l.root == nil || p.Empty() {
		return Site{}, false
	}

	cur := l.root
	var key *yaml.Node
	for _, step := range p.steps {
		var value *yaml.Node
		var ok bool
		switch {
		case !step.seq:
			key, value, ok = locateMapChild(cur, step.Key)
		case step.Field == "":
			key = nil
			value, ok = locateSeqElem(cur, step.Value)
		default:
			key = nil
			value, ok = locateSeqItem(cur, step.Field, step.Value)
		}
		if !ok {
			return Site{}, false
		}
		cur = value
	}

	// A path ending on a Key step reports the KEY's line (where the operator
	// wrote e.g. "binary:"), which for a scalar value is the same line as the
	// value and for a nested mapping/sequence is the more useful anchor — the
	// start of that key's own block, not wherever its first child happens to
	// begin. A path ending on an Elem/Item step has no key scalar at all, so
	// it reports the matched node's own line (a sequence item's mapping node
	// Line is yaml.v3's line of its first key, i.e. exactly where that `-`
	// entry begins).
	line := cur.Line
	if key != nil {
		line = key.Line
	}
	return Site{File: l.file, Line: line, KeyNode: key, ValueNode: cur}, true
}

// locateMapChild returns the key and value nodes for key under mapping node
// m, or ok=false when m is not a mapping or has no such key — the ordinary,
// expected case for a key the defaulted config carries that the operator's
// own file never set (e.g. a struct field left at its zero value).
func locateMapChild(m *yaml.Node, key string) (keyNode, valueNode *yaml.Node, ok bool) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil, false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		if k.Kind == yaml.ScalarNode && k.Value == key {
			return k, m.Content[i+1], true
		}
	}
	return nil, nil, false
}

// locateSeqElem finds a SCALAR sequence element equal to value — e.g. an
// entry inside `flags: ["-c", "--depth"]`. First match wins if the operator
// wrote a duplicate, matching ConfigPath.Elem's own documented contract. A
// non-scalar element (there should never be one under a `flags`-shaped key,
// but a malformed document is not this function's problem to diagnose) is
// simply never a candidate.
func locateSeqElem(n *yaml.Node, value string) (*yaml.Node, bool) {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil, false
	}
	for _, item := range n.Content {
		if item.Kind == yaml.ScalarNode && item.Value == value {
			return item, true
		}
	}
	return nil, false
}

// locateSeqItem finds the mapping element in sequence n whose field renders
// (via locateFieldValue) to value — e.g. the allow rule whose `subcommand` is
// [pull], or whose `tool` is "delete_thing". First match wins, matching
// ConfigPath.Item's documented contract. The whole matched mapping node is
// returned, not just the matched field's value node: a path that stops at an
// Item step (rather than continuing with a further Key/Elem) means "this
// whole rule", and its Site should point at where that rule begins.
func locateSeqItem(n *yaml.Node, field, value string) (*yaml.Node, bool) {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil, false
	}
	for _, item := range n.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		_, fv, ok := locateMapChild(item, field)
		if !ok {
			continue
		}
		if rendered, ok := locateFieldValue(fv); ok && rendered == value {
			return item, true
		}
	}
	return nil, false
}

// locateFieldValue renders a mapping field's value node the way RULING R6
// requires: a bare scalar as itself, or a sequence of scalars joined with a
// single space — subcommand: [compute, instances, list] -> "compute
// instances list", matching strings.Join(rule.Subcommand, " ") exactly as the
// loaded config would spell it (config.AllowRule.Subcommand is []string).
// Anything else (a mapping, a nested document, an alias, or a sequence
// containing a non-scalar) has no defined rendering and reports ok=false
// rather than a partial join that could coincidentally match the wrong rule.
// The switch names every yaml.Kind member (not a default: catch-all) so the
// exhaustive linter (.golangci.yml) forces a decision here if yaml.v3 ever
// adds one.
func locateFieldValue(n *yaml.Node) (string, bool) {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value, true
	case yaml.SequenceNode:
		parts := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return "", false
			}
			parts = append(parts, c.Value)
		}
		return strings.Join(parts, " "), true
	case yaml.MappingNode, yaml.DocumentNode, yaml.AliasNode:
		return "", false
	default:
		return "", false
	}
}
