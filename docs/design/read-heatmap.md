# Read heatmap — design

Status: implemented (2026-07-11, all three phases) · Owner: snow · Prior art: session-linked notes (shipped), history API

## Problem

BearDrive knows everything about *writes* (journals: who, when, which session)
and nothing about *reads*. Admins curating a shared knowledge folder can't see
which files the team actually consumes, so they can't tell a load-bearing
document from dead weight. The killer view is the **read×write matrix**:
heavily-read + long-unwritten is the danger zone (stale knowledge people still
rely on); unread + unwritten is archive material.

Two properties make this more than Confluence-style view counts:

1. **Write provenance is already perfect** (journals), so read data alone
   completes the matrix — competitors have reads but weak write history.
2. **Agent reads are attributable.** The agent hooks + session notes shipped in
   PR #12 mean we can distinguish *human* reads from *agent* reads — the agent
   hot-path is effectively the team's context window, and nobody else can see it.

## Non-goals

- **`/store/*` sync traffic is never a read.** Devices replicating a volume is
  replication, not consumption. Only deliberate consumption counts.
- No per-user browsing profiles in any API response. Aggregate counts only.
- No general web analytics (referrers, dwell time, scroll depth).
- Volume mode (`DirSource`, auth-free viewer) is out of scope — like history,
  this is a hub feature.

## What counts as a read

| Source | Kind | Actor (internal only) | Where recorded |
|---|---|---|---|
| Viewer `GET file` / `render` / `download` | `human` | account email | `serveBlob`, `handleRender` |
| Share link hit `GET /s/<token>` | `share` | share token | `handleShared` |
| Agent tool read (Read / read_file / …) | `agent` | device id | reported by client, phase 3 |
| History version view (`/blob`) | — not counted | | spelunking ≠ consumption |
| `/store/*` | — never | | replication |

Details:

- **Record before the ETag check.** A 304 render is still a person reading the
  file; skipping it would undercount exactly the hottest (most-cached) pages.
- **Debounce to visits, not requests.** An in-memory `(project, path, actor)`
  seen-map with a 10-minute window collapses reload storms and the
  render-then-raw double fetch into one read. The map is pruned on the flush
  tick. Embedded assets fetched during a markdown render do count as reads of
  those assets ("this diagram is viewed a lot" is signal, not noise).

## Data model

One new MetaStore repo. Rows are daily aggregation buckets, not events — the
ledger never stores an event log, so there is nothing sensitive to leak and
nothing that grows per-request.

```go
// ReadStat is one aggregation bucket: reads of one path by one actor on one
// day. Day=="" is the all-time fold (see retention). Actor is an opaque
// internal id (account email / device id / share token) used only to count
// distinct readers; it never appears in an API response.
type ReadStat struct {
    Project string    `json:"project"`
    Path    string    `json:"path"`
    Day     string    `json:"day"`  // "2026-07-11" UTC, or "" for all-time
    Kind    string    `json:"kind"` // human | agent | share
    Actor   string    `json:"actor"`
    Count   int64     `json:"count"`
    Last    time.Time `json:"last"`
}

// ReadRepo persists read buckets. Unlike the other repos this one is batch-
// oriented: reads are telemetry and flushes carry many dirty buckets at once —
// one file rewrite / one SQL transaction per flush, not per bucket.
type ReadRepo interface {
    Load() ([]ReadStat, error)
    PutBatch(stats []ReadStat) error              // upsert by (project,path,day,kind,actor)
    DeleteBatch(keys []ReadStatKey) error         // used by retention fold
}
```

- `MetaStore` gains `Reads() ReadRepo`; file backend adds `reads.json`
  (same load-all / rewrite-atomically discipline, 0o755 dir mode — no secrets),
  SQL backend adds one table with PK `(project, path, day, kind, actor)` and
  the usual idempotent `CREATE TABLE IF NOT EXISTS` migration.
- `db_conformance_test.go` gets ReadRepo cases like every other repo.

### Cardinality & retention

Worst realistic case (1k files × 30 actors × 400 days, every actor reading
every file daily) is implausible; actual rows ≈ files-actually-read × active
actors × active days. Retention keeps it bounded regardless:

- Config `reads.retention_days` (default **400** — enough for a year-over-year
  view). On the daily compaction pass (at load + once per day on the flush
  loop), buckets older than the horizon are **folded into the all-time row**
  `(project, path, "", kind, actor)` and deleted. All-time totals survive
  forever; per-day resolution ages out.

## Server plumbing

New `ReadLedger` service (`webapp/reads.go`), same shape as `DeviceRegistry`:
in-memory state over a repo, with write throttling.

```go
type ReadLedger struct {
    repo ReadRepo
    mu     sync.Mutex
    byKey  map[readKey]ReadStat   // loaded at open, bumped in memory
    dirty  map[readKey]struct{}   // flushed every 30s and on Close
    seen   map[visitKey]time.Time // debounce window
}
func (l *ReadLedger) Record(project, path, kind, actor string)
func (l *ReadLedger) Heat(project, prefix string, since time.Time) map[string]HeatEntry
func (l *ReadLedger) Close() error // final flush
```

- `Server.Reads *ReadLedger` — nil means the feature is off (mirrors
  `Devices`/`Shares`). `Record` on a nil ledger is a no-op, so call sites
  stay unconditional.
- **Recording sites**: `serveBlob` (covers `file` + `download`),
  `handleRender`, and `handleShared`. The `proj()` resolver already knows the
  project id; it stashes it in the request context so `recordRead(r, path,
  kind)` can pick it up without changing every handler signature.
- **Never on the failure path**: record only after the path resolved and
  membership passed (`proj()` runs `projectAllowed` first — a 403 records
  nothing).
- Flush errors degrade silently (log once), same "never break the request"
  posture as sync: telemetry must never 500 a page view.

## API

One endpoint, membership-gated like every per-project route:

```
GET /api/p/<id>/heat?prefix=<folder/>&days=30
→ { "since": "2026-06-11", "entries": {
      "wiki/onboarding.md": { "human": 42, "agent": 17, "share": 3,
                              "readers": 6, "last_read": "2026-07-10T…" },
      … } }
```

- `days=0` → all-time (daily buckets + the all-time fold).
- `readers` = distinct human actors in the window. Actor identities never
  leave the server; this is the only trace of them.
- **No writes-join on the server**: the frontend already has per-path last
  write time from `tree`, so the read×write matrix is a client-side join.
  One source of truth for write recency, no new endpoint.
- `/api/config` gains `"reads": {"enabled": true|false}` so the frontend knows
  whether to fetch heat at all.

## Frontend

**Phase 1 — ambient heat (all members).**
- Folder listing rows (`renderFolderListing`): a heat dot before the meta text,
  intensity 0–4 on a log scale of 30-day reads (any-kind), plus `· 42 reads`
  in `dl-meta`. Folders show the sum of their subtree.
- File view meta line gains `· 42 reads/30d`.
- One `heat?days=30` fetch per project, cached alongside the tree and
  refreshed with it.

**Phase 2 — Insights (admins + org owners).**
- An "Insights" panel per project: SVG quadrant scatter, **x = days since last
  write (log), y = reads in window (log)**, one point per file. Quadrants:
  hot+fresh (healthy), **hot+stale (danger zone — fix these first)**,
  cold+fresh (new, unproven), cold+stale (archive candidates).
- Below it, the danger-zone list ranked by `reads × staleness`, each row
  linking into the file with its history.
- A human/agent toggle (or stacked dot colors) — "what do the agents live on"
  is a different question from "what do people open".
- Dependency-free vanilla JS/SVG like the rest of the frontend.

## Agent reads (phase 3)

Agents read files from the *local synced folder* — the hub never sees those
reads. The hooks pipeline shipped for session notes closes the gap:

1. **Hook**: each platform gets one more matcher — `Read` (Claude Code),
   `read_file|read_many_files` (Gemini), `read_file` (Hermes); Codex reads are
   mostly shell commands, so coverage there is best-effort. The hook runs
   `bdrive read-log`, which reads the hook JSON on stdin, extracts
   `tool_input.file_path`, and — iff the path is inside the mount and not
   ignored — appends `{path, time}` to a spool (`reads.jsonl`, `O_APPEND`,
   capped at ~10k lines) in the volume store. No network in the hook path.
2. **Flush**: the sync cycle (daemon remote tick and one-shot `bdrive sync`)
   drains the spool best-effort to `POST /api/p/<id>/reads` — a new
   device-token-authenticated endpoint that records each entry as
   `kind=agent, actor=<device id>`. Offline → the spool just waits; a failed
   flush never fails the cycle. `remote/http.go` gains the client call as an
   optional capability interface (the `PutSigner` pattern) so `file://`
   test remotes simply don't report.
3. Session attribution rides along free: the persisted session note is active
   during the same turn, so the hub can tag agent read batches with the same
   session id the writes carry (kept server-side; surfaced later if wanted).

## Privacy & config

```json
"reads": { "enabled": true, "retention_days": 400 }
```

- Default **on** (aggregate-only data; a hub admin can disable it).
- Exposed data is only ever: per-path counts by kind, distinct-reader count,
  last-read time. Who-read-what is not queryable through any API, including
  admin APIs — the actor column exists solely to make `readers` honest.
- Heat is member-visible (it helps everyone find the good docs); the Insights
  panel is admin/org-owner only, matching the saved intent.

## Testing

- **Unit** (`reads_test.go`): debounce window, retention fold (daily → all-time),
  distinct-reader counting, nil-ledger no-ops, flush-on-close.
- **Conformance**: ReadRepo ops across file/sqlite/postgres backends.
- **Handler**: render/file/download/share record with the right kind; `/blob`
  and `/store/*` do not; non-member 403 records nothing; `heat` respects
  prefix + window; heat 404s in volume mode.
- **Client** (phase 3): spool append from real hook JSON shapes (reuse the
  fixtures from `agenthooks_test.go`), flush drains + survives offline,
  cycle never fails on report errors.

## Phasing

1. **Ledger + heat** — ReadRepo (file/SQL), ReadLedger, recording sites,
   `/heat`, config block, folder heat dots + file read counts. Ships alone;
   human/share data starts accruing immediately.
2. **Insights quadrant** — admin panel, danger-zone list. Pure frontend + the
   existing endpoints.
3. **Agent reads** — `bdrive read-log`, hook matchers, spool, `POST /reads`,
   human/agent split in the UI. Depends on 1.

Phase 1 is the prerequisite for everything and is deliberately boring: one
repo, one service, three record calls, one endpoint, two UI touches.

Phase 3 is the point of the feature, not tail work: human view counts are a
commodity (every Confluence app has them); *agent* read visibility is the
part nobody else can build. Phases are ordered by dependency, not value —
ship 1 and 3 before polishing 2 if time is short.

## Addendum (2026-07-12): layered Insights dashboard

Chart research against 500-file synthetic data (CodeScene hotspots,
Obsidian heatmap plugins, disk-usage treemaps) reshaped the Insights view
into four stacked sections, all admin/org-owner gated as before, all driven
by ONE heat fetch plus the tree the client already holds, with the
all/human/agent lens applied to every section:

1. **Treemap** (the new landing view, CodeScene-hotspot style): every file
   at once, top-level folder groups labeled; cell area = reads in the
   window, cell color = days since last write (fresh→stale), ⚠ on
   hot+stale cells. Click a file cell → open the file; click a group
   label → open the folder. Squarified treemap implemented in vanilla JS
   (the frontend's no-dependency rule stands); labels only on cells that
   fit them; a single SVG with one delegated click handler so 5,000 files
   stay cheap.
2. **Quadrant scatter**, demoted to the drill-down: unchanged semantics,
   density-handled (translucent dots, radius = agent share of reads).
3. **Agent hot-path**: top-20 files by reads as horizontal stacked bars
   (agent = accent, human = blue), count at the bar end, click to open,
   ⚠ marker on danger-zone rows. Replaces the plain danger list.
4. **Coverage matrix**: agent devices × top-level folders, cell intensity
   = reads. Needs the one API addition below.

**API**: `GET /api/p/<id>/heat?by=device&days=N` returns the agent-kind
breakdown — per device (id + registry-joined name/OS), reads per top-level
folder. Privacy line, unmoved: agent *device* identity is already public
via history, so exposing it here is consistent; **human actor identities
(emails) still never appear in any response** — the breakdown is computed
from agent-kind buckets only, and the handler test asserts no email
leaks.

**Future work, deliberately not built**: calendar heatmap (human vs agent
reads/day) and folder read-share streamgraph. Both need a group-by-day
variant of the heat query; the daily buckets already exist server-side, so
that is an aggregation parameter, not a schema change.
