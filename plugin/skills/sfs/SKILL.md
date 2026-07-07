---
name: sfs
description: Use sfs — a mountable, synced file system for AI agents. Mount any folder and it stays synced across devices through cloud object storage (S3, GCS, S3-compatible, or a shared directory) with full per-file change history and offline support. Use when the user wants to "mount a folder", "unmount", "sync now", "set up sfs cloud storage", "connect sfs to S3/GCS/R2/MinIO/NAS", "check sfs status", "see sfs logs", "see what changed", "who changed this file?", or troubleshoot a stuck sync.
---

# sfs — synced file system for AI agents

`sfs` mounts any folder as a synced volume backed by an object store. Each mount runs a per-mount background daemon that scans for local changes and exchanges with the remote. Files on disk are always real files — every tool, editor, and agent works on them with no integration.

Use this skill whenever the user is working with the `sfs` CLI: mounting, unmounting, syncing, configuring a remote, inspecting state, reading change history, or debugging.

## Command map

| Action | Command |
|---|---|
| Mount, no remote yet | `sfs mnt <folder>` |
| Mount with a remote | `sfs mnt <folder> --remote <url>` |
| Mount in foreground (no background daemon) | `sfs mnt <folder> -f` |
| Stop the sync daemon | `sfs umnt <folder>` |
| Stop and forget the mount | `sfs umnt <folder> --forget` |
| One sync cycle now | `sfs sync [<folder>]` |
| Mounts + daemon + pending state | `sfs status [<folder>]` |
| Change history | `sfs log [<folder>] [-p path] [-n N]` |
| Show / set remote | `sfs remote [<folder>]` · `sfs remote set <folder> <url>` |
| This device's identity | `sfs whoami` |

`<folder>` is created if missing. Omitting it on `sync`/`status`/`log` defaults to the current working directory.

## Project files

Two files at the mount root control a folder's sync behavior:

- **`.sfs`** — the folder's settings (JSON): `volume`, `remote`, and optional `include`. Written by `sfs mnt` / `sfs remote set`; safe to hand-edit (a running daemon picks changes up on its next tick). It is **never synced** — remotes are device-specific — and it travels with the folder: copy a folder containing `.sfs` to a new machine and plain `sfs mnt <folder>` reuses its volume and remote.
- **`.sfsignore`** — opt-out list, gitignore-style. **Syncs like a normal file**, so all devices share the same rules. Syntax subset: `#` comments, `*` within a segment, `**` across segments, `?`, trailing `/` for directories-only, a `/` elsewhere anchors to the mount root, `!` re-includes.

```jsonc
// .sfs
{
  "volume": "agent-workspace",
  "remote": "s3://acme-sfs/agent-workspace",
  "include": ["docs/", "notes/", "*.md"]   // optional: sync ONLY these
}
```

```gitignore
# .sfsignore
*.log
node_modules/
build/
!build/keep.txt
```

Selective-sync semantics — important when advising users:

- A path syncs when it is **not ignored** and (if `include` is non-empty) **matches an include pattern**. Ignore beats include.
- Adding a pattern for an already-synced file makes this device **stop tracking it without deleting it anywhere** — the file stays on disk locally and on every other device. Deleting it locally after that does not propagate either.
- Because `.sfsignore` syncs, adding a rule on one device applies it everywhere on the next cycle.

---

## 1. Mount / unmount / sync

### Mount flow

1. Pick a folder. New empty or with existing files — existing files are imported on the first cycle.
2. Decide on a remote (optional at mount time; configurable later via `sfs remote set`).
3. Run `sfs mnt`. sfs:
   - writes the folder's settings to `<folder>/.sfs` and registers it in `~/.sfs/mounts.json`,
   - opens/creates the volume under `~/.sfs/volumes/<volume>/`,
   - runs an initial cycle (import locals; pull remote state if a remote is set),
   - starts a background daemon (unless `-f`).
4. Verify with `sfs status <folder>`.

### Important `sfs mnt` flags

- `--remote, -r <url>` — `s3://bucket/prefix`, `gs://bucket/prefix`, or `file:///abs/path`. Can be set later via `sfs remote set`.
- `--volume, -v <name>` — override volume name. Default: folder basename. The same volume name on another device + same remote = they sync.
- `--foreground, -f` — run the daemon in the foreground (for systemd/launchd/containers).
- `--scan-interval` (default `3s`) — local scan interval.
- `--remote-interval` (default `10s`) — remote sync interval.

### Multi-device setup

```sh
# Machine A
sfs mnt ~/agent-workspace --remote s3://my-bucket/agent-workspace

# Machine B, C, …
sfs mnt ~/agent-workspace --remote s3://my-bucket/agent-workspace
```

The basename `agent-workspace` becomes the volume name on each device; they converge through the same remote prefix.

### Unmount

- `sfs umnt <folder>` — stop the daemon. Files stay on disk; local volume store under `~/.sfs/volumes/<volume>/` is kept. Re-mount any time to resume.
- `sfs umnt <folder> --forget` — also drop from the mount registry. Local volume data is still preserved; the user must `rm -rf ~/.sfs/volumes/<volume>/` to reclaim disk.

### On-demand sync

`sfs sync [<folder>]` runs a single cycle (scan → upload blobs+journal → pull remote journals → materialize). Useful to:

- Push a change immediately instead of waiting for the next interval.
- Verify credentials and the remote end-to-end.
- Sync once even when the daemon is stopped.

### Examples to walk a user through

```sh
# Brand-new shared workspace
sfs mnt ~/agent-workspace --remote s3://acme-sfs/agent-workspace

# Local-only first, add a remote later
sfs mnt ./notes
sfs remote set ./notes file:///Volumes/nas/sfs/notes
sfs sync ./notes

# Pause syncing for the day
sfs umnt ~/agent-workspace

# Drop a folder entirely (keeps local volume history)
sfs umnt ./notes --forget
```

### What sfs does not sync

`.git` directories, `.DS_Store`, the `.sfs` settings file, sfs's own temp files, empty directories, and anything excluded by `.sfsignore` or left out of an `include` list. Don't suggest mounting a folder where `.git` is the content the user expects synced — they want git, not sfs.

---

## 2. Cloud storage setup

sfs uses each provider's standard credential chain — nothing sfs-specific.

### Supported URL schemes

| Scheme | Backend | Example |
|---|---|---|
| `s3://bucket/prefix` | Amazon S3, or any S3-compatible store via `AWS_ENDPOINT_URL` | `s3://acme-sfs/agent-workspace` |
| `gs://bucket/prefix` | Google Cloud Storage | `gs://acme-sfs/agent-workspace` |
| `file:///abs/path` | Plain directory (local, NAS, Dropbox folder, …) | `file:///Volumes/nas/sfs/notes` |

`sfs remote set` validates the scheme and rejects anything else. The prefix can be multi-segment (`s3://bucket/team/agent/workspace`); sfs writes `blobs/` and `journal/` underneath it.

### Setting the remote

```sh
# At mount time
sfs mnt ./workspace --remote s3://acme-sfs/workspace

# After mounting
sfs remote set ./workspace s3://acme-sfs/workspace

# Inspect
sfs remote ./workspace
```

After `remote set`, run `sfs sync ./workspace` to push immediately. A running daemon picks up the change on its next interval.

### Amazon S3 (`s3://`)

AWS Go SDK v2 credential chain, in order:

1. `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN` env vars
2. `AWS_PROFILE` env var → `~/.aws/credentials` + `~/.aws/config`
3. EC2 / ECS / EKS IAM roles
4. SSO sessions (`aws sso login`)

Region: `AWS_REGION`, or the profile's `region`, or discovered from the bucket.

Minimum IAM policy for one prefix:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket"],
    "Resource": [
      "arn:aws:s3:::acme-sfs",
      "arn:aws:s3:::acme-sfs/agent-workspace/*"
    ]
  }]
}
```

### S3-compatible (MinIO, Cloudflare R2, Backblaze B2, Wasabi…)

Set `AWS_ENDPOINT_URL` (or `AWS_ENDPOINT_URL_S3`) and use the `s3://` scheme:

```sh
# Cloudflare R2
export AWS_ENDPOINT_URL=https://<accountid>.r2.cloudflarestorage.com
export AWS_REGION=auto
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
sfs mnt ./workspace --remote s3://my-r2-bucket/workspace

# MinIO
export AWS_ENDPOINT_URL=http://minio.local:9000
export AWS_REGION=us-east-1
export AWS_ACCESS_KEY_ID=minioadmin
export AWS_SECRET_ACCESS_KEY=minioadmin
sfs mnt ./workspace --remote s3://sfs/workspace
```

Persist these in the user's shell rc or a systemd/launchd unit so the daemon also has them.

### Google Cloud Storage (`gs://`)

Application Default Credentials (ADC):

```sh
# Interactive workstation
gcloud auth application-default login

# Service account
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/key.json

sfs mnt ./workspace --remote gs://acme-sfs/workspace
```

Service account needs `storage.objects.{get,list,create,delete}` on the bucket (`roles/storage.objectAdmin` bucket-scoped works).

### Shared directory (`file://`)

No credentials. Anything readable+writable by the user works — NAS, SMB, Dropbox/iCloud, external drive. Path must be absolute (`file:///Volumes/nas/sfs/notes`).

Caveats:

- The shared directory is the source of truth, just like a bucket. Don't put files in it directly.
- iCloud/Dropbox throttling can slow sync but doesn't break it; conflicts still resolve deterministically.

### Picking a backend

- **Already on AWS** → `s3://`.
- **Already on GCP** → `gs://`.
- **Privacy / no cloud account** → Cloudflare R2 (`s3://` + endpoint); zero egress.
- **Self-hosted / homelab** → MinIO (`s3://` + endpoint), or `file://` over a NAS mount.
- **Single laptop + external drive / iCloud** → `file://`.

A common layout — one bucket, one prefix per volume:

```
s3://acme-sfs/
├── agent-workspace/      # one volume
├── design-notes/         # another volume
└── research/             # another volume
```

### Verifying a remote actually works

```sh
sfs sync ./workspace
```

A clean `synced /path (volume "workspace")` plus a non-error cycle summary means the chain, endpoint, and permissions all work. Common failures:

| Error | Cause | Fix |
|---|---|---|
| `NoCredentialProviders` / `could not load credentials` | No AWS creds in any chain step | Set `AWS_PROFILE` or env vars; for daemons, set them in the launch unit |
| `403 Forbidden` / `AccessDenied` | Creds work but lack `Put/Get/List` on the prefix | Update IAM to include the bucket + `/prefix/*` |
| `404 NoSuchBucket` | Wrong bucket or endpoint | Verify; for R2/MinIO ensure `AWS_ENDPOINT_URL` matches |
| `dial tcp: ... no such host` | Endpoint URL wrong / DNS broken | Recheck `AWS_ENDPOINT_URL` |
| `permission denied` on `file://` | OS-level perms on the dir | `chmod`/`chown` so the user can read+write+list |

### Changing the remote later

- Same bucket, different prefix → effectively a new volume at the new prefix; old prefix is not touched.
- Switching providers (e.g. `file://` → `s3://`) → next sync pushes everything to the new remote. **All devices sharing the volume must point at the new URL** or they'll diverge.

### Credentials and the background daemon

The daemon inherits the env of the `sfs mnt` invocation. If you set `AWS_PROFILE` in a one-off shell, mounted, and opened a new shell without it — the daemon is fine, but `sfs sync` from the fresh shell may fail credential lookup. For long-lived setups, put credentials in the shell rc or the launchd/systemd unit.

---

## 3. Status, logs, identity

Three observation surfaces:

1. **`sfs status`** — registry, daemon liveness, file count, pending push.
2. **`sfs log`** — per-file change history from the journals.
3. **The daemon log file** — what the background syncer actually did, and any errors.

### `sfs status [<folder>]`

With no argument: every registered mount. With a folder: narrows to that mount.

```
device: macbook (d380dea58598) as snow@runbear.io

/Users/snow/agent-workspace
  volume:   agent-workspace
  remote:   s3://acme-sfs/agent-workspace
  daemon:   running (pid 55434)
  files:    142 (3.4 MiB)
  pending:  0 local change(s) not yet pushed
```

Interpretation:

- **`device`** — this machine's identity (same as `sfs whoami`); appears in `sfs log`.
- **`volume`** — folders on other devices with the same volume name + same remote converge.
- **`remote`** — `(none — local only)` means changes are journaled locally but never leave the device.
- **`daemon`** — `running (pid N)` or `stopped`. If stopped, run `sfs mnt <folder>` to start a daemon, or `sfs sync <folder>` for a one-shot.
- **`files`** — tracked file count and total bytes.
- **`pending`** — local journal ops not yet pushed. Should be 0 shortly after a successful sync. Stuck > 0 usually means broken remote/creds, stopped daemon, or a custom `--remote-interval`.

### `sfs log [<folder>]`

```sh
sfs log ./workspace                 # last 50 ops
sfs log ./workspace -n 0            # all ops
sfs log ./workspace -p notes/       # paths under notes/
sfs log ./workspace -p notes/x.md   # one file
```

Each line: `time  kind  path  author on device-name  (size)  [note]`

```
2026-06-17 09:14:02  put     notes/memory.md           snow@runbear.io on macbook  (412 B)
2026-06-17 09:14:55  put     notes/memory.md           agent@runbear.io on linux-vm  (501 B)
2026-06-17 09:15:11  delete  notes/draft.md            snow@runbear.io on macbook
```

Answers:

- "Who last changed file X?" → `sfs log <folder> -p <path> -n 1`.
- "What did device Y do?" → `sfs log <folder> -n 0` and filter by device name.
- "Did my edit cross over?" → run `sfs log` on the other device; the op appears once it has pulled the source device's journal.

History is content-addressed — overwritten and deleted files are still in the log, with blobs retained under `~/.sfs/volumes/<volume>/blobs/`.

### `sfs whoami`

```
device id:   d380dea58598
device name: macbook
author:      snow@runbear.io
sfs home:    /Users/snow/.sfs
```

- **device id** — random 12-hex, generated on first run, persisted to `~/.sfs/device.json`.
- **device name** — hostname (without `.local`).
- **author** — `git config user.email` if present, else `$USER@<hostname>`.

To change name/author, edit `~/.sfs/device.json` and restart the daemon (`sfs umnt`/`sfs mnt`).

### The per-mount daemon log

```sh
# Volume contents (daemon pid + log files live here, one pair per mount)
ls ~/.sfs/volumes/<volume>/

# Tail
tail -F ~/.sfs/volumes/<volume>/daemon-*.log
```

Useful when `pending` is stuck > 0, the daemon flips to `stopped` after a restart, or you changed credentials and want to confirm uptake.

### Diagnostic flow ("sfs doesn't seem to be working")

1. `sfs status` — folder listed? daemon `running`? `pending` stuck > 0?
2. `daemon: stopped` → `sfs mnt <folder>` to restart it.
3. `pending` stuck → `sfs sync <folder>` and read the cycle output. Errors here point at the remote — see the cloud-storage troubleshooting table above.
4. Sync succeeds but the other device doesn't see changes → `sfs sync` on the other device + `sfs log` to confirm the op crossed over.
5. Daemon keeps dying → tail `~/.sfs/volumes/<volume>/daemon-*.log` for the cause.

---

## On-disk layout

```
~/.sfs/
├── device.json              # identity (sfs whoami)
├── mounts.json              # mount registry (sfs status)
└── volumes/<volume>/
    ├── blobs/               # content-addressed file content
    ├── journal/             # per-device append-only op logs
    ├── state-<mountID>.json # what's currently materialized (per mount)
    ├── sync.json            # lamport clock + push cursor
    └── daemon-<mountID>.pid/.log  # daemon state + log, per mount
```

Don't suggest editing files under `volumes/` directly — sfs owns them. `device.json` and `mounts.json` are safe to inspect; `mounts.json` is safe to hand-edit if a mount entry needs surgery, but prefer `sfs umnt --forget` then `sfs mnt`.

Override the whole tree with `SFS_HOME=/path` (used heavily in tests and ephemeral environments).
