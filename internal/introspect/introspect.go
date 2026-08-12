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
	validity := CacheValidity{
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

	// Parse the top-level help for the tool's subcommand catalog. This runs
	// regardless of the allow rules — a tool with none is inert but still needs
	// to be discoverable, or there is no way to learn what to request without
	// guessing. The argv is fixed (`-h` / `--help` / `help`), never AI-supplied.
	if helpText, err := getHelp(ctx, tc, nil); err != nil {
		log.Printf("introspect %s: top-level help failed, no subcommand catalog: %v", toolName, err)
	} else if available := BestParseSubcommands(helpText); len(available) > 0 {
		schema.Available = available
		log.Printf("introspect %s: %d subcommands advertised", toolName, len(available))
	}

	// Introspect each allowed subcommand.
	for _, rule := range tc.Allow {
		subcmdKey := strings.Join(rule.Subcommand, " ")

		helpText, err := getHelp(ctx, tc, rule.Subcommand)
		if err != nil {
			log.Printf("introspect %s %s: help failed: %v", toolName, subcmdKey, err)
			// Store an empty subcommand — we still know it exists, but nothing
			// was probed (ParseOutcomeHelpFailed): an absent flag here is
			// "could not tell", never "confirmed absent".
			schema.Subcommands[subcmdKey] = Subcommand{
				Flags:        make(map[string]Flag),
				ParseOutcome: ParseOutcomeHelpFailed,
			}
			continue
		}

		if !looksLikeHelp(helpText) {
			// getHelp's byte-length threshold cleared, but the output does not
			// have the shape of a help page (e.g. `git bogus -h`'s one-line
			// error text) — do not parse it as if it were one.
			log.Printf("introspect %s %s: help output did not look like a help page, treating as garbage", toolName, subcmdKey)
			schema.Subcommands[subcmdKey] = Subcommand{
				Flags:        make(map[string]Flag),
				ParseOutcome: ParseOutcomeGarbage,
			}
			continue
		}

		result := BestParse(helpText)
		log.Printf("introspect %s %s: parsed %d flags (confidence %.2f)",
			toolName, subcmdKey, len(result.Subcommand.Flags), result.Confidence)

		sub := result.Subcommand
		sub.ParseOutcome = ParseOutcomeParsed
		sub.Coverage = flagCoverage(helpText, sub.Flags)
		schema.Subcommands[subcmdKey] = sub
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
	return firstLine(output)
}

// firstLine returns the first line of s, trimmed — the version-string
// convention getVersion and ProbeVersion both apply to a tool's --version
// output (everything after the first line is typically copyright/build
// detail neither cares about).
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx > 0 {
		return s[:idx]
	}
	return strings.TrimSpace(s)
}

// ProbeVersion runs a tool's --version probe with a caller-supplied
// environment and returns its first line, mirroring getVersion but for a
// caller outside this package's own cached Introspect path — a drift check
// (design/doctor/wraptool-doctor.md §7.6 drift.tool-version-changed) needs
// the INSTALLED tool's CURRENT version string to compare against the cached
// one (LoadCacheStale), and must obtain it without touching the cache. See
// RunProbe for env's meaning.
func ProbeVersion(ctx context.Context, tc *config.ToolConfig, env []string) string {
	output, err := RunProbe(ctx, tc, []string{"--version"}, env)
	if err != nil {
		return ""
	}
	return firstLine(output)
}

// runTool executes the tool binary with the given arguments, inheriting the
// calling process's ENTIRE environment (cmd.Env left nil) — Introspect's own
// existing behaviour, unchanged by RunProbe's addition.
// It captures both stdout and stderr (some tools print help to stderr).
func runTool(ctx context.Context, tc *config.ToolConfig, args []string) (string, error) {
	return RunProbe(ctx, tc, args, nil)
}

// RunProbe executes the tool binary with args, using introspection's own
// vocabulary (help/version forms, a per-tool catalog argv, or an
// operator-authored subcommand from config — design/doctor/wraptool-doctor.md
// §5.1's allowed list; RunProbe itself does not enforce that boundary, the
// caller does, exactly as runTool always has). Both stdout and stderr are
// captured and combined, since many tools print help to stderr; a non-zero
// exit is not an error by itself (most --help paths exit non-zero) — RunProbe
// only errors when there is no output at all to look at.
//
// env, in exec.Cmd.Env's own "KEY=value" form, REPLACES the child process's
// entire environment when non-empty. An empty/nil env leaves cmd.Env unset —
// exec.Cmd's documented default of inheriting the CALLING process's full
// environment, which is Introspect's own existing behaviour (runTool passes
// env == nil). A caller that must not hand the probe its own environment — an
// operator's interactive shell, typically carrying GH_TOKEN/SSH_AUTH_SOCK/
// cloud credentials (doctor; see design/doctor/wraptool-doctor.md §5.1) —
// supplies one explicitly: typically executor.ChildEnv(binary, nil), an
// explicitly nil RESOLVED env, since ChildEnv's second parameter is the
// tool's OWN configured credentials, which a help probe must never receive.
func RunProbe(ctx context.Context, tc *config.ToolConfig, args []string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, tc.Binary, args...)
	if len(env) > 0 {
		cmd.Env = env
	}
	output, err := cmd.CombinedOutput()

	// Many tools exit non-zero for -h/--help. That's fine — we care about output.
	if len(output) > 0 {
		return string(output), nil
	}

	return "", err
}
