# Self-hosting a BearDrive hub in ~10 minutes

Until BearDrive Cloud launches, self-hosting is *the* way to run a team
hub — and it stays a first-class, fully-supported path forever (AGPL, no
features held back). One static Go binary, one config file, any object
store (or a plain directory).

## 1. Install the binary

```sh
brew install runbear-io/tap/beardrive     # macOS / Linuxbrew
# or: go install github.com/runbear-io/beardrive/cmd/bdrive@latest
# or: grab a release tarball — https://github.com/runbear-io/beardrive/releases
```

## 2. Pick storage

Any of: `s3://bucket/prefix`, `gs://bucket/prefix`, an S3-compatible
endpoint, or — simplest for a first run — a plain directory
(`file:///var/lib/bdrive/storage`). Files and change journals live
there; hub metadata (accounts, projects, orgs) lives in the database you
pick in step 3. Clients never see this storage — they sync through the
hub over HTTPS.

## 3. Write config.json

```jsonc
{
  "remote": "file:///var/lib/bdrive/storage",
  "addr": ":4173",
  "upload": true,
  "auth": {
    // Signup is invite-only by default — the safe posture for a public
    // URL. Your first account: start once with signup gated (or use an
    // org invite), then invite teammates from the web UI.
    "admins": ["you@example.com"],
    "users_db": "/var/lib/bdrive/auth.json"
  },
  "reads": { "enabled": true },              // agent read analytics (Insights)
  "database": { "driver": "sqlite", "dsn": "/var/lib/bdrive/hub.db" }
}
```

Full knob reference (allowed domains, email verification via SMTP,
admin approval, Postgres/Supabase, share-link rate limits): the
[README's web-server section](../README.md#web-server).

## 4. Run it

```sh
bdrive web -c config.json
```

Put TLS in front (Caddy/nginx/your platform's LB) — device tokens travel
as bearer tokens. For containers, the repo ships a `Dockerfile`
(distroless, CGO-free) with a Cloud Run recipe in [deploy/](../deploy/)
— any container platform works the same way.

## 5. First sign-in and first project

1. Open `https://your-hub/` → create your account (first run: use the
   signup posture you configured; hub admins are the `admins` emails).
2. On any machine: `bdrive login https://your-hub` (browser flow), then
   in the folder you want synced:
   `bdrive init --name wiki --yes` — or `--shared docs` inside a repo to
   sync only that subfolder.
3. Invite a teammate: sidebar footer → **Manage** → **New invite** —
   the join link both creates their account and adds them to your org.
4. Connect agents: the project's home page in the web UI shows
   copy-paste setup for Claude Code/Cowork, Hermes, and Codex; or run
   `bdrive hooks install` in the folder.

## Upgrading

`brew upgrade beardrive` (clients and hub are the same binary — keep
them roughly in step; the sync protocol is append-only journals + blobs,
which old clients read forward). After upgrading a client, re-run
`bdrive hooks install` once per project to pick up any hook improvements.

## Backup

Everything irreplaceable is in two places: the storage root (blobs +
journals — files and their entire history) and the metadata database
(accounts/projects/orgs/shares). Snapshot both; restore is copy-back.
