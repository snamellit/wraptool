#!/usr/bin/env bash
# §13 capability tests for design/harness/acp-bridge.md.
#
# Establishes the one empirical claim ruling R1 rests on: that the stdio path
# into the container is transparent enough for a protocol wraptool knows
# nothing about (ACP) to survive it.
#
# These tests are protocol-free. No editor, no ACP harness, no wraptool binary
# is required for T1/T3/T4/T5 — they exercise `guix shell --container` directly,
# using the argv `buildGuixShellArgs` (cmd/guixcontainer.go:618) actually
# produces. Keep the ARGV BLOCK below byte-identical to that function's output;
# if it drifts, a pass here proves nothing about wraptool.
#
# Run from a project worktree that has a manifest.scm:
#     ./design/harness/acp-capability-test.sh
#
# T2b and T6 need extra inputs and are skipped unless provided:
#     WRAPTOOL_BIN=/path/to/wraptool  ./design/harness/acp-capability-test.sh
#     RUN_T6=1                        ./design/harness/acp-capability-test.sh

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MANIFEST="$ROOT/manifest.scm"
POOL_HOME="${XDG_DATA_HOME:-$HOME/.local/share}/wraptool/harness-pool/home"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0 fail=0 skip=0
ok()   { printf '  PASS  %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  FAIL  %s\n' "$1"; fail=$((fail+1)); }
skipt(){ printf '  SKIP  %s (%s)\n' "$1" "$2"; skip=$((skip+1)); }

command -v guix >/dev/null || { echo "guix not on PATH — run this on the host, not inside a container"; exit 2; }
[ -f "$MANIFEST" ] || { echo "no manifest.scm at $MANIFEST"; exit 2; }
mkdir -p "$POOL_HOME"

# --- ARGV BLOCK — mirrors cmd/guixcontainer.go:597-633 -----------------------
# buildContainerEntry, with `exec bash -l` replaced by the argv under test.
container_entry() {
  local argv="$1"
  printf 'export WRAPTOOL_CONTAINER=1; export TERM=xterm-256color; export PATH=$PATH; export HOME=%q; cd %q; exec %s' \
    "$POOL_HOME" "$ROOT" "$argv"
}
# buildGuixShellArgs. $1 = entry string. Login shell (-lc) as shipped today;
# T1b re-runs with -c to answer §14.2's "can the login shell be dropped".
guix_run() {
  local entry="$1" shellflag="${2:--lc}"
  guix shell --container --network --emulate-fhs \
    --share="$ROOT=$ROOT" \
    --share="$POOL_HOME=$POOL_HOME" \
    --preserve='^WRAPTOOL_' \
    --preserve='^COLORTERM$' \
    -m "$MANIFEST" \
    -- bash "$shellflag" "$entry"
}
# ----------------------------------------------------------------------------

echo
echo "T1 — byte transparency (stdout unmodified through guix shell + bash)"

# A payload no line-oriented or text-assuming layer survives intact:
# NUL bytes, CR without LF, a >64KiB burst (past one pipe buffer), high bytes,
# and a final line with no trailing newline.
{
  printf 'ACPSTART\n'
  printf 'nul[\000\000]cr[\r]tab[\t]esc[\033]\n'
  printf '\200\376\377 high bytes\n'
  head -c 200000 /dev/urandom | base64 | tr -d '\n'
  printf '\n'
  printf 'no-trailing-newline-END'
} > "$WORK/payload"

# `cat` the payload from inside the container; compare byte for byte.
guix_run "$(container_entry "cat $(printf '%q' "$WORK/payload")")" > "$WORK/got" 2>"$WORK/err"
if cmp -s "$WORK/payload" "$WORK/got"; then
  ok "T1a stdout is byte-identical under bash -lc"
else
  bad "T1a stdout altered — $(stat -c%s "$WORK/payload") sent, $(stat -c%s "$WORK/got" 2>/dev/null || echo 0) received"
  cmp "$WORK/payload" "$WORK/got" 2>&1 | head -3 | sed 's/^/        /'
  echo "        R1 IS NOT VIABLE AS WRITTEN — see §13 closing paragraph"
fi

guix_run "$(container_entry "cat $(printf '%q' "$WORK/payload")")" -c > "$WORK/got_c" 2>/dev/null
if cmp -s "$WORK/payload" "$WORK/got_c"; then
  ok "T1b byte-identical under bash -c (login shell droppable — §14.2)"
else
  bad "T1b differs under bash -c"
fi

echo
echo "T2a — stream separation (nothing but the child on stdout)"

# A container run whose program writes NOTHING must yield an empty stdout;
# guix progress, profile building and wraptool prose must all be on stderr.
guix_run "$(container_entry "true")" > "$WORK/quiet" 2>"$WORK/quiet_err"
if [ ! -s "$WORK/quiet" ]; then
  ok "T2a stdout empty when the child writes nothing ($(stat -c%s "$WORK/quiet_err") bytes on stderr)"
else
  bad "T2a $(stat -c%s "$WORK/quiet") bytes of non-child output on stdout"
  head -c 300 "$WORK/quiet" | sed 's/^/        /'
fi

if [ -n "${WRAPTOOL_BIN:-}" ]; then
  skipt "T2b wraptool's own 22 print sites" "needs the WP2 entry-argv flag, which does not exist yet"
else
  skipt "T2b wraptool's own 22 print sites" "set WRAPTOOL_BIN= to attempt; also blocked on WP1"
fi

echo
echo "T3 — bidirectional round-trip over pipes (not a TTY)"

# JSON-RPC needs stdin to reach the child promptly and the reply to come back
# without waiting for EOF. A redirect-to-file test cannot show this.
cat > "$WORK/echo.sh" <<'INNER'
while IFS= read -r line; do
  printf 'echo:%s\n' "$line"
done
INNER

t3_result="$(
  {
    exec 3>&1
    (
      for i in 1 2 3; do
        printf 'ping-%d\n' "$i"
        sleep 0.3   # if the child buffers, replies arrive late or not at all
      done
      sleep 1
    ) | timeout 180 guix_run "$(container_entry "bash $(printf '%q' "$WORK/echo.sh")")" 2>/dev/null
  }
)"
got_lines="$(printf '%s' "$t3_result" | grep -c '^echo:ping-' || true)"
if [ "$got_lines" -eq 3 ]; then
  ok "T3 all 3 lines round-tripped through pipes"
else
  bad "T3 $got_lines/3 lines round-tripped — stdin or stdout is buffered"
  echo "        R1 IS NOT VIABLE AS WRITTEN — see §13 closing paragraph"
fi

echo
echo "T4 — no TTY required"

# Everything above already ran with pipes on both ends (this is a script, not a
# terminal). Assert the child agrees, so a later `isatty` gate cannot creep in.
tty_state="$(guix_run "$(container_entry 'bash -c "test -t 0 && echo TTY || echo PIPE"')" 2>/dev/null | tr -d '\r\n')"
if [ "$tty_state" = "PIPE" ]; then
  ok "T4 child sees a pipe on stdin, and the run succeeded"
else
  bad "T4 unexpected stdin state: '$tty_state'"
fi

echo
echo "T5 — lifetime and teardown"

# An editor closing the connection is the normal case. Nothing may survive it.
marker="wraptool-acp-t5-$$"
guix_run "$(container_entry "bash -c 'exec -a $marker sleep 300'")" >/dev/null 2>&1 &
parent=$!
sleep 8
kill -TERM "$parent" 2>/dev/null
wait "$parent" 2>/dev/null
sleep 3
if pgrep -f "$marker" >/dev/null 2>&1; then
  bad "T5 orphaned process survived parent death — WP1 needs process-group teardown (§7.11)"
  pgrep -af "$marker" | head -3 | sed 's/^/        /'
  pkill -f "$marker" 2>/dev/null
else
  ok "T5 no orphan after the parent was killed"
fi

echo
echo "T6 — Dev Container exec transparency"
if [ "${RUN_T6:-0}" != "1" ]; then
  skipt "T6" "set RUN_T6=1; needs docker + devcontainer CLI and a running container"
elif ! command -v devcontainer >/dev/null; then
  skipt "T6" "devcontainer CLI not on PATH"
else
  devcontainer exec --workspace-folder "$ROOT" cat "$WORK/payload" > "$WORK/dc" 2>/dev/null
  if cmp -s "$WORK/payload" "$WORK/dc"; then
    ok "T6 devcontainer exec is byte-transparent"
  else
    bad "T6 devcontainer exec altered the stream — Dev Container path goes to 'docker exec -i' (§14.2)"
  fi
fi

echo
printf 'pass %d  fail %d  skip %d\n' "$pass" "$fail" "$skip"
[ "$fail" -eq 0 ]
