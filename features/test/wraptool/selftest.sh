#!/bin/sh
# Standalone test for the wraptool feature's wiring-only major (see
# design/harness/unified-harness-pool.md §12.2/§12.3) — runs without the
# devcontainer CLI, docker, or a real network. Covers:
#   devcontainer-feature.json: valid JSON, no provision/version options
#                              (structural, via node -e — not formatting-
#                              sensitive grep; node, not jq, because the
#                              publish runner already has it for free — it's
#                              how @devcontainers/cli itself runs), containerEnv.PATH
#                              appends the pool runtime bin, and never uses the
#                              devcontainer-CLI ${containerEnv:...} substitution
#                              form (a Feature's containerEnv is a literal
#                              Dockerfile ENV — that form isn't valid there and
#                              breaks every consumer's image build)
#   install.sh: HOME prep, connect.sh staging — and that it never shells out to
#               npm/curl (this major installs no harness binary)
#   connect.sh: the "$WT_HARNESS resolves on PATH" postCreate check — it always
#               RUNS (regardless of WT_CONNECT, §12.3) but its SEVERITY follows
#               intent (connect enabled -> hard fail, connect disabled -> warn
#               and continue) — the WT_CONNECT self-skip, and the wiring output
#               (gateway/token/cwd)
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

if ! command -v node >/dev/null 2>&1; then
  echo "wraptool selftest: node is required" >&2
  exit 1
fi

echo "0. devcontainer-feature.json: structural, wiring-only contract"
if node -e 'JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"))' \
     "$FEATURE_JSON" >"$WORK/nodeerr" 2>&1; then
  pass "valid JSON"
else
  fail "devcontainer-feature.json is not valid JSON: $(cat "$WORK/nodeerr")"
fi
GOT_KEYS=$(node -e '
    var f = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
    process.stdout.write(JSON.stringify(Object.keys(f.options).sort()));
  ' "$FEATURE_JSON" 2>/dev/null || echo "<node error>")
WANT_KEYS='["connect","harness","port","url"]'
[ "$GOT_KEYS" = "$WANT_KEYS" ] || fail "options keys are $GOT_KEYS, want $WANT_KEYS (no provision/version)"
PATH_VAL=$(node -e '
    var f = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
    process.stdout.write(f.containerEnv.PATH);
  ' "$FEATURE_JSON" 2>/dev/null || echo "<node error>")
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

# A curated PATH for the "absent" connect.sh scenarios below: everything
# connect.sh's full wiring path needs (sh, cut, sed, mkdir, dirname) EXCEPT a
# "claude" (or any other harness) binary. The REAL $PATH cannot be trusted to
# lack "claude" (this test may itself be running under a claude-code harness),
# so it is excluded entirely; symlinking specific tool names in, rather than
# falling back to $PATH, guarantees the harness stays absent regardless of
# what the real PATH contains. It is deliberately rich enough (not just `sh`)
# that a broken severity gate can't hide behind an unrelated "coreutil not
# found" crash: if the postCreate check wrongly lets an absent-harness run
# continue, the rest of connect.sh can run all the way through and actually
# succeed — which is exactly the false negative the mutation check below must
# catch as a real exit 0, not an accidental failure for the wrong reason.
ABSENTBIN="$WORK/absentbin"
mkdir -p "$ABSENTBIN"
for c in sh cut sed mkdir dirname; do
  ln -s "$(command -v "$c")" "$ABSENTBIN/$c"
done
ABSENTPATH="$ABSENTBIN"
ABSENTROUTE="$WORK/absentroute"
printf '%s\n' \
  'Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT' \
  'eth0	00000000	010012AC	0003	0	0	0	00000000	0	0	0' > "$ABSENTROUTE"

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

echo "3. connect.sh: harness absent + connect=true -> ERROR, exit non-zero, names harness install"
OUT3="$WORK/mcp3.json"
if env -i WT_ROUTE_FILE="$ABSENTROUTE" WT_SHARE_DIR="$WORK/none3" WT_CONNECT=true WT_HARNESS=claude \
     WT_CONFIG_OUT="$OUT3" PATH="$ABSENTPATH" HOME="$WORK" PWD="$WORK" \
     sh "$CONNECT" >"$WORK/c3" 2>&1; then
  fail "connect.sh should have failed with claude absent from PATH and connect=true"
else
  have "ERROR" "$WORK/c3"
  absent "WARNING" "$WORK/c3"
  have "not found on PATH" "$WORK/c3"
  have "wraptool harness install claude" "$WORK/c3"
  [ -f "$OUT3" ] && fail "connect.sh wrote a config despite the harness being absent"
fi
pass "absent-harness guard (connect enabled) hard-fails"

echo "4. connect.sh: harness absent + connect=false -> the check still RUNS, but only WARNS (exit 0)"
if env -i WT_ROUTE_FILE="$ABSENTROUTE" WT_SHARE_DIR="$WORK/none4" WT_CONNECT=false WT_HARNESS=claude \
     PATH="$ABSENTPATH" HOME="$WORK" PWD="$WORK" \
     sh "$CONNECT" >"$WORK/c4" 2>&1; then
  have "WARNING" "$WORK/c4"
  absent "ERROR" "$WORK/c4"
  have "not found on PATH" "$WORK/c4"
  have "wraptool harness install claude" "$WORK/c4"
  have "connect disabled" "$WORK/c4"
else
  fail "connect.sh should exit 0 with connect=false, even with the harness absent (warn, not fail)"
fi
pass "absent-harness check is not skipped by connect=false, but downgrades to a warning"

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
