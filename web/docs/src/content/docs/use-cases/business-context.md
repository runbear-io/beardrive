---
title: Give your coding agents the business context
description: For an engineering team whose customer context lives where their coding agents can't see it. Sync a context folder into the repo so agents build with the calls, renewals, and roadmap rationale in front of them — and write back what shipped.
---

Your coding agent has the whole codebase and none of the reasons. Why this
customer needs the export, which account the deadline is really for, what was
promised on last week's call — all of it lives in a meeting tool, a CRM, or
someone's head. So the agent writes technically correct code that solves the
wrong problem, and the correction arrives in review.

The fix is not another integration. It is a folder of markdown, synced into the
repo, that the agent reads the same way it reads `src/`.

## What you end up with

- A `context/` folder inside the repo, synced from the hub, that every coding
  agent on the team reads before it plans.
- Context written by the people who have it — sales, CS, support, PM — from
  their own agents, without any of them opening the repo.
- A path back: what shipped, what was deferred, and the known limitations, in
  the same folder, where the customer-facing team's agents pick it up.

## Set it up

Ask your agent, in the repo:

> Set up BearDrive here, syncing only `context/`.

That runs `bdrive init --shared context`, which writes `.bdrive/config.json`,
creates the folder, and starts the daemon. Nothing outside `context/` is
scanned or sent — `.git/` and `.bdrive/` never sync at all.

### Then tell git to ignore it

This step is not optional. Add to `.gitignore`:

```gitignore
context/
.bdrive/
```

A path must have **one writer**. If `context/` is also git-tracked, a
teammate's `git pull` or branch switch rewrites those files with older content,
and sync broadcasts that as a fresh edit — silently reverting the team's latest
notes. Git moves the code; BearDrive moves the context; they never touch the
same paths.

If the folder is already committed, hand it over rather than deleting it:
`git rm -r --cached context` and commit the `.gitignore` change. Teammates pull,
run the same setup, and identical content converges with no conflicts.

### Point the agent at it

Add a few lines to the repo's `AGENTS.md` (or `CLAUDE.md`) saying what is in
`context/` and when to read it. Without it, agents treat the folder as
decoration:

```markdown
## Business context

`context/` is synced from BearDrive — customer calls, account notes, and
roadmap rationale, maintained by the GTM team. Read it before planning any
customer-facing change. It is not git-tracked; do not commit it.
```

See [Shared agent memory](/guides/shared-agent-memory/) for the two-file
pattern this follows.

## The loop

**Context in.** A CS lead finishes a renewal call and asks their own agent to
write it up. They are not in the repo and never will be — they are in a folder
on their laptop that happens to be the same project:

> Write up the Acme renewal call in `customers/acme/`.

Seconds later it is on every engineer's disk, under `context/customers/acme/`.

**Context used.** An engineer starts work:

> Add bulk export to the reporting page.

Their agent reads the context folder before it plans, and comes back with the
constraint nobody put in the ticket — Acme needs CSV specifically, because the
renewal call flagged their finance team can't ingest JSON. That is the whole
point: the objection arrives before the code, not in review.

**Context back.** When it ships:

> Note what we shipped for Acme in `context/customers/acme/shipped.md`.

The CS lead's agent reads that before the next call. The folder is a loop, not a
feed — engineering is a producer of context too, and the round trip is what
keeps the GTM side from promising things that were quietly deferred.

## Getting notes in from meeting tools

BearDrive has no connectors, and does not need one to be useful here: exports
are files. Gong, Granola, Fathom and most CRMs will drop a markdown or text
transcript, and anything that lands in the folder is on every machine seconds
later. A scheduled job that writes exports into the folder is a normal writer
like any other.

Two things worth deciding before you turn the tap on:

- **Raw transcripts are not context.** A folder of hour-long transcripts makes
  agents slower and less accurate, not better. Have the writing agent produce a
  short summary and keep the transcript out, or park raw material in a
  subfolder the repo's agents are told to skip.
- **One writer per path.** If a job rewrites `customers/acme/notes.md` on a
  schedule and a human's agent edits the same file, you get conflict copies.
  Give automation its own subfolder.

## Keep it small, and keep it clean

Everything in `context/` lands on every engineer's laptop. That is the feature,
and it is also the thing to be deliberate about.

- **Scope by project, not by folder.** Membership is per project, and everyone
  in a project sees all of it. Material that shouldn't reach the whole
  engineering team belongs in a *separate project* — not a subfolder. See
  [Turn a personal brain into a company brain](/use-cases/company-brain/) for
  how that model works.
- **Opt things out** with `.bdriveignore` — see
  [Scoping the folder](/guides/scoping/). On a `--shared` mount the seeded root
  `.bdriveignore` sits outside the include list and stays local, so put shared
  rules inside `context/` instead.
- **Watch what actually gets read.** [Read heat](/guides/what-agents-read/)
  shows which context pages agents consume. Pages nothing has read in a month
  are candidates for deletion, and deleting them makes the rest work better.

## What matters for this case

- **[Scoping the folder](/guides/scoping/)** — subfolder syncing and
  `.bdriveignore`, which is the whole mechanism this case rests on.
- **[Shared agent memory](/guides/shared-agent-memory/)** — the `AGENTS.md`
  pointer that makes agents actually open the folder.
- **[Share work across your team's agents](/use-cases/team-artifacts/)** — the
  general version, for teams whose shared folder isn't attached to a repo.
