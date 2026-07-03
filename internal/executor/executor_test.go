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
