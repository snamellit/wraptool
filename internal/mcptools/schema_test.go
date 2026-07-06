package mcptools

import (
	"testing"

	"github.com/pti/wraptool/internal/config"
)

func TestIsBooleanFlag(t *testing.T) {
	boolean := []string{
		"--yes", "-y", "--force", "-f", "--dry-run", "--quiet", "--verbose", // well-known names
		"--no-edit", "--enable-foo", "--disable-bar", // prefix heuristic
	}
	for _, f := range boolean {
		if !IsBooleanFlag(f, nil) {
			t.Errorf("IsBooleanFlag(%q) = false, want true", f)
		}
	}

	notBoolean := []string{"--message", "-m", "--output", "--file", "--format", "--fail-fast"}
	for _, f := range notBoolean {
		if IsBooleanFlag(f, nil) {
			t.Errorf("IsBooleanFlag(%q) = true, want false", f)
		}
	}

	// An explicit flag_constraints entry wins over the well-known-name default,
	// both directions.
	forceAsString := map[string]config.FlagConstraint{"--force": {Type: "string"}}
	if IsBooleanFlag("--force", forceAsString) {
		t.Error("explicit string constraint should override the well-known boolean name --force")
	}
	outputAsBool := map[string]config.FlagConstraint{"--output": {Type: "boolean"}}
	if !IsBooleanFlag("--output", outputAsBool) {
		t.Error("explicit boolean constraint should make --output boolean")
	}

	// A constraint that sets other fields but no type must not suppress the
	// well-known-name / prefix heuristics (the empty-type trap).
	yesRequiredNoType := map[string]config.FlagConstraint{"--yes": {Required: true}}
	if !IsBooleanFlag("--yes", yesRequiredNoType) {
		t.Error("a typeless constraint (e.g. required:true) must fall back to the well-known boolean name")
	}
	noEditRequiredNoType := map[string]config.FlagConstraint{"--no-edit": {Required: true}}
	if !IsBooleanFlag("--no-edit", noEditRequiredNoType) {
		t.Error("a typeless constraint must fall back to the prefix heuristic")
	}
}

func TestToolName(t *testing.T) {
	tests := []struct {
		tool   string
		subcmd []string
		want   string
	}{
		{"git", []string{"commit"}, "git_commit"},
		{"git", []string{"log"}, "git_log"},
		{"gcloud", []string{"compute", "instances", "list"}, "gcloud_compute_instances_list"},
		{"kubectl", []string{"get"}, "kubectl_get"},
	}

	for _, tt := range tests {
		got := ToolName(tt.tool, tt.subcmd)
		if got != tt.want {
			t.Errorf("ToolName(%q, %v) = %q, want %q", tt.tool, tt.subcmd, got, tt.want)
		}
	}
}

func TestFlagToParam(t *testing.T) {
	tests := []struct {
		flag string
		want string
	}{
		{"--set-upstream", "set_upstream"},
		{"--message", "message"},
		{"-m", "m"},
		{"--no-edit", "no_edit"},
	}

	for _, tt := range tests {
		got := FlagToParam(tt.flag)
		if got != tt.want {
			t.Errorf("FlagToParam(%q) = %q, want %q", tt.flag, got, tt.want)
		}
	}
}

func TestParamToFlag(t *testing.T) {
	tests := []struct {
		param string
		want  string
	}{
		{"set_upstream", "--set-upstream"},
		{"message", "--message"},
		{"m", "-m"},
		{"no_edit", "--no-edit"},
	}

	for _, tt := range tests {
		got := ParamToFlag(tt.param)
		if got != tt.want {
			t.Errorf("ParamToFlag(%q) = %q, want %q", tt.param, got, tt.want)
		}
	}
}
