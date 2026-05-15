package requestlog

import "time"

type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusDenied   Status = "denied"
)

// CapabilityRequest represents a request to add a new capability to the whitelist.
type CapabilityRequest struct {
	ID             string   `json:"id"`
	Timestamp      time.Time `json:"timestamp"`
	Status         Status   `json:"status"`
	Tool           string   `json:"tool"`
	Subcommand     []string `json:"subcommand"`
	Flags          []string `json:"flags,omitempty"`
	Rationale      string   `json:"rationale"`
	Enables        string   `json:"enables,omitempty"`
	DeniedCommand  string   `json:"denied_command,omitempty"`
	SessionID      string   `json:"session_id,omitempty"`
	DenyReason     string   `json:"deny_reason,omitempty"`
}
