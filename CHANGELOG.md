# Changelog

Notable changes per release. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); BearDrive is pre-1.0, so
minor versions may ship breaking changes (see [SemVer §4](https://semver.org/#spec-item-4)).

## Unreleased

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
