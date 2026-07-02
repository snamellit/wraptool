#!/bin/sh
# Runtime wiring for the wraptool-connect feature. Runs as postCreateCommand in
# the workspace folder. Writes the MCP client config pointing at the host-side
# wraptool. POSIX sh (dash-safe); installs nothing.
#
# Inputs (env, set by the feature / wraptool up / the user):
#   WRAPTOOL_TOKEN   bearer token for wraptool auth (empty if server trusts localhost)
#   WRAPTOOL_CWD     HOST path of the workspace, passed to the server as ?cwd=
#                    (for VS Code, set containerEnv WRAPTOOL_CWD=${localWorkspaceFolder})
# Options (from options.env, written at install time): WT_HARNESS WT_PORT WT_URL
#
# Test hooks: WT_ROUTE_FILE overrides /proc/net/route; WT_CONFIG_OUT overrides the
# output config path.
set -eu

SHARE=/usr/local/share/wraptool-connect
[ -f "$SHARE/options.env" ] && . "$SHARE/options.env"
: "${WT_HARNESS:=claude}"
: "${WT_PORT:=8717}"
: "${WT_URL:=}"
ROUTE_FILE="${WT_ROUTE_FILE:-/proc/net/route}"

# default_gateway: print the IPv4 default-route gateway by parsing the kernel
# route table. The gateway on the container's bridge network IS the host, so no
# host.docker.internal / --add-host is needed. Gateway is stored little-endian
# hex, so the dotted quad is the byte pairs in reverse.
default_gateway() {
  while read -r iface dest gw _; do
    [ "$dest" = "00000000" ] || continue
    [ "$gw" = "00000000" ] && continue
    printf '%d.%d.%d.%d\n' \
      "0x$(printf '%s' "$gw" | cut -c7-8)" \
      "0x$(printf '%s' "$gw" | cut -c5-6)" \
      "0x$(printf '%s' "$gw" | cut -c3-4)" \
      "0x$(printf '%s' "$gw" | cut -c1-2)"
    return 0
  done < "$ROUTE_FILE"
  return 1
}

# Minimal URL-encoding for the cwd query value (spaces and query delimiters).
urlencode() {
  printf '%s' "$1" | sed -e 's/%/%25/g' -e 's/ /%20/g' -e 's/#/%23/g' -e 's/&/%26/g' -e 's/?/%3F/g'
}

# Resolve the host base URL.
if [ -n "$WT_URL" ]; then
  BASE="${WT_URL%/}"
else
  if GW="$(default_gateway)"; then
    BASE="http://${GW}:${WT_PORT}"
  else
    echo "wraptool-connect: ERROR could not detect host gateway from $ROUTE_FILE; set the feature 'url' option." >&2
    exit 1
  fi
fi

SSE_URL="${BASE}/sse"
if [ -n "${WRAPTOOL_CWD:-}" ]; then
  SSE_URL="${SSE_URL}?cwd=$(urlencode "$WRAPTOOL_CWD")"
fi

# Build the wraptool server entry (add a bearer header only when a token is set).
if [ -n "${WRAPTOOL_TOKEN:-}" ]; then
  SERVER=$(printf '{"type":"sse","url":"%s","headers":{"Authorization":"Bearer %s"}}' "$SSE_URL" "$WRAPTOOL_TOKEN")
else
  SERVER=$(printf '{"type":"sse","url":"%s"}' "$SSE_URL")
fi

case "$WT_HARNESS" in
  claude) TARGET="${WT_CONFIG_OUT:-$PWD/.mcp.json}" ;;
  *) echo "wraptool-connect: ERROR unsupported harness '$WT_HARNESS'" >&2; exit 1 ;;
esac

# Merge into an existing config with jq when available (preserves other MCP
# servers); otherwise write fresh, backing up any existing file once.
if command -v jq >/dev/null 2>&1 && [ -f "$TARGET" ]; then
  tmp="$(mktemp)"
  jq --argjson s "$SERVER" '.mcpServers = (.mcpServers // {}) | .mcpServers.wraptool = $s' "$TARGET" > "$tmp"
  mv "$tmp" "$TARGET"
else
  if [ -f "$TARGET" ] && [ ! -f "$TARGET.wraptool-bak" ]; then
    cp "$TARGET" "$TARGET.wraptool-bak"
    echo "wraptool-connect: backed up existing $TARGET -> $TARGET.wraptool-bak" >&2
  fi
  printf '{\n  "mcpServers": {\n    "wraptool": %s\n  }\n}\n' "$SERVER" > "$TARGET"
fi

echo "wraptool-connect: wrote $TARGET (wraptool @ ${BASE})"
[ -z "${WRAPTOOL_TOKEN:-}" ] && echo "wraptool-connect: WARNING WRAPTOOL_TOKEN empty — set it on the host if wraptool uses auth_token_file." >&2
[ -z "${WRAPTOOL_CWD:-}" ] && echo "wraptool-connect: WARNING WRAPTOOL_CWD empty — the server can't scope tools to this worktree; set containerEnv WRAPTOOL_CWD=\${localWorkspaceFolder}." >&2
exit 0
