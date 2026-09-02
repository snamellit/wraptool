package executor

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestScrubCredentials_KeyValueForms covers every sensitive key name from
// sensitiveKeyNames in both shell (key=value) and JSON ("key": "value")
// notation. Before the JSON-aware separator fix, none of the JSON-form
// cases redacted anything: the single-character separator class `[=:]`
// cannot match JSON's three-character `":"` key/value separator.
func TestScrubCredentials_KeyValueForms(t *testing.T) {
	type form struct {
		name  string
		input string
	}

	// secret is deliberately 12 characters — well under the old 40-char
	// floor — so these cases also exercise "short value with a sensitive
	// key must still redact".
	const secret = "hunter2xyz9k"

	keys := []string{
		"token",
		"secret",
		"password",
		"passwd",
		"credential",
		"private_key",
		"private-key",
		"api_key",
		"api-key",
		"apiKey", // camelCase, no separator between "api" and "Key"
	}

	for _, key := range keys {
		forms := []form{
			{name: "shell", input: key + "=" + secret},
			{name: "json", input: `"` + key + `":"` + secret + `"`},
			{name: "json-spaced", input: `"` + key + `" : "` + secret + `"`},
		}
		for _, f := range forms {
			t.Run(key+"/"+f.name, func(t *testing.T) {
				got := ScrubCredentials(f.input)
				if strings.Contains(got, secret) {
					t.Errorf("expected %q to be scrubbed from %q, got %q", secret, f.input, got)
				}
				if !strings.Contains(got, redacted) {
					t.Errorf("expected %q in output, got %q", redacted, got)
				}
			})
		}
	}
}

// TestScrubCredentials_BareKeyName covers key names that are only "key",
// compound with "key" but not "api_key"/"private_key" specifically, or a
// bare "key" on its own — a regression check against the base branch,
// which redacted these via a separate generic key-name pattern that was
// dropped (and its coverage folded into sensitiveKeyNames) during the
// JSON-separator fix.
func TestScrubCredentials_BareKeyName(t *testing.T) {
	const secret40 = "abcdefghijklmnopqrstuvwxyz1234567890abcd"

	cases := []struct {
		name  string
		input string
	}{
		{"signing_key", "signing_key=" + secret40},
		{"deploy_key", "deploy_key=" + secret40},
		{"session_key_colon", "session_key: " + secret40},
		{"bare_key", "key=" + secret40},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := ScrubCredentials(tt.input)
			if strings.Contains(got, secret40) {
				t.Errorf("expected %q to be scrubbed from %q, got %q", secret40, tt.input, got)
			}
			if !strings.Contains(got, redacted) {
				t.Errorf("expected %q in output, got %q", redacted, got)
			}
		})
	}
}

// TestScrubCredentials_ArrayAndObjectValues covers F1: a sensitive key
// whose JSON value is an array or a nested object. The unquoted-value
// pattern this package used to rely on (`[^\s,}]+`) stops at the first
// comma, space, or `}`, so it swallowed only the opening bracket and left
// every array element or nested field exposed. redactSensitiveValues scans
// brackets by hand (scanBalanced) instead, so the whole value — however
// deeply nested — is consumed and replaced as one unit. Covers both
// compact and pretty-printed (kubectl -o json / gcloud --format=json
// default) formatting.
func TestScrubCredentials_ArrayAndObjectValues(t *testing.T) {
	cases := []struct {
		name  string
		input string
		leak  []string // substrings that must NOT survive in the output
	}{
		{
			name:  "array_compact",
			input: `{"tokens":["aaaaSECRET1","bbbbSECRET2"],"x":1}`,
			leak:  []string{"aaaaSECRET1", "bbbbSECRET2"},
		},
		{
			name: "array_pretty",
			input: `{
  "tokens": [
    "aaaaSECRET1",
    "bbbbSECRET2"
  ],
  "x": 1
}`,
			leak: []string{"aaaaSECRET1", "bbbbSECRET2"},
		},
		{
			name:  "nested_object_compact",
			input: `{"credential":{"type":"pat","value":"SECRETVALUE"}}`,
			leak:  []string{"SECRETVALUE"},
		},
		{
			name: "nested_object_pretty",
			input: `{
  "credential": {
    "type": "pat",
    "value": "SECRETVALUE"
  }
}`,
			leak: []string{"SECRETVALUE"},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := ScrubCredentials(tt.input)
			for _, secret := range tt.leak {
				if strings.Contains(got, secret) {
					t.Errorf("expected %q to be scrubbed, got:\n%s", secret, got)
				}
			}
			if !strings.Contains(got, redacted) {
				t.Errorf("expected %q in output, got:\n%s", redacted, got)
			}
			if !json.Valid([]byte(got)) {
				t.Errorf("expected redacted output to remain valid JSON, got:\n%s", got)
			}
		})
	}
}

// TestScrubCredentials_TruncatedValue covers MCP output truncated
// mid-document (executor.Result.Truncated can be true) with no closing
// quote on a sensitive value. This must still redact the truncated
// fragment rather than leak it — that's the current, intended behaviour,
// but it was previously unverified by any test, so a future refactor of
// scanSensitiveValue/scanJSONString could silently regress it.
//
// The fragment deliberately contains a space, a comma, and a closing
// brace: scanBareToken (the unquoted-value scanner) stops at all three, so
// a version of this fix that fell through to it — rather than redacting
// the whole remainder outright — would only redact up to the first one and
// leak everything after it (see TestScrubCredentials_TruncatedValueMidString
// for the exact production shape, a truncated PEM private key, that
// motivated this).
func TestScrubCredentials_TruncatedValue(t *testing.T) {
	// zzzFAKEsecretTruncated carries none of the GitLab value-shape
	// prefixes, so this exercises the key/value scanning path specifically
	// rather than the standalone prefix patterns.
	const secretFragment = `zzzFAKEsecretTruncated with a space, and a } brace STILLSECRET`
	input := `{"id":42,"runners_token":"` + secretFragment

	got := ScrubCredentials(input)

	if strings.Contains(got, secretFragment) {
		t.Errorf("expected truncated secret fragment to be scrubbed, got %q", got)
	}
	if strings.Contains(got, "STILLSECRET") {
		t.Errorf("expected text after the first space/comma/brace to be scrubbed too, got %q", got)
	}
	if !strings.Contains(got, redacted) {
		t.Errorf("expected %q in output, got %q", redacted, got)
	}
}

// TestScrubCredentials_TruncatedValueMidString covers F8: an unterminated
// quoted value used to fall through to scanBareToken, which stops at the
// first whitespace, comma, or closing brace — so only the HEAD of the
// secret was replaced and everything past that character leaked. Worse,
// for a PEM-shaped key it ate the "-----BEGIN" prefix along with it,
// defeating the standalone PEM value-shape pattern that would otherwise
// have caught the leak as a backstop. This is not exotic: limitedWriter
// (executor.go) cuts output at defaultMaxOutput and can sever a value
// mid-string on any ordinary command whose output happens to be large.
func TestScrubCredentials_TruncatedValueMidString(t *testing.T) {
	cases := []struct {
		name  string
		input string
		leak  []string
	}{
		{
			name: "pem_private_key",
			input: `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----` +
				"\n" + `MIIEvQIBADANBgkqSYNTHETICKEYMATERIALFRAGMENT`,
			leak: []string{"-----BEGIN", "MIIEvQIBADANBgkq", "SYNTHETICKEYMATERIALFRAGMENT"},
		},
		{
			name:  "password_with_space",
			input: `"password":"correct horse battery staple`,
			leak:  []string{"correct horse battery staple", "battery staple"},
		},
		{
			name:  "runners_token_comma_tail",
			input: `"runners_token":"tok, with comma tail`,
			leak:  []string{"tok, with comma tail", "comma tail"},
		},
		{
			name:  "api_key_brace_tail",
			input: `"api_key":"abc}tail`,
			leak:  []string{"abc}tail", "}tail"},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := ScrubCredentials(tt.input)
			for _, frag := range tt.leak {
				if strings.Contains(got, frag) {
					t.Errorf("expected %q not to survive scrubbing, got:\n%s", frag, got)
				}
			}
			if !strings.Contains(got, redacted) {
				t.Errorf("expected %q in output, got:\n%s", redacted, got)
			}
		})
	}
}

// TestScrubCredentials_KubectlDescribeShapes covers F4: `kubectl describe
// pod` output has bare "Label:" lines followed by an unrelated line, which
// a whitespace separator (`\s*`, matching newlines) mistook for a
// multi-line key/value pair — merging two lines and leaving a dangling,
// misleadingly-placed fragment behind. Restricting the separator to
// `[ \t]*` means a key only redacts when it actually has a value on the
// same line, so this kind of output is left untouched instead of being
// mangled.
func TestScrubCredentials_KubectlDescribeShapes(t *testing.T) {
	cases := []string{
		"Tokens:\nEvents:              <none>",
		"Secrets:\n  default-token-abcde\nEvents:  <none>",
	}

	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			if got := ScrubCredentials(input); got != input {
				t.Errorf("expected input unchanged, got:\ninput:  %q\noutput: %q", input, got)
			}
		})
	}
}

// TestScrubCredentials_GlabRunnersTokenFixture is modeled on a real `glab
// api projects/<id>` response: a GitLab project object with a
// runners_token field alongside ordinary, non-sensitive diagnostic fields,
// plus two fields (runners_token_expires_at, ci_job_token_scope_enabled)
// that sit in the same real response and whose key names also contain
// "token" but whose values (null, false) can never be credentials.
//
// syntheticToken is deliberately fake (an obviously non-real placeholder)
// and carries none of the GitLab value-shape prefixes (glpat-, GR1348941,
// ...), so this test exercises the key-name-driven redaction path — not
// the standalone value-shape patterns, which are covered separately by
// TestScrubCredentials_GitLabValuePrefixes.
func TestScrubCredentials_GlabRunnersTokenFixture(t *testing.T) {
	const syntheticToken = "FAKE-TEST-RUNNERS-TOKEN-0000000000000"

	input := `{
  "id": 42,
  "description": "",
  "name": "example-project",
  "name_with_namespace": "Example Group / example-project",
  "path": "example-project",
  "path_with_namespace": "example-group/example-project",
  "default_branch": "main",
  "visibility": "internal",
  "pages_access_level": "enabled",
  "runners_token": "` + syntheticToken + `",
  "runners_token_expires_at": null,
  "ci_job_token_scope_enabled": false,
  "wiki_enabled": true
}`

	if !json.Valid([]byte(input)) {
		t.Fatalf("test fixture is not valid JSON:\n%s", input)
	}

	got := ScrubCredentials(input)

	if strings.Contains(got, syntheticToken) {
		t.Fatalf("expected runners_token value to be scrubbed, got:\n%s", got)
	}
	if !strings.Contains(got, redacted) {
		t.Fatalf("expected %q in output, got:\n%s", redacted, got)
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("expected redacted output to remain valid JSON, got:\n%s", got)
	}

	// Negative cases: ordinary diagnostic fields — including ones whose key
	// names contain "token" but whose values are JSON literals — must
	// survive untouched. Over-redaction here would destroy output the
	// agent legitimately needs.
	for _, want := range []string{
		`"default_branch": "main"`,
		`"visibility": "internal"`,
		`"pages_access_level": "enabled"`,
		`"path_with_namespace": "example-group/example-project"`,
		`"runners_token_expires_at": null`,
		`"ci_job_token_scope_enabled": false`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected unrelated field %q to survive scrubbing, got:\n%s", want, got)
		}
	}
}

// TestScrubCredentials_GitLabValuePrefixes covers the GitLab token-shape
// prefixes that must be redacted even with no key name attached at all
// (e.g. a token embedded in a URL or curl command).
func TestScrubCredentials_GitLabValuePrefixes(t *testing.T) {
	prefixes := []string{
		"glpat",
		"glrt",
		"gldt",
		"glsoat",
		"glimt",
		"glptt",
		"glcbt",
	}

	for _, prefix := range prefixes {
		t.Run(prefix, func(t *testing.T) {
			token := prefix + "-abcDEF123456ghiJKL"
			input := "remote: https://oauth2:" + token + "@gitlab.example.com/group/project.git"

			got := ScrubCredentials(input)

			if strings.Contains(got, token) {
				t.Errorf("expected %q to be scrubbed, got %q", token, got)
			}
			// No fragment (prefix or suffix) of the token may survive.
			if strings.Contains(got, prefix+"-") {
				t.Errorf("expected token prefix %q not to survive scrubbing, got %q", prefix+"-", got)
			}
			if strings.Contains(got, "ghiJKL") {
				t.Errorf("expected token suffix not to survive scrubbing, got %q", got)
			}
			if !strings.Contains(got, redacted) {
				t.Errorf("expected %q in output, got %q", redacted, got)
			}
		})
	}

	// GR1348941 is GitLab's runner-registration token prefix — a distinct,
	// fixed literal (not one of the gl*-prefixed token families above),
	// used with no key name attached (e.g. `gitlab-runner register
	// --registration-token GR1348941...`).
	t.Run("runner_registration_prefix", func(t *testing.T) {
		token := "GR13489412aQomMQNwDMSti1_HNoR"
		input := "register runner with token " + token

		got := ScrubCredentials(input)

		if strings.Contains(got, token) {
			t.Errorf("expected %q to be scrubbed, got %q", token, got)
		}
		if strings.Contains(got, "GR1348941") || strings.Contains(got, "HNoR") {
			t.Errorf("expected no fragment of the token to survive, got %q", got)
		}
		if !strings.Contains(got, redacted) {
			t.Errorf("expected %q in output, got %q", redacted, got)
		}
	})
}

// TestScrubCredentials_NegativeCases asserts ordinary, non-sensitive JSON
// key/value pairs are left completely untouched — over-redaction destroys
// output the agent needs and is treated as a defect, same as under-redaction.
func TestScrubCredentials_NegativeCases(t *testing.T) {
	cases := []string{
		`"pages_access_level":"enabled"`,
		`"default_branch":"main"`,
		`"visibility":"internal"`,
		`{"id":42,"name":"example-project","archived":false}`,
		`commit abc123
Author: user <user@example.com>
fixed the bug`,
	}

	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			if got := ScrubCredentials(input); got != input {
				t.Errorf("expected input unchanged, got:\ninput:  %s\noutput: %s", input, got)
			}
		})
	}
}

// TestScrubCredentials_NoPartialLeak asserts that once a value is matched
// as a credential, no prefix or suffix of it survives redaction — a
// truncated leak is still a leak. Uses a synthetic value with no
// recognized GitLab value-shape prefix, so this exercises the key/value
// scanning path (not the standalone GR1348941 pattern, which would also
// catch this value on its own and make the test pass even if the
// key/value path were broken).
func TestScrubCredentials_NoPartialLeak(t *testing.T) {
	const secret = "zQ7mN2kXFAKEsyntheticTESTvalue9pL"
	input := `"runners_token":"` + secret + `"`

	got := ScrubCredentials(input)

	for i := 4; i <= len(secret); i += 4 {
		if strings.Contains(got, secret[:i]) {
			t.Errorf("prefix %q of the secret survived scrubbing: %q", secret[:i], got)
		}
		if strings.Contains(got, secret[len(secret)-i:]) {
			t.Errorf("suffix %q of the secret survived scrubbing: %q", secret[len(secret)-i:], got)
		}
	}
}

// TestScrubCredentials_BareKeyNegatives covers F9: "key" is a label-selector
// field name Kubernetes uses constantly — a pod toleration, a node-affinity
// matchExpression, a ConfigMap/Secret volume projection item — and is never
// itself a secret value. Narrowing sensitiveKeyNames' "key" alternative to
// require a preceding word character (`[\w-]+key`) drops these while still
// catching every compound spelling (signing_key, deploy_key, ...); see
// TestScrubCredentials_BareKeyName and TestScrubCredentials_CompoundKeyStillMatches
// for the positive side.
func TestScrubCredentials_BareKeyNegatives(t *testing.T) {
	cases := []string{
		// kubectl: a pod toleration.
		`{"key":"node-role.kubernetes.io/control-plane","operator":"Exists","effect":"NoSchedule"}`,
		// kubectl: a node-affinity matchExpression.
		`{"key":"kubernetes.io/hostname","operator":"In","values":["node-1"]}`,
		// kubectl: a ConfigMap/Secret volume projection item.
		`{"key":"app.config","path":"app/config.yaml"}`,
		// gcloud: a KMS key-ring identifier. No word character precedes
		// "key" ("Ring" comes after it), so this is dropped; a sibling
		// identifier like cryptoKeyId, where "crypto" DOES precede "key",
		// is a known, accepted residual (still redacted) — not promised to
		// be fixed, since undoing that would require anchoring "key" more
		// tightly than "at least one preceding word character", which
		// would also drop signing_key/deploy_key/session_key.
		`"keyRingId":"my-ring"`,
		// A short, bare-"key" JSON value — must also survive the
		// bareKeyLongValue 40+-character backstop (see
		// TestScrubCredentials_BareKeyLongValueFloor).
		`"key":"kubernetes.io/hostname"`,
		// Ordinary prose where "key" starts the word, with nothing before
		// it to require a redaction.
		`keyboard: broken`,
	}

	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			if got := ScrubCredentials(input); got != input {
				t.Errorf("expected input unchanged, got:\ninput:  %s\noutput: %s", input, got)
			}
		})
	}
}

// TestScrubCredentials_CompoundKeyStillMatches is the positive side of F9:
// narrowing "key" to require a preceding word character must not regress
// any compound key name, including ones with no separator at all before
// "key" (pubkey) or where "key" sits mid-identifier (aws_secret_access_key,
// already covered separately via "secret").
func TestScrubCredentials_CompoundKeyStillMatches(t *testing.T) {
	const secret40 = "abcdefghijklmnopqrstuvwxyz1234567890abcd"

	cases := []struct {
		name  string
		input string
	}{
		{"pubkey", "pubkey=" + secret40},
		{"aws_secret_access_key", "aws_secret_access_key=" + secret40},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := ScrubCredentials(tt.input)
			if strings.Contains(got, secret40) {
				t.Errorf("expected %q to be scrubbed from %q, got %q", secret40, tt.input, got)
			}
			if !strings.Contains(got, redacted) {
				t.Errorf("expected %q in output, got %q", redacted, got)
			}
		})
	}
}

// TestScrubCredentials_BareKeyLongValueFloor covers bareKeyLongValue: the
// bare (non-compound) spelling "key" only redacts alongside a 40+ character
// value — restoring this package's pre-JSON-fix behaviour for that one
// specific spelling, since the F9 narrowing above would otherwise silently
// regress a genuine `key=<40+ char secret>` shell assignment.
func TestScrubCredentials_BareKeyLongValueFloor(t *testing.T) {
	const long40 = "abcdefghijklmnopqrstuvwxyz1234567890abcd" // exactly 40 chars

	t.Run("long_value_redacts", func(t *testing.T) {
		input := "key=" + long40
		got := ScrubCredentials(input)
		if strings.Contains(got, long40) {
			t.Errorf("expected %q to be scrubbed, got %q", long40, got)
		}
		if !strings.Contains(got, redacted) {
			t.Errorf("expected %q in output, got %q", redacted, got)
		}
	})

	t.Run("short_value_survives", func(t *testing.T) {
		input := `"key":"kubernetes.io/hostname"` // 22 characters
		if got := ScrubCredentials(input); got != input {
			t.Errorf("expected input unchanged, got:\ninput:  %s\noutput: %s", input, got)
		}
	})
}

// TestScrubCredentials_NumericValueContext covers F10: a bare number is
// only ever a credential in shell/prose context (key: 123456, unquoted
// key), which is what this package's own patterns redacted before the
// JSON-separator fix; the fix's unconditional "never redact a number"
// skip regressed that. In genuine JSON (a quoted key), a number sharing a
// token-ish key name is routine diagnostic data (a count, a Unix
// timestamp) and must survive — see TestScrubCredentials_GlabRunnersTokenFixture
// for the JSON-literal case this preserves. true/false/null are skipped in
// both contexts (also covered here).
func TestScrubCredentials_NumericValueContext(t *testing.T) {
	t.Run("unquoted_key_number_redacts", func(t *testing.T) {
		cases := []string{
			`password: 123456`,
			`credential: 000123456789`,
		}
		for _, input := range cases {
			t.Run(input, func(t *testing.T) {
				got := ScrubCredentials(input)
				if got == input {
					t.Errorf("expected %q to be redacted (shell/prose context, base parity), got unchanged", input)
				}
				if !strings.Contains(got, redacted) {
					t.Errorf("expected %q in output, got %q", redacted, got)
				}
			})
		}
	})

	t.Run("quoted_key_number_survives", func(t *testing.T) {
		cases := []string{
			`{"token_count":12345}`,
			`{"runners_token_expires_at":1735689600}`,
		}
		for _, input := range cases {
			t.Run(input, func(t *testing.T) {
				if got := ScrubCredentials(input); got != input {
					t.Errorf("expected input unchanged (JSON context), got:\ninput:  %s\noutput: %s", input, got)
				}
			})
		}
	})

	t.Run("literals_always_survive", func(t *testing.T) {
		cases := []string{
			`{"ci_job_token_scope_enabled":false}`,
			`{"runners_token_expires_at":null}`,
			`token_enabled: true`,
		}
		for _, input := range cases {
			t.Run(input, func(t *testing.T) {
				if got := ScrubCredentials(input); got != input {
					t.Errorf("expected input unchanged (literal, any context), got:\ninput:  %s\noutput: %s", input, got)
				}
			})
		}
	})
}

// TestScrubCredentials_ScannerInvariants pins three scanner behaviours that
// the test suite otherwise doesn't catch a regression of — each case below
// leaks a real secret fragment if the described logic is broken or removed
// (verified by deliberately reverting each fix locally and re-running this
// test, per F11).
func TestScrubCredentials_ScannerInvariants(t *testing.T) {
	t.Run("escaped_quote_does_not_end_string_early", func(t *testing.T) {
		// scanJSONString must treat \" as an escaped quote (skip 2 bytes),
		// not as the closing quote (skip 1). Getting this wrong ends the
		// "token" value's scan right after the escaped quote, leaking
		// everything from there on — including the unrelated "x" field.
		input := `{"token":"aa\"SECRETTAIL","x":"kept"}`
		got := ScrubCredentials(input)
		if strings.Contains(got, "SECRETTAIL") {
			t.Errorf("expected escaped-quote value to be fully scrubbed, got %q", got)
		}
		if !strings.Contains(got, "kept") {
			t.Errorf("expected unrelated field to survive, got %q", got)
		}
	})

	t.Run("bracket_inside_string_does_not_end_array_early", func(t *testing.T) {
		// scanBalanced must skip over quoted-string contents (via
		// scanJSONString) when counting bracket depth, not count a `]`
		// that appears inside a string element as the array's own close.
		// Getting this wrong ends the array scan right after "a]b",
		// leaking the second element in cleartext.
		input := `{"tokens":["a]b","SECRET2"],"x":1}`
		got := ScrubCredentials(input)
		if strings.Contains(got, "SECRET2") {
			t.Errorf("expected array element after an in-string bracket to be scrubbed, got %q", got)
		}
	})

	t.Run("truncated_array_redacts_in_full", func(t *testing.T) {
		// A truncated (never-closed) array value must still be redacted as
		// a whole, the same fail-closed guarantee scanJSONString provides
		// for a truncated quoted value (TestScrubCredentials_TruncatedValue).
		input := `{"tokens":["aaaaSECRET1","bbbbFAKEtruncatedSECRETtail`
		got := ScrubCredentials(input)
		if strings.Contains(got, "aaaaSECRET1") || strings.Contains(got, "FAKEtruncatedSECRETtail") {
			t.Errorf("expected truncated array to be scrubbed in full, got %q", got)
		}
		if !strings.Contains(got, redacted) {
			t.Errorf("expected %q in output, got %q", redacted, got)
		}
	})

	t.Run("truncated_object_redacts_in_full", func(t *testing.T) {
		input := `{"credential":{"type":"pat","value":"FAKEtruncatedOBJECTsecret`
		got := ScrubCredentials(input)
		if strings.Contains(got, "FAKEtruncatedOBJECTsecret") {
			t.Errorf("expected truncated object to be scrubbed in full, got %q", got)
		}
		if !strings.Contains(got, redacted) {
			t.Errorf("expected %q in output, got %q", redacted, got)
		}
	})
}

// TestScrubCredentials_BoundedBalancedScan covers F12: a stray, unmatched
// `[` or `{` after a sensitive key in ordinary (non-JSON) output — e.g. a
// `git log` commit body containing "signing_key: {" with no closing brace
// anywhere afterward — must not swallow everything that follows it up to
// the end of a possibly-huge output. Beyond maxBalancedScanBytes with no
// closing bracket found and more input left, only the bracket itself is
// redacted.
func TestScrubCredentials_BoundedBalancedScan(t *testing.T) {
	filler := strings.Repeat("x", maxBalancedScanBytes+500)
	const tail = "COMMIT-MESSAGE-TAIL-MUST-SURVIVE"
	input := "signing_key: {\n" + filler + "\n" + tail

	got := ScrubCredentials(input)

	if !strings.Contains(got, tail) {
		t.Errorf("expected trailing text beyond the scan budget to survive untouched, got a %d-byte result not containing %q", len(got), tail)
	}
	if !strings.Contains(got, redacted) {
		t.Errorf("expected %q in output (the stray bracket itself), got a %d-byte result", redacted, len(got))
	}
}

// TestScrubCredentials_NoLegacyReCollapse covers F13: the legacy ALL-CAPS
// env-assignment pattern is case-insensitive and shares keywords (SECRET,
// TOKEN, ...) with sensitiveKeyNames, so it used to re-match text
// redactSensitiveValues had already redacted — e.g. matching "SECRET"
// inside "aws_secret_access_key=[REDACTED]" and collapsing it down to
// "aws_[REDACTED]", losing part of the key name for no reason (redaction
// was already complete). Excluding `[`/`]` from its value class stops it
// from ever matching a "[REDACTED]" marker as a "value".
func TestScrubCredentials_NoLegacyReCollapse(t *testing.T) {
	input := "aws_secret_access_key=wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"
	want := "aws_secret_access_key=" + redacted

	got := ScrubCredentials(input)

	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}
