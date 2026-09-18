# PRD: Network efficiency — stop shipping the project on a timer

An idle browser tab on a real project costs **~400 MB/hour**. Nothing is
happening: the tab is open, nobody is typing, and the hub is re-sending the
entire file tree every 15 seconds, uncompressed, to a client that already has
a live change stream telling it what changed.

This spec removes that traffic in stages. Nothing here changes the journal,
the replay order, or any sync invariant — every change is in the viewer API,
its cache headers, and the SPA's refetch policy.

Implement the stages in order; a stage is done only when every acceptance box
in its checklist is checked and its success criteria are measured. Record
progress in §Status.

## Baseline

Measured 2026-09-18 from a HAR of a real editing session on app.beardrive.ai
(project `p-c01bde39`: 5,734 nodes — 5,050 files, 684 directories, depth 10).
Nine minutes, one file open, four characters typed:

**17.7 MB across 104 requests**, of which:

| endpoint | requests | transferred | unique bodies |
|---|---|---|---|
| `tree` | 16 | 16.53 MB | 5 |
| `heat` | 11 | 1.09 MB | 3 |
| `presence` | 37 | 0.01 MB | — |
| everything else | 40 | 0.04 MB | — |

Eleven of sixteen `tree` fetches returned bytes the client already held.
`docs/assets/net-audit.py` scores the same HAR at **92% of the wire
avoidable** — every distinct body in that session, compressed once, is
1.34 MB. The
session ended with the hub's infrastructure returning `429 Rate exceeded.`
(`server: Google Frontend`) on `tree`, `heat`, `file`, `presence` and
`projects`, which unmounted the open editor.

### Where the bytes come from

**Timers, running alongside a live SSE stream** — every one of these predates
`events` and was never retired (`useBrowse.ts` still says "matching the
classic app's 30s refresh"):

| poll | interval | payload | per hour, per foreground tab |
|---|---|---|---|
| `tree` (`hooks/useBrowse.ts:15`) | 15 s | 1.65 MB | **396 MB** |
| `heat` (`hooks/useBrowse.ts:46`) | 60 s | 133 KB | 8 MB |
| `projects` (`hooks/useHub.ts:19`) | 30 s | 1.3 KB | 0.2 MB |
| `presence` POST (`hooks/usePresence.ts:18`) | 10 s, per tab | ~0.2 KB | 0.1 MB |

React Query does not poll a background tab, so this is the cost of a tab a
person is actually looking at.

**No compression anywhere.** `writeJSON` (`server.go:1888`) is a bare
`json.NewEncoder(w).Encode(v)`; static assets are served by
`http.FileServerFS` (`server.go`, `Server.frontend`). Verified against
production with `Accept-Encoding: gzip, br`:

| | served | gzipped | ratio |
|---|---|---|---|
| `tree` | 1.65 MB | 148 KB | 11× |
| `heat` | 133 KB | 25 KB | 5.3× |
| `index-*.js` | 1.34 MB | 431 KB | 3.1× |
| `index-*.css` | 97 KB | 18 KB | 5.2× |

Only the `/store/*` sync proxy compresses (`store.go:290`).

**Fan-out per change frame** (`hooks/useProjectEvents.ts:66-109`): every frame
invalidates `["tree"]`, `["history"]`, `["heat"]`, and the bare `["text"]`
prefix — twice. A one-path write therefore costs the whole tree (1.65 MB),
the whole heat map (133 KB, which a write cannot change: heat is *read*
telemetry) and every cached file body in every project.

**Write amplification.** `RemoteSource.Commit` (`upload.go:147`) appends a
journal op unconditionally, so a save of content identical to the current
blob still journals a version and publishes a change frame — waking every
client for nothing. The billing path already treats this as free
(`billableBytes`, "every co-editor after the first saving the same converged
text") and `restore.go` already refuses it with 409 ("already the current
content"); the journal never got the memo. The HAR contains two byte-identical
511-byte PUTs 4.5 s apart, because `saved` is only updated after the PUT
resolves (`lib/sharedfile.ts:90`) and the response was queued behind a 1.65 MB
tree download.

## Goals

- An idle tab costs approximately nothing.
- A write costs every other client the changed file, not the project.
- Every JSON and asset response is compressed.
- A transient network failure never ends an editing session.

## Non-goals (do NOT do these)

- No change to the journal format, `journal.Less`, `Replay`, or any sync
  invariant. This is a viewer-API and client-policy change only.
- No change to the `/store/*` sync wire (already compressed, separately
  negotiated — see `remote/compress.go`).
- No new client-side database, service worker, or offline cache.
- No pagination of `tree` in these stages. Slimming and incremental patching
  come first; if a project ever outgrows those, that is its own spec.
- Not a rewrite of co-editing. Stage 4 removes duplicate *writes*; electing a
  single snapshotter per room is backlog.

## How success is measured

Two instruments, both repeatable:

1. **`e2e/netbudget.spec.ts`** (new, Stage 1) — drives the seeded hub and
   counts requests with `page.on("request")`. Assertions are on request
   *counts* and response *headers*, never absolute byte totals: the seeded
   project is tiny, so bytes there prove nothing. This is the regression gate
   and runs in CI with the rest of the suite.
2. **A production HAR**, re-measured after each stage on the same project with
   the same script (`docs/assets/net-audit.py`, added in Stage 1). This is
   where the byte numbers in each stage's success criteria come from. Record
   the result in §Status.

Every stage below states its criteria in both forms: what the e2e gate
asserts, and what the HAR must show.

## Stages

### Stage 1 — stop polling what the stream already reports

The SSE stream (`hooks/useProjectEvents.ts`) already announces every change.
Delete the timers that duplicate it. Keep one long safety-net interval so a
silently dropped stream still self-heals within a few minutes.

- [ ] `refetchInterval` removed from `useTree` (`hooks/useBrowse.ts:15`)
- [ ] `refetchInterval` removed from `useHeat` (`hooks/useBrowse.ts:46`)
- [ ] `refetchInterval` removed from `useProjects` (`hooks/useHub.ts:19`)
- [ ] One safety net: a 5-minute `refetchInterval` on `tree` only, with a
      comment naming what it is insuring against (a dropped `events` stream on
      a tab nobody touches)
- [ ] Global `staleTime: 30_000` default in `main.tsx:9` — every query is
      currently stale on arrival, so any remount refetches
- [ ] `e2e/netbudget.spec.ts` added: opens a project, idles 90 s, asserts the
      request count
- [ ] `docs/assets/net-audit.py` added — the HAR analysis used for the
      baseline above, so the next measurement is the same measurement

**Success criteria**

- e2e: in a 90-second idle window with no writes, the tab issues **zero**
  `tree`, `heat` and `projects` requests, and at most 10 requests in total
  (presence beats only).
- HAR, same project: an idle foreground tab for 10 minutes transfers
  **< 2 MB** (from ~66 MB).
- No behavior regression: the existing suite passes, and a peer's write still
  appears in the tree within 2 s (this is what proves the stream, not the
  timer, was doing the work).

### Stage 2 — compress and revalidate

- [ ] A `gzip` response wrapper applied at the mux for JSON and static assets
- [ ] Explicitly skipped: `events` and `collab` (SSE — buffering breaks the
      stream, and `events.go:245` already says so), `/store/*` (negotiates its
      own encoding), `/s/*` (own CSP and rate limiter; add later if wanted)
- [ ] Skipped for bodies below ~1 KB, where the header costs more than it saves
- [ ] `ETag` on `tree` and `heat`, derived from the snapshot the response is
      built from; `If-None-Match` answered with 304
- [ ] `Cache-Control: no-cache` (revalidate, don't reuse blindly) on both
- [ ] Test: a Go test asserts `Content-Encoding: gzip` on `tree` for a client
      that asks, and its absence for one that does not
- [ ] Test: a second `tree` request carrying the first's `ETag` gets 304 and an
      empty body; a write in between makes it 200 again

**Success criteria**

- e2e: `tree` and `heat` responses carry `content-encoding: gzip`; `events`
  and `collab` do not.
- HAR: `tree` wire size **≤ 15%** of its raw size (1.65 MB → ~150 KB); first
  page load's asset bytes **≤ 45%** of today's (~1.5 MB → ~650 KB).
- A repeat `tree` fetch with no intervening write transfers **< 1 KB** (304).

### Stage 3 — narrow the change fan-out

A frame names the paths that changed. Use them.

- [ ] `["heat"]` removed from the change fan-out (`useProjectEvents.ts:68`) —
      a write cannot change read counts; heat refreshes on its own cadence
- [ ] `["text"]` invalidation scoped to the named paths instead of the bare
      prefix (`useProjectEvents.ts:100,109`), which today drops every cached
      body in every project, twice per frame
- [ ] `["history"]` invalidation kept, but only when a history view is mounted
- [ ] The tree cache is **patched** from the frame's paths rather than
      refetched; a full refetch remains the fallback for `resync`, `more`, or
      an empty path list
- [ ] Remaining invalidations coalesced with a ~2 s debounce, so a burst of
      frames (a sync push, a multi-file agent run) costs one refresh
- [ ] Test: multi-client e2e — client B writes one file, client A's request
      count for that frame is asserted

**Success criteria**

- e2e: one peer write costs the other client **≤ 2 requests** (the changed
  body, and nothing else), and **0** `heat` requests.
- HAR: per-write cost on the big project drops from ~1.79 MB to **< 20 KB**.
- A 10-file agent run produces **one** tree refresh, not ten.

### Stage 4 — stop writing what did not change

- [ ] `RemoteSource.Commit` skips the op when the path already resolves to
      that blob, and reports that it did nothing
- [ ] `handleUploadContent` (`upload.go:516`) skips `publishChange` and
      `v.invalidate()` when nothing was written
- [ ] `lib/sharedfile.ts` tracks in-flight save text so a second idle-save of
      the same content never leaves the browser, and so a refetch of our own
      write is recognised before its PUT resolves — which also fixes the
      false-positive "someone else changed this file" banner from #230
- [ ] Test (Go): two identical content PUTs produce one history entry and one
      change frame
- [ ] Test (e2e): typing while a save is in flight does not raise the peer
      banner

**Success criteria**

- Two identical saves → **1** journal op, **1** change frame, **1** history
  row (today: 2 of each).
- e2e: a co-editing session of two clients typing the same document produces
  no duplicate-content PUTs.
- History on a real project stops accumulating consecutive identical-content
  rows.

### Stage 5 — a transient failure must not end an editing session

- [ ] `useTextAt` retries transient statuses (429, 5xx) with backoff instead of
      `retry: false` (`hooks/useBlob.ts:50`); a pinned `?v=` URL keeps today's
      no-retry behavior
- [ ] `EditView` never unmounts a live editor on a read error — the failure is
      a banner, the buffer and its save timer stay alive
      (`components/FileView.tsx:281`)
- [ ] The collab stream is not torn down by a failed *file* read
- [ ] Test (e2e): with `file?path=` forced to 429, the editor stays mounted,
      the buffer keeps its text, and a save still lands once the route recovers

**Success criteria**

- e2e: after a forced 429 on the file read, `.cm-host .cm-content` is still
  present, still holds the typed text, and a subsequent save succeeds.
- No path exists where an unsaved buffer is discarded by a network error.

### Stage 6 — slim the payload

Diminishing returns after Stage 2; do it only if the tree is still the biggest
response on the wire.

- [ ] `path` dropped from tree nodes — 29% of the payload (480 KB), fully
      derivable from the `children` chain; the client already walks the tree
      (`useBrowse.ts:19-34`) and can build it there
- [ ] `author` dropped where it equals `user`
- [ ] `user`/`user_name`/`device` interned into an id table on the response,
      instead of repeating a handful of strings 5,050 times
- [ ] `architecture/webapp-frontend.md` and `architecture/webapp-server.md`
      updated if the node type changes shape

**Success criteria**

- Raw `tree` payload **≥ 35% smaller** for the same project.
- Gzipped `tree` measurably smaller (interning helps less after compression —
  if it does not, stop and revert this stage rather than carry the complexity).

## Backlog (filed, not scheduled)

These came out of the same audit and are real, but none is worth blocking on:

- **Co-editor save election.** Every client in a room runs its own idle-save
  of the same converged text: N writes × N clients. Stage 4 defuses the
  storage and history half; electing one snapshotter is the real fix.
- **Awareness POST batching** (`lib/collab.ts:96`). Document updates coalesce
  at 60 ms; cursor moves POST immediately, one per event.
- **History N+1** (`components/HistoryView.tsx:259`). One
  `heat?session=&device=` request per run card rendered.
- **Analytics volume.** PostHog session recording made 64 requests in nine
  minutes on the user's connection, with `maskTextSelector: "*"`. Worth a
  sampling rate.
- **Unbounded collections.** `projects`, `orgs`, `folders`, `shares`,
  `permissions`, `admin/pending`, `mcp/grants` all return whole collections
  with no limit. Fine at today's sizes; note the ceiling.

## Status

_Not started. Fill in below; each stage records the measured numbers, not a
claim that it is done._

| Stage | State | Measured |
|---|---|---|
| 1 — stop polling | not started | |
| 2 — compress + revalidate | blocked on 1 | |
| 3 — narrow the fan-out | blocked on 1 | |
| 4 — stop no-op writes | blocked on 3 | |
| 5 — survive a failure | not started | |
| 6 — slim the payload | blocked on 2 | |

### Measurements

| date | scenario | requests | transferred | note |
|---|---|---|---|---|
| 2026-09-18 | 9 min, 1 file open, 4 keystrokes | 104 | 17.7 MB | baseline, pre-Stage-1 |
| 2026-09-18 | idle foreground tab, extrapolated | — | ~400 MB/h | from the poll intervals |
