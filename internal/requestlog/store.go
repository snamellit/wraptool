package requestlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Store is an append-only event log. Requests and tool calls are recorded as
// heterogeneous events in a single JSONL stream; current state is projected by
// folding the stream. The log is never rewritten in place.
type Store struct {
	mu   sync.Mutex
	path string
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

// appendEvent marshals payload into an Event envelope and appends one JSONL
// line. Callers hold no lock; appendEvent takes it.
func (s *Store) appendEvent(t EventType, ts time.Time, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendEventLocked(t, ts, payload)
}

func (s *Store) appendEventLocked(t EventType, ts time.Time, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling %s payload: %w", t, err)
	}
	evt := Event{Type: t, Timestamp: ts, Data: data}
	line, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshaling event: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("creating event log directory: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("opening event log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("writing event: %w", err)
	}
	return nil
}

// Append records a new capability request as a capability_requested event.
// It stamps req.ID/Timestamp/Status in place so the caller can read them back.
func (s *Store) Append(req *CapabilityRequest) error {
	req.ID = "req_" + uuid.New().String()[:8]
	req.Timestamp = time.Now().UTC()
	req.Status = StatusPending
	return s.appendEvent(EventCapabilityRequested, req.Timestamp, req)
}

// AppendToolCall records a tool invocation as a tool_call event. It stamps and
// normalizes evt in place (ID, Timestamp, and the invariant below).
//
// It enforces the event's core invariant at this single boundary: Allowed is the
// authoritative policy decision, and DenyReason is only its explanation. An
// allowed call therefore never carries a deny reason, so downstream consumers can
// classify purely on Allowed without having to reconcile the two fields. This
// makes the contradictory state (Allowed && DenyReason != "") unrepresentable in
// the log rather than trusting every call site to keep the pair consistent. The
// converse is not implied — a denial may legitimately carry no reason.
func (s *Store) AppendToolCall(evt *ToolCallEvent) error {
	if evt.Allowed {
		evt.DenyReason = ""
	}
	evt.ID = "call_" + uuid.New().String()[:8]
	evt.Timestamp = time.Now().UTC()
	return s.appendEvent(EventToolCall, evt.Timestamp, evt)
}

// AppendMCPToolCall records an upstream MCP tool invocation as a tool_call
// event with Source="mcp". The caller extracts sessionID/clientName/
// clientVersion from its MCP request context and passes them in, so this
// package stays decoupled from the MCP transport. MCP calls have no Unix exit
// code, so ExitCode is recorded as -1 (N/A). denyReason is empty when allowed.
func (s *Store) AppendMCPToolCall(serverName, toolName string, arguments map[string]any, allowed bool, denyReason string, duration time.Duration, sessionID, clientName, clientVersion string) error {
	evt := &ToolCallEvent{
		SessionID:     sessionID,
		ClientName:    clientName,
		ClientVersion: clientVersion,
		Tool:          toolName,
		Allowed:       allowed,
		DenyReason:    denyReason,
		ExitCode:      -1,
		Duration:      duration.Round(time.Millisecond).String(),
		Source:        "mcp",
		Server:        serverName,
		Arguments:     arguments,
	}
	return s.AppendToolCall(evt)
}

// UpdateStatus records an approval or denial as a new event. The request must
// already exist. The log is not rewritten; List/Get fold the decision in.
func (s *Store) UpdateStatus(id string, status Status, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	reqs, _, err := s.foldRequestsLocked()
	if err != nil {
		return err
	}
	if _, ok := reqs[id]; !ok {
		return fmt.Errorf("request %q not found", id)
	}

	var t EventType
	switch status {
	case StatusApproved:
		t = EventCapabilityApproved
	case StatusDenied:
		t = EventCapabilityDenied
	case StatusPending:
		// pending is the initial state, never a recordable decision event
		return fmt.Errorf("cannot record status %q as an event", status)
	default:
		return fmt.Errorf("cannot record status %q as an event", status)
	}
	return s.appendEventLocked(t, time.Now().UTC(), decision{RequestID: id, Reason: reason})
}

// List returns all requests (projected from the event log), optionally filtered
// by status. Order matches first-seen (request) order.
func (s *Store) List(status Status) ([]CapabilityRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	byID, order, err := s.foldRequestsLocked()
	if err != nil {
		return nil, err
	}
	out := make([]CapabilityRequest, 0, len(order))
	for _, id := range order {
		req := byID[id]
		if status == "" || req.Status == status {
			out = append(out, *req)
		}
	}
	return out, nil
}

// Get returns the request with the given ID, or an error if not found.
func (s *Store) Get(id string) (*CapabilityRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	byID, _, err := s.foldRequestsLocked()
	if err != nil {
		return nil, err
	}
	req, ok := byID[id]
	if !ok {
		return nil, fmt.Errorf("request %q not found", id)
	}
	return req, nil
}

// ListToolCalls returns every recorded tool call, oldest first.
func (s *Store) ListToolCalls() ([]ToolCallEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var calls []ToolCallEvent
	err := s.eachEventLocked(func(e Event) {
		if e.Type != EventToolCall {
			return
		}
		var tc ToolCallEvent
		if json.Unmarshal(e.Data, &tc) == nil {
			calls = append(calls, tc)
		}
	})
	return calls, err
}

// foldRequestsLocked replays the event log into current request state, keyed by
// ID, plus a slice of IDs in first-seen order for stable listing.
func (s *Store) foldRequestsLocked() (map[string]*CapabilityRequest, []string, error) {
	byID := make(map[string]*CapabilityRequest)
	var order []string
	err := s.eachEventLocked(func(e Event) {
		switch e.Type {
		case EventCapabilityRequested:
			var req CapabilityRequest
			if json.Unmarshal(e.Data, &req) != nil {
				return
			}
			if req.Status == "" {
				req.Status = StatusPending
			}
			if _, seen := byID[req.ID]; !seen {
				order = append(order, req.ID)
			}
			byID[req.ID] = &req
		case EventCapabilityApproved, EventCapabilityDenied:
			var d decision
			if json.Unmarshal(e.Data, &d) != nil {
				return
			}
			req, ok := byID[d.RequestID]
			if !ok {
				return
			}
			if e.Type == EventCapabilityApproved {
				req.Status = StatusApproved
			} else {
				req.Status = StatusDenied
				if d.Reason != "" {
					req.DenyReason = d.Reason
				}
			}
		case EventToolCall:
			// audit-only events; not part of capability-request state, so they
			// are ignored when folding requests.
		}
	})
	return byID, order, err
}

// eachEventLocked scans the log and calls fn for each event. Missing file is
// not an error. A malformed line, or a legacy pre-event-log line, is adapted
// or skipped rather than aborting the scan.
func (s *Store) eachEventLocked(fn func(Event)) error {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("opening event log: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var e Event
		if err := json.Unmarshal(line, &e); err != nil || e.Type == "" {
			if adapted, ok := adaptLegacy(line); ok {
				fn(adapted)
			}
			continue
		}
		fn(e)
	}
	return scanner.Err()
}

// adaptLegacy upgrades a pre-event-log JSONL line (a bare CapabilityRequest
// written before the unified log) into a capability_requested Event so old
// logs keep working. Lines it can't interpret are dropped.
func adaptLegacy(line []byte) (Event, bool) {
	var req CapabilityRequest
	if err := json.Unmarshal(line, &req); err != nil || req.ID == "" || req.Status == "" {
		return Event{}, false
	}
	return Event{Type: EventCapabilityRequested, Timestamp: req.Timestamp, Data: line}, true
}
