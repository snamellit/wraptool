package mcptools

import "testing"

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
