package requestlog

import (
	"encoding/json"
	"time"
)

type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusDenied   Status = "denied"
)

// EventType discriminates the payload carried by an Event.
type EventType string

const (
	// EventCapabilityRequested carries a CapabilityRequest (Status pending).
	EventCapabilityRequested EventType = "capability_requested"
	// EventCapabilityApproved / EventCapabilityDenied carry a decision that
	// references a prior request by ID. A request's Status is a projection
	// over these events.
	EventCapabilityApproved EventType = "capability_approved"
	EventCapabilityDenied   EventType = "capability_denied"
	// EventToolCall carries a ToolCallEvent.
	EventToolCall EventType = "tool_call"
)

// Event is the append-only log envelope. The log is a single JSONL stream of
// heterogeneous events discriminated by Type; state (a request's status, the
// list of tool calls) is a projection folded over the stream. The log is never
// rewritten — a status change is a new event, not an in-place mutation.
type Event struct {
	Type      EventType       `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// decision is the payload for approved/denied events.
type decision struct {
	RequestID string `json:"request_id"`
	Reason    string `json:"reason,omitempty"`
}

// ToolCallEvent records every tool invocation for audit purposes.
type ToolCallEvent struct {
	ID             string            `json:"id"`
	Timestamp      time.Time         `json:"timestamp"`
	SessionID      string            `json:"session_id,omitempty"`
	ClientName     string            `json:"client_name,omitempty"`
	ClientVersion  string            `json:"client_version,omitempty"`
	Tool           string            `json:"tool"`
	Subcommand     []string          `json:"subcommand,omitempty"`
	Allowed        bool              `json:"allowed"`
	DenyReason     string            `json:"deny_reason,omitempty"`
	Flags          map[string]string `json:"flags,omitempty"`
	PositionalArgs []string          `json:"positional_args,omitempty"`
	ExitCode       int               `json:"exit_code"`
	Truncated      bool              `json:"truncated,omitempty"`
	Duration       string            `json:"duration,omitempty"`
}

// CapabilityRequest represents a request to add a new capability to the whitelist.
type CapabilityRequest struct {
	ID            string    `json:"id"`
	Timestamp     time.Time `json:"timestamp"`
	Status        Status    `json:"status"`
	Tool          string    `json:"tool"`
	Subcommand    []string  `json:"subcommand"`
	Flags         []string  `json:"flags,omitempty"`
	Rationale     string    `json:"rationale"`
	Enables       string    `json:"enables,omitempty"`
	DeniedCommand string    `json:"denied_command,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	DenyReason    string    `json:"deny_reason,omitempty"`
}
