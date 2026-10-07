#!/bin/sh
# Runtime wiring for the wraptool feature. Runs as postCreateCommand in the
# workspace folder. Writes the MCP client config pointing at the host-side
# wraptool. POSIX sh (dash-safe); installs nothing.
#
# Checks that the selected harness resolves on PATH (§12.2/§12.3: the binary
# now comes from the host pool's runtime/ mount, not this feature) before doing
# anything else. The check always RUNS, regardless of WT_CONNECT, but its
# SEVERITY follows intent: with connect enabled (the default) a missing binary
# hard-fails postCreate — wiring a harness that will never run is pointless.
# With connect explicitly disabled, the same message is a WARNING (exit 0):
# the user took manual control, and a container staged before running
# `wraptool harness install` must not be treated as broken. Self-skips the
# MCP-config write when connect is disabled, so the feature's static
# postCreateCommand is harmless when only the pool binary was wanted.
#
# Inputs (env, set by the feature / wraptool up / the user):
#   WRAPTOOL_TOKEN   bearer token for wraptool auth (empty if server trusts localhost)
#   WRAPTOOL_CWD     HOST path of the workspace, passed to the server as ?cwd=
#                    (for VS Code, set containerEnv WRAPTOOL_CWD=${localWorkspaceFolder})
# Options (from options.env, written at install time): WT_HARNESS WT_CONNECT WT_PORT WT_URL
#
# Test hooks: WT_ROUTE_FILE overrides /proc/net/route; WT_SHARE_DIR overrides the
# install dir (skips the host's options.env); WT_CONFIG_OUT overrides the output
# config path.
set -eu

# WT_SHARE_DIR overrides the install dir (test hook, like WT_ROUTE_FILE): a test
# points it at an empty dir so the host's installed options.env cannot clobber
# the WT_* env it passes in.
SHARE="${WT_SHARE_DIR:-/usr/local/share/wraptool}"
[ -f "$SHARE/options.env" ] && . "$SHARE/options.env"
: "${WT_HARNESS:=claude}"
: "${WT_CONNECT:=true}"
: "${WT_PORT:=8717}"
: "${WT_URL:=}"
ROUTE_FILE="${WT_ROUTE_FILE:-/proc/net/route}"

# Check the harness resolves on PATH — always, not gated by WT_CONNECT (§12.3):
# the pool mount is this feature's remaining reason to exist. Severity depends
# on intent: connect enabled -> ERROR and hard fail; connect disabled -> the
# user explicitly took manual control, so it's a WARNING and postCreate still
# succeeds (a container staged ahead of `wraptool harness install` is not
# broken).
if ! command -v "$WT_HARNESS" >/dev/null 2>&1; then
  if [ "$WT_CONNECT" = true ]; then
    LEVEL="ERROR"
  else
    LEVEL="WARNING"
  fi
  echo "wraptool: $LEVEL '$WT_HARNESS' not found on PATH." >&2
  echo "  The harness binary comes from the host pool, not this feature." >&2
  echo "  Run 'wraptool harness install $WT_HARNESS' on the host, then reopen." >&2
  echo "  Declare the pool runtime mount in devcontainer.json (see the feature description; 'wraptool init devcontainer' scaffolds it, and 'wraptool up' prints the exact entry when missing)." >&2
  if [ "$WT_CONNECT" = true ]; then
    exit 1
  fi
fi

# Self-skip when connect is disabled — wiring is the user's job in that mode.
if [ "$WT_CONNECT" != true ]; then
  echo "wraptool: connect disabled — MCP config not written (wire it yourself)"
  exit 0
fi

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
    echo "wraptool: ERROR could not detect host gateway from $ROUTE_FILE; set the feature 'url' option." >&2
    exit 1
  fi
fi

# Per-harness config shape, mirroring the Go renderers in internal/harness.
#   EP      endpoint path — legacy SSE (/sse) or Streamable HTTP (/mcp, agy, omp)
#   TOP     top-level object key ("mcpServers", or opencode's "mcp")
#   URL_KEY the server entry's URL field (url / serverUrl / httpUrl)
#   TYPE    a leading "type":"…", fragment, or empty
#   EXTRA   a trailing fragment (opencode's ,"enabled":true), or empty
#   TARGET  the config file this harness reads
case "$WT_HARNESS" in
  claude)   EP="/sse"; TOP="mcpServers"; URL_KEY="url";       TYPE='"type":"sse",';    EXTRA='';                TARGET="${WT_CONFIG_OUT:-$PWD/.mcp.json}" ;;
  agy)      EP="/mcp"; TOP="mcpServers"; URL_KEY="serverUrl"; TYPE='';                 EXTRA='';                TARGET="${WT_CONFIG_OUT:-$PWD/.agents/mcp_config.json}" ;;
  opencode) EP="/sse"; TOP="mcp";        URL_KEY="url";       TYPE='"type":"remote",'; EXTRA=',"enabled":true'; TARGET="${WT_CONFIG_OUT:-$PWD/opencode.json}" ;;
  omp)      EP="/mcp"; TOP="mcpServers"; URL_KEY="url";       TYPE='"type":"http",';   EXTRA='';                TARGET="${WT_CONFIG_OUT:-$PWD/.omp/mcp.json}" ;;
  pi)       EP="/sse"; TOP="mcpServers"; URL_KEY="url";       TYPE='';                 EXTRA='';                TARGET="${WT_CONFIG_OUT:-$PWD/.pi/mcp.json}" ;;
  *) echo "wraptool: ERROR unsupported harness '$WT_HARNESS' (container CLIs: claude agy omp opencode pi)" >&2; exit 1 ;;
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

# Ensure the config's parent dir exists (agy/pi write under a subdir).
mkdir -p "$(dirname "$TARGET")"

# The config carries the bearer token: create it owner-only (0600), matching
# `wraptool init`. umask covers the fresh write and the backup; the chmod
# below also tightens a pre-existing file and the jq merge's mktemp output.
umask 077

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
    echo "wraptool: backed up existing $TARGET -> $TARGET.wraptool-bak" >&2
  fi
  printf '{\n  "%s": {\n    "wraptool": %s\n  }\n}\n' "$TOP" "$SERVER" > "$TARGET"
fi
chmod 600 "$TARGET"

echo "wraptool: wrote $TARGET (wraptool @ ${BASE})"
[ -z "${WRAPTOOL_TOKEN:-}" ] && echo "wraptool: WARNING WRAPTOOL_TOKEN empty — set it on the host if wraptool uses auth_token_file." >&2
[ -z "${WRAPTOOL_CWD:-}" ] && echo "wraptool: WARNING WRAPTOOL_CWD empty — the server can't scope tools to this worktree; set containerEnv WRAPTOOL_CWD=\${localWorkspaceFolder}." >&2
exit 0
