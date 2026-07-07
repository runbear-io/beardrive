# BearDrive — a synced file system for AI agents

**BearDrive** mounts any folder as a synced volume: its contents stay
synchronized across all your devices through cloud object storage, every
change is tracked (who, when, on which device), and everything keeps
working offline. The CLI is `bdrive`.

It is built for AI agent workflows — give your agents on every machine the
same `~/agent-workspace`, and notes, plans, memory files, and artifacts
follow them everywhere, with a full audit trail of which agent or human
changed what.

```console
$ bdrive mnt ./workspace --remote s3://my-bucket/workspace
mounted /Users/snow/workspace
  volume:  workspace
  remote:  s3://my-bucket/workspace
  device:  macbook (d380dea58598) as snow@runbear.io
  daemon:  running (pid 55434, scan 3s, remote sync 30s)
```

On another machine:

```console
$ bdrive mnt ./workspace --remote s3://my-bucket/workspace
# … the same files appear, and stay in sync from now on
```

## Features

- **Mount anywhere** — `bdrive mnt ./folder` turns any folder into a synced
  volume. Files are *real files on disk*: every tool, editor, and agent can
  use them with zero integration work.
- **Multi-device sync** — devices converge through a shared remote. Each
  device only writes its own append-only journal, so no locking service or
  server is needed — any object store works.
- **Change tracking** — `bdrive log` shows which device and author changed
  which file, when. Content is stored content-addressed, so history is
  never lost, even for overwritten or deleted files.
- **Cloud-provider agnostic** — Amazon S3 (`s3://`), Google Cloud Storage
  (`gs://`), any S3-compatible store (MinIO, Cloudflare R2 via
  `AWS_ENDPOINT_URL`), or a plain shared directory (`file://`, e.g. a NAS).
- **Offline-first** — the working folder is always fully usable with no
  network. Changes are journaled locally and pushed when the remote becomes
  reachable again.
- **Conflict-safe** — concurrent edits resolve deterministically
  (last-writer-wins), and the losing version is preserved as a
  `name.beardrive-conflict-<device>-<time>` file. Nothing is silently dropped.
- **Selective sync** — a gitignore-style `.beardriveignore` opts files out, and an
  optional `include` list in the folder's `.beardrive` settings narrows sync to
  chosen paths.
- **macOS & Linux.**

## Install

```sh
brew install runbear-io/tap/beardrive  # macOS (and Linuxbrew); installs the `bdrive` CLI
```

or from source:

```sh
go install github.com/runbear-io/beardrive/cmd/bdrive@latest
```

## Quick start

```sh
# 1. Mount a folder, syncing through S3 (or gs://, or file://)
bdrive mnt ./notes --remote s3://my-bucket/notes

# 2. Work normally — create, edit, delete files with any tool.
echo "remember this" > notes/memory.md

# 3. On every other device, mount the same remote:
bdrive mnt ./notes --remote s3://my-bucket/notes

# See what changed, who changed it, and from which device
bdrive log ./notes

# Check sync state and the daemon
bdrive status

# Sync on demand (the daemon also syncs automatically)
bdrive sync ./notes

# Stop syncing (files stay on disk; mount again any time)
bdrive umnt ./notes
```

### Credentials

beardrive uses each provider's standard credential chain — nothing beardrive-specific:

| Remote | Credentials |
|---|---|
| `s3://bucket/prefix` | `AWS_PROFILE`, `~/.aws/credentials`, env vars, IAM roles. S3-compatible stores via `AWS_ENDPOINT_URL`. |
| `gs://bucket/prefix` | Application Default Credentials (`gcloud auth application-default login`) or `GOOGLE_APPLICATION_CREDENTIALS`. |
| `file:///path` | none — any local or network-mounted directory |

## Commands

| Command | Description |
|---|---|
| `bdrive mnt <folder> [--remote URL]` | Mount a folder as a synced volume and start the sync daemon |
| `bdrive umnt <folder>` | Stop syncing (`--forget` also unregisters the mount) |
| `bdrive sync [folder]` | Run one sync cycle now |
| `bdrive status [folder]` | Mounts, daemon state, pending changes |
| `bdrive log [folder] [-p path] [-n N]` | Change history: author, device, time, file |
| `bdrive remote [folder]` / `bdrive remote set <folder> <url>` | Show / set the cloud remote |
| `bdrive web [folder \| remote-url]` | Read-only web viewer (rendered markdown, downloads) |
| `bdrive whoami` | Device identity used in change tracking |

## Project files

Each mounted folder carries its own settings, so configuration travels with
the project:

- **`.beardrive`** — the folder's settings (JSON): `volume`, `remote`, and an
  optional `include` list. Written by `bdrive mnt`, safe to hand-edit (a running
  daemon picks changes up automatically). Never synced — remotes are
  device-specific. Copy a folder containing `.beardrive` to another machine and
  plain `bdrive mnt <folder>` reuses its volume and remote.
- **`.beardriveignore`** — gitignore-style opt-out list at the mount root. Syncs
  like a normal file, so every device shares the same rules. Supports `#`
  comments, `*`, `**`, `?`, trailing `/` for directories, leading `/` (or any
  `/`) for root-anchoring, and `!` to re-include.

```jsonc
// .beardrive
{ "volume": "notes", "remote": "s3://my-bucket/notes", "include": ["docs/", "*.md"] }
```

Opting out is non-destructive: when a pattern starts matching an
already-synced file, the file stops syncing but is deleted nowhere.

## Web viewer

`bdrive web` serves a read-only website for a folder or a BearDrive remote —
browse folders and files, read markdown rendered Obsidian-style (including
`[[wikilinks]]`, task lists, and tables), and download any file.

```sh
bdrive web                              # serve the current directory
bdrive web ./notes                      # serve a folder from disk
bdrive web s3://my-bucket/workspace     # serve a BearDrive remote
```

With no remote given it serves the folder straight from the local file
system — on a BearDrive mount the daemon keeps those files fresh, so this is
the simplest way to run it in production (and needs no cloud credentials
on the serving machine). Pointing it at a remote instead reads the object
store directly — no mount, daemon, or local beardrive state — and each file
shows who changed it last, from which device, and when: the same
provenance as `bdrive log`.

Flags: `--addr` (default `:4173`), `--volume` (display name), `--refresh`
(listing cache, default `10s`), `--dir` / `--remote` (explicit forms of
the positional argument).

## Claude Code plugin

Install beardrive support in Claude Code with two commands:

```
/plugin marketplace add runbear-io/beardrive
/plugin install beardrive@beardrive
```

The plugin sets up everything at once:

- **`/beardrive:mount [folder] [remote]`** — one command that installs beardrive if
  needed, mounts the folder (daemon + `.beardrive` config), and verifies the sync.
  `/beardrive:status` diagnoses problems.
- **Turn-boundary sync hooks**, registered automatically: a blocking pull
  when you send a message (Claude always reads fresh files) and an async
  push when the turn ends. The hook no-ops instantly in folders that aren't
  beardrive mounts, so it's safe globally.
- **The `beardrive` skill** ([plugin/skills/beardrive](plugin/skills/beardrive/SKILL.md)),
  covering mount/unmount/sync, backends and credentials, selective sync, and
  troubleshooting. Working in a clone of this repo picks the same skill up
  automatically via `.claude/skills/`.

## How it works

```
working folder  ←materialize/scan→  local volume store  ←push/pull→  object store
 (real files)                       ~/.beardrive/volumes/<vol>              s3:// gs:// file://
                                    ├─ blobs/   content-addressed (sha256)
                                    ├─ journal/ one append-only op log per device
                                    ├─ state.json  what's materialized
                                    └─ sync.json   lamport clock + push cursor
```

- Every change becomes an **op** (`put`/`delete`) in this device's
  append-only journal, stamped with a lamport clock, wall-clock time, device
  ID, and author. File content goes into a content-addressed blob store.
- A **sync** uploads new blobs, then the journal; it downloads other
  devices' journals and any blobs it's missing. Since each device writes
  only its own journal, there are no concurrent writers per object and any
  dumb object store suffices.
- The folder's state is a deterministic **replay** of all journals ordered
  by `(lamport, time, device)` — every device converges to the same view.
  Concurrent edits keep the last writer at the path; the loser is preserved
  as a conflict-copy file by the device that detects the overlap.
- A per-mount **daemon** scans the folder every few seconds (cheap
  size+mtime check) and exchanges with the remote every ~10s — or
  immediately after local edits. Tune with --scan-interval and
  --remote-interval on `bdrive mnt`.

### What beardrive does not sync

`.git` directories (per-file LWW would corrupt repositories), `.DS_Store`,
the `.beardrive` settings file, its own temp files, and anything excluded by
`.beardriveignore` or omitted from an `include` list. Empty directories are not
tracked (like git).

## Roadmap

- `beardrive restore <path>@<time>` — restore any file from history (all content
  is already retained)
- FUSE/NFS mount mode for lazy-loading huge volumes
- Journal compaction & blob GC policies
- Per-path access scopes for multi-agent setups

## Development

```sh
go build ./...
go test ./...
```

The integration tests in `internal/syncer` simulate multiple devices syncing
through a `file://` remote, including offline operation and concurrent-edit
conflicts. Set `BEARDRIVE_HOME` to relocate all beardrive state (used heavily in tests).

## License

MIT
