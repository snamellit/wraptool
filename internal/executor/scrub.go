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
// permissive: a false positive on a contrived field name only costs a
// redacted value, not a broken response (see sensitiveKeyPrefix), and
// that's far cheaper than a leaked credential. Do not word-boundary-anchor
// "secret": doing so would break "aws_secret_access_key", where "secret"
// sits mid-key. `secretKeyRef` is a known, accepted residual of this: a
// Kubernetes {"secretKeyRef":{"name":"db","key":"password"}} loses the
// whole ref object even though it only ever names which secret to use, not
// a value — not worth narrowing further, since "secret" must stay broad.
//
// "key" is narrower than the rest: `[\w-]+key` requires at least one word
// character (or hyphen) immediately before it. Bare "key" is deliberately
// EXCLUDED here — it's the exact field-name spelling Kubernetes uses for
// non-secret label-selector identifiers (a toleration's or node-affinity
// matchExpression's "key", a ConfigMap/Secret projection's "key"), and
// redacting it destroys output like "why won't this pod schedule" needs.
// See bareKeyLongValue below for the narrow backstop that still catches a
// genuinely long bare-"key" value.
const sensitiveKeyNames = `token|secret|password|passwd|credential|[\w-]+key`

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

// bareKeyLongValue restores this package's pre-JSON-fix behaviour for the
// single bare identifier "key" (as opposed to any compound like
// signing_key, which sensitiveKeyPrefix already covers with no length
// floor). That prior behaviour was `(token|key|secret|password)\s*[=:"]\s*
// [A-Za-z0-9_\-./+=]{40,}` — key redacted only alongside a 40+ character
// value. sensitiveKeyPrefix's `[\w-]+key` deliberately excludes bare "key"
// (see its doc comment — it's Kubernetes' non-secret label-selector field
// name), which would otherwise silently regress a genuine `key=<40+ char
// secret>` shell assignment; this restores exactly that one case, gated on
// the same 40+ character floor, so a short, clearly-non-secret value like
// `"key":"kubernetes.io/hostname"` (22 characters) still survives
// untouched. \b on both sides of "key" is what makes this bare-only:
// "signing_key" has a word character ("_") on both sides of "key", so \b
// does not match there.
var bareKeyLongValue = regexp.MustCompile(
	`(?i)\bkey"?[ \t]*[:=][ \t]*"?[A-Za-z0-9_\-./+=]{40,}`,
)

// numberLiteral matches a bare JSON number.
var numberLiteral = regexp.MustCompile(`^-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?$`)

// maxBalancedScanBytes bounds how far scanBalanced (and the mirror-image
// unterminated-quote case in scanSensitiveValue) will scan looking for a
// closing delimiter. Fail-closed — redacting a value we couldn't fully
// verify the end of, rather than risk leaking it — is correct for a
// genuinely truncated JSON value (MCP output cut off mid-document by
// limitedWriter, executor.go). But a single stray, unmatched `[`, `{`, or
// `"` in ordinary non-JSON output (e.g. a `git log` commit body containing
// "signing_key: {" with no closing brace anywhere in the rest of a
// possibly-huge log) must not be allowed to swallow everything after it.
// Beyond this budget with no closing delimiter found and more input still
// remaining, only the delimiter character itself is redacted.
const maxBalancedScanBytes = 4096

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
	// Bare "key" with a long (40+ char) value; see bareKeyLongValue's doc
	// comment.
	bareKeyLongValue,
	// Environment variable assignments with sensitive names (ALL_CAPS=...
	// shell/env-dump style; broader than sensitiveKeyNames since a bare
	// AUTH prefix — not just AUTH_TOKEN — has always been treated as
	// sensitive here). The value class excludes `[` and `]` specifically
	// so this can't re-match (and mangle) a "[REDACTED]" marker that
	// redactSensitiveValues already produced for the same key — e.g.
	// "aws_secret_access_key=[REDACTED]" staying exactly that, rather
	// than this pattern's case-insensitive "SECRET" branch re-matching
	// "secret_access_key=[REDACTED]" and collapsing it to
	// "aws_[REDACTED]".
	regexp.MustCompile(`(?i)(API_KEY|SECRET|TOKEN|PASSWORD|CREDENTIAL|AUTH)[A-Z_]*=[^\s\[\]]+`),
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
// contain a token). `true`, `false`, and `null` are never redacted: they
// cannot be credentials, and blanket-redacting them (e.g.
// "ci_job_token_scope_enabled":false) destroys diagnostic output the agent
// needs for no security benefit. A bare number is skipped only when the
// key was JSON-quoted — see scanSensitiveValue's doc comment.
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

		// The key was written in JSON notation ("key": ...) iff the match
		// starts with the key's own opening quote — sensitiveKeyPrefix's
		// leading `"?` is the very first thing it can consume, so if
		// present it's always at s[start].
		keyQuoted := start < len(s) && s[start] == '"'

		valueEnd, replacement, ok := scanSensitiveValue(s, end, keyQuoted)
		if !ok {
			// No redactable value here (a bare literal/skipped number, or
			// no value at all on this line) — leave the key and
			// everything after it untouched; it will be copied verbatim
			// below.
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
// separator, if there was no value). keyQuoted reports whether the key
// itself was written in JSON notation ("key": ...) rather than shell/prose
// notation (key=..., key: ...): a bare number is only ever a credential in
// the latter case (base's own patterns redacted e.g. `password: 123456`),
// while in genuine JSON a numeric field sharing a token-ish key name (e.g.
// "token_count":12345, "runners_token_expires_at" paired with a Unix
// timestamp) is routine diagnostic data, not a secret.
func scanSensitiveValue(s string, pos int, keyQuoted bool) (end int, replacement string, ok bool) {
	if pos >= len(s) {
		return pos, "", false
	}

	switch s[pos] {
	case '"':
		if strEnd, closed := scanJSONString(s, pos); closed {
			return strEnd, `"` + redacted + `"`, true
		}
		// Unterminated quoted value: MCP output can be truncated
		// mid-document (executor.Result.Truncated, via limitedWriter in
		// executor.go, which cuts output at defaultMaxOutput and can sever
		// a value mid-string). Redact the remainder as one unit instead of
		// falling through to scanBareToken, which stops at the first
		// whitespace/comma/brace and would leak everything past it — for
		// a PEM-shaped private key value, that's the base64 key material,
		// and it also eats the "-----BEGIN" prefix, defeating the
		// standalone PEM value-shape pattern that would otherwise have
		// caught it.
		//
		// Bounded the same way as scanBalanced (see maxBalancedScanBytes):
		// if a large amount of text follows with no closing quote found
		// nearby, this isn't a truncated value, it's a stray quote in
		// unrelated non-JSON output, so only the opening quote itself is
		// redacted rather than swallowing everything after it.
		if len(s)-pos <= maxBalancedScanBytes {
			return len(s), redacted, true
		}
		return pos + 1, redacted, true
	case '[', '{':
		bracketEnd, complete := scanBalanced(s, pos)
		if complete {
			return bracketEnd, `"` + redacted + `"`, true
		}
		// scanBalanced gave up after maxBalancedScanBytes with brackets
		// still unbalanced and much more input left — a stray delimiter,
		// not a value. Only it was consumed; redact it bare, not quoted,
		// since we're not claiming this was ever a JSON value.
		return bracketEnd, redacted, true
	}

	tokenEnd := scanBareToken(s, pos)
	if tokenEnd == pos {
		return pos, "", false
	}
	token := s[pos:tokenEnd]
	if isUnredactableLiteral(token, keyQuoted) {
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
// s[pos], which must be '[' or '{', honoring nested brackets and quoted
// strings (so a `]` or `}` inside a string value doesn't end the scan
// early). complete is true when a real value was fully identified — either
// its matching closing bracket was found, or the end of s was reached
// within maxBalancedScanBytes (a genuinely truncated value, still redacted
// in full). complete is false when the budget was exhausted with brackets
// still unbalanced and more of s left to scan: a stray, unmatched bracket
// in non-JSON output, not a value: end is then just pos+1 (the bracket
// character alone), so the caller doesn't swallow unrelated output that
// follows it.
func scanBalanced(s string, pos int) (end int, complete bool) {
	depth := 0
	limit := pos + maxBalancedScanBytes
	if limit > len(s) {
		limit = len(s)
	}
	i := pos
	for i < limit {
		switch s[i] {
		case '"':
			j, closed := scanJSONString(s, i)
			if !closed {
				return len(s), true
			}
			i = j
			continue
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
		i++
	}
	if i >= len(s) {
		return len(s), true
	}
	return pos + 1, false
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

// isUnredactableLiteral reports whether token can never be a credential and
// so should be left untouched. `true`, `false`, and `null` never are,
// regardless of context. A bare number is only skipped when keyQuoted is
// true (genuine JSON, e.g. "token_count":12345) — see scanSensitiveValue's
// doc comment for why a number is treated as redactable in shell/prose
// context (keyQuoted false), matching this package's pre-JSON-fix
// behaviour there.
func isUnredactableLiteral(token string, keyQuoted bool) bool {
	switch strings.ToLower(token) {
	case "true", "false", "null":
		return true
	}
	return keyQuoted && numberLiteral.MatchString(token)
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
