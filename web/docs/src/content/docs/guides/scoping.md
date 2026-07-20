---
title: Scoping the folder
description: Decide what agents can see — narrow a project to one subfolder, and opt files out with a gitignore-style .bdriveignore.
---

Shared agent memory works better when it's curated. A folder holding
`node_modules/` and build output costs sync bandwidth, buries the documents that
matter, and gives agents thousands of irrelevant paths to wander into.

Two mechanisms control it: an **include list** that narrows the project to a
subfolder, and **`.bdriveignore`** that opts individual paths out. Both are
applied symmetrically — the same filter governs what's read from disk and what's
written back to it.

## Sync only a subfolder

```sh
bdrive init --shared wiki
```

This is the right shape inside a code repository: sync `wiki/` or `docs/` and
leave the source tree alone. The agent gets a knowledge folder; the code stays
in git where it belongs. The interactive `bdrive init` asks the same question.

The result lands in `.bdrive/config.json` as an include list:

```jsonc
{ "id": "m-5a10b713", "volume": "notes",
  "remote": "https://drive.example.com/p/p-7f3a2c91", "include": ["wiki/"] }
```

:::tip
A `--shared` mount is also where the two-file
[`AGENTS.md` pattern](/guides/shared-agent-memory/) earns its keep — the synced
map lives in `wiki/`, and the repo root gets a pointer to it.
:::

## Opt files out

`.bdriveignore` sits at the mount root and works like `.gitignore`:

```gitignore
# comments
node_modules/
*.log
build/
!build/keep.txt
/only-at-root
```

Supported: `#` comments, `*`, `**`, `?`, a trailing `/` for directories, a
leading (or any) `/` for root-anchoring, and `!` to re-include.

It syncs like a normal file, so every device shares the same rules — one person
excluding `*.tmp` fixes it for the whole team.

`bdrive init` seeds a starter one covering `node_modules`, build directories,
caches, and `.env*`.

## Opting out is non-destructive

When a pattern starts matching an already-synced file, the file stops syncing
but is **deleted nowhere**. The path is dropped from the local cache without a
delete op, so opting out on your machine never removes the file from anyone
else's.

## What never syncs

Regardless of configuration:

- **`.git` directories** — per-file last-writer-wins would corrupt a repository.
  If git is the content you want synced, you want git, not BearDrive.
- **`.DS_Store`**.
- **The `.bdrive/` settings directory** — and it holds no credentials; the
  session token stays in `~/.bdrive`.
- **BearDrive's own temp files** (`.bdrive-tmp-*`).
- **Nested mounts** — a subdirectory with its own `.bdrive/config.json` syncs
  only through its own project. The parent never scans into it, writes over it,
  or propagates deletes for it.
- **Empty directories** — not tracked, the same as git.

## A note on secrets

`.bdriveignore` is the mechanism for keeping `.env*` and key material out, and
the seeded default covers the common cases. But treat it as hygiene, not a
security control: any org member can mint a public link for any synced file.
Secrets belong in a secret manager, not in a folder you hand to agents.
