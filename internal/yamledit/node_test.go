package yamledit

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func decodeMapping(t *testing.T, doc string) *yaml.Node {
	t.Helper()
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		t.Fatalf("not a document: %#v", root)
	}
	m := root.Content[0]
	if m.Kind != yaml.MappingNode {
		t.Fatalf("root is not a mapping: %#v", m)
	}
	return m
}

func TestMapGet(t *testing.T) {
	m := decodeMapping(t, "a: 1\nb: 2\n")
	if got := MapGet(m, "b"); got == nil || got.Value != "2" {
		t.Errorf("MapGet(b) = %v, want 2", got)
	}
	if got := MapGet(m, "missing"); got != nil {
		t.Errorf("MapGet(missing) = %v, want nil", got)
	}
	if got := MapGet(nil, "a"); got != nil {
		t.Errorf("MapGet(nil, ...) = %v, want nil", got)
	}
	scalar := ScalarNode("x")
	if got := MapGet(scalar, "a"); got != nil {
		t.Errorf("MapGet on a non-mapping node = %v, want nil", got)
	}
}

func TestMapSet_AppendsNewKey(t *testing.T) {
	m := decodeMapping(t, "a: 1\n")
	MapSet(m, "b", ScalarNode("2"))
	if got := MapGet(m, "b"); got == nil || got.Value != "2" {
		t.Errorf("MapGet(b) after MapSet = %v, want 2", got)
	}
	if len(m.Content) != 4 {
		t.Errorf("Content length = %d, want 4 (2 keys)", len(m.Content))
	}
}

// TestMapSet_PreservesInlineCommentOnScalarEdit is the core dedup fix: the
// harness-side mapSetNode already did this (fixed in MR !59 review); the
// config-side mapSet did not (same bug, unfixed) until both editors shared
// this function. See internal/config/tools_test.go's
// TestSetToolMetadata_PreservesInlineCommentOnEditedScalar for the same
// guarantee exercised through a real config consumer.
func TestMapSet_PreservesInlineCommentOnScalarEdit(t *testing.T) {
	m := decodeMapping(t, "value: 168h # seven days\n")
	existing := MapGet(m, "value")
	if existing.LineComment != "# seven days" {
		t.Fatalf("test setup: LineComment = %q, want %q", existing.LineComment, "# seven days")
	}

	MapSet(m, "value", ScalarNode("72h"))

	got := MapGet(m, "value")
	if got != existing {
		t.Fatalf("MapSet replaced the node instead of mutating it in place")
	}
	if got.Value != "72h" {
		t.Errorf("Value = %q, want 72h", got.Value)
	}
	if got.LineComment != "# seven days" {
		t.Errorf("LineComment = %q, want preserved %q", got.LineComment, "# seven days")
	}
}

// TestMapSet_StructuralReplacementFallback: when the existing value and the
// incoming value are not both scalars, MapSet replaces the node wholesale —
// there is no scalar comment to preserve, and the node shapes (e.g. scalar
// vs mapping) are incompatible with an in-place field copy.
func TestMapSet_StructuralReplacementFallback(t *testing.T) {
	m := decodeMapping(t, "value: 168h # will be dropped\n")
	newMapping := &yaml.Node{Kind: yaml.MappingNode}
	MapSet(m, "value", newMapping)

	got := MapGet(m, "value")
	if got != newMapping {
		t.Errorf("MapSet did not replace the node on a structural change")
	}
	if got.Kind != yaml.MappingNode {
		t.Errorf("Kind = %v, want MappingNode", got.Kind)
	}
}

func TestMapSet_ScalarReplacedByStructuralValue_ReplacesWholesale(t *testing.T) {
	m := decodeMapping(t, "child: leaf\n")
	seq := &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{ScalarNode("a")}}
	MapSet(m, "child", seq)
	got := MapGet(m, "child")
	if got != seq || got.Kind != yaml.SequenceNode {
		t.Errorf("expected wholesale replacement with the sequence node, got %#v", got)
	}
}

func TestEnsureMapChild_CreatesWhenAbsent(t *testing.T) {
	m := decodeMapping(t, "a: 1\n")
	child := EnsureMapChild(m, "nested")
	if child.Kind != yaml.MappingNode {
		t.Fatalf("child.Kind = %v, want MappingNode", child.Kind)
	}
	if MapGet(m, "nested") != child {
		t.Error("EnsureMapChild did not attach the new mapping under parent")
	}
}

func TestEnsureMapChild_ReturnsExistingMapping(t *testing.T) {
	m := decodeMapping(t, "nested:\n  x: 1\n")
	child := EnsureMapChild(m, "nested")
	if MapGet(child, "x") == nil || MapGet(child, "x").Value != "1" {
		t.Error("EnsureMapChild lost the existing mapping's content")
	}
}

// TestEnsureMapChild_CoercesNonMappingInPlace covers a stray `key: null` (or
// any scalar) under a key the caller is about to add fields to: it must
// become an empty mapping rather than erroring, and any comment attached to
// that node must survive the coercion (comments hang off the node itself,
// which EnsureMapChild mutates in place rather than replacing).
func TestEnsureMapChild_CoercesNonMappingInPlace(t *testing.T) {
	m := decodeMapping(t, "harnesses: null # keep me\n")
	before := MapGet(m, "harnesses")

	child := EnsureMapChild(m, "harnesses")
	if child != before {
		t.Fatalf("EnsureMapChild replaced the node instead of coercing it in place")
	}
	if child.Kind != yaml.MappingNode {
		t.Errorf("Kind = %v, want MappingNode after coercion", child.Kind)
	}
	if child.LineComment != "# keep me" {
		t.Errorf("LineComment = %q, want preserved %q", child.LineComment, "# keep me")
	}
	if len(child.Content) != 0 {
		t.Errorf("coerced mapping should start empty, got %d content nodes", len(child.Content))
	}
}

func TestScalarNode_UntaggedResolvesToNaturalType(t *testing.T) {
	n := ScalarNode("true")
	if n.Tag != "" {
		t.Errorf("Tag = %q, want empty (untagged)", n.Tag)
	}
	if n.Value != "true" {
		t.Errorf("Value = %q, want true", n.Value)
	}
}

func TestStrNode_TaggedAsString(t *testing.T) {
	n := StrNode(">=2 <3")
	if n.Tag != "!!str" {
		t.Errorf("Tag = %q, want !!str", n.Tag)
	}
	if n.Value != ">=2 <3" {
		t.Errorf("Value = %q, want >=2 <3", n.Value)
	}
}
