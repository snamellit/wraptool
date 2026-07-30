package pool

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendAuditWritesJSONLLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	if err := AppendAudit(path, "rollback", map[string]any{"harness": "claude", "to": "2.1.220"}); err != nil {
		t.Fatalf("AppendAudit (1): %v", err)
	}
	if err := AppendAudit(path, "bypass", map[string]any{"harness": "claude", "reason": "CVE-2026-0001"}); err != nil {
		t.Fatalf("AppendAudit (2): %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var events []AuditEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var evt AuditEvent
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
			t.Fatalf("unmarshaling line %q: %v", scanner.Text(), err)
		}
		events = append(events, evt)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != "rollback" {
		t.Errorf("event[0].Type = %q, want %q", events[0].Type, "rollback")
	}
	if events[1].Type != "bypass" {
		t.Errorf("event[1].Type = %q, want %q", events[1].Type, "bypass")
	}
	for i, evt := range events {
		if evt.Timestamp == "" {
			t.Errorf("event[%d] missing timestamp", i)
		}
		if _, err := time.Parse(time.RFC3339, evt.Timestamp); err != nil {
			t.Errorf("event[%d].Timestamp = %q is not RFC3339: %v", i, evt.Timestamp, err)
		}
	}
}

func TestAppendAuditCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "audit.jsonl")

	if err := AppendAudit(path, "activate", nil); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected audit file to exist: %v", err)
	}
}

// TestAppendAuditTimestampHasSubsecondPrecision covers NIT 11: audit
// timestamps use RFC3339Nano (matching internal/requestlog's granularity),
// not the coarser plain-RFC3339 second precision used elsewhere in this
// package, so two events appended in quick succession stay orderable by
// timestamp string alone.
func TestAppendAuditTimestampHasSubsecondPrecision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	if err := AppendAudit(path, "activate", nil); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var evt AuditEvent
	if err := json.Unmarshal(data[:len(data)-1], &evt); err != nil { // strip trailing "\n"
		t.Fatalf("unmarshaling event: %v", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, evt.Timestamp)
	if err != nil {
		t.Fatalf("Timestamp = %q is not RFC3339Nano: %v", evt.Timestamp, err)
	}
	if parsed.Location() != time.UTC {
		t.Errorf("Timestamp = %q, want a UTC (Z-suffixed) timestamp", evt.Timestamp)
	}
}
