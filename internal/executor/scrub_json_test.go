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
func TestScrubCredentials_TruncatedValue(t *testing.T) {
	// zzzFAKEsecretTruncated carries none of the GitLab value-shape
	// prefixes, so this exercises the key/value scanning path specifically
	// rather than the standalone prefix patterns.
	const secretFragment = "zzzFAKEsecretTruncated"
	input := `{"id":42,"runners_token":"` + secretFragment

	got := ScrubCredentials(input)

	if strings.Contains(got, secretFragment) {
		t.Errorf("expected truncated secret fragment to be scrubbed, got %q", got)
	}
	if !strings.Contains(got, redacted) {
		t.Errorf("expected %q in output, got %q", redacted, got)
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
