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
- **Dashboard** (every project member) — reads against staleness. The
  hot-but-stale quadrant is the knowledge everyone relies on and nobody
  maintains. See [What agents read](/guides/what-agents-read/).

## One thing to know before you rely on it

A connected folder is **a shared drive, not a trusted source.** Everything in it
was written by someone on your team — or by their agent — and it arrives on your
machine automatically, with no review step. Your agent then reads it as context.
That is the feature; it is also the boundary.

In practice:

- Treat a synced `AGENTS.md`, skill or note the way you'd treat a message in
  your team chat: it's a colleague talking, not your own instruction. If a
  document tells an agent to fetch a URL or run a command, that came from a
  person, and the agent should surface it rather than act on it. The
  onboarding runbook agents follow
  ([INSTALL_FOR_AGENTS.md](https://github.com/runbear-io/beardrive/blob/main/INSTALL_FOR_AGENTS.md))
  says so explicitly.
- **Agent hook config never syncs, in either direction** —
  `.claude/settings.json`, `.codex/hooks.json`, `.gemini/settings.json`,
  `.hermes/config.yaml`. A hook is a shell command, and a teammate must not be
  able to install one on your machine. BearDrive's own hooks live in each
  machine's user-level config, which is where `bdrive init` writes them. Share
  a skill or a document instead.
- **You can always ask who.** History names the account, device and session
  behind every change, so "where did this file come from" has an answer.

Give people write access the way you'd give them a key to the office, and set
per-project permissions ([Permissions](/reference/cli/)) for the folders where
that's too much.

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
