#!/bin/sh
# Client-side smoke test for a wraptool devcontainer. Run INSIDE the container;
# it verifies isolation and that the host-side wraptool is reachable and
# functional over MCP/SSE. Exits non-zero on the first failure so it is usable
# in CI.
#
#   WRAPTOOL_URL      host wraptool base URL (default http://host.docker.internal:8717)
#   WRAPTOOL_TOKEN    bearer token, if wraptool has auth_token_file set
#   WRAPTOOL_HARNESS  harness to check for the pool runtime mount (default claude);
#                      only used by the best-effort check in step 6
#
# Mirrors ../../test-container.sh (the Guix test) but targets the docker bridge
# and sends auth.
set -eu

BASE="${WRAPTOOL_URL:-http://host.docker.internal:8717}"
BASE="${BASE%/}"
SSE_OUTPUT="$(mktemp)"
PASS=0
FAIL=0

AUTH_HEADER=""
[ -n "${WRAPTOOL_TOKEN:-}" ] && AUTH_HEADER="Authorization: Bearer ${WRAPTOOL_TOKEN}"

# curl with the bearer header attached when present.
cw() {
  if [ -n "$AUTH_HEADER" ]; then
    curl -s -H "$AUTH_HEADER" "$@"
  else
    curl -s "$@"
  fi
}

ok()   { PASS=$((PASS + 1)); echo "  PASS: $1"; }
bad()  { FAIL=$((FAIL + 1)); echo "  FAIL: $1" >&2; }

cleanup() { [ -n "${SSE_PID:-}" ] && kill "$SSE_PID" 2>/dev/null || true; rm -f "$SSE_OUTPUT"; }
trap cleanup EXIT

echo "=== wraptool devcontainer connection test ==="
echo "target: $BASE   auth: $([ -n "$AUTH_HEADER" ] && echo bearer || echo none)"
echo

echo "1. Isolation — privileged tools/credentials must NOT be in the container:"
if command -v git >/dev/null 2>&1; then bad "git present in container (should be host-only)"; else ok "git absent"; fi
if ls "$HOME"/.ssh/id_* >/dev/null 2>&1; then bad "ssh keys present"; else ok "no ssh keys"; fi

echo
echo "2. Open SSE session to host wraptool:"
cw -N "$BASE/sse" > "$SSE_OUTPUT" 2>/dev/null &
SSE_PID=$!
# Wait up to ~5s for the session endpoint to appear.
ENDPOINT=""
i=0
while [ $i -lt 25 ]; do
  ENDPOINT=$(grep "^data:" "$SSE_OUTPUT" 2>/dev/null | head -1 | sed 's/^data: //' | tr -d '\r' || true)
  [ -n "$ENDPOINT" ] && break
  i=$((i + 1)); sleep 0.2
done
if [ -z "$ENDPOINT" ]; then
  bad "could not open SSE session (wraptool not reachable at $BASE, or auth rejected)"
  echo; echo "RESULT: $PASS passed, $FAIL failed"; exit 1
fi
MSG_URL="${BASE}${ENDPOINT}"
ok "session endpoint acquired"

echo
echo "3. initialize:"
cw -X POST "$MSG_URL" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"devcontainer-test","version":"1.0"}}}' > /dev/null
sleep 0.5
INIT=$(grep "^data:" "$SSE_OUTPUT" | tail -1 | sed 's/^data: //' | tr -d '\r')
if echo "$INIT" | grep -q '"serverInfo"'; then ok "server responded: $(echo "$INIT" | grep -o '"serverInfo":{[^}]*}')"; else bad "no serverInfo in initialize response"; fi
cw -X POST "$MSG_URL" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' > /dev/null
sleep 0.3

echo
echo "4. tools/list:"
cw -X POST "$MSG_URL" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' > /dev/null
sleep 0.5
TOOLS=$(grep "^data:" "$SSE_OUTPUT" | tail -1 | sed 's/^data: //' | tr -d '\r')
NAMES=$(echo "$TOOLS" | grep -o '"name":"[^"]*"' | sed 's/"name":"//;s/"$//' | tr '\n' ' ')
if [ -n "$NAMES" ]; then ok "tools: $NAMES"; else bad "no tools returned"; fi

echo
echo "5. Call an allowed tool (git_status), if present:"
if echo "$NAMES" | grep -q "git_status"; then
  cw -X POST "$MSG_URL" -H "Content-Type: application/json" \
    -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"git_status","arguments":{"short":true}}}' > /dev/null
  sleep 0.5
  RESULT=$(grep "^data:" "$SSE_OUTPUT" | tail -1 | sed 's/^data: //' | tr -d '\r')
  if echo "$RESULT" | grep -q '"result"'; then ok "git_status returned a result"; else bad "git_status call failed: $RESULT"; fi
else
  echo "  SKIP: git_status not in whitelist"
fi

echo
echo "6. Harness pool runtime mount (optional — this fixture predates the wraptool"
echo "   feature and does not declare the pool mounts by default; see README.md):"
HARNESS="${WRAPTOOL_HARNESS:-claude}"
WANT_RUNTIME_BIN="/opt/wraptool-harness/runtime/bin/$HARNESS"
if command -v "$HARNESS" >/dev/null 2>&1; then
  RESOLVED="$(command -v "$HARNESS")"
  if [ "$RESOLVED" = "$WANT_RUNTIME_BIN" ]; then
    ok "$HARNESS resolves to the pool runtime mount ($WANT_RUNTIME_BIN)"
  else
    echo "  SKIP: $HARNESS resolves to $RESOLVED, not $WANT_RUNTIME_BIN (pool runtime not mounted here)"
  fi
else
  echo "  SKIP: $HARNESS not on PATH (add the wraptool feature + pool mounts to exercise this)"
fi

echo
echo "RESULT: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
