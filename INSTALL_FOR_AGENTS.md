# BearDrive — install instructions for AI agents

You are an AI agent (Claude Code, Codex, Gemini CLI, Hermes, or similar) and
your user asked you to set up BearDrive by following this document. Work
through the steps in order, telling the user what you're doing at each one.
Steps marked **ask first** require the user's explicit answer — do not guess
or skip them.

BearDrive syncs a folder across a team and their agents through a hub, with
per-file history and share links. Setting it up means: install the CLI, sign
in, pick what to sync, and register turn-boundary sync hooks for your
platform. Full documentation: https://docs.beardrive.ai (agent-readable
index at https://docs.beardrive.ai/llms.txt).

> Maintainers: this is a condensed, URL-addressable version of
> `plugin/commands/install.md` — that file is the source of truth. If the
> two disagree, update this one.

## 1. Install the CLI

Check `command -v bdrive`. If missing:

```sh
brew install runbear-io/tap/beardrive      # macOS / Linuxbrew
```

No Homebrew? Grab the release binary for this OS/arch from
https://github.com/runbear-io/beardrive/releases, or with a Go toolchain:
`go install github.com/runbear-io/beardrive/cmd/bdrive@latest`. If nothing
works, stop and tell the user.

## 2. Teach yourself the CLI for future sessions

```sh
bdrive skill install
```

This detects your platform and installs the beardrive skill into its skills
directory, so your later sessions understand `bdrive` conversationally.

## 3. Sign in

Check `bdrive login --status`. If there's no valid session:

- **Default (BearDrive Cloud):** run `bdrive login` — a browser opens
  beardrive.ai where the user signs in or signs up. A new account gets a free
  personal workspace automatically; a pending team invite lands them in that
  team. Warn the user a browser window is coming.
- **Self-hosted hub:** if the user's team runs its own hub, ask for the URL
  and run `bdrive login https://their-hub`.
- **Headless (no browser):** `bdrive login --device <hub-url>` — show the
  user the code and URL it prints.

## 4. Initialize the project — **ask first**

Two questions (or infer from what the user already told you):

1. **Create a new project or connect an existing one?**
   `bdrive init --name <name>` creates-or-joins by name;
   `bdrive init --project <p-id>` connects by id.
2. **Whole folder, or shared subfolders?** Hard rule: **never sync a repo
   root.** Inside a repo, sync scoped subfolders via `--shared`. Scan for
   knowledge folders (`wiki/`, `docs/`, `notes/`, an Obsidian vault —
   markdown-heavy, not source code) and propose candidates for confirmation.
   Whole-folder is only for a dedicated knowledge folder that is the mount
   itself.

If the chosen folder is git-tracked, git and BearDrive would both write it
(silent-revert hazard). Get consent, then `git rm -r --cached <dir>` and add
`<dir>/` to `.gitignore`; stage but let the user commit.

Then run non-interactively:

```sh
bdrive init --name <project> --yes              # dedicated knowledge folder
bdrive init --name <project> --shared wiki,docs # in a repo: only these sync
```

After init: add `.bdrive/` to `.gitignore` (per-machine state, never
committed). Re-running `bdrive init --yes` later is always safe. To change
the scope later use `bdrive scope add/rm <dir>`, never hand-edit
`.bdrive/config.json`.

## 5. Register sync hooks — don't skip

```sh
bdrive hooks install
```

This detects the agent platforms in use and idempotently merges BearDrive's
hooks into each one's config: pull before every turn, push right after
edits, stamp changes with the agent session. Project-level hook files ride
the repo, so teammates get them too. Run `bdrive hooks` to show the status
table and tell the user which platforms got hooks. (Codex users must run
`/hooks` once inside Codex to trust the project layer; Hermes hooks are
per-user — mention that, and skip Hermes unless the user uses it.)

## 6. Claude Code only: install the plugin

If you are Claude Code, also register the marketplace and plugin so future
sessions get the skill, `/beardrive:*` commands, and hooks built in:

```sh
claude plugin marketplace add runbear-io/beardrive
claude plugin install beardrive@beardrive
```

Tell the user the plugin loads on their next session (current session keeps
working via the hooks from step 5). Other platforms: step 2 already covered
you — skip this.

## 7. Verify, then show the payoff

Run `bdrive status` — confirm the daemon is running and pending is 0.
Summarize what was set up, then pick a representative synced file (the
wiki's index/README), run `bdrive url <file>`, and hand the user the link —
seeing the folder rendered in the browser is the moment the setup clicks.
Teammate links require sign-in; `bdrive share <file>` exists for fully
public URLs.

## Optional: teach agents about the folder — **ask first**

Offer (never do silently) the two-file orientation from
`plugin/commands/install.md` step 4: a synced `<shared>/AGENTS.md` mapping
the folder for the whole team (if one exists already, follow it — don't
rewrite it), and a short pointer section in the repo root's `AGENTS.md` /
`CLAUDE.md` so agents know the folder matters.

## Onboarding teammates

Teammates on any agent paste one prompt into it (the hub's project home
page shows it pre-filled):

```
Follow https://raw.githubusercontent.com/runbear-io/beardrive/main/INSTALL_FOR_AGENTS.md
to connect this folder to BearDrive project <project-id> on <hub-url>.
```

On BearDrive Cloud, drop `on <hub-url>` — login defaults to beardrive.ai.
