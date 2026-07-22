---
title: Share work across your team's agents
description: For a team that doesn't live in a terminal — marketing, ops, design, founders working in Claude Cowork or Claude Code. Everything your agents produce lands in one folder the whole team's agents can read.
---

Your team works with agents all day and the output goes nowhere. A brief lives
in one person's chat history, a competitive analysis in another's, and the third
person's agent re-does work that already exists.

## What you end up with

- One folder every agent on the team writes into and reads from.
- Whatever your agent produces is on a teammate's machine seconds later —
  nobody attaches, uploads, or pastes anything.
- A link you can send a person. Markdown and HTML render as pages, so anything
  an agent wrote is already readable at a URL — one link that opens only for
  members, one public link for anyone outside.

## Set it up

Install the plugin once — Cowork and Claude Code share plugins, so this covers
both — then run `/beardrive:install` in the folder you want shared.
[Set up with your agent](/start/setup/) has the two commands.

Nobody on the team needs to open a terminal: the agent installs what it needs
and reports back. Invite teammates from the hub's sidebar (**Manage → New
invite**); the link creates their account and adds them in one step, and they
run the same `/beardrive:install` on their own machine.

If the shared work should live in one subfolder rather than everything on your
disk, say so — "sync only `client-work/`" — and the agent scopes it.

## The loop

> Draft the Q3 campaign brief and put it in `campaigns/`.

The turn ends and the brief is on the hub. Your colleague, an hour later, asks
their own agent about Q3 — and it has already read the brief, because it pulls
the team's current files before it answers. Nobody sent anything.

Agents aren't the only readers. When a person needs to see it, send the hub
link rather than the file — the hub renders markdown and HTML as pages, so
paste it into Slack, an email or a ticket and teammates get the current version
as a document. Anyone outside the org gets a wall, so the link is safe to
forward internally. (Your agent appends one of these to every synced path it
mentions, so there's usually nothing to go and copy.)

When it needs to leave the company:

> Share the brief with the agency.

You get a public URL that renders the document as a page — no account required,
revocable, and it can expire on its own.

## What matters for this case

- **[Shared agent memory](/guides/shared-agent-memory/)** — write a short map of
  the folder so every teammate's agent knows where things go. Without it, five
  agents invent five folder structures.
- **[Artifacts and links](/guides/agent-artifacts/)** — internal links for
  teammates, public links for clients, and which to use when.
