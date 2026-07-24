# Changelog

Notable changes per release. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); BearDrive is pre-1.0, so
minor versions may ship breaking changes (see [SemVer §4](https://semver.org/#spec-item-4)).

## Unreleased

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
