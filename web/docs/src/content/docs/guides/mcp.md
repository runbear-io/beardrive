---
title: Query the hub over MCP
description: A hub project answers MCP at one URL, so an agent in CI, a cloud sandbox, or on a laptop with no BearDrive installed can list, read and inspect the history of its files with no mount and no clone.
---

Normally an agent reaches a BearDrive project through a folder on a machine
running the sync daemon. That is the good path: files on disk, hooks keeping
them fresh, writes going back to the team.

Some agents can't have it. A CI job, a cloud sandbox, a teammate's laptop that
never ran `bdrive init` — none of them can mount anything, and until now none
of them could read the team's folder at all.

**MCP is the read path for agents that cannot mount; hooks remain the write
path.**

## The endpoint

```
POST https://<your-hub>/api/p/<project-id>/mcp
Authorization: Bearer <device token>
```

One URL per project. It speaks streamable-HTTP MCP — JSON-RPC 2.0 objects
posted to that address, no session, no event stream.

The token is the same device token `bdrive login` stores in
`~/.bdrive/settings.json`, and the same one `BDRIVE_TOKEN` sets. Permission is
the permission you already have: the endpoint sits behind the identical
membership check as the file viewer, so a non-member gets exactly the answer
they get from every other project route, and never sees that MCP is there.

Configuring a client is one entry — for Claude Code:

```sh
claude mcp add --transport http beardrive \
  https://<your-hub>/api/p/<project-id>/mcp \
  --header "Authorization: Bearer $BDRIVE_TOKEN"
```

## The three tools

| Tool | Arguments | Answers |
|---|---|---|
| `list_files` | `prefix?`, `limit?` | every file: path, size, last modified, who changed it |
| `read_file` | `path` | the file's current text |
| `file_history` | `path`, `limit?` | versions newest first — who, when, added/edited/deleted |

`read_file` follows renames: a path an agent noted last week still reads the
file after someone moved it, because the hub resolves the old address forward
to the live one.

## What it does not do

- **No content search.** `list_files` matches file *names* and folder paths.
  Searching *contents* works locally today over the files a project syncs
  (`bdrive grep`); the hub-side version needs a content index that does not
  exist yet.
- **No writes.** No upload, no delete, no move. Writing to a project is the
  sync daemon's job, which is also what makes conflict handling and history
  work.
- **Text only.** Files above 1 MiB, and binary files, come back as an error
  naming the file rather than a truncated or mangled body.
- **Hub projects only.** A single-folder `bdrive serve` is the auth-free plain
  viewer and has no MCP endpoint.

## Reads still count

A `read_file` call books an **agent** read for that path, so the hub's [read
heat and Dashboard](/guides/what-agents-read/) show MCP traffic alongside
reads reported by the sync hooks. It is recorded under a fixed actor
(`mcp:remote`) that carries no identity and can never be a real device —
`/heat` stays identity-free.

`list_files` and `file_history` record nothing, matching the viewer's own
folder listings and history pages.
