package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// ApplyResult describes the outcome of merging a capability request into
// the config file.
type ApplyResult struct {
	// NewRule is true when a new allow rule was appended; false when an
	// existing rule was extended with additional flags.
	NewRule bool
	// AddedFlags lists flags that were added to the rule. Empty if the
	// rule already covered every requested flag.
	AddedFlags []string
	// Snippet is the YAML representation of the resulting allow rule,
	// suitable for printing to the operator.
	Snippet string
}

// ApplyRequest merges a capability request into the config file at path.
// It preserves comments and key ordering by editing the YAML node tree.
// The file is validated before being written and replaced atomically.
//
// Errors are returned without modifying the file when:
//   - The tool is not configured (caller must add a `binary` entry first).
//   - A deny rule for the same subcommand exists (conflict).
//   - The post-edit config fails validation.
func ApplyRequest(path, toolName string, subcommand, flags []string, flagTypes map[string]string) (*ApplyResult, error) {
	result := &ApplyResult{}
	var ruleNode *yaml.Node

	err := editConfig(path, func(doc *yaml.Node) error {
		toolNode, err := docToolNode(doc, toolName)
		if err != nil {
			return err
		}

		if denyNode := mapGet(toolNode, "deny"); denyNode != nil && denyNode.Kind == yaml.SequenceNode {
			for _, rule := range denyNode.Content {
				if rule.Kind != yaml.MappingNode {
					continue
				}
				sub := mapGet(rule, "subcommand")
				if sub != nil && stringSeqEqual(sub, subcommand) {
					return fmt.Errorf("subcommand %v is denied for tool %q; remove the deny rule first", subcommand, toolName)
				}
			}
		}

		allowNode := mapGet(toolNode, "allow")
		if allowNode == nil {
			allowNode = &yaml.Node{Kind: yaml.SequenceNode}
			mapSet(toolNode, "allow", allowNode)
		} else if allowNode.Kind != yaml.SequenceNode {
			return fmt.Errorf("tool %q `allow` is not a sequence", toolName)
		}

		for _, rule := range allowNode.Content {
			if rule.Kind != yaml.MappingNode {
				continue
			}
			sub := mapGet(rule, "subcommand")
			if sub != nil && stringSeqEqual(sub, subcommand) {
				ruleNode = rule
				break
			}
		}

		if ruleNode == nil {
			ruleNode = newAllowRule(subcommand, flags, flagTypes)
			allowNode.Content = append(allowNode.Content, ruleNode)
			result.NewRule = true
			result.AddedFlags = append([]string(nil), flags...)
			return nil
		}

		flagsNode := mapGet(ruleNode, "flags")
		if flagsNode == nil {
			flagsNode = &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
			mapSet(ruleNode, "flags", flagsNode)
		} else if flagsNode.Kind != yaml.SequenceNode {
			return fmt.Errorf("tool %q allow rule `flags` is not a sequence", toolName)
		}
		existing := make(map[string]bool, len(flagsNode.Content))
		for _, f := range flagsNode.Content {
			existing[f.Value] = true
		}
		for _, f := range flags {
			if existing[f] {
				continue
			}
			flagsNode.Content = append(flagsNode.Content, scalarNode(f))
			existing[f] = true
			result.AddedFlags = append(result.AddedFlags, f)
		}
		applyFlagTypes(ruleNode, flags, flagTypes)
		return nil
	})
	if err != nil {
		return nil, err
	}

	if snippet, err := marshalNode(ruleNode); err == nil {
		result.Snippet = snippet
	}
	return result, nil
}

// editConfig reads the config at path, parses it into a YAML node tree
// (preserving comments and ordering), hands the document's root mapping node
// to mutate, then re-encodes, round-trip validates through the typed loader,
// and writes the result atomically. The file is left untouched if mutate or
// validation returns an error.
func editConfig(path string, mutate func(doc *yaml.Node) error) error {
	path = expandPath(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return fmt.Errorf("config is not a YAML document")
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return fmt.Errorf("config root is not a mapping")
	}

	if err := mutate(doc); err != nil {
		return err
	}

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return fmt.Errorf("re-encoding config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("re-encoding config: %w", err)
	}

	// Validate by round-tripping through the typed loader.
	var newCfg Config
	if err := yaml.Unmarshal(out.Bytes(), &newCfg); err != nil {
		return fmt.Errorf("validating edited config: %w", err)
	}
	newCfg.applyDefaults()
	if err := newCfg.Validate(); err != nil {
		return fmt.Errorf("edited config failed validation: %w", err)
	}

	return writeAtomic(path, out.Bytes())
}

// docToolNode returns the mapping node for the named tool under `tools`.
func docToolNode(doc *yaml.Node, toolName string) (*yaml.Node, error) {
	toolsNode := mapGet(doc, "tools")
	if toolsNode == nil || toolsNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("no `tools` mapping in config")
	}
	toolNode := mapGet(toolsNode, toolName)
	if toolNode == nil {
		return nil, fmt.Errorf("tool %q is not configured; add it to the config first (binary path is required)", toolName)
	}
	if toolNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("tool %q entry is not a mapping", toolName)
	}
	return toolNode, nil
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
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

func mapSet(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, scalarNode(key), value)
}

func stringSeqEqual(seq *yaml.Node, want []string) bool {
	if seq.Kind != yaml.SequenceNode || len(seq.Content) != len(want) {
		return false
	}
	for i, item := range seq.Content {
		if item.Value != want[i] {
			return false
		}
	}
	return true
}

func newAllowRule(subcommand, flags []string, flagTypes map[string]string) *yaml.Node {
	rule := &yaml.Node{Kind: yaml.MappingNode}
	mapSet(rule, "subcommand", stringSeq(subcommand))
	if len(flags) > 0 {
		mapSet(rule, "flags", stringSeq(flags))
	}
	applyFlagTypes(rule, flags, flagTypes)
	return rule
}

// applyFlagTypes adds `flag_constraints: {flag: {type: T}}` on rule for each flag
// in `flags` that has a concrete type in flagTypes. It never overwrites an
// existing constraint entry — an operator-authored (or previously applied) type
// wins over a fresh request. Called by ApplyRequest so an approved capability's
// requester-supplied types become authoritative flag_constraints, overriding the
// introspected type. See design/flag-type-inference.md.
func applyFlagTypes(rule *yaml.Node, flags []string, flagTypes map[string]string) {
	if len(flagTypes) == 0 {
		return
	}
	fcNode := mapGet(rule, "flag_constraints")
	for _, f := range flags {
		t, ok := flagTypes[f]
		if !ok || t == "" {
			continue
		}
		if fcNode == nil {
			fcNode = &yaml.Node{Kind: yaml.MappingNode}
			mapSet(rule, "flag_constraints", fcNode)
		}
		if mapGet(fcNode, f) != nil {
			continue // don't clobber an existing constraint
		}
		entry := &yaml.Node{Kind: yaml.MappingNode}
		mapSet(entry, "type", scalarNode(t))
		mapSet(fcNode, f, entry)
	}
}

func stringSeq(values []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, v := range values {
		n.Content = append(n.Content, scalarNode(v))
	}
	return n
}

func scalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: value}
}

// strNode is a scalar explicitly tagged as a string, so the encoder quotes
// values that would otherwise misparse as YAML (a pattern starting with `*`,
// containing `:`, etc.). Use it for free-form text; scalarNode's untagged form
// is right for barewords, ints, and bools that should resolve to their type.
func strNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func intNode(i int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: strconv.Itoa(i)}
}

func boolNode(b bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: strconv.FormatBool(b)}
}

func marshalNode(n *yaml.Node) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return "", err
	}
	enc.Close()
	return buf.String(), nil
}

// writeAtomic writes data to a temp file in the same directory and renames
// it over the target path. Using the same directory guarantees rename(2)
// is atomic and stays on the same filesystem.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".wraptool-config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		cleanup()
		return fmt.Errorf("setting permissions on temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp file over %s: %w", path, err)
	}
	return nil
}
