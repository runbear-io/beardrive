---
name: sfs-remote
description: Configure cloud storage remotes for sfs — S3, Google Cloud Storage, S3-compatible (MinIO, R2), or a shared directory. Use when the user wants to "set up sfs cloud storage", "connect sfs to S3/GCS/R2/MinIO/NAS", change a remote URL, or troubleshoot credentials. Covers `--remote`, `sfs remote`, `sfs remote set`, and the per-provider credential chain.
---

# sfs — remote (cloud storage) setup

sfs syncs through any object store that supports basic PUT/GET/LIST. The remote URL determines both the protocol and the credential chain — sfs uses each provider's standard credentials, nothing sfs-specific.

Use this skill whenever the user wants to:

- Pick a backend (S3 / GCS / S3-compatible / shared directory).
- Set or change the remote URL of an existing mount.
- Debug "files aren't appearing on the other device" caused by remote/auth issues.

For mounting itself, see `sfs-mount`. For checking sync state, see `sfs-status`.

## Supported remote URL schemes

| Scheme | Backend | Example |
|---|---|---|
| `s3://bucket/prefix` | Amazon S3, or any S3-compatible store via `AWS_ENDPOINT_URL` | `s3://acme-sfs/agent-workspace` |
| `gs://bucket/prefix` | Google Cloud Storage | `gs://acme-sfs/agent-workspace` |
| `file:///abs/path` | Plain directory (local, NAS, Dropbox folder, …) | `file:///Volumes/nas/sfs/notes` |

`sfs remote set` validates the scheme and rejects anything else. The prefix can be a multi-segment path (`s3://bucket/team/agent/workspace`); sfs writes `blobs/` and `journal/` underneath it.

## Setting the remote

Two ways:

```sh
# At mount time
sfs mnt ./workspace --remote s3://acme-sfs/workspace

# After mounting (mount must already exist)
sfs remote set ./workspace s3://acme-sfs/workspace

# Inspect current remote
sfs remote ./workspace
```

After `remote set`, run `sfs sync ./workspace` to push immediately. A running daemon will pick up the change on its next interval automatically.

## Credentials by provider

### Amazon S3 (`s3://`)

sfs uses the standard AWS Go SDK v2 credential chain, in order:

1. `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN` env vars
2. `AWS_PROFILE` env var → `~/.aws/credentials` + `~/.aws/config`
3. EC2 / ECS / EKS IAM roles
4. SSO sessions (`aws sso login`)

Region resolution:

- `AWS_REGION` env var, or the profile's `region`, or the bucket's discovered region.

Minimum IAM policy (one prefix):

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

### S3-compatible stores (MinIO, Cloudflare R2, Backblaze B2, Wasabi…)

Set `AWS_ENDPOINT_URL` (or `AWS_ENDPOINT_URL_S3`) before running sfs, and use the `s3://` scheme:

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

Persist these in the user's shell rc (`~/.zshrc` / `~/.bashrc`) or a systemd/launchd unit so the daemon also has them.

### Google Cloud Storage (`gs://`)

sfs uses Application Default Credentials (ADC):

```sh
# Interactive workstation
gcloud auth application-default login

# Service account
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/key.json

sfs mnt ./workspace --remote gs://acme-sfs/workspace
```

The service account needs `storage.objects.{get,list,create,delete}` on the bucket (e.g. `roles/storage.objectAdmin` on the bucket, scoped tighter if needed).

### Shared directory (`file://`)

No credentials. Anything readable+writable by the user works: a NAS mount, an SMB share, a Dropbox/iCloud folder, an external drive. Path must be absolute (`file:///Volumes/nas/sfs/notes`, not `file://./notes`).

Caveats:

- The shared directory becomes the single source of truth, just like a bucket. Don't put files into it directly — let sfs manage it.
- iCloud/Dropbox throttling can slow sync but doesn't break it; conflicts still resolve deterministically.

## Picking a backend

Recommend based on the user's situation:

- **Already on AWS** → `s3://` (cheapest at low volume; native IAM).
- **Already on GCP** → `gs://`.
- **Privacy / no cloud account** → Cloudflare R2 (`s3://` + `AWS_ENDPOINT_URL`); zero egress fees.
- **Self-hosted / homelab** → MinIO (`s3://` + endpoint), or `file://` over a NAS mount.
- **Single laptop + external drive / iCloud** → `file://`.

Bucket-per-team or prefix-per-volume both work. A common layout:

```
s3://acme-sfs/
├── agent-workspace/      # one volume
├── design-notes/         # another volume
└── research/             # another volume
```

## Verifying a remote actually works

```sh
sfs sync ./workspace
```

A clean exit with `synced /path (volume "workspace")` plus a non-error cycle summary means the credential chain, endpoint, and permissions all work. Failures usually surface as one of:

| Error | Cause | Fix |
|---|---|---|
| `NoCredentialProviders` / `could not load credentials` | No AWS creds in any chain step | Set `AWS_PROFILE` or env vars; for daemons, set them in the launch unit |
| `403 Forbidden` / `AccessDenied` | Credentials work but lack `s3:Put/Get/List` on the prefix | Update IAM policy to include the bucket and the prefix `/*` |
| `404 NoSuchBucket` | Wrong bucket name or wrong endpoint | Verify; for R2/MinIO ensure `AWS_ENDPOINT_URL` matches the bucket's region/account |
| `dial tcp: ... no such host` | Endpoint URL wrong or DNS broken | Recheck `AWS_ENDPOINT_URL` |
| `permission denied` on `file://` | OS-level permissions on the shared dir | `chmod`/`chown` so the user can read+write+list |

## Changing the remote later

Safe to change:

- Same bucket, different prefix → effectively starts a new volume at the new prefix. Old prefix is not touched.
- Switching providers (e.g. `file://` → `s3://`) → next sync pushes everything to the new remote. Other devices need their remote pointed at the new URL too, or they'll diverge.

Recommend telling all devices that share a volume at once, then `sfs sync` on each.

## Credentials and the background daemon

The daemon inherits the environment of the `sfs mnt` invocation. If you set `AWS_PROFILE` in a one-off shell, mounted, and then opened a new shell without it — the daemon is fine because it already has the env, but `sfs sync` from a fresh shell may fail credential lookup. For long-lived setups, put credentials in the user's shell rc or in the launchd/systemd unit that supervises sfs.
