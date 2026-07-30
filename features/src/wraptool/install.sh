#!/bin/sh
# Build-time install for the wraptool feature. Two orthogonal jobs off one
# harness choice, each independently toggled (see design/harness/harness-descriptor.md):
#   provision (default true) — install the harness binary into THIS image
#   connect   (default true) — stage the runtime wiring script (run at postCreate)
#
# The shared-pool HOME (containerEnv, /home/wraptool-harness) is a feature
# invariant, not a toggle — see the "//" note in devcontainer-feature.json. This
# script always creates that dir so HOME is valid even when nothing is mounted
# onto it (state degrades to fresh, non-shared rather than a broken HOME).
#
# The harness->package map mirrors internal/harness.Registry the same way
# connect.sh mirrors the Go renderers: a small, deliberate duplication so the
# feature ships standalone (no Go). Keep them in sync when adding a harness.
#
# POSIX sh (dash-safe). Feature options arrive as upper-cased env: HARNESS,
# PROVISION, CONNECT, VERSION, PORT, URL.
set -eu

HARNESS="${HARNESS:-claude}"
PROVISION="${PROVISION:-true}"
CONNECT="${CONNECT:-true}"
VERSION="${VERSION:-}"
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

# --- provision: install the harness binary -------------------------------------
# npm harnesses need node at build (npm) and runtime; error clearly if absent
# rather than pulling node ourselves (keep the feature image-agnostic). Add the
# node feature to your devcontainer.json.
npm_install() {
  pkg="$1"
  if ! command -v npm >/dev/null 2>&1; then
    echo "wraptool: ERROR '$HARNESS' installs via npm but no node/npm in the image." >&2
    echo "  Add a node runtime, e.g. \"ghcr.io/devcontainers/features/node:1\": {} to your devcontainer.json." >&2
    exit 1
  fi
  [ -n "$VERSION" ] && pkg="${pkg}@${VERSION}"
  echo "wraptool: npm install -g $pkg"
  npm install -g "$pkg"
}

if [ "$PROVISION" = true ]; then
  case "$HARNESS" in
    claude)   npm_install "@anthropic-ai/claude-code" ;;
    opencode) npm_install "opencode-ai" ;;
    pi)       npm_install "@earendil-works/pi-coding-agent" ;;
    agy)
      # Antigravity: a static Go binary, no node. Its official script installs to
      # ~/.local/bin; relocate onto PATH so any container user finds it.
      if ! command -v curl >/dev/null 2>&1; then
        echo "wraptool: ERROR 'agy' installs via curl but no curl in the image." >&2
        exit 1
      fi
      echo "wraptool: installing agy via its official script"
      curl -fsSL https://antigravity.google/cli/install.sh | bash
      install -Dm755 "$HOME/.local/bin/agy" /usr/local/bin/agy
      ;;
    gemini)
      echo "wraptool: ERROR 'gemini' has no automated installer here; bake it into your image (provision: false)." >&2
      exit 1
      ;;
    *)
      echo "wraptool: ERROR unknown harness '$HARNESS' (want: claude opencode pi agy gemini)" >&2
      exit 1
      ;;
  esac
else
  echo "wraptool: provision disabled — bring your own '$HARNESS' binary"
fi

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

echo "wraptool: installed (harness=${HARNESS}, provision=${PROVISION}, connect=${CONNECT}; HOME -> ${POOL_HOME})"
