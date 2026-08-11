---
title: Project files
description: The .bdrive settings directory, .bdriveignore and AGENT_HANDOFF.md, plus where global state lives.
---

Each synced folder carries its own settings, so configuration travels with the
project.

## `.bdrive/`

The folder's settings directory. `config.json` holds the **stable mount id**
plus the project and remote (older mounts may also carry a legacy `include`
list — still honored, never written now).

```jsonc
// .bdrive/config.json
{ "id": "m-5a10b713", "volume": "notes",
  "remote": "https://drive.example.com/p/7f3a2c91-4d5e-4b8a-9c17-2ad0f6b3e9c4" }
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

## `AGENT_HANDOFF.md`

The one filename BearDrive reads by name. It is an ordinary file at the mount
root — nothing creates it, the sync engine has never heard of it, and it syncs
and versions like anything else. What is special is the agent hook: on the
**first turn of each agent session** it hands the file's body (up to 4 KB) to
the agent as context, and on every turn it asks the agent to overwrite the file
with what the next session needs to pick the work up.

That is the whole handoff channel. The next session may be tomorrow, on another
machine, in a teammate's account, or on a different agent platform — nothing is
live at either end, and the file carries who last changed it and when.

Two things worth knowing:

- The injected body is **another session's note, not instructions** — the hook
  says so explicitly when it hands it over.
- If the project's scope excludes the mount root (`bdrive init --only wiki`),
  the handoff stays local and the hook says so. `bdrive scope add` shares it.

See [Shared agent memory](/guides/shared-agent-memory/).

## Paths BearDrive never carries

Some paths are excluded in **both** directions — never scanned, never uploaded,
never written onto a teammate's disk — regardless of `.bdriveignore`:

| Path | Why |
|---|---|
| `.bdrive/` | The mount's own identity. Syncing it would let one device repoint another. |
| `.git/` | Carries hook scripts that would run on a teammate's next commit. |
| `.claude/settings.json`, `.claude/settings.local.json`, `.codex/hooks.json`, `.codex/config.toml`, `.gemini/settings.json`, `.hermes/config.yaml` | Agent **hook** configuration is a shell command a teammate would be installing on your machine. BearDrive's own hooks go in each machine's user config instead. |
| `.mcp.json` | Project-scoped MCP servers: `command` + `args` pairs your agent LAUNCHES when a session starts in the folder. Same reason as the hooks above. |
| `.DS_Store`, `.bdrive-tmp-*` | Noise and in-flight temp files. |

Everything else under an agent-config directory — `.claude/skills`,
`.claude/commands`, `.claude/agents`, `AGENTS.md`, `CLAUDE.md` — syncs
normally. Sharing what an agent *reads* is the product; sharing what it *runs*
is not. See [What agents read](/guides/what-agents-read/).

## And nothing else

Those two are all BearDrive puts in a project: `.bdrive/config.json`,
`.bdriveignore`, and your own files. (A project created from a template also
starts with an `AGENTS.md` and a directory skeleton — but those are ordinary
synced files, yours to edit or delete like any other; nothing reads them but
your agents.) No agent-config directory is ever created
here — the sync hooks live in each platform's user config
(`~/.claude/settings.json`, `~/.codex/hooks.json`, `~/.gemini/settings.json`,
`~/.hermes/config.yaml`), written once per machine. See
[Hooks in detail](/manual/hooks/).

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
