package requestlog

import (
	"path/filepath"
	"testing"
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
