---
title: Project files
description: The .bdrive settings directory and .bdriveignore, plus where global state lives.
---

Each synced folder carries its own settings, so configuration travels with the
project.

## `.bdrive/`

The folder's settings directory. `config.json` holds the **stable mount id**
plus project, remote, and include settings.

```jsonc
// .bdrive/config.json
{ "id": "m-5a10b713", "volume": "notes",
  "remote": "https://drive.example.com/p/p-7f3a2c91" }
```

Written by `bdrive init` and safe to hand-edit — a running daemon picks changes
up automatically. Which subfolders sync is *not* stored here: that lives in
`.bdriveignore` (see below), edited with `bdrive scope add`/`rm`. Mounts created
before that change may still carry an `include` list here; it is still honored,
but nothing writes it any more.

It is **never synced** and holds **no credentials**; the session token stays in
`~/.bdrive`.

Because everything is keyed by the mount id, the folder can be renamed or moved
freely. Copy it to another machine and `bdrive init` resumes the same project.

## `.bdriveignore`

A gitignore-style opt-out list at the mount root. It syncs like a normal file,
so every device shares the same rules. See
[Scoping the folder](/guides/scoping/).

## And nothing else

Those two are all BearDrive puts in a project: `.bdrive/config.json`,
`.bdriveignore`, and your own files. No agent-config directory is ever created
here — the sync hooks live in each platform's user config
(`~/.claude/settings.json`, `~/.codex/hooks.json`, `~/.gemini/settings.json`,
`~/.hermes/config.yaml`), written once per machine. See
[Skills and hooks in detail](/manual/skills-and-hooks/).

## Global state

Everything else lives under `$BDRIVE_HOME` (default `~/.bdrive`):

| Path | Contents |
|---|---|
| `device.json` | This device's identity, used in change tracking |
| `settings.json` | Default server, device token, signed-in account |
| `mounts.json` | Mount registry, keyed by stable mount id, holding each mount's last-known path |
| `volumes/<mount-id>/` | The local volume store: blobs, journals, materialization cache, sync state |

Nothing is keyed by folder path, which is why moves and renames are free.
`ResolveMount` self-heals the registry path on the next command.

## The volume store

```
~/.bdrive/volumes/<mount-id>/
├─ blobs/      content-addressed file content (sha256)
├─ journal/    one append-only op log per device
├─ state.json  what's materialized
└─ sync.json   lamport clock + push cursor
```

Also here for a running project: `daemon.pid` and `daemon.log`.
