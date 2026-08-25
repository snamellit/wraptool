package executor

import (
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

// TestScrubCredentials_GlabRunnersTokenFixture is modeled on a real `glab
// api projects/<id>` response: a GitLab project object with a
// runners_token field alongside ordinary, non-sensitive diagnostic fields.
// The token value below is SYNTHETIC (never a real credential) but matches
// the shape (glrt-prefixed... actually plain alphanumeric, as GitLab
// returns it) and length class of the token that leaked in production.
func TestScrubCredentials_GlabRunnersTokenFixture(t *testing.T) {
	const syntheticToken = "GR13489412aQomMQNwDMSti1_HNoR"

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
  "wiki_enabled": true
}`

	got := ScrubCredentials(input)

	if strings.Contains(got, syntheticToken) {
		t.Fatalf("expected runners_token value to be scrubbed, got:\n%s", got)
	}
	if !strings.Contains(got, redacted) {
		t.Fatalf("expected %q in output, got:\n%s", redacted, got)
	}

	// Negative cases: ordinary diagnostic fields must survive untouched.
	// Over-redaction here would destroy output the agent legitimately
	// needs.
	for _, want := range []string{
		`"default_branch": "main"`,
		`"visibility": "internal"`,
		`"pages_access_level": "enabled"`,
		`"path_with_namespace": "example-group/example-project"`,
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
// truncated leak is still a leak.
func TestScrubCredentials_NoPartialLeak(t *testing.T) {
	const secret = "GR13489412aQomMQNwDMSti1_HNoR"
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
