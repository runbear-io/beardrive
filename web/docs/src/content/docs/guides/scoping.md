---
title: Scoping the folder
description: Decide what agents can see — narrow a project to chosen subfolders, change the scope later with bdrive scope, and opt files out with a gitignore-style .bdriveignore.
---

Shared agent memory works better when it's curated. A folder holding
`node_modules/` and build output costs sync bandwidth, buries the documents that
matter, and gives agents thousands of irrelevant paths to wander into.

Two mechanisms control it: an **include list** that narrows the project to a
subfolder, and **`.bdriveignore`** that opts individual paths out. Both are
applied symmetrically — the same filter governs what's read from disk and what's
written back to it.

## Sync only subfolders

```sh
bdrive init --shared wiki
bdrive init --shared wiki,docs   # several subfolders, one project
```

This is the right shape inside a code repository: sync `wiki/` or `docs/` and
leave the source tree alone. The agent gets a knowledge folder; the code stays
in git where it belongs. `--shared` takes one folder or several — comma-separated
or repeated — and they all join the same project, with one membership and one
permission set. The interactive `bdrive init` asks the same question.

The result lands in `.bdrive/config.json` as an include list:

```jsonc
{ "id": "m-5a10b713", "volume": "notes",
  "remote": "https://drive.example.com/p/p-7f3a2c91", "include": ["/wiki/"] }
```

The leading slash anchors each entry to the mount root: `/wiki/` means the
`wiki` folder at the top of this project and nothing else, so a nested
directory that happens to share the name never syncs.

## Change the scope later

`bdrive scope` shows the include list; `scope add` / `scope rm` edit it — no
JSON editing, and the running daemon applies the change within seconds:

```sh
bdrive scope             # what syncs now
bdrive scope add notes   # also sync ./notes
bdrive scope rm docs     # stop syncing ./docs
```

Removing a folder stops syncing it but deletes nothing — local files stay, and
the hub keeps everything already synced (the same
[non-destructive rule](#opting-out-is-non-destructive) as `.bdriveignore`).
Removing the *last* entry is refused, because an empty include list means the
whole folder syncs; if you want to stop syncing entirely, that's `bdrive stop`.

## Check what actually leaves your machine

Rules are one thing; their effect is another. `bdrive scope --explain` walks the
folder and prints every path it found, split into what syncs and what does not:

```sh
bdrive scope --explain
```

```
synced (4)
  .bdriveignore
  docs/architecture.md
  docs/onboarding.md
  specs/BEA-24.md

not synced (2,486)
  .DS_Store
  .bdrive/                       (1 file)
  .env
  .git/                          (312 files)
  node_modules/                  (2,481 files)
  scratch/notes.md
  vendor/acme/                   (own project — syncs separately)

4 files sync, 2,486 do not.
```

A directory that is excluded whole collapses to one counted line, so a folder
with `node_modules` in it prints a handful of lines, not thousands. A nested
mount is labelled rather than called "not synced" — it *does* sync, through its
own project.

The decisions come from the same walk the sync cycle itself uses, so what this
prints cannot drift from what actually leaves. It is a pure read: safe to run
while the daemon is running and while you are offline, it takes no lock and
makes no network call. Output is sorted and stable, which makes it diffable —
the way to prove a rule change did what you meant:

```sh
bdrive scope --explain > before.txt
# edit .bdriveignore
bdrive scope --explain > after.txt
diff before.txt after.txt
```

One thing it does **not** answer: whether a path you exclude *today* is already
on the hub from before the rule existed. Excluding it stops future syncs but
leaves the copy up there — `bdrive forget <path>` is what takes it off.

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

It always syncs — even on an include-list mount where it sits outside the
scope, and even if a pattern matches it — so every device shares the same
rules: one person excluding `*.tmp` fixes it for the whole team.

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
