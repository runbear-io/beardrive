---
title: Carry one context across agents and devices
description: For one person running several machines and more than one agent — laptop, desktop, a server, Claude Code and Codex. One project, mounted everywhere, so whichever agent you talk to starts from the same state.
---

The work is continuous; the machines are not. You start something in Claude Code
on the laptop, pick it up in Codex on the desktop, and the second agent knows
nothing about the first.

## What you end up with

- One project, mounted on every machine, in whatever folder each one prefers.
- Every agent on every device starts its turn from the same files.
- Offline is normal, not an error state — changes queue and reconcile when the
  machine is reachable again.

## Set it up

[Set up with your agent](/start/setup/) on the first machine. On each additional
one, connect the *same project* rather than making a new one — the agent needs
the project id, which the hub shows on the project's home page:

> Set up BearDrive here, connecting to project `p-79d0a07c`.

The folder does not have to match across machines. State is keyed by a stable
project id, never a path, so `~/work` on one and `~/Documents/work` on another
are the same project. Moving or renaming a folder later is free.

Install the skill and hooks **once per machine**, and the hooks once per project
— that is what makes each agent pull before it answers. See
[skills and hooks in detail](/manual/skills-and-hooks/).

## More than one agent per machine

The skill is a cross-agent format and the hooks are written per platform, so
Claude Code, Codex, Gemini CLI, and Hermes can all be wired into the same folder
at once. They read the same files and their writes are stamped with which agent
session made them, so `bdrive log` and the hub's history stay legible even when
four agents share a project.

## What happens when two machines edit the same file

Each device writes only its own append-only journal, and every device replays
all journals in the same deterministic order — so they converge without a
locking service.

If the same file was edited on two machines in the same window, the later write
wins and the other is preserved as a conflict copy beside it. Nothing is lost;
you decide what to merge. Working offline for a day and reconnecting is the same
mechanism, just with a longer window.

## What matters for this case

- **[Skills and hooks in detail](/manual/skills-and-hooks/)** — what to run on
  each new machine, and why the pull hook is the one that matters.
- **[How sync works](/concepts/how-it-works/)** — journals, blobs, and
  deterministic replay, if you want to know why this converges rather than
  hoping it does.
