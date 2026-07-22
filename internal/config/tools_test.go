package config

import (
	"strings"
	"testing"
)

func TestAddTool_CreatesInertTool(t *testing.T) {
	cfgPath, binPath := writeTestConfig(t, rulesBaseConfig)
	// A second tool sharing the same real binary keeps validation happy.
	if err := AddTool(cfgPath, "kubectl", binPath); err != nil {
		t.Fatalf("AddTool: %v", err)
	}
	got := readFile(t, cfgPath)
	if !strings.Contains(got, "kubectl:") || !strings.Contains(got, binPath) {
		t.Errorf("new tool missing:\n%s", got)
	}
	// Pre-existing comment survives.
	if !strings.Contains(got, "# config") {
		t.Errorf("comment lost:\n%s", got)
	}
}

func TestAddTool_DuplicateRejected(t *testing.T) {
	cfgPath, binPath := writeTestConfig(t, rulesBaseConfig)
	if err := AddTool(cfgPath, "git", binPath); err == nil {
		t.Fatal("expected error adding an existing tool")
	}
}

func TestAddTool_MissingBinaryRejected(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	// A non-existent binary fails the round-trip validation.
	if err := AddTool(cfgPath, "kubectl", "/no/such/binary"); err == nil {
		t.Fatal("expected error for a non-existent binary")
	}
}

func TestAddTool_InvalidNameRejected(t *testing.T) {
	cfgPath, binPath := writeTestConfig(t, rulesBaseConfig)
	// Empty, a path separator (would break the /tools/{tool}/... routes), and a
	// space are all rejected by the MCP name charset.
	for _, bad := range []string{"", "my/tool", "has space"} {
		if err := AddTool(cfgPath, bad, binPath); err == nil {
			t.Errorf("expected error for invalid tool name %q", bad)
		}
	}
}

func TestSetToolMetadata_SetAndClear(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := SetToolMetadata(cfgPath, "git", "/srv/project", "45s", []string{"/srv/project/.env"}); err != nil {
		t.Fatalf("SetToolMetadata: %v", err)
	}
	got := readFile(t, cfgPath)
	for _, want := range []string{"working_dir:", "/srv/project", "timeout: 45s", "env_files:", ".env"} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
	// Empty values remove the keys.
	if err := SetToolMetadata(cfgPath, "git", "", "", nil); err != nil {
		t.Fatalf("SetToolMetadata clear: %v", err)
	}
	got = readFile(t, cfgPath)
	for _, gone := range []string{"working_dir", "timeout", "env_files"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s not removed when cleared:\n%s", gone, got)
		}
	}
}

func TestSetToolMetadata_BadTimeoutRejected(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	// A non-duration timeout fails the round-trip validation.
	if err := SetToolMetadata(cfgPath, "git", "", "notaduration", nil); err == nil {
		t.Fatal("expected error for an unparseable timeout")
	}
}

func TestSetToolMetadata_UnknownToolRejected(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := SetToolMetadata(cfgPath, "nope", "/x", "", nil); err == nil {
		t.Fatal("expected error for an unconfigured tool")
	}
}

func TestDeleteTool_RemovesTool(t *testing.T) {
	cfgPath, binPath := writeTestConfig(t, rulesBaseConfig)
	// Add a second tool so removing the first still leaves a valid config.
	if err := AddTool(cfgPath, "kubectl", binPath); err != nil {
		t.Fatalf("AddTool: %v", err)
	}
	if err := DeleteTool(cfgPath, "git"); err != nil {
		t.Fatalf("DeleteTool: %v", err)
	}
	got := readFile(t, cfgPath)
	if strings.Contains(got, "git:") {
		t.Errorf("git tool not removed:\n%s", got)
	}
	if !strings.Contains(got, "kubectl:") {
		t.Errorf("kubectl tool should survive:\n%s", got)
	}
}

func TestDeleteTool_LastToolRejected(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	// git is the only tool; removing it leaves nothing to wrap, so validation
	// refuses it and the file is unchanged.
	before := readFile(t, cfgPath)
	if err := DeleteTool(cfgPath, "git"); err == nil {
		t.Fatal("expected error deleting the last tool")
	}
	if readFile(t, cfgPath) != before {
		t.Error("config changed despite rejected delete")
	}
}

func TestDeleteTool_UnknownRejected(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := DeleteTool(cfgPath, "nope"); err == nil {
		t.Fatal("expected error deleting an unconfigured tool")
	}
}
