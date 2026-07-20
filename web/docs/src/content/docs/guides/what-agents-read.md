---
title: What agents read
description: The hub's read heat and Insights dashboard — which documents your agents actually consume, and which ones everyone relies on and nobody maintains.
---

Writing to shared memory is easy to observe. *Reading* is the part that's
normally invisible — and it's the part that tells you whether the folder is
working.

Hubs track it.

## Three kinds of read

| Kind | Source |
|---|---|
| **Human** | Viewer opens and downloads |
| **Share** | Public `/s/*` link hits |
| **Agent** | Agent tool reads, reported by the sync hooks via `bdrive read-log` — native reads, grep matches, and files named in shell commands |

Sync replication never counts as a read, and neither does viewing a blob in
history. Only genuine consumption.

Agent reads require the hooks from [Set up with your agent](/start/setup/).
Without them you'll see human traffic only.

## In the file browser

Folder listings show heat dots and 30-day read counts to every member. It's the
fastest way to tell which parts of a knowledge folder are load-bearing and which
are decoration.

## Insights

Admins and org owners get an **Insights** dashboard with an all/human/agent
lens — four views:

- **Treemap** — every file, cell size by reads, color by staleness, with ⚠ on
  hot-and-stale. Click through to any file.
- **Reads × freshness scatter** — the hot-but-stale quadrant is the important
  one: knowledge the team relies on that nobody maintains. That quadrant is a
  worklist.
- **Hot path** — top files by reads with the agent/human split. Effectively your
  team's agent context window, measured rather than assumed.
- **Agent coverage matrix** — which agent devices read which folders. Useful for
  spotting an agent that never discovered the folder at all, which usually means
  a missing [root pointer](/guides/shared-agent-memory/).

## Using it

A few things this surfaces that are otherwise guesswork:

- **A document with agent reads and a stale timestamp** is your highest-value
  edit. Agents are grounding answers in it and it's out of date.
- **A document with zero reads after weeks** either isn't discoverable or isn't
  needed. Check `AGENTS.md` mentions it before deleting it.
- **An agent device with narrow coverage** hasn't been oriented. Its machine is
  probably missing the repo-root pointer.

## Privacy

The API (`GET /api/p/<id>/heat?prefix=&days=`) exposes only aggregate counts,
distinct-reader counts, and last-read times. **Never who read what.**

`?by=device` adds an agent-only per-device folder breakdown — device identity is
already public via history. Human email addresses never appear in a heat
response.

Telemetry degrades silently: recording or flushing a read can never fail a
request or a sync cycle.

Hub operators can turn the whole thing off with `"reads": { "enabled": false }`.
See [Hub config](/reference/hub-config/).
