#!/bin/sh
# Standalone test for the wraptool feature's wiring-only major (see
# design/harness/unified-harness-pool.md §12.2/§12.3) — runs without the
# devcontainer CLI, docker, or a real network. Covers:
#   devcontainer-feature.json: valid JSON, no provision/version options
#                              (structural, via jq — not formatting-sensitive
#                              grep), containerEnv.PATH appends the pool
#                              runtime bin, and never uses the devcontainer-CLI
#                              ${containerEnv:...} substitution form (a
#                              Feature's containerEnv is a literal Dockerfile
#                              ENV — that form isn't valid there and breaks
#                              every consumer's image build)
#   install.sh: HOME prep, connect.sh staging — and that it never shells out to
#               npm/curl (this major installs no harness binary)
#   connect.sh: the unconditional "$WT_HARNESS resolves on PATH" postCreate
#               check (runs regardless of WT_CONNECT, §12.3), the WT_CONNECT
#               self-skip, and the wiring output (gateway/token/cwd)
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

if ! command -v jq >/dev/null 2>&1; then
  echo "wraptool selftest: jq is required" >&2
  exit 1
fi

echo "0. devcontainer-feature.json: structural, wiring-only contract"
if jq . "$FEATURE_JSON" >"$WORK/feature.json" 2>"$WORK/jqerr"; then
  pass "valid JSON"
else
  fail "devcontainer-feature.json is not valid JSON: $(cat "$WORK/jqerr")"
fi
GOT_KEYS=$(jq -c '.options | keys' "$FEATURE_JSON" 2>/dev/null || echo "<jq error>")
WANT_KEYS='["connect","harness","port","url"]'
[ "$GOT_KEYS" = "$WANT_KEYS" ] || fail "options keys are $GOT_KEYS, want $WANT_KEYS (no provision/version)"
PATH_VAL=$(jq -r '.containerEnv.PATH' "$FEATURE_JSON" 2>/dev/null || echo "<jq error>")
case "$PATH_VAL" in
  *:/opt/wraptool-harness/runtime/bin) ;;
  *) fail "containerEnv.PATH '$PATH_VAL' does not end with :/opt/wraptool-harness/runtime/bin (append)" ;;
esac
absent '${containerEnv:' "$FEATURE_JSON"
[ "$FAIL" -eq 0 ] && pass "options are exactly connect/harness/port/url; PATH appended; no \${containerEnv:...} form"

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

# A second stub dir with a fake harness binary, for the "harness present"
# connect.sh scenarios — prepended ahead of STUBPATH only where used.
HSTUB="$WORK/hbin"
mkdir -p "$HSTUB"
cat > "$HSTUB/claude" <<'EOF'
#!/usr/bin/env sh
exit 0
EOF
chmod +x "$HSTUB/claude"
PRESENTPATH="$HSTUB:$STUBPATH"

# A deliberately harness-free PATH for the "absent" connect.sh scenarios below
# — the REAL $PATH cannot be trusted to lack "claude" (this test may itself be
# running under a claude-code harness), so it is excluded entirely. connect.sh's
# "$WT_HARNESS on PATH" check is the very first thing it does (only shell
# builtins before it), so the only external binary env(1) needs to exec is `sh`
# itself; symlink just that one in, same trick as the old node-absent guard test.
ABSENTBIN="$WORK/absentbin"
mkdir -p "$ABSENTBIN"
ln -s "$(command -v sh)" "$ABSENTBIN/sh"
ABSENTPATH="$ABSENTBIN"

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

echo "3. connect.sh: harness absent + connect=true -> fails, names harness install"
OUT3="$WORK/mcp3.json"
if env -i WT_SHARE_DIR="$WORK/none3" WT_CONNECT=true WT_HARNESS=claude \
     WT_CONFIG_OUT="$OUT3" PATH="$ABSENTPATH" HOME="$WORK" PWD="$WORK" \
     sh "$CONNECT" >"$WORK/c3" 2>&1; then
  fail "connect.sh should have failed with claude absent from PATH"
else
  have "not found on PATH" "$WORK/c3"
  have "wraptool harness install claude" "$WORK/c3"
  [ -f "$OUT3" ] && fail "connect.sh wrote a config despite the harness being absent"
fi
pass "absent-harness guard (connect enabled)"

echo "4. connect.sh: harness absent + connect=false -> STILL fails (check is unconditional)"
if env -i WT_SHARE_DIR="$WORK/none4" WT_CONNECT=false WT_HARNESS=claude \
     PATH="$ABSENTPATH" HOME="$WORK" PWD="$WORK" \
     sh "$CONNECT" >"$WORK/c4" 2>&1; then
  fail "connect.sh should have failed with claude absent from PATH even when connect=false"
else
  have "not found on PATH" "$WORK/c4"
  absent "connect disabled" "$WORK/c4"
fi
pass "absent-harness guard is not bypassed by connect=false"

echo "5. connect.sh: harness present + connect=false -> self-skip succeeds"
OUT5="$WORK/mcp5.json"
env -i WT_SHARE_DIR="$WORK/none5" WT_CONNECT=false WT_HARNESS=claude \
  WT_CONFIG_OUT="$OUT5" PATH="$PRESENTPATH" HOME="$WORK" PWD="$WORK" \
  sh "$CONNECT" >"$WORK/c5" 2>&1
[ -f "$OUT5" ] && fail "connect.sh wrote config despite WT_CONNECT=false" || true
have "connect disabled" "$WORK/c5"
pass "connect self-skip (harness present)"

echo "6. connect.sh: harness present + connect=true -> wiring rendered (gateway + token + cwd)"
ROUTE="$WORK/route"
printf '%s\n' \
  'Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT' \
  'eth0	00000000	010012AC	0003	0	0	0	00000000	0	0	0' > "$ROUTE"
OUT6="$WORK/mcp6.json"
env -i WT_ROUTE_FILE="$ROUTE" WT_SHARE_DIR="$WORK/none6" WT_CONNECT=true WT_HARNESS=claude \
  WT_CONFIG_OUT="$OUT6" WRAPTOOL_TOKEN=tok WRAPTOOL_CWD=/home/x \
  PATH="$PRESENTPATH" HOME="$WORK" PWD="$WORK" sh "$CONNECT" >/dev/null 2>&1
have 'http://172.18.0.1:8717/sse?cwd=/home/x' "$OUT6"
have 'Bearer tok' "$OUT6"
pass "connect wiring rendered"

if [ "$FAIL" -ne 0 ]; then
  echo "wraptool selftest: $FAIL failure(s)" >&2
  exit 1
fi
echo "wraptool selftest: all passed"
