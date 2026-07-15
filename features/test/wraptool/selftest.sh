#!/bin/sh
# Standalone test for the merged wraptool feature — runs without the devcontainer
# CLI, docker, or a real network. Covers both toggled jobs:
#   install.sh: provision gate (binary install), harness->package map, node guard,
#               HOME prep, options.env freeze, connect.sh staging
#   connect.sh: the CONNECT self-skip and the wiring output (gateway/token/cwd)
# Exit non-zero on failure.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SRC="$HERE/../../src/wraptool"
INSTALL="$SRC/install.sh"
CONNECT="$SRC/connect.sh"
WORK="$(mktemp -d)"
FAIL=0

trap 'rm -rf "$WORK"' EXIT
pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
have() { grep -qF "$1" "$2" || fail "expected '$1' in $2"; }
absent() { grep -qF "$1" "$2" && fail "did not expect '$1' in $2" || true; }

# Fake bin dir: npm/curl stubs that record args instead of acting.
# Portable shebang for the stub scripts: a guix host runner has no /bin/sh (no FHS
# off-container), so #!/bin/sh would fail to exec — /usr/bin/env resolves sh from PATH.
STUB="$WORK/bin"
mkdir -p "$STUB"
cat > "$STUB/npm" <<EOF
#!/usr/bin/env sh
echo "npm \$*" >> "$WORK/npm.log"
EOF
cat > "$STUB/curl" <<EOF
#!/usr/bin/env sh
echo "curl \$*" >> "$WORK/curl.log"
echo 'mkdir -p "\$HOME/.local/bin"; printf "#!/usr/bin/env sh\n" > "\$HOME/.local/bin/agy"; chmod +x "\$HOME/.local/bin/agy"'
EOF
chmod +x "$STUB/npm" "$STUB/curl"
# Prepend the stubs to the REAL PATH (coreutils live in /gnu/store on guix, not
# /usr/bin), so install.sh/connect.sh find mkdir/sed/etc. and the stubs win for
# npm/curl.
STUBPATH="$STUB:$PATH"

# install <harness> [extra env...] — run install.sh with per-run pool/share/home.
install_run() {
  h="$1"; shift
  RUN="$WORK/run.$$"; rm -rf "$RUN"; mkdir -p "$RUN"
  env -i HARNESS="$h" \
    WT_POOL_HOME="$RUN/pool-home" WT_SHARE_DIR="$RUN/share" \
    PATH="$STUBPATH" HOME="$RUN/home" "$@" \
    sh "$INSTALL" >"$RUN/out" 2>&1
  rc=$?
  echo "$RUN"
  return $rc
}

echo "1. provision=true claude -> npm claude, HOME prepped, connect staged"
rm -f "$WORK/npm.log"
R=$(install_run claude) || fail "claude install exited non-zero"
have "npm install -g @anthropic-ai/claude-code" "$WORK/npm.log"
[ -d "$R/pool-home" ] || fail "pool HOME not created"
have "WT_CONNECT='true'" "$R/share/options.env"
[ -x "$R/share/connect.sh" ] || fail "connect.sh not staged"
[ "$FAIL" -eq 0 ] && pass "claude provisioned + connect staged"

echo "2. version pins the npm tag"
rm -f "$WORK/npm.log"
R=$(install_run opencode VERSION=1.2.3) || fail "opencode@1.2.3 exited non-zero"
have "npm install -g opencode-ai@1.2.3" "$WORK/npm.log"
pass "version pin"

echo "3. provision=false -> no npm, but HOME + connect still staged"
rm -f "$WORK/npm.log"
R=$(install_run claude PROVISION=false) || fail "provision=false exited non-zero"
[ -s "$WORK/npm.log" ] && fail "npm should not run when provision=false" || true
[ -d "$R/pool-home" ] || fail "pool HOME not created when provision=false"
[ -x "$R/share/connect.sh" ] || fail "connect.sh not staged when provision=false"
pass "provision=false skips binary, keeps HOME + connect"

echo "4. connect=false -> options.env records it (connect.sh will self-skip)"
R=$(install_run claude CONNECT=false) || fail "connect=false exited non-zero"
have "WT_CONNECT='false'" "$R/share/options.env"
pass "connect=false recorded"

echo "5. npm-harness without node -> loud failure"
# A PATH with coreutils (install.sh needs mkdir/chmod/dirname up top) but no npm,
# so the node guard fires rather than a spurious 'mkdir: not found'.
SH="$(command -v sh)"
NONPM="$WORK/nonpmbin"
mkdir -p "$NONPM"
for c in sh dirname mkdir chmod cat cp; do ln -sf "$(command -v "$c")" "$NONPM/$c"; done
if env -i HARNESS=claude WT_POOL_HOME="$WORK/ph5" WT_SHARE_DIR="$WORK/sh5" \
     PATH="$NONPM" HOME="$WORK/h5" "$SH" "$INSTALL" >"$WORK/out5" 2>&1; then
  fail "claude without npm should have failed"
else
  have "no node/npm in the image" "$WORK/out5"
  pass "node-absent guard"
fi

echo "6. gemini provision -> no automated installer, fails clearly"
if install_run gemini >/dev/null 2>&1; then
  fail "gemini provision should fail"
else
  R=$(install_run gemini || true)
  have "no automated installer" "$R/out"
  pass "gemini guard"
fi

echo "7. connect.sh self-skips when WT_CONNECT=false"
OUT="$WORK/mcp7.json"
env -i WT_SHARE_DIR="$WORK/none7" WT_CONNECT=false WT_CONFIG_OUT="$OUT" \
  PATH="$STUBPATH" HOME="$WORK" PWD="$WORK" sh "$CONNECT" >"$WORK/c7" 2>&1
[ -f "$OUT" ] && fail "connect.sh wrote config despite WT_CONNECT=false" || true
have "connect disabled" "$WORK/c7"
pass "connect self-skip"

echo "8. connect.sh writes wiring when enabled (gateway + token + cwd)"
ROUTE="$WORK/route"
printf '%s\n' \
  'Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT' \
  'eth0	00000000	010012AC	0003	0	0	0	00000000	0	0	0' > "$ROUTE"
OUT8="$WORK/mcp8.json"
env -i WT_ROUTE_FILE="$ROUTE" WT_SHARE_DIR="$WORK/none8" WT_CONNECT=true \
  WT_CONFIG_OUT="$OUT8" WRAPTOOL_TOKEN=tok WRAPTOOL_CWD=/home/x \
  PATH="$STUBPATH" HOME="$WORK" PWD="$WORK" sh "$CONNECT" >/dev/null 2>&1
have 'http://172.18.0.1:8717/sse?cwd=/home/x' "$OUT8"
have 'Bearer tok' "$OUT8"
pass "connect wiring rendered"

if [ "$FAIL" -ne 0 ]; then
  echo "wraptool selftest: $FAIL failure(s)" >&2
  exit 1
fi
echo "wraptool selftest: all passed"
