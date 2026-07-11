# BearDrive — Google Drive for AI agents

**BearDrive** mounts any folder as a synced volume: its contents stay
synchronized across all your devices and teammates through a BearDrive
**hub**, every change is tracked (who, when, on which device), and
everything keeps working offline. The CLI is `bdrive`; a hub is a
`bdrive web` server you (or we) run on an object store — clients sync
through it over HTTPS and never touch the storage directly.

Two things it's for: **sharing files with people** — any synced file
becomes a public URL that renders as a page — and **sharing context across
AI agents**: give every agent on the team the same folder as memory, and
your agent knows what their agent knows. Notes, plans, findings, and
artifacts follow the team everywhere, with a full audit trail of which
agent or human changed what.

```console
$ bdrive login                # once per device (browser sign-in)
$ cd ~/workspace && bdrive init
initialized /Users/snow/workspace
  project: workspace (p-7f3a2c91)
  daemon:  running (pid 55434, scan 3s, remote sync 10s)
```

On another machine:

```console
$ bdrive login && cd ~/workspace && bdrive init
# … connect the same project; the files appear and stay in sync
```

## Features

- **Any folder is a project** — `bdrive init` turns any folder into a synced
  project. Files are *real files on disk*: every tool, editor, and agent can
  use them with zero integration work. Rename or move the folder freely —
  state is keyed by a stable id, never the path.
- **Multi-device sync** — devices converge through a shared hub. Each
  device only writes its own append-only journal, so no locking service is
  needed; the hub can be backed by any object store.
- **Change tracking** — `bdrive log` and the web UI's History view show
  which account changed which file, when, from which device (name, OS, IP).
  Content is stored content-addressed, so every version is retained — view
  or download any point in a file's history.
- **Cloud-provider agnostic** — a hub can store on Amazon S3 (`s3://`),
  Google Cloud Storage (`gs://`), any S3-compatible store (MinIO, Cloudflare
  R2 via `AWS_ENDPOINT_URL`), or a plain shared directory (`file://`, e.g. a
  NAS). Clients never see it.
- **Offline-first** — the working folder is always fully usable with no
  network. Changes are journaled locally and pushed when the remote becomes
  reachable again.
- **Conflict-safe** — concurrent edits resolve deterministically
  (last-writer-wins), and the losing version is preserved as a
  `name.bdrive-conflict-<device>-<time>` file. Nothing is silently dropped.
- **Selective sync** — a gitignore-style `.bdriveignore` opts files out, and
  `bdrive init --shared <dir>` (or the interactive prompt) narrows sync to
  one shared subfolder.
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
# 1. Sign this device in (once). Default server: beardrive.ai;
#    self-hosters pass their own URL.
bdrive login

# 2. Start syncing a project — interactive: create or connect a project,
#    sync the whole folder or just ./shared. Re-run any time to resume.
cd ~/my-project && bdrive init

# 3. Work normally — create, edit, delete files with any tool.
echo "remember this" > memory.md

# On every other device: bdrive login once, then bdrive init in a folder
# and connect the same project.

# See what changed, who changed it, and from which device
bdrive log

# Check sync state and the daemon
bdrive status

# Stop syncing (files stay on disk; bdrive init resumes any time)
bdrive stop
```

Renaming or moving a project folder is safe: state is keyed by a stable
project id, never the path. The daemon notices the move, steps aside, and
the next `bdrive init` (or any bdrive command) at the new location resumes
exactly where it left off — zero re-scan, zero spurious changes.

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
| `bdrive login [server-url]` | Sign this device in (browser flow; `--device` for headless; default server beardrive.ai). Switch hubs with `bdrive login <new-url>` |
| `bdrive logout` | Sign this device out — clear the saved token/account (`--forget` also drops the remembered server) |
| `bdrive init [folder]` | Create/connect a project and start syncing — interactive on a TTY, flags (`--name/--project/--shared/--yes`) for scripts; re-run to resume |
| `bdrive stop [folder]` | Stop syncing (files stay; `bdrive init` resumes) |
| `bdrive share <file>` | Public URL for a synced file (`--list`, `--revoke`, `--expires`) |
| `bdrive sync [folder]` | Run one sync cycle now. `--note <text>` stamps session context (e.g. an agent session id) onto changes — shown in `bdrive log` and hub history; keeps applying to daemon-committed changes until `--note-ttl` (default 30m) expires |
| `bdrive hooks [install]` | Register turn-boundary sync hooks with detected agent platforms (Claude Code, Codex, Gemini CLI, Hermes) — pull each turn, push after edits, session-note stamping; idempotent (`--agent` overrides detection) |
| `bdrive status [folder]` | Projects, daemon state, pending changes |
| `bdrive log [folder] [-p path] [-n N]` | Change history: account, device, time, file |
| `bdrive web [folder \| storage-root-url]` | Web server: viewer (rendered markdown, downloads, history), uploads, multi-project sync hub |
| `bdrive whoami` | Device identity used in change tracking |

## Project files

Each mounted folder carries its own settings, so configuration travels with
the project:

- **`.bdrive/`** — the folder's settings directory: `config.json` holds the
  **stable mount id** plus project/remote/include settings. Written by
  `bdrive init`, safe to hand-edit (a running daemon picks changes up
  automatically). Never synced, and it holds no credentials (the session
  token stays in `~/.bdrive`). Because everything is keyed by the mount id,
  the folder can be renamed or moved freely; copy it to another machine and
  `bdrive init` resumes the same project.
- **`.bdriveignore`** — gitignore-style opt-out list at the mount root. Syncs
  like a normal file, so every device shares the same rules. Supports `#`
  comments, `*`, `**`, `?`, trailing `/` for directories, leading `/` (or any
  `/`) for root-anchoring, and `!` to re-include.

```jsonc
// .bdrive/config.json
{ "id": "m-5a10b713", "volume": "notes",
  "remote": "https://drive.example.com/p/p-7f3a2c91", "include": ["shared/"] }
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
  "projects_db": "/var/lib/bdrive/projects.json",
  "share_rpm": 120,                  // per-IP rate limit on public /s/* links
  "auth": {                          // optional knobs; hub auth is always on
    // Signup is invite-only by default. To allow self-service signup,
    // open it WITH a gate (an ungated open hub is refused at startup):
    "allow_signup": true,
    "allowed_domains": ["example.com"],  // only these domains may sign up
    "require_approval": true,            // …and an admin must approve each one
    "users_db": "/var/lib/bdrive/auth.json",
    "admins": ["admin@example.com"],
    "smtp": { "host": "smtp.example.com", "port": 587,
              "user": "drive@example.com", "pass": "…", "from": "drive@example.com" }
  }
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

Projects are walled by **organization**: every project belongs to one org
(file-backed `orgs.json`), and only that org's members — accounts with the
`owner` or `member` role — can see, browse, or sync it. Your first
`bdrive init` creates an org for you automatically; an owner invites
teammates from the web UI (the org name in the sidebar footer — Invite
mints an expiring join link, `/join/<token>`, that any signed-in account
can open to become a member). A hub upgraded from an earlier version
sweeps its existing projects into a `default` org that all existing
accounts join, so nothing breaks. Public share links stay outside the
wall on purpose.

```sh
# On the server device (knows the storage)
bdrive web -c config.json

# On each client device (knows only the server) — one command does it all:
bdrive login https://drive.example.com:4173   # once per device
cd ~/some-project && bdrive init              # once per project
```

`bdrive login` signs the device in and remembers the server (`settings.json`
under the bdrive home; bare `bdrive login` defaults to beardrive.ai —
`--status` shows the current server and account). To move to a **different
hub**, run `bdrive login <new-url>` and then re-run `bdrive init` in each
folder to connect it to a project there; `bdrive logout` signs out entirely.
`bdrive init` then, per
project, walks you through it on a terminal: **create a new project or
connect an existing one** (picked from the server's list), and **sync the
whole folder or only a shared subfolder** (e.g. `./shared`). Every question
has a flag (`--name`, `--project`, `--shared`, `--yes`), and without a TTY
init never prompts — it creates-or-joins a project named after the folder
and syncs everything. It writes `.bdrive/config.json`, seeds a starter
`.bdriveignore` (node_modules, build dirs, caches, `.env*`), and starts the
daemon — local changes are detected within seconds, and the Claude Code
plugin syncs at every session step. Not signed in yet? init runs the login
flow first.

Under the hood the `https://` remote speaks the hub's per-project
`/api/p/<id>/store` API — journal reads/writes relay through the server,
blob uploads go direct to the object store via the same short-lived
presigned URLs browser uploads use (falling back to relaying when the
backend can't presign). Client pushes and project creation require the
server to run with `--upload`; against a read-only hub, clients still pull
and their pushes wait (offline semantics) until allowed.

### Sharing files by URL

Any synced file can be shared with a public link — hand someone the URL
and they see the file, no account needed:

```console
$ bdrive share wiki/report.html
https://drive.example.com/s/eacc1df3ee6a6ebbdacc535c2796dc30
```

Links always serve the file's **latest** synced content (right for wiki
pages and living reports), and live until revoked — `bdrive share --list`
and `--revoke <token-or-url>` manage them, `--expires 24h` makes one
self-destruct. The web UI has a Share button on every file.

Shared HTML renders as a real page, markdown renders like the viewer
(with a small "Shared with BearDrive" footer; raw HTML is served
byte-for-byte), PDFs open inline. Rendering is sandboxed: `/s/*` responses
carry a strict CSP, never see auth cookies, and sit behind a generous
per-IP rate limit (`share_rpm`), so a malicious shared file's scripts
can't touch hub sessions and a scraper can't turn the hub into a CDN.
Any org member can mint links, and a link is public to whoever has the
URL — don't share folders that hold secrets, and note a LAN-bound hub
means LAN-only links.

### Claude Code integration

The BearDrive plugin (`/plugin marketplace add runbear-io/beardrive`)
makes agents fluent in all of this, and **`/beardrive:install`** sets a
project up conversationally: installs the CLI, signs in, creates or
connects a project (whole folder or a shared subfolder like `wiki/`),
offers to document the shared folder in CLAUDE.md so agents proactively
put shareable artifacts there, and registers project-level hooks in
`.claude/settings.json` — a blocking pull when you submit a prompt (Claude
reads fresh team files) and an async push after every file edit (artifacts
are on the server seconds after Claude writes them), for every teammate
whether or not they installed the plugin. The payoff: "write a report and
share it" becomes Claude generating `wiki/report.html` and replying with a
public URL.

The web UI lists your orgs' projects in the sidebar (⌘K opens a command
palette: fuzzy file search, project switching, share/history/upload
actions); selecting one browses that project's files, and the **History**
view shows every change — which
account made it, when, from which device (name, OS, and the IP the server
observed), with view/download of any past version (content is
content-addressed and retained forever; reverting to a version is the next
phase and the API is already shaped for it). Folder rows have a history
shortcut for a subtree feed; the topbar button shows the current file's
versions or the whole project feed.

### Authentication

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

### Choosing a database

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

- **`/beardrive:install`** — the full team setup, conversationally: CLI,
  sign-in, project init (whole folder or a shared subfolder like `wiki/`),
  a consent-gated CLAUDE.md section for agents, and project-level sync
  hooks in `.claude/settings.json`.
- **`/beardrive:init [folder] [--name/--project/--shared]`** — just start
  syncing a project; `/beardrive:status` diagnoses problems.
- **Turn-boundary sync hooks**, registered automatically: a blocking pull
  when you send a message (Claude always reads fresh files) and an async
  push when the turn ends. The hook no-ops instantly in folders without a
  `.bdrive/` project, so it's safe globally.
- **The `beardrive` skill** ([plugin/skills/beardrive](plugin/skills/beardrive/SKILL.md)),
  covering init/stop/sync, sharing by URL, backends and credentials,
  selective sync, and troubleshooting. Working in a clone of this repo
  picks the same skill up automatically via `.claude/skills/`.

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
  --remote-interval on `bdrive init`.

### What beardrive does not sync

`.git` directories (per-file LWW would corrupt repositories), `.DS_Store`,
the `.bdrive` settings file, its own temp files, nested mounts (a
subdirectory with its own `.bdrive/config.json` syncs only through its own
project — the parent never scans into it, writes over it, or propagates
deletes for it), and anything excluded by `.bdriveignore` or omitted from an
`include` list. Empty directories are not tracked (like git).

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

GNU AGPL-3.0 — Copyright 2026 Runbear, Inc. See [LICENSE](LICENSE).

Everything in this repo is open source and self-hostable: a complete BearDrive
server for one organization's deployment, teams included. The managed service
at beardrive.ai is the same core plus what only makes sense as an operated
service — hosting, PropelAuth SSO, billing and plan quotas, backups, and
support. Provider-specific and billing code stays out of this repo permanently;
the server exposes interfaces (`AuthProvider`, `QuotaProvider`) that the
managed deployment fills in.
