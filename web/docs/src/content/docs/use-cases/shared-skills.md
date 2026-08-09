---
title: Give every agent on the team the same skills
description: For the person on the team who writes the good prompts — the one whose agent always seems to know the right steps. Skills, commands and subagents sync like any other file, so what you teach your agent, everyone's agent knows.
---

You are the person on the team who writes the good skills. Yours knows the
deploy checklist, the query that answers the recurring question, the three
gotchas in the billing code. Everyone else's agent re-derives them every week,
and the only way you have to share one is to paste a file into chat.

## What you end up with

- One folder holding the team's skills. A skill you write is on a teammate's
  disk before their agent's next turn — no export step, no registry, no MCP
  server to configure per client.
- The same for `.claude/commands`, `.claude/agents`, `AGENTS.md` and
  `CLAUDE.md`. Everything an agent *reads* travels; agent **hook**
  configuration never does, because a hook is a shell command and syncing one
  would install it on your teammate's machine. See
  [What agents read](/guides/what-agents-read/).
- Change history per skill: who edited it, when, from which device, and every
  previous version. A skill that got worse is one `bdrive log` away from
  showing you when.

## Set it up

Start your agent in the folder you want shared and give it the one paste from
[Set up with your agent](/start/setup/). Ask for the skills structure and it
seeds one — an `AGENTS.md` explaining how the library is kept plus an example
`.claude/skills/<name>/SKILL.md` to copy:

> Set up BearDrive here from the `skills` template.

For a project's skills, sync the project folder your agent already starts
sessions in and the `.claude/` inside it comes along. For a library that is not
tied to one project, sync `~/.claude/skills` — **the `skills` directory,
never `~/.claude` itself**, which also holds this machine's credentials and
every saved session. `bdrive init` refuses that directory for exactly that
reason and points at `skills` instead.

## The loop

> Write a skill for our release checklist and put it in `.claude/skills/`.

The turn ends and the skill is on the hub. Your colleague's next session picks
it up on its own — their pull hook runs before their agent's first turn, so the
file is simply there, on disk, the way a skill they wrote themselves would be.
Nobody installed anything.

When the checklist changes, edit the skill. Everyone is on the new one by their
next session, and the hub's History view shows what it said before.

## Use it with, not instead of

A skills *registry* answers "who is allowed to publish this, and was it
reviewed" — governance before the fact. BearDrive answers arrival: the file is
on the machine, current, before the agent's first turn. If your team needs
approval gates, keep them and let BearDrive be the delivery; the two do
different jobs.

## What matters for this case

- **[What agents read](/guides/what-agents-read/)** — exactly which agent files
  sync and which never do, and why the line falls there.
- **[Shared agent memory](/guides/shared-agent-memory/)** — the `AGENTS.md`
  that tells every teammate's agent how the library is kept.
- **[Project files](/reference/project-files/)** — the full table of paths
  BearDrive excludes in both directions.
