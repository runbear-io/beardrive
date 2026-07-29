---
description: Start syncing a project in this folder — create a new BearDrive project or connect an existing one, the whole folder or only some subfolders, and start the sync daemon
argument-hint: "[folder] [--name <project> | --project <p-id>] [--only <dirs>]"
---

Start syncing a project with BearDrive. Arguments: `$ARGUMENTS` (optional
folder, optional `--name`/`--project`/`--only`).

Follow these steps:

1. **Check the bdrive CLI is installed**: run `command -v bdrive`. If
   missing, offer to install it (`brew install runbear-io/tap/beardrive`,
   or `go install github.com/runbear-io/beardrive/cmd/bdrive@latest`) and
   wait for the user's choice before installing.

2. **Sign in if needed**: run `bdrive login --status`. With no valid
   session, tell the user a browser window is coming, then run bare
   `bdrive login` (BearDrive Cloud — signing up auto-creates a free
   personal workspace; a pending team invite routes them into that team).
   If the user says their team runs its own hub, use
   `bdrive login https://their-hub` instead. Either way it completes by
   itself.

3. **Detect knowledge tooling** (skip if the folder already contains
   `.bdrive/` — then just run `bdrive init --yes`; it resumes syncing,
   including after a rename/move). Check, in order — first match wins,
   ask if two match (full playbook: the beardrive skill's "Connecting
   knowledge tooling" section):

   - **gbrain** (`gbrain.yml`, or a gbrain MCP server / brain-first
     CLAUDE.md block) → offer to sync the brain's shared subfolder as its
     own project; never a brain root, and one enrichment owner per shared
     folder (everyone else indexes read-only — see the skill).
   - **OKF wiki** (markdown with OKF frontmatter) → offer: connect the
     wiki dir by mounting it, or keep it PR-gated and create a new shared
     folder.
   - **Wiki-ish folder** (`docs`/`wiki`/`notes` full of markdown) → check
     `git log -- <dir>`; dormant → recommend connecting it, active PR
     traffic → recommend a new shared folder. Offer an OKF upgrade
     (`openknowledge from`) after connecting, as a separate consent.
   - **Nothing / empty** → offer a starting point in this order:
     OKF (recommended), gbrain, blank, describe-it.

4. **Initialize** — two hard rules:

   - **Never sync a repo root**: inside a repo, knowledge syncs as a
     mounted subfolder (`bdrive init wiki`), or as the root narrowed
     with `--only` when several folders belong to one project and the
     same people should see all of them (folders needing different
     access go in separate projects). A
     dedicated knowledge folder (empty dir, standalone vault) may be the
     mount itself.
   - **One transport per folder**: a git-tracked dir must leave git
     tracking before it syncs (`git rm -r --cached <dir>` + gitignore;
     stage it, let the user commit). Offer one-way git snapshots if they
     want a git record; `bdrive log -p <path>` covers history for most.

   ```sh
   bdrive init --name <project> --yes                 # dedicated knowledge folder
   bdrive init wiki --name <project> --yes            # ./wiki is the project
   bdrive init . --name <project> --only wiki,docs --yes  # narrow this folder to those subfolders
   ```

5. **Confirm the sync hooks**: `bdrive init` registered them for every agent
   platform it detected, in that platform's **user** config
   (`~/.claude/settings.json`, `~/.codex/hooks.json`, `~/.gemini/settings.json`,
   `~/.hermes/config.yaml`) — once per machine, covering every session in
   every folder, with nothing written inside the project. Files pull at every
   turn start, push after edits, every change is stamped with the agent
   session that made it, and agent file reads feed the hub's read heatmap
   (queued locally by `bdrive read-log`, reported on the next sync). Read
   init's output and tell the user which platforms are covered. If Codex is
   among them, pass on that its hooks are experimental and off by default —
   `[features] codex_hooks = true` in `~/.codex/config.toml`, then trust the
   hook when Codex asks. `bdrive hooks` shows the status table;
   `bdrive hooks uninstall` removes them.

6. **Verify**: run `bdrive status <folder>` and confirm the daemon is
   running and pending is 0. Summarize: project name/id, what syncs, and
   that edits propagate to every team member within seconds. Offer the
   two-file agent orientation, each part as its own consent (full flow:
   the skill's "Teaching agents the folder" section): a synced
   `<shared>/AGENTS.md` mapping the folder — draft it if this user is
   creating the project, read and follow it if joining — and, for
   mounts inside a repo, a short pointer to it in the repo
   root's `AGENTS.md`/`CLAUDE.md` (the only file Codex loads, and what
   makes any agent aware the folder matters). Then tell the user how
   teammates connect (invite link → `bdrive init`; the scope rides
   `.bdriveignore`, so it matches automatically).

For the full team setup (the AGENTS.md orientation + the sync hooks init
registers in `~/.claude/settings.json`), suggest `/beardrive:install` instead.
