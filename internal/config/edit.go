package config

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/pti/wraptool/internal/yamledit"
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

		if denyNode := yamledit.MapGet(toolNode, "deny"); denyNode != nil && denyNode.Kind == yaml.SequenceNode {
			for _, rule := range denyNode.Content {
				if rule.Kind != yaml.MappingNode {
					continue
				}
				sub := yamledit.MapGet(rule, "subcommand")
				if sub != nil && stringSeqEqual(sub, subcommand) {
					return fmt.Errorf("subcommand %v is denied for tool %q; remove the deny rule first", subcommand, toolName)
				}
			}
		}

		allowNode := yamledit.MapGet(toolNode, "allow")
		if allowNode == nil {
			allowNode = &yaml.Node{Kind: yaml.SequenceNode}
			yamledit.MapSet(toolNode, "allow", allowNode)
		} else if allowNode.Kind != yaml.SequenceNode {
			return fmt.Errorf("tool %q `allow` is not a sequence", toolName)
		}

		for _, rule := range allowNode.Content {
			if rule.Kind != yaml.MappingNode {
				continue
			}
			sub := yamledit.MapGet(rule, "subcommand")
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

		flagsNode := yamledit.MapGet(ruleNode, "flags")
		if flagsNode == nil {
			flagsNode = &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
			yamledit.MapSet(ruleNode, "flags", flagsNode)
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
			flagsNode.Content = append(flagsNode.Content, yamledit.ScalarNode(f))
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
// validation returns an error. This is a thin wrapper over yamledit.Edit,
// which also drives cmd/harness.go's manifest.yaml editor — config supplies
// its own Validate (round-trip through the typed Config loader) and keeps
// the file's original 0600 permissions; the load/decode/mutate/encode/
// atomic-write shape itself lives in yamledit.
func editConfig(path string, mutate func(doc *yaml.Node) error) error {
	path = expandPath(path)
	return yamledit.Edit(path, yamledit.Options{
		Kind:     "config",
		FileMode: 0600,
		Validate: func(data []byte) error {
			var newCfg Config
			if err := yaml.Unmarshal(data, &newCfg); err != nil {
				return err
			}
			newCfg.applyDefaults()
			return newCfg.Validate()
		},
	}, mutate)
}

// docToolNode returns the mapping node for the named tool under `tools`.
func docToolNode(doc *yaml.Node, toolName string) (*yaml.Node, error) {
	toolsNode := yamledit.MapGet(doc, "tools")
	if toolsNode == nil || toolsNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("no `tools` mapping in config")
	}
	toolNode := yamledit.MapGet(toolsNode, toolName)
	if toolNode == nil {
		return nil, fmt.Errorf("tool %q is not configured; add it to the config first (binary path is required)", toolName)
	}
	if toolNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("tool %q entry is not a mapping", toolName)
	}
	return toolNode, nil
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
	yamledit.MapSet(rule, "subcommand", stringSeq(subcommand))
	if len(flags) > 0 {
		yamledit.MapSet(rule, "flags", stringSeq(flags))
	}
	applyFlagTypes(rule, flags, flagTypes)
	return rule
}

// applyFlagTypes adds `flag_constraints: {flag: {type: T}}` on rule for each flag
// in `flags` that has a concrete type in flagTypes. It never overwrites an
// existing constraint entry — an operator-authored (or previously applied) type
// wins over a fresh request. Called by ApplyRequest so an approved capability's
// requester-supplied types become authoritative flag_constraints, overriding the
// introspected type. See design/policy/flag-type-inference.md.
func applyFlagTypes(rule *yaml.Node, flags []string, flagTypes map[string]string) {
	if len(flagTypes) == 0 {
		return
	}
	fcNode := yamledit.MapGet(rule, "flag_constraints")
	for _, f := range flags {
		t, ok := flagTypes[f]
		if !ok || t == "" {
			continue
		}
		if fcNode == nil {
			fcNode = &yaml.Node{Kind: yaml.MappingNode}
			yamledit.MapSet(rule, "flag_constraints", fcNode)
		}
		if yamledit.MapGet(fcNode, f) != nil {
			continue // don't clobber an existing constraint
		}
		// flagConstraintNode is the single builder for a constraint entry; here
		// only the type is known (a request supplies just the value type).
		yamledit.MapSet(fcNode, f, flagConstraintNode(FlagConstraint{Type: ConstraintType(t)}))
	}
}

func stringSeq(values []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, v := range values {
		n.Content = append(n.Content, yamledit.ScalarNode(v))
	}
	return n
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
