---
description: Mount a folder as a synced sfs volume — one command sets up the sync daemon, the .sfs project config, and (via this plugin's hooks) automatic sync at every turn boundary
argument-hint: [folder] [remote e.g. s3://bucket/prefix]
---

Mount a folder as a synced sfs volume. Arguments: `$ARGUMENTS` (optional folder, optional remote URL).

Follow these steps:

1. **Check sfs is installed**: run `command -v sfs`. If missing, offer to install it (`brew install runbear-io/tap/sfs`, or `go install github.com/runbear-io/sfs/cmd/sfs@latest`) and wait for the user's choice before installing.

2. **Determine the folder**: first argument if given, otherwise the current directory. If the folder already contains a `.sfs` file, the volume and remote are already configured — just run `sfs mnt <folder>` and skip step 3.

3. **Determine the remote**: second argument if given. If not given, ask the user which backend they want:
   - `s3://bucket/prefix` — Amazon S3 or any S3-compatible store (R2/MinIO via `AWS_ENDPOINT_URL`)
   - `gs://bucket/prefix` — Google Cloud Storage
   - `file:///abs/path` — a plain shared directory (NAS, external drive)
   - none — local-only for now (`sfs remote set` can add one later)

4. **Mount**: run `sfs mnt <folder> [--remote <url>]`. This registers the background sync daemon and writes the folder's settings to `<folder>/.sfs`.

5. **Verify**: run `sfs status <folder>` and show the result. If the remote errored, consult the sfs skill's troubleshooting table (credentials are the usual cause).

6. **Tell the user what's now active** (briefly):
   - the daemon syncs continuously (every few seconds);
   - this plugin's hooks also sync at every turn boundary — a blocking pull when they send a message, an async push when the turn ends — so Claude always works on fresh files;
   - `.sfsignore` in the folder root excludes files (gitignore-style); an `"include"` list in `.sfs` narrows what syncs;
   - `sfs log` shows who changed what, `sfs umnt` stops syncing.
