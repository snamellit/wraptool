#!/usr/bin/env sh
# Protocol-level spike for design/harness/acp-bridge.md §13.
#
# Runs a harness's ACP entry point inside a Guix container and speaks ACP over
# this process's own stdin/stdout — so an editor can spawn THIS SCRIPT as its
# ACP agent. It is a hand-rolled stand-in for the entry-argv primitive of §12
# WP1, which does not exist yet, and it deliberately mirrors
# buildGuixShellArgs / buildContainerEntry (cmd/guixcontainer.go:597-633) so
# that a result here predicts a result from the real thing.
#
# Prerequisites:
#   wraptool harness install opencode     # binary + auth land in the pool
#   opencode auth login                   # inside a `wraptool up` shell, once
#
# Zed (~/.config/zed/settings.json):
#   "agent_servers": {
#     "wraptool-opencode": {
#       "command": "/abs/path/to/acp-protocol-run.sh",
#       "args": [],
#       "env": {}
#     }
#   }
#
# EVERYTHING this script prints for its own purposes goes to stderr. stdout
# belongs to the harness alone — that is the §4.1 invariant, and violating it
# here would break the protocol in exactly the way the design says it would.

set -eu

HARNESS="${WRAPTOOL_ACP_HARNESS:-opencode}"
ROOT="${WRAPTOOL_ACP_ROOT:-$PWD}"
POOL="${XDG_DATA_HOME:-$HOME/.local/share}/wraptool/harness-pool"
POOL_HOME="$POOL/home"

case "$HARNESS" in
  opencode) ACP_ARGV='opencode acp' ;;
  gemini)   ACP_ARGV='gemini --acp' ;;
  *)        echo "no known ACP entry point for '$HARNESS' — see §8" >&2; exit 2 ;;
esac

[ -f "$ROOT/manifest.scm" ] || { echo "no manifest.scm at $ROOT" >&2; exit 2; }
[ -d "$POOL_HOME" ] || { echo "no harness pool home at $POOL_HOME — run 'wraptool harness install $HARNESS'" >&2; exit 2; }

# Pool plan, simplified from resolveGuixPoolPlan (cmd/guixcontainer.go:472).
# The real one gates on the active pointer for this platform; here we expose
# whichever trees exist, since a spike only needs the binary to resolve.
set --
PATH_APPEND=""
if [ -d "$POOL/runtime/bin" ]; then
  set -- "$@" --expose="$POOL/runtime=$POOL/runtime"
  PATH_APPEND="$PATH_APPEND:$POOL/runtime/bin"
fi
if [ -d "$POOL/bin" ] && [ -d "$POOL/lib" ]; then
  set -- "$@" --expose="$POOL/bin=$POOL/bin" --expose="$POOL/lib=$POOL/lib"
  PATH_APPEND="$PATH_APPEND:$POOL/bin"
fi
[ -n "$PATH_APPEND" ] || { echo "harness pool has neither runtime/bin nor bin+lib" >&2; exit 2; }

echo "acp spike: $HARNESS ($ACP_ARGV) in $ROOT" >&2

# buildContainerEntry, with `exec bash -l` replaced by the ACP entry point.
ENTRY="export WRAPTOOL_CONTAINER=1; export TERM=xterm-256color; export PATH=\$PATH$PATH_APPEND; export HOME='$POOL_HOME'; cd '$ROOT'; exec $ACP_ARGV"

# `exec` so no shell sits between the editor and the harness holding pipes
# open, and so signals reach guix directly. Note T5 (§13.1): guix's wrapper
# survives SIGTERM, so a container may outlive the editor closing this
# connection. Check with: pgrep -f 'guix shell --container'
exec guix shell --container --network --emulate-fhs \
  --share="$ROOT=$ROOT" \
  "$@" \
  --share="$POOL_HOME=$POOL_HOME" \
  --preserve='^WRAPTOOL_' \
  --preserve='^COLORTERM$' \
  -m "$ROOT/manifest.scm" \
  -- bash -lc "$ENTRY"
