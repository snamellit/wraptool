package toolname

import (
	"strings"
	"testing"
)

func TestParam(t *testing.T) {
	cases := map[string]string{
		"--set-upstream": "set_upstream",
		"-m":             "m",
		"--":             "",
		"-":              "",
		"--no-edit":      "no_edit",
	}
	for flag, want := range cases {
		if got := Param(flag); got != want {
			t.Errorf("Param(%q) = %q, want %q", flag, got, want)
		}
	}
}

func TestSanitize(t *testing.T) {
	// The permitted set is the intersection wraptool mints into: Gemini's
	// `[A-Za-z_][A-Za-z0-9_.:-]{0,127}` minus ":".
	cases := []struct{ in, want string }{
		{"sigil_sigil/lookup", "sigil_sigil_lookup"},                   // the reported agy/Gemini failure
		{"kubectl_config_get-contexts", "kubectl_config_get-contexts"}, // dashes are fine, unchanged
		{"9lives", "_9lives"},                                          // must not start with a digit
		{".hidden", "_.hidden"},
		{"a b:c", "a_b_c"},
		{"café/x", "caf__x"}, // one "_" per RUNE, not per byte
		{"", "_"},
		{strings.Repeat("a", MaxLen), strings.Repeat("a", MaxLen)}, // exactly at the cap, untouched
	}
	for _, c := range cases {
		if got := Sanitize(c.in); got != c.want {
			t.Errorf("Sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// Over-long names are truncated to the cap and kept distinct by a digest
	// of the whole pre-truncation name — a shared prefix past the cut must not
	// collapse two tools onto one registered name.
	long := strings.Repeat("a", MaxLen*2)
	for _, in := range []string{long, long + "_zzz", strings.Repeat("a", MaxLen+1)} {
		if got := Sanitize(in); len(got) != MaxLen {
			t.Errorf("Sanitize(<%d chars>) has length %d, want %d", len(in), len(got), MaxLen)
		}
	}
	if a, b := Sanitize(long), Sanitize(long+"_zzz"); a == b {
		t.Errorf("two long names sharing a prefix past the cut both sanitize to %q", a)
	}

	// Idempotence is what lets mcptools.NewRegisteredTool re-apply this at the
	// registration choke point without changing an already-minted name.
	for _, in := range []string{"sigil_sigil/lookup", "9lives", "", long, "café/x"} {
		once := Sanitize(in)
		if twice := Sanitize(once); twice != once {
			t.Errorf("Sanitize not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

func TestCLIJoinsAndSanitizes(t *testing.T) {
	cases := []struct {
		tool string
		sub  []string
		want string
	}{
		{"git", []string{"commit"}, "git_commit"},
		{"gcloud", []string{"compute", "instances", "list"}, "gcloud_compute_instances_list"},
		{"git", nil, "git"},
		{"acli", []string{"jira/workitem"}, "acli_jira_workitem"},
	}
	for _, c := range cases {
		if got := CLI(c.tool, c.sub); got != c.want {
			t.Errorf("CLI(%q, %q) = %q, want %q", c.tool, c.sub, got, c.want)
		}
	}
}

func TestProxyMatchesTheJoinItDocuments(t *testing.T) {
	if got, want := Proxy("sigil", "sigil/lookup"), Sanitize("sigil_sigil/lookup"); got != want {
		t.Errorf("Proxy = %q, want %q — the collision check and the minter must agree", got, want)
	}
}
