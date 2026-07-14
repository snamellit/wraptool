#!/bin/sh
# Build-time install for the wraptool-harness feature. Installs one AI coding
# harness binary into the image and prepares the shared-pool HOME mount point.
#
# Two axes, deliberately separate (see design/harness-descriptor.md §7):
#   binary   — installed here, per image (cheap to copy; isolated per project)
#   state    — NOT here; HOME is pinned (containerEnv) to /home/wraptool-harness,
#              which `wraptool up` (or your devcontainer.json) bind-mounts from the
#              host pool ($XDG_DATA_HOME/wraptool/harness-pool/home, per-developer)
#              so auth/skills/config persist across every project.
#
# The harness->package map below mirrors internal/harness.Registry the same way
# connect.sh mirrors the Go renderers: a small, deliberate duplication so the
# feature ships standalone (no Go). Keep them in sync when adding a harness.
#
# POSIX sh (dash-safe). Feature options arrive as upper-cased env: HARNESS, VERSION.
set -eu

HARNESS="${HARNESS:-claude}"
VERSION="${VERSION:-}"
# WT_POOL_HOME overrides the mount target (test hook, like connect.sh's WT_*): a
# test points it at a writable temp dir. Must match containerEnv.HOME in
# devcontainer-feature.json and cmd/guixcontainer.go's devcontainerPoolHome.
POOL_HOME="${WT_POOL_HOME:-/home/wraptool-harness}"

# npm_install <package> — install a global npm harness at the requested version.
# npm harnesses need node at build (npm) and runtime; error clearly if absent
# rather than pulling node ourselves (keep the feature image-agnostic, like
# wraptool-connect). Add the node feature to your devcontainer.json.
npm_install() {
  pkg="$1"
  if ! command -v npm >/dev/null 2>&1; then
    echo "wraptool-harness: ERROR '$HARNESS' installs via npm but no node/npm in the image." >&2
    echo "  Add a node runtime, e.g. \"ghcr.io/devcontainers/features/node:1\": {} to your devcontainer.json." >&2
    exit 1
  fi
  [ -n "$VERSION" ] && pkg="${pkg}@${VERSION}"
  echo "wraptool-harness: npm install -g $pkg"
  npm install -g "$pkg"
}

case "$HARNESS" in
  claude)   npm_install "@anthropic-ai/claude-code" ;;
  opencode) npm_install "opencode-ai" ;;
  pi)       npm_install "@earendil-works/pi-coding-agent" ;;
  agy)
    # Antigravity: a static Go binary, no node. Its official script installs to
    # ~/.local/bin; relocate onto PATH so any container user finds it.
    if ! command -v curl >/dev/null 2>&1; then
      echo "wraptool-harness: ERROR 'agy' installs via curl but no curl in the image." >&2
      exit 1
    fi
    echo "wraptool-harness: installing agy via its official script"
    curl -fsSL https://antigravity.google/cli/install.sh | bash
    install -Dm755 "$HOME/.local/bin/agy" /usr/local/bin/agy
    ;;
  gemini)
    echo "wraptool-harness: ERROR 'gemini' has no automated installer here; bake it into your image." >&2
    exit 1
    ;;
  *)
    echo "wraptool-harness: ERROR unknown harness '$HARNESS' (want: claude opencode pi agy gemini)" >&2
    exit 1
    ;;
esac

# Prepare the shared-pool HOME mount point. The host pool dir is bind-mounted
# here at run time; create it now (world-writable so it works whatever uid the
# container user ends up being) so the harness has a HOME even if no mount lands
# (degrades to fresh, non-shared state rather than failing).
mkdir -p "$POOL_HOME"
chmod 0777 "$POOL_HOME"

echo "wraptool-harness: installed $HARNESS (HOME -> $POOL_HOME; bind-mount the host pool onto it to share state)"
