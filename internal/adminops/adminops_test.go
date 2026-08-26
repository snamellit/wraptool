package adminops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/requestlog"
)

// newTestStore builds a Store backed by a temp JSONL log and returns a
// sibling config path (not yet written — each test writes its own body).
func newTestStore(t *testing.T) (*requestlog.Store, string) {
	t.Helper()
	dir := t.TempDir()
	return requestlog.NewStore(filepath.Join(dir, "requests.jsonl")), filepath.Join(dir, "config.yaml")
}

// writeConfig writes a minimal config with one tool ("git") whose binary
// exists and is executable, so config.ApplyRequest's post-edit Validate
// passes on any host.
func writeConfig(t *testing.T, cfgPath, extraToolYAML string) {
	t.Helper()
	dir := filepath.Dir(cfgPath)
	binPath := filepath.Join(dir, "git")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "" +
		"mcp:\n" +
		"  transport: stdio\n" +
		"tools:\n" +
		"  git:\n" +
		"    binary: " + binPath + "\n" +
		extraToolYAML
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendRequest(t *testing.T, store *requestlog.Store, tool string, sub, flags []string, flagTypes map[string]string) string {
	t.Helper()
	req := &requestlog.CapabilityRequest{
		Tool: tool, Subcommand: sub, Flags: flags, FlagTypes: flagTypes,
		Rationale: "test",
	}
	if err := store.Append(req); err != nil {
		t.Fatalf("Append: %v", err)
	}
	return req.ID
}

func TestApproveNewRule(t *testing.T) {
	store, cfgPath := newTestStore(t)
	writeConfig(t, cfgPath, "")
	id := appendRequest(t, store, "git", []string{"status"}, []string{"--short"}, nil)

	result, err := Approve(store, cfgPath, id, true, nil)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if result.Outcome != OutcomeNewRule {
		t.Errorf("Outcome = %v, want OutcomeNewRule", result.Outcome)
	}
	if result.ApplyErr != nil {
		t.Errorf("ApplyErr = %v, want nil", result.ApplyErr)
	}
	if len(result.AddedFlags) != 1 || result.AddedFlags[0] != "--short" {
		t.Errorf("AddedFlags = %v, want [--short]", result.AddedFlags)
	}
	if result.Snippet == "" {
		t.Error("Snippet is empty, want the new rule's YAML")
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != requestlog.StatusApproved {
		t.Errorf("status = %q, want approved", got.Status)
	}
}

func TestApproveExtended(t *testing.T) {
	store, cfgPath := newTestStore(t)
	writeConfig(t, cfgPath, "    allow:\n      - subcommand: [status]\n        flags: [--short]\n")
	id := appendRequest(t, store, "git", []string{"status"}, []string{"--porcelain"}, nil)

	result, err := Approve(store, cfgPath, id, true, nil)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if result.Outcome != OutcomeExtended {
		t.Errorf("Outcome = %v, want OutcomeExtended", result.Outcome)
	}
	if len(result.AddedFlags) != 1 || result.AddedFlags[0] != "--porcelain" {
		t.Errorf("AddedFlags = %v, want [--porcelain]", result.AddedFlags)
	}
}

func TestApproveNoChange(t *testing.T) {
	store, cfgPath := newTestStore(t)
	writeConfig(t, cfgPath, "    allow:\n      - subcommand: [status]\n        flags: [--short]\n")
	id := appendRequest(t, store, "git", []string{"status"}, []string{"--short"}, nil)

	result, err := Approve(store, cfgPath, id, true, nil)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if result.Outcome != OutcomeNoChange {
		t.Errorf("Outcome = %v, want OutcomeNoChange", result.Outcome)
	}
	if len(result.AddedFlags) != 0 {
		t.Errorf("AddedFlags = %v, want none", result.AddedFlags)
	}
}

func TestApproveNotApplied(t *testing.T) {
	store, cfgPath := newTestStore(t)
	writeConfig(t, cfgPath, "")
	id := appendRequest(t, store, "git", []string{"status"}, nil, nil)

	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	result, err := Approve(store, cfgPath, id, false, nil)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if result.Outcome != OutcomeNotApplied {
		t.Errorf("Outcome = %v, want OutcomeNotApplied", result.Outcome)
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("apply=false must not touch the config file")
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != requestlog.StatusApproved {
		t.Errorf("status = %q, want approved", got.Status)
	}
}

// TestApprovePartialFailure exercises the "status flipped to approved, but
// the config write failed" case: an unknown tool causes config.ApplyRequest
// to fail its docToolNode lookup.
func TestApprovePartialFailure(t *testing.T) {
	store, cfgPath := newTestStore(t)
	writeConfig(t, cfgPath, "")
	id := appendRequest(t, store, "kubectl", []string{"get", "pods"}, nil, nil)

	result, err := Approve(store, cfgPath, id, true, nil)
	if err != nil {
		t.Fatalf("Approve returned an error (want ApplyErr instead): %v", err)
	}
	if result.ApplyErr == nil {
		t.Fatal("ApplyErr = nil, want a config-apply error for an unconfigured tool")
	}
	if !strings.Contains(result.ApplyErr.Error(), "kubectl") {
		t.Errorf("ApplyErr = %v, want it to name the tool", result.ApplyErr)
	}

	// Status still flips to approved even though the apply failed.
	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != requestlog.StatusApproved {
		t.Errorf("status = %q, want approved despite apply failure", got.Status)
	}
}

func TestApproveUnknownID(t *testing.T) {
	store, cfgPath := newTestStore(t)
	writeConfig(t, cfgPath, "")

	if _, err := Approve(store, cfgPath, "req_missing", true, nil); err == nil {
		t.Error("Approve with an unknown ID should return an error")
	}
}

func TestApproveFlagTypes(t *testing.T) {
	store, cfgPath := newTestStore(t)
	writeConfig(t, cfgPath, "")
	id := appendRequest(t, store, "git", []string{"status"}, []string{"--reviewer"}, map[string]string{"--reviewer": "boolean"})

	// The caller's merged flagTypes overrides the requester's proposal.
	result, err := Approve(store, cfgPath, id, true, map[string]string{"--reviewer": "string"})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if result.Outcome != OutcomeNewRule {
		t.Fatalf("Outcome = %v, want OutcomeNewRule", result.Outcome)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "type: string") {
		t.Errorf("config missing overridden flag type; got:\n%s", data)
	}
	if strings.Contains(string(data), "type: boolean") {
		t.Errorf("proposed boolean type leaked despite override; got:\n%s", data)
	}
}

func TestDenyWithReason(t *testing.T) {
	store, _ := newTestStore(t)
	id := appendRequest(t, store, "git", []string{"push"}, []string{"--force"}, nil)

	if err := Deny(store, id, "too risky"); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != requestlog.StatusDenied {
		t.Errorf("status = %q, want denied", got.Status)
	}
	if got.DenyReason != "too risky" {
		t.Errorf("DenyReason = %q, want %q", got.DenyReason, "too risky")
	}
}

func TestDenyWithoutReason(t *testing.T) {
	store, _ := newTestStore(t)
	id := appendRequest(t, store, "git", []string{"push"}, nil, nil)

	if err := Deny(store, id, ""); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != requestlog.StatusDenied {
		t.Errorf("status = %q, want denied", got.Status)
	}
	if got.DenyReason != "" {
		t.Errorf("DenyReason = %q, want empty", got.DenyReason)
	}
}

func TestDenyUnknownID(t *testing.T) {
	store, _ := newTestStore(t)
	if err := Deny(store, "req_missing", "no"); err == nil {
		t.Error("Deny with an unknown ID should return an error")
	}
}
