---
description: Set up BearDrive for this project — install the CLI, sign in, create/connect a project, optionally document the shared folder in CLAUDE.md, and register project-level sync hooks so every teammate's files stay fresh during Claude sessions
argument-hint: "[project-name] [--shared <dir>]"
---

Set up BearDrive for the current project, end to end. Work through these
steps in order, telling the user what you're doing at each one.

## 1. Ensure the `bdrive` binary exists

Run `command -v bdrive`. If missing, install it:
- macOS/Linuxbrew: `brew install runbear-io/tap/beardrive`
- otherwise: `go install github.com/runbear-io/beardrive/cmd/bdrive@latest`
If neither works, stop and tell the user how to install manually.

## 2. Sign in if needed

Run `bdrive login --status`. If there is no server or the token is invalid,
run `bdrive login` — it opens the user's browser to sign in (or sign up) and
completes by itself. Default server is beardrive.ai; if the user mentioned a
self-hosted server, pass its URL: `bdrive login https://their-server`.
Tell the user a browser window is coming before you run it.

## 3. Initialize the project

If `$ARGUMENTS` gives a project name and/or `--shared <dir>`, use them.
Otherwise ask the user two questions (or infer from their request):
- **Create a new project or connect an existing one?** (`bdrive share --list`
  isn't needed here — `bdrive init --name <name>` creates-or-joins by name;
  `bdrive init --project <p-id>` connects by id.)
- **Sync the whole folder, or only a shared subfolder** (e.g. `./wiki` or
  `./shared`)? Hard rule: **never sync a repo root** — inside a repo,
  knowledge always syncs as a scoped subfolder via `--shared`. Whole-folder
  is only for a dedicated knowledge folder (an empty dir, a standalone
  vault) that is the mount itself.

Then run it non-interactively, e.g.:
```sh
bdrive init --name <project-name> --yes            # dedicated knowledge folder
bdrive init --name <project-name> --shared wiki    # in a repo: only ./wiki syncs
```
Re-running `bdrive init --yes` later is always safe: it resumes syncing
(including after the folder was renamed or moved).

## 4. Teach agents about the shared folder (ask first — never do this silently)

Two files with different jobs (full rationale: the beardrive skill's
"Teaching agents the folder" section). Offer each as its own consent:

**a. The folder's own map — `<shared>/AGENTS.md` (synced, team-wide).**
If the shared folder already has an `AGENTS.md`, read it and follow it —
it is the team's source of truth; do not rewrite it while onboarding.
If it has none and this user is creating the project, offer to draft one:
explore the folder (top-level dirs, naming patterns, what's actually
there) and write a short map — what each area is for, naming conventions,
where agents should put their output, what not to touch. Keep it under a
screen; it syncs to every member, so write it for the whole team, not
this machine.

**b. A root pointer in this repo (per machine, never synced).** For a
`--shared` mount inside a repo, append a short section to the repo root's
`AGENTS.md` and/or `CLAUDE.md` — both if both exist; `AGENTS.md` is what
Codex and Hermes read (Codex never discovers nested instruction files,
and no platform knows the folder *matters* until told). Shape it like
this (adapt the folder name; create the file if missing):

```markdown
## Shared folder (BearDrive)

`wiki/` is the team's shared folder, synced via BearDrive — changes
propagate to everyone within seconds and every change is tracked (who,
when, which device). Read `wiki/AGENTS.md` before working there. Put
shareable artifacts — reports, notes, plans — in `wiki/` so the team
sees them; never secrets (`bdrive share wiki/<file>` mints public URLs).
```

Point at the synced `AGENTS.md` rather than duplicating its conventions —
the pointer is for awareness and routing; the conventions live in the
folder, stay current for everyone, and are versioned by the hub. For a
standalone knowledge mount (dedicated folder, no enclosing repo) skip the
pointer: `AGENTS.md` at the mount root is loaded natively by every
platform.

## 5. Register agent sync hooks

Run `bdrive hooks install` in the project. It detects the agent platforms
in use — Claude Code (`.claude/`), Codex (`.codex/`), Gemini CLI
(`.gemini/`), Hermes (`~/.hermes/`) — and idempotently merges beardrive's
sync hooks into each platform's own hook config, preserving any hooks
already there. Project-level files (`.claude/settings.json`,
`.codex/hooks.json`, `.gemini/settings.json`) ride the repo, so every
teammate gets them — plugin or not, whatever agent they use; Hermes hooks
are per-user (`~/.hermes/config.yaml`).

The registered hooks pull before every turn (the agent always reads the
team's latest files), push right after edits (artifacts land on the server
seconds after they're created — daemon or no daemon), and stamp every
change with the agent session that made it (`bdrive sync --note "<agent>
session <id>"` — visible in `bdrive log` and the hub's history views).
A third hook (`bdrive read-log`) queues which files the agent read — via
the native read tool, grep-style searches (the files the matches came
from), or shell commands that name project files — so the hub's read
heatmap can show admins what the team's agents actually consume. Reads
are reported on the next sync, never from the hook itself. They are fast no-ops in folders without
`.bdrive/`.

Tell the user which platforms got hooks (`bdrive hooks` shows the status
table). If Codex is among them, mention they must run `/hooks` inside
Codex once to trust the project's `.codex` layer. To register a platform
that wasn't detected: `bdrive hooks install --agent claude,codex,gemini,hermes`.

## 6. Verify and summarize

Run `bdrive status` and confirm the daemon is running and pending is 0.
Then tell the user what was set up, and demonstrate the payoff: if they
have (or you just generated) an HTML/PDF/markdown artifact in the synced
folder, run `bdrive share <file>` and hand them the URL.
