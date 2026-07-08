---
description: Start syncing a project in this folder — create a new BearDrive project or connect an existing one, whole folder or a shared subfolder, and start the sync daemon
argument-hint: "[folder] [--name <project> | --project <p-id>] [--shared <dir>]"
---

Start syncing a project with BearDrive. Arguments: `$ARGUMENTS` (optional
folder, optional `--name`/`--project`/`--shared`).

Follow these steps:

1. **Check the bdrive CLI is installed**: run `command -v bdrive`. If
   missing, offer to install it (`brew install runbear-io/tap/beardrive`,
   or `go install github.com/runbear-io/beardrive/cmd/bdrive@latest`) and
   wait for the user's choice before installing.

2. **Sign in if needed**: run `bdrive login --status`. With no server or a
   stale token, run `bdrive login` (tell the user a browser window is
   coming; it completes by itself). Default server is beardrive.ai; pass a
   self-hosted URL if the user mentioned one.

3. **Initialize**: if the folder already contains `.bdrive/`, just run
   `bdrive init --yes` there — it resumes syncing (including after a
   rename/move). Otherwise decide the project name (argument, or ask, or
   default to the folder name) and scope (whole folder, or only a shared
   subfolder like `./wiki` via `--shared`), then run it non-interactively:

   ```sh
   bdrive init --name <project> --yes            # whole folder
   bdrive init --name <project> --shared wiki    # only ./wiki syncs
   ```

4. **Verify**: run `bdrive status <folder>` and confirm the daemon is
   running and pending is 0. Summarize: project name/id, what syncs, and
   that edits propagate to every team member within seconds.

For the full team setup (CLAUDE.md guidance + per-project sync hooks in
`.claude/settings.json`), suggest `/beardrive:install` instead.
