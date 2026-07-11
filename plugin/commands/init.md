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

3. **Detect knowledge tooling** (skip if the folder already contains
   `.bdrive/` — then just run `bdrive init --yes`; it resumes syncing,
   including after a rename/move). Check, in order — first match wins,
   ask if two match (full playbook: the beardrive skill's "Connecting
   knowledge tooling" section):

   - **gbrain** (`gbrain.yml`, or a gbrain MCP server / brain-first
     CLAUDE.md block) → offer to sync the brain's shared subfolder as its
     own project; never a brain root.
   - **OKF wiki** (markdown with OKF frontmatter) → offer: connect the
     wiki dir via `--shared`, or keep it PR-gated and create a new shared
     folder.
   - **Wiki-ish folder** (`docs`/`wiki`/`notes` full of markdown) → check
     `git log -- <dir>`; dormant → recommend connecting it, active PR
     traffic → recommend a new shared folder. Offer an OKF upgrade
     (`openknowledge from`) after connecting, as a separate consent.
   - **Nothing / empty** → offer a starting point in this order:
     OKF (recommended), gbrain, blank, describe-it.

4. **Initialize** — two hard rules:

   - **Never sync a repo root**: inside a repo, knowledge syncs as a
     scoped subfolder via `--shared`. A dedicated knowledge folder
     (empty dir, standalone vault) may be the mount itself.
   - **One transport per folder**: a git-tracked dir must leave git
     tracking before it syncs (`git rm -r --cached <dir>` + gitignore;
     stage it, let the user commit). Offer one-way git snapshots if they
     want a git record; `bdrive log -p <path>` covers history for most.

   ```sh
   bdrive init --name <project> --yes            # dedicated knowledge folder
   bdrive init --name <project> --shared wiki    # in a repo: only ./wiki syncs
   ```

5. **Register agent sync hooks**: run `bdrive hooks install <folder>`. It
   detects the agent platforms in use (Claude Code, Codex, Gemini CLI,
   Hermes — by their config dirs in the project or home) and idempotently
   merges beardrive's sync hooks into each platform's own hook config, so
   files pull at every turn start, push after edits, every change is
   stamped with the agent session that made it, and agent file reads feed
   the hub's read heatmap (queued locally by `bdrive read-log`, reported
   on the next sync). Tell the user which platforms got hooks; if Codex is
   among them, mention they must run `/hooks` inside Codex once to trust
   the project's `.codex` layer.

6. **Verify**: run `bdrive status <folder>` and confirm the daemon is
   running and pending is 0. Summarize: project name/id, what syncs, and
   that edits propagate to every team member within seconds. Offer a
   consent-gated CLAUDE.md note and tell the user how teammates connect
   (invite link → `bdrive init` → same `--shared` scope, which is
   per-device).

For the full team setup (CLAUDE.md guidance + per-project sync hooks in
`.claude/settings.json`), suggest `/beardrive:install` instead.
