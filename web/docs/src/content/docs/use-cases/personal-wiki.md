---
title: Run a personal wiki, publish part of it
description: For one person — a researcher, writer, or consultant — who wants a versioned notebook their agent maintains, and the ability to publish a single page without publishing the notebook.
---

You keep notes and your agent helps write them. Two things are missing: a
reliable record of how a document got to its current state, and a way to show
one page to someone without handing over the whole notebook.

## What you end up with

- A notebook your agent reads and writes, on every machine you use.
- Full history — every version of every file, kept forever, attributed to the
  session that wrote it.
- Public links for the pages you choose, revocable and optionally expiring.

## Set it up

You don't need a team or a server. [Set up with your agent](/start/setup/) —
signing up on [beardrive.ai](https://beardrive.ai) creates a free personal
workspace, and `bdrive login` targets it by default.

## Tracking what changed

Ask, rather than diffing:

> What changed in my notes this week?

The hub's **History** answers the same question visually: a feed of every change
with time, device, and agent session, and any past version one click away.
Nothing is deleted — content is content-addressed and retained, so a version
from three months ago is still there.

This is the difference between a notebook and a synced folder: you can always
reconstruct how a conclusion was reached.

## Publishing one page

> Share `research/pricing-teardown.md` publicly, expiring in a week.

You get a URL that renders the markdown as a page. No account needed to read it,
it always serves the file's latest content, and you can revoke it at any time.
The rest of the notebook stays private — sharing is per file, never per folder.

Public pages are sandboxed and rate-limited, and carry a small "Shared with
BearDrive" footer.

## What matters for this case

- **[Artifacts and links](/guides/agent-artifacts/)** — public share links in
  full: expiry, revocation, and how shared files render.
- **[Carry one context across agents and devices](/use-cases/multi-device/)** —
  if the notebook should follow you to a second machine.
