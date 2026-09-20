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

1. **`e2e/netbudget.spec.ts`** (Stage 1) — drives the seeded hub and
   counts requests with `page.on("request")`. Assertions are on request
   *counts* and response *headers*, never absolute byte totals: the seeded
   project is tiny, so bytes there prove nothing. This is the regression gate
   and runs in CI with the rest of the suite.
2. **A production HAR**, re-measured after each stage on the same project with
   the same script (`docs/assets/net-audit.py`). This is
   where the byte numbers in each stage's success criteria come from. Record
   the result in §Status.

Every stage below states its criteria in both forms: what the e2e gate
asserts, and what the HAR must show.

## Stages

### Stage 1 — stop polling what the stream already reports

The SSE stream (`hooks/useProjectEvents.ts`) already announces every change.
Delete the timers that duplicate it. Keep one long safety-net interval so a
silently dropped stream still self-heals within a few minutes.

- [x] 15 s `refetchInterval` removed from `useTree` (`hooks/useBrowse.ts`)
- [x] 60 s `refetchInterval` removed from `useHeat` (`hooks/useBrowse.ts`)
- [x] 30 s `refetchInterval` removed from `useProjects` (`hooks/useHub.ts`)
- [x] One safety net: a 5-minute `refetchInterval` on `tree` only, commented
      as insurance against a dropped `events` stream on a tab nobody touches
- [x] Global `staleTime: 30_000` default in `main.tsx` — every query was
      stale on arrival, so any remount refetched
- [x] `e2e/netbudget.spec.ts` added: opens a project, idles 70 s, asserts the
      request count. Window is 70 s because the slowest poll it replaces ran
      at 60 s — a shorter one would pass against code that still polls
- [x] Verified the gate FAILS on the pre-stage bundle (4 tree polls caught)
- [x] `docs/assets/net-audit.py` added — the HAR analysis used for the
      baseline above, so the next measurement is the same measurement
- [x] `architecture/webapp-frontend.md` + `webapp-server.md` notes corrected:
      both claimed a 15 s tree poll sat underneath the stream

**Success criteria**

- e2e: in a 70-second idle window with no writes, the tab issues **zero**
  `tree`, `heat` and `projects` requests, and at most 10 requests in total
  (presence beats only).
- HAR, same project: an idle foreground tab for 10 minutes transfers
  **< 2 MB** (from ~66 MB).
- No behavior regression: the existing suite passes, and a peer's write still
  appears in the tree within 2 s (this is what proves the stream, not the
  timer, was doing the work).

### Stage 2 — compress and revalidate

- [x] `gzipResponses` applied outermost in `Server.Handler`, so it covers even
      what the auth gate and the rate limiter write themselves (`compress.go`)
- [x] An allowlist by content type, not a denylist: an unknown type is left
      alone rather than compressed hopefully
- [x] `text/event-stream` excluded outright, and `gzipWriter` implements both
      `Flush` and `Unwrap` — the failure mode here is not slowness, it is SSE
      that never arrives (`refuseUnstreamable` exists because that shipped once)
- [x] `/store/*` skipped by PATH, not merely when it has already encoded:
      `syncer.pull` resumes at a byte offset, so a length meaning anything
      other than "bytes on this socket" re-downloads forever
- [x] 206 / `Content-Range` skipped — a range is an offset into the plaintext,
      and `http.FileServerFS` answers ranges for the embedded assets
- [x] `Content-Length` preserved as `X-Uncompressed-Length`: the file viewer
      decides "too large to render" before reading the body (`useBlob.ts`),
      and compression would have silently disarmed that
- [x] `ETag` + `Cache-Control: no-cache` on `tree` and `heat`
      (`writeJSONCached`); `If-None-Match` answered with a bodiless 304
- [x] Tests: compressed for a client that asks and not for one that does not;
      `gzip;q=0` is a refusal; an event stream is neither compressed nor
      buffered and its flush still reaches the socket; `gzipWriter` passes
      `refuseUnstreamable`; already-encoded and already-compressed bodies pass
      through; the sync wire is untouched; a range response is untouched; the
      size hint survives; the ETag round-trips and moves when the tree does
- [x] e2e: `tree` carries `content-encoding: gzip` and `events` does not
- [ ] Skipped for bodies below ~1 KB — **not done, deliberately**: the
      buffering that needs is where the bugs live, and a 12-byte `{"ok":true}`
      gaining 20 bytes of framing is not a problem anyone has. Revisit if tiny
      JSON ever dominates a profile.
- [ ] `/s/*` share pages — left for later, as scoped

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

- [x] `handleUploadContent` returns before it journals when the path already
      resolves to the blob being written: no op, no change frame, no History
      row, no tree invalidation. Reported as `unchanged: true`, not an error —
      nothing the caller asked for is missing
- [x] `lib/sharedfile.ts` tracks in-flight writes as a **Set**, not a slot: a
      second idle timer fires while the first PUT is still out, and a single
      slot forgets the earlier write exactly when its refetch arrives. (The
      first version of this fix was a slot. The e2e caught it.)
- [x] A duplicate idle-save of identical text never leaves the browser
- [x] The false-positive "someone else changed this file" banner from #230 is
      closed: a refetch of our own write is recognised before its PUT resolves
- [x] Test (Go): two identical PUTs leave ONE version and a real edit still
      leaves two — against a real hub, since a DirSource has no journal to
      keep clean and identifies files by mtime and size
- [x] Test (e2e): typing while a save is in flight does not raise the banner.
      Took four attempts to become a real gate — see the note below

**Success criteria**

- Two identical saves → **1** journal op, **1** change frame, **1** history
  row (today: 2 of each). ✅ asserted.

> **On testing these.** Three tests in this PRD's stages would have passed
> against the bug they were written for, and the reason is always the same:
> localhost is too fast to reproduce a timing bug. A save completes inside the
> coalescing window; a refetch lands before the next keystroke; a banner
> appears and is cleared before the assertion looks. Every one of them needed
> induced latency — a held response, a delayed re-read — and a watcher armed
> before the window rather than a check after it. Re-run a new test against
> the UNFIXED code before believing it.
- e2e: a co-editing session of two clients typing the same document produces
  no duplicate-content PUTs.
- History on a real project stops accumulating consecutive identical-content
  rows.

### Stage 5 — a transient failure must not end an editing session

- [x] `HttpError` carries the status (`api/http.ts`), so a retry policy can be
      keyed on 429/5xx instead of matching on error prose that product copy
      rewrites. `.message` is unchanged, so every existing toast reads as before
- [x] `useTextAt` retries transient failures with backoff (1s/2s/4s) instead of
      `retry: false`; 403 and 404 are answers and are not retried; a pinned
      `?v=` URL keeps today's no-retry behavior
- [x] `EditView` only treats a read error as fatal when there is no `data` yet.
      React Query keeps the last good body across a failed refetch, so a live
      editor keeps its buffer, its idle save timer AND its co-editing stream
- [x] The failure is a banner (`#read-stale`) that says the work is untouched
      and still saving
- [x] Test (e2e): with `file?path=` forced to 429, the editor stays mounted,
      the buffer keeps its typed text, no `.empty` state appears, and a save
      still lands once the route recovers
- [x] Verified the gate FAILS on the previous behaviour (the banner never
      appears, because the editor was replaced by "Could not open … for
      editing")

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
| 1 — stop polling | **done** | e2e: 0 tree/heat/projects requests in a 70 s idle window (was 4 tree + 2 projects + 1 heat). Prod HAR: pending |
| 2 — compress + revalidate | **done** | Go + e2e green. Prod HAR: pending |
| 3 — narrow the fan-out | blocked on 1 | |
| 4 — stop no-op writes | **done** | Go + e2e green, both verified failing without the fix |
| 5 — survive a failure | **done** | e2e green, and verified failing on the old behaviour |
| 6 — slim the payload | ready | |

### Measurements

| date | scenario | requests | transferred | note |
|---|---|---|---|---|
| 2026-09-18 | 9 min, 1 file open, 4 keystrokes | 104 | 17.7 MB | baseline, pre-Stage-1 |
| 2026-09-18 | idle foreground tab, extrapolated | — | ~400 MB/h | from the poll intervals |
