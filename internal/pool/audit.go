package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AuditEvent is one line of audit.jsonl: an append-only stream of pool
// mutation events (bypass, activate, rollback, …), deliberately mirroring
// internal/requestlog's append-only event-log style.
type AuditEvent struct {
	Type      string         `json:"type"`
	Timestamp string         `json:"timestamp"` // UTC RFC3339
	Fields    map[string]any `json:"fields,omitempty"`
}

// AppendAudit appends one event to the audit log at path: marshal, then a
// single O_APPEND write of one JSON line, creating the file and its parent
// directory on first use. Timestamp is stamped as UTC RFC3339 at call time.
func AppendAudit(path, eventType string, fields map[string]any) error {
	evt := AuditEvent{
		Type:      eventType,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Fields:    fields,
	}
	line, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshaling audit event: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating audit log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening audit log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("writing audit event: %w", err)
	}
	return nil
}
