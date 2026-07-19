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

Run `bdrive login --status`. If there is no valid session, tell the user a
browser window is coming, then sign in:
- **Default: BearDrive Cloud.** Run bare `bdrive login` — the browser opens
  beardrive.ai where they sign in or sign up. A brand-new account gets a
  free personal workspace automatically (no forms beyond signup itself); a
  pending team invite lands them in that team instead. No prior signup is
  needed — this step IS the signup.
- **Self-hosted team:** if the user says their team runs its own hub, ask
  for the URL and run `bdrive login https://their-hub`.

## 3. Initialize the project

If `$ARGUMENTS` gives a project name and/or `--shared <dir>`, use them.
Otherwise ask the user two questions (or infer from their request):
- **Create a new project or connect an existing one?** (`bdrive init
  --name <name>` creates-or-joins by name; `bdrive init --project <p-id>`
  connects by id.)
- **Sync the whole folder, or only a shared subfolder?** Hard rule:
  **never sync a repo root** — inside a repo, knowledge always syncs as a
  scoped subfolder via `--shared`. Whole-folder is only for a dedicated
  knowledge folder (an empty dir, a standalone vault) that is the mount
  itself. Don't ask open-endedly: scan the repo for an existing knowledge
  folder (`wiki/`, `docs/`, `notes/`, `handbook/`, an Obsidian vault —
  markdown-heavy, not source code) and propose the best candidate for
  confirmation, e.g. "I found `./wiki` — sync that?".

**One transport per folder.** If the chosen folder is currently git-tracked,
BearDrive and git would both write it — the silent-revert hazard. Get consent,
then hand it off: `git rm -r --cached <dir>` and add `<dir>/` to `.gitignore`;
stage the change but let the user commit. (Full detection ladder — git,
Obsidian, symlinks — in the beardrive skill's "Connecting knowledge tooling".)

Then run it non-interactively, e.g.:
```sh
bdrive init --name <project-name> --yes            # dedicated knowledge folder
bdrive init --name <project-name> --shared wiki    # in a repo: only ./wiki syncs
```
Re-running `bdrive init --yes` later is always safe: it resumes syncing
(including after the folder was renamed or moved).

After init, tell git what's what: add `.bdrive/` to `.gitignore` (per-machine
state, never committed) and COMMIT `.bdriveignore` (on a `--shared` mount the
root `.bdriveignore` is local-only to each clone, so git is how the team
shares it).

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
sees them, and whenever you mention a synced file's path, append its
gated link on an emoji: `` `wiki/<file>` `` [🔗](\<hub link>) —
`bdrive url wiki/<file>` prints the link (teammates sign in to view).
Never put secrets here (`bdrive share` mints fully public URLs).
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
Heads-up before installing: Hermes hooks are PER-USER (`~/.hermes/config.yaml`,
outside the repo) — mention that when it's among the targets, and skip it
unless the user actually uses Hermes.

If the user mentions teammates on other agents (Codex, Gemini CLI, Hermes),
tell them those teammates need no terminal either — they paste one prompt
into their own agent (the hub's project home page shows it filled in):

```
Set up BearDrive in this folder.
1. If `bdrive` is missing, install it: brew install runbear-io/tap/beardrive
2. bdrive skill install   # so you know the CLI next time
3. bdrive login --device <hub-url>   # show me the code and the URL
4. bdrive init --project <project-id>
5. bdrive hooks install   # don't skip this - it's what syncs every turn
```

Step 2 leaves the beardrive skill in that agent's skills dir
(`~/.codex/skills/beardrive/` and friends) so their later sessions are
conversational. Handing teammates loose commands is how the hooks step gets
skipped.

## 6. Verify and summarize

Run `bdrive status` and confirm the daemon is running and pending is 0.
Then tell the user what was set up — and ALWAYS finish with the payoff:
pick a representative file in the synced folder (the wiki's index/README,
or an artifact you just generated), run `bdrive url <file>`, and hand the
user the link with an invitation to open it — seeing their folder rendered
in the browser is the moment the setup clicks. Teammate links require
sign-in (safe by default); mention `bdrive share <file>` exists for fully
public links when someone outside the hub needs it.
