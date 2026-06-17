---
name: sfs-mount
description: Mount, unmount, and sync folders with sfs. Use when the user wants to "mount a folder", "unmount", "stop syncing", "sync now", or set up a shared agent workspace across devices with sfs. Covers `sfs mnt`, `sfs umnt`, and `sfs sync` including foreground daemon mode and scan intervals.
---

# sfs — mount, unmount, sync

`sfs` mounts any folder as a synced volume backed by a cloud object store (or a shared directory). Each mount runs a per-mount background daemon that scans the folder for changes and exchanges with the remote. Files on disk are always real files — every tool works on them with no integration.

Use this skill whenever the user wants to:

- Mount a folder and start syncing it.
- Stop syncing (with or without forgetting the mount).
- Run an on-demand sync cycle.

For remote URL setup and credentials, see the `sfs-remote` skill. For inspecting state, see the `sfs-status` skill.

## Commands at a glance

| Action | Command |
|---|---|
| Mount a folder, no remote yet | `sfs mnt <folder>` |
| Mount with a remote in one shot | `sfs mnt <folder> --remote <url>` |
| Mount in foreground (no background daemon) | `sfs mnt <folder> -f` |
| Stop the sync daemon | `sfs umnt <folder>` |
| Stop and forget the mount entirely | `sfs umnt <folder> --forget` |
| Run one sync cycle now | `sfs sync [<folder>]` |

`<folder>` is created if it doesn't exist. Omitting `<folder>` on `sync`/`status`/`log` defaults to the current working directory.

## Mount flow

1. **Pick a folder.** A new empty folder works; an existing folder with files works too — existing files are imported into the volume on the first cycle.
2. **Decide on a remote.** Optional at mount time. If skipped, the volume is local-only until `sfs remote set <folder> <url>` (see `sfs-remote`).
3. **Run `sfs mnt`.** sfs:
   - registers the folder in `~/.sfs/mounts.json`,
   - opens (or creates) the volume under `~/.sfs/volumes/<volume>/`,
   - runs an initial cycle (import local files; pull remote state if a remote is set),
   - starts a background daemon (unless `-f`).
4. **Verify** with `sfs status <folder>` (see `sfs-status`).

### Important `sfs mnt` flags

- `--remote, -r <url>` — `s3://bucket/prefix`, `gs://bucket/prefix`, or `file:///abs/path`. Can be set later via `sfs remote set`.
- `--volume, -v <name>` — override volume name. Default: folder basename. The same volume name on another device + same remote means they sync.
- `--foreground, -f` — run the daemon in the foreground. Useful for systemd / launchd / containers where you want sfs to be PID 1 of its own service. Do **not** combine with starting a second background daemon.
- `--scan-interval` (default `3s`) — how often to scan the folder for local changes.
- `--remote-interval` (default `10s`) — how often to push/pull with the remote.

### Reusing the same volume on multiple machines

```sh
# Machine A
sfs mnt ~/agent-workspace --remote s3://my-bucket/agent-workspace

# Machine B (and C, …)
sfs mnt ~/agent-workspace --remote s3://my-bucket/agent-workspace
```

The basename `agent-workspace` becomes the volume name on each machine, and they converge through the same remote prefix.

## Unmount flow

`sfs umnt <folder>` stops the per-mount daemon. Files stay on disk, the local volume store under `~/.sfs/volumes/<volume>/` is kept, and you can `sfs mnt` the folder again later to resume.

`sfs umnt <folder> --forget` additionally removes the entry from the mount registry. The local volume data is still preserved.

To fully reclaim disk space, the user must manually delete `~/.sfs/volumes/<volume>/` — sfs does not do this automatically.

## On-demand sync

`sfs sync [<folder>]` runs a single cycle: scan local changes, upload new blobs and the local journal, pull remote journals, materialize the result. This is what the daemon does on its interval; running it manually is useful when:

- The user just made changes and wants to see them on another device immediately.
- Verifying that credentials and the remote URL work end-to-end.
- The daemon was stopped but a one-shot sync is still wanted.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `mnt` says `already mounted as volume "X"` | Folder is in the registry under a different volume name | Drop the `--volume` flag or pass the existing one |
| `mnt` succeeds but no daemon | `-f` was used, or `daemon.Start` failed | Re-run without `-f`, or check `sfs status` and the foreground output |
| Files don't appear on the other device | Remote not set, or credentials missing | `sfs remote <folder>` to check; run `sfs sync` to surface the error |
| Conflict files appear (`*.sfs-conflict-*`) | Two devices edited the same path concurrently | Pick a winner manually; sfs preserves the loser intentionally |

## Examples to walk a user through

```sh
# Brand-new shared workspace
sfs mnt ~/agent-workspace --remote s3://acme-sfs/agent-workspace

# Local-only first, add a remote later
sfs mnt ./notes
sfs remote set ./notes file:///Volumes/nas/sfs/notes
sfs sync ./notes

# Pause syncing for the day
sfs umnt ~/agent-workspace

# Drop a folder entirely from sfs (keeps local volume history)
sfs umnt ./notes --forget
```

## What sfs does not sync

`.git` directories, `.DS_Store`, sfs's own temp files, and empty directories. Don't suggest mounting a folder where `.git` is the primary content the user expects synced — they want git, not sfs.
