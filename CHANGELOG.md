# Changelog

Notable changes per release. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); BearDrive is pre-1.0, so
minor versions may ship breaking changes (see [SemVer §4](https://semver.org/#spec-item-4)).

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
