#!/bin/sh
# Sync the current project if it is a beardrive project (has a .bdrive/ settings dir).
# Fast no-op otherwise, so this hook is safe on every turn in every project.
#
# Runs blocking on UserPromptSubmit (fresh files before Claude reads them)
# and async on Stop (push edits out without delaying the turn).
#
# Hooks receive JSON on stdin with a session_id; it is passed as the sync
# note so every change journaled during this session — including ones the
# background daemon commits — is stamped with the Claude Code session that
# made it, and shows up in `bdrive log` and the hub's history views.
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
[ -d .bdrive ] || exit 0
command -v bdrive >/dev/null 2>&1 || exit 0
sid=""
if [ ! -t 0 ]; then
  sid=$(head -c 8192 | sed -n 's/.*"session_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
fi
if [ -n "$sid" ]; then
  bdrive sync . --note "claude-code session $sid" >/dev/null 2>&1 || true
else
  bdrive sync . >/dev/null 2>&1 || true
fi
