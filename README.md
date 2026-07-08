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
  `name.bdrive-conflict-<device>-<time>` file. Nothing is silently dropped.
- **Selective sync** — a gitignore-style `.bdriveignore` opts files out, and an
  optional `include` list in the folder's `.bdrive` settings narrows sync to
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
| `https://host:port/p/<id>` | none — syncs through a bdrive web hub; only the server holds storage credentials (see [The sync hub and `bdrive init`](#the-sync-hub-and-bdrive-init)) |

## Commands

| Command | Description |
|---|---|
| `bdrive mnt <folder> [--remote URL]` | Mount a folder as a synced volume and start the sync daemon |
| `bdrive umnt <folder>` | Stop syncing (`--forget` also unregisters the mount) |
| `bdrive sync [folder]` | Run one sync cycle now |
| `bdrive status [folder]` | Mounts, daemon state, pending changes |
| `bdrive log [folder] [-p path] [-n N]` | Change history: author, device, time, file |
| `bdrive remote [folder]` / `bdrive remote set <folder> <url>` | Show / set the cloud remote |
| `bdrive login <server-url>` | Set this device's bdrive web server (verified and remembered) |
| `bdrive init [folder]` | One-command onboarding: join/create a project on the logged-in server, seed `.bdriveignore`, mount, start syncing |
| `bdrive web [folder \| storage-root-url]` | Web server: viewer (rendered markdown, downloads), uploads, multi-project sync hub |
| `bdrive whoami` | Device identity used in change tracking |

## Project files

Each mounted folder carries its own settings, so configuration travels with
the project:

- **`.bdrive`** — the folder's settings (JSON): `volume`, `remote`, and an
  optional `include` list. Written by `bdrive mnt`, safe to hand-edit (a running
  daemon picks changes up automatically). Never synced — remotes are
  device-specific. Copy a folder containing `.bdrive` to another machine and
  plain `bdrive mnt <folder>` reuses its volume and remote.
- **`.bdriveignore`** — gitignore-style opt-out list at the mount root. Syncs
  like a normal file, so every device shares the same rules. Supports `#`
  comments, `*`, `**`, `?`, trailing `/` for directories, leading `/` (or any
  `/`) for root-anchoring, and `!` to re-include.

```jsonc
// .bdrive
{ "volume": "notes", "remote": "s3://my-bucket/notes", "include": ["docs/", "*.md"] }
```

Opting out is non-destructive: when a pattern starts matching an
already-synced file, the file stops syncing but is deleted nowhere.

## Web server

`bdrive web` serves a website — browse folders and files, read markdown
rendered Obsidian-style (including `[[wikilinks]]`, task lists, and
tables), download any file — and, pointed at a storage root, becomes a
**multi-project sync hub**. It is read-only unless started with `--upload`.

```sh
bdrive web                              # serve the current directory (viewer)
bdrive web ./notes                      # serve a folder from disk (viewer)
bdrive web -c config.json               # everything from a config file
bdrive web s3://my-bucket/root --upload # multi-project sync hub
```

With a folder it serves files straight from disk — on a BearDrive mount the
daemon keeps them fresh, so this is the simplest read-only deployment (no
cloud credentials on the serving machine). With a storage root URL it runs
in hub mode, described below.

Flags: `--addr` (default `:4173`), `--volume` (display name), `--refresh`
(listing cache, default `10s`), `--dir` / `--remote` (explicit forms of
the positional argument), `--upload` (allow client writes, off by default),
`--upload-ttl` (presigned-URL lifetime, default `15m`), `--projects-db`
(hub project registry file, default `$BDRIVE_HOME/projects.json`),
`-c/--config` (read all of the above from a JSON file; explicit flags win):

```jsonc
// bdrive web -c config.json
{
  "remote": "s3://my-bucket/root",   // storage root (hub) — or "dir": "./folder" (viewer)
  "addr": ":4173",
  "upload": true,
  "upload_ttl": "15m",
  "refresh": "10s",
  "projects_db": "/var/lib/bdrive/projects.json"
}
```

### The sync hub and `bdrive init`

In hub mode the server hosts many **projects** on one storage root — each
project's data lives under its own prefix (`<root>/<project-id>/`), and a
file-backed registry (`projects.json`, loaded at start, rewritten
atomically on every change) maps project ids to names. Client devices sync
whole folders through the hub without ever knowing where the storage is or
holding any cloud credentials; the server device is the only one configured
with the bucket.

```sh
# On the server device (knows the storage)
bdrive web -c config.json

# On each client device (knows only the server) — one command does it all:
bdrive login https://drive.example.com:4173   # once per device
cd ~/some-project && bdrive init              # once per project
```

`bdrive login` verifies the server and remembers it as the device default
(`settings.json` under the bdrive home; run it with no argument to show the
current server). `bdrive init` then, per project: creates-or-joins
a project named after the folder (`--name <x>` for an explicit name,
`--project <id>` to join by id), writes the folder's `.bdrive` (project id +
server URL), seeds a starter `.bdriveignore` (node_modules, build dirs,
caches, `.env*`) when none exists, and mounts the folder — the daemon starts
syncing immediately: local changes are detected within seconds, and the
Claude Code plugin syncs at every session step.

Under the hood the `https://` remote speaks the hub's per-project
`/api/p/<id>/store` API — journal reads/writes relay through the server,
blob uploads go direct to the object store via the same short-lived
presigned URLs browser uploads use (falling back to relaying when the
backend can't presign). Client pushes and project creation require the
server to run with `--upload`; against a read-only hub, clients still pull
and their pushes wait (offline semantics) until allowed.

The web UI lists the hub's projects in the sidebar; selecting one browses
that project's files with full per-file provenance (who, which device,
when — the same as `bdrive log`).

### Uploads

The browser client is deliberately storage-blind: it never sees the remote
URL, bucket, or any credentials. On page load it fetches `/api/config` and
follows whatever the server allows.

With `--upload` set, the server decides per upload how the bytes travel:

- **Direct** — for backends that can presign (S3 and S3-compatible stores;
  GCS when the server runs with credentials that can sign, e.g. a service
  account): the server mints a short-lived presigned `PUT` URL for the
  content-addressed blob (`blobs/<sha256>`), the browser uploads straight
  to the object store, then asks the server to commit. The commit verifies
  the blob actually exists and appends a `put` op to the *server's own*
  journal — the blobs-before-journal ordering and the one-writer-per-journal
  invariant both hold. Expired URLs are refused by the store; the client
  just re-runs init. Direct uploads to a bucket also need a CORS rule on
  the bucket allowing `PUT` from the viewer's origin.
- **Through the server** — `file://` remotes and plain-folder serving can't
  presign, so the client sends content to the server, which stores it
  (object store + journal, or straight to disk for a served folder, where
  the daemon will pick it up like any local edit).

## Claude Code plugin

Install beardrive support in Claude Code with two commands:

```
/plugin marketplace add runbear-io/beardrive
/plugin install beardrive@beardrive
```

The plugin sets up everything at once:

- **`/beardrive:mount [folder] [remote]`** — one command that installs beardrive if
  needed, mounts the folder (daemon + `.bdrive` config), and verifies the sync.
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
 (real files)                       ~/.bdrive/volumes/<vol>              s3:// gs:// file://
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
the `.bdrive` settings file, its own temp files, and anything excluded by
`.bdriveignore` or omitted from an `include` list. Empty directories are not
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
conflicts. Set `BDRIVE_HOME` to relocate all beardrive state (used heavily in tests).

## License

MIT
