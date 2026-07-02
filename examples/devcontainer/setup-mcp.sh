#!/bin/sh
# postCreateCommand: write the MCP client config pointing Claude Code at the
# host-side wraptool. Run automatically when the devcontainer is created; safe
# to re-run. Reads WRAPTOOL_URL / WRAPTOOL_TOKEN from the environment.
set -e

: "${WRAPTOOL_URL:=http://host.docker.internal:8717}"
CONFIG_DIR="$HOME/.config/claude"
CONFIG="$CONFIG_DIR/settings.json"

mkdir -p "$CONFIG_DIR"

# Bearer header only when a token is configured (empty token = wraptool trusts
# localhost, which does not apply across the bridge, so a token is expected).
jq -n \
  --arg url "${WRAPTOOL_URL%/}/sse" \
  --arg tok "${WRAPTOOL_TOKEN:-}" \
  '{
     mcpServers: {
       wraptool: (
         { url: $url, transport: "sse" }
         + (if $tok == "" then {} else { headers: { Authorization: ("Bearer " + $tok) } } end)
       )
     }
   }' > "$CONFIG"

echo "wrote MCP config -> $CONFIG (wraptool @ ${WRAPTOOL_URL%/})"
if [ -z "${WRAPTOOL_TOKEN:-}" ]; then
  echo "WARNING: WRAPTOOL_TOKEN is empty — set it on the host if wraptool has auth_token_file." >&2
fi
