#!/bin/sh
# Turn-start hook: pull the team's latest files and hand Claude the
# project's gated-link formula (hookSpecificOutput JSON on stdout), so any
# synced file path the agent mentions gets a hub link on a 🔗 — computed
# fresh from the binary each turn, immune to stale skill copies. `bdrive
# sync --hook` reads the event JSON from stdin itself (session note) and
# never fails the turn. Fast no-op outside beardrive projects.
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
[ -d .bdrive ] || exit 0
command -v bdrive >/dev/null 2>&1 || exit 0
exec bdrive sync . --hook claude-code 2>/dev/null
