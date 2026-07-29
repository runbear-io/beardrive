---
description: Set up BearDrive for this project — install the CLI, sign in, create/connect a project, optionally document the shared folder in CLAUDE.md, and register project-level sync hooks so every teammate's files stay fresh during Claude sessions
argument-hint: "[project-name] [--only <dirs>]"
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

If `$ARGUMENTS` gives a project name and/or `--only <dirs>` (comma-separated),
use them.
Otherwise ask the user two questions:
- **Create a new project or connect an existing one?** (`bdrive init
  --name <name>` creates-or-joins by name; `bdrive init --project <p-id>`
  connects by id.) Skip only if the user already gave a project id or name.
- **Which folder syncs?** ALWAYS ask this one — where you were started
  names a workspace, not what to sync, and an empty folder is not an
  answer either. Hard rule: **never sync a repo root** — inside a repo,
  knowledge always syncs as a mounted subfolder, or the root narrowed with
  `--only`.
  Whole-folder is only for a dedicated knowledge folder (a standalone
  vault) that is the mount itself — and even then confirm it. Don't ask
  open-endedly: scan the repo for existing knowledge folders (`wiki/`,
  `docs/`, `notes/`, `handbook/`, an Obsidian vault — markdown-heavy,
  not source code, whatever the name) and propose concrete choices:
  lead with the candidates you found; when there are none, recommend
  creating a dedicated subfolder (e.g. `wiki/`) as the default; plus a
  different folder entirely (`bdrive init <path>`), or the whole folder
  when it qualifies.
  (`bdrive init . --only wiki,docs` puts them in one project — one
  membership, one permission set; folders needing different access go in
  separate projects).
  Hard gate: do not run `bdrive init` until the user has answered the
  folder question in this conversation — no exception for empty folders,
  non-repos, or non-interactive sessions (if you cannot ask, end your
  turn with the question instead of proceeding).
  Ask with the **AskUserQuestion** tool (one question, header "Sync
  folder", your recommendation first and labelled "(Recommended)", then
  the alternatives) rather than plain prose — the user picks instead of
  typing a path.
  The mount is always exactly the folder you name: `bdrive init wiki
  --project <p-id>` makes ./wiki the project. Nothing re-roots a mount
  elsewhere. To sync only part of a folder, narrow it in place with
  `--only`, which writes `.bdriveignore` rules and keeps the mount where
  it is.

**Init first, git handoff second.** Init can refuse (this device may already
sync that project elsewhere); a refusal after `.gitignore` and `git rm
--cached` leaves the repo half-changed for nothing.

**One transport per folder.** If the chosen folder is currently git-tracked,
BearDrive and git would both write it — the silent-revert hazard. Get consent,
then hand it off: `git rm -r --cached <dir>` and add `<dir>/` to `.gitignore`;
stage the change but let the user commit. (Full detection ladder — git,
Obsidian, symlinks — in the beardrive skill's "Connecting knowledge tooling".)

Then run it non-interactively, e.g.:
```sh
bdrive init --name <project-name> --yes               # this folder is the project
bdrive init wiki --name <project-name> --yes          # ./wiki is the project
bdrive init . --name <project-name> --only wiki,docs --yes  # narrow this folder to those subfolders
```
Re-running `bdrive init --yes` later is always safe: it resumes syncing
(including after the folder was renamed or moved). To widen or narrow the
scope later, use `bdrive scope add <dir>` / `bdrive scope rm <dir>` from the
mount root — it edits the managed block in `.bdriveignore`, so nobody has to
hand-write negation rules.

After init, tell git what's what: add `.bdrive/` to `.gitignore` (per-machine
state, never committed). `.bdriveignore` always syncs through BearDrive, and it carries the `--only`
scope rules too, so the team shares both automatically; committing it to git
as well is fine but optional.

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
mount inside a repo, append a short section to the repo root's
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

## 5. Confirm the sync hooks

`bdrive init` already registered them — there is no separate hooks command to
run. It writes each platform's **user** config once per machine
(`~/.claude/settings.json`, `~/.codex/hooks.json`, `~/.gemini/settings.json`,
`~/.hermes/config.yaml`), because platforms read hook config only from the
directory a session starts in: a per-project file would fire only for sessions
that happen to start there, and — living inside a mount — would sync to the
whole team. Nothing agent-shaped is written into the project.

The hooks pull before every turn (the agent always reads the team's latest
files), push right after edits (artifacts land on the server seconds after
they're created — daemon or no daemon), and stamp every change with the agent
session that made it (`bdrive sync --note "<agent> session <id>"` — visible in
`bdrive log` and the hub's history views). A third hook (`bdrive read-log`)
queues which files the agent read — via the native read tool, grep-style
searches, or shell commands that name project files — so the hub's read
heatmap can show admins what the team's agents actually consume. Reads are
reported on the next sync, never from the hook itself. All of them are fast
no-ops outside bdrive folders.

Read init's output for the platforms it covered and tell the user. Pass on the
one manual step: **Codex hooks are experimental and off by default** — enable
with `[features] codex_hooks = true` in `~/.codex/config.toml`, and Codex asks
once to trust the hook definition. If a platform the user works with is
missing, `bdrive hooks install --agent <name>` adds it; `bdrive hooks` shows
the status table and `bdrive hooks uninstall` removes them.

Teammates do not inherit hooks through the repo any more — each device
installs its own when it runs `bdrive init`. If the user mentions teammates on
other agents (Codex, Gemini CLI, Hermes), tell them those teammates need no
terminal either — they paste one prompt into their own agent (the hub's
project home page shows it filled in):

```
Follow https://raw.githubusercontent.com/runbear-io/beardrive/main/INSTALL_FOR_AGENTS.md
to set up BearDrive project <project-id> on <hub-url>. Ask me which folder to sync.
```

## 6. Verify and summarize

Run `bdrive status` and confirm the daemon is running and pending is 0.
Then tell the user what was set up — and ALWAYS finish with the payoff:
pick a representative file in the synced folder (the wiki's index/README,
or an artifact you just generated), run `bdrive url <file>`, and hand the
user the link with an invitation to open it — seeing their folder rendered
in the browser is the moment the setup clicks. Teammate links require
sign-in (safe by default); mention `bdrive share <file>` exists for fully
public links when someone outside the hub needs it.
