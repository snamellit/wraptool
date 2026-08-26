package cmd

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/pti/wraptool/internal/requestlog"
)

func TestRequestsListJSON(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "requests.jsonl")
	cfgPath := writeCfg(t, "mcp:\n  transport: stdio\n  request_log: "+logPath+"\n")

	store := requestlog.NewStore(logPath)
	req := &requestlog.CapabilityRequest{Tool: "git", Subcommand: []string{"status"}, Rationale: "test"}
	if err := store.Append(req); err != nil {
		t.Fatalf("Append: %v", err)
	}

	root := NewRootCmd()
	root.SetArgs([]string{"requests", "list", "--json", "--config", cfgPath})
	out := captureStdout(t, func() {
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	var got []requestlog.CapabilityRequest
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal output: %v\noutput: %s", err, out)
	}
	if len(got) != 1 || got[0].ID != req.ID {
		t.Fatalf("got %+v, want one request with ID %s", got, req.ID)
	}
	if got[0].Tool != "git" || got[0].Rationale != "test" {
		t.Errorf("got %+v, want tool=git rationale=test", got[0])
	}
}

func TestRequestsListJSONEmpty(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "requests.jsonl")
	cfgPath := writeCfg(t, "mcp:\n  transport: stdio\n  request_log: "+logPath+"\n")

	root := NewRootCmd()
	root.SetArgs([]string{"requests", "list", "--json", "--config", cfgPath})
	out := captureStdout(t, func() {
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	var got []requestlog.CapabilityRequest
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal output: %v\noutput: %s", err, out)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want an empty array", got)
	}
}
