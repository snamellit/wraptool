// Package toolname owns the naming rules wraptool's MCP surface is bound by:
// the grammar a model API enforces on a function-declaration name, the schema
// property-key grammar derived from a CLI flag, and the minting rules built on
// them.
//
// It is its own package because the rules have two kinds of caller that cannot
// import each other. internal/config validates names at config-load time and
// checks that two of them do not collide; internal/mcptools and
// internal/mcpproxy mint and register them. config cannot import mcptools
// without a cycle, so a rule living in either one gets copied into the other
// and the copies drift — which is exactly the bug this package was extracted
// to end, and the same reason internal/yamledit was extracted before it.
package toolname

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// ParamPattern is the property-key pattern the Anthropic API enforces on tool
// input schemas. A flag whose derived parameter name violates it would be
// rejected with a 400 mid-session, so callers reject it at config-load time.
const ParamPattern = `^[a-zA-Z0-9_.-]{1,64}$`

var paramRe = regexp.MustCompile(ParamPattern)

// ValidParam reports whether name is a usable schema property key.
func ValidParam(name string) bool { return paramRe.MatchString(name) }

// Param is the schema property key derived from a CLI flag.
// e.g. "--set-upstream" -> "set_upstream"; "-m" -> "m"
func Param(flag string) string {
	return strings.ReplaceAll(strings.TrimLeft(flag, "-"), "-", "_")
}

// unsafeRe matches every character NOT permitted in an MCP tool name by the
// strictest model API wraptool's tools are handed to. Gemini (and therefore
// Antigravity/agy) enforces `[A-Za-z_][A-Za-z0-9_.:-]{0,127}` on a function
// declaration name and rejects the WHOLE request when one entry violates it —
// a single bad name disables every tool in the session, not just its own.
// Anthropic's grammar is similar. `:` is permitted by Gemini but deliberately
// excluded here: it is not universally accepted, and nothing in wraptool
// mints one.
var unsafeRe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// MaxLen is the longest tool name the grammar above admits (Gemini's {0,127}
// quantifier follows a mandatory leading character, so 128 total).
const MaxLen = 128

// digestLen is the hex digest width appended to a truncated name. 8 hex chars
// = 32 bits: ample to separate the handful of names one server can mint, while
// leaving the human-readable prefix as long as possible.
const digestLen = 8

// Sanitize maps a minted tool name onto the grammar above: each character
// outside the permitted set becomes "_", a result that would not start with a
// letter or underscore is prefixed with "_", and a result longer than MaxLen is
// truncated with a digest suffix that keeps distinct long names distinct. It is
// idempotent — sanitizing a sanitized name is a no-op — which is what lets the
// registration choke point (mcptools.NewRegisteredTool) re-apply it as defense
// in depth.
//
// This bites upstream MCP servers (mcp_servers), not wrapped CLIs: an upstream
// is free to namespace its own tools with a separator wraptool does not
// control — sigil publishes "sigil/lookup" — and that name is otherwise
// carried verbatim into the registered name. Callers apply it on the
// collision-detection path too, so two upstream names that differ only in an
// unsafe character ("a/b" vs "a_b"), or only past the truncation point, are
// caught as the collision they become once sanitized, rather than silently
// registering twice.
func Sanitize(name string) string {
	// Replacement is per-RUNE, so a multi-byte character collapses to a single
	// "_" — a hand-rolled byte loop would emit one per byte.
	s := unsafeRe.ReplaceAllString(name, "_")
	// Only [A-Za-z0-9_.-] survive that pass, so the illegal leading characters
	// are a digit, "." and "-", plus the empty string.
	if s == "" || s[0] == '.' || s[0] == '-' || (s[0] >= '0' && s[0] <= '9') {
		s = "_" + s
	}
	if len(s) > MaxLen {
		// Every surviving byte is ASCII, so a byte-index cut cannot split a
		// rune. The digest covers the whole pre-truncation name, so two names
		// sharing a prefix past the cut still differ.
		sum := sha256.Sum256([]byte(s))
		s = s[:MaxLen-digestLen-1] + "_" + hex.EncodeToString(sum[:])[:digestLen]
	}
	return s
}

// CLI is the registered MCP name for a wrapped CLI subcommand: the tool name
// joined to its subcommand path, sanitized.
// e.g. ("git", ["commit"]) -> "git_commit"
// e.g. ("gcloud", ["compute", "instances", "list"]) -> "gcloud_compute_instances_list"
func CLI(tool string, subcommand []string) string {
	parts := make([]string, 0, 1+len(subcommand))
	parts = append(parts, tool)
	parts = append(parts, subcommand...)
	return Sanitize(strings.Join(parts, "_"))
}

// Proxy is the registered MCP name for a tool proxied from an upstream server:
// the server's name_prefix joined to the upstream tool name, sanitized. The
// mcpproxy source and the collision check in config must agree by
// construction, so both mint through here.
func Proxy(prefix, upstreamTool string) string {
	return Sanitize(prefix + "_" + upstreamTool)
}
