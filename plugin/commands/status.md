---
description: Show sfs sync status — mounts, daemon state, pending changes — and diagnose any sync problems
argument-hint: [folder]
---

Show the sfs sync status. Argument: `$ARGUMENTS` (optional folder; default all mounts).

1. Run `sfs status $ARGUMENTS` and show the output.
2. If anything looks wrong, diagnose using the sfs skill:
   - `daemon: stopped` → restart with `sfs mnt <folder>`
   - `pending` stuck above 0 → run `sfs sync <folder>` and read the error; it usually points at credentials or the remote
   - changes not appearing from another device → run `sfs log <folder>` to see whether the ops arrived
3. Summarize the state in one or two sentences.
