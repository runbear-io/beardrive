# BearDrive — install instructions for AI agents

You are an AI agent (Claude Code, Codex, Gemini CLI, Hermes, or similar) and
your user asked you to set up BearDrive by following this document. Work
through the steps in order, telling the user what you're doing at each one.
Steps marked **ask first** require the user's explicit answer — do not guess
or skip them.

BearDrive syncs a folder across a team and their agents through a hub, with
per-file history and share links. Setup is short by design: make sure the CLI
exists, ask the user what to sync, and run **one** `bdrive init` — it signs
in, registers the sync hooks (and, on macOS and Linux, a login item so sync
resumes after a reboot), starts syncing and prints the project link. Every extra command you invent is another permission prompt
for the user. Full documentation: https://docs.beardrive.ai (agent-readable
index at https://docs.beardrive.ai/llms.txt).

**How to run commands, throughout.** Each distinct command you run may cost
the user a permission prompt, so:

- **one command per shell call** — never chain with `&&`, `;` or a pipe; a
  compound command needs approval for every part;
- **no preflight** — do not check versions or status "just to be sure";
- **never retry a denied command** — say so once, hand the user the exact
  `! <command>` to run plus the allow-rule `"Bash(bdrive:*)"`, then continue
  with the steps that do not depend on it.

> Maintainers: this file is the source of truth for agent onboarding. When
> the CLI's init/login/hooks flow changes, update it here.

## 1. Install the CLI (only if it is missing)

Run `command -v bdrive` — this is the one preflight worth a prompt. If it
prints a path, go straight to step 2. If missing:

```sh
brew install runbear-io/tap/beardrive      # macOS / Linuxbrew
```

No Homebrew? Grab the release binary for this OS/arch from
https://github.com/runbear-io/beardrive/releases, or with a Go toolchain:
`go install github.com/runbear-io/beardrive/cmd/bdrive@latest`. If nothing
works, stop and tell the user.

## 2. Do not run a login command

`bdrive init` (step 3) signs the device in when there is no session, and
without a TTY it uses the device-code flow automatically: it prints one link
for the user to open in any browser and approve — no code to type. Pass the hub with
`--server <hub-url>` and init signs in *there* — so a hub this device has
never seen still needs no separate command.

So: no `bdrive login`, no `bdrive login --status`. They are extra permission
prompts for something init does anyway. The only time to run login on its own
is when the user explicitly asks to switch hubs without connecting a folder.

## 3. Initialize the project — **ask first**

Two questions:

1. **Create a new project or connect an existing one?** (Skip only if the
   user already gave a project id or name.)
   `bdrive init --name <name>` creates-or-joins by name;
   `bdrive init --project <p-id>` connects by id.
2. **Which folder syncs?** ALWAYS ask, and always ask with a named
   recommendation — never an open-ended "which folder and what scope?".
   Pick your recommendation with this rule, in order:
   - **You know the project's name** (the paste prompt carries it, or the
     user gave one): **recommend a folder of that name** — "create
     `<project-name>/` here and sync that", lowercased with spaces as
     dashes. Folder name = project name is what makes every teammate's
     checkout look the same and the hub links read right.
   - **You found a knowledge folder** (markdown-heavy, not source code —
     `docs/`, `notes/`, an Obsidian vault, or any folder that
     looks like one whatever its name): recommend it.
   - **Neither** (no project name given, no knowledge folder, including
     when the folder is empty): **recommend creating `shared/`** —
     "create `shared/` here and sync that". `bdrive init shared` names the
     new project after the folder, so the project is `shared` too, and
     that pairing is the default for a fresh setup. An empty folder is not
     evidence that it is meant to be the knowledge folder, and the
     folder *not* being a git repo is not a reason to sync it whole —
     the recommendation is the same either way. The repo-root rule
     below is a separate, harder prohibition, not the only reason to
     prefer a subfolder.

   Then list the alternatives: a different folder entirely (`bdrive init
   <path>`), or the whole current folder — an alternative the user may
   choose, never your recommendation.

   Hard rule: **never mount a repo root bare.** The one sanctioned way to
   sync inside a repo without picking a single subfolder is to mount the
   root *narrowed*: `bdrive init . --only docs,notes`, which syncs only
   those subfolders. That is exactly how several sibling folders share
   one project — do not propose moving folders around to give them a
   common parent. Wait for the pick.

   The shape to aim for: "I recommend creating `shared/` here and syncing
   that. Alternatives: a different path, or this whole folder if you
   mean it to be the knowledge folder itself. Which do you want?" — with
   `shared/` replaced by the project's name when you have one.

   **On Claude Code, ask with the AskUserQuestion tool** rather than
   plain prose — one question, header "Sync folder", your recommendation
   as the first option labelled "(Recommended)", then the alternatives.
   The user picks instead of typing a path. Every other agent: prose.

Executing the pick — **the mount is always exactly the folder you name.**
`bdrive init shared --project <p-id>` makes ./shared the project, so the
project's files land inside it. There is no flag that re-roots a mount
somewhere else. Syncing only part of a folder is `--only`, which narrows
a mount without moving it: `bdrive init . --only docs,notes` keeps the
mount at `.` and writes `.bdriveignore` rules so only those subfolders
sync (their paths keep the `docs/` prefix on the hub, which is what
teammates then see).

**Hard gate: do not run `bdrive init` until the user has answered
the folder question in this conversation.** There is no exception — not for an
empty folder, not for a non-repo, not for a non-interactive session. If
you cannot ask, end your turn with the question instead of proceeding.

Run init BEFORE the git handoff: init can refuse (e.g. this device already
syncs that project somewhere else), and a refusal after you have already
rewritten `.gitignore` and unstaged files leaves the repo half-changed. If
the chosen folder is git-tracked, git and BearDrive would both write it
(silent-revert hazard). Get consent, then `git rm -r --cached <dir>` and add
`<dir>/` to `.gitignore`; stage but let the user commit.

Then run **one** command — init signs in if needed, registers the hooks and
the login autostart, syncs, and prints the project link. Do not precede it
with `command -v bdrive` or `bdrive --version`: every extra command is
another permission prompt, and if the binary is missing this one says so.

```sh
bdrive init <project-name> --project <p-id> --server <hub-url> --yes  # that subfolder is the project
bdrive init shared --yes                                              # fresh setup: ./shared, project "shared"
bdrive init . --name <project> --only docs,notes --yes                # this folder, only those subfolders sync
```

Drop `--server` when the user gave no hub URL (BearDrive Cloud is the
default), and `--project`/`--name` follow the answer to question 1 — with
no name given, `bdrive init shared` names the project after the folder,
so no `--name` is needed.

**Run one command per shell call.** Never chain with `&&`, `;` or a pipe: a
compound command needs approval for each part, so chaining multiplies the
prompts.

**If a command is blocked by your harness's permissions**, say so once and do
not retry it. Hand the user both of these, then continue with every step that
does not depend on it and re-check at the end:

- the command to run themselves — in Claude Code, `! bdrive init …`
- the allow-rule that prevents it recurring: `"Bash(bdrive:*)"`

Approving with "don't ask again" also works, and since setup is a single
command that is the last prompt they will see.

After init: add `.bdrive/` to `.gitignore` (per-machine state, never
committed). Re-running `bdrive init --yes` later is always safe. To change
the scope later use `bdrive scope add/rm <dir>`, never hand-edit
`.bdrive/config.json`.

## 4. Confirm the sync hooks

`bdrive init` already did this — do not run a separate hooks command. It
registers turn-boundary hooks (pull before every turn, push right after
edits, stamp changes with the agent session) **once per machine**, in each
platform's own user config: `~/.claude/settings.json`, `~/.codex/hooks.json`,
`~/.gemini/settings.json`, `~/.hermes/config.yaml`. That covers every session
in every folder, and nothing is written inside the project — a hook file in a
synced folder would travel to the whole team.

Read init's output for the platforms it registered and tell the user. One
platform needs a manual step worth passing on: **Codex hooks are experimental
and off by default** — the user enables them with `[features] codex_hooks =
true` in `~/.codex/config.toml`, and Codex asks once to trust the hook.

Only if a platform the user works with is missing from init's output: run
`bdrive hooks install --agent <name>`. `bdrive hooks` shows the status table,
`bdrive hooks uninstall` removes them again.

## 5. Verify, then show the payoff

Init printed the project's hub link and a sync summary — use them rather
than running more commands. Summarize what was set up and hand the user that
link: seeing the folder rendered in the browser is the moment the setup
clicks. Teammate links require sign-in; `bdrive share <file>` exists for
fully public URLs.

Only if something looked wrong in init's output: `bdrive status` shows the
daemon and pending count, and `bdrive url <file>` links a specific file.

## Optional: teach agents about the folder — **ask first**

Offer (never do silently) a two-file orientation: a synced
`<shared>/AGENTS.md` mapping
the folder for the whole team (if one exists already, follow it — don't
rewrite it), and a short pointer section in the repo root's `AGENTS.md` /
`CLAUDE.md` so agents know the folder matters.

## Onboarding teammates

Teammates on any agent paste one prompt into it (the hub's project home
page shows it pre-filled):

```
Follow https://raw.githubusercontent.com/runbear-io/beardrive/main/INSTALL_FOR_AGENTS.md
to set up BearDrive project <project-id> on <hub-url>. Ask me which folder to
sync (the project is named "<project-name>").
```

The project name is in the prompt so the agent can recommend a folder of the
same name; without it, the recommendation is `shared/`.

On BearDrive Cloud, drop `on <hub-url>` — login defaults to beardrive.ai.
