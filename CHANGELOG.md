# Changelog

Notable changes per release. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); BearDrive is pre-1.0, so
minor versions may ship breaking changes (see [SemVer §4](https://semver.org/#spec-item-4)).
## Unreleased

## v0.18.6 — 2026-11-10

**Update your devices** for incremental journal append/tail sync, lazy blob
downloads, Copy-on-Write local blob storage, and automatic local blob garbage
collection; running daemons switch over the first time the updated `bdrive`
runs. **Update self-hosted hubs** for `POST /api/p/<id>/store/append`,
prefix-verified journal tail reads, chunked-file streaming without duplicate
whole-blob backfill, and net-delta journal quota accounting. Older clients and
hubs keep syncing through the existing full-journal `PUT`/`GET` fallback; update
both sides before relying on chunked sync for files larger than 4 MiB.

- **Incremental journal append and prefix-verified tail sync.** Devices push only newly committed journal lines to `POST /api/p/<id>/store/append` (with `prev_size` and `prev_sha256` verification, torn-tail newline repair, strict JSONL and monotonic sequence validation, and conditional storage CAS) and pull only appended bytes via `GET /api/p/<id>/store/object?offset=&prefix_sha256=` (`X-Bdrive-Tail-Offset`), falling back to full-object `PUT`/`GET` against older peers or on prefix mismatch. The S3 backend now implements conditional writes (`If-Match` / `If-None-Match`) alongside GCS and `file://`. (#346)
- **Lazy blob pulls, Copy-on-Write local storage, and local orphan-blob garbage collection.** Sync cycles download only blobs needed for the current live file tree, concurrent conflict copies, and 3-way merge bases instead of every historical version ever written to the project. On macOS (APFS `clonefile`) and Linux (`FICLONE`), local blob capture and materialization use Copy-on-Write clones with `0400` read-only blob permissions, and the daemon periodically prunes unreferenced local blobs while preserving all blobs referenced by active worktrees, local journals, and state caches. (#346)
- **Base-aware 3-way merge and chunked-file deduplication on the hub.** Local edits record the prior blob digest in `Op.Base` (`base,omitempty`) so concurrent edits merge against their true common ancestor, with line-boundary-safe merging for `.json` and `.jsonl` files. On the hub, reading a chunked file (`manifests/<sha>`) streams reassembled chunks through a reference-counted spool without writing a second full `blobs/<sha>` copy back to object storage, and quota accounting bills only net journal growth and once per manifest. (#346)
- **Cross-journal and co-editing concurrency hardening.** Browser writes (`upload`, `restore`, `remove`, `undorun`) serialize journal updates under `rs.upmu` and re-verify cross-journal state and target base conditions (existence, absence, and symlink targets) before committing. Shared journal fetches and chunk reassembly detach from individual request cancellation, and co-editing room shutdown snapshots retain conditional CAS on `ydoc/head/<path>`. (#346)

## v0.18.5 — 2026-11-09

**Update your devices** for the faster agent hooks; running daemons switch over
the first time the updated `bdrive` runs. **Update self-hosted hubs** for the
co-editing fixes; on-disk and storage formats are unchanged.

- **Agent hooks no longer walk the whole tree when the daemon is current.** The turn-start hook (`bdrive sync --hook`) skips its sync cycle when the mount's daemon vouches that it has pulled everything the hub announced: it reads what the daemon already recorded (inbound changes, secret findings) and saves the session note. On a 78,685-file tree, hook latency went from `644 ms` to `16 ms` at p50 and from `715 ms` to `18 ms` at p95. The post-edit hook now runs `bdrive sync --defer`, which only saves the note under the same condition. The daemon writes `<volume>/pulled` after a tick that pulled every mount and renews it while the hub's change stream is alive; a peer announcement, a failed pass, the stream ending, or the worker exiting withdraws it. Without a daemon, or beside one that cannot vouch, both hooks run a full cycle as before. Existing installs pick up the post-edit hook change on the next `bdrive init` or `bdrive hooks install`. (#343)
- **Inbound changes are no longer dropped when the daemon appends while a hook drains them.** Appends and the drain of the inbound spool share a lock, so an event written during a drain is kept for the next one. (#343)
- **The hub no longer empties files or forks conflict copies of co-edited files.** A co-editing room that could not read its file opened as if the file were new and could write empty text past the stale-write and shrink guards; a read error now refuses the room, and every room write is conditional. A refused snapshot merges line by line against the outside write instead of always parking a conflict copy, and parks one only when both sides changed the same lines. In the sync engine, an empty version never forks and never keeps a path. (#339)
- **An agent appending to a file that people are editing in the browser keeps both appends.** The room writes the outside version followed by its own new lines instead of forking a copy, and a room that parks a copy now takes the outside version first, so its next snapshot no longer overwrites the outside write. (#344)
- **Rejoining a co-edited file after such a merge no longer duplicates lines.** The room stores the document state that spells the file it wrote, so the next room starts from it. One room's snapshots now run one at a time, and a final snapshot that fails during shutdown is retried without blocking shutdown. (#345)

## v0.18.4 — 2026-11-08

**Update your devices.** Running sync daemons switch over automatically the first
time the updated `bdrive` runs (`bdrive resume`, `bdrive sync`,
`bdrive status <folder>`, or an agent turn). Update self-hosted hubs if you view
synced HTML pages with relative assets.

### Measured Resource Savings

Measured on a project volume with `12,952`–`13,036` directories, `77,200`–`77,679`
files, and `349,938` journal operations across `49`–`52` device journals
(`v0.18.3` vs `v0.18.4` incorporating #340 and #341):

| Environment | Metric | `v0.18.3` | `v0.18.4` | Change |
|---|---|---|---|---|
| **macOS** (`FSEvents` vs `kqueue`, `12,952` dirs / `77,278` files) | Open file descriptors (`lsof`) | `90,647` (`O_EVTONLY` per dir & file, `65.6%` of `kern.maxfilesperproc`, `34.4%` of `kern.maxvnodes`) | **`25`** (`1` `FSEvents` stream, `0` watched file/dir FDs) | **-99.97% (`-90,622` FDs)** |
| **macOS** (`bdrive daemon`) | Process memory (RSS) & idle CPU | `1,045–1,499 MB` RSS, `+3.2–4.0 CPU s` spike every 60s | **`187 MB` steady-state RSS** (`45 MB` at startup; `311–539 MB` after #340 on `kqueue`), **`0.00%` idle CPU** | **-82–88% steady RSS (`-95%` at startup), 0 idle spikes** |
| **Linux containers** (`fanotify` vs `inotify`, `12,960–13,036` dirs, `max_user_watches=12,288`) | Watcher slots & kernel slab | `12,288 / 12,288` `inotify` watches exhausted (`ENOSPC` → `0` watches, 3s full-tree `lstat` polling) | **`0` `inotify` watches**, **`12,960–13,036` `fanotify` dir marks** (`1` FD, `87.9 B/dir` = `~1.14 MB` `slab_unreclaimable`) | **100% `inotify` headroom, 0 `ENOSPC`** |
| **Linux containers** (`bdrive daemon`) | Process memory (RSS) & idle CPU | `597–823 MB` RSS, `48.6–63.0%` avg CPU (`100%+` single-core spikes) | **`191–399 MB` RSS** (`280–291 MB` under fallback polling), **`0.00–0.03%` idle CPU** | **-33–77% RSS, ~100% idle CPU reduction** |

### Changes

- **Native macOS `FSEvents` and Linux unprivileged `fanotify` local watchers.** On macOS (`darwin/arm64`, `darwin/amd64`), the daemon now watches each mounted root through a single recursive `FSEvents` stream (`CGO_ENABLED=0`) instead of opening a `kqueue` file descriptor for every directory and file, falling back to `fsnotify`/`kqueue` if `FSEvents` initialization fails. On Linux (`5.17+`), the daemon now prefers unprivileged `fanotify` (`FAN_CLASS_NOTIF | FAN_REPORT_DFID_NAME` with `name_to_handle_at(..., AT_HANDLE_FID)`) on directories before falling back to `inotify` (e.g., on older kernels, filesystems that do not export decodable file handles, or when `fanotify` is blocked), avoiding `fs.inotify.max_user_watches` exhaustion on shared container hosts while keeping `.git`, `.bdrive`, nested mounts, and `.bdriveignore` directories pruned. (#341)
- **In-memory journal, fold, and state caching with adaptive fallback polling.** `store.Store` caches parsed device journals in memory (parsing only newly appended byte ranges), persists a fold sidecar (`fold-cache/<device>.fold`) validated against each journal file's `(ino, size, mtime_ns)` and raw tail bytes (or full SHA-256 when recently modified), and caches `state-<mount>.json` keyed by `(size, mtime_ns)`. `syncer.pull` skips full-file re-reads when a peer journal only appended bytes, and `syncer.scan` skips loading the full journal fold when a targeted local scan finds zero dirty files. When a local filesystem watcher is active, the full-scan safety interval increases from `60s` to `5m` (so an unwatched external mutation can wait up to `5m`); when the watcher is unavailable, fallback polling backs off to `15×` the last full-scan duration (floored at `--scan-interval`, `3s` by default, and capped at `60s`) while keeping remote polling on `--remote-interval`. (#340)
- **Synced HTML pages show their relative images again.** A page's `src="shot.png"` resolved against `/api/p/<id>/` instead of the page's folder, and the sandboxed frame sends no session cookie for it anyway, so every relative image broke. The frame now loads from a path-shaped URL (`/api/p/<id>/raw/<token>/<path>`) carrying a short-lived (4-hour), folder-scoped read credential minted at `/api/p/<id>/frame`. The hub stores its signing key at `hub/frame-key` in the storage root on first use when the storage backend supports atomic create (`PutIf` / `PutIfAbsent`); backends without atomic create keep using `/file`. (#338)
## v0.18.3 — 2026-11-05

**Update your devices.** Everything here is in the client and sync daemon; the
hub and on-disk formats are unchanged, and running daemons switch over the first
time the updated `bdrive` runs.

- **Large-folder local syncs no longer stall behind full-volume ignore and journal passes.** `.bdriveignore` matching uses non-backtracking regexes with literal and segment prefilters, `backfillOps` and `materialize` skip ignore evaluation for regular files whose cached state already matches the journal target, and each cycle reuses its loaded journal fold when peer journals on disk have not changed instead of re-reading and re-sorting all device journals up to three times. (#336)
- **Local watcher pruning, echo suppression, and transient error retries.** Local watcher setup and runtime directory creation skip `.git`, `.bdrive`, nested mounts, and `.bdriveignore`-pruned directories. Echo suppression evaluates candidates outside `pendingMu` and skips loading the state cache when no pre-fence events are pending, `EMFILE`/`ENFILE` watcher rebuilds release the prior watcher before retrying, and non-`ENOENT` file or directory read failures during targeted scans queue a bounded single retry while preserving cached children against spurious deletes. (#336)

## v0.18.2 — 2026-11-03

Update your devices for targeted watcher scans, and update self-hosted hubs for
the co-editing and MCP changes. A single-process hub needs no Redis setting.
For co-editing across processes, configure `collab.redis` and use shared project
storage that supports conditional writes.

- **Co-editing across hub processes.** An optional Redis Streams relay carries
  document updates between processes, while a storage lease chooses the one
  that writes each room's snapshots. Processes share the document's seed and
  rebuild from its newest stored history, avoiding duplicate seeding and replay
  of edits that an outside revert removed. Without the relay, a second process
  refuses a room another holds so the browser can retry on the holder. (#325)
- **Conditional writes for local hub storage.** The `file://` backend now
  supports compare-and-swap under a filesystem lock, used by leases, room
  history, and the hub's existing conditional journal-append path. This relies
  on local-filesystem locking, not shared network-filesystem guarantees. (#325)
- **Snapshot live co-editing rooms on graceful shutdown.** `bdrive serve`
  attempts to snapshot live rooms on SIGTERM or SIGINT before HTTP shutdown.
  The shutdown context now cancels local file-lock waits; storage backends
  outside that local path and a hard kill can still interrupt saving. (#325)
- **MCP tool titles and safety hints.** Every tool has a human-readable title
  and closed-world annotations. Read tools declare read-only and idempotent
  behavior; tools that change files declare destructive behavior. These are
  hints for clients, not a replacement for permission checks. (#330)
- **Local watcher wakes scan only affected paths.** Materialization echoes no longer trigger extra full-tree scans, and transient macOS watcher `ENOENT` races recover without stopping the watcher. Full scans remain the safety path for startup, policy changes, uncertain batches, and the one-minute deadline. (#328)
- **Malformed collab Redis settings no longer expose credentials.** Parse
  errors omit the URL and input values. BearDrive still refuses to start with
  an invalid setting; the error is generic, so the parser's detailed diagnosis
  is no longer available.
- **Shutdowns no longer hang on a busy local file remote.** A canceled file CAS lock wait returns without disturbing the other writer, while normal graceful shutdowns still save live co-editing changes.
- **Website and setup references are easier to find.** The README links
  directly to the website and documentation, and the CLI and hub configuration
  references explain targeted scans and the optional Redis relay. (#327)

## v0.18.1 — 2026-10-27

The entries below preserve two fixes already included in v0.18.1. See the
[original release](https://github.com/runbear-io/beardrive/releases/tag/v0.18.1)
for the full commit list.

- **Local watcher recovery releases resources.** Recreated directories are re-registered, and macOS watcher shutdown no longer retains file descriptors.
- **Share links no longer run out because your team synced.** The hub reported every byte a device pulled through `/store/*` to `QuotaProvider.RecordEgress`, the counter a managed hub enforces its monthly share-link transfer cap against, so ordinary syncing used up the allowance and every `/s/*` link in the org answered "This link has exceeded its transfer limit". `RecordEgress` now reports share-link transfers only, and sync traffic goes to `RecordSyncEgress` on a provider that implements the new optional `SyncEgressRecorder` — measured, never enforced. Existing providers keep compiling and simply aren't told.


## v0.18.0 — 2026-10-15

**Update your devices.** Everything here is in the client; the hub is
unchanged, and older clients keep syncing with it. Running sync daemons switch
over to the new supervisor the first time the updated `bdrive` runs on a
machine (`bdrive init`, `resume`, `sync`, `status <folder>`, or an agent turn), with
nothing to re-init.

- **One project, several folders on the same device.** Git worktrees and extra
  checkouts of a project can now all be mounted at once. They share one local
  volume store and one device journal (`volume_id` in `mounts.json`), while
  each folder keeps its own materialization cache (`state-<mount-id>.json`),
  pause state (`paused-<mount-id>`), session note (`note-<mount-id>.json`),
  secret warnings (`secrets-<mount-id>.json`), and agent-hook inbound list
  (`inbound-<mount-id>.jsonl`). `bdrive init` used to refuse a second folder,
  and deleting a worktree before running `init` in a fresh one started the
  device's journal over and hit `409 Conflict` on every push. A folder joining
  a project already synced on this device reuses the shared local volume's
  `.bdriveignore` instead of overwriting it with the starter template. (#278)
- **One background sync supervisor per user session instead of one process per
  project.** A single `bdrive daemon run` supervisor (`~/.bdrive/daemon.lock`)
  runs one worker goroutine per active project volume
  (`~/.bdrive/volumes/<volume-id>/daemon.lock`) and shares HTTP/2 hub
  connections across projects. After a binary upgrade, the next `bdrive`
  command or agent turn replaces the running supervisor automatically.
  `bdrive stop` pauses only the selected folder while sibling worktrees and
  other projects keep syncing; `bdrive stop --all` pauses every project on the
  device. (#281, #282)
- **Pulling a large batch of changes downloads in parallel.** Missing blobs on
  pull now download with up to 16 concurrent requests instead of one by one,
  matching push concurrency. (#280)

## v0.17.1 — 2026-10-14

A hub-only fix; clients are unchanged. Live on the managed hub already.

- **The web app's first screen no longer stalls on a busy hub.** Every
  request now authenticates once. The project list used to re-authenticate
  once per project on the whole hub — other organizations' projects
  included — so on a hub whose auth provider looks sessions up in a database,
  opening the app could wait 20+ seconds on `/api/projects` and slow every
  other request sharing that database. (#275)

## v0.17.0 — 2026-10-14

**Update your devices to get `bdrive comments` and symlink sync.** The hub
side of this release is already live on the managed hub. Older clients keep
syncing: they never see comments, and they skip a synced symlink rather than
materializing it.

- **Review comments.** Anyone who can read a file on the hub can leave
  feedback on it while reading, the way a Vercel preview toolbar works. Select
  text in rendered markdown, text or HTML, or pick any element on an HTML
  page. Comments show as highlights with numbered pins and as threads in a
  side panel. An agent sweeps the open ones with the new `bdrive comments`
  command: each thread shows the quoted text and its line in the local copy
  of the file. Then `bdrive comments resolve <id> -m "what changed"`;
  `reply` and `reopen` too, and `--json` for scripts. Comments follow their
  file across renames, are hidden with it by folder rules, and count toward
  the org's storage. (#276)
- **Symlinks sync as links.** A symbolic link in a mount used to vanish
  without a word. It now syncs as a link carrying its target string — never
  the target's bytes — and the hub lists it with its target, never serving
  content through it. (#273)
- **MCP agents see the project's `AGENTS.md` / `CLAUDE.md`**, marked as
  teammate-written, and every single-file tool names the guides that apply
  to its path. (#274)

Web app, live on the managed hub already:

- A light theme, with a System / Light / Dark switch in the profile menu;
  System is the default. (#272)
- Drag the reading column wider or narrower. (#266)
- Browser tabs are named after the document, not its folder path. (#268)
- Download and export buttons inside a synced HTML page work, in the viewer
  and on share links. (#277)
- Reconnecting to a co-editing room no longer duplicates the file's
  content. (#267)

## v0.16.1 — 2026-09-25

**Update your devices.** The fix that matters here is in the client, and a
hub cannot apply it on a device's behalf.

- **A device no longer keeps a partial copy of a project forever.** A blob
  whose download failed in the cycle that pulled its op — a dropped
  connection, a hub shedding load, a `bdrive init` interrupted mid-download —
  was never requested again, because the peer journal naming it was already
  on disk and yielded nothing on later pulls. The path stayed unwritten while
  `bdrive status` reported a healthy mount, and a device joining a large
  project could hold a small fraction of its files indefinitely. Each remote
  pass now backfills content the merged state names but the store lacks,
  in batches bounded by count and bytes; while a backfill is landing
  content the daemon runs its remote passes back to back instead of waiting
  out the five-minute watched cadence. Devices already in this state heal
  on their own after upgrading — no re-init. (#264)

Hub and web editor fixes, live on the managed hub already:

- Co-editing: while a room is live the hub is the only writer of the file,
  so co-editors no longer race each other into conflict copies (#257, #258,
  #260), and two files open in one browser no longer mix (#259).
- The hub bounds its Postgres pool, and a transient 401 no longer turns into
  an endless login redirect loop (#256).
- Multi-process hubs: journal appends are compare-and-swap (#254), and
  `bdrive login` survives landing on a different hub process (#251, #252).

## v0.16.0 — 2026-09-22

**Deploy hubs before clients.** A hub older than this release refuses the
chunk keys a new client pushes for large files (sync degrades to
offline-retry until the hub upgrades; nothing is lost). And a *client* older
than this release cannot pull files between 32 and 100 MiB — it caps reads
at its old 32 MiB ceiling, fails the content check, and reports "blob
corrupt on remote" every cycle even though the hub is healthy. Upgrade the
hub first, then clients promptly if your projects hold large files.

- **Delta sync** — files larger than 4 MiB move as content-defined chunks:
  a small edit to a 20 MiB file now transfers ~2 MB instead of ~21 MB, in
  both directions. Chunk boundaries come from a rolling hash, so insertions
  anywhere in the file stay cheap. Local storage keeps files whole; the hub
  reassembles whole blobs on demand, so older clients and every hub read
  surface (viewer, history, shares, downloads) work unchanged.
- The per-file sync ceiling rises from 32 MiB to 100 MiB — files up to
  100 MiB now materialize on every device.
- `bdrive import` refuses an archive whose journals reference content the
  archive does not hold (what an old `bdrive export` produces against a
  newer hub, silently missing large files); `--allow-incomplete` overrides.
- Hub hardening around the new key classes: manifests are write-once, must
  name only chunks the store holds, and reassembly is bounded (256 MiB) —
  a project member cannot poison, re-point, or amplify large-file storage.

**Update your devices to get the sync traffic fix.** The largest change here
is a change to the *client*: a daemon holding a healthy change stream now
polls the hub every five minutes instead of every ten seconds. The hub cannot
lower a cadence a client decides, so an un-upgraded device keeps costing what
it always did — about 17,000 requests a day while doing nothing at all.

- **An idle device stops shouting at the hub.** The daemon already held a
  live change stream that announces every peer write, and polled at full rate
  beside it anyway. With the stream healthy the remote cadence drops 30x; it
  snaps back to the old one the instant the stream dies, for any reason, so a
  hub that cannot stream syncs exactly as before. Measured against
  production: one idle device on one project was 9.4% of the entire hub&apos;s
  daily traffic.
- **An idle browser tab stops announcing itself.** Presence was a 10-second
  heartbeat per TAB, ~19% of everything the hub served, sent by people who
  were reading. A member holding an open change stream is present by
  definition, so the hub keys presence to that connection and the tab speaks
  only when it arrives, navigates or leaves.
- **Co-editing writes the file once per pause, not once per editor.** Every
  client&apos;s save timer was rearmed by a peer&apos;s keystroke, so four people in
  one document wrote four identical copies and grew four History versions
  every time the room went quiet.
- **A permission change reaches devices in under a second** (it could take up
  to the poll interval), pushed on the stream that is already open. The frame
  deliberately names no prefix: a rule that hides a folder must not leak its
  name to the people it hides it from.
- **Fixes a hub that minted a new identity on every restart.** A hub whose
  state directory does not survive a restart — a container, anything on a
  tmpfs — was writing its own journal under a fresh random device id each
  time, leaving a permanent file per boot in every project it touched. Hubs
  with a persistent home are unaffected and keep the id they have.
- **An MCP connection revoked elsewhere now stops working immediately**
  rather than at the next hub restart, and an MCP consent can be completed by
  a hub process other than the one that started it.

## v0.15.0 — 2026-08-11

**Upgrade if your pushes are being refused.** Hubs running the journal
ownership gate require a device id to be bound to its account, and that
binding is made by the login request naming its device — something a CLI
older than this release does not do. The symptom is a sync that pulls
normally while every push 403s with "this device is not registered to your
account on this hub", surviving any number of re-logins. Update, then run
`bdrive login` on that machine; pending local changes are journaled and go
out on the next cycle (#112, #146).

- **`bdrive grep`** — search the text inside the files a project syncs,
  without materializing them (BEA-99).
- **Agents see what changed before they overwrite it**: the sync hook
  reports teammates' changes into the session (BEA-127), and the hub tracks
  what each agent session *read*, not just what it changed (BEA-98).
- **Hub rendering**: mermaid fences render as diagrams in the viewer and on
  share pages (BEA-91); `.csv`/`.tsv` render as a table instead of a wall
  of monospace (BEA-74).
- **Sharing is harder to do by accident**: the hub refuses to share a file
  that looks like it holds credentials (BEA-111), share links follow a
  moved file and old URLs redirect (BEA-81), and a link reports how many
  times it has been opened (BEA-76).
- **Honest degraded sync** — a refused push keeps its verdict instead of
  being cleared by the daemon's local-only ticks, and `bdrive status`,
  `bdrive sync` and the daemon log print the hub's own reason for the
  refusal rather than a generic "read-only" (BEA-403, #146).
- **Restore asks before it syncs to every device** (BEA-129); history stops
  re-downloading every journal on each view (BEA-85); `/history?path=` shows
  that file rather than the whole project (BEA-64).
- **Agent hooks**: agent skills sync, and `~/.claude` is refused as a mount
  root (BEA-117); macOS asks before it pops "Background Items Added" at
  init (#139).
- 318 security hardening fixes across hub, sync, CLI and SPA (#112), and
  launch pricing guardrails — egress caps, ignore defaults, storage
  tiering (#114).

<!-- v0.12.0–v0.14.0 shipped without changelog entries. -->

## v0.11.0 — 2026-07-27

- **`bdrive forget` + `bdrive sync --prune`** — take an already-synced path
  off the hub after ignoring it, without deleting anyone's local files
  (BEA-20).
- **Per-project permissions** — none/read/write/admin per member,
  invite-only projects, and honest degraded sync when a device's access
  shrinks (#46).
- **File version history grew up**: a history row opens the exact version
  it describes (`?v=<sha>` deep links, BEA-7), the hub shows what changed
  between versions (BEA-10), the feed sorts by wall-clock time instead of
  Lamport clock (BEA-9), and the kind marker is a text badge instead of a
  fake disclosure toggle (BEA-17).
- **Hub UX**: the project Dashboard opens to every member and `/insights`
  is now `/dashboard` (old URLs redirect, BEA-12); a file's public share
  links are managed from the file itself and from project settings, not
  the org panel (BEA-16); read counts say what they're made of and the
  visit debounce is pinned (BEA-15); folder rows keep their metadata on
  phones and the heat dot has a name (BEA-14).
- **Sync scope**: multi-folder `--shared` at init plus `bdrive scope` to
  edit the sync scope later (#53); `.bdriveignore` itself always syncs
  (#54); `--shared` include entries anchor to the mount root instead of
  matching same-named nested dirs (BEA-5).
- **Agent-first onboarding**: `INSTALL_FOR_AGENTS.md` is one paste-able
  URL that onboards any agent (#57), and a hub with no projects shows the
  same two-line agent prompt (#60).
- Project rename, description and icon from its Settings page (#51); the
  sidebar always brands as BearDrive, never the storage name (#52);
  Billing entry in the account menu for managed hubs (#55).
- CLI copy no longer points at a hub device list that doesn't exist
  (BEA-13).
- Claude Code plugin **0.4.0**.

## v0.10.0 — 2026-07-26

(v0.9.0 was tagged 2026-07-25 without a changelog section; its items are
folded in here.)

- **`bdrive export` / `bdrive import`** — move projects between hubs with
  full change history.
- **Onboarding fixes**: headless login falls back to the device-code flow
  instead of hanging on a browser that never opens; `bdrive --version`
  works; `bdrive init` prints next steps; changes are attributed to the
  signed-in account with labeled authorship. Follow-ups: init next-steps
  gated on background mode, daemon reconnects on token change, `whoami`
  surfaces settings errors.
- `bdrive version` now reports the real version for
  `go install …@vX.Y.Z` builds (previously always `0.1.0-dev`).
- Web: delete a project from its Settings page, behind type-the-name.
- **Fix: agent hooks no longer sync (or inject hub links into) folders this
  device never opted into.** `bdrive sync`/`sync --hook`/`read-log` now
  require the mount to be enrolled here — a `.bdrive/config.json` that
  merely arrived with a folder (git clone, copied dir) is inert until
  `bdrive init`; previously one hook firing silently minted a device
  identity, registered the mount, and journaled the whole folder. And
  `bdrive stop` now truly pauses: it sets a per-device paused marker that
  gates the hooks and `bdrive sync` (which previously resumed a stopped
  project every agent turn and re-registered even after `stop --forget`);
  only `bdrive init` resumes.
- **`bdrive skill install`** — the binary now carries the `beardrive`
  skill and installs it into any agent that reads `SKILL.md`
  (`~/.claude|.codex|.gemini|.hermes/skills/beardrive/`), idempotently;
  bare `bdrive skill` prints the detection table.
- **Hub install guide, Codex and Hermes tabs: one paste, no terminal** —
  the same shape as the Claude tab. The pasted prompt has the agent install
  the CLI, keep the skill, sign in (`login --device`, so it can relay the
  code instead of hoping a browser opened), `bdrive init`, and
  `bdrive hooks install` — the step hand-copied setups routinely skipped.
  The plain commands moved into an "or run it yourself" fallback.

## v0.8.0 — 2026-07-16

- **Gated links on every mentioned file path**: Claude Code's turn-start
  hook (`bdrive sync --hook`) now injects the project's link formula each
  turn, so agents append `` `path` `` [🔗](hub link) to any synced path
  they mention — sign-in + membership required, safe to paste internally.
  Works even with stale skill copies; other platforms get the same
  convention via the skill. Plugin 0.3.0.
- `bdrive hooks install` now converges its managed hook groups to the
  current shape on reinstall — improvements reach existing projects
  instead of being frozen by the idempotency marker.

## v0.7.1 — 2026-07-15

- **Markdown frontmatter renders as a key/value table** in the viewer and
  on public share pages (author key order, escaped, strict fallthrough
  for anything that isn't a well-formed YAML mapping).
- Landing-page copy: the Claude chat mockup demonstrates the gated team
  link, matching the shipped default.

## v0.7.0 — 2026-07-14

- **`bdrive url <file>`** — internal, permission-walled links (sign-in +
  project membership required) that agents share when they create files;
  the plugin now teaches agents to include the link in their reply.
- Mobile layout overhaul: responsive chrome now covers tablet and phone
  landscape, 44px touch targets throughout, five designer-review rounds.
- Read-heat hooks re-registered on upgrade pick up broader matchers.

## v0.6.0 — 2026-07-13

- **Web UI rewritten in React + TypeScript** (same URLs, same design):
  committed build output keeps `go build`/`go install` Node-free.
- **Read-heat coverage fix**: agent reads via shell commands (`cat`,
  `grep`, `tail`) and Grep matches now count, not just native file reads;
  `bdrive hooks install` upgrades existing hook matchers in place.
- Content-hashed assets served immutable; committed e2e harness +
  42-spec Playwright suite.

## v0.5.0 — 2026-07-12

- **Project home page**: connect-an-agent guide (Claude Code & Cowork
  plugin flow, Hermes/Codex CLI) with real hub URL + project id filled
  in; Insights embedded for admins/org owners.
- Two-file AGENTS.md orientation for shared folders in the plugin flows.
- Expandable history notes; RESTful `/insights` and `/history` routes.

## v0.4.0 — 2026-07-12

- **Read heat / Insights**: per-file read telemetry (human vs agent vs
  share), heat dots in listings, and the Insights dashboard — treemap,
  reads×staleness scatter with the hot-but-stale danger quadrant, hot
  path, per-agent coverage matrix.
- **Agent read reporting**: `bdrive read-log` + hooks spool agent file
  reads locally and report on next sync.

## v0.3.1 — 2026-07-10

- Parallel blob upload + progress bar for large initial imports.

## v0.3.0 — 2026-07-10

- **Hub-only architecture**: clients sync exclusively through a
  `bdrive web` hub over HTTPS (the `remote` command and direct
  client-to-bucket sync were removed); `bdrive logout` added.
- **SQL metadata backends**: hub accounts/projects/orgs/shares can live
  in SQLite or Postgres (incl. Supabase) instead of JSON files.
- Dockerfile + Cloud Run deployment recipe.

## v0.2.2 — 2026-07-08

- **BearDrive**: the project (formerly `sfs`) got its name; CLI became
  `bdrive`.
- Multi-project sync hub with accounts and orgs; interactive
  `bdrive init` / browser `bdrive login` onboarding; public share links;
  web viewer folded into the CLI as `bdrive web`; per-file history in
  the web UI; `/beardrive:install` team onboarding for Claude Code.

## v0.1.0 — 2026-06-12

- First release: per-device append-only journals, last-writer-wins
  replay, content-addressed blobs, offline-first sync through S3/GCS/
  file remotes, conflict copies, daemon with turn-boundary agent hooks.
