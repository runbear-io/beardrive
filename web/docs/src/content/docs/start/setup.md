---
title: Set up with your agent
description: You don't install BearDrive — you ask your agent to. One command in Claude Code, or one paste in Codex, Gemini CLI, or Hermes, and the folder syncs from then on.
---

You don't set BearDrive up. Your agent does.

Give it one instruction and it installs the CLI, signs this machine in, connects
the folder to a project, and registers the hooks that keep everything in sync —
then tells you what it did. You never open a config file.

## Claude Code and Cowork

Install the plugin once, in any session:

```
/plugin marketplace add runbear-io/beardrive
/plugin install beardrive@beardrive
```

Then, in the folder you want synced:

```
/beardrive:install
```

It walks you through it: creates or connects a project, asks whether to sync the
whole folder or a subfolder like `wiki/`, offers to write the
[agent orientation files](/guides/shared-agent-memory/), and registers the sync
hooks. It asks before anything it changes.

Cowork shares Claude Code's plugins, so installing it once covers both.

Two more commands come with the plugin: **`/beardrive:init`** to start syncing
without the full setup conversation, and **`/beardrive:status`** to diagnose a
sync problem.

:::tip[Project-level hooks reach the whole team]
`/beardrive:install` writes hooks into `.claude/settings.json`, which is
committed with the repository — so **teammates sync whether or not they
installed the plugin**.
:::

## Codex, Gemini CLI, and Hermes

These agents ship no BearDrive knowledge yet, so the instructions travel in the
message. Start the agent in the folder you want synced and paste:

```
Set up BearDrive in this folder.
1. If `bdrive` is missing, install it: brew install runbear-io/tap/beardrive
   (no Homebrew? grab the release binary for this OS/arch from
   https://github.com/runbear-io/beardrive/releases)
2. bdrive skill install   # so you know the CLI next time
3. bdrive login --device https://your-hub   # show me the code and the URL
4. bdrive init --project <project-id>
5. bdrive hooks install   # don't skip this - it's what syncs every turn
Then tell me what got set up.
```

You copy one thing; the agent handles every deviation — already installed, no
Homebrew, browser sign-in, wrong folder.

Step 2 is the durable part. Once the skill is installed the agent knows the CLI
from then on, so "share this file" or "what changed?" work without you
explaining anything again.

:::tip[Don't retype this for teammates]
A project's home page in the hub shows this same paste with your hub URL and
project id already filled in. Send people there.
:::

## What your agent just set up

Two things, worth knowing by name:

- **The skill** — a `SKILL.md` in the agent's own skills directory, teaching it
  the `bdrive` CLI. It is a cross-agent format, so one file works in Claude Code,
  Codex, Gemini CLI, and Hermes.
- **The hooks** — a blocking pull when you send a message, so the agent always
  reads the team's current files, and an async push when the turn ends, so what
  it writes reaches everyone else within seconds.

The hooks are what make syncing automatic, and they are the step people skip
when they set up by hand. [Skills and hooks in detail](/manual/skills-and-hooks/)
covers what gets written where.

## Check it worked

Ask the agent — "is BearDrive set up in this folder?" — or look yourself:

```sh
bdrive status    # projects, daemon state, pending changes
bdrive skill     # which agents know the CLI on this machine
bdrive hooks     # which agents sync this project automatically
```

## Next

[Your first hour](/start/first-hour/) — what the loop feels like once an agent is
connected.

Would rather drive it yourself? [Manual setup](/manual/install/) reaches the same
place, command by command.
