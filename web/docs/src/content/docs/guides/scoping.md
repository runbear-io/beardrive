---
title: Scoping the folder
description: Decide what agents can see — narrow a project to chosen subfolders, change the scope later with bdrive scope, and opt files out with a gitignore-style .bdriveignore.
---

Shared agent memory works better when it's curated. A folder holding
`node_modules/` and build output costs sync bandwidth, buries the documents that
matter, and gives agents thousands of irrelevant paths to wander into.

One mechanism controls it: **`.bdriveignore`**, a gitignore-style rule file at
the mount root. It opts individual paths out, and — as a bdrive-managed block of
"only these folders" rules — it narrows the project to chosen subfolders. Rules
are applied symmetrically: the same filter governs what's read from disk and
what's written back to it.

## Sync only subfolders

A mount is always exactly the folder you name:

```sh
bdrive init wiki    # ./wiki is the project
```

Its contents *are* the project's contents — no `wiki/` prefix on the hub — and
nothing outside it is ever scanned. This is the right shape inside a code
repository: sync `wiki/` or `docs/` and leave the source tree alone. The agent
gets a knowledge folder; the code stays in git where it belongs.

When the project has to be the enclosing folder — several subfolders belonging
to one project, or agents that work from the repo root — narrow it with
`--only`:

```sh
bdrive init . --only wiki,docs
```

Both folders join the same project, with one membership and one permission set.
The interactive `bdrive init` asks the same question.

`--only` writes ordinary `.bdriveignore` rules, in a managed block at the top of
the file:

```gitignore
# bdrive scope — only these folders sync (managed by bdrive; change with `bdrive scope add/rm`)
/*
!/wiki/
!/docs/
# end bdrive scope
```

`/*` excludes everything at the mount root; each `!` line re-includes one
folder, anchored to that root — so a nested directory that happens to share the
name never syncs. The block goes first because matching is last-match-wins:
ordinary rules below it still apply, which keeps `node_modules/` excluded
*inside* a scoped folder.

There is no separate scope setting to keep in step.

:::note[Legacy include lists]
Projects created before the scope moved into `.bdriveignore` carry an `include`
list in `.bdrive/config.json`. It is still honored, but never written any more —
`bdrive scope` reports it and points you at `bdrive init . --only <dirs>` to
move it into `.bdriveignore`.
:::

## The scope is the team's

Because the rules live in `.bdriveignore`, and that file syncs, a narrow scope
is the whole team's scope: every device that syncs the project picks it up.
Widening or narrowing it is a change everyone sees, not a local preference.
(Legacy include lists are the exception — they sit in the never-synced
`.bdrive/config.json` and apply to one device only.)

## Change the scope later

`bdrive scope` shows what syncs; `scope add` / `scope rm` edit the managed block
— no hand-written negation syntax, and the running daemon applies the change
within seconds:

```sh
bdrive scope             # what syncs now
bdrive scope add notes   # also sync ./notes
bdrive scope rm docs     # stop syncing ./docs
```

Both act on an already-narrowed project; on a whole-folder mount `scope add`
points you at `bdrive init . --only <dirs>` and `scope rm` at a plain
`.bdriveignore` rule.

Removing a folder stops syncing it but deletes nothing — local files stay, and
the hub keeps everything already synced (the same
[non-destructive rule](#opting-out-is-non-destructive) as any other
`.bdriveignore` change). Removing the *last* entry is refused, because an empty
block means the whole folder syncs; if you want to stop syncing entirely, that's
`bdrive stop`.

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
A scoped mount is also where the two-file
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

It always syncs — even when the scope block excludes everything around it, and
even if a pattern matches it — so every device shares the same rules: one
person excluding `*.tmp` fixes it for the whole team.

`bdrive init` seeds a starter one covering `node_modules`, build directories,
caches, and `.env*`.

## Opting out is non-destructive

When a pattern starts matching an already-synced file, the file stops syncing
but is **deleted nowhere**. The path is dropped from the local cache without a
delete op, so opting out on your machine never removes the file from anyone
else's.

To take something off the hub as well, use `bdrive forget <path>`: it writes the
rule and removes what already synced from the hub, keeping the local copy on
every device. `bdrive sync --prune` does the same reconciliation for rules you
added by hand — but it **refuses on a scoped project**, because "only these
folders" rules exclude everything else the project holds, and pruning them would
strip all of it from the hub for every teammate, not just this device. Name the
specific paths with `bdrive forget` instead, or widen the rules first.

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
