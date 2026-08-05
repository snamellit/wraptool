#!/bin/sh
# Standalone test for the wraptool feature's wiring-only major (see
# design/harness/unified-harness-pool.md §12.2/§12.3) — runs without the
# devcontainer CLI, docker, or a real network. Covers:
#   devcontainer-feature.json: no provision/version options; containerEnv pins
#                              HOME and appends the pool runtime bin to PATH
#                              (after ${containerEnv:PATH}, never prepended)
#   install.sh: HOME prep, connect.sh staging — and that it never shells out to
#               npm/curl (this major installs no harness binary)
#   connect.sh: the CONNECT self-skip and the wiring output (gateway/token/cwd)
# Exit non-zero on failure.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SRC="$HERE/../../src/wraptool"
FEATURE_JSON="$SRC/devcontainer-feature.json"
INSTALL="$SRC/install.sh"
CONNECT="$SRC/connect.sh"
WORK="$(mktemp -d)"
FAIL=0

trap 'rm -rf "$WORK"' EXIT
pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
have() { grep -qF "$1" "$2" || fail "expected '$1' in $2"; }
absent() { grep -qF "$1" "$2" && fail "did not expect '$1' in $2" || true; }

echo "0. devcontainer-feature.json: wiring-only contract"
absent '"provision": {' "$FEATURE_JSON"
absent '"version": {' "$FEATURE_JSON"
have '"HOME": "/home/wraptool-harness"' "$FEATURE_JSON"
have '"PATH": "${containerEnv:PATH}:/opt/wraptool-harness/runtime/bin"' "$FEATURE_JSON"
[ "$FAIL" -eq 0 ] && pass "no provision/version options; HOME pinned; PATH appended after \${containerEnv:PATH}"

# Fake bin dir: npm/curl stubs that record args instead of acting — install.sh
# must never invoke them (no binary provisioning in the wiring-only major).
STUB="$WORK/bin"
mkdir -p "$STUB"
cat > "$STUB/npm" <<EOF
#!/usr/bin/env sh
echo "npm \$*" >> "$WORK/npm.log"
EOF
cat > "$STUB/curl" <<EOF
#!/usr/bin/env sh
echo "curl \$*" >> "$WORK/curl.log"
EOF
chmod +x "$STUB/npm" "$STUB/curl"
# Prepend the stubs to the REAL PATH (coreutils live in /gnu/store on guix, not
# /usr/bin), so install.sh/connect.sh find mkdir/sed/etc.; the stubs would win
# for npm/curl if install.sh ever called them (it must not).
STUBPATH="$STUB:$PATH"

# install <harness> [extra env...] — run install.sh with per-run pool/share/home.
install_run() {
  h="$1"; shift
  RUN="$WORK/run.$$.$h"; rm -rf "$RUN"; mkdir -p "$RUN"
  env -i HARNESS="$h" \
    WT_POOL_HOME="$RUN/pool-home" WT_SHARE_DIR="$RUN/share" \
    PATH="$STUBPATH" HOME="$RUN/home" "$@" \
    sh "$INSTALL" >"$RUN/out" 2>&1
  rc=$?
  echo "$RUN"
  return $rc
}

echo "1. install.sh: HOME prepped, connect staged, no binary installed (every harness)"
for h in claude opencode pi agy gemini; do
  R=$(install_run "$h") || fail "$h install exited non-zero"
  [ -d "$R/pool-home" ] || fail "$h: pool HOME not created"
  have "WT_HARNESS='$h'" "$R/share/options.env"
  have "WT_CONNECT='true'" "$R/share/options.env"
  [ -x "$R/share/connect.sh" ] || fail "$h: connect.sh not staged"
done
[ -s "$WORK/npm.log" ] && fail "install.sh invoked npm — this major must not install a binary" || true
[ -s "$WORK/curl.log" ] && fail "install.sh invoked curl — this major must not install a binary" || true
[ "$FAIL" -eq 0 ] && pass "HOME/connect staged for every harness; no npm/curl invocation"

echo "2. connect=false -> options.env records it (connect.sh will self-skip)"
R=$(install_run claude CONNECT=false) || fail "connect=false exited non-zero"
have "WT_CONNECT='false'" "$R/share/options.env"
pass "connect=false recorded"

echo "3. connect.sh self-skips when WT_CONNECT=false"
OUT="$WORK/mcp3.json"
env -i WT_SHARE_DIR="$WORK/none3" WT_CONNECT=false WT_CONFIG_OUT="$OUT" \
  PATH="$STUBPATH" HOME="$WORK" PWD="$WORK" sh "$CONNECT" >"$WORK/c3" 2>&1
[ -f "$OUT" ] && fail "connect.sh wrote config despite WT_CONNECT=false" || true
have "connect disabled" "$WORK/c3"
pass "connect self-skip"

echo "4. connect.sh writes wiring when enabled (gateway + token + cwd)"
ROUTE="$WORK/route"
printf '%s\n' \
  'Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT' \
  'eth0	00000000	010012AC	0003	0	0	0	00000000	0	0	0' > "$ROUTE"
OUT4="$WORK/mcp4.json"
env -i WT_ROUTE_FILE="$ROUTE" WT_SHARE_DIR="$WORK/none4" WT_CONNECT=true \
  WT_CONFIG_OUT="$OUT4" WRAPTOOL_TOKEN=tok WRAPTOOL_CWD=/home/x \
  PATH="$STUBPATH" HOME="$WORK" PWD="$WORK" sh "$CONNECT" >/dev/null 2>&1
have 'http://172.18.0.1:8717/sse?cwd=/home/x' "$OUT4"
have 'Bearer tok' "$OUT4"
pass "connect wiring rendered"

if [ "$FAIL" -ne 0 ]; then
  echo "wraptool selftest: $FAIL failure(s)" >&2
  exit 1
fi
echo "wraptool selftest: all passed"
