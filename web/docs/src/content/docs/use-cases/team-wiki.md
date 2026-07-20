---
title: Keep a wiki your agents maintain
description: For a team whose knowledge lives in documents nobody updates. The wiki gets written as a side effect of work, every change is attributed, and Insights shows which pages are load-bearing and stale.
---

Every team has the same wiki problem: the pages that matter most are the ones
nobody has touched in a year, and nobody knows which those are.

An agent-maintained wiki inverts it. Knowledge gets written because writing it
is now the cheapest way to finish the task, and the hub tells you which pages
are carrying weight.

## What you end up with

- A wiki that grows as a side effect of work, not as a chore someone schedules.
- Every page attributed: the account, the device, and the agent session behind
  each change, with every past version retained.
- A read map — which pages your team's agents actually consume, and which of
  those are stale.

## Set it up

Inside a repository, sync only the wiki:

> Set up BearDrive here, but only sync `wiki/`.

Git directories never sync (per-file last-writer-wins would corrupt a
repository), and a narrow scope keeps the surface honest. A standalone wiki
folder works the same way — see [Set up with your agent](/start/setup/).

## The loop

Work normally. When something is worth keeping, say where it goes:

> Write up what we learned in `wiki/research/q3-findings.md`.

Next week someone asks their agent about churn. It reads the findings page
first, because it pulls before it answers — then writes what *it* learned back
into the same folder. That is the compounding part.

The wiki improves every time anyone works, which is the only wiki maintenance
model that has ever survived contact with a busy team.

## Reading the health of it

The hub's **Insights** (hub admins and org owners) plots reads against
staleness. The quadrant to watch is **hot and stale**: pages your agents read
constantly that nobody has updated. That is your maintenance queue, and it is
usually three pages, not thirty.

Folder listings show read counts to every member, so the signal is not locked
behind an admin screen.

## What matters for this case

- **[What agents read](/guides/what-agents-read/)** — read heat, Insights, and
  what the hub deliberately does not record.
- **[Shared agent memory](/guides/shared-agent-memory/)** — an `AGENTS.md` map
  at the wiki root so agents file things consistently instead of inventing a
  structure per session.
- **[Scoping the folder](/guides/scoping/)** — subfolder syncing and
  `.bdriveignore`.
