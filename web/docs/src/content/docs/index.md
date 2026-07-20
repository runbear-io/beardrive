---
title: What is BearDrive?
description: BearDrive gives every agent on your team the same folder as memory — real files with provenance, synced across devices and teammates through a hub.
---

BearDrive mounts any folder as a synced volume. Its contents stay synchronized
across all your devices and teammates through a BearDrive **hub**, every change
is tracked (who, when, on which device), and everything keeps working offline.

The CLI is `bdrive`. A hub is a `bdrive web` server you (or we) run on an object
store — clients sync through it over HTTPS and never touch the storage directly.

## What it's for

Sharing context across AI agents. Give every agent on the team the same folder
as memory, and your agent knows what their agent knows. Notes, plans, findings,
and artifacts follow the team everywhere.

Unlike a memory API, they stay **real files with provenance**: every change is
attributed to the human, agent, and device that made it, and the hub's Insights
show what your agents actually read — including the hot-but-stale documents
everyone relies on and nobody maintains.

Because they're real files, every tool, editor, and agent already works with
them. There is no SDK and no integration to write.

People are covered too. Any synced file becomes a public URL that renders as a
page.

## Where to start

- **[Connect an agent](/guides/connect-an-agent/)** — the point of the product.
  Claude Code, Codex, Gemini CLI, or Hermes, reading and writing the shared
  folder every turn.
- **[Quickstart](/start/quickstart/)** — sign in and start syncing a folder, if
  you'd rather see the mechanics first.
- **[Run a hub](/self-hosting/run-a-hub/)** — self-host in about ten minutes.

## What you get

- **Any folder is a project** — `bdrive init` turns any folder into a synced
  project. Files are real files on disk. Rename or move the folder freely;
  state is keyed by a stable id, never the path.
- **Multi-device sync** — devices converge through a shared hub. Each device
  writes only its own append-only journal, so no locking service is needed and
  any object store suffices.
- **Change tracking** — `bdrive log` and the web UI's History view show which
  account changed which file, when, from which device. Every version is
  retained.
- **Read analytics** — the hub records what humans, share links, and *agents*
  actually read, and surfaces the knowledge nobody maintains.
- **Cloud-provider agnostic** — a hub stores on Amazon S3, Google Cloud Storage,
  any S3-compatible store (MinIO, Cloudflare R2), or a plain shared directory.
  Clients never see it.
- **Offline-first** — changes are journaled locally and pushed when the remote
  becomes reachable again.
- **Conflict-safe** — concurrent edits resolve deterministically
  (last-writer-wins), and the losing version is preserved as a
  `name.bdrive-conflict-<device>-<time>` file.
- **Selective sync** — a gitignore-style `.bdriveignore` opts files out, and
  `bdrive init --shared <dir>` narrows sync to one subfolder.
- **macOS and Linux.**

## Hub options

You need a hub to sync through. Two ways to get one:

- **BearDrive Cloud** — zero setup, bare `bdrive login`, free personal
  workspace on signup, at [beardrive.ai](https://beardrive.ai).
- **Self-host** — one static Go binary, one config file, any object store.
  See [Run a hub](/self-hosting/run-a-hub/). It stays a first-class, fully
  supported path forever: AGPL, no features held back.

## License

GNU AGPL-3.0. Everything in the repository is open source and self-hostable — a
complete BearDrive server for one organization's deployment, teams included. The
managed service at beardrive.ai is the same core plus what only makes sense as
an operated service: hosting, SSO, billing and plan quotas, backups, support.
