#!/bin/sh
# Queue the file reads from a Read tool call for the hub's read heatmap.
# `bdrive read-log` parses the hook's stdin JSON itself and only appends to
# a local spool (drained on the next sync) — no network, no locking, so this
# is safe to run on every Read in every project. Fast no-op outside
# beardrive projects.
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
[ -d .bdrive ] || exit 0
command -v bdrive >/dev/null 2>&1 || exit 0
bdrive read-log . >/dev/null 2>&1 || true
