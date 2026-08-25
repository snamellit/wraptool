package executor

import (
	"regexp"
	"strings"
)

// sensitiveKeyNames lists the key-name fragments that mark a key/value pair
// as a credential, regardless of the value's length or character set.
// Redacting by key name (rather than by guessing what a secret "looks
// like") is the load-bearing rule here: a GitLab runners_token is only 29
// characters, well under the 40-character floor this package used to
// require, and slipped through unredacted. Matching is substring-based —
// "runners_token", "AWS_SECRET_ACCESS_KEY", and "client_secret" all contain
// one of these fragments — which is deliberately permissive: a false
// positive on a contrived field name (e.g. "secretary") is far cheaper than
// a leaked credential.
const sensitiveKeyNames = `token|secret|password|passwd|credential|private[_-]?key|api[_-]?key`

// sensitiveKeyValue matches a key/value pair whose key contains one of
// sensitiveKeyNames, in either shell notation (key=value, key: value) or
// JSON notation ("key": "value", "key":"value", "key" : "value"). JSON's
// separator is the three-character sequence `":"` (closing key-quote,
// colon, opening value-quote); the previous version of this pattern used a
// single-character separator class (`[=:]`) that could match the colon but
// not the surrounding quotes, so it never matched JSON output at all — that
// was the root cause of the credential-in-JSON leak this pattern fixes.
//
// The optional `[\w-]*` around the keyword lets it match compound key names
// (aws_secret_access_key, runners_token, client_secret) as well as
// camelCase names with no separator at all (apiKey — `[_-]?` inside
// api[_-]?key already matches zero occurrences, so "apiKey" case-folds to
// "apikey" and matches directly).
//
// The value alternation accepts either a JSON string (with escape
// handling, so an embedded escaped quote doesn't end the match early) or an
// unquoted shell-style token that runs until the next whitespace, comma, or
// closing brace.
var sensitiveKeyValue = regexp.MustCompile(
	`(?i)"?[\w-]*(?:` + sensitiveKeyNames + `)[\w-]*"?\s*[:=]\s*(?:"(?:\\.|[^"\\])*"|[^\s,}]+)`,
)

// credentialPatterns matches common credential patterns that should be
// scrubbed from command output before returning to the AI context.
//
// This is regex-only, not a structural (parse-as-JSON, walk, redact by key)
// pass. A structural pass would need to re-serialize the document to
// remove a value, which reformats output the agent may depend on (key
// order, whitespace, number formatting) and must fall back to this same
// regex path anyway whenever the input isn't valid JSON — which MCP tool
// output routinely isn't, since executor.Result.Truncated output is cut off
// mid-document. The patterns below already satisfy the JSON case (via the
// `":"` separator handling in sensitiveKeyValue) without that complexity,
// so a structural pass is not worth the added surface area.
var credentialPatterns = []*regexp.Regexp{
	// Bearer/Basic auth tokens.
	regexp.MustCompile(`(?i)(Bearer|Basic)\s+[A-Za-z0-9_\-./+=]{8,}`),
	// Key/value pairs whose key name marks the value as a credential; see
	// sensitiveKeyValue's doc comment above. No length floor — a short
	// value with a sensitive key name is still a credential.
	sensitiveKeyValue,
	// AWS access keys (AKIA...).
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// SSH private key markers.
	regexp.MustCompile(`-----BEGIN[A-Z ]*PRIVATE KEY-----`),
	// GitLab personal/project/group/deploy/impersonation/pipeline-trigger/
	// CI-job token prefixes, and the runner-registration token prefix.
	// These are value-shape signals for secrets that appear with no key
	// name attached at all (e.g. embedded in a URL or a curl command).
	regexp.MustCompile(`\bgl(?:pat|rt|dt|soat|imt|ptt|cbt)-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`\bGR1348941[A-Za-z0-9_-]+`),
	// Environment variable assignments with sensitive names (ALL_CAPS=...
	// shell/env-dump style; broader than sensitiveKeyNames since a bare
	// AUTH prefix — not just AUTH_TOKEN — has always been treated as
	// sensitive here).
	regexp.MustCompile(`(?i)(API_KEY|SECRET|TOKEN|PASSWORD|CREDENTIAL|AUTH)[A-Z_]*=[^\s]+`),
}

const redacted = "[REDACTED]"

// ScrubCredentials removes potential credentials from command output.
func ScrubCredentials(output string) string {
	for _, pattern := range credentialPatterns {
		output = pattern.ReplaceAllString(output, redacted)
	}
	return output
}

// ScrubEnvVars removes specific environment variable values from output.
// This is used to scrub the tool's configured env vars from output.
func ScrubEnvVars(output string, envVars map[string]string) string {
	for _, value := range envVars {
		if len(value) >= 8 { // only scrub non-trivial values
			output = strings.ReplaceAll(output, value, redacted)
		}
	}
	return output
}
