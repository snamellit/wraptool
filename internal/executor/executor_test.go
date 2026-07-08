package executor

import (
	"strings"
	"testing"
)

// TestChildEnv locks the env-isolation contract shared by CLI tools and (from
// Phase 1) spawned stdio MCP servers: PATH (binary dir prepended), HOME, LANG,
// plus the resolved vars — and nothing inherited from wraptool's own env.
func TestChildEnv(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	t.Setenv("PATH", "/usr/bin:/bin") // deterministic base for the assertion below
	t.Setenv("WRAPTOOL_SECRET", "must-not-leak")

	env := ChildEnv("/opt/tools/gcloud", map[string]string{"CLOUDSDK_CORE_PROJECT": "demo"})

	want := map[string]string{
		"PATH":                  "/opt/tools:/usr/bin:/bin",
		"HOME":                  "/home/tester",
		"LANG":                  "C.UTF-8",
		"CLOUDSDK_CORE_PROJECT": "demo",
	}
	got := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}

	for k, v := range want {
		if got[k] != v {
			t.Errorf("ChildEnv[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, leaked := got["WRAPTOOL_SECRET"]; leaked {
		t.Error("ChildEnv leaked a parent environment variable (WRAPTOOL_SECRET)")
	}
	if len(got) != len(want) {
		t.Errorf("ChildEnv has %d vars, want exactly %d: %v", len(got), len(want), env)
	}
}

func TestChildPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin") // deterministic base
	cases := []struct {
		binary string
		want   string
	}{
		{"/home/pti/google-cloud-sdk/bin/kubectl", "/home/pti/google-cloud-sdk/bin:/usr/bin:/bin"},
		{"/opt/tools/gcloud", "/opt/tools:/usr/bin:/bin"},
		{"/usr/bin/git", "/usr/bin:/bin"}, // already in base, no duplicate
		{"/bin/ls", "/usr/bin:/bin"},
		{"kubectl", "/usr/bin:/bin"}, // bare name → "." → base only
	}
	for _, c := range cases {
		if got := childPath(c.binary); got != c.want {
			t.Errorf("childPath(%q) = %q, want %q", c.binary, got, c.want)
		}
	}
}

// TestChildPathInheritsServerPATH locks the option-3 behaviour: the child PATH
// is the server's PATH (so genuine deps like ssh, in their own store prefix,
// resolve), with the tool dir prepended and no duplicate entry.
func TestChildPathInheritsServerPATH(t *testing.T) {
	t.Setenv("PATH", "/gnu/store/openssh/bin:/usr/bin:/bin")

	if got, want := childPath("/gnu/store/git/bin/git"), "/gnu/store/git/bin:/gnu/store/openssh/bin:/usr/bin:/bin"; got != want {
		t.Errorf("childPath prepends tool dir to server PATH: got %q, want %q", got, want)
	}
	// A dir already on the server PATH is not duplicated.
	if got, want := childPath("/gnu/store/openssh/bin/ssh"), "/gnu/store/openssh/bin:/usr/bin:/bin"; got != want {
		t.Errorf("childPath must not duplicate an existing entry: got %q, want %q", got, want)
	}
}

// TestChildPathFallback: an empty server PATH falls back to the minimal base.
func TestChildPathFallback(t *testing.T) {
	t.Setenv("PATH", "")
	if got, want := childPath("/opt/tools/gcloud"), "/opt/tools:/usr/bin:/bin"; got != want {
		t.Errorf("childPath fallback: got %q, want %q", got, want)
	}
}
