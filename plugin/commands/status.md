---
description: Show bdrive sync status — mounts, daemon state, pending changes — and diagnose any sync problems
argument-hint: [folder]
---

Show the bdrive sync status. Argument: `$ARGUMENTS` (optional folder; default all mounts).

1. Run `bdrive status $ARGUMENTS` and show the output.
2. If anything looks wrong, diagnose using the beardrive skill:
   - `daemon: stopped` → restart with `bdrive mnt <folder>`
   - `pending` stuck above 0 → run `bdrive sync <folder>` and read the error; it usually points at credentials or the remote
   - changes not appearing from another device → run `bdrive log <folder>` to see whether the ops arrived
3. Summarize the state in one or two sentences.
