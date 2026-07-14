#!/bin/sh
# Standalone test for the wraptool-harness feature's install.sh — runs without
# the devcontainer CLI, docker, or a real network. Stubs npm/curl on PATH and
# asserts the harness->package mapping, the node-absent guard, the shared-pool
# HOME setup, and the error cases. Exit non-zero on failure.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALL="$HERE/../../src/wraptool-harness/install.sh"
WORK="$(mktemp -d)"
FAIL=0

trap 'rm -rf "$WORK"' EXIT
pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
have() { grep -qF "$1" "$2" || fail "expected '$1' in $2"; }

# A fake bin dir with npm/curl/bash stubs that record their args instead of
# doing anything. install(1) (coreutils) is left real for the agy relocate.
STUB="$WORK/bin"
mkdir -p "$STUB"
cat > "$STUB/npm" <<EOF
#!/bin/sh
echo "npm \$*" >> "$WORK/npm.log"
EOF
cat > "$STUB/curl" <<EOF
#!/bin/sh
echo "curl \$*" >> "$WORK/curl.log"
# emit a no-op installer script for the piped 'bash'
echo 'mkdir -p "\$HOME/.local/bin"; printf "#!/bin/sh\n" > "\$HOME/.local/bin/agy"; chmod +x "\$HOME/.local/bin/agy"'
EOF
chmod +x "$STUB/npm" "$STUB/curl"
STUBPATH="$STUB:/usr/bin:/bin"

run() { # run <harness> [extra env assignments...]
  h="$1"; shift
  env -i HARNESS="$h" WT_POOL_HOME="$WORK/pool-home" PATH="$STUBPATH" HOME="$WORK/fakehome" "$@" \
    sh "$INSTALL"
}

echo "1. claude -> npm @anthropic-ai/claude-code, HOME mount prepared"
rm -f "$WORK/npm.log"
run claude >/dev/null 2>&1 || fail "claude install exited non-zero"
have "npm install -g @anthropic-ai/claude-code" "$WORK/npm.log"
[ -d "$WORK/pool-home" ] || fail "pool HOME dir not created"
[ "$FAIL" -eq 0 ] && pass "claude npm + pool HOME"

echo "2. version pins the npm tag"
rm -f "$WORK/npm.log"
run opencode VERSION=1.2.3 >/dev/null 2>&1 || fail "opencode@1.2.3 exited non-zero"
have "npm install -g opencode-ai@1.2.3" "$WORK/npm.log"
pass "version pin"

echo "3. pi -> npm @earendil-works/pi-coding-agent"
rm -f "$WORK/npm.log"
run pi >/dev/null 2>&1 || fail "pi install exited non-zero"
have "npm install -g @earendil-works/pi-coding-agent" "$WORK/npm.log"
pass "pi npm"

echo "4. npm-harness without node -> loud failure"
mkdir -p "$WORK/emptybin" # PATH with no npm/curl so the node guard fires
SH="$(command -v sh)"     # absolute: env can't resolve 'sh' from the empty PATH
if env -i HARNESS=claude WT_POOL_HOME="$WORK/ph4" PATH="$WORK/emptybin" HOME="$WORK/fh4" \
     "$SH" "$INSTALL" >"$WORK/out4" 2>&1; then
  fail "claude without npm should have failed"
else
  have "no node/npm in the image" "$WORK/out4"
  pass "node-absent guard"
fi

echo "5. agy -> curl installer, relocated to /usr/local/bin equivalent"
# agy relocates into /usr/local/bin (needs root); redirect via a writable prefix
# is not parameterized, so just assert curl ran and it attempted the install.
rm -f "$WORK/curl.log"
run agy >"$WORK/out5" 2>&1 || true # install(1) into /usr/local/bin may lack perms in CI
have "antigravity.google/cli/install.sh" "$WORK/curl.log"
pass "agy curl installer invoked"

echo "6. gemini -> no automated installer, fails clearly"
if run gemini >"$WORK/out6" 2>&1; then
  fail "gemini should have no automated installer"
else
  have "no automated installer" "$WORK/out6"
  pass "gemini guard"
fi

echo "7. unknown harness -> fails clearly"
if run bogus >"$WORK/out7" 2>&1; then
  fail "unknown harness should fail"
else
  have "unknown harness 'bogus'" "$WORK/out7"
  pass "unknown guard"
fi

if [ "$FAIL" -ne 0 ]; then
  echo "wraptool-harness selftest: $FAIL failure(s)" >&2
  exit 1
fi
echo "wraptool-harness selftest: all passed"
