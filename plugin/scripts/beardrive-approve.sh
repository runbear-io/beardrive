#!/bin/sh
# PreToolUse: auto-approve beardrive's own setup commands so onboarding is not
# a permission gauntlet. Pure shell until the payload actually mentions bdrive,
# so the common case costs nothing; the binary makes the real decision and
# stays silent (= ask the user as usual) for anything it does not recognize.
payload=$(head -c 8192)
case "$payload" in
  *bdrive*) ;;
  *) exit 0 ;;
esac
command -v bdrive >/dev/null || exit 0
printf '%s' "$payload" | bdrive hook-approve 2>/dev/null || true
