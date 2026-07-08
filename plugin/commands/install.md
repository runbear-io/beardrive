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
  `./shared`)? A shared subfolder is right when only part of the repo — a
  company wiki, a deliverables folder — should be shared across the team.

Then run it non-interactively, e.g.:
```sh
bdrive init --name <project-name> --yes            # whole folder
bdrive init --name <project-name> --shared wiki    # only ./wiki syncs
```
Re-running `bdrive init --yes` later is always safe: it resumes syncing
(including after the folder was renamed or moved).

## 4. Offer to update CLAUDE.md (ask first — never do this silently)

Ask: "Want me to add a section to CLAUDE.md so agents know about the shared
folder?" If yes, append a section shaped like this (adapt folder name and
wording to the project; create CLAUDE.md if missing):

```markdown
## Shared folder (BearDrive)

`wiki/` is the company wiki, synced across all team members and agents via
BearDrive. Anything saved there propagates to everyone within seconds, and
every change is tracked (who, when, from which device).

- Put shareable artifacts — generated HTML/PDF/markdown reports, notes,
  plans — in `wiki/` so the team can see them.
- To hand someone a public link to a file: `bdrive share wiki/<file>` —
  prints a URL anyone can open (rendered, no account needed).
- Do not put secrets in `wiki/`; share links are public to whoever has
  the URL.
```

## 5. Register project-level sync hooks (ask first)

Ask: "Want me to register sync hooks in `.claude/settings.json` so files
sync automatically during Claude sessions — for every teammate, plugin or
not?" If yes, merge this into the project's `.claude/settings.json`
(create it if missing; preserve existing hooks — append to the arrays,
never overwrite them):

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "sh -c 'cd \"${CLAUDE_PROJECT_DIR:-.}\" && [ -d .bdrive ] && command -v bdrive >/dev/null && bdrive sync . >/dev/null 2>&1 || true'",
            "timeout": 30,
            "statusMessage": "beardrive: pulling latest files"
          }
        ]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "Write|Edit|MultiEdit",
        "hooks": [
          {
            "type": "command",
            "command": "sh -c 'cd \"${CLAUDE_PROJECT_DIR:-.}\" && [ -d .bdrive ] && command -v bdrive >/dev/null && bdrive sync . >/dev/null 2>&1 || true'",
            "async": true
          }
        ]
      }
    ]
  }
}
```

The pull at prompt-submit means Claude always reads the team's latest
files; the async push after each Write/Edit means artifacts land on the
server seconds after Claude creates them — daemon or no daemon. Both are
fast no-ops in folders without `.bdrive/`.

## 6. Verify and summarize

Run `bdrive status` and confirm the daemon is running and pending is 0.
Then tell the user what was set up, and demonstrate the payoff: if they
have (or you just generated) an HTML/PDF/markdown artifact in the synced
folder, run `bdrive share <file>` and hand them the URL.
