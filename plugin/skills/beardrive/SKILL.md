---
name: beardrive
description: Use beardrive — a mountable, synced file system for AI agents. Mount any folder and it stays synced across devices through cloud object storage (S3, GCS, S3-compatible, or a shared directory) with full per-file change history and offline support. Use when the user wants to "mount a folder", "unmount", "sync now", "set up beardrive cloud storage", "connect beardrive to S3/GCS/R2/MinIO/NAS", "check bdrive status", "see beardrive logs", "see what changed", "who changed this file?", or troubleshoot a stuck sync.
---

# BearDrive — synced file system for AI agents

**BearDrive** (CLI: `bdrive`) mounts any folder as a synced volume backed by an object store. Each mount runs a per-mount background daemon that scans for local changes and exchanges with the remote. Files on disk are always real files — every tool, editor, and agent works on them with no integration.

Use this skill whenever the user is working with the `bdrive` CLI: mounting, unmounting, syncing, configuring a remote, inspecting state, reading change history, or debugging.

## Command map

| Action | Command |
|---|---|
| Mount, no remote yet | `bdrive mnt <folder>` |
| Mount with a remote | `bdrive mnt <folder> --remote <url>` |
| Mount in foreground (no background daemon) | `bdrive mnt <folder> -f` |
| Stop the sync daemon | `bdrive umnt <folder>` |
| Stop and forget the mount | `bdrive umnt <folder> --forget` |
| One sync cycle now | `bdrive sync [<folder>]` |
| Mounts + daemon + pending state | `bdrive status [<folder>]` |
| Change history | `bdrive log [<folder>] [-p path] [-n N]` |
| Show / set remote | `bdrive remote [<folder>]` · `bdrive remote set <folder> <url>` |
| This device's identity | `bdrive whoami` |
| Point this device at a hub (once per device) | `bdrive login https://host:4173` — verifies the server, saves it as the device default (`$BDRIVE_HOME/settings.json`); bare `bdrive login` shows the current server |
| One-command project onboarding (storage-blind client) | `bdrive init [<folder>]` — needs a prior `bdrive login`; creates-or-joins a hub project named after the folder (`--name <x>` explicit, `--project <id>` join by id), writes `.bdrive`, seeds a starter `.bdriveignore`, mounts, starts the daemon. No separate `mnt` needed. |
| Web server: viewer + multi-project sync hub (read-only unless `--upload`) | `bdrive web [<folder> \| <storage-root-url>]` (serves cwd by default, `--addr :4173`; `-c config.json` reads remote/addr/upload/projects_db settings from a file, explicit flags win; a storage root URL makes it a hub hosting many projects at `<root>/<project-id>/`, registry in `--projects-db` file, default `$BDRIVE_HOME/projects.json`; `--upload` lets browsers add files, client devices push, and projects be created — direct to storage via expiring presigned URLs on S3/GCS, relayed through the server for `file://`; `--upload-ttl 15m`; clients never see the remote URL or credentials) |

`<folder>` is created if missing. Omitting it on `sync`/`status`/`log` defaults to the current working directory.

## Project files

Two files at the mount root control a folder's sync behavior:

- **`.bdrive`** — the folder's settings (JSON): `volume`, `remote`, and optional `include`. Written by `bdrive mnt` / `bdrive remote set`; safe to hand-edit (a running daemon picks changes up on its next tick). It is **never synced** — remotes are device-specific — and it travels with the folder: copy a folder containing `.bdrive` to a new machine and plain `bdrive mnt <folder>` reuses its volume and remote.
- **`.bdriveignore`** — opt-out list, gitignore-style. **Syncs like a normal file**, so all devices share the same rules. Syntax subset: `#` comments, `*` within a segment, `**` across segments, `?`, trailing `/` for directories-only, a `/` elsewhere anchors to the mount root, `!` re-includes.

```jsonc
// .bdrive
{
  "volume": "agent-workspace",
  "remote": "s3://acme-beardrive/agent-workspace",
  "include": ["docs/", "notes/", "*.md"]   // optional: sync ONLY these
}
```

```gitignore
# .bdriveignore
*.log
node_modules/
build/
!build/keep.txt
```

Selective-sync semantics — important when advising users:

- A path syncs when it is **not ignored** and (if `include` is non-empty) **matches an include pattern**. Ignore beats include.
- Adding a pattern for an already-synced file makes this device **stop tracking it without deleting it anywhere** — the file stays on disk locally and on every other device. Deleting it locally after that does not propagate either.
- Because `.bdriveignore` syncs, adding a rule on one device applies it everywhere on the next cycle.

---

## 1. Mount / unmount / sync

### Mount flow

1. Pick a folder. New empty or with existing files — existing files are imported on the first cycle.
2. Decide on a remote (optional at mount time; configurable later via `bdrive remote set`).
3. Run `bdrive mnt`. beardrive:
   - writes the folder's settings to `<folder>/.bdrive` and registers it in `~/.bdrive/mounts.json`,
   - opens/creates the volume under `~/.bdrive/volumes/<volume>/`,
   - runs an initial cycle (import locals; pull remote state if a remote is set),
   - starts a background daemon (unless `-f`).
4. Verify with `bdrive status <folder>`.

### Important `bdrive mnt` flags

- `--remote, -r <url>` — `s3://bucket/prefix`, `gs://bucket/prefix`, or `file:///abs/path`. Can be set later via `bdrive remote set`.
- `--volume, -v <name>` — override volume name. Default: folder basename. The same volume name on another device + same remote = they sync.
- `--foreground, -f` — run the daemon in the foreground (for systemd/launchd/containers).
- `--scan-interval` (default `3s`) — local scan interval.
- `--remote-interval` (default `10s`) — remote sync interval.

### Multi-device setup

```sh
# Machine A
bdrive mnt ~/agent-workspace --remote s3://my-bucket/agent-workspace

# Machine B, C, …
bdrive mnt ~/agent-workspace --remote s3://my-bucket/agent-workspace
```

The basename `agent-workspace` becomes the volume name on each device; they converge through the same remote prefix.

### Unmount

- `bdrive umnt <folder>` — stop the daemon. Files stay on disk; local volume store under `~/.bdrive/volumes/<volume>/` is kept. Re-mount any time to resume.
- `bdrive umnt <folder> --forget` — also drop from the mount registry. Local volume data is still preserved; the user must `rm -rf ~/.bdrive/volumes/<volume>/` to reclaim disk.

### On-demand sync

`bdrive sync [<folder>]` runs a single cycle (scan → upload blobs+journal → pull remote journals → materialize). Useful to:

- Push a change immediately instead of waiting for the next interval.
- Verify credentials and the remote end-to-end.
- Sync once even when the daemon is stopped.

### Examples to walk a user through

```sh
# Brand-new shared workspace
bdrive mnt ~/agent-workspace --remote s3://acme-beardrive/agent-workspace

# Local-only first, add a remote later
bdrive mnt ./notes
bdrive remote set ./notes file:///Volumes/nas/beardrive/notes
bdrive sync ./notes

# Pause syncing for the day
bdrive umnt ~/agent-workspace

# Drop a folder entirely (keeps local volume history)
bdrive umnt ./notes --forget
```

### What beardrive does not sync

`.git` directories, `.DS_Store`, the `.bdrive` settings file, beardrive's own temp files, empty directories, and anything excluded by `.bdriveignore` or left out of an `include` list. Don't suggest mounting a folder where `.git` is the content the user expects synced — they want git, not beardrive.

---

## 2. Cloud storage setup

beardrive uses each provider's standard credential chain — nothing beardrive-specific.

### Supported URL schemes

| Scheme | Backend | Example |
|---|---|---|
| `s3://bucket/prefix` | Amazon S3, or any S3-compatible store via `AWS_ENDPOINT_URL` | `s3://acme-beardrive/agent-workspace` |
| `gs://bucket/prefix` | Google Cloud Storage | `gs://acme-beardrive/agent-workspace` |
| `file:///abs/path` | Plain directory (local, NAS, Dropbox folder, …) | `file:///Volumes/nas/beardrive/notes` |
| `https://host:port/p/<project-id>` | One project on a `bdrive web` hub — the client holds **no storage credentials**; the server device owns the bucket config. Server must run with `--upload` for clients to push. Set up with `bdrive init` (never hand-write the `/p/<id>` URL) | `https://drive.example.com:4173/p/p-7f3a2c91` |

`bdrive remote set` validates the scheme and rejects anything else. The prefix can be multi-segment (`s3://bucket/team/agent/workspace`); beardrive writes `blobs/` and `journal/` underneath it.

### Setting the remote

```sh
# At mount time
bdrive mnt ./workspace --remote s3://acme-beardrive/workspace

# After mounting
bdrive remote set ./workspace s3://acme-beardrive/workspace

# Inspect
bdrive remote ./workspace
```

After `remote set`, run `bdrive sync ./workspace` to push immediately. A running daemon picks up the change on its next interval.

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
      "arn:aws:s3:::acme-beardrive",
      "arn:aws:s3:::acme-beardrive/agent-workspace/*"
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
bdrive mnt ./workspace --remote s3://my-r2-bucket/workspace

# MinIO
export AWS_ENDPOINT_URL=http://minio.local:9000
export AWS_REGION=us-east-1
export AWS_ACCESS_KEY_ID=minioadmin
export AWS_SECRET_ACCESS_KEY=minioadmin
bdrive mnt ./workspace --remote s3://beardrive/workspace
```

Persist these in the user's shell rc or a systemd/launchd unit so the daemon also has them.

### Google Cloud Storage (`gs://`)

Application Default Credentials (ADC):

```sh
# Interactive workstation
gcloud auth application-default login

# Service account
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/key.json

bdrive mnt ./workspace --remote gs://acme-beardrive/workspace
```

Service account needs `storage.objects.{get,list,create,delete}` on the bucket (`roles/storage.objectAdmin` bucket-scoped works).

### Shared directory (`file://`)

No credentials. Anything readable+writable by the user works — NAS, SMB, Dropbox/iCloud, external drive. Path must be absolute (`file:///Volumes/nas/beardrive/notes`).

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
s3://acme-beardrive/
├── agent-workspace/      # one volume
├── design-notes/         # another volume
└── research/             # another volume
```

### Verifying a remote actually works

```sh
bdrive sync ./workspace
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

The daemon inherits the env of the `bdrive mnt` invocation. If you set `AWS_PROFILE` in a one-off shell, mounted, and opened a new shell without it — the daemon is fine, but `bdrive sync` from the fresh shell may fail credential lookup. For long-lived setups, put credentials in the shell rc or the launchd/systemd unit.

---

## 3. Status, logs, identity

Three observation surfaces:

1. **`bdrive status`** — registry, daemon liveness, file count, pending push.
2. **`bdrive log`** — per-file change history from the journals.
3. **The daemon log file** — what the background syncer actually did, and any errors.

### `bdrive status [<folder>]`

With no argument: every registered mount. With a folder: narrows to that mount.

```
device: macbook (d380dea58598) as snow@runbear.io

/Users/snow/agent-workspace
  volume:   agent-workspace
  remote:   s3://acme-beardrive/agent-workspace
  daemon:   running (pid 55434)
  files:    142 (3.4 MiB)
  pending:  0 local change(s) not yet pushed
```

Interpretation:

- **`device`** — this machine's identity (same as `bdrive whoami`); appears in `bdrive log`.
- **`volume`** — folders on other devices with the same volume name + same remote converge.
- **`remote`** — `(none — local only)` means changes are journaled locally but never leave the device.
- **`daemon`** — `running (pid N)` or `stopped`. If stopped, run `bdrive mnt <folder>` to start a daemon, or `bdrive sync <folder>` for a one-shot.
- **`files`** — tracked file count and total bytes.
- **`pending`** — local journal ops not yet pushed. Should be 0 shortly after a successful sync. Stuck > 0 usually means broken remote/creds, stopped daemon, or a custom `--remote-interval`.

### `bdrive log [<folder>]`

```sh
bdrive log ./workspace                 # last 50 ops
bdrive log ./workspace -n 0            # all ops
bdrive log ./workspace -p notes/       # paths under notes/
bdrive log ./workspace -p notes/x.md   # one file
```

Each line: `time  kind  path  author on device-name  (size)  [note]`

```
2026-06-17 09:14:02  put     notes/memory.md           snow@runbear.io on macbook  (412 B)
2026-06-17 09:14:55  put     notes/memory.md           agent@runbear.io on linux-vm  (501 B)
2026-06-17 09:15:11  delete  notes/draft.md            snow@runbear.io on macbook
```

Answers:

- "Who last changed file X?" → `bdrive log <folder> -p <path> -n 1`.
- "What did device Y do?" → `bdrive log <folder> -n 0` and filter by device name.
- "Did my edit cross over?" → run `bdrive log` on the other device; the op appears once it has pulled the source device's journal.

History is content-addressed — overwritten and deleted files are still in the log, with blobs retained under `~/.bdrive/volumes/<volume>/blobs/`.

### `bdrive whoami`

```
device id:   d380dea58598
device name: macbook
author:      snow@runbear.io
beardrive home:    /Users/snow/.bdrive
```

- **device id** — random 12-hex, generated on first run, persisted to `~/.bdrive/device.json`.
- **device name** — hostname (without `.local`).
- **author** — `git config user.email` if present, else `$USER@<hostname>`.

To change name/author, edit `~/.bdrive/device.json` and restart the daemon (`bdrive umnt`/`bdrive mnt`).

### The per-mount daemon log

```sh
# Volume contents (daemon pid + log files live here, one pair per mount)
ls ~/.bdrive/volumes/<volume>/

# Tail
tail -F ~/.bdrive/volumes/<volume>/daemon-*.log
```

Useful when `pending` is stuck > 0, the daemon flips to `stopped` after a restart, or you changed credentials and want to confirm uptake.

### Diagnostic flow ("beardrive doesn't seem to be working")

1. `bdrive status` — folder listed? daemon `running`? `pending` stuck > 0?
2. `daemon: stopped` → `bdrive mnt <folder>` to restart it.
3. `pending` stuck → `bdrive sync <folder>` and read the cycle output. Errors here point at the remote — see the cloud-storage troubleshooting table above.
4. Sync succeeds but the other device doesn't see changes → `bdrive sync` on the other device + `bdrive log` to confirm the op crossed over.
5. Daemon keeps dying → tail `~/.bdrive/volumes/<volume>/daemon-*.log` for the cause.

---

## On-disk layout

```
~/.bdrive/
├── device.json              # identity (bdrive whoami)
├── mounts.json              # mount registry (bdrive status)
└── volumes/<volume>/
    ├── blobs/               # content-addressed file content
    ├── journal/             # per-device append-only op logs
    ├── state-<mountID>.json # what's currently materialized (per mount)
    ├── sync.json            # lamport clock + push cursor
    └── daemon-<mountID>.pid/.log  # daemon state + log, per mount
```

Don't suggest editing files under `volumes/` directly — beardrive owns them. `device.json` and `mounts.json` are safe to inspect; `mounts.json` is safe to hand-edit if a mount entry needs surgery, but prefer `bdrive umnt --forget` then `bdrive mnt`.

Override the whole tree with `BDRIVE_HOME=/path` (used heavily in tests and ephemeral environments).
