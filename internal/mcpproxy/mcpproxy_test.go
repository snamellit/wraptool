package mcpproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/executor"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// --- scrubContent (pure helper) ---

func TestScrubContentScrubsTextAndPassesNonText(t *testing.T) {
	env := map[string]string{"TOKEN": "supersecretvalue"}
	content := []mcp.Content{
		mcp.NewTextContent("the token is supersecretvalue here"),
		mcp.NewImageContent("base64data", "image/png"),
	}

	scrubContent(content, env)

	tc, ok := mcp.AsTextContent(content[0])
	if !ok {
		t.Fatalf("content[0] is not text after scrub: %T", content[0])
	}
	if strings.Contains(tc.Text, "supersecretvalue") {
		t.Errorf("env secret was not scrubbed: %q", tc.Text)
	}
	if !strings.Contains(tc.Text, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker, got %q", tc.Text)
	}

	// Non-text block passes through unchanged.
	if _, ok := mcp.AsImageContent(content[1]); !ok {
		t.Errorf("non-text content should pass through unchanged, got %T", content[1])
	}
}

func TestScrubContentScrubsCredentialPatterns(t *testing.T) {
	content := []mcp.Content{
		mcp.NewTextContent("Authorization: Bearer abcdef0123456789ABCDEF"),
	}
	scrubContent(content, nil)

	tc, _ := mcp.AsTextContent(content[0])
	if strings.Contains(tc.Text, "abcdef0123456789ABCDEF") {
		t.Errorf("bearer token not scrubbed: %q", tc.Text)
	}
}

// --- env isolation: the spawn env never carries wraptool's own environment ---

func TestChildEnvExcludesParentEnv(t *testing.T) {
	const leaky = "WRAPTOOL_MCPPROXY_LEAK_CHECK"
	t.Setenv(leaky, "leaked-parent-value")

	env := executor.ChildEnv("/usr/bin/some-mcp-server", map[string]string{"TOKEN": "abc"})

	for _, kv := range env {
		if strings.HasPrefix(kv, leaky+"=") {
			t.Fatalf("child env leaked parent variable %s: %q", leaky, kv)
		}
	}
	// Sanity: the configured var and HOME are present.
	if !containsKV(env, "TOKEN=abc") {
		t.Errorf("configured env var missing from child env: %v", env)
	}
	if !containsPrefix(env, "HOME=") {
		t.Errorf("HOME missing from child env: %v", env)
	}
}

// --- integration: connect -> list -> call -> scrub against an in-process server ---

// newStubServer builds an in-process MCP server exposing two tools. echo_secret
// echoes a fixed secret string in its result (to exercise scrubbing); forbidden
// exists only to prove default-deny filtering.
func newStubServer(secret string) *server.MCPServer {
	s := server.NewMCPServer("stub", "0.0.1", server.WithToolCapabilities(true))

	s.AddTool(
		mcp.NewTool("echo_secret", mcp.WithDescription("echoes a secret")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("here is the value: " + secret), nil
		},
	)
	s.AddTool(
		mcp.NewTool("forbidden", mcp.WithDescription("should not be exposed")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("nope"), nil
		},
	)
	return s
}

// wrapInProcess starts an in-process client against srv and wraps it in a
// mcpproxy.Client so the full list/call/scrub path can be exercised without a
// real stdio child process.
func wrapInProcess(t *testing.T, srv *server.MCPServer, resolvedEnv map[string]string) *Client {
	t.Helper()
	ctx := context.Background()

	inproc, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	if err := inproc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ClientInfo = mcp.Implementation{Name: "wraptool", Version: "test"}
	if _, err := inproc.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return &Client{
		serverName:  "stub",
		upstream:    inproc,
		resolvedEnv: resolvedEnv,
		timeout:     5 * time.Second,
	}
}

func TestSourceListCallScrubAndPolicy(t *testing.T) {
	const secret = "supersecretvalue123"
	c := wrapInProcess(t, newStubServer(secret), map[string]string{"TOKEN": secret})
	t.Cleanup(func() { _ = c.Close() })

	// Policy: allow echo_secret only. forbidden is default-denied.
	cfg := &config.MCPServerConfig{
		NamePrefix: "stub",
		Allow:      []config.MCPAllowRule{{Tool: "echo_secret"}},
	}
	policy := whitelist.NewMCPPolicy(map[string]*config.MCPServerConfig{"stub": cfg})

	src := NewSource("stub", cfg, policy, nil, "test")
	src.client = c // inject the in-process-backed client (white-box)

	tools, err := src.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}

	// Default-deny: only the allow-listed tool is registered, namespaced.
	if len(tools) != 1 {
		t.Fatalf("expected 1 registered tool, got %d: %+v", len(tools), tools)
	}
	if tools[0].Name != "stub_echo_secret" {
		t.Errorf("expected namespaced name stub_echo_secret, got %q", tools[0].Name)
	}
	for _, rt := range tools {
		if strings.Contains(rt.Name, "forbidden") {
			t.Errorf("denied tool was registered: %q", rt.Name)
		}
	}

	// Invoke through the handler and confirm the echoed secret is scrubbed.
	res, err := tools[0].Handler(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	tc, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("result content is not text: %T", res.Content[0])
	}
	if strings.Contains(tc.Text, secret) {
		t.Errorf("secret not scrubbed from upstream result: %q", tc.Text)
	}
	if !strings.Contains(tc.Text, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in result, got %q", tc.Text)
	}
}

func TestSourceSchemaPassthrough(t *testing.T) {
	// A stub server whose tool advertises a non-trivial input schema. wraptool
	// must expose that schema byte-for-byte via NewToolWithRawSchema, so the AI
	// sees the upstream's schema unchanged (transparent payload, §4.4).
	srv := server.NewMCPServer("stub", "0.0.1", server.WithToolCapabilities(true))
	upstreamTool := mcp.NewTool("schematool",
		mcp.WithDescription("has a schema"),
		mcp.WithString("path", mcp.Required(), mcp.Description("a path")),
		mcp.WithNumber("count", mcp.Description("a count")),
	)
	srv.AddTool(upstreamTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})

	c := wrapInProcess(t, srv, nil)
	t.Cleanup(func() { _ = c.Close() })

	cfg := &config.MCPServerConfig{
		NamePrefix: "stub",
		Allow:      []config.MCPAllowRule{{Tool: "schematool"}},
	}
	policy := whitelist.NewMCPPolicy(map[string]*config.MCPServerConfig{"stub": cfg})
	src := NewSource("stub", cfg, policy, nil, "test")
	src.client = c

	// Discover the upstream tool directly so we can compare its InputSchema
	// against what the registered wraptool tool carries.
	upstream, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var want []byte
	for _, ut := range upstream {
		if ut.Name == "schematool" {
			want, err = json.Marshal(ut.InputSchema)
			if err != nil {
				t.Fatalf("marshaling upstream schema: %v", err)
			}
		}
	}
	if want == nil {
		t.Fatal("upstream schematool not found")
	}

	tools, err := src.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	got := []byte(tools[0].Tool.RawInputSchema)
	if !bytes.Equal(got, want) {
		t.Errorf("registered raw schema does not equal json.Marshal(upstream.InputSchema):\n got: %s\nwant: %s", got, want)
	}
}

func TestHandlerDeniesUnallowedTool(t *testing.T) {
	c := wrapInProcess(t, newStubServer("x"), nil)
	t.Cleanup(func() { _ = c.Close() })

	cfg := &config.MCPServerConfig{
		NamePrefix: "stub",
		Allow:      []config.MCPAllowRule{{Tool: "echo_secret"}},
	}
	policy := whitelist.NewMCPPolicy(map[string]*config.MCPServerConfig{"stub": cfg})

	// A handler for a tool that is not allow-listed must refuse.
	h := &handler{client: c, serverName: "stub", toolName: "forbidden", policy: policy}
	res, err := h.Handle(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("Handle returned transport error: %v", err)
	}
	tc, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("denial result is not text: %T", res.Content[0])
	}
	if !strings.Contains(tc.Text, "DENIED") {
		t.Errorf("expected denial message, got %q", tc.Text)
	}
}

// TestIsolatedCommandEnv proves the credential-isolation guarantee at the point
// that actually spawns the child: the stdio command factory must set the child's
// env to *exactly* executor.ChildEnv output, with no inherited parent variables.
// (mcp-go's default transport does append(os.Environ(), env...), which would
// leak wraptool's own environment — this factory overrides that.)
func TestIsolatedCommandEnv(t *testing.T) {
	t.Setenv("WRAPTOOL_LEAK_CANARY", "must-not-reach-child")

	env := executor.ChildEnv("/usr/bin/git", map[string]string{"TOKEN": "s3cret"})
	cmd, err := isolatedCommand(context.Background(), "/usr/bin/git", env, []string{"status"})
	if err != nil {
		t.Fatalf("isolatedCommand: %v", err)
	}

	if !slices.Equal(cmd.Env, env) {
		t.Errorf("cmd.Env = %v, want exactly ChildEnv output %v", cmd.Env, env)
	}
	if containsPrefix(cmd.Env, "WRAPTOOL_LEAK_CANARY=") {
		t.Error("child inherited a parent env var (WRAPTOOL_LEAK_CANARY leaked)")
	}
}

func containsKV(env []string, kv string) bool {
	return slices.Contains(env, kv)
}

func containsPrefix(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// TestSourceSanitizesUpstreamToolNameForModelAPI pins the reported agy/Gemini
// failure: sigil publishes its tools as "sigil/lookup", and the raw
// "<name_prefix>_<upstream_tool>" join carried that slash into the registered
// name. Gemini rejects the whole GenerateContentRequest over one invalid
// function name ("tools[54].function_declarations[0].name: [FIELD_INVALID]"),
// so a single upstream naming convention disabled every wraptool tool in the
// session. The exposed name must be sanitized; the upstream call must still
// use the unmodified name.
func TestSourceSanitizesUpstreamToolNameForModelAPI(t *testing.T) {
	srv := server.NewMCPServer("stub", "0.0.1", server.WithToolCapabilities(true))
	called := ""
	srv.AddTool(
		mcp.NewTool("sigil/lookup", mcp.WithDescription("slash-namespaced upstream tool")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			called = req.Params.Name
			return mcp.NewToolResultText("ok"), nil
		},
	)
	c := wrapInProcess(t, srv, nil)
	t.Cleanup(func() { _ = c.Close() })

	cfg := &config.MCPServerConfig{
		NamePrefix: "sigil",
		Allow:      []config.MCPAllowRule{{Tool: "sigil/lookup"}},
	}
	policy := whitelist.NewMCPPolicy(map[string]*config.MCPServerConfig{"sigil": cfg})

	src := NewSource("sigil", cfg, policy, nil, "test")
	src.client = c // inject the in-process-backed client (white-box)

	tools, err := src.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 registered tool, got %d: %+v", len(tools), tools)
	}
	if tools[0].Name != "sigil_sigil_lookup" {
		t.Errorf("expected the sanitized name sigil_sigil_lookup, got %q", tools[0].Name)
	}

	// Dispatch is unaffected: the upstream is still called by its own name.
	if _, err := tools[0].Handler(context.Background(), mcp.CallToolRequest{}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if called != "sigil/lookup" {
		t.Errorf("upstream was called as %q, want the unmodified sigil/lookup", called)
	}
}
