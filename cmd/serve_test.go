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
	cfg.MCP.AuthTokenFile = tokenFile
	h, err := newMCPHTTPHandler(s, cfg)
	if err != nil {
		t.Fatalf("newMCPHTTPHandler: %v", err)
	}
	return h
}

// newTestSSEHandler builds what serveSSE actually serves: the combined handler
// behind the browser guard, for a listener bound to 127.0.0.1:8717.
func newTestSSEHandler(t *testing.T) http.Handler {
	t.Helper()
	return browserGuard(newTestHandler(t), "127.0.0.1:8717")
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
// (claude, opencode): GET /sse must open an event stream (200 text/event-stream),
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

// TestBrowserGuard covers the header rules on their own: a hostile page cannot
// forge Origin, and a rebinding attack cannot forge Host.
func TestBrowserGuard(t *testing.T) {
	cases := []struct {
		name, listen, origin, host string
		want                       int
	}{
		{"no origin, loopback host", "127.0.0.1:8717", "", "127.0.0.1:8717", http.StatusOK},
		{"loopback origin", "127.0.0.1:8717", "http://localhost:3000", "127.0.0.1:8717", http.StatusOK},
		{"ipv6 loopback origin", "127.0.0.1:8717", "http://[::1]:8717", "localhost:8717", http.StatusOK},
		{"hostile origin", "127.0.0.1:8717", "https://evil.example", "127.0.0.1:8717", http.StatusForbidden},
		{"null origin", "127.0.0.1:8717", "null", "127.0.0.1:8717", http.StatusForbidden},
		{"rebound host", "127.0.0.1:8717", "", "evil.example:8717", http.StatusForbidden},
		// A wildcard bind is reached over the bridge gateway, whose address the
		// server does not know, so Host cannot be checked there.
		{"wildcard bind accepts any host", "0.0.0.0:8717", "", "172.17.0.1:8717", http.StatusOK},
		{"wildcard bind still rejects origins", "0.0.0.0:8717", "https://evil.example", "172.17.0.1:8717", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := browserGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}), tc.listen)

			req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/sse", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// TestBrowserGuardSurvivesLeakedToken is the drive-by-execution regression: a
// page that somehow holds the token still cannot reach the tools, and the
// guard covers /mcp as well as /sse.
func TestBrowserGuardSurvivesLeakedToken(t *testing.T) {
	ts := httptest.NewServer(newTestSSEHandler(t))
	defer ts.Close()

	// /message is the endpoint the attack actually posts tool calls to: it
	// checks only method and session id, so the guard has to cover it.
	for _, path := range []string{"/mcp", "/sse", "/message"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(initializeBody))
		req.Header.Set("Content-Type", "text/plain") // a CORS simple request
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Authorization", "Bearer secret")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s from a hostile origin = %d, want 403", path, resp.StatusCode)
		}
	}

	// A loopback origin with the right token is still served.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(initializeBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Origin", "http://127.0.0.1:5173")
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("POST /mcp from a loopback origin = %d, want 200; body: %s", resp.StatusCode, body)
	}
}
