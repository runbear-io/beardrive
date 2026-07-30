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
# TestCLIDaemonPidFileNamesTheLockHolder covers this too, but only catches it
# about one run in five on macOS — the window is tight there. On Linux it is
# deterministic, which is why this stays until the pidfile ordering is fixed.
# Scoped to ONE folder on purpose: the section above left its own mount and
# daemon running, and counting "any daemon" or picking "any daemon.pid" made
# this report the other project's process.
live_daemons() {  # live_daemons <folder>
  for p in /proc/[0-9]*; do
    c=$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null)
    case "$c" in *"bdrive daemon"*"$1"*) echo "${p#/proc/}" ;; esac
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
LIVE=$(live_daemons "$S" | tr '\n' ' ')
info "daemon.pid says ${FILEPID:-<empty>}; actually alive: ${LIVE:-<none>}"
case " $LIVE " in
  *" $FILEPID "*) pass "daemon.pid names a live daemon" ;;
  *)              fail "daemon.pid names $FILEPID, which is not running" ;;
esac

STOPOUT=$(bdrive stop 2>&1)
sleep 2
STILL=$(live_daemons "$S" | tr '\n' ' ')
case "$STOPOUT" in
  *"no such process"*) fail "stop failed: $STOPOUT" ;;
  *)                   pass "stop reported no error" ;;
esac
[ -z "$STILL" ] \
  && pass "nothing syncing after stop" \
  || fail "stop left daemon(s) running: $STILL — sync cannot be turned off"

echo
echo "== done: $FAILED failure(s) =="
exit $((FAILED > 0))
