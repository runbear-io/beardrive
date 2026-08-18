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
Follow beardrive.ai/setup to set up BearDrive. Ask me which folder to sync.
```

`beardrive.ai/setup` redirects to
[INSTALL_FOR_AGENTS.md](https://github.com/runbear-io/beardrive/blob/main/INSTALL_FOR_AGENTS.md),
the runbook the agent actually follows. It fetches that page and works through
it — install the CLI, sign in (it prints a link you approve in the browser),
create the project, register the sync hooks. You copy one thing; the agent
handles every deviation — already installed, no Homebrew, browser sign-in,
wrong folder. The instructions live at that URL rather than inside the prompt,
so they never go stale in someone's copy.

**"Ask me which folder to sync" is doing real work — keep it.** Without it,
an agent reads the rest of the sentence as permission to decide for you, and
the usual guess is *this whole folder*, which is the one answer the runbook
tells it never to recommend. With it, the agent stops and offers you a named
recommendation first. Starting from nothing it suggests creating `shared/`
and names the new project `shared`; if it finds a folder that already looks
like notes, it suggests that one instead.

**Self-hosting?** Say where: `Follow beardrive.ai/setup to set up BearDrive on
https://hub.example.com. Ask me which folder to sync.` On BearDrive Cloud
there is nothing to add — sign-in defaults to beardrive.ai.

**Joining a project a teammate already made?** Use the pre-filled paste from
that project's home page in the hub (see the tip below) rather than this one.
It carries the project's id and name, so the agent joins the existing project
instead of creating a new one — and recommends a folder named after it, so
`handbook` on the hub is `handbook/` on everyone's disk.

If the folder turns out to be empty once it is connected, the agent offers one
more choice: start from a structure, or from scratch. Four are shipped:

- **Docs + decision records** (`docs/`, `decisions/`) — the boring default, and
  the one to take if you are not sure.
- **LLM wiki** (`sources/`, `wiki/`, `index.md`, `log.md`) — you curate the raw
  material and ask the questions; the agent writes and maintains every page,
  keeps the index current, and health-checks the whole thing for stale claims
  and orphans. It suits accumulating knowledge over weeks — research, a
  competitor file, a company brain fed by transcripts and threads.
- **PARA** (`projects/`, `areas/`, `resources/`, `archives/`) — sorted by how
  actionable something is, with explicit archiving.
- **Shared agent skills** (`.claude/skills/`) — a skill library the whole
  team's agents load, kept current by syncing. See
  [Give every agent on the team the same skills](https://beardrive.ai/use-cases/).

Each is a small skeleton plus an `AGENTS.md` telling every agent on the team
where a new file goes, when something is archived or superseded, and what a good
filename looks like. That file is the point; the folders are scaffolding. A
folder that already has files in it is never restructured, and "from scratch"
stays a real answer. The same choices appear when you create a project in the
hub, and on the command line as `bdrive init --template docs`.

The hooks step is the durable part: once they are registered, every later
session in every folder syncs automatically, with nothing to remember.

:::tip[Don't retype this for teammates]
A project's home page in the hub shows a paste with your hub URL and project
id already filled in. Send people there rather than asking them to assemble
one — it is the difference between a teammate joining your project and a
teammate creating a second one beside it.
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
