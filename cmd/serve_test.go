package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/config"

	"github.com/mark3labs/mcp-go/server"
)

// newTestHandler builds the combined SSE + Streamable HTTP handler over a bare
// MCPServer, with Bearer auth backed by a token file containing "secret".
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	s := server.NewMCPServer("wraptool-test", "0.0.0")
	cfg := &config.Config{}
	cfg.Server.AuthTokenFile = tokenFile
	h, err := newMCPHTTPHandler(s, cfg)
	if err != nil {
		t.Fatalf("newMCPHTTPHandler: %v", err)
	}
	return h
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
	`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

// TestStreamableInitializeWithAuth is the regression test for the Antigravity
// "Method Not Allowed" failure: a Streamable HTTP client POSTs initialize to
// /mcp and must get a 200 JSON-RPC result, not the 405 the SSE-only /sse gave.
func TestStreamableInitializeWithAuth(t *testing.T) {
	ts := httptest.NewServer(newTestHandler(t))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(initializeBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /mcp initialize status = %d, want 200; body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "protocolVersion") {
		t.Errorf("initialize result missing protocolVersion; body: %s", body)
	}
}

// TestStreamableInitializeWithoutAuth confirms the shared auth wrapper still
// guards the /mcp endpoint.
func TestStreamableInitializeWithoutAuth(t *testing.T) {
	ts := httptest.NewServer(newTestHandler(t))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(initializeBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST /mcp status = %d, want 401", resp.StatusCode)
	}
}

// TestSSEEndpointStillServed guards against regressing the legacy SSE clients
// (claude, cursor): GET /sse must open an event stream (200 text/event-stream),
// not 404/405, now that /mcp shares the mux.
func TestSSEEndpointStillServed(t *testing.T) {
	ts := httptest.NewServer(newTestHandler(t))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/sse", nil)
	req.Header.Set("Authorization", "Bearer secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sse status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("GET /sse Content-Type = %q, want text/event-stream", ct)
	}
	// The stream is long-lived; cancel via context (deferred) rather than reading.
}
