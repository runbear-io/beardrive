# sfs — a synced file system for AI agents

**sfs** mounts any folder as a synced volume: its contents stay synchronized
across all your devices through cloud object storage, every change is
tracked (who, when, on which device), and everything keeps working offline.

It is built for AI agent workflows — give your agents on every machine the
same `~/agent-workspace`, and notes, plans, memory files, and artifacts
follow them everywhere, with a full audit trail of which agent or human
changed what.

```console
$ sfs mnt ./workspace --remote s3://my-bucket/workspace
mounted /Users/snow/workspace
  volume:  workspace
  remote:  s3://my-bucket/workspace
  device:  macbook (d380dea58598) as snow@runbear.io
  daemon:  running (pid 55434, scan 3s, remote sync 30s)
```

On another machine:

```console
$ sfs mnt ./workspace --remote s3://my-bucket/workspace
# … the same files appear, and stay in sync from now on
```

## Features

- **Mount anywhere** — `sfs mnt ./folder` turns any folder into a synced
  volume. Files are *real files on disk*: every tool, editor, and agent can
  use them with zero integration work.
- **Multi-device sync** — devices converge through a shared remote. Each
  device only writes its own append-only journal, so no locking service or
  server is needed — any object store works.
- **Change tracking** — `sfs log` shows which device and author changed
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
  `name.sfs-conflict-<device>-<time>` file. Nothing is silently dropped.
- **macOS & Linux.**

## Install

```sh
brew install runbear-io/tap/sfs        # macOS (and Linuxbrew)
```

or from source:

```sh
go install github.com/runbear-io/sfs/cmd/sfs@latest
```

## Quick start

```sh
# 1. Mount a folder, syncing through S3 (or gs://, or file://)
sfs mnt ./notes --remote s3://my-bucket/notes

# 2. Work normally — create, edit, delete files with any tool.
echo "remember this" > notes/memory.md

# 3. On every other device, mount the same remote:
sfs mnt ./notes --remote s3://my-bucket/notes

# See what changed, who changed it, and from which device
sfs log ./notes

# Check sync state and the daemon
sfs status

# Sync on demand (the daemon also syncs automatically)
sfs sync ./notes

# Stop syncing (files stay on disk; mount again any time)
sfs umnt ./notes
```

### Credentials

sfs uses each provider's standard credential chain — nothing sfs-specific:

| Remote | Credentials |
|---|---|
| `s3://bucket/prefix` | `AWS_PROFILE`, `~/.aws/credentials`, env vars, IAM roles. S3-compatible stores via `AWS_ENDPOINT_URL`. |
| `gs://bucket/prefix` | Application Default Credentials (`gcloud auth application-default login`) or `GOOGLE_APPLICATION_CREDENTIALS`. |
| `file:///path` | none — any local or network-mounted directory |

## Commands

| Command | Description |
|---|---|
| `sfs mnt <folder> [--remote URL]` | Mount a folder as a synced volume and start the sync daemon |
| `sfs umnt <folder>` | Stop syncing (`--forget` also unregisters the mount) |
| `sfs sync [folder]` | Run one sync cycle now |
| `sfs status [folder]` | Mounts, daemon state, pending changes |
| `sfs log [folder] [-p path] [-n N]` | Change history: author, device, time, file |
| `sfs remote [folder]` / `sfs remote set <folder> <url>` | Show / set the cloud remote |
| `sfs whoami` | Device identity used in change tracking |

## How it works

```
working folder  ←materialize/scan→  local volume store  ←push/pull→  object store
 (real files)                       ~/.sfs/volumes/<vol>              s3:// gs:// file://
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
  size+mtime check) and exchanges with the remote every ~30s — or
  immediately after local edits.

### What sfs does not sync

`.git` directories (per-file LWW would corrupt repositories), `.DS_Store`,
and its own temp files. Empty directories are not tracked (like git).

## Roadmap

- `sfs restore <path>@<time>` — restore any file from history (all content
  is already retained)
- FUSE/NFS mount mode for lazy-loading huge volumes
- `.sfsignore` patterns
- Journal compaction & blob GC policies
- Per-path access scopes for multi-agent setups

## Development

```sh
go build ./...
go test ./...
```

The integration tests in `internal/syncer` simulate multiple devices syncing
through a `file://` remote, including offline operation and concurrent-edit
conflicts. Set `SFS_HOME` to relocate all sfs state (used heavily in tests).

## License

MIT
