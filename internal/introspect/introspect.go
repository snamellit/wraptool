package introspect

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/pti/wraptool/internal/config"
)

// Introspect discovers the flags and descriptions for all allowed subcommands
// of a tool by running the tool's help command and parsing the output.
// It uses caching and manual overrides.
func Introspect(ctx context.Context, toolName string, tc *config.ToolConfig) (*Schema, error) {
	// Check cache first.
	cached := LoadCache(toolName, tc.Introspection.CacheTTL)
	if cached != nil {
		log.Printf("introspect %s: using cached schema", toolName)
		return cached, nil
	}

	schema := &Schema{
		Binary:         tc.Binary,
		IntrospectedAt: time.Now().UTC(),
		Subcommands:    make(map[string]Subcommand),
	}

	// Get version for cache invalidation.
	schema.VersionOutput = getVersion(ctx, tc)

	// Introspect each allowed subcommand.
	for _, rule := range tc.Allow {
		subcmdKey := strings.Join(rule.Subcommand, " ")

		helpText, err := getHelp(ctx, tc, rule.Subcommand)
		if err != nil {
			log.Printf("introspect %s %s: help failed: %v", toolName, subcmdKey, err)
			// Store an empty subcommand — we still know it exists.
			schema.Subcommands[subcmdKey] = Subcommand{
				Flags: make(map[string]Flag),
			}
			continue
		}

		result := BestParse(helpText)
		log.Printf("introspect %s %s: parsed %d flags (confidence %.2f)",
			toolName, subcmdKey, len(result.Subcommand.Flags), result.Confidence)

		schema.Subcommands[subcmdKey] = result.Subcommand
	}

	// Load and merge overrides.
	if tc.Introspection.OverridesFile != "" {
		if err := mergeOverrides(schema, tc.Introspection.OverridesFile); err != nil {
			log.Printf("introspect %s: override merge failed: %v", toolName, err)
		}
	}

	// Save to cache.
	if err := SaveCache(toolName, schema); err != nil {
		log.Printf("introspect %s: cache save failed: %v", toolName, err)
	}

	return schema, nil
}

// getHelp runs the tool's help command for a specific subcommand.
// It tries -h first (short, structured output), then --help.
func getHelp(ctx context.Context, tc *config.ToolConfig, subcommand []string) (string, error) {
	// Try -h first (git, cobra tools give structured output).
	args := append(subcommand, "-h")
	output, err := runTool(ctx, tc, args)
	if err == nil && len(output) > 20 {
		return output, nil
	}

	// Try --help.
	args = append(subcommand[:len(subcommand):len(subcommand)], "--help")
	output, err = runTool(ctx, tc, args)
	if err == nil && len(output) > 20 {
		return output, nil
	}

	// Try "help <subcommand>".
	args = append([]string{"help"}, subcommand...)
	output, err = runTool(ctx, tc, args)
	if err == nil && len(output) > 20 {
		return output, nil
	}

	return "", fmt.Errorf("no help output obtained for %v", subcommand)
}

// getVersion tries to get the tool's version string.
func getVersion(ctx context.Context, tc *config.ToolConfig) string {
	output, err := runTool(ctx, tc, []string{"--version"})
	if err != nil {
		return ""
	}
	// Take just the first line.
	if idx := strings.IndexByte(output, '\n'); idx > 0 {
		return output[:idx]
	}
	return strings.TrimSpace(output)
}

// runTool executes the tool binary with the given arguments.
// It captures both stdout and stderr (some tools print help to stderr).
func runTool(ctx context.Context, tc *config.ToolConfig, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, tc.Binary, args...)
	output, err := cmd.CombinedOutput()

	// Many tools exit non-zero for -h/--help. That's fine — we care about output.
	if len(output) > 0 {
		return string(output), nil
	}

	return "", err
}
