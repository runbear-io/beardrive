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

Full knob reference for auth and databases: the sections below;
share-link rate limits and upload TTLs: the
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
4. Connect agents: the project's home page in the web UI shows one-paste
   setup for Claude Code/Cowork, Hermes, and Codex — hub URL and project
   id already filled in. Teammates paste it into their own agent, which
   installs the CLI, keeps the beardrive skill (`bdrive skill install`),
   signs in, mounts the project, and registers the sync hooks.

## Authentication reference

Hubs always require sign-in — every change is attributed to a real account.
The whole API — web UI, uploads, project creation, device sync — needs a
session; only `/api/config` and the auth pages stay open (the plain-folder
viewer, `bdrive web ./folder`, remains auth-free). Accounts are
email + password + name, kept in a file-backed registry (`auth.json`:
bcrypt password hashes and SHA-256 token digests, atomically rewritten —
no plaintext credentials ever touch disk).

**Signup is invite-only by default** — the safe posture for a hub on a
public URL. New people get in only through an expiring invite link an owner
mints; the link lets them create an account (bypassing the gates below) and
join, in one step. To allow self-service signup instead, set
`"allow_signup": true` **with a gate** — the server refuses to start an open
hub that has none, so a fake email can never just walk in. Three postures:

- **Invite-only** (default): `allow_signup` unset/false. Only invite links create accounts.
- **Approval-gated**: `allow_signup: true` + `require_approval: true` — anyone can sign up, but a hub admin approves each new account before it works (no SMTP needed).
- **Domain-restricted + verified**: `allow_signup: true` + `allowed_domains: ["you.com"]` + `require_verification: true` (needs `smtp`) — only your company's addresses may sign up, each confirming an emailed link. Verification without SMTP is refused (the link would otherwise only reach the server log).

Admins tune verification/approval live from the web UI (**Admin → Signup &
access**); `allowed_domains`, the admin list, and `allow_signup` are
server-config-owned so a browser session can never widen who gets in.

`bdrive login <url>` on a client device opens the server's sign-in page in
a browser (sign up right there if needed); when the user signs in, the
page bounces a one-time code to the CLI's loopback listener and the
terminal finishes on its own, storing a long-lived per-device token
(revocable server-side). On headless/SSH machines, `bdrive login --device`
prints a short code to approve from any signed-in browser instead. Every
sync and every `bdrive init` then authenticates with that token.

"Forgot password" emails a one-hour reset link via the `auth.smtp` block —
plain SMTP, so any provider works. With no SMTP configured, the link is
printed to the server log so an admin can hand it over; reset is never
fully broken.

Two notes: put a hub behind TLS (reverse proxy
or tailscale) — `bdrive login` warns when signing in over plain http to a
non-localhost address. Internally all of this sits behind an
`AuthProvider` interface; the open-source server ships the built-in
email/password provider, and alternative identity backends can be swapped
in without touching the CLI or the API.

## Choosing a database

A hub keeps a little **metadata** — accounts, projects, orgs, invites,
shares, devices — separate from your files. (File content and the sync
journals always live in the object store; the database never holds them.)
You choose where that metadata lives with the `database` block:

```jsonc
"database": { "driver": "file" }                       // default — JSON under BDRIVE_HOME
"database": { "driver": "sqlite",   "dsn": "/var/lib/bdrive/hub.db" }
"database": { "driver": "postgres", "dsn": "postgres://…@…pooler.supabase.com:6543/postgres" }
```

- **file** (default): zero dependencies, human-readable JSON, perfect for a
  laptop or a small self-hosted hub.
- **sqlite**: one embedded database file — a real DB locally with no server
  to run.
- **postgres**: a managed Postgres such as **Supabase** for production —
  just point `dsn` at its connection string (use the transaction pooler for
  many connections). Since Supabase *is* Postgres, this stays fully
  open-source with no managed-only lock-in.

`file` and `sqlite` are single-writer (run one hub instance); Postgres is
transactional and can back more than one instance. Switching backends
doesn't migrate existing data — pick one when you set the hub up. Both SQL
drivers are pure Go, so the binary stays a CGO-free static build.

## Upgrading

`brew upgrade beardrive` (clients and hub are the same binary — keep
them roughly in step; the sync protocol is append-only journals + blobs,
which old clients read forward). After upgrading a client, re-run
`bdrive hooks install` once per project to pick up any hook improvements,
and `bdrive skill install` once per machine to refresh the agent skill.

## Backup

Everything irreplaceable is in two places: the storage root (blobs +
journals — files and their entire history) and the metadata database
(accounts/projects/orgs/shares). Snapshot both; restore is copy-back.
