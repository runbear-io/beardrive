#!/bin/sh
# Sync the current project if it is a beardrive project (has a .bdrive/ settings dir).
# Fast no-op otherwise, so this hook is safe on every turn in every project.
#
# Runs blocking on UserPromptSubmit (fresh files before Claude reads them)
# and async on Stop (push edits out without delaying the turn).
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
[ -d .bdrive ] || exit 0
command -v bdrive >/dev/null 2>&1 || exit 0
bdrive sync . >/dev/null 2>&1 || true
