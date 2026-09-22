# PRD: The hub talks to itself — make push authoritative, then unpin the instance

The hub serves **183,150 requests a day**, and it serves them at 04:00 UTC at
almost exactly the rate it serves them at 17:00. A product driven by people
swings tenfold between night and peak. This one swings 1.6×.

That flatness is the whole finding: **~95% of the traffic is machines polling
on timers while nobody is working.** One idle laptop's sync daemon accounts
for 9.4% of the entire hub. Two of them are a fifth of it.

None of this needed new transport. The push channels already exist and both
sides already speak them — an SSE change stream (`/events`, consumed by the
browser *and* by `remote.httpBackend.Watch`) and a co-editing websocket. The
polls simply never learned to stand down next to them.

This spec removes the chatter first, because it is mostly deletion and touches
no invariant. Then it fixes three things that are broken today at one
instance. Only then does it approach `max_instance_count = 1`, which is a real
project and whose stated justification turns out to be false.

Implement the stages in order; a stage is done only when every acceptance box
in its checklist is checked and its success criteria are measured. Record
progress in §Status.

> **Standing rule, inherited from `network-efficiency-prd.md` and earned three
> times there.** Every new test in this PRD must be re-run against the
> **unfixed** code and observed to FAIL before it is believed. Five tests in
> that PRD passed against the bug they were written for. Localhost is too fast
> to reproduce a timing bug, an empty feed proves nothing, and a nil provider
> is never in the path. A gate you have not watched fail is not a gate.

## Baseline

Measured 2026-09-21 from `beardrive-prod` Cloud Run logs and Cloud Monitoring.
Live service: one instance, 1 vCPU, concurrency 1000, timeout 3600s.

**183,150 requests / 24h = 2.1 req/s average.** Hourly range 6,405–10,457 —
**peak-to-trough 1.6×**.

Request mix over a representative 20-minute window (3,446 requests, 33 distinct
clients: 12 sync daemons, 17 browsers):

| endpoint | requests | % | what it is |
|---|---|---|---|
| `GET /store/list` | ~1,400 | 41% | daemon poll, every cycle, per project |
| `POST /presence` | ~670 | 19% | browser beat, every 10s, **per tab** |
| `GET /scope` | ~283 | 8% | daemon, every cycle, paired 1:1 with `store/list` |
| `GET /store/object` | 272 | 8% | real blob/journal transfer |
| `GET /tree` | 117 | 3% | 5-min "insurance" poll, ~148 KB gz |
| `PUT /upload/content` | 90 | 3% | editor idle-saves, N identical bodies |

Daemons were **67%** of all requests. A single laptop produced **1,223 of
3,446 (35%)**, cycling every 3.3s rather than the configured 10s.

### What one client costs while doing nothing

| client | requests/day | share of the whole hub |
|---|---|---|
| idle daemon, one project, 10s cadence | 17,280 | **9.4%** |
| same daemon during an agent session (3s) | 57,600 | 31% |
| idle browser tab (presence beat alone) | 8,640 | **4.7%** |
| idle browser (tree poll alone) | 288 | 41.6 MB/day |

### Where the polls ignore the push

Five, with the line that does it:

1. **`daemon.go:512`** — `doRemote` gates only on `remoteInterval` and never
   consults `watch`. A healthy open change stream does not extend the poll by
   one millisecond. The code states the intent plainly at `daemon.go:438`:
   *"It is an accelerator only: every guarantee still rests on the tick."*
2. **`daemon.go:599`** — a local-only tick with `res.LocalOps > 0` zeroes
   `lastRemote`, forcing the *next* 3s tick to be a full remote cycle. This is
   the 3.3s laptop: an agent writing files pins the daemon at the scan
   interval.
3. **`syncer.go:169`** — `loadScope` calls `GET /scope` before every scan,
   unconditionally, with no `If-None-Match`. The response carries a `tag`
   whose only purpose is to say "unchanged" — *after* the round trip.
4. **`useBrowse.ts:29`** — a 5-minute tree refetch as insurance against the
   same stream that already emits a `: keepalive` comment every 20s
   (`events.go:53`), which is a better liveness signal than 148 KB.
5. **`usePresence.ts:18`** — `BEAT_MS = 10_000`, **per tab**, not
   leader-elected (the SSE stream is, via Web Lock). Six tabs, six beats.
   Anyone with an editor open is simultaneously announcing presence over
   ycollab awareness: two liveness mechanisms for one person.

And one write amplification: **`sharedfile.ts:195`** rearms the 700ms save
timer on *any* `Y.Text` change including a remote peer's, so N co-editors each
PUT the identical full body; `ycollab.go:63` then snapshots the same bytes a
third time.

### The 429, and why it is a separate problem

378 requests were shed in ten minutes on 2026-09-21 with
`The request was aborted because there was no available instance` — while CPU
sat at 38%, memory 56%, concurrency 26/1000 and handler p95 under 400ms, on one
healthy instance. Nothing was full. `max_instance_count = 1` leaves zero burst
headroom: the autoscaler wants a second instance, is denied one, and sheds.
Minute-granularity metrics cannot see the sub-second arrival bursts that
trigger it, so healthy-looking graphs are expected and are **not** evidence
against it.

Phases 1–2 do not fix this. They lower the arrival rate that provokes it, which
is worth roughly 10× and is cheaper than Phase 3.

## Goals

- Cut hub requests per idle client by **≥90%** without losing a single
  freshness guarantee.
- Make the 24h traffic profile track human activity: peak-to-trough **≥4×**,
  from 1.6×.
- Fix three defects that are live today at one instance (device-id churn,
  unrefreshed MCP grants, lossy read ledger).
- Leave `max_instance_count = 1` *removable* — with an honest, ordered blocker
  list rather than a comment that is no longer true.

## Non-goals (do NOT do these)

- **Do not add a new transport.** SSE downlink and the collab websocket both
  work. Adding a second socket where a poll should simply stop is more code
  for less benefit.
- **Do not use Web Push as a sync channel.** ~240 msgs/min/device, 4 KB
  payloads, permission-gated, relayed through FCM/APNs, and structurally
  irrelevant to the CLI daemon, which is two-thirds of the traffic. It is
  admissible in the backlog for one thing only: "a teammate changed this while
  your tab was closed".
- **Do not raise `max_instance_count` before Phase 3 lands in order.** Sign-in
  would fail ~50% of the time and co-editing would hold two documents per file.
- **Do not weaken the scope-before-scan ordering.** `loadScope` runs before the
  scan so the scan cannot mint an op the hub will refuse; a refused op wedges
  that device's sync until someone edits its journal by hand.
- **Do not add Redis in Phases 1–2.** Postgres is already in prod and covers
  the fan-out need at this scale.

## How success is measured

Three instruments, all repeatable:

1. **`e2e/netbudget.spec.ts`** — the existing regression gate from
   `network-efficiency-prd.md`. Extend it; assert request *counts* and
   *headers*, never byte totals (the seeded project is tiny).
2. **A new `internal/daemon` test** for the poll/watch interaction. This is
   where Stage 1 lives or dies, and it must be a Go test — a live stream
   suppressing a timer is deterministic and machine-local.
3. **Production, via the same two queries each time.** These produce every
   number in §Baseline, so each re-measurement is the same measurement:

   ```sh
   # 24h volume and shape (the flatness metric)
   gcloud monitoring ... run.googleapis.com/request_count   # see §Measurements

   # 20-minute request mix by endpoint and client
   gcloud logging read 'resource.type="cloud_run_revision" AND
     resource.labels.service_name="bdrive-cloud" AND httpRequest.requestMethod:*' \
     --project beardrive-prod --limit 5000 \
     --format 'value(timestamp, httpRequest.remoteIp, httpRequest.userAgent,
                     httpRequest.requestMethod, httpRequest.requestUrl, httpRequest.status)'
   ```

Every stage states its criteria in both forms: what the automated gate
asserts, and what production must show.

---

# Phase 1 — make push authoritative

No new infrastructure. No invariant touched. Mostly deletion.

### Stage 1 — the daemon stands down while the stream is live

The single biggest win in this document: 30× fewer requests per idle device.

`doRemote` learns a third state. With a healthy watch open, the remote cadence
drops from `remoteInterval` (10s) to a long safety net (5 min). The instant the
stream dies — for any reason, including the hub's own 1h `streamMaxAge` — the
cadence reverts to 10s until it is re-established.

- [x] `daemon.go` — `doRemote` consults watch health, not just elapsed time
- [x] A `watchedInterval` (5 min) distinct from `remoteInterval` (10s), with
      the reversion path on stream close, token change and `res.Offline`
- [x] `daemon.go:599` — a local edit still forces a prompt remote cycle (this
      is correct and must survive), but is **rate-limited** so a file-writing
      agent cannot pin the daemon at the 3s scan interval. A minimum spacing,
      not a removal: an agent's write must still reach the hub promptly
- [x] The watch-death path is *age-discriminated* exactly as today
      (`daemon.go:608`): a stream that lived ≥ `watchRetry` re-dials at once
- [x] Go test: a fake `Watcher` held open across N ticks asserts the remote
      cycle count collapses; closing the channel asserts it recovers to 10s
- [x] **Verified the test FAILS on the pre-stage daemon** (it must count the
      10s polls that run beside the live stream)

**Success criteria**

- Go test: with a live stream over a simulated 5 minutes, remote cycles drop
  from 30 to ≤2; after the stream closes, the next cycle is ≤10s away.
- Production: an idle daemon-project falls from 17,280 to **≤600 requests/day**.
- No freshness regression: a peer's write still lands within **2s** (this is
  what proves the stream was doing the work all along).

### Stage 2 — stop asking for scope on every cycle

`/scope` is 8% of all hub traffic to re-answer a question whose answer changes
approximately never.

The safety ordering is non-negotiable, so the fix is *not* a client-side cache
with a lag. The hub pushes a `scope` frame on the same stream that is already
open, which makes the client's knowledge **fresher** than today's 10s poll, not
staler.

- [x] `events.go` — a `scope` frame published when a folder rule changes
- [x] `remote.Watcher`'s contract currently discards the frame body
      (`chan struct{}`, `http.go:598`). Widen it minimally to carry the frame
      kind, or add a sibling channel — do not plumb the whole payload
- [x] `syncer.go:169` — `loadScope` skips the fetch when the persisted
      `st.ScopeTag` is current and no scope frame has arrived; fetches
      immediately when one has, or when there is no live stream
- [x] Fallback preserved: no stream, or a hub that never sends the frame ⇒
      fetch every cycle exactly as today
- [x] Test: a rule change mid-session reaches the device and is enforced
      **before** the next scan commits an op
- [x] **Verified the test FAILS** against a client that skips scope without the
      push (it must catch the op the hub would refuse)

**Success criteria**

- `/scope` falls from ~8% of requests to **<0.5%**.
- A narrowing rule change is enforced on a connected device in **<2s**
  (today: up to 10s).
- No device ever journals an op the hub refuses — the existing scope tests pass
  unchanged.

### Stage 3 — presence rides the connection that already exists

19% of hub traffic to say "I am still looking at this page", from tabs where
nobody is doing anything.

Presence stops being a heartbeat and becomes **a property of the SSE
connection**, which the hub already tracks in `eventHub.subs`. Liveness is the
connection; the only uplink left is a path change, which is an event, not a
timer.

- [x] Presence derived from the subscriber registry (`events.go:90`) — a
      connected subscriber *is* present; `presenceTTL` keyed to the connection,
      not to a 15s window
- [x] The uplink fires **on navigation only**. No interval
- [x] Per-browser, not per-tab: the existing Web Lock leader (which already
      owns the stream) reports the set of paths its tabs are on
- [x] Anyone with an editor open already announces via ycollab awareness —
      reconcile so one person is not two rosters
- [x] e2e: a tab idle for 70s with no navigation issues **zero** presence
      requests, and the roster still shows them
- [x] **Verified the gate FAILS** on the pre-stage bundle (it must catch ≥6
      beats in that window)

**Success criteria**

- e2e: zero presence requests in a 70s idle window; roster still correct.
- Production: `POST /presence` falls from ~19% of traffic to **<1%**.
- A teammate opening a file still appears in the roster within **2s**, and
  disappears within **20s** of closing the tab.

### Stage 4 — one writer for a co-edited file

Three writers of identical bytes, which is also the history-noise complaint.

The hub holds the document (`ycollab.go`) and already snapshots it. With a
hub-held room live, the browser's idle save is redundant — and worse, it is
redundant **per co-editor**.

- [x] `sharedfile.ts` — the idle save does not fire for changes that arrived
      from a peer; only local edits arm the timer
- [x] With a live hub-held room, the client defers the write to the hub
      entirely; the solo path (no room) keeps saving exactly as today
- [x] `ycollab.go` — a periodic snapshot while a room is live, so a long
      session is not one write at the end. Identical content still journals
      nothing (`upload.go`), so cadence is cheap
- [x] The crash case stays covered: `OnLastPeer`/`OnUnloadDocument` unchanged
- [x] Test: four simulated editors typing for 60s produce **one** journal
      version per quiet period, not four
- [x] **Verified the test FAILS** against the current client (it must count the
      N identical PUTs)

**Success criteria**

- Four co-editors typing for a minute produce `PUT /upload/content` counts that
  scale with *edits*, not with `editors × edits` — target **≥70% fewer** writes.
- History shows one version per quiet period, not N.
- No lost edits: the existing collab, stress and concurrency suites pass,
  including under `-race`.

### Stage 5 — the tree poll goes away — **AMENDED: it stays**

**This stage was wrong and is not being implemented as written.** Recorded
rather than quietly dropped, because the reasoning is the point.

Two things came out of measuring it instead of assuming:

1. **The keepalive is unreachable from the browser.** `EventSource` never
   surfaces comment lines to `onmessage` — that is exactly why `events.go:53`
   sends `: keepalive` as a comment, so clients never see it. Making it a real
   frame would make the DAEMON wake on it too (`http.go` filters on `data: `),
   turning a 20s heartbeat into a sync cycle every 20 seconds on every device
   and undoing Stage 1 several times over.
2. **The poll already costs almost nothing.** `/tree` goes through
   `writeJSONCached` (`server.go:1952`), so an unchanged tree is a **304 of
   about 30 bytes**. The 5-minute interval is 288 requests and roughly 8 KB a
   day per browser — 0.16% of the hub's daily traffic, not the 3% the sampled
   window suggested (that window was active use, not idle).

So the trade on offer was: delete the only insurance against a stream that
died silently, to save 8 KB a day. A half-open connection is precisely the
failure the browser cannot detect on its own, and the cost of getting it wrong
is a tab that shows stale content indefinitely with nothing to say so.

- [x] Measured rather than removed; `useBrowse.ts:29` left in place
- [x] Reason recorded here so the next reader does not re-derive it

**Success criteria** — superseded. The stage's goal (`/tree` < 1% of requests)
is met by Stages 1 and 3 reducing everything around it, not by removing this.

---

# Phase 2 — three things broken today, at one instance

None of these need scale-out. All three are live now.

### Stage 6 — a hub identity that survives a cold start

`BDRIVE_HOME=/tmp/bdrive` is per-instance tmpfs, so `config.LoadDevice()`
(`config.go:69`) mints a **new random device id on every cold start and every
revision**. Each one becomes a permanent `journal/<rand>.jsonl` per project,
and every reader's cold fold pays a round trip per id that has ever existed.
This is unbounded growth and it is already happening.

- [x] The hub's device id becomes durable and explicit — derived from config or
      persisted in the metadata store, not minted onto a tmpfs
- [x] A migration/compaction story for the ids already stranded in prod
      (count them first; do not delete a journal that holds real ops)
- [x] Test: two sequential hub starts with a wiped `BDRIVE_HOME` journal to the
      **same** key
- [x] **Verified the test FAILS** against the current binary

**Success criteria**

- Restarting the hub adds **zero** new journal keys.
- `List("journal/")` on `p-c01bde39` returns a count that stops growing across
  deploys.

### Stage 7 — the two registries that never refresh

- [x] `MCPAuth` (`mcpauth.go:113`) — grants/tokens loaded once in
      `NewMCPAuth` and never refreshed, though `sqlMCPRepo.Version()` already
      exists. **A revocation is not honoured for the life of the process.**
      Add the `versionGate` every other registry has
- [x] `ReadLedger` (`reads.go:142`) — **MOVED TO PHASE 3.** Filed here on the
      assumption it was broken at one instance. It is not: the hub is the only
      writer (viewer reads, `/api/p/<id>/reads` from devices, and the desktop
      sidecar all land in this process), so the in-memory map is authoritative
      and is reloaded at boot. `PutBatch` upserting ABSOLUTE counts only loses
      data once a second writer exists, which is Stage 9's problem and wants
      Stage 9's answer — making the flush additive means changing the
      `ReadRepo` contract, both backends and the conformance suite, for a
      benefit nothing can observe until the instance cap lifts
- [x] `cloud/cmd/bdrive-cloud/main.go` never sets `Refresh`, so the snapshot
      cache is **off** in prod and every request refolds journals. Set it
- [x] Test each: a second process's write is observed after the gate ticks

**Success criteria**

- An MCP token revoked through one path is a 401 on the next request, not after
  a restart.
- ~~Read counts survive a concurrent flush without loss.~~ — moved to Phase 3
  with the item itself; there is no concurrent flush at one instance.
- The snapshot cache is ON in the cloud binary, so a read within the refresh
  window does not refold the project's journals.

---

# Phase 3 — unpin `max_instance_count`

**The reason written in `run.tf` is false today.** Two instances would not both
append one journal — under `BDRIVE_HOME=/tmp` they mint different ids
(Stage 6 makes that *deliberate* rather than accidental). The real blockers are
elsewhere and are worse. Ordered; each is a prerequisite for the next.

### Stage 8 — in-memory auth state to SQL — **DONE**

Sign-in would fail roughly half the time on two instances.

- [x] **The seam.** `PendingRepo` + `PendingGrant` on `MetaStore`: short-lived,
      single-use state keyed by (kind, key), with an opaque payload so four
      owners do not put four shapes in one schema. `Take` is atomic (a
      transaction on SQL) because single use has to be — two pollers winning
      one code is the bug `takeGranted` already fixes inside one process.
      Expiry is enforced on READ, never by a sweeper having run. Both backends
      + `TestPendingRepoConformance`
- [x] **MCP consent codes** (`mcpauth.go`) — the first consumer, and a real
      cross-instance flow: the browser POSTs consent to one instance and the
      client exchanges the code from another. Orphan cleanup rewritten as a
      property of the GRANT (no token digest, older than `mcpCodeTTL`) rather
      than a walk over in-memory codes — simpler, and it no longer strands a
      grant when a code vanishes for any other reason
- [x] **CLI one-time codes and device-flow grants** (`authcli.go`) — done, and
      the design question is settled the way the recommendation said: the caps
      stay PER-PROCESS. They bound memory, which is a per-process resource, so
      a shared counter would be the wrong shape as well as a round trip on the
      mint path. `seen` is this process's index of what it has outstanding;
      the store is the truth. Two consequences worth writing down:
      - the link lookup stopped being a linear scan over every pending grant
        and became a keyed row of its own. A store has no "find by field", and
        the scan was already flagged in a ponytail note as the thing to fix if
        the ceiling rose
      - `takeGranted` reads through the store's atomic `Take`, so the race it
        was written to close inside one process — every poll in flight when a
        human approves getting its own permanent token — stays closed ACROSS
        processes
- [x] **PropelAuth OAuth state nonce** (`cloud/internal/authpropel`, cloud #47)
      — the biggest single sign-in blocker, and it was never about journals.
      The nonce is consumed through the store's atomic `Take`, so a callback
      replayed from browser history or a forwarded URL cannot be honoured
      twice, or honoured twice by two instances racing. Defaults to
      `webapp.NewMemoryPending` — the hub's OWN in-memory store, not a second
      one with its own expiry rules, which is why that was exported
- [ ] Builtin email-verify / reset grants (`authlocal.go:78`) — not prod, same
      shape

**Success criteria:** two instances behind a non-sticky LB complete 50/50
browser sign-ins and 20/20 `bdrive login` flows.

**Met, with one honest qualification.** Every piece of sign-in state that used
to be a map inside one process now lives in a store any process can read: MCP
consent codes, CLI one-time codes and device-flow grants, and the OAuth state
nonce. Each is covered by tests that mint on one process and redeem on
another, each pairs that with the case that would be a security bug if it
crossed wrongly (an approved grant won by EXACTLY ONE process; a replayed
callback refused), and each keeps a permanent CONTRAST test asserting that
with no shared store the state stays put — which is what a single-instance
hub should keep doing.

The qualification: this was proven by two provider instances sharing a store
in one test process, not by literally standing up two Cloud Run instances
behind the load balancer and counting 50 sign-ins. That test cannot exist
until the cap is raised, and the cap should not be raised until Stages 10 and
11 land. So the criterion is met in substance and the literal 50/50 run
belongs to Stage 12.

### Stage 9 — cross-instance fan-out — **the transport is done**

`eventHub` is a per-process map, so a frame reaches only clients pinned to the
writing instance. **Postgres `LISTEN/NOTIFY`** — already in prod, no new
infrastructure, and comfortably within its envelope at a handful of listener
instances. Redis only if that envelope is exceeded.

- [x] `eventRelay` seam (`relay.go`), with `publish` split from the local
      `fanout` so a frame from another process re-enters the SAME path a local
      one takes. It carries bytes, not a `changeEvent`: what arrives is the
      JSON the other process marshalled, and re-decoding it here would only
      create a way for the two paths to disagree
- [x] Echo suppression in ONE place. Both transports broadcast to every
      listener including the publisher, so the origin is prefixed to the wire
      payload and stripped on the way out — without it every client is told
      about each change twice and refetches twice
- [x] An oversized frame degrades to `resync`, never to a truncated path list.
      A client handed half the paths believes it has been told about all of
      them; Postgres `NOTIFY` refuses anything over 8000 bytes outright
- [x] `memRelay` for the shape, `pgRelay` for the transport — the latter with
      a dedicated LISTEN connection (it blocks for the life of the process)
      and NOTIFY through the existing pool, sent via `pg_notify($1,$2)` rather
      than a built statement, because the thing that would need escaping is a
      path chosen by whoever wrote the file
- [x] Reconnection with backoff, which matters more than it looks: a dropped
      listener is SILENT — the hub keeps serving and simply stops hearing
      other processes, so the symptom is "some tabs are stale", not an error
- [x] Tested against a real Postgres in CI (`meta-postgres`), including the
      echo and oversize cases. The `-run` pattern names them, because a test
      that runs for nobody is the trap that job was added to close
- [ ] **Not wired on by default.** The relay is opt-in until the instance cap
      lifts: at one instance it would hold a Postgres connection to talk to
      nobody, and `db-f1-micro` allows about 25 of them
- [ ] Presence roster follows the same path (it splits and *flaps* otherwise)
- [ ] Per-process caps (`maxSubsTotal`) reconsidered — they multiply by N
- [ ] `ReadLedger` (moved here from Stage 7) — still outstanding
- [ ] **`ReadLedger`, moved here from Stage 7.** `byKey` is loaded once and
      never refreshed, and `PutBatch` upserts ABSOLUTE counts by
      `(project, path, day, kind, actor)` — so two instances each holding
      `boot + their own increments` overwrite each other on every flush, losing
      roughly half of all read telemetry. Needs the change-token gate AND an
      additive flush, which is a `ReadRepo` contract change across both
      backends and `db_conformance_test.go`

**Success criteria:** a write on instance A reaches a browser on instance B in
<2s; rosters agree across instances.

### Stage 10 — conditional writes in the storage layer

`upload.go:471`'s `lockPath` is a process-local `sync.Map` and no backend
issues a storage precondition, so two instances can both pass `If-Match` and
both journal. This is the lost-update hazard, and it is worth doing **even at
one instance** for the MCP compare-and-swap that currently advertises more than
it delivers (`mcp.go:1352` already admits this).

- [ ] GCS generation preconditions (and the S3 equivalent) on journal writes
- [ ] Quota reservations and the invite seat lock stop being per-process

### Stage 11 — co-editing across instances

Two instances = two `crdt.Doc`s per file, each seeding from storage and each
snapshotting back: precisely the failure the relay rewrite eliminated,
restored at the server.

`github.com/reearth/ygo` ships a `cluster` package for exactly this —
`Server.AttachRelay(cluster.Relay)`, with a Redis implementation. **Caveat that
must be designed for:** the relay shares CRDT state but does *not* dedupe
`OnLoadDocument`/`OnLastPeer`/`OnUnloadDocument`, so N nodes would fire N
snapshots and reintroduce the history noise. A snapshot owner per room is part
of this stage, not a follow-up.

- [ ] Choose: ygo `cluster` + Redis, or room affinity by consistent hash with
      internal forwarding (no new infrastructure, but a routing layer)
- [ ] Snapshot ownership elected per room either way
- [ ] Cloud Run session affinity is **best-effort only** and is not a
      substitute for either

### Stage 12 — raise the cap

- [ ] `max_instance_count` raised only after 8–11, with the `run.tf` comment
      rewritten to say what is actually true
- [ ] Load test that reproduces the 2026-09-21 burst and shows zero shedding

**Success criteria:** the burst that shed 378 requests sheds zero.

## Backlog (filed, not scheduled)

- **Web Push for the closed-tab case only** — "a teammate changed this while
  you were away". Never as a sync channel (see §Non-goals).
- **Agent hook cost** — `bdrive sync .` fires per `Write|Edit` tool call *and*
  per user turn, on top of the daemon's own cycle. `read-log` is already
  correct (spools locally, never touches the network); the sync hooks could
  spool a wakeup for the daemon instead of running a full cycle each.
- **`Setup.tsx:262`** — a 400ms poll with no timeout or attempt cap; a sidecar
  stuck in `syncing` polls at 2.5 Hz for as long as the window is open.
- **`db-f1-micro` connection ceiling** (~25) — not a problem at one instance,
  a hard one at N.
- **Journal compaction** — `appendOps` reads and rewrites the *whole* journal
  object per commit (`upload.go:231`), which is O(journal) per write forever.

## Status

| Stage | State | Measured |
|---|---|---|
| 1 — daemon stands down | **done** (#246) | 10 polls in 3s → 1, gate re-run against unfixed daemon |
| 2 — scope on push | **done** (#246) | rule change reaches a device in <1s, was up to 5 min |
| 3 — presence on the connection | **done** (#246) | zero presence requests in a 70s idle window |
| 4 — one writer per file | **done** (#246) | a bystander in a room issues zero writes |
| 5 — tree poll removed | **amended — kept** | 304s at ~30 bytes; see the stage |
| 6 — durable hub identity | **done** (#247) | prod had 43 journal keys on a 5-device project |
| 7 — refreshing registries | **done** (#247) | MCP revocation honoured without a restart; ReadLedger moved to Phase 3 |
| 8 — auth state to SQL | **done** (#248, #251, #252, cloud #47) | every sign-in flow crosses processes; 50/50 live run deferred to Stage 12 |
| 9 — cross-instance fan-out | **transport done** (#250) | frame crosses, no echo, oversize degrades — on real postgres in CI |
| 10 — conditional writes | not started | |
| 11 — co-editing across instances | not started | |
| 12 — raise the cap | not started | |

### Phase 1 notes (2026-09-21)

**Stage 1 made Stage 2 mandatory rather than optional.** Standing down beside
the change stream cut `/scope` by the same 30x as everything else — it is
fetched per remote cycle, so it fell out for free — but it also stretched the
window in which a device holds a stale scope from ~10s to ~5 minutes. That is
not a volume problem, it is the wedging hazard `loadScope` exists to prevent.
Stage 2 closed it by pushing, which makes the window shorter than it was
before Stage 1 (<1s). Volume was never the reason to do Stage 2.

**Stage 4 uncovered a real bug that predates it.** `base` — the `If-Match` sha
every save carries — only ever moved on our OWN successful write. In a room
that is wrong: a co-editor's save moves the file's head, everyone else keeps
the sha from before it, and their next save is a 409. The old
"everyone saves on every change" behaviour hid it, because each client
refreshed its own sha constantly. Removing the redundant writes exposed it as
`two people editing different paragraphs both land` failing — window two saved,
was refused, and parked its work in a `.bdrive-conflict-` copy **of the file
against itself**. Fixed with `SharedFile.rebase(sha)`, wired into both editing
surfaces. The sha is adopted only where the content is ACCEPTED, never on a
blocked merge — adopting it there would turn `If-Match` from a guard into a
rubber stamp.

**Every gate in this phase was re-run against the unfixed code and watched to
fail**, including the two guard tests whose job is to catch over-correction
(`TestPollResumesWhenTheStreamDies`, `TestPresenceStillExpiresWithoutAStream`)
— those were failed deliberately by breaking the fix.

**e2e baseline.** The Playwright suite fails ~8 specs per run on `main` as
well, in a set that varies between runs (the admin/hub cascade plus a rotating
third). Measured on `main` for this phase: 269 passed / 8 failed. Every spec
that failed on this branch was re-run in isolation and passed. The one
REPRODUCIBLE failure was the Stage 4 bug above, which is what a real
regression looks like next to this noise.

Already landed ahead of this PRD (2026-09-21, `beardrive-cloud` #43/#44):
Cloud Run `timeout` 300s → 3600s (every `/events` and `/ycollab` request was
ending at exactly 301.0s, all day — the "realtime editing is unstable" report
and, via `OnLastPeer`, the history-noise one), memory 512Mi → 1Gi (production
had logged `Memory limit of 512 MiB exceeded with 636 MiB used` four times on
2026-09-17), concurrency → 1000 and startup CPU boost on.

### Measurements

Record each stage's before/after here, from the two production queries in
§How success is measured. The baseline row is the one to beat:

| date | req/24h | peak:trough | daemon share | idle daemon req/day |
|---|---|---|---|---|
| 2026-09-21 (baseline) | 183,150 | 1.6× | 67% | 17,280 |
| 2026-09-22 (Phase 1+2 deployed) | ~4,600/hr vs ~7,900/hr — **≈45% down**, see below | n/a yet | **80%** (up from 67%) | unchanged |

**Phase 1 landed by half, and the reason matters more than the number.**

Measured 2026-09-22 08:00–08:20Z against the same-shaped window on 09-21:
1,561 requests vs 3,446. Hourly volume fell from a 6,700–10,400 band to
3,500–5,100.

What moved: `POST /presence` fell **76%** (≈670 → 160 in twenty minutes), and
the editor's duplicate writes are gone. Both are browser-side, and the browser
gets its bundle from the hub — so those landed the moment the hub deployed.

What did NOT move: `/store/list` fell about 11% and is now **80% of all hub
traffic**, with daemons at 80% of clients (up from 67%). The daemons are
holding their change streams correctly — `/events` opens now last the full
3601s, so the timeout fix is working for them — but they are still polling at
`remoteInterval`, because **Stage 1 is a change to the `bdrive` BINARY**. The
hub cannot lower a cadence that a client decides.

So the 30x-per-idle-device win, which is the largest single item in this
document, is gated on a CLI release and on devices upgrading to it. Until
then the hub keeps paying 17,280 requests a day per idle daemon-project. That
is not a defect in the change; it is a deployment fact the PRD failed to state
anywhere, and it should have been the first line of Stage 1.

A correction on the way: an earlier read of this window said `/scope` had
vanished. It had not — my aggregation collapsed only UUID-shaped project ids,
so the `p-xxxxxxx` ones stayed split across rows and fell below the cutoff I
was printing. `/scope` is still fetched once per remote cycle, exactly as
expected from daemons on the old binary.

**Journal-key growth (Stage 6).** Recorded so the next reader can check it
stopped rather than take my word for it. On 2026-09-22, immediately after the
Phase 2 deploy (`70f2c46`): **103 journal objects hub-wide, 43 of them on
`p-c01bde39`** — a project with about five real devices. The id this hub now
derives from its storage root is `9882b418dfba`; it appears once, the first
time the hub journals, and then never again. The test of the fix is that the
count does not move across the NEXT few deploys. It is not proof yet.

The 43 already there are NOT cleaned up. They hold real ops mixed with
near-empty ones and a journal is append-only, so compaction is its own careful
job — filed in the backlog, not done.
