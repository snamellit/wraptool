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

// Store manages the capability request log.
type Store struct {
	mu   sync.Mutex
	path string
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

// Append adds a new capability request to the log.
func (s *Store) Append(req *CapabilityRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	req.ID = "req_" + uuid.New().String()[:8]
	req.Timestamp = time.Now().UTC()
	req.Status = StatusPending

	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("creating request log directory: %w", err)
	}

	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("opening request log: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("writing request: %w", err)
	}

	return nil
}

// List returns all requests, optionally filtered by status.
func (s *Store) List(status Status) ([]CapabilityRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening request log: %w", err)
	}
	defer f.Close()

	var requests []CapabilityRequest
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var req CapabilityRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue // skip malformed lines
		}
		if status == "" || req.Status == status {
			requests = append(requests, req)
		}
	}

	return requests, scanner.Err()
}

// UpdateStatus changes the status of a request by ID.
// It rewrites the entire file (the log is small enough for this).
func (s *Store) UpdateStatus(id string, status Status, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	requests, err := s.readAll()
	if err != nil {
		return err
	}

	found := false
	for i := range requests {
		if requests[i].ID == id {
			requests[i].Status = status
			if reason != "" {
				requests[i].DenyReason = reason
			}
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("request %q not found", id)
	}

	return s.writeAll(requests)
}

func (s *Store) readAll() ([]CapabilityRequest, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var requests []CapabilityRequest
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var req CapabilityRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		requests = append(requests, req)
	}
	return requests, scanner.Err()
}

func (s *Store) writeAll(requests []CapabilityRequest) error {
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}

	for _, req := range requests {
		data, err := json.Marshal(req)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}

	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	return os.Rename(tmp, s.path)
}
