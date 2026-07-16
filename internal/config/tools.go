package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// AddTool adds a new wrapped tool with the given binary path and no rules. The
// tool is inert until allow rules are added (see AddRule) — a rule-less tool
// registers no MCP tools and default-denies — so bootstrapping is a two-step
// flow: create the tool, then author its rules. The edit is round-trip
// validated (the binary must be an absolute path to an existing regular file)
// and written atomically; a running server hot-reloads it.
func AddTool(path, name, binary string) error {
	// Reuse the MCP property-key charset: it excludes '/' (which would break the
	// /tools/{tool}/... routes) and matches what the tool name must satisfy when
	// it becomes part of an MCP tool name. Rejects the empty name too ({1,64}).
	if !paramNameRe.MatchString(name) {
		return fmt.Errorf("tool name %q must match %s", name, paramNamePattern)
	}
	if binary == "" {
		return fmt.Errorf("binary path is required")
	}
	return editConfig(path, func(doc *yaml.Node) error {
		toolsNode := mapGet(doc, "tools")
		if toolsNode == nil {
			toolsNode = &yaml.Node{Kind: yaml.MappingNode}
			mapSet(doc, "tools", toolsNode)
		} else if toolsNode.Kind != yaml.MappingNode {
			return fmt.Errorf("`tools` is not a mapping")
		}
		if mapGet(toolsNode, name) != nil {
			return fmt.Errorf("tool %q already exists", name)
		}
		tool := &yaml.Node{Kind: yaml.MappingNode}
		mapSet(tool, "binary", scalarNode(binary))
		mapSet(toolsNode, name, tool)
		return nil
	})
}

// DeleteTool removes a wrapped tool and all its rules. Round-trip validation
// still applies, so removing the last tool (with no mcp_servers configured) is
// refused — the config would expose nothing to wrap.
func DeleteTool(path, name string) error {
	return editConfig(path, func(doc *yaml.Node) error {
		toolsNode := mapGet(doc, "tools")
		if toolsNode == nil || toolsNode.Kind != yaml.MappingNode {
			return fmt.Errorf("no `tools` mapping in config")
		}
		if mapGet(toolsNode, name) == nil {
			return fmt.Errorf("tool %q is not configured", name)
		}
		mapDelete(toolsNode, name)
		return nil
	})
}
