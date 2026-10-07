#!/usr/bin/env bash
# S6 entry checks for the Docker Sandboxes runtime
# (design/harness/docker-sandbox-runtime.md §17, "S6 entry checks").
#
#   ./test-sbx-entry.sh [--wraptool] <sandbox-name>
#
# Part A drives `sbx exec` directly; it decides the S6 gate. --wraptool adds
# part B, which goes through `wraptool up --runtime=sbx` and must be run from
# the root of the (scratch) repository that owns <sandbox-name>. Never run it
# from the wraptool checkout hosting your session: it stops the sandbox.
#
# Linux only (util-linux script(1), coreutils env --default-signal).
#
# Interactive cases run under a pseudo-terminal from script(1); resizes are
# applied to that pty with `stty -F`, which makes the kernel send SIGWINCH to
# the sbx client exactly as a terminal window resize would. A8's `less`
# redraw, B3 and B5 stay manual.
#
# Exit status: 0 when every gate case (A1 A2 A5 A6 A7 A8) passed, 1 otherwise.
# Raw transcripts are kept in $OUT (default: a fresh temp dir) for §14.

set -u

# Every non-interactive sbx call reads </dev/null: timeout(1) runs its child
# in its own process group, so a child that touches the terminal is stopped
# by SIGTTIN and hangs the run.

usage() {
  echo "usage: $0 [--wraptool] <sandbox-name>" >&2
  exit 2
}

WITH_WRAPTOOL=0
while [ $# -gt 0 ]; do
  case $1 in
    --wraptool) WITH_WRAPTOOL=1; shift ;;
    -h|--help) usage ;;
    -*) usage ;;
    *) break ;;
  esac
done
[ $# -eq 1 ] || usage
NAME=$1
WRAPTOOL_BIN=${WRAPTOOL_BIN:-wraptool}
OUT=${OUT:-$(mktemp -d -t sbx-s6.XXXXXX)}
mkdir -p "$OUT"

# The sbx client that starts a sandbox must not carry an agent (D9, R14).
unset SSH_AUTH_SOCK
# The pty sessions need a real terminal type even when this runs headless.
case ${TERM:-dumb} in dumb|"") export TERM=xterm-256color ;; esac

for t in sbx script stty timeout ps; do
  command -v "$t" >/dev/null 2>&1 || { echo "missing tool: $t" >&2; exit 2; }
done
if ! timeout -k 10 60 sbx ls --quiet </dev/null 2>/dev/null | grep -qx -- "$NAME"; then
  echo "sandbox $NAME not found in \`sbx ls --quiet\`" >&2
  exit 2
fi

GATE="A1 A2 A5 A6 A7 A8"
gate_failed=0

result() { # id status message
  printf '%-3s %-6s %s\n' "$1" "$2" "$3"
  if [ "$2" = FAIL ] && [[ " $GATE " == *" $1 "* ]]; then
    gate_failed=1
  fi
}

cold() {
  timeout -k 10 120 sbx stop "$NAME" </dev/null >/dev/null 2>&1 || true
}

wait_for() { # file fixed-string timeout-seconds
  local i=0
  while [ "$i" -lt $(($3 * 10)) ]; do
    grep -qF -- "$2" "$1" 2>/dev/null && return 0
    sleep 0.1
    i=$((i + 1))
  done
  return 1
}

# pty_start ID SHELL-COMMAND: run the command under a pty, typed input via
# fd 3. Sets PTY_PID and PTY_LOG.
pty_start() {
  PTY_LOG=$OUT/$1.log
  local fifo=$OUT/$1.in
  rm -f "$fifo"
  mkfifo "$fifo"
  exec 3<>"$fifo"
  # A non-interactive shell starts background jobs with SIGINT ignored, and
  # that would carry into sbx and hide what A8 tests: restore the default.
  env --default-signal=INT,QUIT script -qfec "$2" /dev/null <"$fifo" >"$PTY_LOG" 2>&1 &
  PTY_PID=$!
}

# pty_tty: the slave pty of the running session, e.g. /dev/pts/7.
pty_tty() {
  local child tty i=0
  while [ "$i" -lt 50 ]; do
    child=$(ps -o pid= --ppid "$PTY_PID" 2>/dev/null | head -1 | tr -d ' ')
    if [ -n "$child" ]; then
      tty=$(ps -o tty= -p "$child" 2>/dev/null | tr -d ' ')
      case $tty in pts/*) echo "/dev/$tty"; return 0 ;; esac
    fi
    sleep 0.1
    i=$((i + 1))
  done
  return 1
}

pty_send() { printf '%b' "$1" >&3; }

# pty_wait TIMEOUT: wait for the session, kill it on timeout. Sets PTY_RC
# (124 on timeout, like timeout(1)).
pty_wait() {
  local i=0
  while kill -0 "$PTY_PID" 2>/dev/null && [ "$i" -lt $(($1 * 10)) ]; do
    sleep 0.1
    i=$((i + 1))
  done
  if kill -0 "$PTY_PID" 2>/dev/null; then
    kill "$PTY_PID" 2>/dev/null
    wait "$PTY_PID" 2>/dev/null
    PTY_RC=124
  else
    wait "$PTY_PID"
    PTY_RC=$?
  fi
  exec 3>&-
}

echo "sandbox: $NAME   transcripts: $OUT"
echo "sbx: $(timeout -k 10 30 sbx version </dev/null 2>&1 | head -1)"
echo

# --- A. raw sbx exec ---------------------------------------------------------

cold
timeout -k 10 180 sbx exec -i "$NAME" bash -lc 'exit 3' </dev/null >"$OUT/A1.log" 2>&1
rc=$?
[ $rc -eq 3 ] && result A1 PASS "cold -i exit 3 -> 3" || result A1 FAIL "cold -i exit 3 -> $rc"

cold
pty_start A2 "exec sbx exec -it $NAME bash -lc 'exit 3'"
pty_wait 180
[ $PTY_RC -eq 3 ] && result A2 PASS "cold -it exit 3 -> 3" || result A2 FAIL "cold -it exit 3 -> $PTY_RC"

pty_start A3 "exec sbx exec -it $NAME bash -lc 'kill -TERM \$\$'"
pty_wait 60
[ $PTY_RC -eq 143 ] && result A3 PASS "SIGTERM -> 143" || result A3 INFO "SIGTERM -> $PTY_RC (expected 143)"

cold
timeout -k 10 180 sbx exec -i "$NAME" bash -lc 'echo out; echo err >&2' </dev/null \
  >"$OUT/A4.stdout" 2>"$OUT/A4.stderr"
if [ "$(cat "$OUT/A4.stdout")" = out ]; then
  result A4 PASS "cold start keeps stdout clean"
else
  result A4 INFO "stdout carries more than the command's output, see A4.stdout"
fi

got=$(printf 'hi\n' | timeout -k 10 60 sbx exec -i "$NAME" cat 2>"$OUT/A5.stderr")
rc=$?
if [ $rc -eq 0 ] && [ "$got" = hi ]; then
  result A5 PASS "stdin EOF ends the command"
else
  result A5 FAIL "rc=$rc output=$(printf %q "$got") (124 = hung on EOF)"
fi

pty_start A6 "stty rows 30 cols 100; exec sbx exec -it $NAME bash -lc 'tty; stty size; echo TERM=\$TERM'"
pty_wait 60
# Match anywhere in a line: on sbx 0.47.0 the tty and size lines run
# together ("/dev/pts/030 100").
term=$(grep -o 'TERM=[^[:space:]]*' "$PTY_LOG" | head -1)
if grep -q '/dev/pts/[0-9]' "$PTY_LOG" && grep -qE '30 100([^0-9]|$)' "$PTY_LOG" \
  && [ -n "${term#TERM=}" ] && [ "$term" != TERM=dumb ]; then
  result A6 PASS "pty, 30x100 carried over, $term"
else
  result A6 FAIL "see $PTY_LOG"
fi

pty_start A7 "stty rows 30 cols 100; exec sbx exec -it $NAME bash -lc 'trap \"echo size=\\\$(stty size)\" WINCH; echo ready; while :; do sleep 0.2; done'"
a7=FAIL
if wait_for "$PTY_LOG" ready 60 && tty=$(pty_tty); then
  stty -F "$tty" rows 40 cols 120
  if wait_for "$PTY_LOG" "size=40 120" 10; then
    stty -F "$tty" rows 25 cols 90
    wait_for "$PTY_LOG" "size=25 90" 10 && a7=PASS
  fi
fi
pty_send '\003'
pty_wait 15
[ $a7 = PASS ] && result A7 PASS "two resizes reached the sandbox" || result A7 FAIL "see $PTY_LOG"

pty_start A8 "exec sbx exec -it $NAME bash -lc 'trap \"echo trapped\" INT; echo ready; sleep 4242; echo sleep-rc=\$?'"
if wait_for "$PTY_LOG" ready 60; then
  sleep 1
  pty_send '\003'
fi
pty_wait 15
left=$(timeout -k 10 60 sbx exec "$NAME" bash -c \
  'for f in /proc/[0-9]*/cmdline; do tr "\0" " " <"$f"; echo; done 2>/dev/null | grep -c "^sleep 4242"' </dev/null)
if grep -q 'sleep-rc=130' "$PTY_LOG" && [ $PTY_RC -eq 0 ] && [ "${left:-x}" = 0 ]; then
  result A8 PASS "Ctrl-C reached the sandbox, shell survived, nothing left"
else
  result A8 FAIL "session rc=$PTY_RC, leftover sleeps=${left:-?}, see $PTY_LOG"
fi
result A8 MANUAL "\`less\` redraw on resize"

# --- B. through wraptool up ----------------------------------------------------

if [ $WITH_WRAPTOOL -eq 1 ]; then
  echo
  # wt_session ID EXIT-COMMAND: open the wraptool shell cold, wait until it
  # answers, then type EXIT-COMMAND. Sets PTY_RC.
  wt_session() {
    cold
    pty_start "$1" "exec $WRAPTOOL_BIN up --runtime=sbx --shell=always"
    if wait_for "$PTY_LOG" "opening a shell in sandbox" 300; then
      # Typed text is echoed back, so wait for the computed marker instead.
      local i=0
      while [ "$i" -lt 30 ] && ! grep -q 'wt-2' "$PTY_LOG"; do
        pty_send 'echo wt-$((1+1))\n'
        wait_for "$PTY_LOG" wt-2 2 && break
        i=$((i + 1))
      done
      pty_send "$2\n"
    fi
    pty_wait 60
  }

  wt_session B1 exit
  [ $PTY_RC -eq 0 ] && result B1 PASS "exit -> 0" || result B1 FAIL "exit -> $PTY_RC, see $PTY_LOG"

  wt_session B2 'exit 3'
  case $PTY_RC in
    3) result B2 PASS "exit 3 -> 3" ;;
    1) result B2 KNOWN "exit 3 -> 1 (exit status not propagated, all runtimes)" ;;
    *) result B2 FAIL "exit 3 -> $PTY_RC, see $PTY_LOG" ;;
  esac

  cold
  timeout -k 10 300 "$WRAPTOOL_BIN" up --runtime=sbx --shell=always </dev/null >"$OUT/B4.log" 2>&1
  rc=$?
  [ $rc -eq 0 ] && result B4 PASS "non-tty stdin -> 0" || result B4 FAIL "rc=$rc (124 = hung), see $OUT/B4.log"

  result B3 MANUAL "resize + Ctrl-C in claude inside \`wraptool up\`"
  result B5 MANUAL "close the window, then look for leftover claude processes"
fi

echo
if [ $gate_failed -eq 0 ]; then
  echo "S6 gate: PASS"
else
  echo "S6 gate: FAIL"
fi
exit $gate_failed
