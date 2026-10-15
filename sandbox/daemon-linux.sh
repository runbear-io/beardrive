#!/bin/bash
# The two daemon checks that need a Linux machine. Everything else about the
# daemon (lock-vs-pid liveness, resume idempotency, stay-stopped) is covered by
# internal/daemon and internal/webapp/cli_e2e_test.go — don't re-test it here.
#
#   ./sandbox/run.sh daemon-linux
set -u

pass() { echo "  PASS  $1"; }
fail() { echo "  FAIL  $1"; FAILED=$((FAILED+1)); }
info() { echo "  ..    $1"; }
FAILED=0

bdrive-signin >/dev/null || exit 1

echo "== the systemd user unit =="
# Only reachable on Linux, and only assertable at all because autostart.Install
# never shells out to systemctl — writing the files IS the registration, so
# faking sd_booted(3)'s directory probe exercises the real path.
UNITDIR="$HOME/.config/systemd/user"
S=/work/unit; rm -rf "$S"; mkdir -p "$S"; cd "$S"

if [ -e /run/systemd/system ]; then
  info "this container runs systemd; skipping the sd_booted fake"
else
  rm -rf "$UNITDIR"
  bdrive init --name unittest --yes >/dev/null 2>&1
  [ -f "$UNITDIR/beardrive.service" ] \
    && fail "wrote a unit on a machine with no systemd" \
    || pass "no /run/systemd/system -> writes no unit, quietly"
  mkdir -p /run/systemd/system
fi

bdrive init --yes >/dev/null 2>&1
if [ -f "$UNITDIR/beardrive.service" ]; then
  pass "unit written to $UNITDIR/beardrive.service"
  grep -q 'bdrive resume' "$UNITDIR/beardrive.service" \
    && pass "unit runs 'bdrive resume'" || fail "unit does not run 'bdrive resume'"
  [ -L "$UNITDIR/default.target.wants/beardrive.service" ] \
    && pass "enabled via default.target.wants" \
    || fail "no default.target.wants symlink — systemd would ignore the unit"
else
  fail "no unit written even with /run/systemd/system present"
fi
rmdir /run/systemd/system 2>/dev/null

echo
echo "== daemon.pid must name the lock holder =="
# A single foreground supervisor owns every volume lock. Its command line
# never names a folder; each volume's pidfile must name that same lock holder.
live_daemons() {
  for p in /proc/[0-9]*; do
    c=$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null)
    case "$c" in *"bdrive daemon run "*) echo "${p#/proc/}" ;; esac
  done
}

S=/work/race; rm -rf "$S"; mkdir -p "$S"; cd "$S"
bdrive init --name racetest --yes >/dev/null 2>&1
bdrive resume >/dev/null 2>&1          # no delay: that is the test
sleep 3

MOUNT=$(node -e 'console.log(require("/work/race/.bdrive/config.json").id)')
PIDFILE="$HOME/.bdrive/volumes/$MOUNT/daemon.pid"
info "mount $MOUNT"
FILEPID=$(cat "$PIDFILE" 2>/dev/null)
GLOBALPID=$(cat "$HOME/.bdrive/daemon.pid" 2>/dev/null)
UNITMOUNT=$(node -e 'console.log(require("/work/unit/.bdrive/config.json").id)')
UNITPIDFILE="$HOME/.bdrive/volumes/$UNITMOUNT/daemon.pid"
UNITPID=$(cat "$UNITPIDFILE" 2>/dev/null)
LIVE=$(live_daemons | tr '\n' ' ')
info "daemon.pid says ${FILEPID:-<empty>}; actually alive: ${LIVE:-<none>}"
case " $LIVE " in
  *" $FILEPID "*) pass "daemon.pid names a live daemon" ;;
  *)              fail "daemon.pid names $FILEPID, which is not running" ;;
esac
[ -n "$FILEPID" ] && [ "$FILEPID" = "$GLOBALPID" ] && [ "$UNITPID" = "$GLOBALPID" ] \
  && pass "two projects share the global daemon PID $GLOBALPID" \
  || fail "project PIDs ($FILEPID, $UNITPID) differ from global PID $GLOBALPID"
COUNT=$(live_daemons | wc -l)
[ "$COUNT" -eq 1 ] \
  && pass "exactly one global bdrive daemon run process" \
  || fail "expected one global daemon, found $COUNT: $LIVE"

STOPOUT=$(bdrive stop 2>&1)
sleep 2
STILL=$(live_daemons | tr '\n' ' ')
case "$STOPOUT" in
  *"no such process"*) fail "stop failed: $STOPOUT" ;;
  *)                   pass "stop reported no error" ;;
esac
! [ -f "$PIDFILE" ] \
  && pass "/work/race is no longer syncing after stop" \
  || fail "stop left /work/race's daemon.pid behind"
[ -f "$UNITPIDFILE" ] && [ "$(cat "$UNITPIDFILE")" = "$GLOBALPID" ] \
  && pass "/work/unit is still syncing under the same global PID" \
  || fail "stopping /work/race also stopped /work/unit"
[ "$(live_daemons | wc -l)" -eq 1 ] \
  && pass "the shared supervisor stays alive for /work/unit" \
  || fail "expected one supervisor after stopping /work/race: $STILL"
UNITSTATUS=$(bdrive status /work/unit 2>&1)
case "$UNITSTATUS" in
  *"daemon:   running (pid $GLOBALPID)"*) pass "/work/unit reports running" ;;
  *) fail "/work/unit is not running: $UNITSTATUS" ;;
esac
bdrive stop /work/unit >/dev/null 2>&1 \
  || fail "could not stop /work/unit"
sleep 2
STILL=$(live_daemons | tr '\n' ' ')
[ -z "$STILL" ] \
  && pass "no bdrive daemon remains after stopping the final project" \
  || fail "final stop left global daemon(s) running: $STILL"

echo
echo "== done: $FAILED failure(s) =="
exit $((FAILED > 0))
