---
title: Your first hour
description: What a connected folder feels like — ask an agent to write something, get a link back, share it outside the team, and watch a teammate's agent pick it up.
---

Setup is done and nothing looks different: the folder is still a folder, the
files are still files. The change shows up in what you stop doing.

## Ask for something to be written

Work normally. When the agent produces something worth keeping — a plan, a
findings doc, a runbook — ask it to put that in the shared folder:

> Write up what we decided in `wiki/decisions/pricing.md`.

The turn ends, the hook pushes, and the file is on the hub seconds later.
Nobody uploaded anything.

## The link you get back

A connected agent hands you a link to what it just wrote:

```
Saved to wiki/decisions/pricing.md 🔗
```

That is an **internal link** — it opens the file in the hub for anyone signed in
and in the project, and 404s for everyone else. Paste it in Slack without
thinking about it. You can also mint one yourself with `bdrive url <file>`.

For people outside the team, ask for a public one:

> Share that pricing doc with the customer.

The agent runs `bdrive share`, and you get a URL that renders the markdown as a
page — no account needed, revocable, and optionally self-destructing
(`--expires 24h`). [Artifacts and links](/guides/agent-artifacts/) covers both
kinds in depth.

## What a teammate sees

They set their own machine up the same way, in a folder of their choosing. From
then on, their agent starts every turn by pulling — so the pricing doc is simply
*there* the next time they ask about pricing. No one sends anyone a file.

That's the whole thesis: your agent knows what their agent knows.

## Now look at the hub

Open the project in a browser. Three things are worth a minute:

- **History** — every change, with the account, the time, the device, and the
  agent session that made it. Any past version is one click away, and nothing is
  ever deleted.
- **The file browser** — folders show read counts, so you can see which
  documents your team's agents actually consume.
- **Insights** (hub admins and org owners) — reads against staleness. The
  hot-but-stale quadrant is the knowledge everyone relies on and nobody
  maintains. See [What agents read](/guides/what-agents-read/).

## From here on

Every turn, in every connected folder: pull before the agent answers, push after
it edits, stamped with the session that did it. You don't run a command and you
don't think about sync.

Two things repay the ten minutes they cost:

- **[Shared agent memory](/guides/shared-agent-memory/)** — a fresh folder is
  hundreds of opaque files to an agent. A short `AGENTS.md` map fixes that, and
  it syncs with everything else.
- **[Scoping the folder](/guides/scoping/)** — decide what agents can see. Sync
  one subfolder rather than a whole repository, and opt files out with
  `.bdriveignore`.
