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

// SetToolMetadata sets a tool's non-secret runtime metadata: working_dir,
// timeout (a duration string like "60s"), and env_files (paths to env files).
// An empty working_dir or timeout, or an empty envFiles slice, removes that key.
// Inline `env` values are deliberately NOT editable here — secrets belong in an
// env_files file (per-folder .env/.envrc), not inline in the shared config, so
// the Web UI never becomes a plaintext secret store. Round-trip validated (an
// unparseable timeout is rejected) and written atomically; the server
// hot-reloads.
func SetToolMetadata(path, name, workingDir, timeout string, envFiles []string) error {
	if name == "" {
		return fmt.Errorf("tool name is required")
	}
	return editConfig(path, func(doc *yaml.Node) error {
		toolNode, err := docToolNode(doc, name)
		if err != nil {
			return err
		}
		// working_dir and env-file paths may contain characters (spaces, etc.)
		// that need quoting, so they are string-tagged; timeout is a bare token.
		if workingDir != "" {
			mapSet(toolNode, "working_dir", strNode(workingDir))
		} else {
			mapDelete(toolNode, "working_dir")
		}
		if timeout != "" {
			mapSet(toolNode, "timeout", scalarNode(timeout))
		} else {
			mapDelete(toolNode, "timeout")
		}
		if len(envFiles) > 0 {
			seq := &yaml.Node{Kind: yaml.SequenceNode}
			for _, f := range envFiles {
				seq.Content = append(seq.Content, strNode(f))
			}
			mapSet(toolNode, "env_files", seq)
		} else {
			mapDelete(toolNode, "env_files")
		}
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
