package introspect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/executor"
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
		sub.CoverageFound, sub.CoverageTokens = flagCoverage(helpText, sub.Flags)
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

// ProbeVersion runs a tool's --version probe and returns its first line,
// mirroring getVersion but for a caller outside this package's own cached
// Introspect path — a drift check (design/doctor/wraptool-doctor.md §7.6
// drift.tool-version-changed) needs the INSTALLED tool's CURRENT version
// string to compare against the cached one (LoadCacheStale), and must obtain
// it without touching the cache. See RunProbe for the environment it runs
// under.
func ProbeVersion(ctx context.Context, tc *config.ToolConfig) string {
	output, err := RunProbe(ctx, tc, []string{"--version"})
	if err != nil {
		return ""
	}
	return firstLine(output)
}

// runTool executes the tool binary with the given arguments. It is
// Introspect's own name for RunProbe — kept as a thin, unexported alias so
// this file's serve-time call sites (getHelp, getVersion) don't have to say
// "RunProbe" for what is, from here, just "run the tool".
func runTool(ctx context.Context, tc *config.ToolConfig, args []string) (string, error) {
	return RunProbe(ctx, tc, args)
}

// RunProbe executes the tool binary with args, using introspection's own
// vocabulary (help/version forms, a per-tool catalog argv, or an
// operator-authored subcommand from config — design/doctor/wraptool-doctor.md
// §5.1's allowed list; RunProbe itself does not enforce that boundary, the
// caller does). Both stdout and stderr are captured and combined, since many
// tools print help to stderr; a non-zero exit is not by itself a reason to
// discard output (most --help paths exit non-zero) — RunProbe errors only
// when there is no output at all to look at.
//
// There is exactly one probe environment, not a doctor-specific one and a
// serve-time one: every call — Introspect's own cached path (via runTool)
// and any external caller (doctor's drift checks) alike — runs the child
// with executor.ChildEnv(tc.Binary, nil), the SAME construction
// internal/executor and internal/mcpproxy already use for every other child
// process wraptool spawns. Before this, runTool set no cmd.Env at all and
// inherited the CALLING process's entire environment — introspection was the
// one execution path inside serve that did NOT go through ChildEnv, so a
// doctor probe built on the real Introspect/runTool path would have reported
// on a hypothetical (a scrubbed environment serve itself never actually
// used) rather than on what serve will do. There is no env parameter here on
// purpose: the production path must have no choice to make about it. The
// resolved env passed to ChildEnv is always nil — a help/version/catalog
// probe has no business seeing the tool's OWN configured credentials
// (env/env_files), only PATH/HOME/LANG. See runProbeEnv for the underlying
// exec mechanics and its test seam.
func RunProbe(ctx context.Context, tc *config.ToolConfig, args []string) (string, error) {
	output, _, err := runProbeEnv(ctx, tc, args, executor.ChildEnv(tc.Binary, nil))
	return output, err
}

// probeResult carries what RunProbe deliberately throws away for its
// output-string-only callers, but which ProbeCatalog (item 6, design
// §5.3/§7.6) needs to decide whether a probe may be trusted as exhaustive: a
// probe that failed to start, was killed by its own deadline, or exited
// non-zero with output that just happens to look plausible must not be
// treated the same as a clean, complete run. Exited is false when the
// process never produced an exit code at all (start failure, or a signal
// kill including the deadline). DeadlineExceeded is true only when ctx's own
// timeout — not a caller-supplied, pre-expired context — is what ended the
// probe. The output itself is returned separately by runProbeEnv, not
// duplicated here.
type probeResult struct {
	Exited           bool
	ExitCode         int
	DeadlineExceeded bool
}

// runProbeEnv is RunProbe's underlying exec mechanics, with env as an
// explicit parameter and the exit/deadline detail RunProbe's simpler
// (output, error) signature can't carry. It exists as a test seam — so this
// package's own tests can exercise env-replacement and exit/deadline
// classification directly — but every PRODUCTION caller reaches this only
// through RunProbe, which always supplies executor.ChildEnv(tc.Binary, nil)
// and never leaves the choice open.
//
// env, in exec.Cmd.Env's own "KEY=value" form, is assigned to cmd.Env
// unconditionally, including when nil — exec.Cmd's documented nil meaning
// ("inherit the calling process's environment") only applies if the caller
// truly wants that; RunProbe never passes nil, so nil here is reachable only
// from a test that wants to observe inheritance explicitly. An empty-but-
// non-nil env (len 0) is NOT the same as nil: it is the strongest isolation
// a caller can ask for (no environment at all) and must be honoured exactly,
// never silently upgraded to "inherit everything" — RunProbe's predecessor
// had exactly that bug (gating on len(env) > 0).
func runProbeEnv(ctx context.Context, tc *config.ToolConfig, args []string, env []string) (string, probeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, tc.Binary, args...)
	cmd.Env = env
	outputBytes, runErr := cmd.CombinedOutput()
	output := string(outputBytes)

	result := probeResult{
		DeadlineExceeded: ctx.Err() == context.DeadlineExceeded,
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		result.Exited = true
		result.ExitCode = 0
	case errors.As(runErr, &exitErr):
		result.Exited = true
		result.ExitCode = exitErr.ExitCode()
	default:
		result.Exited = false
	}

	// Many tools exit non-zero for -h/--help. That's fine — we care about
	// output. This (output, error) return keeps runTool/ProbeVersion's
	// existing behaviour: any output at all is success, and the error is
	// reported only when there is truly nothing to look at.
	if len(output) > 0 {
		return output, result, nil
	}
	return "", result, runErr
}
