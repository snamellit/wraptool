#!/bin/sh
set -e

BASE="http://127.0.0.1:8717"
SSE_OUTPUT="/tmp/sse_stream"

echo "=== Inside Guix container ==="
echo

echo "1. Verify isolation -- no git, no credentials:"
echo -n "   git:      "; which git 2>/dev/null && echo "FOUND (bad!)" || echo "not found (good)"
echo -n "   ssh keys: "; ls ~/.ssh/id_* 2>/dev/null && echo "FOUND (bad!)" || echo "not found (good)"
echo "   HOME:     $HOME"

echo
echo "2. Connect to wraptool SSE endpoint on host:"

# Keep the SSE connection alive in the background.
# This is required -- the session only exists while GET /sse is open.
curl -s -N "$BASE/sse" > "$SSE_OUTPUT" 2>/dev/null &
SSE_PID=$!
sleep 1

# Extract the session endpoint.
ENDPOINT=$(grep "^data:" "$SSE_OUTPUT" | head -1 | sed 's/^data: //' | tr -d '\r')

if [ -z "$ENDPOINT" ]; then
  echo "   ERROR: could not connect to wraptool SSE endpoint"
  kill $SSE_PID 2>/dev/null || true
  exit 1
fi

MSG_URL="${BASE}${ENDPOINT}"
echo "   Session endpoint: $MSG_URL"

post() {
  curl -s -X POST "$MSG_URL" -H "Content-Type: application/json" -d "$1"
  sleep 0.3
  # Read response from SSE stream.
  tail -1 "$SSE_OUTPUT" | grep "^data:" | sed 's/^data: //' | tr -d '\r'
}

echo
echo "3. Initialize MCP session:"
post '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"container-test","version":"1.0"}}}'
sleep 0.5
INIT=$(grep "^data:" "$SSE_OUTPUT" | tail -1 | sed 's/^data: //' | tr -d '\r')
echo "   $(echo "$INIT" | grep -o '"serverInfo":{[^}]*}')"

# Send initialized notification.
curl -s -X POST "$MSG_URL" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' > /dev/null
sleep 0.3

echo
echo "4. List available tools:"
curl -s -X POST "$MSG_URL" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' > /dev/null
sleep 0.5
TOOLS=$(grep "^data:" "$SSE_OUTPUT" | tail -1 | sed 's/^data: //' | tr -d '\r')
echo "   $(echo "$TOOLS" | grep -o '"name":"[^"]*"' | sed 's/"name":"//;s/"$//' | tr '\n' ', ')"

echo
echo "5. Call git_status --short --branch (ALLOWED):"
curl -s -X POST "$MSG_URL" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"git_status","arguments":{"short":true,"branch":true}}}' > /dev/null
sleep 0.5
RESULT=$(grep "^data:" "$SSE_OUTPUT" | tail -1 | sed 's/^data: //' | tr -d '\r')
echo "   $(echo "$RESULT" | sed 's/.*"text":"//;s/"}.*//' | sed 's/\\n/\n   /g' | head -10)"

echo
echo "6. Call git_log --oneline -n 3 (ALLOWED):"
curl -s -X POST "$MSG_URL" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"git_log","arguments":{"oneline":"","n":3}}}' > /dev/null
sleep 0.5
RESULT=$(grep "^data:" "$SSE_OUTPUT" | tail -1 | sed 's/^data: //' | tr -d '\r')
echo "   $(echo "$RESULT" | sed 's/.*"text":"//;s/"}.*//' | sed 's/\\n/\n   /g')"

echo
echo "7. Call git_log -n 999 (DENIED -- exceeds max 50):"
curl -s -X POST "$MSG_URL" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"git_log","arguments":{"n":999}}}' > /dev/null
sleep 0.5
RESULT=$(grep "^data:" "$SSE_OUTPUT" | tail -1 | sed 's/^data: //' | tr -d '\r')
echo "   $(echo "$RESULT" | sed 's/.*"text":"//;s/"}.*//' | sed 's/\\n/\n   /g')"

echo
echo "8. Submit capability request for git push:"
curl -s -X POST "$MSG_URL" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"wraptool_request_capability","arguments":{"tool":"git","subcommand":["push"],"flags":["--set-upstream"],"rationale":"Need to push feature branch for CI","enables":"CI pipeline"}}}' > /dev/null
sleep 0.5
RESULT=$(grep "^data:" "$SSE_OUTPUT" | tail -1 | sed 's/^data: //' | tr -d '\r')
echo "   $(echo "$RESULT" | sed 's/.*"text":"//;s/"}.*//' | sed 's/\\n/\n   /g')"

# Cleanup.
kill $SSE_PID 2>/dev/null || true

echo
echo "=== Container test complete ==="
