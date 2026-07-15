package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Rule kinds addressable by the CRUD helpers below.
const (
	KindAllow = "allow"
	KindDeny  = "deny"
)

func validKind(kind string) error {
	if kind != KindAllow && kind != KindDeny {
		return fmt.Errorf("rule kind must be %q or %q, got %q", KindAllow, KindDeny, kind)
	}
	return nil
}

// AddRule appends a rule to a tool's allow or deny sequence. For deny rules
// the flags argument is ignored (deny rules match on subcommand only). The
// edit is validated and written atomically; a running server hot-reloads it.
func AddRule(path, toolName, kind string, subcommand, flags []string) error {
	if err := validKind(kind); err != nil {
		return err
	}
	return editConfig(path, func(doc *yaml.Node) error {
		toolNode, err := docToolNode(doc, toolName)
		if err != nil {
			return err
		}
		seq, err := rulesSeq(toolNode, toolName, kind, true)
		if err != nil {
			return err
		}
		seq.Content = append(seq.Content, newRule(kind, subcommand, flags))
		return nil
	})
}

// UpdateRule replaces the subcommand (and, for allow rules, the flags) of the
// rule at index in a tool's allow or deny sequence. expect is the rule's
// current subcommand as the operator last saw it; if it no longer matches the
// edit is refused so a concurrent change is not silently clobbered. For an
// allow rule, an empty flags slice removes the `flags` key; other keys
// (flag_constraints, deny_flags, comments) are preserved.
func UpdateRule(path, toolName, kind string, index int, expect, subcommand, flags []string) error {
	if err := validKind(kind); err != nil {
		return err
	}
	return editConfig(path, func(doc *yaml.Node) error {
		rule, err := ruleAt(doc, toolName, kind, index, expect)
		if err != nil {
			return err
		}
		mapSet(rule, "subcommand", stringSeq(subcommand))
		if kind == KindAllow {
			if len(flags) > 0 {
				mapSet(rule, "flags", stringSeq(flags))
			} else {
				mapDelete(rule, "flags")
			}
			// Drop flag_constraints for flags that no longer exist so the
			// config does not accumulate orphaned entries.
			pruneFlagConstraints(rule, flags)
		}
		return nil
	})
}

// DeleteRule removes the rule at index from a tool's allow or deny sequence.
// expect guards against concurrent edits (see UpdateRule).
func DeleteRule(path, toolName, kind string, index int, expect []string) error {
	if err := validKind(kind); err != nil {
		return err
	}
	return editConfig(path, func(doc *yaml.Node) error {
		toolNode, err := docToolNode(doc, toolName)
		if err != nil {
			return err
		}
		seq, err := rulesSeq(toolNode, toolName, kind, false)
		if err != nil {
			return err
		}
		if _, err := checkRuleAt(seq, toolName, kind, index, expect); err != nil {
			return err
		}
		seq.Content = append(seq.Content[:index], seq.Content[index+1:]...)
		return nil
	})
}

// rulesSeq returns the allow/deny sequence node for a tool, creating an empty
// one when create is true and it is absent.
func rulesSeq(toolNode *yaml.Node, toolName, kind string, create bool) (*yaml.Node, error) {
	seq := mapGet(toolNode, kind)
	if seq == nil {
		if !create {
			return nil, fmt.Errorf("tool %q has no %s rules", toolName, kind)
		}
		seq = &yaml.Node{Kind: yaml.SequenceNode}
		mapSet(toolNode, kind, seq)
		return seq, nil
	}
	if seq.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("tool %q `%s` is not a sequence", toolName, kind)
	}
	return seq, nil
}

// ruleAt resolves the rule node at index within a tool's kind sequence,
// enforcing the optimistic-concurrency check against expect.
func ruleAt(doc *yaml.Node, toolName, kind string, index int, expect []string) (*yaml.Node, error) {
	toolNode, err := docToolNode(doc, toolName)
	if err != nil {
		return nil, err
	}
	seq, err := rulesSeq(toolNode, toolName, kind, false)
	if err != nil {
		return nil, err
	}
	return checkRuleAt(seq, toolName, kind, index, expect)
}

func checkRuleAt(seq *yaml.Node, toolName, kind string, index int, expect []string) (*yaml.Node, error) {
	if index < 0 || index >= len(seq.Content) {
		return nil, fmt.Errorf("tool %q has no %s rule at index %d", toolName, kind, index)
	}
	rule := seq.Content[index]
	if rule.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("tool %q %s rule %d is not a mapping", toolName, kind, index)
	}
	if expect != nil {
		sub := mapGet(rule, "subcommand")
		if sub == nil || !stringSeqEqual(sub, expect) {
			return nil, fmt.Errorf("tool %q %s rule %d changed since it was loaded; reload and retry", toolName, kind, index)
		}
	}
	return rule, nil
}

func newRule(kind string, subcommand, flags []string) *yaml.Node {
	if kind == KindAllow {
		return newAllowRule(subcommand, flags, nil)
	}
	rule := &yaml.Node{Kind: yaml.MappingNode}
	mapSet(rule, "subcommand", stringSeq(subcommand))
	return rule
}

// pruneFlagConstraints removes flag_constraints entries whose flag is not in
// keepFlags, and drops the flag_constraints key entirely when it empties.
func pruneFlagConstraints(rule *yaml.Node, keepFlags []string) {
	fc := mapGet(rule, "flag_constraints")
	if fc == nil || fc.Kind != yaml.MappingNode {
		return
	}
	keep := make(map[string]bool, len(keepFlags))
	for _, f := range keepFlags {
		keep[f] = true
	}
	var kept []*yaml.Node
	for i := 0; i+1 < len(fc.Content); i += 2 {
		if keep[fc.Content[i].Value] {
			kept = append(kept, fc.Content[i], fc.Content[i+1])
		}
	}
	if len(kept) == 0 {
		mapDelete(rule, "flag_constraints")
		return
	}
	fc.Content = kept
}

// mapDelete removes a key/value pair from a mapping node, if present.
func mapDelete(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
