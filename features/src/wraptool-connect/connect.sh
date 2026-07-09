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
# Test hooks: WT_ROUTE_FILE overrides /proc/net/route; WT_SHARE_DIR overrides the
# install dir (skips the host's options.env); WT_CONFIG_OUT overrides the output
# config path.
set -eu

# WT_SHARE_DIR overrides the install dir (test hook, like WT_ROUTE_FILE): a test
# points it at an empty dir so the host's installed options.env cannot clobber
# the WT_* env it passes in.
SHARE="${WT_SHARE_DIR:-/usr/local/share/wraptool-connect}"
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

# Per-harness config shape, mirroring the Go renderers in internal/harness. Only
# the container-CLI harnesses (those that run inside the container) are here; the
# host IDEs (cursor, roo) are configured host-side by `wraptool init`, never this.
#   EP      endpoint path — legacy SSE (/sse) or Streamable HTTP (/mcp, agy)
#   TOP     top-level object key ("mcpServers", or opencode's "mcp")
#   URL_KEY the server entry's URL field (url / serverUrl / httpUrl)
#   TYPE    a leading "type":"…", fragment, or empty
#   EXTRA   a trailing fragment (opencode's ,"enabled":true), or empty
#   TARGET  the config file this harness reads
case "$WT_HARNESS" in
  claude)   EP="/sse"; TOP="mcpServers"; URL_KEY="url";       TYPE='"type":"sse",';    EXTRA='';                TARGET="${WT_CONFIG_OUT:-$PWD/.mcp.json}" ;;
  agy)      EP="/mcp"; TOP="mcpServers"; URL_KEY="serverUrl"; TYPE='';                 EXTRA='';                TARGET="${WT_CONFIG_OUT:-$PWD/.agents/mcp_config.json}" ;;
  opencode) EP="/sse"; TOP="mcp";        URL_KEY="url";       TYPE='"type":"remote",'; EXTRA=',"enabled":true'; TARGET="${WT_CONFIG_OUT:-$PWD/opencode.json}" ;;
  gemini)   EP="/sse"; TOP="mcpServers"; URL_KEY="httpUrl";   TYPE='';                 EXTRA='';                TARGET="${WT_CONFIG_OUT:-$PWD/.gemini/settings.json}" ;;
  pi)       EP="/sse"; TOP="mcpServers"; URL_KEY="url";       TYPE='';                 EXTRA='';                TARGET="${WT_CONFIG_OUT:-$PWD/.pi/mcp.json}" ;;
  *) echo "wraptool-connect: ERROR unsupported harness '$WT_HARNESS' (container CLIs: claude agy opencode gemini pi)" >&2; exit 1 ;;
esac

URL="${BASE}${EP}"
if [ -n "${WRAPTOOL_CWD:-}" ]; then
  URL="${URL}?cwd=$(urlencode "$WRAPTOOL_CWD")"
fi

# Build the wraptool server entry (add a bearer header only when a token is set).
if [ -n "${WRAPTOOL_TOKEN:-}" ]; then
  HDR=$(printf ',"headers":{"Authorization":"Bearer %s"}' "$WRAPTOOL_TOKEN")
else
  HDR=''
fi
SERVER=$(printf '{%s"%s":"%s"%s%s}' "$TYPE" "$URL_KEY" "$URL" "$HDR" "$EXTRA")

# Ensure the config's parent dir exists (agy/gemini/pi write under a subdir).
mkdir -p "$(dirname "$TARGET")"

# Merge into an existing config with jq when available (preserves other MCP
# servers under the same top-level key); otherwise write fresh, backing up any
# existing file once.
if command -v jq >/dev/null 2>&1 && [ -f "$TARGET" ]; then
  tmp="$(mktemp)"
  jq --argjson s "$SERVER" --arg top "$TOP" '.[$top] = (.[$top] // {}) | .[$top].wraptool = $s' "$TARGET" > "$tmp"
  mv "$tmp" "$TARGET"
else
  if [ -f "$TARGET" ] && [ ! -f "$TARGET.wraptool-bak" ]; then
    cp "$TARGET" "$TARGET.wraptool-bak"
    echo "wraptool-connect: backed up existing $TARGET -> $TARGET.wraptool-bak" >&2
  fi
  printf '{\n  "%s": {\n    "wraptool": %s\n  }\n}\n' "$TOP" "$SERVER" > "$TARGET"
fi

echo "wraptool-connect: wrote $TARGET (wraptool @ ${BASE})"
[ -z "${WRAPTOOL_TOKEN:-}" ] && echo "wraptool-connect: WARNING WRAPTOOL_TOKEN empty — set it on the host if wraptool uses auth_token_file." >&2
[ -z "${WRAPTOOL_CWD:-}" ] && echo "wraptool-connect: WARNING WRAPTOOL_CWD empty — the server can't scope tools to this worktree; set containerEnv WRAPTOOL_CWD=\${localWorkspaceFolder}." >&2
exit 0
