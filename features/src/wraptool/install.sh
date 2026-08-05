#!/bin/sh
# Build-time no-op for the wraptool feature's wiring-only major (see
# design/harness/unified-harness-pool.md §12.2/§12.3). The harness binary is no
# longer installed here — it comes from the host harness pool's `runtime/` tree,
# mounted read-only at /opt/wraptool-harness/runtime by `wraptool up` (or
# declared manually for a plain `devcontainer up`, §12.3). This script only:
#   - pins the shared-pool HOME (containerEnv invariant, see the "//" note in
#     devcontainer-feature.json): creates the mount point dir so HOME is valid
#     even when nothing is mounted onto it (degrades to fresh, non-shared state
#     rather than a broken HOME);
#   - stages connect.sh + its frozen options for postCreate (the `connect` job).
#
# No packages installed, no network access. POSIX sh (dash-safe). Feature
# options arrive as upper-cased env: HARNESS, CONNECT, PORT, URL.
set -eu

HARNESS="${HARNESS:-claude}"
CONNECT="${CONNECT:-true}"
PORT="${PORT:-8717}"
URL="${URL:-}"
# WT_POOL_HOME overrides the mount target (test hook, like connect.sh's WT_*).
# Must match containerEnv.HOME in devcontainer-feature.json and cmd/guixcontainer.go.
POOL_HOME="${WT_POOL_HOME:-/home/wraptool-harness}"
SHARE="${WT_SHARE_DIR:-/usr/local/share/wraptool}"
FEATURE_DIR="$(cd "$(dirname "$0")" && pwd)"

# Always prepare the shared-pool HOME mount point (world-writable so it works
# whatever uid the container user ends up being).
mkdir -p "$POOL_HOME"
chmod 0777 "$POOL_HOME"

# --- connect: stage the runtime wiring script ----------------------------------
# connect.sh runs at postCreate (options frozen here; postCreate has no feature
# env). It self-skips when CONNECT is false, so the postCreateCommand is harmless
# either way.
mkdir -p "$SHARE"
cat > "$SHARE/options.env" <<EOF
WT_HARNESS='${HARNESS}'
WT_CONNECT='${CONNECT}'
WT_PORT='${PORT}'
WT_URL='${URL}'
EOF
cp "$FEATURE_DIR/connect.sh" "$SHARE/connect.sh"
chmod +x "$SHARE/connect.sh"

echo "wraptool: wired (harness=${HARNESS}, connect=${CONNECT}); HOME -> ${POOL_HOME}, PATH += /opt/wraptool-harness/runtime/bin"
