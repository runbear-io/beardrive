---
title: Migrate between hubs
description: Move a project from one BearDrive hub to another — cloud to self-hosted or back — with full history, step by step.
---

`bdrive export` and `bdrive import` move a whole project between hubs — from
BearDrive Cloud to your own server, from a self-hosted hub into the cloud, or
between two self-hosted hubs. The archive carries every device's journal and
every retained content blob, so **per-file history and authorship arrive
intact**, and devices that later connect to the migrated project resume
exactly where they left off.

You never need server access on either end. Both commands run on any device
that's a member of the project.

## What you need

- A device that syncs the project (any member device works).
- An account on the destination hub (sign up there first if you don't have one).
- Uploads enabled on the destination hub (`--upload` on a self-hosted
  `bdrive web`; BearDrive Cloud has them on).

## Step 1 — sync, so the export is complete

On the device doing the migration:

```sh
cd ~/team-wiki
bdrive sync
```

Export copies from the **hub**, not from your folder, so anything not pushed
yet won't be in the archive. `export` warns if it sees unpushed local changes.
If teammates are actively editing, ask them to pause — changes they push after
your export stays behind on the old hub.

## Step 2 — export

```sh
bdrive export ~/team-wiki -o team-wiki.tar.gz
```

```
exported "team-wiki": 3 journal(s), 128 blob(s), 42.7 MB → team-wiki.tar.gz
```

The archive is the project's store laid out as a tar.gz — `journal/` op logs
plus content-addressed `blobs/` — with a small manifest naming the project.
It contains every version of every file, not just the latest, so it doubles
as a full offline backup.

## Step 3 — log into the destination hub

```sh
bdrive login https://your-hub.example
```

This switches the device's session. The old hub is untouched — the project
there keeps working for everyone else until you decide otherwise.

## Step 4 — import

```sh
bdrive import team-wiki.tar.gz
```

```
imported into "team-wiki" (p-4e1a9b02, created on https://your-hub.example): 3 journal(s), 128 blob(s), 42.7 MB

connect a folder to it:  bdrive init --project p-4e1a9b02
```

Import creates the project (named from the archive; `--name` overrides),
verifies each blob's content hash on the way up, and refuses to write into a
project that already has content — a typo can't merge two histories.

## Step 5 — reconnect devices

On each device that should follow the project to its new home:

```sh
bdrive login https://your-hub.example
cd ~/team-wiki && bdrive init --project p-4e1a9b02
```

Because the journals were copied byte-for-byte, a reconnecting device's local
state already matches the new hub — the first sync just confirms it. Invite
teammates to the destination hub's org so they can connect too.

## Step 6 — retire the old project

Nothing does this automatically, on purpose: the old project stays intact
until you remove it from the old hub. Run both in parallel as long as you
like — but note the two hubs don't sync with each other; edits made against
the old hub after the export won't follow.

## What migrates, what doesn't

| Migrates (in the archive) | Stays behind (hub metadata) |
|---|---|
| Every file, every version | Public share links |
| Per-file history and authorship | Org membership and invite links |
| All devices' journals | Read-heat / insights data |
| Conflict copies | Device registry entries |

Recreate shares with `bdrive share` and re-invite teammates from the new
hub's UI.

## Troubleshooting

- **"the project's hub is unreachable"** — export talks to the hub; get
  online (or fix the hub) first.
- **"uploads are disabled on this server"** — start the destination hub with
  `--upload` (or `upload: true` in its config file).
- **"project … already has content"** — you're importing into a non-empty
  project. Pass `--name something-new` to create a fresh one.
- **"corrupt archive"** — a blob's bytes don't match its content hash. Re-run
  the export; don't trust that archive.
- Import counts against the destination org's storage quota on managed hubs.
