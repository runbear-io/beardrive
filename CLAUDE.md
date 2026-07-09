# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

**BearDrive** is the product name; **`bdrive`** is its CLI binary (file conventions: the `.bdrive/` settings directory and `.bdriveignore` at the project root, `~/.bdrive` home, `BDRIVE_HOME`). BearDrive is a Go CLI that mounts any folder as a synced volume: contents sync across devices through cloud object storage (S3, GCS, S3-compatible, or a plain directory), with per-file change history and offline support. No server required — devices converge through append-only journals in a dumb object store; an optional `bdrive web` server can front the store as a sync hub for storage-blind client devices.

The repo ships one binary: `cmd/bdrive` — the CLI, the sync daemon, and the web server (`bdrive web`: viewer, uploads, multi-project sync hub).

## Commands

```sh
go build ./...                                   # build everything
go test ./...                                    # run all tests
go test ./internal/syncer -run TestConflict -v   # run a single test
go vet ./...                                     # vet
go build -o bdrive ./cmd/bdrive                  # build the binary (gitignored at repo root)
```

There is no Makefile, linter config, or CI config in-repo. Releases run `goreleaser release` on a tagged commit (see `.goreleaser.yaml`); the version is injected via `-ldflags "-X main.version=..."` into `cmd/bdrive/main.go`.

When testing the CLI manually, set `BDRIVE_HOME=/some/tmp/dir` to relocate all beardrive state (device identity, mount registry, volume stores) away from the real `~/.bdrive`.

## Architecture

Data flows in two hops; the local volume store is the pivot:

```
working folder  ←scan/materialize→  volume store (~/.bdrive/volumes/<vol>)  ←push/pull→  object store
 (real files)                       blobs/ + journal/ + state + sync                  s3:// gs:// file://
```

Package roles (`internal/`):

- **`journal`** — the core data model. Every change is an `Op` (`put`/`delete`) in a per-device append-only JSONL log. `Less` defines the total order `(lamport, time, device, seq)`; `Replay` folds all ops into the volume state, last-writer-wins per path. Everything else is machinery around this.
- **`store`** — a volume's local on-disk state: content-addressed blob store (`blobs/<aa>/<sha256>`), per-device journal copies, the per-mount materialization cache (`state-<mount-id>.json`, size+mtime fingerprints for cheap change detection), sync state (lamport clock + push cursor), and the exclusive flock that serializes cycles.
- **`remote`** — the `Backend` interface (Put/Get/List/Exists) with `file://`, `s3://`, `gs://`, and `https://` implementations (`https://` syncs through a `bdrive web` server's `/api/store` API — the client device holds no storage credentials). `PutSigner` is the optional presign capability (S3/GCS). Remote layout: `blobs/<sha256>` + `journal/<device>.jsonl` under the URL prefix.
- **`syncer`** — the heart: `Session.Cycle()` runs one pass: scan → commit local ops → pull peer journals → preserve conflict copies → materialize merged state → push blobs + own journal. Read the package doc comment in `syncer.go` first. `ignore.go` holds the path filter (`.bdriveignore` rules + the `.bdrive` include list), applied symmetrically in scan and materialize; a newly filtered path is dropped from the cache *without* a delete op so opting out locally never deletes remotely.
- **`daemon`** — per-mount background loop (detached process, `daemon.pid`/`daemon.log` in the mount's volume dir). Scans every `--scan-interval` (3s), talks to the remote every `--remote-interval` (10s) or immediately after local edits. Re-reads `.bdrive/config.json` each tick; if it vanishes (folder moved/renamed/deleted) the daemon **exits cleanly without propagating deletes** — the next bdrive command at the new location resumes it (self-heal on next touch).
- **`config`** — global state under `$BDRIVE_HOME` (default `~/.bdrive`): device identity (`device.json`), settings (`settings.json`: default server + device token + signed-in account), and the mount registry (`mounts.json`, keyed by **stable mount id**, holding only each mount's last-known path). The per-folder `.bdrive/` directory (`project.go`) holds `config.json` with the mount id + volume/remote/include; **nothing is keyed by the folder path**, so renames/moves are free — `ResolveMount` self-heals the registry path, and the volume store lives at `~/.bdrive/volumes/<mount-id>/`. `.bdrive/` is never synced and holds no credentials.
- **`webapp`** — the `bdrive web` server, in two modes. Single-volume: `Source` is a `DirSource` (plain folder from disk) or `RemoteSource` (folds journals into a file tree with per-file provenance). Hub: `Root` + `Projects` host many projects on one storage root, each under `<root>/<project-id>/` via `remote.Prefixed`; `ProjectDB` (`projects.go`) is a file-backed registry (JSON, loaded at open, rewritten atomically per change) with create-or-join-by-name semantics, name-scoped per organization. Orgs (`orgs.go`, file-backed `orgs.json`) wall projects by membership (email → owner|member): every per-project route — viewer APIs, uploads, history, shares management, the `/store/*` sync proxy — 403s for non-members, `/api/projects` lists only your orgs' projects, owners mint expiring multi-use invite links (`/join/<token>`), and a pre-org hub migrates all projects into a "default" org (all existing accounts join, oldest owns) at startup. `QuotaProvider` (`quota.go`) is the plan-enforcement seam mirroring `AuthProvider` — CheckWrite/RecordUsage on every write path, CheckSeat on invite redemption; OSS ships only `UnlimitedQuota`, managed deployments swap the provider. Renders markdown (goldmark + Obsidian `[[wikilinks]]`). With `--upload` it accepts writes: browser uploads (`upload.go` — direct-to-storage via presigned URLs when the backend implements `remote.PutSigner`, relayed otherwise; ops journaled under the server's own device) and the per-project `/api/p/<id>/store/*` proxy (`store.go`) that whole devices sync through — the `https://` remote backend (`remote/http.go`) is its client; journals are never presigned, only immutable blobs. Frontend is dependency-free vanilla JS embedded via `go:embed static`; it learns everything from `/api/config` (+ `/api/projects` in hub mode) and never sees storage info or credentials. It uses native History-API path routing (`/<project-id>/<path>` in hub mode, `/<path>` in volume mode, `/join/<token>` for invites — no `#`, slashes stay literal); `Server.frontend` serves `index.html` as the SPA fallback for any non-asset, non-API/auth/share route so deep links and refreshes resolve, and all client API/asset URLs are root-absolute so a deep path doesn't break relative resolution.

`cmd/bdrive/` is a thin cobra CLI over these packages (`login`, `init`, `stop`, `sync`, `status`, `log`, `remote`, `web`, `whoami`, `daemon`, `version` — `mnt`/`umnt` are gone; `init` is the front door and `stop` pauses). `bdrive login` signs the device in (bare form uses the remembered server or `config.DefaultServer` = beardrive.ai; loopback-callback browser flow in `login.go`, `--device` for headless) and stores server+token+account in `settings.json`. `bdrive init` is interactive on a TTY (survey menus: create-new vs connect-existing with a project list; whole-folder vs `--shared <dir>`, which becomes the include list) with full flag bypass (`--name/--project/--shared/--yes`) and never prompts without a TTY; it runs the login flow first when there is no session, writes `.bdrive/config.json`, seeds `.bdriveignore`, and starts sync via `startSync`; re-running it resumes — including after a folder move. `bdrive web -c config.json` configures the server from a file, explicit flags winning.

Authentication (`webapp/auth.go`, `authlocal.go`, `mail.go`) is **mandatory in hub mode** — the config's `auth` block tunes `users_db`/`allow_signup`/`allowed_domains`/`require_verification`/`require_approval`/`admins`/`smtp`; the plain-folder viewer stays auth-free — and sits behind the `AuthProvider` interface — the OSS server ships only `BuiltinAuth` (email+password accounts and device tokens in a file-backed `auth.json`; bcrypt for passwords, SHA-256 digests for tokens, plaintext never stored; server-owned `/auth/*` pages; one-time codes for the CLI callback and device flows; SMTP reset mail with a log-link fallback). **Signup is invite-only by default** (`allow_signup` defaults false): a valid org invite bootstraps an account even when self-signup is closed — `BuiltinAuth.InviteValid` (wired to `OrgDB.ValidInvite`) lets `pageSignup`/`pageLogin` offer account creation for a `/join/<token>` target, and `signupInvited` skips the domain/verification/approval gates and activates immediately (the invite is the vetting). `BuiltinAuth.ValidateSignupPolicy` (called at hub startup, `web.go`) refuses an ungated open hub and email-verification-without-SMTP rather than silently leaving the door open. The three postures: invite-only (default), approval-gated (`require_approval`), and domain-restricted+verified (`allowed_domains`+`require_verification`+`smtp`); `allow_signup`/`allowed_domains`/`admins` stay server-config-owned so a browser session can't widen access. A managed deployment can swap in a different provider (e.g. PropelAuth) without touching the CLI or API — keep provider-specific code out of this repo. The sync client picks up its token from `BDRIVE_TOKEN` or `settings.json` and sends `X-Bdrive-Device{,-Name,-Os}` headers (`remote/http.go`); the hub's file-backed device registry (`webapp/devices.go`) records per-device name/OS/account/server-observed IP. Journal ops carry the signed-in account (`Op.User`/`UserName` from `Session.Account`; `Author` remains the git/OS fallback). History (`webapp/history.go`): `GET /api/p/<id>/history?path=|prefix=` (newest first, device-registry join) and `GET /api/p/<id>/blob?sha=` stream any exact version — blobs are retained forever, so the future revert phase is just re-putting an old blob as a new op. Share links (`webapp/shares.go`, file-backed `shares.json`): any signed-in member mints `/s/<token>` public URLs (`bdrive share`, or the UI's Share button) serving the file's LATEST content until revoked (optional expiry); `/s/*` responses are sandboxed (CSP `sandbox allow-scripts`, no auth cookies) so shared HTML can't attack hub sessions — keep that header on any change; `/s/*` also sits behind a per-IP token bucket (`ratelimit.go`, `share_rpm` config), and markdown share pages get a "Shared with BearDrive" footer (raw HTML is never injected into).

## Invariants — do not break these

- **Each device writes only its own journal.** This is the whole concurrency story: no locking service is needed because no object ever has two writers. Never write to another device's journal file or remote key.
- **Blobs are pushed before the journal** (`syncer.push`), so a peer never sees an op whose content is missing. Preserve this ordering.
- **Scan happens before pull** in `Cycle`, so local edits are journaled (and content captured) before remote state can overwrite the working folder.
- **Replay must stay deterministic.** Any change to `journal.Less` or `Replay` changes what every device converges to.
- **Materialize never clobbers dirty files**: a file whose size/mtime differs from the state cache changed mid-cycle and is left for the next scan.
- **All state files are written atomically** (temp file + rename, see `store.WriteFileAtomic`). Temp files are prefixed `.bdrive-tmp-` and ignored by the scanner.
- **`Cycle` runs under the volume flock** — the daemon and one-shot CLI commands (`bdrive sync`) coexist through it.
- Errors during pull/push degrade to `Result.Offline` rather than failing the cycle; unreadable/vanished files during scan are skipped and retried next cycle. Follow this "never break sync, retry next cycle" posture.

## Testing conventions

The real coverage is the integration tests in `internal/syncer/syncer_test.go`: each test builds multiple simulated devices (`newDevice`) syncing through a shared `file://` remote (`sharedRemote`), then drives explicit `cycle()` calls to test convergence, offline operation, and concurrent-edit conflicts. Extend these when touching sync behavior — a new sync feature without a multi-device test is untested where it matters.

## Claude Code plugin

`plugin/` is a Claude Code plugin (skill + `/beardrive:install` + `/beardrive:init` + `/beardrive:status` commands + turn-boundary sync hooks). `/beardrive:install` (`plugin/commands/install.md`) is the team onboarding flow: binary, login, init, a consent-gated CLAUDE.md section about the shared folder, and project-level hooks in `.claude/settings.json` (blocking pull at UserPromptSubmit, async push on PostToolUse Write/Edit) so teammates without the plugin still sync, published via the marketplace manifest at `.claude-plugin/marketplace.json` (`/plugin marketplace add runbear-io/beardrive`). The canonical skill lives at `plugin/skills/beardrive/SKILL.md`; `.claude/skills/beardrive` is a symlink to it. The hook script `plugin/scripts/beardrive-sync.sh` (and the inline project-level hook commands) must stay a fast no-op for folders without a `.bdrive/` dir — it runs on every turn in every project.

## Docs to keep in sync

- `README.md` and `plugin/skills/beardrive/SKILL.md` both document CLI behavior, flags, output formats, and the on-disk layout. When changing CLI commands, flags, output, or layout, update both — the skill is what makes Claude Code beardrive-aware for end users and must match the actual binary.
