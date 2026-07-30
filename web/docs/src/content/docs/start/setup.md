---
title: Set up with your agent
description: You don't install BearDrive — you ask your agent to. One paste in Claude Code, Cowork, Codex, Gemini CLI, or Hermes, and the folder syncs from then on.
---

You don't set BearDrive up. Your agent does.

Give it one instruction and it installs the CLI, signs this machine in, connects
the folder to a project, and registers the hooks that keep everything in sync —
then tells you what it did. You never open a config file.

## Any agent: one paste

Start the agent — Claude Code, Cowork, Codex, Gemini CLI, Hermes — in the folder
you want synced, and paste:

```
Follow https://raw.githubusercontent.com/runbear-io/beardrive/main/INSTALL_FOR_AGENTS.md
to set up BearDrive project <project-id> on <hub-url>. Ask me which folder to sync.
```

The agent fetches that page and works through it — install the CLI, sign in
(it prints a link you approve in the browser), connect the project, register
the sync hooks. You copy one
thing; the agent handles every deviation — already installed, no Homebrew,
browser sign-in, wrong folder. On BearDrive Cloud, drop `on <hub-url>` —
sign-in defaults to beardrive.ai.

The hooks step is the durable part: once they are registered, every later
session in every folder syncs automatically, with nothing to remember.

:::tip[Don't retype this for teammates]
A project's home page in the hub shows this same paste with your hub URL and
project id already filled in. Send people there.
:::

## What your agent just set up

The sync hooks, and nothing else you have to think about:

- a **blocking pull** when you send a message, so the agent always reads the
  team's current files — it also tells the agent your project's link
  convention, so it can hand you a hub link for any file it mentions;
- an **async push** after every file edit, so what it writes reaches everyone
  else within seconds;
- **read tracking**, so the hub's Dashboard can show what your agents actually
  read.

They go in the agent's user config, once per machine, and are a no-op outside
BearDrive folders. Your agent also registers a login item, so syncing comes
back on its own after a reboot rather than waiting for the next session. `bdrive init` registers them for you, so there is nothing
extra to run — [hooks in detail](/manual/hooks/) covers what gets written
where.

## Check it worked

Ask the agent — "is BearDrive set up in this folder?" — or look yourself:

```sh
bdrive status    # projects, daemon state, pending changes
bdrive hooks     # which agents on this machine sync automatically
```

## Next

[Your first hour](/start/first-hour/) — what the loop feels like once an agent is
connected.

Would rather drive it yourself? [Manual setup](/manual/install/) reaches the same
place, command by command.
