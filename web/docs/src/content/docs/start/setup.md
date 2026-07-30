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
to set up BearDrive project <project-id> on <hub-url>. Ask me which folder to
sync (the project is named "<project-name>").
```

The agent fetches that page and works through it — install the CLI, sign in
(it prints a link you approve in the browser), connect the project, register
the sync hooks. You copy one
thing; the agent handles every deviation — already installed, no Homebrew,
browser sign-in, wrong folder. On BearDrive Cloud, drop `on <hub-url>` —
sign-in defaults to beardrive.ai.

The project name is in the prompt because the agent recommends a folder of
the same name — so `handbook` on the hub is `handbook/` on everyone's disk.
Starting from nothing (no project id, no name), it recommends `shared/` and
names the new project `shared`.

If the folder turns out to be empty once it is connected, the agent offers one
more choice: start from a structure, or from scratch. Three are shipped:

- **Docs + decision records** (`docs/`, `decisions/`) — the boring default, and
  the one to take if you are not sure.
- **LLM wiki** (`sources/`, `wiki/`, `index.md`, `log.md`) — you curate the raw
  material and ask the questions; the agent writes and maintains every page,
  keeps the index current, and health-checks the whole thing for stale claims
  and orphans. It suits accumulating knowledge over weeks — research, a
  competitor file, a company brain fed by transcripts and threads.
- **PARA** (`projects/`, `areas/`, `resources/`, `archives/`) — sorted by how
  actionable something is, with explicit archiving.

Each is a small skeleton plus an `AGENTS.md` telling every agent on the team
where a new file goes, when something is archived or superseded, and what a good
filename looks like. That file is the point; the folders are scaffolding. A
folder that already has files in it is never restructured, and "from scratch"
stays a real answer. The same choices appear when you create a project in the
hub, and on the command line as `bdrive init --template docs`.

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
