# PRD: Let the hub hold the document

Co-editing works. What surrounds it does not: every bug this area has produced
comes from the same root, which is that **nobody owns the document**. The CRDT
lives only between browsers, the hub is a relay that never looks inside a
frame, and the file is written by whichever client stopped typing last.

That design was correct when it was chosen, for a reason written into
`collab.go`: a cgo y-crdt "would break the cross-compiled release the same way
a cgo sqlite would". **That constraint no longer holds** — pure-Go, CGO-free
Yjs ports now exist and embed as an `http.Handler`.

This spec replaces the hand-rolled provider with a server-held document, and
deletes the machinery that exists only to compensate for not having one.

Nothing here changes the journal, `journal.Less`, `Replay`, or any sync
invariant. The file stays the artifact.

## What "nobody owns it" has cost

Every item below is a real failure, not a hypothetical. Dates are 2026-09-18.

**1. A client that loses the relay overwrites everyone.**
`CollabDoc.onUnavailable` falls back to single-writer editing over its own
buffer. Two browsers then hold two documents and take turns PUTting them
wholesale. Reproduced in `e2e/concurrent-edit.spec.ts`: six characters typed
by one editor vanished, no conflict copy, no warning. A real history feed
showed it as versions alternating between ~1.3 KB and ~750 B several times a
minute — for a user whose network had just changed.

Mitigated (not fixed) by the `If-Match` + conflict-copy work on
`fix/editor-conflict-copies`. A conflict copy is the right safety net; needing
one every time a laptop changes network is not.

**2. Seeding is a claim, with a grace timer.**
`seedClaimGrace` (10 s) exists because a Yjs document seeded independently by
two clients is not the same document — merging them duplicates every
character. So the room tells exactly one joiner it is first, and a claim that
never produces anything has to expire. A server that holds the document has
nothing to claim.

**3. The room log grows without bound, then everyone rebuilds.**
`maxRoomBytes` (8 MiB) is memory a member can grow by typing. Past it the room
empties and every client reconnects — and the next seeder seeds from
`seedText`, captured when its editor opened, which may be hours old.

**4. Reconnect replays the whole log.**
`reconnect()` builds a fresh `Y.Doc` and re-applies every update in the room,
because the provider has no state-vector handshake. Long sessions pay for
their own history on every flaky connection.

**5. Discovery is a chicken-and-egg.**
Awareness is relayed and never logged, so an announcement is lost to anyone who
arrives later. Suppressing cursor traffic when alone (the obvious fix for
one-POST-per-arrow-key) makes two people permanently invisible to each other
unless a re-announce is bolted on — which it now is, and which a server that
knows who is in the room would not need.

**6. Snapshotting is a client's job.**
"Whoever stops typing last writes the file" means N co-editors each write the
same converged text: N journal ops, N change frames, N history rows, N full
tree refetches for every other client. See
`docs/network-efficiency-prd.md` stages 3 and 4, which are partly cleaning up
after this.

Items 2, 3, 4, 5 and 6 are not bugs to fix. They are **compensations for the
missing owner**, and they disappear with it.

## What changed outside this repo

Pure-Go Yjs ports, no CGO, wire-compatible with `yjs@13.x`:

- [Deln0r/ygo](https://github.com/Deln0r/ygo) — byte-for-byte V1/V2 wire
  compatibility with cross-language fixtures; its `yserve` is a single-binary
  Hocuspocus-compatible server that **also embeds as a plain `http.Handler`**,
  which is what keeps our auth, org walls and `proj()` permission wrapper in
  front of it.
- [reearth/ygo](https://github.com/reearth/ygo) — versioned persistence over
  `modernc.org/sqlite`, the same pure-Go driver `db_sql.go` already uses. Ships
  with **no built-in auth** and binds loopback by default, which is fine
  embedded and dangerous standalone.
- Both are listed on the [Yjs ports page](https://docs.yjs.dev/ecosystem/ports-to-other-languages).
- [y-sweet](https://github.com/jamsocket/y-sweet) is the more established
  S3-backed option and is rejected here only because it is a second binary to
  deploy, which costs the single-binary self-hosting story.

On the client, `@hocuspocus/provider` replaces our provider with a maintained
one (reconnect, backoff, state-vector sync). The repo already depends on
`@tiptap/*`, so this is the native pairing rather than a new ecosystem.

## The decision this needs first

**Today the hub never parses a client-supplied CRDT update.** `collab.go`
stores opaque bytes and hands them on. Embedding a CRDT ends that property:
untrusted bytes get decoded, in-process, in the hub.

That is a security-posture change, not a dependency bump, and it is the one
call that cannot be made by whoever picks up this document. Decide it
explicitly, in §Status, before Stage 1.

Arguments for: every other input the hub parses (journals, JSON, markdown,
uploads) is already untrusted, and `store.go` already inflates client-supplied
gzip under a bounded reader. Arguments against: a CRDT decoder is a much larger
attack surface than any of those, in a young library, reachable by any project
member.

## Goals

- A disconnected client cannot overwrite a teammate's work, by construction.
- One editing session writes one file version per settle, not one per editor.
- A reconnect transfers what the client is missing, not the session's history.
- `collab.ts`'s bespoke provider and the compensations above are **deleted**,
  not extended.

## Non-goals (do NOT do these)

- Not a CRDT change. Yjs stays; nothing that failed was a Yjs bug.
- The file remains the artifact. Agents, the CLI and the desktop write files
  and never touch a CRDT — any design that makes the document authoritative
  over the file is wrong for this product.
- **Keep `If-Match` and conflict copies.** They protect the file from every
  writer, including the ones with no room to join. This work should make them
  rare, never unnecessary.
- No Node runtime, and no second binary to deploy.
- No hosted service (Liveblocks, Tiptap Cloud, PartyKit): self-hosting is the
  product.

## Stages

### Stage 0 — decide, then spike

- [x] The parsing-untrusted-updates decision, recorded in §Status with the
      conditions it carries
- [x] **Port chosen: `github.com/reearth/ygo` v1.50.0.** CGO-free; ships
      `crdt`, `awareness`, `provider/websocket`, `persistence` and `cluster`
      (which is the multi-process answer, not a gap); 50 minor releases and
      its own JS-compat suite in-tree. `Deln0r/ygo` was the other candidate
      and is also credible — same wire claims, a Hocuspocus-compatible
      `yserve` — and is the fallback if this one stalls.
- [x] **Cross-language fixtures pass, against the exact `yjs` the browser
      ships** (`node_modules/yjs`), both directions and both encodings:

      | | V1 | V2 |
      |---|---|---|
      | JS reads a Go document (`ünïcode ✅`) | MATCH | MATCH |
      | Go reads a JS document (`日本語 🎉`) | MATCH | MATCH |
      | Go reads a JS document of 10,000 ops | MATCH | MATCH |

      These move into CI in Stage 1 as a Go test with checked-in fixtures, so
      a version bump that breaks the wire fails the build rather than the
      editor.
- [x] **Memory per open document measured** (`crdt.New()` + `YText`, heap
      delta after GC, 50-200 documents per shape):

      | shape | per document |
      |---|---|
      | 10 KB file inserted whole (loading a file) | 11.5 KB |
      | 100 KB file inserted whole | 105.5 KB |
      | 10 KB file typed **character by character** | **101.7 KB** |

      The third row is the planning number, and it is the surprise: editing
      costs ~10x the content, because every keystroke is its own item until
      GC merges them. So an actively-edited document is ~100 KB and a hub
      with 100 of them open is ~10 MB — against today's relay, which caps
      each room's update log at 8 MiB on its own.

      **The cap is therefore eviction, not bytes.** `maxRoomBytes` existed
      because an append-only log grows without bound while a document does
      not: the same text typed twice is one document and two log entries.
      Idle documents are dropped the way `roomIdle` drops idle rooms, and the
      file is the durable copy either way.
- [x] A hub running multiple processes: `ygo/cluster` exists for exactly this.
      Scope for Stage 1 is a single process with the cluster path unused and
      named as the upgrade, rather than pretending the question does not
      exist.

**Success criteria:** a written go/no-go with the fixture results and the
memory number in it. **GO.** Fixtures pass, ~100 KB per actively-edited
document, and the cap is an eviction policy rather than a byte ceiling.

### Stage 1 — the hub holds the document

- [x] `reearth/ygo`'s websocket server embedded as an `http.Handler`
      (`ycollab.go`), mounted behind the existing `proj()` wrapper
- [x] **The room name is the hub's, never the caller's.** ygo reads it from
      `PathValue("room")` or the URL's last segment, so a caller who could
      name the room would make the project id in the path decoration — any
      member of any project could join any other project's document by asking
      for its name. Verified failing without the guard
- [x] Read-only membership applies as read-only, not as refusal: the route is
      `PermRead` and the CONNECTION carries `ReadOnly`, so a member who may
      read a file can open it and watch it being edited
- [x] `filterJournal`'s sibling question: a path the caller cannot see is
      **404, never 403** — the rule the viewer's `pathFilter` already applies,
      because a 403 confirms the file is there
- [x] Documents are **rebuilt from the file deterministically**, by the hub,
      on room creation and before any client is attached (`fileSeed.LoadDoc`).
      This is what retires the seed claim outright: there is nothing to claim
      and nothing to race
- [x] Seeding is bounded (`maxSeedBytes`), because a held document costs ~10x
      its content in CRDT items
- [x] The old relay is untouched and still mounted; this is beside it
- [x] Wire fixtures in CI, produced by the frontend's own yjs and checked in
      as bytes so CI needs no node

**Success criteria:** two browsers converge through the hub with the bespoke
provider deleted from the path; `e2e/concurrent-edit.spec.ts` still passes.

### Stage 2 — the client stops being a provider

- [x] **`y-websocket`, not `@hocuspocus/provider`.** ygo speaks y-websocket
      natively and Hocuspocus only behind a server flag, so this is one fewer
      thing that has to agree. Same gain either way: a state-vector handshake
      instead of replaying the whole room log on reconnect, and backoff that
      is somebody else's problem
- [x] The client is told, not left to guess: `/api/config` carries
      `collab.held`. A hub too old to serve the route and a proxy that refuses
      an upgrade fail identically, and guessing wrong means an editor waiting
      for a document nobody will send
- [x] Nothing seeds client-side on the held path — the hub built the document
      from the file before anyone attached
- [x] The relay's update and awareness POSTs are silent on the held path: the
      socket carries both, and posting them too was the same bytes twice at a
      route that only serves GET (405s in the console, which is how it was
      found)
- [ ] `collab.ts` deleted, not adapted — **Stage 4**. Both transports live in
      it for now, which is what "the old relay stays reachable for a release"
      means in practice
- [ ] `onSolo` and its callers deleted — **Stage 4**, for the same reason

**Success criteria:** `zz-stress-edit.spec.ts` — three editors, one cut off
throughout, one losing the relay halfway — produces **zero conflict copies**,
where today it produces them by design. Reconnect transfers less than the
session's total update volume (measured, not assumed).

### Stage 3 — the server writes the file

- [x] The hub snapshots the document to the file when the last editor leaves
      and when the room is unloaded (`OnLastPeer`, `OnUnloadDocument`)
- [x] **A safety net, not a replacement.** The browser still saves on idle.
      That is not double-writing: identical content journals nothing
      (#238), so whichever write lands second is free. What the snapshot adds
      is the case no client can cover — every client going away at once, which
      used to lose whatever had not reached the 700 ms idle save
- [x] One settle = one journal op, whatever the number of editors — which
      #238 already delivers for identical text, and this does not undo
- [x] `Op.User` names the human. A version authored by "the server" is a
      regression in History even when the server holds the pen, so the writer
      is recorded at `Authorize` and a room nobody could write to writes
      nothing at all
- [x] The snapshot RE-ENTERS the API rather than calling the uploader: quota,
      folder permissions, the no-op check, journaling and the change frame are
      then the same code every other write goes through. A second path into
      the file is a second set of rules to keep in agreement
- [x] Seeding moved from the persistence adapter to `OnLoadDocument`, which
      hands over the actual document instead of encoded bytes

**Success criteria:** a three-editor session that today produces one version
per editor per pause produces one per pause. ✅ — though honestly it was #238
that delivered it, by making the duplicate writes free rather than by stopping
them. This stage's own contribution is durability: the document survives every
client leaving at once.

### Stage 4 — delete the compensations

- [x] `internal/webapp/collab.go` **deleted** (441 lines), with its two test
      files: the relay, its rooms, the seed claim, `seedClaimGrace`,
      `maxRoomBytes`, the `full` rebuild and the resync frame
- [x] `lib/collab.ts` reduced from a hand-rolled provider to a thin wrapper
      over `y-websocket`; the SSE/POST transport, the log replay, the
      announce/re-announce dance and `soloText`/`soloApply` are gone
- [x] `/api/config`'s `collab.held` removed too — with one transport there is
      nothing left to choose, and a capability flag nobody reads is exactly
      the "new mechanism replacing one that went away" this stage forbids
- [x] Solo mode survives only as *no live collaboration*: an unreachable hub
      leaves the editor open and saving through `upload/content`. That is not
      a second CRDT path — a client editing its own COPY of a shared document
      is precisely what let two browsers overwrite each other
- [x] Both architecture diagrams updated and parse-checked. The `collabRoom`
      note was the largest single block of "why this is hard" in either file;
      it is now one note about a hub that owns the document

**Success criteria:** net lines deleted, and no new mechanism replacing one
that went away. ✅ **1,669 deletions against 68 insertions.**

### What the deletion is worth, concretely

`e2e/concurrent-edit.spec.ts` used to assert that a relay-less editor's work
was *preserved beside* a teammate's, because the two held different documents.
It now asserts they **converge**: every character both people typed is in the
file, and no conflict copy was needed to get it there. The stress spec agrees
— `1 paths, 2 versions`, where it used to report three paths and two conflict
copies.

The conflict-copy machinery stays. It guards the file against writers that
never touch a CRDT at all — an agent, the CLI, a device syncing — which is a
door the relay's removal does not close.

## Risks

- **Young libraries.** Mitigated by Stage 0's fixtures and by keeping the old
  relay behind a flag for a release.
- **Untrusted parsing.** The Stage 0 decision; bound the decoder's inputs the
  way `maxInflatedPut` bounds gzip.
- **Memory.** A held document per open file is state the relay never had.
- **Horizontal scaling.** The managed hub may run more than one process; two
  instances holding one document need a backplane or sticky routing. Answer it
  in Stage 0 or scope it out loudly.

## Status

_All five stages are implemented. The hub holds the document, seeds it from
the file, writes it back, and the compensations the relay needed are deleted
rather than disabled._

| Stage | State | Notes |
|---|---|---|
| 0 — decide + spike | **done — GO** | reearth/ygo v1.50.0; JS<->Go fixtures pass V1+V2 incl. 10k ops; ~100 KB per edited doc |
| 1 — hub holds the document | **done** | behind proj(); hub seeds from the file; wire fixtures in CI |
| 2 — client stops being a provider | **done** | y-websocket; editor mounts on the hub-held document; 25 editor e2e pass |
| 3 — server writes the file | **done** | snapshot on last peer; attributed to the human; re-enters the API |
| 4 — delete the compensations | **done** | -1669/+68; two browsers now converge instead of conflicting |

### The decision

- [x] **May the hub parse client-supplied CRDT updates?** **Yes.**
      Decided by: Snow Lee. Date: 2026-09-19.

      Conditions carried forward into Stage 0 and Stage 1 rather than left as
      a sentiment: the decoder's inputs are bounded the way `maxInflatedPut`
      bounds gzip, the server is mounted behind the existing `proj()` wrapper
      so folder permissions and org walls apply unchanged, and the old relay
      stays behind a config flag for one release.
