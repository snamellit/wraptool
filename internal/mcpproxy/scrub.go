package mcpproxy

import (
	"log"

	"github.com/pti/wraptool/internal/executor"

	"github.com/mark3labs/mcp-go/mcp"
)

// scrubContent scrubs credentials and the server's configured env-var values
// from the text-bearing blocks of an MCP tool result, in place. It reuses the
// same executor.Scrub helper as the CLI path, so an upstream result gets the
// same secret-hiding treatment as CLI stdout/stderr.
//
// Non-text blocks (images, audio, embedded resources) cannot be scrubbed
// meaningfully, so they pass through unchanged; their presence is logged for
// the audit trail. A strict text-only mode is a deliberate future option.
func scrubContent(content []mcp.Content, env map[string]string) {
	for i, c := range content {
		if tc, ok := mcp.AsTextContent(c); ok {
			tc.Text = executor.Scrub(tc.Text, env)
			content[i] = *tc
			continue
		}
		log.Printf("mcpproxy: passing through non-text content block (%T) unscrubbed", c)
	}
}
