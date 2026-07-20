---
title: Turn a personal brain into a company brain
description: You already have a knowledge base your agent reads — an OKF bundle, a gbrain repo, an Obsidian vault. Sync it so every teammate's agent reads the same one, with no export and no schema.
---

You already have the brain. It works, your agent reads it, and it is markdown on
disk. What it doesn't have is teammates.

## Why there is nothing to convert

BearDrive syncs directories of real files. A knowledge base that is already a
folder of markdown needs no export, no adapter, and no schema:

- **[OKF](https://cloud.google.com/blog/products/data-analytics/how-the-open-knowledge-format-can-improve-data-sharing/)**
  bundles — markdown with YAML frontmatter, cross-linked, `index.md` and
  `log.md` at the root.
- **[gbrain](https://github.com/garrytan/gbrain)** brain repos — `people/`,
  `companies/`, `projects/`, `notes/`, one page per concept.
- Obsidian vaults, Logseq graphs, or a folder of notes you never named.

Your format is already the wire format. BearDrive moves the files and stays out
of the way — it has no opinion about frontmatter, link syntax, or folder names.

Derived state stays put too. gbrain keeps its index in PGLite or Postgres and
its config in `~/.gbrain/`, both outside the brain repo, so there is nothing to
exclude. If yours does write an index beside the notes, drop it in
[`.bdriveignore`](/guides/scoping/).

## Set it up

Same as any project — [ask your agent](/start/setup/), in the brain folder:

> Set up BearDrive here and invite my team.

If the brain is a subfolder of something larger, sync just that subfolder:
`bdrive init --shared brain/`. Teammates mount the same project on their own
machines, wherever they like it on disk, and keep running their own local brain
against the synced files.

That is the whole migration. Files start arriving in seconds.

## What you can skip at small scale

gbrain's own company-brain setup shares a brain through infrastructure: a remote
Postgres, an HTTP MCP server, and OAuth credentials scoped per teammate, with
isolation enforced in SQL. That buys real things — one retrieval endpoint, and
per-person access the database itself guarantees.

If what you actually want is *everyone's agent reading the same notes*, file
sync plus each person's local brain gets you there with no shared database, no
server to run, and no connection limits. Keep the server for SQL-enforced
scoping, a single query endpoint, or a brain too large to sit on a laptop.

The two compose. BearDrive distributes the source of truth; anything you run on
top of those files is your business.

## Privacy is per project, not per folder

This is the one place the models differ, and it is worth getting right.

gbrain scopes access per person — `internal/bob/` invisible to Alice, enforced
by OAuth and SQL. **BearDrive's unit of membership is the project**: everyone in
a project sees the whole folder. There is no per-folder permission.

So a walled boundary is a **separate project**:

| Content | Project |
|---|---|
| Company knowledge everyone should have | `company-brain` — whole team |
| HR, comp, performance notes | `people-ops` — that team only |
| A customer's material you can't mix | `customer-acme` — the account team |

One machine can mount all three, in sibling folders. Your agent reads across
them locally, because on disk they're just folders — the wall is on the hub,
where it belongs.

## When two brains rewrite one note

Consolidation passes rewrite markdown. If two teammates' agents rewrite the same
file in the same window, BearDrive resolves it last-writer-wins and preserves
the loser as a conflict copy beside it — nothing is lost, but someone has to
merge the two.

In practice:

- Let **one machine** run scheduled consolidation, not everybody's.
- Give people **their own subfolders** for raw capture (`notes/alice/`), and
  keep the synthesized pages in shared space.
- Ad-hoc edits are fine. The window that matters is seconds, and
  [history](/guides/agent-artifacts/) shows who wrote what if you need to look.

## What you get that the git repo didn't

- **No git ceremony on notes.** No commit, no push, no pull, no merge conflicts
  in prose. Files land in seconds.
- **Provenance per change** — the account, the device, and the agent session
  behind every edit, in the hub's history.
- **Share links** for people outside the team: any page becomes a public URL
  that renders, revocable and optionally expiring.
- **[Read heat](/guides/what-agents-read/)** — which pages your agents actually
  consume, and which ones everyone relies on and nobody maintains. Brains rot
  quietly; this is how you see it.

## Read next

- **[Shared agent memory](/guides/shared-agent-memory/)** — an `AGENTS.md` map at
  the brain's root, so a teammate's agent knows the layout on its first turn.
- **[Scoping the folder](/guides/scoping/)** — subfolder syncing and
  `.bdriveignore`, for the parts that shouldn't travel.
