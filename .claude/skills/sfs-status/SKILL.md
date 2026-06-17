---
name: sfs-status
description: Inspect sfs mounts, the sync daemon, change history, and identity. Use when the user wants to "check sfs status", "see sfs logs", "see what changed", "is sfs syncing?", "is the daemon running?", "who changed this file?", or troubleshoot a stuck sync. Covers `sfs status`, `sfs log`, `sfs whoami`, and reading the per-mount daemon log.
---

# sfs — status, logs, and identity

sfs writes everything observable to three places:

1. **`sfs status`** — registry, daemon liveness, file count, pending push.
2. **`sfs log`** — per-file change history from the journals.
3. **The daemon log** — what the background syncer actually did and any errors.

Use this skill when the user wants to inspect any of those, or diagnose "it doesn't seem to be syncing".

For mounting itself, see `sfs-mount`. For remote setup, see `sfs-remote`.

## `sfs status [<folder>]` — what's mounted, is it running?

With no argument, lists every registered mount. With a folder, narrows to that mount.

```sh
sfs status
```

Output anatomy:

```
device: macbook (d380dea58598) as snow@runbear.io

/Users/snow/agent-workspace
  volume:   agent-workspace
  remote:   s3://acme-sfs/agent-workspace
  daemon:   running (pid 55434)
  files:    142 (3.4 MiB)
  pending:  0 local change(s) not yet pushed
```

Interpret each line:

- **`device`** — this machine's identity. Same as `sfs whoami`. This is what appears in `sfs log` attribution.
- **`volume`** — the volume name. Folders on other devices that mount the same volume name through the same remote will converge.
- **`remote`** — `(none — local only)` means changes are journaled locally but never leave the device. See `sfs-remote`.
- **`daemon`** — `running (pid N)` means the per-mount background syncer is alive. `stopped` means the folder is registered but nothing is watching it; run `sfs mnt <folder>` to start a daemon, or `sfs sync <folder>` for a one-shot.
- **`files`** — number of tracked files and total bytes. Mismatch with the working folder usually means a scan hasn't run yet (wait a scan interval or `sfs sync`).
- **`pending`** — local journal ops not yet pushed to the remote. Should be 0 shortly after a successful sync. If it stays > 0:
  - the remote URL/credentials might be broken → check `sfs-remote`,
  - the daemon might be stopped → status will show `stopped`,
  - the remote-interval might be long → default is 10s, but anything custom is shown via `--remote-interval` at mount time.

Read the whole block before claiming "it's healthy".

## `sfs log [<folder>]` — change history

```sh
sfs log ./workspace                 # last 50 ops
sfs log ./workspace -n 0            # all ops
sfs log ./workspace -p notes/       # only paths under notes/
sfs log ./workspace -p notes/x.md   # one file
```

Each line shows `time  kind  path  author on device-name  (size)  [note]`:

```
2026-06-17 09:14:02  put     notes/memory.md           snow@runbear.io on macbook  (412 B)
2026-06-17 09:14:55  put     notes/memory.md           agent@runbear.io on linux-vm  (501 B)
2026-06-17 09:15:11  delete  notes/draft.md            snow@runbear.io on macbook
```

Use it to answer:

- "Who last changed file X?" → `sfs log <folder> -p <path> -n 1`.
- "What did device Y do?" → `sfs log <folder> -n 0` and grep by device-name.
- "Was my edit from machine A picked up on machine B?" → run `sfs log` on B; the op should appear once B has pulled A's journal.

History is content-addressed — overwritten and deleted files are still in the log, with the blob retained in `~/.sfs/volumes/<volume>/blobs/`.

## `sfs whoami` — device identity

```
device id:   d380dea58598
device name: macbook
author:      snow@runbear.io
sfs home:    /Users/snow/.sfs
```

- **device id** — random 12 hex chars, generated on first sfs run, persisted to `~/.sfs/device.json`.
- **device name** — hostname (without `.local`).
- **author** — `git config user.email` if present, else `$USER@<hostname>`.

To change name/author, edit `~/.sfs/device.json` and restart the daemon (`sfs umnt <folder>` then `sfs mnt <folder>`).

## The per-mount daemon log

The daemon writes its activity to a log file inside the volume's daemon dir. Find it via:

```sh
# Volume dir
ls ~/.sfs/volumes/<volume>/

# Daemon state and log for this mount (one mount per file)
ls ~/.sfs/volumes/<volume>/daemons/
```

Tail the most recent log to see scan/sync cycles and errors:

```sh
tail -F ~/.sfs/volumes/<volume>/daemons/*.log
```

Useful when:

- `pending` stays > 0 and you suspect a remote error.
- The daemon shows `stopped` but you just started it (the log will show the startup failure).
- You changed credentials and want to confirm the daemon picked them up.

## Common diagnostic flow

User says "sfs doesn't seem to be working":

1. `sfs status` — does the folder appear? Is the daemon `running`? Is `pending` stuck > 0?
2. If `daemon: stopped` — restart with `sfs mnt <folder>`.
3. If `pending` is stuck — run `sfs sync <folder>` and read the cycle output. Errors here point at the remote (see `sfs-remote` troubleshooting).
4. If sync succeeds but the other device still doesn't see changes — run `sfs sync` on the other device too and then `sfs log` to confirm the op crossed over.
5. If the daemon keeps dying — tail `~/.sfs/volumes/<volume>/daemons/*.log` for the cause.

## What's on disk

```
~/.sfs/
├── device.json              # identity (sfs whoami)
├── mounts.json              # mount registry (sfs status)
└── volumes/<volume>/
    ├── blobs/               # content-addressed file content
    ├── journal/             # per-device append-only op logs
    ├── state.json           # what's currently materialized
    ├── sync.json            # lamport clock + push cursor
    └── daemons/             # one pid+log file per mount of this volume
```

Don't suggest editing files under `volumes/` directly — sfs owns them. `device.json` and `mounts.json` are safe to inspect; `mounts.json` is safe to hand-edit if a mount entry needs surgery, but prefer `sfs umnt --forget` then `sfs mnt`.

Override the whole tree with `SFS_HOME=/path` (useful for tests and ephemeral environments).
