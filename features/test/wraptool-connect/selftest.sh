#!/bin/sh
# Standalone test for the wraptool-connect feature's connect.sh — runs without
# the devcontainer CLI or docker. Exercises gateway parsing, cwd/token wiring,
# and jq merge. Exit non-zero on failure.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
CONNECT="$HERE/../../src/wraptool-connect/connect.sh"
WORK="$(mktemp -d)"
FAIL=0

trap 'rm -rf "$WORK"' EXIT
pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
have() { grep -qF "$1" "$2" || fail "expected '$1' in $2"; }

# A fake /proc/net/route with default gateway 172.18.0.1 (0xAC120001, stored
# little-endian as 010012AC). Columns: Iface Dest Gateway Flags ...
ROUTE="$WORK/route"
printf '%s\n' \
  'Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT' \
  'eth0	00000000	010012AC	0003	0	0	0	00000000	0	0	0' \
  'eth0	000012AC	00000000	0001	0	0	0	0000FFFF	0	0	0' > "$ROUTE"

echo "1. Auto-detected gateway + cwd + token -> .mcp.json"
OUT="$WORK/mcp.json"
env -i sh -c "
  WT_ROUTE_FILE='$ROUTE' WT_SHARE_DIR='$WORK/none' WT_CONFIG_OUT='$OUT' \
  WRAPTOOL_TOKEN='tok123' WRAPTOOL_CWD='/home/pti/src/proj a' \
  PATH='$PATH' HOME='$WORK' PWD='$WORK' \
  sh '$CONNECT'
" >/dev/null
have 'http://172.18.0.1:8717/sse?cwd=/home/pti/src/proj%20a' "$OUT"
have 'Bearer tok123' "$OUT"
have '"type":"sse"' "$OUT"
[ "$FAIL" -eq 0 ] && pass "gateway+cwd+token rendered"

echo "2. Explicit url, no token -> no headers, trailing slash trimmed"
OUT2="$WORK/mcp2.json"
env -i sh -c "
  WT_ROUTE_FILE='$ROUTE' WT_SHARE_DIR='$WORK/none' WT_CONFIG_OUT='$OUT2' WT_URL='http://10.0.0.5:9000/' \
  PATH='$PATH' HOME='$WORK' PWD='$WORK' \
  sh '$CONNECT'
" >/dev/null
have 'http://10.0.0.5:9000/sse' "$OUT2"
if grep -qF 'Authorization' "$OUT2"; then fail "unexpected auth header with no token"; else pass "no header without token"; fi

echo "3. jq merge preserves an existing MCP server"
OUT3="$WORK/mcp3.json"
printf '{\n  "mcpServers": {\n    "other": {"type":"stdio","command":"foo"}\n  }\n}\n' > "$OUT3"
if command -v jq >/dev/null 2>&1; then
  env -i sh -c "
    WT_ROUTE_FILE='$ROUTE' WT_SHARE_DIR='$WORK/none' WT_CONFIG_OUT='$OUT3' WT_URL='http://10.0.0.5:8717' \
    PATH='$PATH' HOME='$WORK' PWD='$WORK' \
    sh '$CONNECT'
  " >/dev/null
  have '"other"' "$OUT3"
  have '"wraptool"' "$OUT3"
  jq -e . "$OUT3" >/dev/null && pass "merged config is valid JSON, other server kept"
else
  echo "  SKIP: jq not installed (merge path)"
fi

echo "4. agy harness -> .agents/mcp_config.json, Streamable HTTP /mcp, serverUrl, no type"
OUT4="$WORK/agy/.agents/mcp_config.json"
env -i sh -c "
  WT_ROUTE_FILE='$ROUTE' WT_SHARE_DIR='$WORK/none' WT_CONFIG_OUT='$OUT4' WT_HARNESS='agy' \
  WRAPTOOL_TOKEN='tok123' WRAPTOOL_CWD='/home/pti/src/proj a' \
  PATH='$PATH' HOME='$WORK' PWD='$WORK' \
  sh '$CONNECT'
" >/dev/null
have 'http://172.18.0.1:8717/mcp?cwd=/home/pti/src/proj%20a' "$OUT4"
have '"serverUrl"' "$OUT4"
have 'Bearer tok123' "$OUT4"
if grep -qF '"type"' "$OUT4"; then fail "agy config should not carry a type field"; else pass "agy streamable /mcp rendered, no type"; fi

echo
echo "RESULT: $FAIL failed"
[ "$FAIL" -eq 0 ]
