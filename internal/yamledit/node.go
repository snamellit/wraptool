// Package yamledit holds the yaml.Node primitives and generic edit driver
// shared by wraptool's two comment-preserving node-tree editors:
// internal/config (config.yaml, consumed by ApplyRequest and the webui rule
// CRUD) and cmd/harness.go (the harness pool's manifest.yaml, consumed by
// `harness policy set` and `harness install`'s constraint write). Both files
// are user-owned, comment-friendly YAML that wraptool itself edits in
// place — the shared contract is: preserve comments and key order,
// round-trip validate before writing, and write atomically. What differs
// per caller — the typed validator, the file's permission bits, whether a
// missing file seeds an empty document or errors, and the atomic-write
// implementation itself — is parameterized on Options; the caller's own
// semantic validation (config.Config vs pool.Manifest) never lives here.
package yamledit

import "gopkg.in/yaml.v3"

// MapGet returns the value node for key on mapping node m, or nil if m is
// not a mapping node or has no such key.
func MapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// MapSet sets key's value on mapping node m. When key already exists AND
// both the existing and incoming values are plain scalars, only the
// existing node's Tag/Style/Value fields are overwritten; every other field
// on that node is left untouched — in particular its
// LineComment/HeadComment/FootComment and its yaml Anchor (yaml.v3 hangs
// both off the node, not the mapping), so editing a field like
// `minimum_release_age: 168h # seven days` keeps its comment, and a field
// like `value: &v 168h` keeps its anchor. One consequence of mutating in
// place rather than replacing: because a yaml.v3 alias node holds a
// pointer to the anchor's *yaml.Node rather than a copy of it, any alias
// elsewhere in the document that points at this node resolves to the new
// value too, once the document is next decoded — an alias updates with its
// anchor. A structural swap (e.g. a scalar value replaced by a mapping, or
// vice versa) still replaces the node wholesale — there is no scalar
// comment or anchor to preserve in that case, and the node shapes are
// incompatible.
func MapSet(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			existing := m.Content[i+1]
			if existing.Kind == yaml.ScalarNode && value.Kind == yaml.ScalarNode {
				existing.Tag = value.Tag
				existing.Style = value.Style
				existing.Value = value.Value
				return
			}
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, ScalarNode(key), value)
}

// EnsureMapChild returns the mapping node at key under parent, creating
// (and appending) an empty one if absent, and coercing an existing
// non-mapping value (e.g. a stray `harnesses: null`) into an empty mapping
// rather than erroring — the caller is always about to add fields under it.
// Coercion mutates the existing node in place, so any comment attached to
// it survives.
func EnsureMapChild(parent *yaml.Node, key string) *yaml.Node {
	if child := MapGet(parent, key); child != nil {
		if child.Kind != yaml.MappingNode {
			child.Kind = yaml.MappingNode
			child.Tag = "!!map"
			child.Value = ""
			child.Content = nil
		}
		return child
	}
	child := &yaml.Node{Kind: yaml.MappingNode}
	MapSet(parent, key, child)
	return child
}

// ScalarNode is an untagged plain scalar: the encoder resolves it to its
// natural YAML type (bool, duration-parseable string, etc.).
func ScalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: value}
}

// StrNode is a scalar explicitly tagged as a string, so the encoder quotes
// values that would otherwise misparse as YAML (a pattern starting with
// `*`, containing `:`, a version constraint like ">=2 <3" that starts with
// a folded-block-scalar indicator character, etc.). Use it for free-form
// text; ScalarNode's untagged form is right for barewords, ints, and bools
// that should resolve to their type.
func StrNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}
