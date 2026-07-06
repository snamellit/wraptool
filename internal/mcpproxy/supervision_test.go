package mcpproxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// fakeUpstream is a controllable upstreamConn: it can report a dead transport
// on demand (to exercise respawn) and records whether it was closed.
type fakeUpstream struct {
	mu      sync.Mutex
	tools   []mcp.Tool
	err     error // returned by ListTools/CallTool when non-nil
	result  *mcp.CallToolResult
	calls   int
	listed  int
	closedN int
}

func (f *fakeUpstream) ListTools(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed++
	if f.err != nil {
		return nil, f.err
	}
	return &mcp.ListToolsResult{Tools: f.tools}, nil
}

func (f *fakeUpstream) CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeUpstream) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedN++
	return nil
}

// newSupervisedClient builds a Client wired to a fake upstream and a fake
// connector, with no backoff sleeps and a fixed clock, for supervision tests.
func newSupervisedClient(initial upstreamConn, connect func(context.Context) (upstreamConn, error)) *Client {
	return &Client{
		serverName: "stub",
		timeout:    time.Second,
		now:        func() time.Time { return time.Unix(0, 0) },
		stderr:     &ringBuffer{max: 1024},
		upstream:   initial,
		connect:    connect,
	}
}

func TestCallToolRespawnsOnDeadTransport(t *testing.T) {
	dead := &fakeUpstream{err: transport.ErrTransportClosed}
	healthy := &fakeUpstream{result: mcp.NewToolResultText("ok")}

	var connects int
	c := newSupervisedClient(dead, func(context.Context) (upstreamConn, error) {
		connects++
		return healthy, nil
	})

	res, err := c.CallTool(context.Background(), "t", nil)
	if err != nil {
		t.Fatalf("CallTool after respawn: %v", err)
	}
	if res != healthy.result {
		t.Errorf("result did not come from the respawned child")
	}
	if connects != 1 {
		t.Errorf("expected exactly one respawn, got %d", connects)
	}
	if dead.closedN != 1 {
		t.Errorf("dead child should have been closed once, got %d", dead.closedN)
	}
	if c.currentUpstream() != healthy {
		t.Errorf("live upstream should be the respawned child")
	}
}

func TestCallToolCrashloopCapStops(t *testing.T) {
	dead := &fakeUpstream{err: transport.ErrTransportClosed}

	var connects int
	c := newSupervisedClient(dead, func(context.Context) (upstreamConn, error) {
		connects++
		// Every respawn is itself already dead, so the next call fails again.
		return &fakeUpstream{err: transport.ErrTransportClosed}, nil
	})

	// Drive calls until the cap is hit; each dead call triggers one respawn
	// attempt until maxRestarts within the window is reached.
	for i := range maxRestarts + 3 {
		_, err := c.CallTool(context.Background(), "t", nil)
		if !isDead(err) {
			t.Fatalf("call %d: expected a dead-transport error, got %v", i, err)
		}
	}
	if connects != maxRestarts {
		t.Errorf("expected respawn to stop at the cap (%d), got %d attempts", maxRestarts, connects)
	}
}

func TestCrashloopCapResetsAfterWindow(t *testing.T) {
	dead := &fakeUpstream{err: transport.ErrTransportClosed}
	var connects int
	now := time.Unix(0, 0)
	c := newSupervisedClient(dead, func(context.Context) (upstreamConn, error) {
		connects++
		return &fakeUpstream{err: transport.ErrTransportClosed}, nil
	})
	c.now = func() time.Time { return now }

	for range maxRestarts + 2 {
		_, _ = c.CallTool(context.Background(), "t", nil)
	}
	if connects != maxRestarts {
		t.Fatalf("pre-window: expected %d respawns, got %d", maxRestarts, connects)
	}

	// Advance past the window: the counter resets and respawns resume.
	now = now.Add(restartWindow + time.Second)
	_, _ = c.CallTool(context.Background(), "t", nil)
	if connects != maxRestarts+1 {
		t.Errorf("after window: expected respawn to resume (%d), got %d", maxRestarts+1, connects)
	}
}

func TestListToolsRespawnsButDoesNotFireCallback(t *testing.T) {
	dead := &fakeUpstream{err: transport.ErrTransportClosed}
	healthy := &fakeUpstream{tools: []mcp.Tool{{Name: "a"}, {Name: "b"}}}

	var fired int
	c := newSupervisedClient(dead, func(context.Context) (upstreamConn, error) {
		return healthy, nil
	})
	c.setOnToolsChanged(func() { fired++ })

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools after respawn: %v", err)
	}
	if len(tools) != 2 {
		t.Errorf("expected the fresh child's 2 tools, got %d", len(tools))
	}
	// ListTools must not re-trigger registration (it is already inside one),
	// or reregister would recurse.
	if fired != 0 {
		t.Errorf("ListTools should not fire onToolsChanged, fired %d times", fired)
	}
}

func TestCallToolFiresCallbackAfterRespawn(t *testing.T) {
	dead := &fakeUpstream{err: transport.ErrTransportClosed}
	healthy := &fakeUpstream{result: mcp.NewToolResultText("ok")}

	var fired int
	c := newSupervisedClient(dead, func(context.Context) (upstreamConn, error) {
		return healthy, nil
	})
	c.setOnToolsChanged(func() { fired++ })

	if _, err := c.CallTool(context.Background(), "t", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if fired != 1 {
		t.Errorf("a successful respawn should fire onToolsChanged once, got %d", fired)
	}
}

func TestConcurrentCallsRespawnOnce(t *testing.T) {
	dead := &fakeUpstream{err: transport.ErrTransportClosed}
	healthy := &fakeUpstream{result: mcp.NewToolResultText("ok")}

	var mu sync.Mutex
	var connects int
	c := newSupervisedClient(dead, func(context.Context) (upstreamConn, error) {
		mu.Lock()
		connects++
		mu.Unlock()
		return healthy, nil
	})

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, _ = c.CallTool(context.Background(), "t", nil)
		})
	}
	wg.Wait()

	// The first goroutine to trip on the dead child respawns it; the rest see
	// the already-replaced upstream and must not respawn again.
	if connects != 1 {
		t.Errorf("expected a single respawn under concurrency, got %d", connects)
	}
	if dead.closedN != 1 {
		t.Errorf("dead child should be closed exactly once, got %d", dead.closedN)
	}
}

func TestIsDead(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{transport.ErrTransportClosed, true},
		{errors.New("failed to write request: broken pipe"), true},
		{errors.New("some other error"), false},
	}
	for _, tc := range cases {
		if got := isDead(tc.err); got != tc.want {
			t.Errorf("isDead(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
