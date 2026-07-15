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

	// MCP source fields. Additive and omitempty so pre-MCP CLI event lines
	// still unmarshal and existing projections keep working. Readers treat an
	// empty Source as "cli".
	Source    string         `json:"source,omitempty"`    // "cli" | "mcp"; empty => cli
	Server    string         `json:"server,omitempty"`    // mcp_servers.<name>, only set when Source=="mcp"
	Arguments map[string]any `json:"arguments,omitempty"` // raw MCP call arguments; empty for CLI
}

// CapabilityRequest represents a request to add a new capability to the whitelist.
type CapabilityRequest struct {
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	Status     Status    `json:"status"`
	Tool       string    `json:"tool"`
	Subcommand []string  `json:"subcommand"`
	Flags      []string  `json:"flags,omitempty"`
	// FlagTypes carries the requester's inferred value type for a flag (keyed by
	// flag name → "string"|"integer"|"boolean"). The requester (a developer, or
	// an AI that read the tool's --help) types the flag far more accurately than
	// the generic introspection parser can; on approval these become
	// flag_constraints in the config, which override the introspected type. Only
	// flags also present in Flags are applied. See design/flag-type-inference.md.
	FlagTypes     map[string]string `json:"flag_types,omitempty"`
	Rationale     string            `json:"rationale"`
	Enables       string            `json:"enables,omitempty"`
	DeniedCommand string            `json:"denied_command,omitempty"`
	SessionID     string            `json:"session_id,omitempty"`
	DenyReason    string            `json:"deny_reason,omitempty"`
}
