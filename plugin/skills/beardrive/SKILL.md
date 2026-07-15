---
name: beardrive
description: Use BearDrive — the open-source Google Drive for AI agents and their teams. Start syncing any project folder (bdrive init) and it stays in sync across devices and teammates through a BearDrive hub, with accounts, per-file change history, public share links, and offline support. Use when the user wants to "set up beardrive", "sync this folder", "share this file by URL", "start/stop syncing", "connect to a beardrive server", "switch to a different hub", "check bdrive status", "see what changed", "who changed this file?", or troubleshoot a stuck sync.
---

# BearDrive — Google Drive for AI agents

**BearDrive** (CLI: `bdrive`) turns any folder into a synced project: a background daemon per project scans for local changes and exchanges them with a **hub** (a `bdrive web` server). Files on disk are always real files — every tool, editor, and agent works on them with no integration.

Use this skill whenever the user is working with the `bdrive` CLI: initializing or stopping projects, syncing, sharing files by URL, signing in / switching hubs, inspecting state, reading change history, or debugging. ("Mount" in older docs = today's `bdrive init`.)

## Command map

| Action | Command |
|---|---|
| Start syncing a project (create/connect; the front door) | `bdrive init [<folder>]` — interactive on a TTY; flags `--name <x>` / `--project <id>` / `--shared <dir>` / `--yes` for scripts and agents (NEVER prompts without a TTY). Re-run to resume, including after the folder was renamed/moved. Runs `bdrive login` first if the device has no session. |
| Run the daemon in the foreground | `bdrive init -f` |
| Stop syncing | `bdrive stop [<folder>]` (`--forget` also unregisters) |
| One sync cycle now | `bdrive sync [<folder>]` |
| Register agent sync hooks (Claude Code, Codex, Gemini CLI, Hermes) | `bdrive hooks install [<folder>]` — auto-detects the platforms in use and merges pull/push/session-note/read-tracking hooks into each one's own hook config, idempotently; bare `bdrive hooks` shows the status table |
| Record agent file reads (hook plumbing) | `bdrive read-log [<folder>]` — parses a hook event JSON from stdin and queues in-project reads locally (native reads, grep matches, and files named in shell commands); drained to the hub on the next sync as agent traffic in the read heatmap. Registered automatically by `bdrive hooks install`; rarely run by hand |
| Mounts + daemon + pending state | `bdrive status [<folder>]` |
| Change history | `bdrive log [<folder>] [-p path] [-n N]` |
| This device's identity | `bdrive whoami` |
| Sign this device in (once per device) | `bdrive login [url]` — bare form uses the remembered server or beardrive.ai. Opens the sign-in page in a browser (sign-up available there); the terminal completes on its own and stores a per-device token. `--device` prints a code to approve from any browser (SSH/headless); `--status` shows server + account. Password reset: "Forgot password?" on the sign-in page (emailed via the server's SMTP config, or the link appears in the server log). **Switch hubs** with `bdrive login <new-url>`, then re-run `bdrive init` in each folder. |
| Sign this device out | `bdrive logout` — clears the saved token + account (folders untouched); `--forget` also drops the remembered server. The device token stays valid server-side until it expires — revoke it from the hub's device list to be sure. |
| Link a synced file for teammates | `bdrive url <file>` — prints the file's hub viewer URL (sign-in + project membership required; always the latest content). Computed locally, no network; `--sync` pushes first so a just-created file's link resolves immediately; no arg = the project home page. **After creating a shareable artifact (.md/.html/.csv/report/plan) in the shared folder, include this link in your reply** so teammates can open it. |
| Share a synced file publicly by URL | `bdrive share <file>` — prints a link anyone can open (HTML renders as a page, markdown rendered, PDFs inline; sandboxed; always the latest content; no account needed). `--expires 24h` for self-destructing links; `--list` / `--revoke <token-or-url>` to manage. Put generated reports in the shared folder, sync, then share. |
| Set up a project for a Claude Code team | `/beardrive:install` — installs the CLI, signs in, runs init (whole/shared folder), offers the two-file agent orientation (synced `<shared>/AGENTS.md` map + repo-root pointer), and registers agent sync hooks via `bdrive hooks install` (pull at turn start, push after edits, session-note stamping — for every detected platform, not just Claude) |
| Per-file / folder change history in the web UI | History button (file versions or project feed) and per-folder ⌚ — each entry: account, time, device (name/OS/IP), view/download of that exact version. API: `GET /api/p/<id>/history?path=\|prefix=`, `GET /api/p/<id>/blob?sha=` |
| Web server: viewer + multi-project sync hub (read-only unless `--upload`) | `bdrive web [<folder> \| <storage-root-url>]` (serves cwd by default, `--addr :4173`; `-c config.json` reads remote/addr/upload/projects_db/database/auth settings from a file, explicit flags win; a storage root URL makes it a hub hosting many projects at `<root>/<project-id>/`, registry in `--projects-db` file, default `$BDRIVE_HOME/projects.json`; `--upload` lets browsers add files, client devices push, and projects be created — direct to storage via expiring presigned URLs on S3/GCS, relayed through the server for `file://`; `--upload-ttl 15m`; clients never see the remote URL or credentials; hub projects are walled by org membership — invite teammates from the web UI; the viewer has a ⌘K palette for fuzzy file search, project switching, and quick actions) |

`<folder>` is created if missing. Omitting it on `sync`/`status`/`log` defaults to the current working directory.

## Project files

Two files at the mount root control a folder's sync behavior:

- **`.bdrive/`** — the folder's settings **directory**; `config.json` inside holds the **stable mount id** (`m-xxxxxxxx`) plus `volume`, `remote`, optional `include`. Written by `bdrive init`; safe to hand-edit (a running daemon picks changes up on its next tick). It is **never synced**, holds **no credentials** (the token lives in `~/.bdrive/settings.json`), and because all state is keyed by the mount id — not the path — the folder can be **renamed or moved freely**; the daemon exits on a move and the next bdrive command at the new location resumes.
- **`.bdriveignore`** — opt-out list, gitignore-style. **Syncs like a normal file**, so all devices share the same rules. Syntax subset: `#` comments, `*` within a segment, `**` across segments, `?`, trailing `/` for directories-only, a `/` elsewhere anchors to the mount root, `!` re-includes.

```jsonc
// .bdrive/config.json
{
  "id": "m-5a10b713",
  "volume": "agent-workspace",
  "remote": "https://drive.example.com/p/p-7f3a2c91",
  "include": ["shared/"]   // optional: sync ONLY these (set by init --shared)
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

## 1. Init / stop / sync

### Init flow

1. `bdrive login` once per device (browser sign-in; default server beardrive.ai; sign-up available on the page).
2. Run `bdrive init` in the folder. Interactive on a TTY (create new / connect existing project; whole folder / shared subfolder); with flags or without a TTY it creates-or-joins a project named after the folder and syncs everything. It:
   - writes `<folder>/.bdrive/config.json` (mount id + project + remote) and registers the mount id in `~/.bdrive/mounts.json`,
   - seeds a starter `.bdriveignore` (node_modules, build dirs, caches, `.env*`) when none exists,
   - opens the volume store under `~/.bdrive/volumes/<mount-id>/`,
   - runs an initial cycle (import locals; pull project state),
   - starts a background daemon (unless `-f`).
3. Verify with `bdrive status <folder>`.

### `bdrive init` flags

- `--name <x>` — project name to create-or-join (default: folder basename).
- `--project <id>` — connect an existing project by id (`p-xxxxxxxx`).
- `--shared <dir>` — sync only this subfolder (becomes the include list; remote paths keep the prefix so all devices see the same layout).
- `--yes, -y` — accept defaults, never prompt.
- `--foreground, -f` — run the daemon in the foreground (systemd/launchd/containers).

### Multi-device setup

```sh
# every machine: sign in once, then per project
bdrive login https://drive.example.com:4173
cd ~/agent-workspace && bdrive init --name agent-workspace
```

Devices connecting the same project (by name or id) converge through the hub. A hub is required — clients always sync through a `bdrive web` server and never talk to the object store directly.

Hub projects belong to an **organization**: only members of the project's org can see or sync it (project names are scoped per org too). Your first `bdrive init` creates your org automatically. **Hubs are invite-only by default** — the safe posture for a public URL. To give a teammate access, an org **owner** opens the web UI and clicks **Invite** in the sidebar footer — it mints an expiring join link (`…/join/<token>`); the teammate opens it and creates an account through the link (invites bootstrap signup even when public self-signup is closed), and is in. An admin can instead open self-service signup with a gate (admin approval, or allowed-domains + email verification) under **Admin → Signup & access** / the config's `auth` block.

A hub stores its metadata (accounts, projects, orgs, invites, shares, devices — never files or journals, which stay in object storage) in a database chosen by the config's `database` block: `{"driver":"file"}` (default, JSON under `$BDRIVE_HOME`), `{"driver":"sqlite","dsn":"…/hub.db"}`, or `{"driver":"postgres","dsn":"postgres://…"}` for a managed Postgres such as Supabase. file/sqlite are single-writer; Postgres backs multiple instances. If a teammate's `bdrive init --project <id>` gets 403/404 or the project list looks empty, the missing invite is the reason. Public share links (`bdrive share`) intentionally bypass the org wall; internal links (`bdrive url`) stay behind it — prefer them for teammates, and reserve `bdrive share` for people outside the hub.

**Share what you make**: whenever you create a shareable artifact in the synced folder — a report, plan, analysis, or export (.md, .html, .csv, .pdf, …) — get its internal link with `bdrive url <file>` and include it in your reply. The sync hooks push within seconds so the link resolves almost immediately; use `bdrive url <file> --sync` when the reader will click right away. Never mint a public `bdrive share` link for this unless the user asks for one.

### Renames and moves

Renaming/moving a project folder is safe: the daemon notices its folder vanished and exits **without propagating any deletes**; run `bdrive init` (or any bdrive command) at the new location and it resumes with zero spurious changes. `bdrive status` shows stale paths as "folder missing".

### Stop

- `bdrive stop <folder>` — stop the daemon. Files stay on disk; the local volume store under `~/.bdrive/volumes/<mount-id>/` is kept. `bdrive init` resumes any time.
- `bdrive stop <folder> --forget` — also drop from the mount registry. Local volume data is still preserved; `rm -rf ~/.bdrive/volumes/<mount-id>/` reclaims disk.

### On-demand sync

`bdrive sync [<folder>]` runs a single cycle (scan → upload blobs+journal → pull remote journals → materialize). Useful to:

- Push a change immediately instead of waiting for the next interval.
- Verify credentials and the remote end-to-end.
- Sync once even when the daemon is stopped.

**Session-linked notes**: `bdrive sync --note "<text>"` stamps the text onto
every change this cycle commits — and persists it (in the mount's volume
store, never synced) so changes the background daemon commits over the next
`--note-ttl` (default 30m) carry it too. Notes appear in `bdrive log` (as
`[note]`) and under each entry in the hub's history views. The plugin's sync
hooks pass `--note "claude-code session <session-id>"` automatically, so
every change made during a Claude Code session is traceable to the session
that made it. An explicit empty `--note ""` clears the persisted note;
conflict-copy ops keep their own `conflict copy of <path>` note.

### Agent sync hooks (Claude Code, Codex, Gemini CLI, Hermes)

`bdrive hooks install [<folder>]` registers turn-boundary sync for every
agent platform it detects (by config dir, in the project or home):

| Platform | Config it writes | Pull / push / read events |
|---|---|---|
| Claude Code (& Cowork) | `<project>/.claude/settings.json` | `UserPromptSubmit` / `PostToolUse` (Write\|Edit) / `PostToolUse` (Read\|Grep\|Bash) |
| Codex (ChatGPT) | `<project>/.codex/hooks.json` | `UserPromptSubmit` / `PostToolUse` (apply_patch) / `PostToolUse` (read_file\|shell, best-effort) — user must `/hooks`-trust the layer once |
| Gemini CLI | `<project>/.gemini/settings.json` | `BeforeAgent` / `AfterTool` (write_file\|replace) / `AfterTool` (read_file\|read_many_files\|search\|shell) |
| Hermes | `~/.hermes/config.yaml` (per-user) | `pre_llm_call` / `post_tool_call` (write_file\|patch) / `post_tool_call` (read_file\|grep\|bash) |

Every platform pipes hook JSON with a `session_id`, so one hook command
serves all four: it syncs the project (fast no-op outside bdrive folders)
and stamps changes with `<agent> session <id>`. The read hook runs `bdrive
read-log`, which queues the read locally (no network) for the hub's read
heatmap — coverage is tool-aware: native read tools report their paths,
grep-style searches count the files their matches came from, and shell
commands count the existing files they name (`cat notes.md`, `tail wiki/log.md`);
listing tools (glob, ls) are deliberately excluded — seeing a name isn't
reading. Merging is idempotent and preserves existing hooks — each hook
carries its own marker, configs from before the read hook gain just the
missing group on re-install, and a registered hook's matcher is upgraded
in place when coverage grows (re-run `bdrive hooks install` after upgrading
the binary); `--agent claude,codex,gemini,hermes` overrides
detection; bare `bdrive hooks` prints the detection/registration table.
Project-level configs ride the repo, so hooks reach the whole team.

### Read heat (who actually reads what)

Hubs aggregate reads per file — viewer opens and downloads count as human
reads, share-link hits as share reads, and hook-reported agent reads as
agent reads; `/store` sync replication never counts. The web UI shows heat
dots and read counts on folder listings (all members), and admins / org
owners get an **Insights** dashboard (⋯ menu, all/human/agent lens): a
treemap of every file (size = 30-day reads, color = staleness, ⚠ =
hot+stale, click-through), a reads × freshness scatter (hot-but-stale
quadrant = fix first), the hot path (top files by reads, agent/human
split), and an agent coverage matrix (agent devices × folders). Counts
only, never human reader identities. API:
`GET /api/p/<id>/heat?prefix=&days=30`, plus `?by=device` for the
agent-only per-device folder breakdown. Server config: `"reads":
{"enabled": true, "retention_days": 400}` (on by default in hub mode).

### Examples to walk a user through

```sh
# Brand-new shared project (interactive)
cd ~/agent-workspace && bdrive init

# Non-interactive (agents/scripts): join-or-create by name, whole folder
bdrive init ~/agent-workspace --name agent-workspace --yes

# Only share a subfolder
bdrive init ./research --shared shared

# Pause syncing for the day
bdrive stop ~/agent-workspace

# Drop a folder entirely (keeps local volume history)
bdrive stop ./notes --forget
```

### Connecting knowledge tooling (gbrain, OKF, docs folders)

When guiding `bdrive init`, detect existing knowledge tooling and connect it instead of blind-syncing the folder. Two rules govern every case:

- **One transport per folder.** Never sync a folder that has another writer. Git-tracked paths: a teammate's `git pull` or branch switch rewrites files with older content, and sync broadcasts that as a fresh edit — silently reverting the team's latest pages. A gbrain brain root: every private capture and overnight enrichment would become team-visible, and each member's cron rewriting the same pages fills the project with conflict copies. Moving a folder from git to beardrive is a **handoff**: `git rm -r --cached <dir>` + add `<dir>/` to `.gitignore`, stage the change but let the user commit it (teammates then pull and re-init; identical content converges with no conflicts). If they want a git record anyway, offer **one-way snapshots** (a scheduled job commits the synced folder's state to an archive branch — git only ever reads the folder) and note that hub history (`bdrive log -p <path>`) usually covers the need.
- **Knowledge syncs as a scoped folder.** Inside a repo, always `--shared <dir>` — never the repo root. A dedicated knowledge folder (an empty dir, a standalone vault) may be the mount itself. The sync scope is per-device (`.bdrive/` never syncs), so recommend the same `--shared <dir>` when each teammate connects.

Detection ladder — first match wins; if two rungs match, ask which to connect:

1. **gbrain** — `gbrain.yml` in the folder, or a gbrain MCP server in `.mcp.json` / a brain-first protocol block in CLAUDE.md (brain root is then found via `~/.gbrain/config.json` or the MCP config). gbrain's own architecture states the markdown repo is the system of record and the DB a derived cache — beardrive replaces the git hop as the brain's sync transport:
   - The shared team folder is **never anyone's brain root**. First user: share a subfolder of their brain (e.g. `~/brain/team/`) as its own project. Followers: mount that project as a subfolder of their own brain (or a sibling dir) — never merge roots; "merge my existing brain into it" is a manual migration, not an init option.
   - Register the shared folder as its **own gbrain source** (`gbrain sources add`; `.gbrain-source` dotfile routes writes) — slugs are per-source, the `federated` flag controls blending into personal search, and the schema pack is settable per source so team pages keep their types (`person`, `company`, …) instead of degrading to untyped notes.
   - Recommend `gbrain config set link_resolution.global_basename true` so the team's `[[wikilinks]]` resolve from any mount path (`gbrain doctor` reports the edge gain first).
   - Mirror the brain's `db_only` dirs from `gbrain.yml` into `.bdriveignore` (machine-generated, restorable from the DB — and often a privacy fence; ask before syncing them).
   - gbrain's sync cron keeps re-indexing what beardrive pulls in; suggest switching it to `gbrain sync --no-pull` (ask before editing a crontab). On PGLite, remind: stop `gbrain serve` before large syncs (single-writer contention).
   - **One enrichment owner per shared folder.** Exactly one gbrain instance may write enrichment into the shared source — prefer a dedicated bot account/device (hub-side if possible) over a member's laptop; every other member indexes the source read-only with enrichment off for it. Members still capture: route each member's automated writes (email ingestion, meeting notes) into their own subtree (e.g. `inbox/<member>/`) so no path ever has two writers, and let the owner fold captures into canonical pages. Nothing enforces the election, so a `*.bdrive-conflict-*` file naming the owner's device is the canary that a second enricher (or a human race) appeared — surface it, don't ignore it.
   - **Owner's cycle discipline**: sync (pull) → `gbrain sync` (import human edits into the DB) → enrich → write back promptly, skipping files modified in the last few minutes and `*.bdrive-conflict-*` files. A write-back from a stale DB copy after a pull is causally "later," so it silently reverts the human's edit with **no conflict copy** — re-check the file changed since import before writing. Keep enrichment output deterministic (stable ordering, no embedded timestamps): byte-identical rewrites produce no op at all, so idempotent passes are free.
2. **OKF** — markdown with OKF v0.1 frontmatter (confirm with `openknowledge validate` if the CLI is present; don't install just to detect). Offer: (a) connect the wiki dir via `--shared` — with the git handoff if tracked — or (b) keep the wiki PR-gated in git and create a new shared folder (starting-point menu below). Recommend (a) when the wiki is the team's knowledge, (b) when it's review-gated repo documentation.
3. **Wiki-ish folder** — a markdown-dense dir named `docs`/`wiki`/`notes`/`kb` with no knowledge tooling. Check `git log -- <dir>`: dormant → recommend connecting it as the live team space (handoff included); active PR traffic → recommend a new shared folder instead, and say why. After connecting, offer — as a separate consent, it rewrites their files — an in-place upgrade to OKF (`openknowledge from <dir>`) for validation and agent-readability.
4. **Nothing** — empty or unstructured folder: offer a starting point, in this order: **(a) OKF (recommended** — open spec, plain files, zero runtime, upgradeable to gbrain later**)**, (b) gbrain (full agent brain; heavier — per-member local DB), (c) blank, (d) describe-it (user describes the purpose; scaffold a custom OKF shape, redirecting to gbrain if the description is graph-shaped: entities, relationships, "who/what" queries).

Conflict copies are named `<file>.bdrive-conflict-<device>-<timestamp>` and sync like normal files. `openknowledge validate` does **not** flag them (they aren't `.md`) — pair validate with a `*.bdrive-conflict-*` glob check when offering a post-edit validation hook.

Every branch ends the same way: verify (`bdrive status`, pending 0), the consent-gated two-file agent orientation (next section), and the teammate onboarding sentence (invite link → `bdrive init` → same `--shared` scope).

### Teaching agents the folder (AGENTS.md)

A newly mounted shared folder is hundreds of opaque files to an agent. Two files with different jobs fix that — offer each as its own consent, never write either silently:

1. **The folder's own map — `<shared>/AGENTS.md` (synced, team-wide).** The single source of truth for conventions: what each area is for, naming patterns, where agents should *write* (e.g. reports → `reports/`), what not to touch. Because the folder syncs, the map travels with it — every member on every platform gets it, and hub history tracks who changed the rules. It is scaffolded **once, by the project creator** (explore the folder, draft it, keep it under a screen); joiners read it and follow it — a new member's agent must not rewrite team conventions on day one.
2. **A root pointer (per machine, never synced).** For `--shared` mounts inside a repo, append 2–3 lines to the repo root's `AGENTS.md` and/or `CLAUDE.md` (both if both exist): the folder is shared via BearDrive, read `<shared>/AGENTS.md` before working there, put shareable artifacts there, no secrets. Point at the synced map — don't duplicate its conventions, or the copy goes stale.

The pointer is not optional politeness — platform discovery differs:

| Platform | Finds `<shared>/AGENTS.md` on its own? |
|---|---|
| Claude Code / Cowork | Lazily — loaded when a file in that subtree is first read |
| Hermes | Lazily — progressive discovery; walks up from files it touches |
| Codex | **Never** — only loads `AGENTS.md` along the root→cwd path |

And even where lazy loading works, it fires only *after* the agent decides to enter the folder; only the root pointer gives it the awareness to go there ("save the report where the team sees it"). A standalone knowledge mount (a dedicated folder, no enclosing repo) needs only the synced `AGENTS.md`: at the mount root every platform loads it at session start.

**Orientation ritual** — your own behavior in any synced folder: on first contact, read its `AGENTS.md` before substantive work. If there is none, orient from the tree plus `bdrive log <folder>` (recent changes show which areas are alive), and — if this device created the project — offer to draft `AGENTS.md` for the team.

### What beardrive does not sync

`.git` directories, `.DS_Store`, the `.bdrive` settings file, beardrive's own temp files, empty directories, nested mounts (a subdirectory with its own `.bdrive/config.json` syncs only through its own project — the parent never scans into it, materializes over it, or propagates deletes for it), and anything excluded by `.bdriveignore` or left out of an `include` list. Don't suggest mounting a folder where `.git` is the content the user expects synced — they want git, not beardrive.

---

## 2. Storing a hub's data (object storage)

This section is about the **hub** (`bdrive web <storage-root>`), which keeps its blobs + journals in an object store you point it at — **clients never touch storage**; they sync through the hub over `https://`. The credentials below live on the hub's machine, not on client devices. beardrive uses each provider's standard credential chain — nothing beardrive-specific.

### Supported URL schemes

| Scheme | Backend | Example |
|---|---|---|
| `s3://bucket/prefix` | Amazon S3, or any S3-compatible store via `AWS_ENDPOINT_URL` | `s3://acme-beardrive/agent-workspace` |
| `gs://bucket/prefix` | Google Cloud Storage | `gs://acme-beardrive/agent-workspace` |
| `file:///abs/path` | Plain directory (local, NAS, Dropbox folder, …) | `file:///Volumes/nas/beardrive/notes` |
| `https://host:port/p/<project-id>` | One project on a `bdrive web` hub — the client holds **no storage credentials**; the server device owns the bucket config. Server must run with `--upload` for clients to push. Set up with `bdrive init` (never hand-write the `/p/<id>` URL) | `https://drive.example.com:4173/p/p-7f3a2c91` |

The `s3://`/`gs://`/`file://` URL is the **hub's** storage root; the prefix can be multi-segment (`s3://bucket/team/agent/workspace`), and beardrive writes `blobs/` and `journal/` underneath it. The `https://…/p/<id>` scheme is what a **client** folder uses, wired automatically by `bdrive init` (never hand-written).

### Pointing a hub at storage (server-side)

```sh
# Start a hub on an S3 bucket; clients then `bdrive login` + `bdrive init`
bdrive web s3://acme-beardrive/root --upload

# Or from a config file
bdrive web -c config.json    # { "remote": "s3://acme-beardrive/root", "upload": true, … }
```

Clients never set storage: they run `bdrive login <hub-url>` once, then `bdrive init` per folder.

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
bdrive web s3://my-r2-bucket/workspace --upload

# MinIO
export AWS_ENDPOINT_URL=http://minio.local:9000
export AWS_REGION=us-east-1
export AWS_ACCESS_KEY_ID=minioadmin
export AWS_SECRET_ACCESS_KEY=minioadmin
bdrive web s3://beardrive/workspace --upload
```

Persist these in the user's shell rc or a systemd/launchd unit so the daemon also has them.

### Google Cloud Storage (`gs://`)

Application Default Credentials (ADC):

```sh
# Interactive workstation
gcloud auth application-default login

# Service account
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/key.json

bdrive web gs://acme-beardrive/workspace --upload
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

The daemon inherits the env of the `bdrive init` invocation. If you set `AWS_PROFILE` in a one-off shell, mounted, and opened a new shell without it — the daemon is fine, but `bdrive sync` from the fresh shell may fail credential lookup. For long-lived setups, put credentials in the shell rc or the launchd/systemd unit.

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
- **`daemon`** — `running (pid N)` or `stopped`. If stopped, run `bdrive init <folder>` to start a daemon, or `bdrive sync <folder>` for a one-shot.
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

History is content-addressed — overwritten and deleted files are still in the log, with blobs retained under `~/.bdrive/volumes/<mount-id>/blobs/`.

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

To change name/author, edit `~/.bdrive/device.json` and restart the daemon (`bdrive stop`/`bdrive init`).

### The per-mount daemon log

```sh
# Volume contents (daemon pid + log files live here, one pair per mount)
ls ~/.bdrive/volumes/<mount-id>/

# Tail
tail -F ~/.bdrive/volumes/<mount-id>/daemon.log
```

Useful when `pending` is stuck > 0, the daemon flips to `stopped` after a restart, or you changed credentials and want to confirm uptake.

### Diagnostic flow ("beardrive doesn't seem to be working")

1. `bdrive status` — folder listed? daemon `running`? `pending` stuck > 0?
2. `daemon: stopped` → `bdrive init <folder>` to restart it.
3. `pending` stuck → `bdrive sync <folder>` and read the cycle output. Errors here point at the remote — see the cloud-storage troubleshooting table above.
4. Sync succeeds but the other device doesn't see changes → `bdrive sync` on the other device + `bdrive log` to confirm the op crossed over.
5. Daemon keeps dying → tail `~/.bdrive/volumes/<mount-id>/daemon.log` for the cause.

---

## On-disk layout

```
~/.bdrive/
├── device.json              # identity (bdrive whoami)
├── mounts.json              # mount registry (bdrive status)
└── volumes/<mount-id>/
    ├── blobs/               # content-addressed file content
    ├── journal/             # per-device append-only op logs
    ├── state-<mount-id>.json # what's currently materialized
    ├── sync.json            # lamport clock + push cursor
    └── daemon.pid / daemon.log    # daemon state + log
```

Don't suggest editing files under `volumes/` directly — beardrive owns them. `device.json` and `mounts.json` are safe to inspect; `mounts.json` is safe to hand-edit if a mount entry needs surgery, but prefer `bdrive stop --forget` then `bdrive init`.

Override the whole tree with `BDRIVE_HOME=/path` (used heavily in tests and ephemeral environments).
