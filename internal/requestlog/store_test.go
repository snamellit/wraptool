package requestlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "events.jsonl"))
}

func TestRequestLifecycleProjection(t *testing.T) {
	s := newTestStore(t)

	req := &CapabilityRequest{Tool: "git", Subcommand: []string{"push"}, Rationale: "deploy"}
	if err := s.Append(req); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if req.ID == "" || req.Status != StatusPending {
		t.Fatalf("Append did not stamp request: %+v", req)
	}

	got, err := s.Get(req.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusPending {
		t.Fatalf("status = %q, want pending", got.Status)
	}

	if err := s.UpdateStatus(req.ID, StatusDenied, "too broad"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, _ = s.Get(req.ID)
	if got.Status != StatusDenied || got.DenyReason != "too broad" {
		t.Fatalf("after deny: %+v", got)
	}

	// Status filtering reflects the projected state.
	pending, _ := s.List(StatusPending)
	if len(pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(pending))
	}
	denied, _ := s.List(StatusDenied)
	if len(denied) != 1 {
		t.Fatalf("denied = %d, want 1", len(denied))
	}
}

// TestToolCallsSurviveApproval is the regression guard for the original bug:
// approving/denying a request used to rewrite the whole log as requests,
// destroying interleaved tool_call events. In the event log they must persist.
func TestToolCallsSurviveApproval(t *testing.T) {
	s := newTestStore(t)

	if err := s.AppendToolCall(&ToolCallEvent{Tool: "git", Subcommand: []string{"status"}, Allowed: true, ExitCode: 0}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	req := &CapabilityRequest{Tool: "kubectl", Subcommand: []string{"get"}}
	if err := s.Append(req); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if err := s.AppendToolCall(&ToolCallEvent{Tool: "gcloud", Subcommand: []string{"auth"}, Allowed: false, DenyReason: "not whitelisted"}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	// The event that historically corrupted the log.
	if err := s.UpdateStatus(req.ID, StatusApproved, ""); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	calls, err := s.ListToolCalls()
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("tool calls = %d, want 2 (approval clobbered them?)", len(calls))
	}
	if calls[0].Tool != "git" || calls[1].Tool != "gcloud" {
		t.Fatalf("tool call order/content wrong: %+v", calls)
	}
	if calls[1].Allowed || calls[1].DenyReason != "not whitelisted" {
		t.Fatalf("denied call detail lost: %+v", calls[1])
	}

	// And the request itself is approved.
	got, _ := s.Get(req.ID)
	if got.Status != StatusApproved {
		t.Fatalf("request status = %q, want approved", got.Status)
	}
}

// TestAppendToolCallEnforcesInvariant pins the core invariant: an allowed call
// can never carry a deny reason, so downstream consumers classify on Allowed
// alone. A contradictory event is normalized at the append boundary rather than
// persisted as a state that has to be reconciled later.
func TestAppendToolCallEnforcesInvariant(t *testing.T) {
	s := newTestStore(t)

	// Buggy caller: allowed=true AND a deny reason. The reason must be dropped.
	if err := s.AppendToolCall(&ToolCallEvent{
		Tool: "acli", Allowed: true, DenyReason: "should be cleared",
	}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}
	// A genuine denial keeps its reason.
	if err := s.AppendToolCall(&ToolCallEvent{
		Tool: "git", Allowed: false, DenyReason: "not whitelisted",
	}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	calls, err := s.ListToolCalls()
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if calls[0].DenyReason != "" {
		t.Errorf("allowed call retained a deny reason: %q", calls[0].DenyReason)
	}
	if calls[1].Allowed || calls[1].DenyReason != "not whitelisted" {
		t.Errorf("denied call detail lost: %+v", calls[1])
	}
}

// TestMCPToolCallRoundTrip asserts an MCP-shaped ToolCallEvent (with the new
// Source/Server/Arguments fields) survives a write→read projection cycle.
func TestMCPToolCallRoundTrip(t *testing.T) {
	s := newTestStore(t)

	args := map[string]any{"title": "test", "body": "desc"}
	if err := s.AppendMCPToolCall("github", "create_issue", args, true, "", 42*time.Millisecond, "sess_123", "claude", "1.0"); err != nil {
		t.Fatalf("AppendMCPToolCall: %v", err)
	}

	calls, err := s.ListToolCalls()
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	got := calls[0]
	if got.Source != "mcp" || got.Server != "github" || got.Tool != "create_issue" {
		t.Fatalf("MCP fields not preserved: %+v", got)
	}
	if got.Allowed != true || got.DenyReason != "" || got.ExitCode != -1 {
		t.Fatalf("outcome fields not preserved: %+v", got)
	}
	if got.SessionID != "sess_123" || got.ClientName != "claude" || got.ClientVersion != "1.0" {
		t.Fatalf("session fields not preserved: %+v", got)
	}
	if len(got.Arguments) != 2 || got.Arguments["title"] != "test" || got.Arguments["body"] != "desc" {
		t.Fatalf("Arguments not preserved: %v", got.Arguments)
	}
}

// TestCLILegacyLineStillParses asserts a pre-MCP CLI-shaped JSONL line (with no
// Source/Server/Arguments fields) still parses and appears in ListToolCalls,
// with the new MCP fields zero-valued.
func TestCLILegacyLineStillParses(t *testing.T) {
	s := newTestStore(t)

	legacyLine := []byte(`{"type":"tool_call","timestamp":"2026-01-01T00:00:00Z","data":{"id":"call_abc","timestamp":"2026-01-01T00:00:00Z","tool":"git","subcommand":["status"],"allowed":true,"exit_code":0,"duration":"100ms"}}` + "\n")
	if err := os.WriteFile(s.path, legacyLine, 0600); err != nil {
		t.Fatalf("write legacy line: %v", err)
	}

	calls, err := s.ListToolCalls()
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	got := calls[0]
	if got.Tool != "git" || !got.Allowed || got.ExitCode != 0 {
		t.Fatalf("CLI fields not parsed: %+v", got)
	}
	if len(got.Subcommand) != 1 || got.Subcommand[0] != "status" {
		t.Fatalf("CLI subcommand not parsed: %+v", got)
	}
	if got.Source != "" || got.Server != "" || got.Arguments != nil {
		t.Fatalf("legacy line should have zero-valued MCP fields: Source=%q Server=%q Arguments=%v", got.Source, got.Server, got.Arguments)
	}
}

func TestUpdateStatusUnknownRequest(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpdateStatus("req_missing", StatusApproved, ""); err == nil {
		t.Fatal("expected error for unknown request")
	}
}

func TestEmptyLog(t *testing.T) {
	s := newTestStore(t)
	reqs, err := s.List("")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(reqs) != 0 {
		t.Fatalf("reqs = %d, want 0", len(reqs))
	}
	calls, err := s.ListToolCalls()
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %d, want 0", len(calls))
	}
}
