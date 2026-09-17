---
title: Connect an agent over MCP
description: Give an agent access to your BearDrive projects with no CLI install and no synced folder, by connecting it to the hub's MCP endpoint and picking which projects it may touch.
---

Everything else in this section assumes an agent that runs on a machine with
the CLI installed and the folder synced to disk. That is the right setup for
Claude Code on your laptop, and the wrong one for everything else: an agent in
a browser tab has no disk, a teammate evaluating your project has no reason to
install anything, and an automation has no `$HOME` to sync into.

**MCP** is the other door. If your hub has it enabled, any client that speaks
[MCP](https://modelcontextprotocol.io) — Claude, ChatGPT, Cursor, and others —
can read and write your projects directly, with nothing installed.

## Connect

Point the client at your hub's MCP endpoint:

```
https://your-hub.example.com/mcp
```

The client handles the rest. It registers itself, sends you to a consent
screen on your own hub, and asks you to sign in if you are not already.

The consent screen lists every project you can reach, grouped by
organization, with the access you have on each:

```
Claude wants to access your projects
Signed in as you@example.com

  Acme Inc
    ☐ wiki            you can edit
    ☐ infra           you can edit
    ☐ finance         you can view

              [ Cancel ]  [ Connect selected projects ]
```

**Tick the projects you want this agent to work in.** Nothing is pre-selected,
and one connection can carry several projects. The agent gets access to
exactly what you ticked and nothing else — including projects you create
later, which need a new connection.

## What the agent sees

One filesystem, with each connected project as a top-level folder, named the
way you named it:

```
/
├── wiki/
│   ├── docs/spec.md
│   └── README.md
└── infra/
    └── terraform/main.tf
```

Either the project's name or its id works as that first segment, so
`/wiki/docs/spec.md` and `/6f4a…/docs/spec.md` are the same file.

It can `list`, `read`, `glob` and `grep` across all of them at once, and
`write`, `edit`, `delete` and `move` in the ones you can edit. Two more tools
have no equivalent on a local filesystem:

- **`history`** — who changed a file, when, and the id of every past version.
- **`restore`** — put a previous version back.

Together they are the undo an agent otherwise does not have. "This file looks
wrong, what did it say yesterday, put that back" is two calls.

There is no `mkdir` — directories exist because files are in them, so writing
`/wiki/docs/new/deep/note.md` just works. There is no shell either: an agent
that needs to *run* the code it is editing still needs the CLI and a synced
folder.

`read` normally returns numbered lines, which is what an agent wants to reason
about. When it is *copying* a file it should pass `raw: true` and get the exact
bytes back — numbered output is lossy (a file with and without a trailing
newline look identical), so a copy made from it would not match the original.

## Links back to the hub

Every file the tools name comes back with its hub page beside the path:

```
spec.md   4.1 KB   2026-09-17T09:12Z   sha:1f3c9ab   https://your-hub.example.com/6f4a…/docs/spec.md
```

So an answer can link the file it is talking about, instead of naming a path
you then have to go and find. These are the same gated URLs
[`bdrive url`](/reference/cli/) prints: opening one needs hub sign-in and
membership of the project, which makes them safe to paste into a ticket or a
team chat and useless to anyone outside it. [Public share
links](/guides/agent-artifacts/) stay something you mint deliberately.

The link always carries the project's **id**, even where the path shows its
name. A name is only unambiguous among the projects *you* can see, and a link
is for whoever you send it to.

## What it can and cannot do

The connection acts as **you**, and it can never do more than you can:

- Changes appear in **History** under your name, like anything you do in the
  browser. Your teammates can see what the agent did and undo it.
- A project you can only view is read-only for the agent too.
- A [restricted folder](/guides/scoping/) you cannot see stays invisible to it.
- If someone removes your access to a project, every connection you have made
  loses it at the same moment. There is nothing to revoke separately.
- `write` and `edit` both take the version id the agent read. If a teammate
  changed the file in between, the change is refused and the agent re-reads
  instead of silently overwriting them.
- Moving a file onto one that already exists is refused rather than silently
  replacing it.

Reads through MCP are counted as **agent** reads in the
[read heatmap](/guides/what-agents-read/), so they never inflate the "people
are reading this" signal.

## Disconnect

**Account menu → Connected agents** lists everything you have connected —
which client, which projects, when it was last used — with a Disconnect button
on each.

From a terminal:

```bash
bdrive mcp list
bdrive mcp revoke mcpg_1a2b3c4d
```

Revoking takes effect immediately — the agent's current token stops working on
its next call, not at some later expiry, and its refresh token dies with it. A
connection that goes unused for 30 days expires on its own.

Or over HTTP:

```bash
curl -H "Cookie: <your session>" https://your-hub.example.com/api/mcp/grants
curl -X DELETE -H "Cookie: <your session>" https://your-hub.example.com/api/mcp/grants/<id>
```

## For hub operators

MCP is **off by default**. Turn it on in the hub config:

```json
{ "mcp": { "enabled": true } }
```

See [Hub configuration](/reference/hub-config/) for the endpoints this adds
and how grants are scoped.
