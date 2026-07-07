#!/bin/sh
# Sync the current project if it is an sfs mount (has a .sfs settings file).
# Fast no-op otherwise, so this hook is safe on every turn in every project.
#
# Runs blocking on UserPromptSubmit (fresh files before Claude reads them)
# and async on Stop (push edits out without delaying the turn).
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
[ -f .sfs ] || exit 0
command -v sfs >/dev/null 2>&1 || exit 0
sfs sync . >/dev/null 2>&1 || true
