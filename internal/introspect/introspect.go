package introspect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/pti/wraptool/internal/config"
)

// configHash is a stable hash of the introspection-relevant config for a tool:
// the binary path, the set of allowed subcommands (which decides what gets
// introspected), and the overrides file path. Flags are deliberately excluded —
// introspection parses a subcommand's whole --help regardless of which flags a
// rule allows, so they don't affect the cached schema. A change here regenerates
// the cache (and re-introspects a newly-added subcommand, which a TTL-only cache
// missed).
func configHash(tc *config.ToolConfig) string {
	h := sha256.New()
	fmt.Fprintln(h, tc.Binary)
	subs := make([]string, 0, len(tc.Allow))
	for _, r := range tc.Allow {
		subs = append(subs, strings.Join(r.Subcommand, " "))
	}
	sort.Strings(subs)
	for _, s := range subs {
		fmt.Fprintln(h, s)
	}
	fmt.Fprintln(h, tc.Introspection.OverridesFile)
	return hex.EncodeToString(h.Sum(nil))
}

// newestInputMtime returns the latest modification time across the inputs that
// produce a tool's schema but aren't captured by configHash: the wrapped tool
// binary (upgraded tool), the wraptool binary itself (changed parser code — the
// case that kept `--reviewer` boolean after a rebuild), and the overrides file.
// A cache older than this is stale. A missing tool binary or overrides file is
// returned as an error (removed or renamed?).
func newestInputMtime(tc *config.ToolConfig) (time.Time, error) {
	var newest time.Time
	consider := func(path string, required bool) error {
		if path == "" {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			if required {
				return err
			}
			return nil // best-effort for inputs we can tolerate not resolving
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	}
	if err := consider(tc.Binary, true); err != nil {
		return time.Time{}, fmt.Errorf("tool binary %q: %w", tc.Binary, err)
	}
	if err := consider(tc.Introspection.OverridesFile, true); err != nil {
		return time.Time{}, fmt.Errorf("overrides file %q: %w", tc.Introspection.OverridesFile, err)
	}
	if exe, err := os.Executable(); err == nil {
		_ = consider(exe, false) // wraptool's own binary; tolerate a stat failure
	}
	return newest, nil
}

// Introspect discovers the flags and descriptions for all allowed subcommands
// of a tool by running the tool's help command and parsing the output.
// It uses caching and manual overrides.
func Introspect(ctx context.Context, toolName string, tc *config.ToolConfig) (*Schema, error) {
	// Build the cache-validity key from the current inputs. A missing tool binary
	// surfaces here as a meaningful error (removed or renamed?) rather than a
	// silent empty schema — though config.Validate normally catches it first.
	newest, err := newestInputMtime(tc)
	if err != nil {
		return nil, fmt.Errorf("introspecting %s: %w", toolName, err)
	}
	validity := cacheValidity{
		ConfigHash:  configHash(tc),
		NewestInput: newest,
		TTL:         tc.Introspection.CacheTTL,
	}

	// Check cache first.
	cached := LoadCache(toolName, validity)
	if cached != nil {
		log.Printf("introspect %s: using cached schema", toolName)
		return cached, nil
	}

	schema := &Schema{
		Binary:         tc.Binary,
		ConfigHash:     validity.ConfigHash,
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
