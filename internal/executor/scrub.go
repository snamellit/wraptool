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
// "runners_token", "aws_secret_access_key", "client_secret", and
// "signing_key" all contain one of these fragments — which is deliberately
// permissive: a false positive on a contrived field name (e.g.
// "session_key" meaning something entirely mundane) only costs a redacted
// value, not a broken response (see sensitiveKeyPrefix), and that's far
// cheaper than a leaked credential. Do not word-boundary-anchor this: doing
// so would break "aws_secret_access_key", where "secret" sits mid-key.
//
// "key" alone is included (not just "api_key"/"private_key") to match
// other compound key names such as signing_key, deploy_key, session_key,
// and a bare "key".
const sensitiveKeyNames = `token|secret|password|passwd|credential|key`

// sensitiveKeyPrefix matches a sensitive key name plus the separator up to
// (but not including) the start of its value, in either shell notation
// (key=, key: ) or JSON notation ("key":, "key" : ). JSON's separator is
// the three-character sequence `":"` (closing key-quote, colon, opening
// value-quote); a single-character separator class (`[=:]`) can match the
// colon but not the surrounding quotes, so it never matches JSON at all —
// that was the root cause of the credential-in-JSON leak this package
// exists to fix.
//
// It intentionally does NOT match the value itself. JSON values can be
// arrays or objects with unbounded nesting ({"credential":{"type":"pat",
// "value":"..."}}), which Go's RE2-based regexp engine cannot match with a
// single pattern (no backreferences, no recursion). redactSensitiveValues
// below scans the value by hand starting at this match's end, so an array
// or object value is fully consumed instead of leaking everything past its
// first comma or closing bracket.
//
// [ \t]* (not \s*) on both sides of the separator: \s* also matches
// newlines, which let a colon-terminated line of plain text (e.g.
// "Tokens:\nEvents: <none>", from `kubectl describe pod`) be treated as a
// key whose "value" was the start of the following line, merging the two
// and leaving a dangling fragment behind. Restricting the separator to the
// same line means a key only redacts when it actually has a value on that
// line, which covers real key/value output (shell and JSON alike) without
// reaching across lines.
//
// The optional `[\w-]*` around the keyword lets it match compound key
// names (aws_secret_access_key, runners_token, client_secret) as well as
// camelCase names with no separator at all (apiKey — "key" inside
// sensitiveKeyNames already matches "Key" case-insensitively with no
// separator needed).
var sensitiveKeyPrefix = regexp.MustCompile(
	`(?i)"?[\w-]*(?:` + sensitiveKeyNames + `)[\w-]*"?[ \t]*[:=][ \t]*`,
)

// numberLiteral matches a bare JSON number.
var numberLiteral = regexp.MustCompile(`^-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?$`)

// credentialPatterns matches value-shape or ALL_CAPS-assignment credential
// patterns that don't need the key/value-aware handling in
// redactSensitiveValues.
//
// This whole file is regex-only, not a structural (parse-as-JSON, walk,
// redact by key, re-serialize) pass. A structural pass would need to
// re-serialize the document to remove a value, which reformats output the
// agent may depend on (key order, whitespace, number formatting) and must
// still fall back to a regex path whenever the input isn't valid JSON —
// which MCP tool output routinely isn't, since executor.Result.Truncated
// output is cut off mid-document. redactSensitiveValues handles the JSON
// separator and nested-value cases by hand-scanning (see its doc comment)
// without needing a full parse, so a structural pass is not worth the
// added surface area.
var credentialPatterns = []*regexp.Regexp{
	// Bearer/Basic auth tokens.
	regexp.MustCompile(`(?i)(Bearer|Basic)\s+[A-Za-z0-9_\-./+=]{8,}`),
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
	output = redactSensitiveValues(output)
	for _, pattern := range credentialPatterns {
		output = pattern.ReplaceAllString(output, redacted)
	}
	return output
}

// redactSensitiveValues finds every sensitiveKeyPrefix match (key name +
// separator, see its doc comment) and hand-scans the value that follows,
// replacing only the value — never the key or separator — so redacted JSON
// stays syntactically valid JSON (a client piping tool output through `jq`
// must not see a syntax error just because the response happened to
// contain a token). `true`, `false`, `null`, and bare numbers are never
// redacted: they cannot be credentials, and blanket-redacting them (e.g.
// "automount_service_account_token":false) destroys diagnostic output the
// agent needs for no security benefit.
func redactSensitiveValues(s string) string {
	locs := sensitiveKeyPrefix.FindAllStringIndex(s, -1)
	if locs == nil {
		return s
	}

	var b strings.Builder
	cursor := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		if start < cursor {
			// This prefix match sits inside a value already consumed and
			// redacted by an earlier match (e.g. a nested "api_key" field
			// inside a "credential": {...} object) — nothing left to do.
			continue
		}

		valueEnd, replacement, ok := scanSensitiveValue(s, end)
		if !ok {
			// No redactable value here (a bare literal/number, or no value
			// at all on this line) — leave the key and everything after it
			// untouched; it will be copied verbatim below.
			continue
		}

		b.WriteString(s[cursor:end])
		b.WriteString(replacement)
		cursor = valueEnd
	}
	b.WriteString(s[cursor:])
	return b.String()
}

// scanSensitiveValue scans the value starting at s[pos:], returning the
// position just past it, its redacted replacement, and whether it should
// be redacted at all. pos is always right after a sensitiveKeyPrefix
// match, i.e. right at the start of the value (or at whatever follows the
// separator, if there was no value).
func scanSensitiveValue(s string, pos int) (end int, replacement string, ok bool) {
	if pos >= len(s) {
		return pos, "", false
	}

	switch s[pos] {
	case '"':
		if strEnd, closed := scanJSONString(s, pos); closed {
			return strEnd, `"` + redacted + `"`, true
		}
		// Unterminated quoted value — MCP output can be truncated
		// mid-document (executor.Result.Truncated). Fall through to the
		// bare-token scan below, which (starting at the opening quote)
		// consumes through to the end of the string and still redacts it,
		// rather than leaking the truncated fragment untouched.
	case '[', '{':
		bracketEnd := scanBalanced(s, pos)
		return bracketEnd, `"` + redacted + `"`, true
	}

	tokenEnd := scanBareToken(s, pos)
	if tokenEnd == pos {
		return pos, "", false
	}
	token := s[pos:tokenEnd]
	if isUnredactableLiteral(token) {
		return pos, "", false
	}
	return tokenEnd, redacted, true
}

// scanJSONString scans a JSON string starting at s[pos] == '"', honoring
// backslash escapes, and returns the index just past the closing quote.
// closed is false if the end of s is reached with no closing quote (a
// truncated value).
func scanJSONString(s string, pos int) (end int, closed bool) {
	i := pos + 1
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '"':
			return i + 1, true
		}
		i++
	}
	return len(s), false
}

// scanBalanced scans a bracketed JSON value (array or object) starting at
// s[pos], which must be '[' or '{', and returns the index just past its
// matching closing bracket — honoring nested brackets and quoted strings
// (so a `]` or `}` inside a string value doesn't end the scan early). If
// the end of s is reached before the brackets balance (a truncated value),
// it returns len(s): the whole remaining, truncated fragment is treated as
// the value and gets redacted rather than left leaking.
func scanBalanced(s string, pos int) (end int) {
	depth := 0
	i := pos
	for i < len(s) {
		switch s[i] {
		case '"':
			j, closed := scanJSONString(s, i)
			if !closed {
				return len(s)
			}
			i = j
			continue
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
		i++
	}
	return len(s)
}

// scanBareToken scans an unquoted value — a shell-style key=value
// assignment, or a bare JSON literal/number — stopping before whitespace, a
// comma, or a closing brace/bracket.
func scanBareToken(s string, pos int) (end int) {
	i := pos
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r', ',', '}', ']':
			return i
		}
		i++
	}
	return i
}

// isUnredactableLiteral reports whether token is a JSON literal (true,
// false, null) or a bare number — values that can never be credentials, so
// redacting them only destroys diagnostic output for no security benefit.
func isUnredactableLiteral(token string) bool {
	switch strings.ToLower(token) {
	case "true", "false", "null":
		return true
	}
	return numberLiteral.MatchString(token)
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
